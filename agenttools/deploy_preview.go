package agenttools

// deploy_preview.go implements the agent-invoked `deploy_preview` MCP tool: the
// coding agent, after building a static site inside its /work tree, calls this to
// stand up (or refresh) a live preview on the project's cloud and get back a URL.
//
// C2 (thin walking skeleton): entirely runner-side. The preview is keyed by the
// Task id, deployed via DeployStaticSitePreview using the runner's own IAM role +
// region — no control-plane record, no deployment-server round trip.
//
// Cross-run distribution reuse is STATELESS: the source of truth is the CloudFront
// account itself, found on the first call per process by matching the distribution
// Comment (discoverExistingPreviewDist). That is what makes reuse correct across
// horizontally-scaled runners — every BYO runner shares the org's cloud account, so
// any of them finds the task's existing distribution. The in-memory cache below is
// ONLY a within-process fast-path (empty on every other runner and on this runner's
// next Step) — it never carries reuse across processes.
//
// This is thin, not final: the self-discovery is best-effort — if
// cloudfront:ListDistributions is denied or eventually-consistent and misses, the
// create path re-runs and collides on the account-global OAC/cache-policy names.
// C4 replaces both the taskID keying and the lookup with a persisted
// ephemeral-Environment + Deployment record (kit #194) — authoritative shared state
// every runner reads — which also surfaces previews in the dashboard.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

// containerWorkDir is where the agent's working tree is mounted inside the
// agentbox container (mirrors run_agent_step's agentboxWorkDirInContainer). Used
// only to defensively strip an absolute container path the agent might pass for
// publish_dir; the real filesystem root is DeployPreviewDeps.WorkDirHost.
const containerWorkDir = "/work"

// logTailMaxBytes caps the deploy log echoed back to the agent in the tool result.
const logTailMaxBytes = 2000

// DeployPreviewDeps is the task-scoped context the deploy_preview handler closes
// over. Built once per RunAgentStep from the job's TaskJobContext + the runner's
// own identity, then handed to RegisterDeployPreview.
type DeployPreviewDeps struct {
	OrgID       string    // organization id (resource naming)
	TaskID      string    // owning task; the preview key (bucket/dist naming) for C2
	Region      string    // the runner's region string, e.g. "us-east-1"
	WorkDirHost string    // host path of the bind-mounted /work; publish_dir resolves under it
	LogsWriter  io.Writer // the Step Job's log writer; deploy progress streams here

	// BuildClients lazily constructs the AWS clients (runner IAM role + region).
	// A closure so cloud_api_clients + the Region job-param wiring stay in the
	// commands package and this handler only runs it when a preview is requested.
	BuildClients func() (*s3.Client, *cloudfront.Client, error)
}

// previewDist is the remembered identity of a preview's CloudFront distribution.
type previewDist struct {
	distID string
	domain string
}

// deployPreviewResult is the JSON the agent receives from a tools/call.
type deployPreviewResult struct {
	URL            string `json:"url"`
	Status         string `json:"status"`
	DistributionID string `json:"distribution_id"`
	Note           string `json:"note,omitempty"`
	LogTail        string `json:"log_tail,omitempty"`
}

const deployPreviewInputSchema = `{
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

// RegisterDeployPreview registers the deploy_preview tool on s, closing over the
// per-task deps and a per-container distribution cache. Call once per agent
// container (alongside RegisterPing), before the server starts serving.
func RegisterDeployPreview(s *agentmcp.Server, deps DeployPreviewDeps) {
	var mu sync.Mutex
	cache := map[string]previewDist{}
	s.Register(agentmcp.Tool{
		Name: "deploy_preview",
		Description: "Deploy the built static site to a live preview URL on the project's cloud and return the URL. " +
			"Call this AFTER the site is built — publish_dir (relative to /work) must contain index.html. " +
			"Set is_spa=true for single-page apps with client-side routing. Re-call to redeploy after changes; the same preview URL is reused.",
		InputSchema: json.RawMessage(deployPreviewInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleDeployPreview(ctx, deps, &mu, cache, args)
		},
	})
}

func handleDeployPreview(ctx context.Context, deps DeployPreviewDeps, mu *sync.Mutex, cache map[string]previewDist, rawArgs json.RawMessage) (string, error) {
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
	previewID := deps.TaskID
	if previewID == "" {
		return "", fmt.Errorf("no task id in preview context")
	}
	distDir := resolvePublishDir(deps.WorkDirHost, args.PublishDir)

	s3Client, cfClient, err := deps.BuildClients()
	if err != nil {
		return "", fmt.Errorf("build cloud clients: %w", err)
	}

	// Resolve the existing distribution to reuse (empty => first deploy creates one).
	// The cache is a per-process fast-path; discoverExistingPreviewDist is the actual
	// cross-runner reuse mechanism (it reads the shared cloud account).
	mu.Lock()
	known, ok := cache[previewID]
	mu.Unlock()
	if !ok {
		distID, domain, derr := discoverExistingPreviewDist(ctx, cfClient, previewID)
		switch {
		case derr != nil:
			// The lookup is the ONLY cross-process reuse mechanism, so a failure here
			// (e.g. cloudfront:ListDistributions denied, or eventual consistency) means
			// a later Step can't find this distribution and will collide on the
			// account-global OAC name when it recreates. Surface it, don't swallow it.
			io.WriteString(deps.LogsWriter, fmt.Sprintf(
				"warning: could not check for an existing preview distribution (%v); treating as first deploy — a later redeploy may fail if one already exists\n", derr))
		case distID != "":
			known = previewDist{distID: distID, domain: domain}
		}
	}

	var tail bytes.Buffer
	logs := io.MultiWriter(deps.LogsWriter, &tail)
	res, err := DeployStaticSitePreview(StaticPreviewDeployInput{
		OrgID:            deps.OrgID,
		PreviewID:        previewID,
		DistDirectory:    distDir,
		Region:           deps.Region,
		IsSPA:            args.IsSPA,
		ExistingDistID:   known.distID,
		S3Client:         s3Client,
		CloudfrontClient: cfClient,
		SkipDeployWait:   true, // return promptly; the CDN propagates async (C3 polls until live)
	}, logs)
	if err != nil {
		return "", fmt.Errorf("deploy preview: %w", err)
	}

	// The create path returns the fresh id + domain; the reuse path returns only
	// the id (it just invalidated), so fall back to what we already knew.
	distID := res.DistributionID
	if distID == "" {
		distID = known.distID
	}
	domain := res.DomainName
	if domain == "" {
		domain = known.domain
	}
	mu.Lock()
	cache[previewID] = previewDist{distID: distID, domain: domain}
	mu.Unlock()

	out := deployPreviewResult{
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
// workDirHost. It strips a leading container /work prefix (in case the agent
// passed an absolute container path) and neutralizes any ../ traversal by
// resolving the path as if rooted at /, so the result can never escape workDirHost.
func resolvePublishDir(workDirHost, publishDir string) string {
	p := strings.TrimSpace(publishDir)
	p = strings.TrimPrefix(p, containerWorkDir) // "/work/dist" -> "/dist"; "dist" unchanged
	rel := strings.TrimPrefix(filepath.Clean("/"+p), "/")
	return filepath.Join(workDirHost, rel)
}

// discoverExistingPreviewDist looks for a CloudFront distribution already serving
// this preview, matched by the Comment DeployStaticSitePreview stamps on create
// ("Preview distribution for <previewID>"). Lets a later Step reuse a distribution
// an earlier Step created instead of duplicating it. Returns ("","",nil) when none
// exists. C2-thin only — C4's persisted Deployment record makes this unnecessary.
func discoverExistingPreviewDist(ctx context.Context, cfClient *cloudfront.Client, previewID string) (string, string, error) {
	want := "Preview distribution for " + previewID
	input := &cloudfront.ListDistributionsInput{}
	for {
		out, err := cfClient.ListDistributions(ctx, input)
		if err != nil {
			return "", "", err
		}
		if out.DistributionList == nil {
			return "", "", nil
		}
		for i := range out.DistributionList.Items {
			d := out.DistributionList.Items[i]
			if aws.ToString(d.Comment) == want {
				return aws.ToString(d.Id), aws.ToString(d.DomainName), nil
			}
		}
		if out.DistributionList.IsTruncated != nil && *out.DistributionList.IsTruncated {
			input.Marker = out.DistributionList.NextMarker
			continue
		}
		return "", "", nil
	}
}

// tailString returns the last max bytes of b as a string, prefixed with an
// ellipsis when truncated, on a rune boundary so the JSON stays valid UTF-8.
func tailString(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	t := b[len(b)-max:]
	// Advance to the next rune boundary so we don't slice a multibyte rune.
	for len(t) > 0 && t[0]&0xC0 == 0x80 {
		t = t[1:]
	}
	return "…" + string(t)
}
