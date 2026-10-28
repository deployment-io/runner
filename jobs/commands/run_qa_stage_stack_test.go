package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
)

const stackQAImage = "deploymenthq/agentbox:1.9.37"

// secretPath is a production URL path the wiring reply carries: it must never
// reach the Job output, the pull request or the Step log.
const secretPath = "/zz-secret-route"

// fakeStackOps fakes the stack path's plans, deploys and StackWiringV1. A site's
// preview is at https://s-<repo>.cloudfront.net, a backend's at
// https://b-<repo>.cloudfront.net ("/" in <repo> made "-").
type fakeStackOps struct {
	mu sync.Mutex
	// sites and backends are the deployment each repository's preview is built
	// like ("" for a detected site).
	sites, backends map[string]string
	planErr         map[string]error // "site:<repo>" / "backend:<repo>"
	deployErr       map[string]error
	reserveErr      map[string]error // by repository
	links           []task_previews.StackLinkV1
	wiringErr       error

	events      []string
	wiringCalls int
	wiringRepos []task_previews.StackRepositoryV1
	siteVars    map[string]map[string]string
	backendVars map[string]map[string]string
	siteDeps    []agenttools.DeployStaticSitePreviewDeps
	webDeps     []agenttools.DeployWebServicePreviewDeps
}

func stackHost(prefix, repo string) string {
	return "https://" + prefix + "-" + strings.ReplaceAll(repo, "/", "-") + ".cloudfront.net"
}

func (f *fakeStackOps) event(e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeStackOps) ops() *qaStackOps {
	f.siteVars, f.backendVars = map[string]map[string]string{}, map[string]map[string]string{}
	return &qaStackOps{
		planSite: func(_ agenttools.DeployStaticSitePreviewDeps, repo string) (agenttools.StaticSitePreviewPlan, error) {
			f.event("plan site " + repo)
			if err := f.planErr["site:"+repo]; err != nil {
				return agenttools.StaticSitePreviewPlan{}, err
			}
			return agenttools.StaticSitePreviewPlan{Repository: repo, DeploymentName: f.sites[repo]}, nil
		},
		reserveSite: func(_ context.Context, deps agenttools.DeployStaticSitePreviewDeps, p agenttools.StaticSitePreviewPlan) (string, error) {
			f.event("reserve " + p.Repository)
			f.siteDeps = append(f.siteDeps, deps)
			if err := f.reserveErr[p.Repository]; err != nil {
				return "", err
			}
			return stackHost("s", p.Repository), nil
		},
		deploySite: func(_ context.Context, deps agenttools.DeployStaticSitePreviewDeps, p agenttools.StaticSitePreviewPlan, vars map[string]string) (string, error) {
			f.event("deploy site " + p.Repository)
			f.siteDeps = append(f.siteDeps, deps)
			f.siteVars[p.Repository] = vars
			if err := f.deployErr["site:"+p.Repository]; err != nil {
				return "", err
			}
			return stackHost("s", p.Repository), nil
		},
		planBackend: func(_ agenttools.DeployWebServicePreviewDeps, repo string) (agenttools.WebServicePreviewPlan, error) {
			f.event("plan backend " + repo)
			if err := f.planErr["backend:"+repo]; err != nil {
				return agenttools.WebServicePreviewPlan{}, err
			}
			return agenttools.WebServicePreviewPlan{Repository: repo, DeploymentName: f.backends[repo]}, nil
		},
		deployBackend: func(_ context.Context, deps agenttools.DeployWebServicePreviewDeps, p agenttools.WebServicePreviewPlan, vars map[string]string) (agenttools.WebServicePreviewDeployed, error) {
			f.event("deploy backend " + p.Repository)
			f.webDeps = append(f.webDeps, deps)
			f.backendVars[p.Repository] = vars
			if err := f.deployErr["backend:"+p.Repository]; err != nil {
				return agenttools.WebServicePreviewDeployed{}, err
			}
			url := stackHost("b", p.Repository)
			return agenttools.WebServicePreviewDeployed{URL: url, HealthURL: url + "/healthz"}, nil
		},
		wiring: func(repos []task_previews.StackRepositoryV1) (task_previews.StackWiringReplyV1, error) {
			f.event("wiring")
			f.wiringCalls++
			f.wiringRepos = repos
			if f.wiringErr != nil {
				return task_previews.StackWiringReplyV1{}, f.wiringErr
			}
			return task_previews.StackWiringReplyV1{Links: f.links}, nil
		},
	}
}

// webLink is a link from the site 0-web ("web") to the API 1-api ("api").
func webLink(urlVars []task_previews.StackURLVariableV1, originVars []task_previews.StackOriginVariableV1) task_previews.StackLinkV1 {
	return task_previews.StackLinkV1{FrontendRepository: "0-web", Frontend: "web", APIRepository: "1-api", API: "api",
		APIURLVariables: urlVars, AllowedOriginVariables: originVars}
}

func urlVar(name, path string) []task_previews.StackURLVariableV1 {
	return []task_previews.StackURLVariableV1{{Name: name, Path: path}}
}

func originVar(name, sep string) []task_previews.StackOriginVariableV1 {
	return []task_previews.StackOriginVariableV1{{Name: name, Separator: sep}}
}

// stackStage is a stack-path stage ready for deployAndSpawnQA over the Task's
// repositories names ("<idx>-<name>" made from their order), with sites and
// backends, deploying through f; it records each spawn's spec.
func stackStage(t *testing.T, names, sites, backends []string, f *fakeStackOps) (*qaStage, *lockedBuffer, *[]agentboxSpawnSpec) {
	t.Helper()
	logs := &lockedBuffer{}
	stage := qaTestStage(t, t.TempDir(), nil, logs)
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.AgentboxImage, stackQAImage)
	for _, name := range names {
		stage.ctx.Entries = append(stage.ctx.Entries, tasks.RepositoryEntry{Name: name, CloneURL: "https://github.com/acme/" + name + ".git"})
	}
	stage.stack = true
	stage.repositories, stage.backends = sites, backends
	stage.webDistributions = agenttools.NewWebPreviewDistributions()
	stage.staticDistributions = agenttools.NewWebPreviewDistributions()
	stage.webPreviewDeps = func(record *agenttools.PreviewRecord) *agenttools.DeployWebServicePreviewDeps {
		return &agenttools.DeployWebServicePreviewDeps{Record: record, LogsWriter: logs}
	}
	stage.stackOps = f.ops()
	var specs []agentboxSpawnSpec
	stage.spawnQA = func(s agentboxSpawnSpec) (agentResult, error) {
		f.event("spawn")
		specs = append(specs, s)
		return qaCompleted(signUpPassed), nil
	}
	return stage, logs, &specs
}

// runStackRound runs one round's deploys and spawn.
func runStackRound(t *testing.T, stage *qaStage, round int) (agentResult, error) {
	t.Helper()
	env, err := stage.qaSpawnEnv()
	if err != nil {
		t.Fatal(err)
	}
	stage.roundMergeResults, stage.roundWiring = nil, nil
	return stage.deployAndSpawnQA(round, stackQAImage, t.TempDir(), env)
}

func qaPreviews(t *testing.T, spec agentboxSpawnSpec) []qaPreview {
	t.Helper()
	var previews []qaPreview
	if err := json.Unmarshal([]byte(envMap(spec.env)["QA_PREVIEWS"]), &previews); err != nil {
		t.Fatalf("QA_PREVIEWS: %v", err)
	}
	return previews
}

// --- the version gate --------------------------------------------------------

// Below 1.9.37 (or with a tag that isn't a version) the stage is today's; from
// 1.9.37 the runner deploys every preview and the QA run gets QA_PREVIEWS and the
// verify tools only.
func TestQAStackPathOnlyFrom1937(t *testing.T) {
	for _, tc := range []struct {
		image        string
		wantStack    bool
		wantBackends bool
	}{
		{image: "deploymenthq/agentbox:1.9.36", wantBackends: true},
		{image: "deploymenthq/agentbox:latest"},
		{image: stackQAImage, wantStack: true},
	} {
		t.Run(tc.image, func(t *testing.T) {
			var logs strings.Builder
			stage := backendGateStage(t, &logs, []string{"0-web", "1-api"}, []string{"0-web"}, []string{"1-api"})
			jobs.SetParameterValue[string](stage.parameters, parameters_enums.AgentboxImage, tc.image)
			f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
				links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), nil)}}
			stage.stackOps = f.ops()
			stage.webPreviewDeps = func(record *agenttools.PreviewRecord) *agenttools.DeployWebServicePreviewDeps {
				return &agenttools.DeployWebServicePreviewDeps{Record: record, LogsWriter: io.Discard}
			}
			stage.deployBackend = func(context.Context, agenttools.DeployWebServicePreviewDeps, string) (agenttools.WebServicePreviewDeployed, error) {
				return agenttools.WebServicePreviewDeployed{URL: "https://d1.cloudfront.net", HealthURL: "https://d1.cloudfront.net/healthz"}, nil
			}
			var spec agentboxSpawnSpec
			stage.spawnQA = func(s agentboxSpawnSpec) (agentResult, error) { spec = s; return qaCompleted(signUpPassed), nil }
			stage.runQA = func(round int) (agentResult, error) {
				env, err := stage.qaSpawnEnv()
				if err != nil {
					return agentResult{}, err
				}
				return stage.deployAndSpawnQA(round, tc.image, t.TempDir(), env)
			}
			if _, err := stage.run(); err != nil {
				t.Fatal(err)
			}
			env := envMap(spec.env)
			_, hasPreviews := env["QA_PREVIEWS"]
			_, hasBackends := env["QA_BACKENDS"]
			tools := toolNames(spec)
			if tc.wantStack {
				if !hasPreviews || hasBackends || env["QA_REPOSITORIES"] != "" || f.wiringCalls != 1 {
					t.Errorf("env %v, wiring calls %d; want QA_PREVIEWS, no QA_BACKENDS, blank QA_REPOSITORIES", env, f.wiringCalls)
				}
				if tools != "ping,verify_preview_reachable,verify_preview_rendered" {
					t.Errorf("tools = %s", tools)
				}
				return
			}
			if hasPreviews || f.wiringCalls != 0 || len(f.events) != 0 || env["QA_REPOSITORIES"] != "0-web" || hasBackends != tc.wantBackends {
				t.Errorf("env %v, stack events %v; want today's", env, f.events)
			}
			if !strings.Contains(tools, "deploy_static_site_preview") {
				t.Errorf("tools = %s", tools)
			}
		})
	}
}

// --- the gate ----------------------------------------------------------------

func TestQAStackGateDeploysTheUnchangedRepositoriesToo(t *testing.T) {
	for _, image := range []string{stackQAImage, "deploymenthq/agentbox:1.9.36"} {
		t.Run(image, func(t *testing.T) {
			var logs strings.Builder
			workDir := t.TempDir()
			bases := []baseCommitOutput{
				{Index: 0, Dir: "0-web", CommitSHA: qaTestRepo(t, workDir, "0-web")},
				{Index: 1, Dir: "1-api", CommitSHA: qaTestRepo(t, workDir, "1-api")},
			}
			stage := qaTestStage(t, workDir, bases, &logs)
			jobs.SetParameterValue[string](stage.parameters, parameters_enums.AgentboxImage, image)
			stage.checkPreview = func(dir string) agenttools.RunnerBuiltPreviewCheck {
				return agenttools.RunnerBuiltPreviewCheck{OK: dir == "0-web", Reason: "not a static site"}
			}
			stage.checkBackend = func(dir string) error {
				if dir == "1-api" {
					return nil
				}
				return errors.New("no web service")
			}
			if g := stage.gate(); !reflect.DeepEqual(g, qaGate{}) {
				t.Errorf("no change: gate = %+v, want no QA", g)
			}
			writeFile(t, filepath.Join(workDir, "1-api", "changed.txt"), "changed\n")
			g := stage.gate()
			const line = "QA stage: deploying 0-web too: unchanged, but part of this change's stack"
			if image == stackQAImage {
				if !g.stack || !reflect.DeepEqual(g.static, []string{"0-web"}) || !reflect.DeepEqual(g.backends, []string{"1-api"}) || !strings.Contains(logs.String(), line) {
					t.Errorf("gate = %+v, log:\n%s", g, logs.String())
				}
				return
			}
			if g.stack || len(g.static) != 0 || !reflect.DeepEqual(g.backends, []string{"1-api"}) || strings.Contains(logs.String(), line) {
				t.Errorf("gate = %+v", g)
			}
		})
	}
}

// --- a round -----------------------------------------------------------------

// The site's distribution is reserved before the backend deploys with the site's
// origin; the site builds after, pointed at the backend; round 2 does the same
// with the stage's memos, without a second wiring lookup.
func TestQAStackRoundWiresTheSiteToTheBackend(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", "/v1"), originVar("CORS_ORIGINS", ","))}}
	stage, logs, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	for round := 1; round <= 2; round++ {
		f.events = nil
		if _, err := runStackRound(t, stage, round); err != nil {
			t.Fatal(err)
		}
		want := []string{"plan site 0-web", "plan backend 1-api", "reserve 0-web", "deploy backend 1-api", "deploy site 0-web", "spawn"}
		if round == 1 {
			want = []string{"plan site 0-web", "plan backend 1-api", "wiring", "reserve 0-web", "deploy backend 1-api", "deploy site 0-web", "spawn"}
		}
		if !reflect.DeepEqual(f.events, want) {
			t.Errorf("round %d: events = %v\nwant %v", round, f.events, want)
		}
		if got := f.backendVars["1-api"]; !reflect.DeepEqual(got, map[string]string{"CORS_ORIGINS": "https://s-0-web.cloudfront.net"}) {
			t.Errorf("round %d: backend variables = %v", round, got)
		}
		if got := f.siteVars["0-web"]; !reflect.DeepEqual(got, map[string]string{"VITE_API_URL": "https://b-1-api.cloudfront.net/v1"}) {
			t.Errorf("round %d: site variables = %v", round, got)
		}
		spec := (*specs)[round-1]
		got := envMap(spec.env)["QA_PREVIEWS"]
		const wantPreviews = `[{"repository":"0-web","kind":"site","url":"https://s-0-web.cloudfront.net","connected_backends":["1-api"]},` +
			`{"repository":"1-api","kind":"backend","url":"https://b-1-api.cloudfront.net","health_url":"https://b-1-api.cloudfront.net/healthz"}]`
		if got != wantPreviews {
			t.Errorf("round %d: QA_PREVIEWS = %s\nwant %s", round, got, wantPreviews)
		}
		if hosts := strings.Join(spec.previewDeps.Record.Hosts(), ","); hosts != "b-1-api.cloudfront.net,s-0-web.cloudfront.net" {
			t.Errorf("round %d: the verify tools accept %s", round, hosts)
		}
		if !spec.verifyToolsOnly {
			t.Errorf("round %d: the spawn serves the deploy tools", round)
		}
		wantWiring := []qaWiringOutput{{Frontend: "web", API: "api", Connected: true, FrontendVariables: []string{"VITE_API_URL"}, APIVariables: []string{"CORS_ORIGINS"}}}
		if !reflect.DeepEqual(stage.roundWiring, wantWiring) {
			t.Errorf("round %d: wiring = %+v", round, stage.roundWiring)
		}
	}
	if f.wiringCalls != 1 || !reflect.DeepEqual(f.wiringRepos, []task_previews.StackRepositoryV1{
		{Repository: "0-web", CloneURL: "https://github.com/acme/web.git"}, {Repository: "1-api", CloneURL: "https://github.com/acme/api.git"},
	}) {
		t.Errorf("wiring looked up %d times with %+v", f.wiringCalls, f.wiringRepos)
	}
	// Every round deploys with the stage's one memo of each kind.
	for _, d := range f.siteDeps {
		if d.Distributions != stage.staticDistributions || d.MergeResult == nil {
			t.Error("a site deployed without the stage's distributions memo or the round's merge results")
		}
	}
	for _, d := range f.webDeps {
		if d.Distributions != stage.webDistributions || d.MergeResult == nil {
			t.Error("a backend deployed without the stage's distributions memo or the round's merge results")
		}
	}
	const line = "QA round 2: QA built web's preview to call api's preview (VITE_API_URL) and allowed its origin on api's preview (CORS_ORIGINS)."
	if !strings.Contains(logs.String(), line) {
		t.Errorf("the log lacks %q:\n%s", line, logs.String())
	}
}

func TestQAStackLinkSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		site  string // 0-web's deployment, "" for a detected site
		links []task_previews.StackLinkV1
	}{
		{name: "another static site of the repository", site: "web", links: []task_previews.StackLinkV1{
			func() task_previews.StackLinkV1 {
				l := webLink(urlVar("VITE_API_URL", ""), nil)
				l.Frontend = "web-staging"
				return l
			}()}},
		{name: "another web service of the repository", site: "web", links: []task_previews.StackLinkV1{
			func() task_previews.StackLinkV1 {
				l := webLink(urlVar("VITE_API_URL", ""), nil)
				l.API = "api-worker"
				return l
			}()}},
		{name: "a detected site", site: "", links: []task_previews.StackLinkV1{
			func() task_previews.StackLinkV1 {
				l := webLink(urlVar("VITE_API_URL", ""), nil)
				l.Frontend = ""
				return l
			}()}},
		{name: "only an allowed-origin variable", site: "web", links: []task_previews.StackLinkV1{webLink(nil, originVar("CORS_ORIGINS", ","))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStackOps{sites: map[string]string{"0-web": tc.site}, backends: map[string]string{"1-api": "api"}, links: tc.links}
			stage, _, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
			if _, err := runStackRound(t, stage, 1); err != nil {
				t.Fatal(err)
			}
			for _, e := range f.events {
				if strings.HasPrefix(e, "reserve") {
					t.Errorf("reserved an unlinked site: %v", f.events)
				}
			}
			if len(f.backendVars["1-api"]) != 0 || len(f.siteVars["0-web"]) != 0 {
				t.Errorf("variables set: backend %v, site %v", f.backendVars["1-api"], f.siteVars["0-web"])
			}
			want := []qaWiringOutput{{Frontend: "web", API: "api", Reason: qaWiringNoLink}}
			if !reflect.DeepEqual(stage.roundWiring, want) {
				t.Errorf("wiring = %+v", stage.roundWiring)
			}
			if p := qaPreviews(t, (*specs)[0]); !reflect.DeepEqual(p[0].UnconnectedBackends, []string{"1-api"}) || len(p[0].ConnectedBackends) != 0 {
				t.Errorf("site entry = %+v", p[0])
			}
		})
	}
}

// Two sites on one backend: a "," variable lists both origins in work-dir
// order; a "" one holds the first site's only, and the second isn't connected.
func TestQAStackTwoSitesOnOneBackend(t *testing.T) {
	links := func(sep string) []task_previews.StackLinkV1 {
		var out []task_previews.StackLinkV1
		for _, site := range []string{"0-admin", "1-web"} {
			out = append(out, task_previews.StackLinkV1{FrontendRepository: site, Frontend: site, APIRepository: "2-api", API: "api",
				APIURLVariables: urlVar("API_URL", ""), AllowedOriginVariables: originVar("ALLOWED_ORIGIN", sep)})
		}
		return out
	}
	t.Run("comma", func(t *testing.T) {
		f := &fakeStackOps{sites: map[string]string{"0-admin": "0-admin", "1-web": "1-web"}, backends: map[string]string{"2-api": "api"}, links: links(",")}
		stage, _, _ := stackStage(t, []string{"admin", "web", "api"}, []string{"0-admin", "1-web"}, []string{"2-api"}, f)
		if _, err := runStackRound(t, stage, 1); err != nil {
			t.Fatal(err)
		}
		if got := f.backendVars["2-api"]["ALLOWED_ORIGIN"]; got != "https://s-0-admin.cloudfront.net,https://s-1-web.cloudfront.net" {
			t.Errorf("ALLOWED_ORIGIN = %q", got)
		}
		for _, site := range []string{"0-admin", "1-web"} {
			if f.siteVars[site]["API_URL"] != "https://b-2-api.cloudfront.net" {
				t.Errorf("%s variables = %v", site, f.siteVars[site])
			}
		}
	})
	t.Run("single origin", func(t *testing.T) {
		f := &fakeStackOps{sites: map[string]string{"0-admin": "0-admin", "1-web": "1-web"}, backends: map[string]string{"2-api": "api"}, links: links("")}
		stage, _, specs := stackStage(t, []string{"admin", "web", "api"}, []string{"0-admin", "1-web"}, []string{"2-api"}, f)
		if _, err := runStackRound(t, stage, 1); err != nil {
			t.Fatal(err)
		}
		if got := f.backendVars["2-api"]; !reflect.DeepEqual(got, map[string]string{"ALLOWED_ORIGIN": "https://s-0-admin.cloudfront.net"}) {
			t.Errorf("backend variables = %v", got)
		}
		if len(f.siteVars["1-web"]) != 0 || f.siteVars["0-admin"]["API_URL"] == "" {
			t.Errorf("site variables = %v", f.siteVars)
		}
		w := stage.roundWiring[1]
		if w.Connected || w.Reason != qaWiringSingleOrigin {
			t.Errorf("1-web's wiring = %+v", w)
		}
		const line = "QA didn't connect web's preview to api's preview: api's production setting ALLOWED_ORIGIN holds a single origin, used by admin's preview."
		if got := qaWiringLine(w, stage.roundWiring); got != line {
			t.Errorf("line = %q", got)
		}
		p := qaPreviews(t, (*specs)[0])
		if !reflect.DeepEqual(p[0].ConnectedBackends, []string{"2-api"}) || !reflect.DeepEqual(p[1].UnconnectedBackends, []string{"2-api"}) {
			t.Errorf("previews = %+v", p)
		}
	})
}

func TestQAStackBackendFailed(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links:     []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), originVar("CORS_ORIGINS", ","))},
		deployErr: map[string]error{"backend:1-api": errors.New("The backend preview's deploy failed: the image build failed\nlog tail")}}
	stage, _, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.siteVars["0-web"]) != 0 {
		t.Errorf("the site was wired to a failed backend: %v", f.siteVars["0-web"])
	}
	got := envMap((*specs)[0].env)["QA_PREVIEWS"]
	const want = `[{"repository":"0-web","kind":"site","url":"https://s-0-web.cloudfront.net","unconnected_backends":["1-api"]},` +
		`{"repository":"1-api","kind":"backend","error":"The backend preview's deploy failed: the image build failed"}]`
	if got != want {
		t.Errorf("QA_PREVIEWS = %s\nwant %s", got, want)
	}
	if line := qaWiringLine(stage.roundWiring[0], stage.roundWiring); line != "QA didn't connect web's preview to api's preview: api's preview failed to deploy." {
		t.Errorf("line = %q", line)
	}
}

// A site whose plan, reservation or build fails isn't connected to the backend
// it is linked to: "site failed".
func TestQAStackSiteFailed(t *testing.T) {
	siteErr := errors.New("the preview build of web failed")
	for _, tc := range []struct {
		name                           string
		planErr, deployErr, reserveErr map[string]error
	}{
		{name: "plan", planErr: map[string]error{"site:0-web": siteErr}},
		{name: "reserve", reserveErr: map[string]error{"0-web": siteErr}},
		{name: "build", deployErr: map[string]error{"site:0-web": siteErr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
				links:   []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), originVar("CORS_ORIGINS", ","))},
				planErr: tc.planErr, deployErr: tc.deployErr, reserveErr: tc.reserveErr}
			stage, _, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
			if _, err := runStackRound(t, stage, 1); err != nil || len(*specs) != 1 {
				t.Fatalf("err %v, spawns %d; want QA to run", err, len(*specs))
			}
			if want := []qaWiringOutput{{Frontend: "web", API: "api", Reason: qaWiringSiteFailed}}; !reflect.DeepEqual(stage.roundWiring, want) {
				t.Errorf("wiring = %+v", stage.roundWiring)
			}
			if line := qaWiringLine(stage.roundWiring[0], stage.roundWiring); line != "QA didn't connect web's preview to api's preview: web's preview failed to deploy." {
				t.Errorf("line = %q", line)
			}
			if tc.name == "reserve" {
				if got, ok := f.backendVars["1-api"]; !ok || len(got) != 0 {
					t.Errorf("backend variables = %v (deployed %v); want deployed without the site's origin", got, ok)
				}
			}
		})
	}
}

// A site whose reservation failed keeps "site failed" when its backend then
// fails too.
func TestQAStackSiteFailedThenBackendFailed(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links:      []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), originVar("CORS_ORIGINS", ","))},
		reserveErr: map[string]error{"0-web": errors.New("the distribution couldn't be reserved")},
		deployErr:  map[string]error{"backend:1-api": errors.New("the image build failed")}}
	stage, _, _ := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	runStackRound(t, stage, 1)
	if want := []qaWiringOutput{{Frontend: "web", API: "api", Reason: qaWiringSiteFailed}}; !reflect.DeepEqual(stage.roundWiring, want) {
		t.Errorf("wiring = %+v", stage.roundWiring)
	}
}

// A site that lost a single origin and then fails to build is "site failed".
func TestQAStackSingleOriginThenSiteFailed(t *testing.T) {
	var links []task_previews.StackLinkV1
	for _, site := range []string{"0-admin", "1-web"} {
		links = append(links, task_previews.StackLinkV1{FrontendRepository: site, Frontend: site, APIRepository: "2-api", API: "api",
			APIURLVariables: urlVar("API_URL", ""), AllowedOriginVariables: originVar("ALLOWED_ORIGIN", "")})
	}
	f := &fakeStackOps{sites: map[string]string{"0-admin": "0-admin", "1-web": "1-web"}, backends: map[string]string{"2-api": "api"}, links: links,
		deployErr: map[string]error{"site:1-web": errors.New("the preview build of web failed")}}
	stage, _, _ := stackStage(t, []string{"admin", "web", "api"}, []string{"0-admin", "1-web"}, []string{"2-api"}, f)
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if w := stage.roundWiring[1]; !reflect.DeepEqual(w, qaWiringOutput{Frontend: "web", API: "api", Reason: qaWiringSiteFailed}) {
		t.Errorf("1-web's wiring = %+v", w)
	}
	if w := stage.roundWiring[0]; !w.Connected {
		t.Errorf("0-admin's wiring = %+v", w)
	}
}

// Every preview failed: the round couldn't run and nothing is spawned.
func TestQAStackNothingDeployed(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		planErr:   map[string]error{"site:0-web": errors.New("web has no build command, so its preview can't be built")},
		deployErr: map[string]error{"backend:1-api": errors.New("the image build failed")}}
	stage, _, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	result, err := runStackRound(t, stage, 1)
	const want = "no preview deployed: web has no build command, so its preview can't be built"
	if err == nil || err.Error() != want || len(*specs) != 0 {
		t.Fatalf("err = %v, spawns %d; want %q and no spawn", err, len(*specs), want)
	}
	if out := stage.roundOutput(1, result, err); out.Status != tasks.QAStatusCouldNotRun || out.StatusReason != want {
		t.Errorf("round = %s %q", out.Status, out.StatusReason)
	}
}

func TestQAStackWiringLookupFailed(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), nil)}, wiringErr: errors.New("connection refused")}
	stage, logs, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	if _, err := runStackRound(t, stage, 1); err != nil || len(*specs) != 1 {
		t.Fatalf("err %v, spawns %d; want QA to run", err, len(*specs))
	}
	if want := []qaWiringOutput{{Frontend: "web", API: "api", Reason: qaWiringLookupFailed}}; !reflect.DeepEqual(stage.roundWiring, want) {
		t.Errorf("wiring = %+v", stage.roundWiring)
	}
	if !strings.Contains(logs.String(), "QA stage: couldn't look up how the previews connect: connection refused") {
		t.Errorf("log:\n%s", logs.String())
	}
}

// One repository with both previews, a failed site and a failed backend.
func TestQAStackPreviewsContract(t *testing.T) {
	f := &fakeStackOps{
		sites:    map[string]string{"0-app": "app-web", "1-docs": "docs"},
		backends: map[string]string{"0-app": "app-api", "2-worker": "worker"},
		links: []task_previews.StackLinkV1{{FrontendRepository: "0-app", Frontend: "app-web", APIRepository: "0-app", API: "app-api",
			APIURLVariables: urlVar("API_URL", "/api")}},
		deployErr: map[string]error{
			"site:1-docs":      errors.New("the preview build of docs failed (npm run build): exit status 1. Its output is in the Step log"),
			"backend:2-worker": errors.New(strings.Repeat("x", 400)),
		},
	}
	stage, _, specs := stackStage(t, []string{"app", "docs", "worker"}, []string{"0-app", "1-docs"}, []string{"0-app", "2-worker"}, f)
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	got := envMap((*specs)[0].env)["QA_PREVIEWS"]
	want := `[{"repository":"0-app","kind":"site","url":"https://s-0-app.cloudfront.net","connected_backends":["0-app"],"unconnected_backends":["2-worker"]},` +
		`{"repository":"0-app","kind":"backend","url":"https://b-0-app.cloudfront.net","health_url":"https://b-0-app.cloudfront.net/healthz"},` +
		`{"repository":"1-docs","kind":"site","error":"the preview build of docs failed (npm run build): exit status 1. Its output is in the Step log"},` +
		`{"repository":"2-worker","kind":"backend","error":"` + strings.Repeat("x", 297) + `…"}]`
	if got != want {
		t.Errorf("QA_PREVIEWS = %s\nwant %s", got, want)
	}
	if f.siteVars["0-app"]["API_URL"] != "https://b-0-app.cloudfront.net/api" {
		t.Errorf("site variables = %v", f.siteVars["0-app"])
	}
}

// --- the report ----------------------------------------------------------------

func TestQAWiringLines(t *testing.T) {
	for _, tc := range []struct {
		w    qaWiringOutput
		want string
	}{
		{qaWiringOutput{Frontend: "web", API: "api", Connected: true, FrontendVariables: []string{"A_URL", "B_URL"}, APIVariables: []string{"CORS"}},
			"QA built web's preview to call api's preview (A_URL, B_URL) and allowed its origin on api's preview (CORS)."},
		{qaWiringOutput{Frontend: "web", API: "api", Connected: true, FrontendVariables: []string{"A_URL"}},
			"QA built web's preview to call api's preview (A_URL). No production setting of api lists web's address, so api's preview allows the origins Preview configuration gives it."},
		{qaWiringOutput{Frontend: "web", API: "api", Reason: qaWiringNoLink},
			"QA didn't connect web's preview to api's preview: no production setting of web holds api's address."},
		{qaWiringOutput{Frontend: "web", API: "api", Reason: qaWiringBackendFailed},
			"QA didn't connect web's preview to api's preview: api's preview failed to deploy."},
		{qaWiringOutput{Frontend: "web", API: "api", Reason: qaWiringSiteFailed},
			"QA didn't connect web's preview to api's preview: web's preview failed to deploy."},
		{qaWiringOutput{Frontend: "web", API: "api", Reason: qaWiringLookupFailed},
			"QA didn't connect web's preview to api's preview: deployment.io couldn't look up their production settings."},
	} {
		if got := qaWiringLine(tc.w, nil); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
}

// The final round's pairs are listed after the "QA checked …" lines, also when
// the round couldn't run; no value, path or production host reaches the Job
// output, the pull request or the Step log.
func TestQAStackReportCarriesNamesOnly(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", secretPath), originVar("CORS_ORIGINS", " "))}}
	stage, logs, _ := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	stage.out = &qaOutput{Repositories: stage.repositories, Backends: stage.backends}
	stage.runQA = func(round int) (agentResult, error) {
		env, err := stage.qaSpawnEnv()
		if err != nil {
			return agentResult{}, err
		}
		result, err := stage.deployAndSpawnQA(round, stackQAImage, t.TempDir(), env)
		stage.roundMergeResults = []qaMergeResultOutput{{Repository: "web", BaseBranch: "main", BaseCommit: strings.Repeat("a", 40), Merged: true}}
		return result, err
	}
	if _, err := stage.qaRound(1); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.finish(); err != nil {
		t.Fatal(err)
	}
	if f.siteVars["0-web"]["VITE_API_URL"] != "https://b-1-api.cloudfront.net"+secretPath {
		t.Fatalf("the site wasn't built with the path: %v", f.siteVars["0-web"])
	}
	jobOutput, _ := jobs.GetParameterValue[string](stage.parameters, parameters_enums.JobOutput)
	qa := readQAFromJobOutput(stage.parameters)
	if qa == nil || len(qa.Rounds) != 1 || len(qa.Rounds[0].Wiring) != 1 || !reflect.DeepEqual(qa.Backends, []string{"1-api"}) {
		t.Fatalf("qa = %+v", qa)
	}
	section := (&taskOpenPR{qa: qa}).qaSection()
	checked := strings.Index(section, "QA checked web merged with main")
	built := strings.Index(section, "- QA built web's preview to call api's preview (VITE_API_URL) and allowed its origin on api's preview (CORS_ORIGINS).")
	if checked < 0 || built < checked {
		t.Errorf("section:\n%s", section)
	}
	for name, text := range map[string]string{"job output": jobOutput, "pull request": section, "log": logs.String()} {
		if strings.Contains(text, secretPath) || strings.Contains(text, strings.TrimPrefix(secretPath, "/")) {
			t.Errorf("the %s carries the production path:\n%s", name, text)
		}
	}

	// A round that couldn't run still lists its pairs.
	qa.Status, qa.StatusReason = tasks.QAStatusCouldNotRun, "the QA run produced no qa_result"
	qa.Rounds[0].Status, qa.Rounds[0].Criteria = tasks.QAStatusCouldNotRun, nil
	qa.Rounds[0].MergeResults = nil
	if section := (&taskOpenPR{qa: qa}).qaSection(); !strings.Contains(section, "QA built web's preview") {
		t.Errorf("couldn't-run section:\n%s", section)
	}
}

// --- precedence on a backend ------------------------------------------------------

// A wired variable replaces Preview configuration's in the task definition and
// the build args; the preview database's variable still wins over it.
func TestQAStackBackendPrecedence(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "task")
	writeFile(t, filepath.Join(workDir, "1-api", "Dockerfile"), "FROM scratch\n")
	service := task_previews.WebServicePreviewSettingsV1{DeploymentName: "api", EnvironmentName: "production", Region: "us-east-1",
		DockerBuildArgs: []string{"CORS_ORIGINS=prod", "DATABASE_URL=prod"}, Port: 8080, HealthCheckPath: "/healthz", Cpu: "256", Memory: "512",
		PreviewDatabase: &task_previews.PreviewDatabaseV1{Mode: task_previews.PreviewDatabaseModeUse, Engine: "postgres", Version: "16", Variables: []string{"DATABASE_URL"}}}
	var env string
	var buildArgs interface{}
	logs := &lockedBuffer{}
	deps := agenttools.DeployWebServicePreviewDeps{
		Region: "us-east-1", WorkDirHost: workDir, LogsWriter: logs,
		Store: &fakeQAWebStore{}, DatabaseStore: &fakeQAWebStore{},
		Record:       agenttools.NewPreviewRecord(nil, io.Discard),
		Repositories: []tasks.RepositoryEntry{{Name: "web"}, {Name: "api"}},
		Settings: func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
			return task_previews.WebServicePreviewSettingsReplyV1{Services: []task_previews.WebServicePreviewSettingsV1{service}, NetworkReady: true,
				NamespaceName: "previews-1b2c", ConfigurationSet: true, Variables: map[string]string{"CORS_ORIGINS": "https://staging.example", "DATABASE_URL": "postgres://staging", "KEEP": "1"}}, nil
		},
		Deploy: func(_ context.Context, in agenttools.WebServicePreviewDeployInput) (agenttools.WebServicePreviewDeployOutput, error) {
			p, err := buildWebPreviewParameters(webPreviewParametersInput{OrganizationIDNamespace: "orgns", OrganizationIdFromJob: testWebOrgID, Region: 1, Deploy: in})
			if err != nil {
				return agenttools.WebServicePreviewDeployOutput{}, err
			}
			if err := setWebPreviewDatabaseParameters(p, in, webPreviewDatabase{host: "db.previews-1b2c"}); err != nil {
				return agenttools.WebServicePreviewDeployOutput{}, err
			}
			env = paramValue(t, p, parameters_enums.EnvironmentVariables).(string)
			buildArgs = paramValue(t, p, parameters_enums.DockerBuildArgs)
			return agenttools.WebServicePreviewDeployOutput{LoadBalancerDNS: "alb.elb.amazonaws.com"}, nil
		},
		BuildCloudFront: func() (agenttools.WebPreviewCloudFront, error) { return &fakeQACloudFront{}, nil },
	}
	plan, err := agenttools.PlanWebServicePreview(deps, "1-api")
	if err != nil {
		t.Fatal(err)
	}
	wired := map[string]string{"CORS_ORIGINS": "https://s-0-web.cloudfront.net", "DATABASE_URL": "https://s-0-web.cloudfront.net"}
	if _, err := agenttools.DeployWebServicePreviewWith(context.Background(), deps, plan, wired); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CORS_ORIGINS=https://s-0-web.cloudfront.net\n", "KEEP=1\n", "DATABASE_URL=postgresql://"} {
		if !strings.Contains(env, want) {
			t.Errorf("environment lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "DATABASE_URL=https://") || strings.Contains(env, "staging") {
		t.Errorf("the environment carries the wired or Preview configuration DATABASE_URL:\n%s", env)
	}
	// The database's value wins in the build args too: neither the wired value nor
	// Preview configuration's reaches the image build.
	args := fmt.Sprint(buildArgs)
	if !strings.HasPrefix(args, "[CORS_ORIGINS=https://s-0-web.cloudfront.net DATABASE_URL=postgresql://") {
		t.Errorf("build args = %v", buildArgs)
	}
	if strings.Contains(args, "DATABASE_URL=https://") || strings.Contains(args, "staging") {
		t.Errorf("build args carry the wired or Preview configuration DATABASE_URL: %v", buildArgs)
	}
	if !strings.Contains(logs.String(), "DATABASE_URL replaces the value QA set for it") {
		t.Errorf("log:\n%s", logs.String())
	}
}
