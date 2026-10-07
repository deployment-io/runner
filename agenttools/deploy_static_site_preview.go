package agenttools

// deploy_static_site_preview.go implements the agent-invoked `deploy_static_site_preview` MCP tool: the
// coding agent calls this to stand up (or refresh) a live preview of a static site on
// the project's cloud and get back a URL. For a repository deployment.io deploys as a
// static site, the runner builds the preview itself the way the deploy does (see
// static_site_preview_build.go); for any other repository the agent builds it inside
// its /work tree and passes the output directory.
//
// C4: the preview is a persisted control-plane record. On each call the tool asks the
// injected PreviewStore to find-or-create the task's ephemeral Environment + the lean
// static Deployment for this service, and hands back that Deployment's id (bucket/
// resource naming) + the resources (distribution id/domain) persisted from a prior
// deploy. So reuse is stateless and correct ACROSS runners — the record is the source
// of truth, no in-memory cache and no CloudFront self-discovery. After a first deploy
// the tool persists the new resources back through the store so later calls/steps/
// runners reuse them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

const (
	// containerWorkDir mirrors run_agent_step's agentboxWorkDirInContainer; used only
	// to strip an absolute container path an agent might pass for publish_dir.
	containerWorkDir = "/work"
	// logTailMaxBytes caps the deploy log echoed back to the agent in the result.
	logTailMaxBytes = 2000
)

// DeployStaticSitePreviewDeps is the task-scoped context the deploy_static_site_preview handler closes
// over, built once per RunAgentStep.
type DeployStaticSitePreviewDeps struct {
	OrgID       string    // organization id (bucket naming)
	Region      string    // the runner's region string, e.g. "us-east-1"
	WorkDirHost string    // host path of the bind-mounted /work; publish_dir resolves under it
	LogsWriter  io.Writer // the Step Job's log writer; deploy progress streams here

	// BuildClients lazily constructs the AWS clients (runner IAM role + region).
	BuildClients func() (*s3.Client, *cloudfront.Client, error)

	// Store find-or-creates the persisted preview record and saves resources back onto
	// it — the control-plane seam. Bound to one serviceType by the commands layer, so a
	// web-service or database preview tool reuses this same struct with its own store.
	Store PreviewStore

	// Record is the agent run's preview record, shared with verify_preview_reachable:
	// the host of every URL this tool returns is added to it.
	Record *PreviewRecord

	// Repositories are the Task's repositories in checkout order: the one at index i
	// is checked out at <WorkDirHost>/<i>-<Name> and is /work/<i>-<Name> to the agent.
	Repositories []tasks.RepositoryEntry

	// BuildSettings asks deployment-server how the org deploys the repository at
	// cloneURL as a static site, with the org's preview configuration. The reply
	// carries the configuration's values: they reach the build and nothing else.
	BuildSettings func(cloneURL string) (task_previews.StaticSiteBuildSettingsReplyV1, error)

	// BuildSite runs the deploy build's container steps (pull, start, install, build,
	// remove) in dir with buildCommand and env ("KEY=value"), streaming to logs.
	BuildSite func(ctx context.Context, dir, buildCommand string, env []string, logs io.Writer) error
}

// Values of deployStaticSitePreviewResult.BuiltBy.
const (
	builtByDeploymentIO = "deployment.io"
	builtByAgent        = "agent"
)

// deployStaticSitePreviewResult is the JSON the agent receives from a tools/call.
// It never contains a value of the preview configuration.
type deployStaticSitePreviewResult struct {
	URL            string `json:"url"`
	Status         string `json:"status"`
	DistributionID string `json:"distribution_id"`
	BuiltBy        string `json:"built_by"`
	Configuration  string `json:"configuration,omitempty"`
	Note           string `json:"note,omitempty"`
	LogTail        string `json:"log_tail,omitempty"`
}

const deployStaticSitePreviewInputSchema = `{
  "type": "object",
  "properties": {
    "repository": {
      "type": "string",
      "description": "The repository's directory under /work (e.g. \"0-deployment-io/dashboard\"). Pass it for any repository: if deployment.io deploys it as a static site, deployment.io builds the preview the way the deploy does, with the org's preview configuration."
    },
    "root_directory": {
      "type": "string",
      "description": "Only when the tool asks for it: the root directory of the site to preview, for a repository deployment.io deploys as several sites with different settings (\"\" for the repository root)."
    },
    "publish_dir": {
      "type": "string",
      "description": "Only for a repository deployment.io doesn't deploy: the directory your build produced, relative to /work (e.g. \"0-org/repo/dist\"). Must contain index.html. Ignored when deployment.io builds the site."
    },
    "is_spa": {
      "type": "boolean",
      "description": "Only with publish_dir: true for a single-page app with client-side routing (unknown paths serve index.html). Default false. Ignored when deployment.io builds the site."
    }
  }
}`

// RegisterDeployStaticSitePreview registers the deploy_static_site_preview tool. The
// returned stop cancels the preview builds still running and waits until their
// copies are removed; call it when the agent run ends.
func RegisterDeployStaticSitePreview(s *agentmcp.Server, deps DeployStaticSitePreviewDeps) (stop func()) {
	builds := newPreviewBuilds()
	s.Register(agentmcp.Tool{
		Name: "deploy_static_site_preview",
		Description: "Deploy a static site to a live preview URL on the project's cloud and return the URL. " +
			"Pass repository (its directory under /work) for any repository: if deployment.io deploys it as a static " +
			"site, deployment.io builds the preview itself the way the deploy does, from your working tree, with the " +
			"org's preview configuration — you don't build it and never see that configuration. If the build takes a " +
			"while the result is status \"building\": call again with the same arguments to wait for it. " +
			"Only for other repositories (the tool says so), build it yourself and pass publish_dir (relative to /work) — " +
			"the output of your REAL build command, containing index.html — and is_spa=true for single-page apps with " +
			"client-side routing. NEVER hand-create files to deploy; if the build fails, report that instead of " +
			"deploying a placeholder. Re-call to redeploy after changes (the same preview URL is reused), then use " +
			"verify_preview_reachable to confirm it's live.",
		InputSchema: json.RawMessage(deployStaticSitePreviewInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleDeployStaticSitePreview(ctx, deps, builds, args)
		},
	})
	return builds.stop
}

// errNotDeployedSite is the agent-built path's error when publish_dir is missing.
const errNotDeployedSite = "this repository isn't a site deployment.io deploys, so build it yourself and pass publish_dir"

func handleDeployStaticSitePreview(ctx context.Context, deps DeployStaticSitePreviewDeps, builds *previewBuilds, rawArgs json.RawMessage) (string, error) {
	var args struct {
		Repository    string  `json:"repository"`
		RootDirectory *string `json:"root_directory"`
		PublishDir    string  `json:"publish_dir"`
		IsSPA         bool    `json:"is_spa"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if strings.TrimSpace(args.Repository) != "" {
		// A call repeating one whose build is still there attaches to it at once; any
		// other looks up the settings first, within the same previewBuildCallWait.
		started := time.Now()
		request := strings.TrimSpace(args.Repository) + "\x00"
		if args.RootDirectory != nil {
			root, _ := cleanRelativeDir(*args.RootDirectory)
			request += "root:" + root
		}
		if out, err, ok := builds.attach(ctx, request, previewBuildCallWait); ok {
			return out, err
		}
		plan, ok, err := planRunnerBuiltPreview(deps, args.Repository, args.RootDirectory)
		if err != nil {
			return "", err
		}
		if ok {
			return builds.callFor(ctx, request, plan.serviceName, func(buildCtx context.Context) (string, error) {
				return runRunnerBuiltPreview(buildCtx, deps, plan)
			}, previewBuildCallWait-time.Since(started))
		}
		if strings.TrimSpace(args.PublishDir) == "" {
			return "", errors.New(errNotDeployedSite)
		}
	}
	if strings.TrimSpace(args.PublishDir) == "" {
		return "", fmt.Errorf("publish_dir is required — or pass repository, and deployment.io builds a site it deploys itself")
	}
	// The reuse key is derived solely from publish_dir (repo + build-dir) — deterministic
	// and reproducible across steps/runners, with no name for the agent to remember. A
	// publish_dir that yields no usable key is rejected (not collapsed to a shared name).
	serviceName, ok := deriveServiceName(args.PublishDir)
	if !ok {
		return "", fmt.Errorf("could not derive a service name from publish_dir %q — point it at the built output inside your repo (e.g. %q)", args.PublishDir, "<repo>/dist")
	}
	distDir := resolvePublishDir(deps.WorkDirHost, args.PublishDir)
	// Not cancelled with the call: as before, an agent-built deploy runs to its end.
	out, err := deployPreviewDir(context.Background(), deps, serviceName, distDir, args.IsSPA)
	if err != nil {
		return "", err
	}
	out.BuiltBy = builtByAgent
	return marshalResult(out)
}

// deployPreviewDir is deployPreviewDirectory; tests replace it to deploy nowhere.
var deployPreviewDir = deployPreviewDirectory

// deployPreviewDirectory deploys distDir as the preview of serviceName: it resolves
// the persisted preview identity, uploads into it (reusing its distribution), saves
// the resources of a first deploy and records the URL's host. Cancelling ctx gives up
// waiting for the preview record and aborts the cloud requests in flight and refuses
// the next ones.
func deployPreviewDirectory(ctx context.Context, deps DeployStaticSitePreviewDeps, serviceName, distDir string, isSPA bool) (deployStaticSitePreviewResult, error) {
	// Resolve the persisted preview identity (find-or-create). The record is the
	// source of truth for the deployment id + any existing distribution.
	previewID, existing, err := ensurePreview(ctx, deps.Store, serviceName)
	if err != nil {
		return deployStaticSitePreviewResult{}, fmt.Errorf("ensure preview record: %w", err)
	}

	s3Client, cfClient, err := deps.BuildClients()
	if err != nil {
		return deployStaticSitePreviewResult{}, fmt.Errorf("build cloud clients: %w", err)
	}
	if ctx.Done() != nil {
		s3Client = s3.New(s3Client.Options(), func(o *s3.Options) { o.HTTPClient = newCtxHTTPClient(ctx, o.HTTPClient) })
		cfClient = cloudfront.New(cfClient.Options(), func(o *cloudfront.Options) { o.HTTPClient = newCtxHTTPClient(ctx, o.HTTPClient) })
	}

	var tail bytes.Buffer
	logs := io.MultiWriter(deps.LogsWriter, &tail)
	res, err := DeployStaticSitePreview(StaticPreviewDeployInput{
		OrgID:            deps.OrgID,
		PreviewID:        previewID,
		DistDirectory:    distDir,
		Region:           deps.Region,
		IsSPA:            isSPA,
		ExistingDistID:   existing.CloudFrontDistributionID,
		S3Client:         s3Client,
		CloudfrontClient: cfClient,
		SkipDeployWait:   true, // return promptly; the CDN propagates async (verify_preview_reachable polls)
	}, logs)
	if err != nil {
		return deployStaticSitePreviewResult{}, fmt.Errorf("deploy preview: %w", err)
	}

	// A first deploy returns the fresh id + domain — persist them. A reuse returns
	// only the id (nothing new to save), so fall back to the persisted domain.
	distID := res.DistributionID
	if distID == "" {
		distID = existing.CloudFrontDistributionID
	}
	domain := res.DomainName
	if domain == "" {
		domain = existing.CloudFrontDomainName
	}
	if res.DomainName != "" {
		deps.Store.SavePreview(previewID, PreviewState{
			CloudFrontDistributionID:  res.DistributionID,
			CloudFrontDistributionArn: res.DistributionArn,
			CloudFrontDomainName:      res.DomainName,
		})
	}

	previewURL := "https://" + domain
	deps.Record.AddURL(previewURL)
	return deployStaticSitePreviewResult{
		URL:            previewURL,
		Status:         "deployed",
		DistributionID: distID,
		Note:           "CDN propagation may take a few minutes before the URL serves the latest content.",
		LogTail:        tailString(tail.Bytes(), logTailMaxBytes),
	}, nil
}

// ensurePreview is Store.EnsurePreview, given up when ctx is cancelled. The RPC takes
// no context, so a cancelled call leaves it to finish in the background: it only
// find-or-creates the preview record, which the next deploy reuses.
func ensurePreview(ctx context.Context, store PreviewStore, serviceName string) (string, PreviewState, error) {
	if ctx.Done() == nil {
		return store.EnsurePreview(serviceName)
	}
	if err := ctx.Err(); err != nil {
		return "", PreviewState{}, err
	}
	type reply struct {
		id       string
		existing PreviewState
		err      error
	}
	done := make(chan reply, 1)
	go func() {
		id, existing, err := store.EnsurePreview(serviceName)
		done <- reply{id, existing, err}
	}()
	select {
	case r := <-done:
		return r.id, r.existing, r.err
	case <-ctx.Done():
		return "", PreviewState{}, ctx.Err()
	}
}

// abortUploadTimeout bounds a multipart upload's abort sent after ctx is cancelled.
const abortUploadTimeout = 30 * time.Second

// ctxHTTPClient sends the cloud clients' requests cancelled with ctx as well as with
// their own context — the shared upload and CloudFront helpers take no context.
// The one exception is the uploader's AbortMultipartUpload, its cleanup after a
// failed upload: it is sent even after ctx is cancelled, within abortUploadTimeout,
// so a cancelled upload doesn't strand its parts in the bucket.
type ctxHTTPClient struct {
	ctx  context.Context
	next interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func newCtxHTTPClient(ctx context.Context, next interface {
	Do(*http.Request) (*http.Response, error)
}) ctxHTTPClient {
	if next == nil {
		next = http.DefaultClient
	}
	return ctxHTTPClient{ctx: ctx, next: next}
}

func (c ctxHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if isAbortMultipartUpload(req) {
		reqCtx, cancel := context.WithTimeout(req.Context(), abortUploadTimeout)
		resp, err := c.next.Do(req.WithContext(reqCtx))
		if err != nil {
			cancel()
			return nil, err
		}
		resp.Body = &releasingBody{ReadCloser: resp.Body, release: cancel}
		return resp, nil
	}
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithCancel(req.Context())
	stopAfter := context.AfterFunc(c.ctx, cancel)
	release := func() {
		stopAfter()
		cancel()
	}
	resp, err := c.next.Do(req.WithContext(reqCtx))
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// isAbortMultipartUpload reports whether req is S3's AbortMultipartUpload
// (DELETE /<key>?uploadId=…).
func isAbortMultipartUpload(req *http.Request) bool {
	return req.Method == http.MethodDelete && req.URL != nil && req.URL.Query().Has("uploadId")
}

// releasingBody releases a ctxHTTPClient request's context once its body is closed.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func marshalResult(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// resolvePublishDir maps the agent-supplied publish_dir onto a host path under
// workDirHost. It strips a leading container /work prefix and neutralizes any ../
// traversal by resolving as if rooted at /, so the result can never escape
// workDirHost.
func resolvePublishDir(workDirHost, publishDir string) string {
	p := strings.TrimSpace(publishDir)
	p = strings.TrimPrefix(p, containerWorkDir) // "/work/dist" -> "/dist"; "dist" unchanged
	rel := strings.TrimPrefix(filepath.Clean("/"+p), "/")
	return filepath.Join(workDirHost, rel)
}

// deriveServiceName builds a deterministic, repo-aware service key from publish_dir —
// the sole source of the preview's reuse identity. A /work-relative publish path is
// "<idx>-<org>/<repo>/<subdir>"; strip the numeric repo-index prefix and sanitize to
// "<org>-<repo>-<subdir>", so two same-type services (different repos/subdirs) get
// distinct keys and each reuses correctly across steps/runners. Returns ok=false when
// nothing usable remains (a degenerate publish_dir with no alphanumerics — which also
// wouldn't contain index.html), so the caller rejects it rather than collapse to a
// shared name that two services could then clobber.
func deriveServiceName(publishDir string) (string, bool) {
	p := strings.TrimSpace(publishDir)
	p = strings.TrimPrefix(p, containerWorkDir)
	rel := strings.TrimPrefix(filepath.Clean("/"+p), "/")
	// Strip a leading numeric "<idx>-" repo-index prefix (keeps the key stable even
	// if the task's repo ordering ever changes).
	if i := strings.IndexByte(rel, '-'); i > 0 && isAllDigits(rel[:i]) {
		rel = rel[i+1:]
	}
	key := sanitizeServiceKey(rel)
	if key == "" {
		return "", false
	}
	return key, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sanitizeServiceKey collapses each run of non-alphanumeric characters (path
// separators included) into a single '-', trimming leading/trailing dashes.
func sanitizeServiceKey(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// tailString returns the last max bytes of b as a string, prefixed with an ellipsis
// when truncated, on a rune boundary so the JSON stays valid UTF-8.
func tailString(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	t := b[len(b)-max:]
	for len(t) > 0 && t[0]&0xC0 == 0x80 {
		t = t[1:]
	}
	return "…" + string(t)
}
