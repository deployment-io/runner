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

	// DatabaseStore find-or-creates the persisted record of the preview's own
	// database, a private service of the Task ("<service key>-db"). Nil: no
	// preview databases.
	DatabaseStore PreviewStore

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

	// MergeResult, when set, gives the preview's build the merge result of its
	// repository instead of a copy of the working tree, exactly as
	// DeployStaticSitePreviewDeps.MergeResult does. Set by the QA stage only; nil
	// (implement and fix runs) builds the branch as built.
	MergeResult MergeResultProvider

	// Distributions remembers the distributions created by the deploys that share
	// it: RegisterDeployWebServicePreview makes one per agent run when unset, and
	// the QA stage passes one to every round.
	Distributions *WebPreviewDistributions
}

// WebPreviewDistributions are the CloudFront distributions created by the deploys
// that share it, by preview id. SavePreview only queues a save, so a later deploy's
// EnsurePreview can still return none; without these it would create a second
// distribution and the preview's URL would change.
type WebPreviewDistributions struct {
	mu   sync.Mutex
	byID map[string]PreviewState
}

// NewWebPreviewDistributions is an empty distributions memo.
func NewWebPreviewDistributions() *WebPreviewDistributions {
	return &WebPreviewDistributions{byID: map[string]PreviewState{}}
}

func (d *WebPreviewDistributions) remember(previewID string, s PreviewState) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byID[previewID] = s
}

// fill returns existing with the distribution this run created for previewID when
// existing has none on record yet.
func (d *WebPreviewDistributions) fill(previewID string, existing PreviewState) PreviewState {
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
	// Database is the preview's own database, deployed before the API, which
	// then runs against it (and the preview migrate command, if set, with it). Nil: none, and
	// the API deploys exactly as without one.
	Database *PreviewDatabaseDeploy
	// WiredNames are the names of Settings.Variables the QA stage set (sorted; nil
	// for the tool). The database's value of such a name replaces it in the build
	// args as well as the environment.
	WiredNames []string
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

// WebServicePreviewDeployed is a backend preview deploy that finished: the ECS
// service was created or updated, not necessarily serving yet.
type WebServicePreviewDeployed struct {
	URL        string
	HealthURL  string
	Deployment string // the production deployment it's built like
	// database is the preview's own database (the tool's result reports it);
	// DatabaseNote says why it has none.
	database     *previewDatabaseResult
	DatabaseNote string
	LogTail      string
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
	// Database is the preview's own database; DatabaseNote says why it has none.
	Database     *previewDatabaseResult `json:"database,omitempty"`
	DatabaseNote string                 `json:"database_note,omitempty"`
	LogTail      string                 `json:"log_tail,omitempty"`
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
	if deps.Distributions == nil {
		deps.Distributions = NewWebPreviewDistributions()
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
			"(a new preview takes a few minutes). Re-call to redeploy after changes; the same URL is reused. When the service's " +
			"code needs a Postgres, MySQL or MongoDB database, deployment.io runs a throwaway one beside the preview, points the " +
			"service's database variables at it and, when a person set a preview migrate command for the service, runs it " +
			"against the database first: " +
			"`database` in the result says which, from which files, and whether migrations ran; `database_note` says why " +
			"there is none.",
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
	// wired are variables the QA stage sets on top of the preview configuration's
	// (the origins of the site previews that call it); nil for the tool.
	wired map[string]string
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
		deployed, err := runWebServicePreview(buildCtx, deps, plan)
		if err != nil {
			return "", err
		}
		return marshalResult(deployWebServicePreviewResult{
			URL:        deployed.URL,
			HealthURL:  deployed.HealthURL,
			Status:     "deployed",
			Deployment: deployed.Deployment,
			Note: "The ECS service was created or updated; it isn't known to be serving yet. Call verify_preview_reachable on " +
				"health_url until it is live — a new preview takes a few minutes to start and for CloudFront to propagate.",
			Database:     deployed.database,
			DatabaseNote: deployed.DatabaseNote,
			LogTail:      deployed.LogTail,
		})
	}, previewBuildCallWait-time.Since(started))
}

// DeployWebServicePreview is deploy_web_service_preview's plan and deploy of the
// backend preview of repository (its only web service: no root_directory), run
// synchronously. The QA stage deploys backends with it; the tool runs the same
// planWebServicePreview and runWebServicePreview, the deploy in the background.
func DeployWebServicePreview(ctx context.Context, deps DeployWebServicePreviewDeps, repository string) (WebServicePreviewDeployed, error) {
	plan, err := planWebServicePreview(deps, repository, nil)
	if err != nil {
		return WebServicePreviewDeployed{}, err
	}
	return runWebServicePreview(ctx, deps, plan)
}

// CheckWebServicePreview is deploy_web_service_preview's own decision, without
// deploying anything: nil when repository has a backend preview (one managed web
// service built from a Dockerfile in the runner's region), else the refusal.
func CheckWebServicePreview(deps DeployWebServicePreviewDeps, repository string) error {
	_, err := planWebServicePreview(deps, repository, nil)
	return err
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
	service, err := chooseWebService(settings, repoDir, rootDirectory, deps.Region, entry.BaseBranch)
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
// root directory is rootDirectory (when given) and whose region is region. Of
// several there, the one preferTaskBranchServices leaves: deploying baseBranch (the
// Task repository's base branch), in a Production environment. Every other outcome
// is the refusal the agent sees.
func chooseWebService(settings task_previews.WebServicePreviewSettingsReplyV1, repository string, rootDirectory *string, region, baseBranch string) (task_previews.WebServicePreviewSettingsV1, error) {
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
	remaining := preferTaskBranchServices(inRegion, baseBranch)
	if len(remaining) == 1 {
		return remaining[0], nil
	}
	if rootDirectory == nil && !sameWebServiceRoot(remaining) {
		names := make([]string, 0, len(remaining))
		for _, s := range remaining {
			names = append(names, s.DeploymentName)
		}
		return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("%s is deployed as several web services here (%s); pass root_directory to choose one.", repository, strings.Join(names, ", "))
	}
	described := make([]string, 0, len(remaining))
	for _, s := range remaining {
		d := fmt.Sprintf("%s in %s", s.DeploymentName, s.EnvironmentName)
		if s.Branch != "" {
			d += ", from " + s.Branch
		}
		described = append(described, d)
	}
	return task_previews.WebServicePreviewSettingsV1{}, fmt.Errorf("%s is deployed as several web services here that a preview can't choose between (%s). "+
		"A preview uses the one that deploys the Task's base branch (%s) and, among those, one in an environment classified Production.",
		repository, strings.Join(described, "; "), baseBranch)
}

// sameWebServiceRoot reports whether every service has the same root directory
// (trimRootDirectory).
func sameWebServiceRoot(services []task_previews.WebServicePreviewSettingsV1) bool {
	for _, s := range services[1:] {
		if trimRootDirectory(s.RootDirectory) != trimRootDirectory(services[0].RootDirectory) {
			return false
		}
	}
	return true
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

// runWebServicePreview deploys p from a copy of the agent's working tree (or the
// merge result, when deps.MergeResult can make it) and puts a CloudFront
// distribution in front of it on the first deploy, removing the copy in every case.
func runWebServicePreview(ctx context.Context, deps DeployWebServicePreviewDeps, p webServicePreviewPlan) (WebServicePreviewDeployed, error) {
	previewID, existing, err := ensurePreview(ctx, deps.Store, p.serviceName)
	if err != nil {
		return WebServicePreviewDeployed{}, fmt.Errorf("ensure preview record: %w", err)
	}
	existing = deps.Distributions.fill(previewID, existing)
	copyDir, err := PreviewBuildSource(ctx, DeployStaticSitePreviewDeps{WorkDirHost: deps.WorkDirHost, LogsWriter: deps.LogsWriter, MergeResult: deps.MergeResult}, p.repository)
	if copyDir != "" {
		defer os.RemoveAll(copyDir)
	}
	if err != nil {
		return WebServicePreviewDeployed{}, fmt.Errorf("copy %s for the preview build: %w", p.repository, err)
	}
	serviceDir := filepath.Join(copyDir, p.rootDirectory)
	if info, err := os.Stat(serviceDir); err != nil || !info.IsDir() {
		return WebServicePreviewDeployed{}, fmt.Errorf("%s's root directory %q isn't a directory in %s", p.service.DeploymentName, p.service.RootDirectory, p.repository)
	}
	if err := checkInside(copyDir, serviceDir); err != nil {
		return WebServicePreviewDeployed{}, fmt.Errorf("%s's root directory: %w", p.service.DeploymentName, err)
	}

	database, databaseNote, err := planPreviewDatabase(ctx, deps, p, serviceDir)
	if err != nil {
		return WebServicePreviewDeployed{}, err
	}
	settings := withWiredVariables(p.settings, p.wired, database, deps.LogsWriter)

	var tail bytes.Buffer
	logs := io.MultiWriter(deps.LogsWriter, &tail)
	io.WriteString(deps.LogsWriter, fmt.Sprintf("Backend preview: %s built by deployment.io like %s\n", p.serviceName, webServiceDeploymentName(p.service)))
	out, err := deps.Deploy(ctx, WebServicePreviewDeployInput{
		PreviewID:     previewID,
		ServiceKey:    p.serviceName,
		BuildDir:      copyDir,
		RootDirectory: p.rootDirectory,
		Service:       p.service,
		Settings:      settings,
		Existing:      existing,
		Database:      database,
		WiredNames:    wiredNames(p.wired),
		BuildLogs:     deps.LogsWriter,
		Logs:          logs,
	})
	if err != nil {
		return WebServicePreviewDeployed{}, webPreviewDeployFailure(err, database, deps.LogsWriter, tail.Bytes())
	}
	var databaseResult *previewDatabaseResult
	if database != nil {
		// The API's service deploy runs only after the preview migrate command, if set, succeeded.
		migrations := database.Migrations.summary()
		logPreviewDatabaseMigrations(deps.LogsWriter, migrations)
		databaseResult = newPreviewDatabaseResult(database.PreviewDatabasePlan, migrations)
	}
	domain, err := ensureWebPreviewDistribution(ctx, deps, previewID, existing, out.LoadBalancerDNS, logs)
	if err != nil {
		return WebServicePreviewDeployed{}, webPreviewFailureWithDetails(err, databaseResult, tail.Bytes())
	}

	previewURL := "https://" + domain
	deps.Record.AddURL(previewURL)
	return WebServicePreviewDeployed{
		URL:          previewURL,
		HealthURL:    previewURL + healthCheckPath(p.service.HealthCheckPath),
		Deployment:   webServiceDeploymentName(p.service),
		database:     databaseResult,
		DatabaseNote: databaseNote,
		LogTail:      tailString(tail.Bytes(), logTailMaxBytes),
	}, nil
}

// withWiredVariables is settings with wired's variables replacing the preview
// configuration's of the same name, so the task definition and the build args read
// them. The preview database's variables still win over both (the deployer puts the
// database's values in the environment and, for a wired name, the build args too);
// a wired one the database sets is logged by name.
func withWiredVariables(settings task_previews.WebServicePreviewSettingsReplyV1, wired map[string]string, database *PreviewDatabaseDeploy, logs io.Writer) task_previews.WebServicePreviewSettingsReplyV1 {
	if len(wired) == 0 {
		return settings
	}
	databaseSets := map[string]bool{}
	if database != nil {
		for _, name := range database.Variables {
			databaseSets[name] = true
		}
	}
	variables := make(map[string]string, len(settings.Variables)+len(wired))
	for k, v := range settings.Variables {
		variables[k] = v
	}
	for _, k := range sortedKeys(wired) {
		if databaseSets[k] {
			io.WriteString(logs, fmt.Sprintf("Backend preview: the preview database's %s replaces the value QA set for it\n", k))
		}
		variables[k] = wired[k]
	}
	settings.Variables = variables
	return settings
}

// wiredNames is wired's names, sorted; nil when there are none.
func wiredNames(wired map[string]string) []string {
	if len(wired) == 0 {
		return nil
	}
	return sortedKeys(wired)
}

// webPreviewFailureDetails is what a failed deploy's error carries after its
// message when the preview has its own database: the database, and the log tail
// (the preview migrate command's output when it failed). Never a password or a URL.
type webPreviewFailureDetails struct {
	Database *previewDatabaseResult `json:"database"`
	LogTail  string                 `json:"log_tail,omitempty"`
}

// webPreviewDeployFailure is a failed deploy's error, with its details when the
// preview has a database (a preview without one fails as it always did).
func webPreviewDeployFailure(err error, database *PreviewDatabaseDeploy, stepLog io.Writer, tail []byte) error {
	if database == nil {
		return err
	}
	migrations := database.Migrations.failedSummary()
	logPreviewDatabaseMigrations(stepLog, migrations)
	return webPreviewFailureWithDetails(err, newPreviewDatabaseResult(database.PreviewDatabasePlan, migrations), tail)
}

func webPreviewFailureWithDetails(err error, database *previewDatabaseResult, tail []byte) error {
	if database == nil {
		return err
	}
	encoded, marshalErr := json.Marshal(webPreviewFailureDetails{Database: database, LogTail: tailString(tail, logTailMaxBytes)})
	if marshalErr != nil {
		return err
	}
	return fmt.Errorf("%w\n%s", err, encoded)
}

// planPreviewDatabase decides the preview's own database (resolvePreviewDatabase,
// detecting over the service directory of the build copy) and find-or-creates its
// preview record. It writes what it decided to the Step log. No database: nil and
// the note saying why.
func planPreviewDatabase(ctx context.Context, deps DeployWebServicePreviewDeps, p webServicePreviewPlan, serviceDir string) (*PreviewDatabaseDeploy, string, error) {
	plan, note := resolvePreviewDatabase(p.service, p.settings.Variables, detectPreviewDatabaseIn(os.DirFS(serviceDir), p.service.VariableNames))
	if plan != nil && deps.DatabaseStore == nil {
		plan, note = nil, "deployment.io can't run preview databases here"
	}
	if plan == nil {
		logPreviewDatabaseNote(deps.LogsWriter, note)
		return nil, note, nil
	}
	serviceKey := previewDatabaseServiceKey(p.serviceName)
	previewID, _, err := ensurePreview(ctx, deps.DatabaseStore, serviceKey)
	if err != nil {
		return nil, "", fmt.Errorf("ensure preview database record: %w", err)
	}
	logPreviewDatabasePlan(deps.LogsWriter, *plan)
	command, migrations := previewDatabaseMigrations(p.service.PreviewMigrateCommand)
	return &PreviewDatabaseDeploy{
		PreviewID:           previewID,
		ServiceKey:          serviceKey,
		PreviewDatabasePlan: *plan,
		MigrateCommand:      command,
		Migrations:          migrations,
	}, "", nil
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
	deps.Distributions.remember(previewID, saved)
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
