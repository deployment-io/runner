package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/agenttools"
	runnerclient "github.com/deployment-io/deployment-runner/client"
)

// THE STACK PATH (agentbox minPreviewsQAAgentboxVersion and later).
//
// Before each QA round the runner deploys every preview of the change's stack
// itself, from the round's merge result, in this order:
//
//  1. plan every site and backend preview, as the tools do;
//  2. reserve the distribution of each site linked to a backend of the run, so
//     its origin is known before anything is built;
//  3. deploy each backend, its allowed-origin variables set to the origins of
//     the sites linked to it;
//  4. build and deploy each site, its API-address variables pointing at the
//     backend previews it is connected to;
//
// and then spawns the QA run with QA_PREVIEWS and only the verify tools.

// qaStackOps are the stack path's lookups and deploys.
type qaStackOps struct {
	planSite      func(deps agenttools.DeployStaticSitePreviewDeps, repository string) (agenttools.StaticSitePreviewPlan, error)
	reserveSite   func(ctx context.Context, deps agenttools.DeployStaticSitePreviewDeps, plan agenttools.StaticSitePreviewPlan) (string, error)
	deploySite    func(ctx context.Context, deps agenttools.DeployStaticSitePreviewDeps, plan agenttools.StaticSitePreviewPlan, variables map[string]string) (string, error)
	planBackend   func(deps agenttools.DeployWebServicePreviewDeps, repository string) (agenttools.WebServicePreviewPlan, error)
	deployBackend func(ctx context.Context, deps agenttools.DeployWebServicePreviewDeps, plan agenttools.WebServicePreviewPlan, variables map[string]string) (agenttools.WebServicePreviewDeployed, error)
	wiring        func(repositories []task_previews.StackRepositoryV1) (task_previews.StackWiringReplyV1, error)
	// probePages finds out whether a deployed backend preview serves pages.
	probePages func(ctx context.Context, url, healthURL string) agenttools.PreviewPagesProbe
}

// qaStackOps is the stage's stack operations: the test seam's, or the tools'
// own plans and deploys and deployment-server's StackWiringV1.
func (s *qaStage) qaStackOps() qaStackOps {
	if s.stackOps != nil {
		return *s.stackOps
	}
	orgID, taskID := s.ctx.OrganizationID, s.ctx.TaskID
	return qaStackOps{
		planSite:      agenttools.PlanStaticSitePreview,
		reserveSite:   agenttools.ReserveStaticSitePreview,
		deploySite:    agenttools.DeployStaticSitePreviewWith,
		planBackend:   agenttools.PlanWebServicePreview,
		deployBackend: agenttools.DeployWebServicePreviewWith,
		wiring: func(repositories []task_previews.StackRepositoryV1) (task_previews.StackWiringReplyV1, error) {
			return runnerclient.Get().StackWiring(orgID, taskID, repositories, agenttools.PreviewBuildSettingsTimeout)
		},
		probePages: agenttools.ProbeServesPages,
	}
}

// Values of qaPreview.Kind.
const (
	qaPreviewSite    = "site"
	qaPreviewBackend = "backend"
)

// qaPreview is one QA_PREVIEWS entry: a preview the runner deployed (its URL,
// and a backend's health URL) or why it failed. A site with a URL lists every
// backend entry of the run in exactly one of its two lists.
type qaPreview struct {
	Repository          string   `json:"repository"`
	Kind                string   `json:"kind"`
	URL                 string   `json:"url,omitempty"`
	HealthURL           string   `json:"health_url,omitempty"`
	ConnectedBackends   []string `json:"connected_backends,omitempty"`
	UnconnectedBackends []string `json:"unconnected_backends,omitempty"`
	// ServesPages is true on a deployed backend whose probe found that it
	// serves pages (qaStackPreview.servesPages); never on a site or a failed
	// entry.
	ServesPages bool `json:"serves_pages,omitempty"`
	// Roles are a backend's roles QA may sign in as (qaStackPreview.roles),
	// absent when it has none. Never an email or a password.
	Roles []qaPreviewRole `json:"roles,omitempty"`
	// BrowserRoles are a site's roles QA may sign in as in the browser: those
	// of each connected backend that offers it (qaStackPreview.browserRoles),
	// absent when there are none — or, on a backend that offers them on its own
	// URL (qaStackRound.ownBrowserSignIn), its own. Never an email or a password.
	BrowserRoles []qaPreviewBrowserRole `json:"browser_roles,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// qaPreviewBrowserRole is a browser role of a QA_PREVIEWS entry: a role of the
// backend entry whose repository is Backend.
type qaPreviewBrowserRole struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Backend     string `json:"backend"`
}

// qaPreviewRole is a role of a backend's QA_PREVIEWS entry.
type qaPreviewRole struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// qaStackPreview is one preview of a stack-path round: its plan, then what its
// deploy did.
type qaStackPreview struct {
	repository string
	// deploymentName is the deployment the preview is built like; "" for a
	// detected site and a preview that couldn't be planned.
	deploymentName string
	site           agenttools.StaticSitePreviewPlan
	backend        agenttools.WebServicePreviewPlan
	// planErr is why it couldn't be planned; err why it failed (planErr too).
	planErr string
	err     string
	// leftOut is a backend the gate left out (droppedBackends): planErr says
	// why, and its pairs are "backend left out", not a failed deploy.
	leftOut bool
	// origin is a site's reserved address, https://<distribution domain>.
	origin         string
	url, healthURL string
	// roles, signIn and users are a deployed backend's roles, sign-in request
	// and its roles' test users (in the roles' order) when QA may sign in as
	// them: its seed command ran and exited 0, it has a role and a sign-in
	// request (signInRoles).
	roles  []task_previews.PreviewRoleV1
	signIn *task_previews.PreviewSignInV1
	users  []agenttools.PreviewTestUser
	// browserRoles, browserSignInPath and browserUsers are, kept apart from
	// the API sign-in's, a deployed backend's roles, browser sign-in page and
	// its roles' test users when QA may sign in as them in the browser on a
	// site connected to it, or on its own URL (browserSignInRoles).
	browserRoles      []task_previews.PreviewRoleV1
	browserSignInPath string
	browserUsers      []agenttools.PreviewTestUser
	// probe is a deployed backend's pages probe, running from its deploy
	// until waitProbes; servesPages is what it found.
	probe       chan agenttools.PreviewPagesProbe
	servesPages bool
}

// qaStackRound is one stack-path round's deploys.
type qaStackRound struct {
	stage    *qaStage
	label    string
	ops      qaStackOps
	ctx      context.Context
	siteDeps agenttools.DeployStaticSitePreviewDeps
	webDeps  agenttools.DeployWebServicePreviewDeps
	sites    []*qaStackPreview
	backends []*qaStackPreview
	pairs    []*qaStackPair
	// readsBackendBrowserRoles is true when the round's agentbox reads a
	// backend entry's browser_roles (minOwnBrowserSignInAgentboxVersion).
	readsBackendBrowserRoles bool
	// backendDetails is what the failing backend deploy did, for fail's
	// progress event; nil outside that deploy.
	backendDetails *previewDeployDetails
}

// deployStackAndSpawnQA is a stack-path QA round once its directory is ready: it
// deploys every preview, adds QA_PREVIEWS to env and spawns the QA run with the
// verify tools alone — and, when a backend preview deployed, verify_api_request
// with QA_API_REQUEST_TOOL=1, and beside it, when a backend has roles,
// verify_api_sign_in with QA_API_SIGN_IN_TOOL=1. When a site preview can be
// checked signed in (or a backend preview that serves pages, with an agentbox
// that reads it), verify_preview_rendered takes as.
func (s *qaStage) deployStackAndSpawnQA(round int, imageRef, qaDir string, env []string) (agentResult, error) {
	previewDeps := buildStaticSitePreviewDeps(s.ctx, s.parameters, s.workDirHost, s.logsWriter)
	mergeResults := s.newQAMergeResults()
	defer func() {
		mergeResults.remove()
		s.roundMergeResults = mergeResults.results()
	}()
	previewDeps.MergeResult = mergeResults
	if s.staticDistributions == nil {
		s.staticDistributions = agenttools.NewWebPreviewDistributions()
	}
	previewDeps.Distributions = s.staticDistributions
	label := fmt.Sprintf("QA round %d", round)
	previews, err := s.deployStack(label, imageVersionAtLeast(imageRef, minOwnBrowserSignInAgentboxVersion), previewDeps)
	if err != nil {
		return agentResult{}, err
	}
	spec := s.qaSpawnSpec(imageRef, qaDir, withQAPreviews(env, previews), previewDeps)
	spec.verifyToolsOnly = true
	if hasDeployedBackend(previews) {
		spec.apiRequestTool = true
		spec.env = append(spec.env, "QA_API_REQUEST_TOOL=1")
		spec.apiPasswords = s.testUsers.Passwords()
		if len(s.roundSignIns) > 0 {
			spec.apiSignIns = s.roundSignIns
			spec.apiSignInTool = true
			spec.env = append(spec.env, "QA_API_SIGN_IN_TOOL=1")
		}
	}
	spec.browserSignIns = s.roundBrowserSignIns
	return s.spawnQARound(label, spec)
}

// hasDeployedBackend reports whether previews has a backend preview with a URL.
func hasDeployedBackend(previews []qaPreview) bool {
	for _, p := range previews {
		if p.Kind == qaPreviewBackend && p.URL != "" {
			return true
		}
	}
	return false
}

// deployStack deploys the round's previews and records how they connect
// (roundWiring). readsBackendBrowserRoles is true when the round's agentbox
// reads a backend entry's browser_roles. It returns QA_PREVIEWS' entries, or an error
// when the user stopped the Step or no preview deployed.
func (s *qaStage) deployStack(label string, readsBackendBrowserRoles bool, previewDeps *agenttools.DeployStaticSitePreviewDeps) ([]qaPreview, error) {
	ctx, cancel := s.stopContext()
	defer cancel()
	r := &qaStackRound{stage: s, label: label, ops: s.qaStackOps(), ctx: ctx, siteDeps: *previewDeps, webDeps: *s.roundWebPreviewDeps(previewDeps), readsBackendBrowserRoles: readsBackendBrowserRoles}
	r.plan()
	reply, lookupFailed := s.stackWiringReply(r.ops)
	r.pairs = pairStack(r.sites, r.backends, reply.Links, lookupFailed)
	if err := r.deploy(); err != nil {
		return nil, err
	}
	s.roundWiring = r.wiringOutputs()
	s.roundSignIns = r.signIns()
	s.roundBrowserSignIns = r.browserSignIns()
	for _, w := range s.roundWiring {
		io.WriteString(s.logsWriter, fmt.Sprintf("%s: %s\n", label, qaWiringLine(w, s.roundWiring)))
	}
	previews := r.previews()
	for _, p := range previews {
		if p.URL != "" {
			return previews, nil
		}
	}
	return previews, fmt.Errorf("no preview deployed: %s", previews[0].Error)
}

// stackWiringReply is how the stack's previews connect, looked up once per
// stage when it has a site and a backend; true when the lookup failed.
func (s *qaStage) stackWiringReply(ops qaStackOps) (task_previews.StackWiringReplyV1, bool) {
	if len(s.repositories) == 0 || len(s.stackBackends()) == 0 {
		return task_previews.StackWiringReplyV1{}, false
	}
	if !s.wiringLooked {
		s.wiringLooked = true
		repositories := make([]task_previews.StackRepositoryV1, 0, len(s.ctx.Entries))
		for i, e := range s.ctx.Entries {
			repositories = append(repositories, task_previews.StackRepositoryV1{Repository: fmt.Sprintf("%d-%s", i, e.Name), CloneURL: e.CloneURL})
		}
		reply, err := ops.wiring(repositories)
		if err != nil {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: couldn't look up how the previews connect: %s\n", err))
			s.wiringFailed = true
		} else {
			s.wiring = reply
		}
	}
	return s.wiring, s.wiringFailed
}

// plan plans every site and backend preview of the round.
func (r *qaStackRound) plan() {
	for _, repository := range r.stage.repositories {
		p := &qaStackPreview{repository: repository}
		plan, err := r.ops.planSite(r.siteDeps, repository)
		if err != nil {
			r.fail(p, "site", err)
			p.planErr = p.err
		} else {
			p.site, p.deploymentName = plan, plan.DeploymentName
		}
		r.sites = append(r.sites, p)
	}
	dropped := map[string]droppedBackend{}
	for _, d := range r.stage.droppedBackends {
		dropped[d.backendDir] = d
	}
	for _, repository := range r.stage.stackBackends() {
		p := &qaStackPreview{repository: repository}
		if d, ok := dropped[repository]; ok {
			// Left out by the gate: nothing is deployed for it, and its pairs
			// are "backend left out".
			p.planErr = d.previewError()
			p.err = p.planErr
			p.leftOut = true
			r.backends = append(r.backends, p)
			continue
		}
		plan, err := r.ops.planBackend(r.webDeps, repository)
		if err != nil {
			r.fail(p, "backend", err)
			p.planErr = p.err
		} else {
			p.backend, p.deploymentName = plan, plan.DeploymentName
		}
		r.backends = append(r.backends, p)
	}
}

// stackBackends is every backend of a stack-path round in work-dir order: the
// ones it deploys and the ones the gate left out (droppedBackends).
func (s *qaStage) stackBackends() []string {
	if len(s.droppedBackends) == 0 {
		return s.backends
	}
	all := append([]string{}, s.backends...)
	for _, d := range s.droppedBackends {
		all = append(all, d.backendDir)
	}
	return qaStackRepositories(nil, all)
}

// fail records why p failed (its first line, capped, as QA_PREVIEWS tells it)
// and logs it whole.
func (r *qaStackRound) fail(p *qaStackPreview, kind string, err error) {
	what := "the preview of " + p.repository
	if kind == "backend" {
		what = "the backend preview of " + p.repository
	}
	io.WriteString(r.stage.logsWriter, fmt.Sprintf("%s: %s failed to deploy: %s\n", r.label, what, err))
	p.err = qaPreviewError(err)
	r.stage.emitPreviewDeploy(previewDeployResult{repo: r.stage.entryName(p.repository), kind: kind, err: err, details: r.backendDetails})
}

// deploy reserves the linked sites' addresses, then deploys the backends, then
// the sites, and then waits for the backends' pages probes. Only a user stop is
// an error.
func (r *qaStackRound) deploy() error {
	if err := r.reserveSites(); err != nil {
		return err
	}
	if err := r.deployBackends(); err != nil {
		return err
	}
	if err := r.deploySites(); err != nil {
		return err
	}
	return r.waitProbes()
}

// reserveSites reserves the distribution of every site linked to a backend of
// the run, so its origin is known before the backend deploys.
func (r *qaStackRound) reserveSites() error {
	for _, site := range r.sites {
		if site.err != "" || !r.linkedToABackend(site) {
			continue
		}
		origin, err := r.ops.reserveSite(r.ctx, r.siteDeps, site.site)
		if r.stage.stopped() {
			return types.ErrJobStoppedByUser
		}
		if err != nil {
			r.fail(site, "site", err)
			markSiteFailed(r.pairs, site)
			continue
		}
		site.origin = origin
		r.siteDeps.Record.AddURL(origin)
		io.WriteString(r.stage.logsWriter, fmt.Sprintf("%s: the preview of %s will be at %s\n", r.label, site.repository, origin))
	}
	return nil
}

func (r *qaStackRound) linkedToABackend(site *qaStackPreview) bool {
	for _, p := range r.pairs {
		if p.site == site && p.linked() {
			return true
		}
	}
	return false
}

// deployBackends deploys each backend in work-dir order with its allowed-origin
// variables set to the origins of the sites linked to it.
func (r *qaStackRound) deployBackends() error {
	for _, backend := range r.backends {
		if backend.err != "" {
			continue
		}
		origins := stackOrigins(r.pairs, backend)
		line := fmt.Sprintf("%s: deploying the backend preview of %s", r.label, backend.repository)
		if len(origins) > 0 {
			line += fmt.Sprintf(" (allowing its site previews' origins in %s)", strings.Join(sortedKeys(origins), ", "))
		}
		io.WriteString(r.stage.logsWriter, line+"\n")
		webDeps, facts := recordPreviewDeploy(r.webDeps)
		d, err := r.ops.deployBackend(r.ctx, webDeps, backend.backend, origins)
		if r.stage.stopped() {
			return types.ErrJobStoppedByUser
		}
		if err != nil {
			r.backendDetails = facts.result(d)
			r.fail(backend, "backend", err)
			r.backendDetails = nil
			for _, p := range r.pairs {
				if p.backend == backend && p.linked() && (p.reason == "" || p.reason == qaWiringSingleOrigin) {
					p.reason = qaWiringBackendFailed
				}
			}
			continue
		}
		backend.url, backend.healthURL = d.URL, d.HealthURL
		r.signInRoles(backend, d)
		r.browserSignInRoles(backend, d)
		r.webDeps.Record.AddURL(d.URL)
		io.WriteString(r.stage.logsWriter, fmt.Sprintf("%s: the backend preview of %s is deployed at %s (health check %s)\n", r.label, backend.repository, d.URL, d.HealthURL))
		r.stage.emitPreviewDeploy(previewDeployResult{repo: r.stage.entryName(backend.repository), kind: qaPreviewBackend, url: d.URL, details: facts.result(d)})
		r.startProbe(backend)
	}
	return nil
}

// startProbe starts finding out, in the background while the round deploys
// on, whether the deployed backend serves pages.
func (r *qaStackRound) startProbe(backend *qaStackPreview) {
	backend.probe = make(chan agenttools.PreviewPagesProbe, 1)
	go func(ctx context.Context, url, healthURL string, out chan<- agenttools.PreviewPagesProbe) {
		out <- r.ops.probePages(ctx, url, healthURL)
	}(r.ctx, backend.url, backend.healthURL, backend.probe)
}

// waitProbes waits for each backend's pages probe, in work-dir order, and logs
// what it found. Only a user stop is an error.
func (r *qaStackRound) waitProbes() error {
	for _, backend := range r.backends {
		if backend.probe == nil {
			continue
		}
		var probe agenttools.PreviewPagesProbe
		select {
		case probe = <-backend.probe:
		case <-r.ctx.Done():
		}
		if r.stage.stopped() {
			return types.ErrJobStoppedByUser
		}
		backend.servesPages = probe.ServesPages
		what := "is an API"
		if probe.ServesPages {
			what = "serves pages"
		}
		io.WriteString(r.stage.logsWriter, fmt.Sprintf("%s: the backend preview of %s %s (%s)\n", r.label, backend.repository, what, probe.Reason))
	}
	return nil
}

// deploySites builds and deploys each site in work-dir order, waiting for each,
// its API-address variables pointing at the backends it is connected to.
func (r *qaStackRound) deploySites() error {
	for _, site := range r.sites {
		if site.err != "" {
			continue
		}
		addresses := stackAPIAddresses(r.pairs, site)
		line := fmt.Sprintf("%s: building the preview of %s", r.label, site.repository)
		if len(addresses) > 0 {
			line += fmt.Sprintf(" (pointing %s at its backend previews)", strings.Join(sortedKeys(addresses), ", "))
		}
		io.WriteString(r.stage.logsWriter, line+"\n")
		url, err := r.ops.deploySite(r.ctx, r.siteDeps, site.site, addresses)
		if r.stage.stopped() {
			return types.ErrJobStoppedByUser
		}
		if err != nil {
			r.fail(site, "site", err)
			markSiteFailed(r.pairs, site)
			continue
		}
		site.url = url
		r.siteDeps.Record.AddURL(url)
		io.WriteString(r.stage.logsWriter, fmt.Sprintf("%s: the preview of %s is deployed at %s\n", r.label, site.repository, url))
		r.stage.emitPreviewDeploy(previewDeployResult{repo: r.stage.entryName(site.repository), kind: qaPreviewSite, url: url})
	}
	return nil
}

// wiringOutputs is every pair as the round records it.
func (r *qaStackRound) wiringOutputs() []qaWiringOutput {
	if len(r.pairs) == 0 {
		return nil
	}
	out := make([]qaWiringOutput, 0, len(r.pairs))
	for _, p := range r.pairs {
		out = append(out, p.wiringOutput(r.stage.entryName))
	}
	return out
}

// entryName is the Task's repository entry name of the work-dir name dir
// ("<idx>-<name>"), as qaMergeResultOutput.Repository names it; dir itself when
// no entry has it.
func (s *qaStage) entryName(dir string) string {
	if name, ok := s.taskEntryName(dir); ok {
		return name
	}
	return dir
}

// taskEntryName is the full name of the Task repository checked out at dir
// ("<idx>-<name>"); ok is false for a directory that is none of them.
func (s *qaStage) taskEntryName(dir string) (string, bool) {
	for i, e := range s.ctx.Entries {
		if fmt.Sprintf("%d-%s", i, e.Name) == dir {
			return e.Name, true
		}
	}
	return "", false
}

// previews is QA_PREVIEWS: an entry per preview in work-dir order, a
// repository's site before its backend.
func (r *qaStackRound) previews() []qaPreview {
	sites, backends := map[string]*qaStackPreview{}, map[string]*qaStackPreview{}
	for _, p := range r.sites {
		sites[p.repository] = p
	}
	for _, p := range r.backends {
		backends[p.repository] = p
	}
	var out []qaPreview
	for _, repository := range qaStackRepositories(r.stage.repositories, r.stage.stackBackends()) {
		if site := sites[repository]; site != nil {
			out = append(out, r.sitePreview(site))
		}
		if backend := backends[repository]; backend != nil {
			entry := qaPreview{Repository: repository, Kind: qaPreviewBackend, Error: backend.err}
			if backend.err == "" {
				entry.URL, entry.HealthURL, entry.ServesPages = backend.url, backend.healthURL, backend.servesPages
				for _, role := range backend.roles {
					entry.Roles = append(entry.Roles, qaPreviewRole{Name: role.Name, Description: role.Description})
				}
				if r.ownBrowserSignIn(backend) {
					for _, role := range backend.browserRoles {
						entry.BrowserRoles = append(entry.BrowserRoles, qaPreviewBrowserRole{Name: role.Name, Description: role.Description, Backend: backend.repository})
					}
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// signInRoles records on backend the roles QA may sign in as after its deploy
// d: all of them when its seed command ran and exited 0, it has a role and a
// sign-in request, and the round's holder has each role's test user; else none.
func (r *qaStackRound) signInRoles(backend *qaStackPreview, d agenttools.WebServicePreviewDeployed) {
	if d.SignIn == nil {
		return
	}
	if users, ok := r.seededUsers(d); ok {
		backend.roles, backend.signIn, backend.users = d.Roles, d.SignIn, users
	}
}

// browserSignInRoles records on backend the roles QA may sign in as in the
// browser after its deploy d: all of them when its seed command ran and exited
// 0, it has a role and a browser sign-in page, and the round's holder has each
// role's test user; else none. It needs no sign-in request.
func (r *qaStackRound) browserSignInRoles(backend *qaStackPreview, d agenttools.WebServicePreviewDeployed) {
	path := strings.TrimSpace(d.BrowserSignInPath)
	if path == "" {
		return
	}
	if users, ok := r.seededUsers(d); ok {
		backend.browserRoles, backend.browserSignInPath, backend.browserUsers = d.Roles, path, users
	}
}

// seededUsers is the test user of each of d's roles, in their order, when its
// seed command ran and exited 0, it has a role and the round's holder has each
// role's test user.
func (r *qaStackRound) seededUsers(d agenttools.WebServicePreviewDeployed) ([]agenttools.PreviewTestUser, bool) {
	if d.Seed != agenttools.PreviewSeedSucceeded || len(d.Roles) == 0 || r.webDeps.TestUsers == nil {
		return nil, false
	}
	byRole := map[string]agenttools.PreviewTestUser{}
	for _, user := range r.webDeps.TestUsers.Users(d.PreviewID) {
		byRole[user.Role] = user
	}
	users := make([]agenttools.PreviewTestUser, 0, len(d.Roles))
	for _, role := range d.Roles {
		user, ok := byRole[role.Name]
		if !ok {
			return nil, false
		}
		users = append(users, user)
	}
	return users, true
}

// signIns are the round's backends QA may sign in at.
func (r *qaStackRound) signIns() []agenttools.APISignInPreview {
	var out []agenttools.APISignInPreview
	for _, backend := range r.backends {
		if backend.err == "" && len(backend.roles) > 0 {
			out = append(out, agenttools.APISignInPreview{URL: backend.url, SignIn: *backend.signIn, Users: backend.users})
		}
	}
	return out
}

// browserSignIns are the round's previews QA may check signed in: each
// deployed site with a connected backend that offers browser sign-in, and
// those backends in connected_backends order; then each backend that offers it
// on its own URL, with itself as its only backend.
func (r *qaStackRound) browserSignIns() []agenttools.BrowserSignInSite {
	var out []agenttools.BrowserSignInSite
	for _, site := range r.sites {
		var backends []agenttools.BrowserSignInBackend
		for _, backend := range r.browserBackends(site) {
			backends = append(backends, browserSignInBackend(backend))
		}
		if len(backends) > 0 {
			out = append(out, agenttools.BrowserSignInSite{URL: site.url, Backends: backends})
		}
	}
	for _, backend := range r.backends {
		if r.ownBrowserSignIn(backend) {
			out = append(out, agenttools.BrowserSignInSite{URL: backend.url, Backends: []agenttools.BrowserSignInBackend{browserSignInBackend(backend)}})
		}
	}
	return out
}

func browserSignInBackend(backend *qaStackPreview) agenttools.BrowserSignInBackend {
	return agenttools.BrowserSignInBackend{Repository: backend.repository, SignInPath: backend.browserSignInPath, Users: backend.browserUsers}
}

// browserBackends are the backends that offer site browser sign-in, in its
// pairs' order: none for a site that failed; else each deployed one it is
// connected to with roles QA may sign in as in the browser, but for one that
// offers them on its own URL.
func (r *qaStackRound) browserBackends(site *qaStackPreview) []*qaStackPreview {
	if site.err != "" {
		return nil
	}
	var out []*qaStackPreview
	for _, p := range r.pairs {
		if p.site == site && p.connected() && p.backend.err == "" && len(p.backend.browserRoles) > 0 && !r.ownBrowserSignIn(p.backend) {
			out = append(out, p.backend)
		}
	}
	return out
}

// ownBrowserSignIn reports whether backend offers its browser sign-in roles on
// its own URL instead of on its connected sites: it deployed, serves pages and
// has roles QA may sign in as in the browser, and the round's agentbox reads a
// backend entry's browser_roles.
func (r *qaStackRound) ownBrowserSignIn(backend *qaStackPreview) bool {
	return r.readsBackendBrowserRoles && backend.err == "" && backend.servesPages && len(backend.browserRoles) > 0
}

// sitePreview is site's QA_PREVIEWS entry: with a URL, every backend of the
// run is in connected_backends or unconnected_backends.
func (r *qaStackRound) sitePreview(site *qaStackPreview) qaPreview {
	entry := qaPreview{Repository: site.repository, Kind: qaPreviewSite}
	if site.err != "" {
		entry.Error = site.err
		return entry
	}
	entry.URL = site.url
	for _, p := range r.pairs {
		if p.site != site {
			continue
		}
		if p.connected() {
			entry.ConnectedBackends = append(entry.ConnectedBackends, p.backend.repository)
		} else {
			entry.UnconnectedBackends = append(entry.UnconnectedBackends, p.backend.repository)
		}
	}
	for _, backend := range r.browserBackends(site) {
		for _, role := range backend.browserRoles {
			entry.BrowserRoles = append(entry.BrowserRoles, qaPreviewBrowserRole{Name: role.Name, Description: role.Description, Backend: backend.repository})
		}
	}
	return entry
}

// withQAPreviews adds QA_PREVIEWS — the round's previews as a JSON array — to a
// QA run's environment.
func withQAPreviews(env []string, previews []qaPreview) []string {
	encoded, err := json.Marshal(previews)
	if err != nil {
		return env
	}
	return append(env, "QA_PREVIEWS="+string(encoded))
}
