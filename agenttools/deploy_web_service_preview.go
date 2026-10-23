package agenttools

// deploy_web_service_preview.go implements the agent-invoked `deploy_web_service_preview`
// MCP tool: the backend preview of a repository deployment.io deploys as a web service
// built from a Dockerfile. The runner builds the image from a copy of the agent's
// working tree, with the org's preview configuration, and deploys it through the same
// commands production uses — ECR, the preview's own load balancer, an ECS service with
// Service Connect — then puts a CloudFront distribution in front of the load balancer
// so the preview has an https URL. The deploy itself is injected (Deploy) because the
// commands package owns those steps.
//
// Like deploy_static_site_preview's runner-built path, a deploy runs in the background,
// one per service; a call waits for it at most previewBuildCallWait and a later call
// with the same arguments attaches to it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

// DeployWebServicePreviewDeps is the task-scoped context the deploy_web_service_preview
// handler closes over, built once per agent run.
type DeployWebServicePreviewDeps struct {
	Region      string    // the runner's region string, e.g. "us-east-1"
	WorkDirHost string    // host path of the bind-mounted /work
	LogsWriter  io.Writer // the Step Job's log writer; deploy progress streams here

	// Store find-or-creates the persisted web-service preview record and saves the
	// CloudFront distribution back onto it.
	Store PreviewStore

	// Record is the agent run's preview record, shared with the static tool and the
	// verify tools: the URL this tool returns is added to it.
	Record *PreviewRecord

	// Repositories are the Task's repositories in checkout order, as for
	// DeployStaticSitePreviewDeps.
	Repositories []tasks.RepositoryEntry

	// Settings asks deployment-server how the org deploys the repository at cloneURL
	// as a web service, with the org's preview configuration and the region's
	// network. The configuration's values reach the deploy and nothing else.
	Settings func(cloneURL string) (task_previews.WebServicePreviewSettingsReplyV1, error)

	// Deploy builds and deploys the preview with production's web-service steps and
	// returns its load balancer's DNS name. A refusal is returned as is; a failed
	// step as WebPreviewDeployFailed.
	Deploy func(ctx context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error)

	// BuildCloudFront lazily constructs the CloudFront client (runner IAM role).
	BuildCloudFront func() (WebPreviewCloudFront, error)

	// distributions remembers the distributions this agent run created
	// (RegisterDeployWebServicePreview sets it).
	distributions *webPreviewDistributions
}

// webPreviewDistributions are the CloudFront distributions an agent run created, by
// preview id. SavePreview only queues a save, so a later deploy's EnsurePreview can
// still return none; without these it would create a second distribution and the
// preview's URL would change.
type webPreviewDistributions struct {
	mu   sync.Mutex
	byID map[string]PreviewState
}

func newWebPreviewDistributions() *webPreviewDistributions {
	return &webPreviewDistributions{byID: map[string]PreviewState{}}
}

func (d *webPreviewDistributions) remember(previewID string, s PreviewState) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byID[previewID] = s
}

// fill returns existing with the distribution this run created for previewID when
// existing has none on record yet.
func (d *webPreviewDistributions) fill(previewID string, existing PreviewState) PreviewState {
	if d == nil || existing.CloudFrontDistributionID != "" {
		return existing
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.byID[previewID]; ok {
		existing.CloudFrontDistributionID = s.CloudFrontDistributionID
		existing.CloudFrontDistributionArn = s.CloudFrontDistributionArn
		existing.CloudFrontDomainName = s.CloudFrontDomainName
	}
	return existing
}

// WebServicePreviewDeployInput is one backend preview deploy.
type WebServicePreviewDeployInput struct {
	PreviewID string // the preview Deployment's id
	// ServiceKey is the preview's service name in the Task (its record's key).
	ServiceKey string
	// BuildDir is the build copy directory (PreviewBuildSource), beside the work dir.
	BuildDir string
	// RootDirectory is Service.RootDirectory, cleaned: "" for the repository root.
	RootDirectory string
	Service       task_previews.WebServicePreviewSettingsV1
	// Settings is the reply Service came from: the preview configuration, the
	// network and the preview namespace.
	Settings task_previews.WebServicePreviewSettingsReplyV1
	// Existing is what EnsurePreview returned: the handles a prior deploy persisted.
	Existing PreviewState
	// BuildLogs gets the image build's output: the Step log only, since a build can
	// print the preview configuration. Logs gets every later step's.
	BuildLogs io.Writer
	Logs      io.Writer
}

// WebServicePreviewDeployOutput is what a deploy that got as far as the load balancer
// returns.
type WebServicePreviewDeployOutput struct {
	LoadBalancerDNS string
}

// WebPreviewDeployFailed is the error of a failed step of a backend preview deploy.
func WebPreviewDeployFailed(err error) error {
	return fmt.Errorf("The backend preview's deploy failed: %w", err)
}

// webPreviewDeployingMessage is a call's answer while its service's deploy is still
// running.
const webPreviewDeployingMessage = "The backend preview is still deploying (a new one takes about 5 minutes). Call deploy_web_service_preview again with the same arguments to wait for it."

// deployWebServicePreviewResult is the JSON the agent receives from a tools/call.
// It never contains a value of the preview configuration.
type deployWebServicePreviewResult struct {
	URL        string `json:"url"`
	HealthURL  string `json:"health_url"`
	Status     string `json:"status"`
	Deployment string `json:"deployment"`
	Note       string `json:"note,omitempty"`
	LogTail    string `json:"log_tail,omitempty"`
}

const deployWebServicePreviewInputSchema = `{
  "type": "object",
  "properties": {
    "repository": {
      "type": "string",
      "description": "The repository's directory under /work (e.g. \"0-acme/api\") that deployment.io deploys as a web service."
    },
    "root_directory": {
      "type": "string",
      "description": "Only when the tool asks for it: the root directory of the web service to preview, for a repository deployment.io deploys as several web services (\"\" for the repository root)."
    }
  },
  "required": ["repository"]
}`

// RegisterDeployWebServicePreview registers the deploy_web_service_preview tool. The
// returned stop waits for the deploys still running (their steps can't be cancelled)
// and their build copies to be removed; call it when the agent run ends.
func RegisterDeployWebServicePreview(s *agentmcp.Server, deps DeployWebServicePreviewDeps) (stop func()) {
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	if deps.distributions == nil {
		deps.distributions = newWebPreviewDistributions()
	}
	s.Register(agentmcp.Tool{
		Name: "deploy_web_service_preview",
		Description: "Deploy the backend preview of a repository deployment.io deploys as a web service built from a " +
			"Dockerfile, and return its https URL. deployment.io builds the image from your working tree with the service's " +
			"Dockerfile and the org's preview configuration (you never see that configuration) and deploys it the way " +
			"production does, on its own load balancer behind CloudFront. Pass repository (its directory under /work), and " +
			"root_directory only when the tool asks for it. A deploy takes several minutes: while it runs the result is " +
			"status \"deploying\" — call again with the same arguments to wait for it. Status \"deployed\" means the service " +
			"was created or updated, not that it's serving yet: call verify_preview_reachable on health_url until it is live " +
			"(a new preview takes a few minutes). Re-call to redeploy after changes; the same URL is reused.",
		InputSchema: json.RawMessage(deployWebServicePreviewInputSchema),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handleDeployWebServicePreview(ctx, deps, builds, args)
		},
	})
	return builds.stop
}

// webServicePreviewPlan is everything a backend preview deploy needs, decided before
// it starts.
type webServicePreviewPlan struct {
	repository    string // "<idx>-<name>"
	service       task_previews.WebServicePreviewSettingsV1
	rootDirectory string // service.RootDirectory, cleaned
	serviceName   string
	settings      task_previews.WebServicePreviewSettingsReplyV1
}

func handleDeployWebServicePreview(ctx context.Context, deps DeployWebServicePreviewDeps, builds *previewBuilds, rawArgs json.RawMessage) (string, error) {
	var args struct {
		Repository    string  `json:"repository"`
		RootDirectory *string `json:"root_directory"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if strings.TrimSpace(args.Repository) == "" {
		return "", errors.New("repository is required: the repository's directory under /work")
	}
	started := time.Now()
	request := strings.TrimSpace(args.Repository) + "\x00"
	if args.RootDirectory != nil {
		request += "root:" + trimRootDirectory(*args.RootDirectory)
	}
	if out, err, ok := builds.attach(ctx, request, previewBuildCallWait); ok {
		return out, err
	}
	plan, err := planWebServicePreview(deps, args.Repository, args.RootDirectory)
	if err != nil {
		return "", err
	}
	return builds.callFor(ctx, request, plan.serviceName, func(buildCtx context.Context) (string, error) {
		return runWebServicePreview(buildCtx, deps, plan)
	}, previewBuildCallWait-time.Since(started))
}

// planWebServicePreview resolves repository among the Task's repositories, asks
// deployment-server how it's deployed as a web service and chooses the service.
func planWebServicePreview(deps DeployWebServicePreviewDeps, repository string, rootDirectory *string) (webServicePreviewPlan, error) {
	repoDir, entry, err := resolveTaskRepository(deps.Repositories, repository)
	if err != nil {
		return webServicePreviewPlan{}, err
	}
	if deps.Settings == nil || deps.Deploy == nil {
		return webServicePreviewPlan{}, fmt.Errorf("deployment.io can't deploy backend previews here")
	}
	settings, err := lookupWithin(deps.Settings, entry.CloneURL,
		"deployment.io didn't answer in time; call deploy_web_service_preview again")
	if err != nil {
		return webServicePreviewPlan{}, fmt.Errorf("look up how deployment.io deploys %s: %w", repoDir, err)
	}
	service, err := chooseWebService(settings, repoDir, rootDirectory, deps.Region)
	if err != nil {
		return webServicePreviewPlan{}, err
	}
	root, err := cleanRelativeDir(service.RootDirectory)
	if err != nil {
		return webServicePreviewPlan{}, fmt.Errorf("%s's root directory: %w", service.DeploymentName, err)
	}
	serviceName, ok := webPreviewServiceKey(repoDir, root)
	if !ok {
		return webServicePreviewPlan{}, fmt.Errorf("could not derive a service name for %s", service.DeploymentName)
	}
	return webServicePreviewPlan{
		repository:    repoDir,
		service:       service,
		rootDirectory: root,
		serviceName:   serviceName,
		settings:      settings,
	}, nil
}

// webPreviewServiceKey is the key of the backend preview of the service at root in
// repoDir: deriveServiceName's readable key with a hash of the exact repository and
// root appended, since that key collapses distinct paths ("services/api-v1" and
// "services/api/v1") into one. The repository's "<idx>-" prefix is left out of the
// hash, as deriveServiceName leaves it out of the key.
func webPreviewServiceKey(repoDir, root string) (string, bool) {
	key, ok := deriveServiceName(repoDir + "/" + root)
	if !ok {
		return "", false
	}
	repo := strings.Trim(filepath.ToSlash(repoDir), "/")
	if i := strings.IndexByte(repo, '-'); i > 0 && isAllDigits(repo[:i]) {
		repo = repo[i+1:]
	}
	sum := sha256.Sum256([]byte(repo + "\x00" + root))
	return key + "-" + hex.EncodeToString(sum[:4]), true
}

// chooseWebService picks the web service to preview from settings: the one whose
// root directory is rootDirectory (when given) and whose region is region. Every
// other outcome is the refusal the agent sees.
func chooseWebService(settings task_previews.WebServicePreviewSettingsReplyV1, repository string, rootDirectory *string, region string) (task_previews.WebServicePreviewSettingsV1, error) {
	if len(settings.Services) == 0 {
		if len(settings.RuntimeBuiltServices) == 0 {
			return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("deployment.io doesn't deploy %s as a web service, so it has no backend preview.", repository)
		}
		return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("%s build from a language runtime; backend previews need a Dockerfile.", strings.Join(settings.RuntimeBuiltServices, ", "))
	}
	if !settings.NetworkReady {
		return task_previews.WebServicePreviewSettingsV1{}, errors.New(settings.NetworkMissingReason)
	}
	candidates := settings.Services
	if rootDirectory != nil {
		want := trimRootDirectory(*rootDirectory)
		var matches []task_previews.WebServicePreviewSettingsV1
		for _, s := range candidates {
			if trimRootDirectory(s.RootDirectory) == want {
				matches = append(matches, s)
			}
		}
		if len(matches) == 0 {
			return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("%s has no web service with root directory %q here (%s).", repository, want, webServiceRoots(candidates))
		}
		candidates = matches
	}
	var inRegion []task_previews.WebServicePreviewSettingsV1
	for _, s := range candidates {
		if s.Region == region {
			inRegion = append(inRegion, s)
		}
	}
	switch len(inRegion) {
	case 0:
		return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("%s's web service is deployed in %s, not in this runner's region (%s).", repository, webServiceRegions(candidates), region)
	case 1:
		return inRegion[0], nil
	}
	names := make([]string, 0, len(inRegion))
	for _, s := range inRegion {
		names = append(names, s.DeploymentName)
	}
	return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("%s is deployed as several web services here (%s); pass root_directory to choose one.", repository, strings.Join(names, ", "))
}

// trimRootDirectory is a root directory as chooseWebService compares it: surrounding
// space, a leading "./" and surrounding "/" trimmed; "." is the repository root, "".
func trimRootDirectory(dir string) string {
	d := strings.TrimSpace(dir)
	d = strings.TrimPrefix(d, "./")
	d = strings.Trim(d, "/")
	if d == "." {
		return ""
	}
	return d
}

// webServiceRegions lists the distinct regions of services, sorted.
func webServiceRegions(services []task_previews.WebServicePreviewSettingsV1) string {
	seen := map[string]bool{}
	var regions []string
	for _, s := range services {
		if !seen[s.Region] {
			seen[s.Region] = true
			regions = append(regions, s.Region)
		}
	}
	sort.Strings(regions)
	return strings.Join(regions, ", ")
}

// webServiceRoots lists each service with its root directory.
func webServiceRoots(services []task_previews.WebServicePreviewSettingsV1) string {
	parts := make([]string, 0, len(services))
	for _, s := range services {
		parts = append(parts, fmt.Sprintf("%s: root_directory %q", s.DeploymentName, trimRootDirectory(s.RootDirectory)))
	}
	return strings.Join(parts, "; ")
}

// runWebServicePreview deploys p from a copy of the agent's working tree and puts a
// CloudFront distribution in front of it on the first deploy, removing the copy in
// every case.
func runWebServicePreview(ctx context.Context, deps DeployWebServicePreviewDeps, p webServicePreviewPlan) (string, error) {
	previewID, existing, err := ensurePreview(ctx, deps.Store, p.serviceName)
	if err != nil {
		return "", fmt.Errorf("ensure preview record: %w", err)
	}
	existing = deps.distributions.fill(previewID, existing)
	copyDir, err := PreviewBuildSource(ctx, DeployStaticSitePreviewDeps{WorkDirHost: deps.WorkDirHost, LogsWriter: deps.LogsWriter}, p.repository)
	if copyDir != "" {
		defer os.RemoveAll(copyDir)
	}
	if err != nil {
		return "", fmt.Errorf("copy %s for the preview build: %w", p.repository, err)
	}
	serviceDir := filepath.Join(copyDir, p.rootDirectory)
	if info, err := os.Stat(serviceDir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s's root directory %q isn't a directory in %s", p.service.DeploymentName, p.service.RootDirectory, p.repository)
	}
	if err := checkInside(copyDir, serviceDir); err != nil {
		return "", fmt.Errorf("%s's root directory: %w", p.service.DeploymentName, err)
	}

	var tail bytes.Buffer
	logs := io.MultiWriter(deps.LogsWriter, &tail)
	io.WriteString(deps.LogsWriter, fmt.Sprintf("Backend preview: %s built by deployment.io like %s\n", p.serviceName, webServiceDeploymentName(p.service)))
	out, err := deps.Deploy(ctx, WebServicePreviewDeployInput{
		PreviewID:     previewID,
		ServiceKey:    p.serviceName,
		BuildDir:      copyDir,
		RootDirectory: p.rootDirectory,
		Service:       p.service,
		Settings:      p.settings,
		Existing:      existing,
		BuildLogs:     deps.LogsWriter,
		Logs:          logs,
	})
	if err != nil {
		return "", err
	}
	domain, err := ensureWebPreviewDistribution(ctx, deps, previewID, existing, out.LoadBalancerDNS, logs)
	if err != nil {
		return "", err
	}

	previewURL := "https://" + domain
	deps.Record.AddURL(previewURL)
	return marshalResult(deployWebServicePreviewResult{
		URL:        previewURL,
		HealthURL:  previewURL + healthCheckPath(p.service.HealthCheckPath),
		Status:     "deployed",
		Deployment: webServiceDeploymentName(p.service),
		Note: "The ECS service was created or updated; it isn't known to be serving yet. Call verify_preview_reachable on " +
			"health_url until it is live — a new preview takes a few minutes to start and for CloudFront to propagate.",
		LogTail: tailString(tail.Bytes(), logTailMaxBytes),
	})
}

// ensureWebPreviewDistribution returns the domain of the preview's CloudFront
// distribution: the existing one, left as it is, or one created in front of the load
// balancer at loadBalancerDNS and saved on the preview record.
func ensureWebPreviewDistribution(ctx context.Context, deps DeployWebServicePreviewDeps, previewID string, existing PreviewState, loadBalancerDNS string, logs io.Writer) (string, error) {
	if existing.CloudFrontDistributionID != "" {
		if existing.CloudFrontDomainName == "" {
			return "", WebPreviewDeployFailed(fmt.Errorf("the preview's distribution %s has no domain on record", existing.CloudFrontDistributionID))
		}
		io.WriteString(logs, fmt.Sprintf("Reusing the preview's CloudFront distribution: %s\n", existing.CloudFrontDistributionID))
		return existing.CloudFrontDomainName, nil
	}
	if deps.BuildCloudFront == nil {
		return "", WebPreviewDeployFailed(errors.New("no CloudFront client"))
	}
	cf, err := deps.BuildCloudFront()
	if err != nil {
		return "", WebPreviewDeployFailed(fmt.Errorf("build the CloudFront client: %w", err))
	}
	dist, err := createWebPreviewDistribution(ctx, cf, previewID, loadBalancerDNS, logs)
	if err != nil {
		return "", WebPreviewDeployFailed(err)
	}
	saved := PreviewState{
		CloudFrontDistributionID:  dist.id,
		CloudFrontDistributionArn: dist.arn,
		CloudFrontDomainName:      dist.domain,
	}
	deps.distributions.remember(previewID, saved)
	deps.Store.SavePreview(previewID, saved)
	return dist.domain, nil
}

// webServiceDeploymentName names the production deployment a preview is built like.
func webServiceDeploymentName(s task_previews.WebServicePreviewSettingsV1) string {
	return fmt.Sprintf("%s (%s)", s.DeploymentName, s.EnvironmentName)
}

// healthCheckPath is path with a leading "/", "/" when empty.
func healthCheckPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

// lookupWithin calls lookup, giving up after PreviewBuildSettingsTimeout with
// timeoutMessage so a stalled request never holds the tool call past the agent CLI's
// timeout.
func lookupWithin[T any](lookup func(string) (T, error), cloneURL, timeoutMessage string) (T, error) {
	type answer struct {
		reply T
		err   error
	}
	ch := make(chan answer, 1)
	go func() {
		reply, err := lookup(cloneURL)
		ch <- answer{reply, err}
	}()
	timer := time.NewTimer(PreviewBuildSettingsTimeout)
	defer timer.Stop()
	select {
	case a := <-ch:
		return a.reply, a.err
	case <-timer.C:
		var zero T
		return zero, errors.New(timeoutMessage)
	}
}

// WebPreviewCloudFront is the part of *cloudfront.Client a backend preview uses.
type WebPreviewCloudFront interface {
	CreateDistribution(ctx context.Context, params *cloudfront.CreateDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error)
}

// WritePreviewFiles writes the preview configuration's secret files into dir, as
// the static preview's build does (writePreviewFiles).
func WritePreviewFiles(dir string, files []task_previews.PreviewFileV1) error {
	return writePreviewFiles(dir, files)
}

// CheckInside returns an error unless p, its symlinks resolved, is inside base.
func CheckInside(base, p string) error {
	return checkInside(base, p)
}
