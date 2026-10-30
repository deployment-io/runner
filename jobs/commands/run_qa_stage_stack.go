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
	Error               string   `json:"error,omitempty"`
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
	// origin is a site's reserved address, https://<distribution domain>.
	origin         string
	url, healthURL string
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
}

// deployStackAndSpawnQA is a stack-path QA round once its directory is ready: it
// deploys every preview, adds QA_PREVIEWS to env and spawns the QA run with the
// verify tools alone.
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
	previews, err := s.deployStack(label, previewDeps)
	if err != nil {
		return agentResult{}, err
	}
	spec := s.qaSpawnSpec(imageRef, qaDir, withQAPreviews(env, previews), previewDeps)
	spec.verifyToolsOnly = true
	return s.spawnQARound(label, spec)
}

// deployStack deploys the round's previews and records how they connect
// (roundWiring). It returns QA_PREVIEWS' entries, or an error when the user
// stopped the Step or no preview deployed.
func (s *qaStage) deployStack(label string, previewDeps *agenttools.DeployStaticSitePreviewDeps) ([]qaPreview, error) {
	ctx, cancel := s.stopContext()
	defer cancel()
	r := &qaStackRound{stage: s, label: label, ops: s.qaStackOps(), ctx: ctx, siteDeps: *previewDeps, webDeps: *s.roundWebPreviewDeps(previewDeps)}
	r.plan()
	reply, lookupFailed := s.stackWiringReply(r.ops)
	r.pairs = pairStack(r.sites, r.backends, reply.Links, lookupFailed)
	if err := r.deploy(); err != nil {
		return nil, err
	}
	s.roundWiring = r.wiringOutputs()
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
			// are "backend failed".
			p.planErr = d.previewError()
			p.err = p.planErr
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
}

// deploy reserves the linked sites' addresses, then deploys the backends, then
// the sites. Only a user stop is an error.
func (r *qaStackRound) deploy() error {
	if err := r.reserveSites(); err != nil {
		return err
	}
	if err := r.deployBackends(); err != nil {
		return err
	}
	return r.deploySites()
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
		d, err := r.ops.deployBackend(r.ctx, r.webDeps, backend.backend, origins)
		if r.stage.stopped() {
			return types.ErrJobStoppedByUser
		}
		if err != nil {
			r.fail(backend, "backend", err)
			for _, p := range r.pairs {
				if p.backend == backend && p.linked() && (p.reason == "" || p.reason == qaWiringSingleOrigin) {
					p.reason = qaWiringBackendFailed
				}
			}
			continue
		}
		backend.url, backend.healthURL = d.URL, d.HealthURL
		r.webDeps.Record.AddURL(d.URL)
		io.WriteString(r.stage.logsWriter, fmt.Sprintf("%s: the backend preview of %s is deployed at %s (health check %s)\n", r.label, backend.repository, d.URL, d.HealthURL))
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
	for i, e := range s.ctx.Entries {
		if fmt.Sprintf("%d-%s", i, e.Name) == dir {
			return e.Name
		}
	}
	return dir
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
				entry.URL, entry.HealthURL = backend.url, backend.healthURL
			}
			out = append(out, entry)
		}
	}
	return out
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
