package agenttools

// deploy_preview.go implements the agent-invoked `deploy_preview` MCP tool: the
// coding agent, after building a static site inside its /work tree, calls this to
// stand up (or refresh) a live preview on the project's cloud and get back a URL.
//
// C4: the preview is a persisted control-plane record. On each call the tool asks
// deployment-server (via EnsurePreview) to find-or-create the task's ephemeral
// Environment + the lean static Deployment for this service, and hands back that
// Deployment's id (bucket/resource naming) + the distribution id/domain persisted
// from a prior deploy. So distribution reuse is stateless and correct ACROSS runners
// — the record is the source of truth, no in-memory cache and no CloudFront
// self-discovery. After a first deploy the tool persists the new distribution back
// via SaveDistribution so later calls/steps/runners reuse it.

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
	// defaultPreviewServiceName names the static service when the agent doesn't.
	defaultPreviewServiceName = "static-site"
)

// DeployPreviewDeps is the task-scoped context the deploy_preview handler closes
// over, built once per RunAgentStep.
type DeployPreviewDeps struct {
	OrgID       string    // organization id (bucket naming)
	Region      string    // the runner's region string, e.g. "us-east-1"
	WorkDirHost string    // host path of the bind-mounted /work; publish_dir resolves under it
	LogsWriter  io.Writer // the Step Job's log writer; deploy progress streams here

	// BuildClients lazily constructs the AWS clients (runner IAM role + region).
	BuildClients func() (*s3.Client, *cloudfront.Client, error)

	// EnsurePreview resolves (creating if needed) the persisted preview identity for
	// the named service under this task's ephemeral env: the Deployment id (bucket +
	// resource naming) and the distribution id/domain persisted from a prior deploy
	// (empty on the first). Idempotent — round-trips deployment-server.
	EnsurePreview func(serviceName string) (previewID, existingDistID, existingDomain string, err error)

	// SaveDistribution persists a freshly created distribution back onto the preview
	// Deployment record so the next call/step/runner reuses it.
	SaveDistribution func(previewID, distID, arn, domain string)
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
    },
    "name": {
      "type": "string",
      "description": "Optional service name within this task's preview (default \"static-site\"). Use distinct names when a task previews more than one static service."
    }
  },
  "required": ["publish_dir"]
}`

// RegisterDeployPreview registers the deploy_preview tool.
func RegisterDeployPreview(s *agentmcp.Server, deps DeployPreviewDeps) {
	s.Register(agentmcp.Tool{
		Name: "deploy_preview",
		Description: "Deploy the built static site to a live preview URL on the project's cloud and return the URL. " +
			"Call this AFTER a successful build — publish_dir (relative to /work) must be the output of your REAL build " +
			"command and contain index.html. NEVER hand-create files to deploy; if the build fails, report that instead " +
			"of deploying a placeholder. Set is_spa=true for single-page apps with client-side routing. Re-call to " +
			"redeploy after changes (the same preview URL is reused), then use verify_preview to confirm it's live.",
		InputSchema: json.RawMessage(deployPreviewInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleDeployPreview(ctx, deps, args)
		},
	})
}

func handleDeployPreview(ctx context.Context, deps DeployPreviewDeps, rawArgs json.RawMessage) (string, error) {
	var args struct {
		PublishDir string `json:"publish_dir"`
		IsSPA      bool   `json:"is_spa"`
		Name       string `json:"name"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if strings.TrimSpace(args.PublishDir) == "" {
		return "", fmt.Errorf("publish_dir is required")
	}
	serviceName := strings.TrimSpace(args.Name)
	if serviceName == "" {
		serviceName = defaultPreviewServiceName
	}
	distDir := resolvePublishDir(deps.WorkDirHost, args.PublishDir)

	// Resolve the persisted preview identity (find-or-create). The record is the
	// source of truth for the deployment id + any existing distribution.
	previewID, existingDistID, existingDomain, err := deps.EnsurePreview(serviceName)
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
		ExistingDistID:   existingDistID,
		S3Client:         s3Client,
		CloudfrontClient: cfClient,
		SkipDeployWait:   true, // return promptly; the CDN propagates async (verify_preview polls)
	}, logs)
	if err != nil {
		return "", fmt.Errorf("deploy preview: %w", err)
	}

	// A first deploy returns the fresh id + domain — persist them. A reuse returns
	// only the id (nothing new to save), so fall back to the persisted domain.
	distID := res.DistributionID
	if distID == "" {
		distID = existingDistID
	}
	domain := res.DomainName
	if domain == "" {
		domain = existingDomain
	}
	if res.DomainName != "" && deps.SaveDistribution != nil {
		deps.SaveDistribution(previewID, res.DistributionID, res.DistributionArn, res.DomainName)
	}

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
// workDirHost. It strips a leading container /work prefix and neutralizes any ../
// traversal by resolving as if rooted at /, so the result can never escape
// workDirHost.
func resolvePublishDir(workDirHost, publishDir string) string {
	p := strings.TrimSpace(publishDir)
	p = strings.TrimPrefix(p, containerWorkDir) // "/work/dist" -> "/dist"; "dist" unchanged
	rel := strings.TrimPrefix(filepath.Clean("/"+p), "/")
	return filepath.Join(workDirHost, rel)
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
