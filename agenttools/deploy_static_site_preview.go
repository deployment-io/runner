package agenttools

// deploy_static_site_preview.go implements the agent-invoked `deploy_static_site_preview` MCP tool: the
// coding agent, after building a static site inside its /work tree, calls this to
// stand up (or refresh) a live preview on the project's cloud and get back a URL.
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
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
}

// deployStaticSitePreviewResult is the JSON the agent receives from a tools/call.
type deployStaticSitePreviewResult struct {
	URL            string `json:"url"`
	Status         string `json:"status"`
	DistributionID string `json:"distribution_id"`
	Note           string `json:"note,omitempty"`
	LogTail        string `json:"log_tail,omitempty"`
}

const deployStaticSitePreviewInputSchema = `{
  "type": "object",
  "properties": {
    "publish_dir": {
      "type": "string",
      "description": "Path to the built static-site directory, relative to /work (e.g. \"dist\" or \"web/build\"). Must contain index.html."
    },
    "is_spa": {
      "type": "boolean",
      "description": "True for a single-page app with client-side routing (unknown paths serve index.html). Default false."
    }
  },
  "required": ["publish_dir"]
}`

// RegisterDeployStaticSitePreview registers the deploy_static_site_preview tool.
func RegisterDeployStaticSitePreview(s *agentmcp.Server, deps DeployStaticSitePreviewDeps) {
	s.Register(agentmcp.Tool{
		Name: "deploy_static_site_preview",
		Description: "Deploy the built static site to a live preview URL on the project's cloud and return the URL. " +
			"Call this AFTER a successful build — publish_dir (relative to /work) must be the output of your REAL build " +
			"command and contain index.html. NEVER hand-create files to deploy; if the build fails, report that instead " +
			"of deploying a placeholder. Set is_spa=true for single-page apps with client-side routing. Re-call to " +
			"redeploy after changes (the same preview URL is reused), then use verify_preview_reachable to confirm it's live.",
		InputSchema: json.RawMessage(deployStaticSitePreviewInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleDeployStaticSitePreview(ctx, deps, args)
		},
	})
}

func handleDeployStaticSitePreview(ctx context.Context, deps DeployStaticSitePreviewDeps, rawArgs json.RawMessage) (string, error) {
	var args struct {
		PublishDir string `json:"publish_dir"`
		IsSPA      bool   `json:"is_spa"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if strings.TrimSpace(args.PublishDir) == "" {
		return "", fmt.Errorf("publish_dir is required")
	}
	// The reuse key is derived solely from publish_dir (repo + build-dir) — deterministic
	// and reproducible across steps/runners, with no name for the agent to remember. A
	// publish_dir that yields no usable key is rejected (not collapsed to a shared name).
	serviceName, ok := deriveServiceName(args.PublishDir)
	if !ok {
		return "", fmt.Errorf("could not derive a service name from publish_dir %q — point it at the built output inside your repo (e.g. %q)", args.PublishDir, "<repo>/dist")
	}
	distDir := resolvePublishDir(deps.WorkDirHost, args.PublishDir)

	// Resolve the persisted preview identity (find-or-create). The record is the
	// source of truth for the deployment id + any existing distribution.
	previewID, existing, err := deps.Store.EnsurePreview(serviceName)
	if err != nil {
		return "", fmt.Errorf("ensure preview record: %w", err)
	}

	s3Client, cfClient, err := deps.BuildClients()
	if err != nil {
		return "", fmt.Errorf("build cloud clients: %w", err)
	}

	var tail bytes.Buffer
	logs := io.MultiWriter(deps.LogsWriter, &tail)
	res, err := DeployStaticSitePreview(StaticPreviewDeployInput{
		OrgID:            deps.OrgID,
		PreviewID:        previewID,
		DistDirectory:    distDir,
		Region:           deps.Region,
		IsSPA:            args.IsSPA,
		ExistingDistID:   existing.CloudFrontDistributionID,
		S3Client:         s3Client,
		CloudfrontClient: cfClient,
		SkipDeployWait:   true, // return promptly; the CDN propagates async (verify_preview_reachable polls)
	}, logs)
	if err != nil {
		return "", fmt.Errorf("deploy preview: %w", err)
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

	out := deployStaticSitePreviewResult{
		URL:            "https://" + domain,
		Status:         "deployed",
		DistributionID: distID,
		Note:           "CDN propagation may take a few minutes before the URL serves the latest content.",
		LogTail:        tailString(tail.Bytes(), logTailMaxBytes),
	}
	b, err := json.Marshal(out)
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
