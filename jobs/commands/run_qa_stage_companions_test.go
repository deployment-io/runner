package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
)

const companionQAImage = "deploymenthq/agentbox:1.9.48"

// companionToken is the installation token a companion's fetch carries: it
// must never reach an error, the Step log, QA_PREVIEWS or the Job output.
const companionToken = "ghs_c0mpanionInstallationToken"

// companionLookupArgs are a CompanionBackendsV1 call's arguments.
type companionLookupArgs struct {
	repositories []task_previews.StackRepositoryV1
	frontends    []task_previews.CompanionFrontendV1
	region       string
}

// checkoutFiles runs checkout into a new directory and returns every file it
// wrote, path → contents; the directory is removed.
func checkoutFiles(checkout func(ctx context.Context, dir string) error) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "companion-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := checkout(context.Background(), dir); err != nil {
		return nil, err
	}
	files := map[string]string{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	})
	return files, err
}

// appServer is the companion acme/app-server ("app-server", deployed from main)
// that 0-web ("web") calls through VITE_APP_URL (path /v2), its origin allowed
// in ALLOWED_ORIGINS.
func appServer() task_previews.CompanionBackendV1 {
	return task_previews.CompanionBackendV1{
		DeploymentID: "65a1",
		API:          "app-server",
		Repository:   tasks.RepositoryEntry{Name: "acme/app-server", CloneURL: "https://github.com/acme/app-server.git", BaseBranch: "main", InstallationID: "inst-1"},
		Links: []task_previews.CompanionLinkV1{{FrontendRepository: "0-web", Frontend: "web",
			APIURLVariables: urlVar("VITE_APP_URL", "/v2"), AllowedOriginVariables: originVar("ALLOWED_ORIGINS", ",")}},
	}
}

// companionOrigin is a repository on disk with files on its main branch, the
// stand-in for a companion's GitHub repository.
type companionOrigin struct {
	dir string
}

func newCompanionOrigin(t *testing.T, files map[string]string) *companionOrigin {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	o := &companionOrigin{dir: filepath.Join(t.TempDir(), "origin")}
	for name, contents := range files {
		writeFile(t, filepath.Join(o.dir, name), contents)
	}
	runGit(t, o.dir, "init", "-q")
	runGit(t, o.dir, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, o.dir, "add", "-A")
	runGit(t, o.dir, "commit", "-q", "-m", "production")
	return o
}

func (o *companionOrigin) head(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, o.dir, "rev-parse", "HEAD"))
}

// advance commits files onto main.
func (o *companionOrigin) advance(t *testing.T, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		writeFile(t, filepath.Join(o.dir, name), contents)
	}
	runGit(t, o.dir, "add", "-A")
	runGit(t, o.dir, "commit", "-q", "-m", "main moves on")
}

// companionStage is stackStage with the stage's merge-result fetch source
// pointing at origin (counting the lookups, and handing out companionToken as
// the secret), its temporary root under the test's, and the runner's region
// eu-west-1.
func companionStage(t *testing.T, names, sites, backends []string, f *fakeStackOps, origin *companionOrigin) (*qaStage, *lockedBuffer, *[]agentboxSpawnSpec, *int) {
	t.Helper()
	stage, logs, specs := stackStage(t, names, sites, backends, f)
	stage.fixTreesTmpRoot = t.TempDir()
	t.Cleanup(stage.removeCompanionSources)
	stage.webPreviewDeps = func(record *agenttools.PreviewRecord) *agenttools.DeployWebServicePreviewDeps {
		return &agenttools.DeployWebServicePreviewDeps{Region: "eu-west-1", Record: record, LogsWriter: logs}
	}
	var mu sync.Mutex
	fetches := 0
	stage.mergeFetchSource = func(context.Context, tasks.RepositoryEntry) (string, string, error) {
		mu.Lock()
		fetches++
		mu.Unlock()
		return origin.dir, companionToken, nil
	}
	return stage, logs, specs, &fetches
}

// runCompanionRound runs one round's deploys and spawn with an agentbox that
// reads companions.
func runCompanionRound(t *testing.T, stage *qaStage, round int) (agentResult, error) {
	t.Helper()
	return runStackRoundWith(t, stage, round, companionQAImage)
}

func runStackRoundWith(t *testing.T, stage *qaStage, round int, image string) (agentResult, error) {
	t.Helper()
	env, err := stage.qaSpawnEnv()
	if err != nil {
		t.Fatal(err)
	}
	stage.roundMergeResults, stage.roundWiring = nil, nil
	stage.roundCompanions, stage.roundBackendsNotDeployed = nil, nil
	return stage.deployAndSpawnQA(round, image, t.TempDir(), env)
}

func companionName() string { return "companion-0-acme/app-server" }

// --- a round with a companion --------------------------------------------------

// A site linked to a companion is reserved, the companion deploys after the
// Task's backend with the site's origin, and the site builds pointed at both,
// each variable at its own backend. Round 2 reuses the lookup and builds the same
// commit, though main has moved on.
func TestQACompanionRoundDeploysAndWiresTheCompanion(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n", "app.txt": "production\n"})
	sha := origin.head(t)
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, environments: map[string]string{"0-web": "production"},
		backends: map[string]string{"1-api": "api"},
		links:    []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), originVar("CORS_ORIGINS", ","))},
		companionReply: task_previews.CompanionBackendsReplyV1{
			Companions: []task_previews.CompanionBackendV1{appServer()},
			NotDeployed: []task_previews.CompanionNotDeployedV1{
				{FrontendRepository: "0-web", Frontend: "web", Variable: "VITE_PAY_URL", API: "payments", Reason: task_previews.ReasonNotDockerfile},
				{FrontendRepository: "0-web", Frontend: "web", Variable: "VITE_X_URL", Reason: task_previews.ReasonAmbiguous},
			},
		}}
	stage, logs, specs, fetches := companionStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f, origin)
	companion := companionName()
	for round := 1; round <= 2; round++ {
		f.events = nil
		if _, err := runCompanionRound(t, stage, round); err != nil {
			t.Fatal(err)
		}
		want := []string{"plan site 0-web", "plan backend 1-api", "plan companion " + companion, "reserve 0-web", "deploy backend 1-api", "deploy backend " + companion, "deploy site 0-web", "spawn"}
		if round == 1 {
			want = []string{"plan site 0-web", "plan backend 1-api", "companions", "plan companion " + companion, "wiring", "reserve 0-web", "deploy backend 1-api", "deploy backend " + companion, "deploy site 0-web", "spawn"}
		}
		if !reflect.DeepEqual(f.events, want) {
			t.Errorf("round %d: events = %v\nwant %v", round, f.events, want)
		}
		if got := f.backendVars[companion]; !reflect.DeepEqual(got, map[string]string{"ALLOWED_ORIGINS": "https://s-0-web.cloudfront.net"}) {
			t.Errorf("round %d: companion variables = %v", round, got)
		}
		if got := f.backendVars["1-api"]; !reflect.DeepEqual(got, map[string]string{"CORS_ORIGINS": "https://s-0-web.cloudfront.net"}) {
			t.Errorf("round %d: backend variables = %v", round, got)
		}
		wantSite := map[string]string{"VITE_API_URL": "https://b-1-api.cloudfront.net", "VITE_APP_URL": "https://b-companion-0-acme-app-server.cloudfront.net/v2"}
		if got := f.siteVars["0-web"]; !reflect.DeepEqual(got, wantSite) {
			t.Errorf("round %d: site variables = %v", round, got)
		}
		got := envMap((*specs)[round-1].env)["QA_PREVIEWS"]
		wantPreviews := `[{"repository":"0-web","kind":"site","url":"https://s-0-web.cloudfront.net","connected_backends":["1-api","companion-0-acme/app-server"],` +
			`"backends_not_deployed":[{"variable":"VITE_PAY_URL","api":"payments","reason":"the web service isn't built from a Dockerfile"},{"variable":"VITE_X_URL","reason":"matches more than one web service"}]},` +
			`{"repository":"1-api","kind":"backend","url":"https://b-1-api.cloudfront.net","health_url":"https://b-1-api.cloudfront.net/healthz"},` +
			`{"repository":"companion-0-acme/app-server","kind":"backend","url":"https://b-companion-0-acme-app-server.cloudfront.net","health_url":"https://b-companion-0-acme-app-server.cloudfront.net/healthz","companion":true,"deployment":"app-server"}]`
		if got != wantPreviews {
			t.Errorf("round %d: QA_PREVIEWS = %s\nwant %s", round, got, wantPreviews)
		}
		wantWiring := []qaWiringOutput{
			{Frontend: "web", API: "api", Connected: true, FrontendVariables: []string{"VITE_API_URL"}, APIVariables: []string{"CORS_ORIGINS"}},
			{Frontend: "web", API: "acme/app-server", Connected: true, FrontendVariables: []string{"VITE_APP_URL"}, APIVariables: []string{"ALLOWED_ORIGINS"}, Companion: true},
		}
		if !reflect.DeepEqual(stage.roundWiring, wantWiring) {
			t.Errorf("round %d: wiring = %+v", round, stage.roundWiring)
		}
		wantCompanions := []qaCompanionOutput{{API: "app-server", Repository: "acme/app-server", Branch: "main", Commit: sha, ServiceName: "svc-" + companion, Deployed: true}}
		if !reflect.DeepEqual(stage.roundCompanions, wantCompanions) {
			t.Errorf("round %d: companions = %+v", round, stage.roundCompanions)
		}
		wantNotDeployed := []qaBackendNotDeployedOutput{
			{Frontend: "web", Variable: "VITE_PAY_URL", API: "payments", Reason: task_previews.ReasonNotDockerfile},
			{Frontend: "web", Variable: "VITE_X_URL", Reason: task_previews.ReasonAmbiguous},
		}
		if !reflect.DeepEqual(stage.roundBackendsNotDeployed, wantNotDeployed) {
			t.Errorf("round %d: backends not deployed = %+v", round, stage.roundBackendsNotDeployed)
		}
		if round == 1 {
			origin.advance(t, map[string]string{"app.txt": "newer\n"})
		}
	}
	if f.companionCalls != 1 || *fetches != 1 {
		t.Errorf("lookups %d, source fetches %d; want one of each per stage", f.companionCalls, *fetches)
	}
	wantArgs := companionLookupArgs{
		repositories: []task_previews.StackRepositoryV1{{Repository: "0-web", CloneURL: "https://github.com/acme/web.git"}, {Repository: "1-api", CloneURL: "https://github.com/acme/api.git"}},
		frontends:    []task_previews.CompanionFrontendV1{{Repository: "0-web", Frontend: "web", Environment: "production"}},
		region:       "eu-west-1",
	}
	if !reflect.DeepEqual(f.companionArgs, wantArgs) {
		t.Errorf("lookup args = %+v", f.companionArgs)
	}
	// Both rounds built the commit fetched in round 1.
	if len(f.built) != 2 || f.built[0]["app.txt"] != "production\n" || !reflect.DeepEqual(f.built[0], f.built[1]) {
		t.Errorf("built = %v", f.built)
	}
	for _, line := range []string{
		"QA stage: 0-web calls app-server (acme/app-server) through VITE_APP_URL — deploying an unchanged preview of it from main\n",
		"QA stage: 0-web's VITE_PAY_URL has no backend preview: the web service isn't built from a Dockerfile (payments)\n",
		"QA stage: 0-web's VITE_X_URL has no backend preview: matches more than one web service\n",
		"QA: the preview of app-server builds from main at " + sha[:7] + "\n",
		"QA round 1: deploying the backend preview of companion-0-acme/app-server (allowing its site previews' origins in ALLOWED_ORIGINS)\n",
		"QA round 2: QA built web's preview to call acme/app-server's preview (VITE_APP_URL) and allowed its origin on acme/app-server's preview (ALLOWED_ORIGINS).\n",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, logs.String())
		}
	}
	if strings.Count(logs.String(), "QA: the preview of app-server builds from") != 1 {
		t.Errorf("the source line isn't logged once:\n%s", logs.String())
	}
	log := logs.String()
	lookup := strings.Index(log, "QA stage: 0-web calls app-server")
	notDeployed := strings.Index(log, "QA stage: 0-web's VITE_PAY_URL")
	source := strings.Index(log, "QA: the preview of app-server builds from")
	deploy := strings.Index(log, "QA round 1: deploying the backend preview of companion-0-acme/app-server")
	if !(lookup < notDeployed && notDeployed < source && source < deploy) {
		t.Errorf("the lines are out of order:\n%s", log)
	}
	if strings.Contains(log, companionToken) {
		t.Errorf("the log carries the token:\n%s", log)
	}
}

// A site no link of the companion names lists it unconnected and is not
// reserved for it; a site linked to it is reserved even with no Task backend.
func TestQACompanionUnlinkedSite(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	c := appServer()
	c.Links[0].Frontend = "web"
	f := &fakeStackOps{sites: map[string]string{"0-web": "web", "1-docs": "docs"},
		companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{c}}}
	stage, _, specs, _ := companionStage(t, []string{"web", "docs"}, []string{"0-web", "1-docs"}, nil, f, origin)
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.companionArgs.frontends, []task_previews.CompanionFrontendV1{{Repository: "0-web", Frontend: "web"}, {Repository: "1-docs", Frontend: "docs"}}) {
		t.Errorf("frontends = %+v", f.companionArgs.frontends)
	}
	previews := qaPreviews(t, (*specs)[0])
	if len(previews) != 3 || !reflect.DeepEqual(previews[0].ConnectedBackends, []string{companionName()}) ||
		!reflect.DeepEqual(previews[1].UnconnectedBackends, []string{companionName()}) || !previews[2].Companion {
		t.Errorf("previews = %+v", previews)
	}
	for _, e := range f.events {
		if e == "reserve 1-docs" {
			t.Errorf("the unlinked site was reserved: %v", f.events)
		}
	}
	if !reflect.DeepEqual(f.events[:5], []string{"plan site 0-web", "plan site 1-docs", "companions", "plan companion " + companionName(), "reserve 0-web"}) {
		t.Errorf("events = %v", f.events)
	}
	want := []qaWiringOutput{
		{Frontend: "web", API: "acme/app-server", Connected: true, FrontendVariables: []string{"VITE_APP_URL"}, APIVariables: []string{"ALLOWED_ORIGINS"}, Companion: true},
		{Frontend: "docs", API: "acme/app-server", Reason: qaWiringNoLink, Companion: true},
	}
	if !reflect.DeepEqual(stage.roundWiring, want) {
		t.Errorf("wiring = %+v", stage.roundWiring)
	}
}

// --- when companions are used ----------------------------------------------------

// Below 1.9.48 nothing is looked up and QA_PREVIEWS, the round's records and the
// log are today's; a lookup that finds nothing adds nothing either.
func TestQACompanionsOnlyFrom1948(t *testing.T) {
	run := func(image string, reply task_previews.CompanionBackendsReplyV1) (*fakeStackOps, *qaStage, string, string) {
		f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
			links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), nil)}, companionReply: reply}
		stage, logs, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
		stage.mergeFetchSource = func(context.Context, tasks.RepositoryEntry) (string, string, error) {
			return "", "", errors.New("no fetch")
		}
		for round := 1; round <= 2; round++ {
			if _, err := runStackRoundWith(t, stage, round, image); err != nil {
				t.Fatal(err)
			}
		}
		return f, stage, envMap((*specs)[1].env)["QA_PREVIEWS"], logs.String()
	}
	f, before, previews, log := run("deploymenthq/agentbox:1.9.47", task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{appServer()}})
	if f.companionCalls != 0 || strings.Contains(previews, "companion") || strings.Contains(log, "companion") || strings.Contains(log, "app-server") ||
		before.roundCompanions != nil || before.roundBackendsNotDeployed != nil || before.backendDeployAllowance() != qaBackendDeployAllowance {
		t.Errorf("1.9.47: lookups %d, QA_PREVIEWS %s, log:\n%s", f.companionCalls, previews, log)
	}
	f, after, previewsEmpty, logEmpty := run(companionQAImage, task_previews.CompanionBackendsReplyV1{})
	if f.companionCalls != 1 || previewsEmpty != previews || logEmpty != log || after.roundCompanions != nil || after.roundBackendsNotDeployed != nil ||
		!reflect.DeepEqual(after.roundWiring, before.roundWiring) {
		t.Errorf("an empty reply changed something: lookups %d\nQA_PREVIEWS %s\nwant %s\nlog:\n%s\nwant:\n%s", f.companionCalls, previewsEmpty, previews, logEmpty, log)
	}
}

// Off the stack path (QA_REPOSITORIES) nothing is looked up.
func TestQACompanionsNotOffTheStackPath(t *testing.T) {
	f := &fakeStackOps{companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{appServer()}}}
	stage, _, specs := stackStage(t, []string{"web"}, []string{"0-web"}, nil, f)
	stage.stack = false
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if f.companionCalls != 0 || len(*specs) != 1 || envMap((*specs)[0].env)["QA_PREVIEWS"] != "" || stage.roundCompanions != nil {
		t.Errorf("lookups %d, events %v", f.companionCalls, f.events)
	}
}

// A round whose sites have no deployment (detected, or failed to plan) looks
// nothing up, and the stage doesn't look again in round 2.
func TestQACompanionsNeedAPlannedSiteWithADeployment(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": ""}, planErr: map[string]error{"site:1-docs": errors.New("no build settings")},
		companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{appServer()}}}
	stage, _, _ := stackStage(t, []string{"web", "docs"}, []string{"0-web", "1-docs"}, nil, f)
	f.sites["1-docs"] = "docs"
	for round := 1; round <= 2; round++ {
		_, _ = runCompanionRound(t, stage, round)
	}
	if f.companionCalls != 0 || len(stage.companions) != 0 {
		t.Errorf("lookups %d", f.companionCalls)
	}
}

// A failed lookup is logged once, leaves the stage with no companion and isn't
// retried in round 2.
func TestQACompanionLookupFailed(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), nil)}, companionErr: errors.New("connection refused")}
	stage, logs, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
	for round := 1; round <= 2; round++ {
		if _, err := runCompanionRound(t, stage, round); err != nil {
			t.Fatal(err)
		}
	}
	const line = "QA stage: couldn't look up the backends outside this Task that the site previews call: connection refused\n"
	if f.companionCalls != 1 || strings.Count(logs.String(), line) != 1 {
		t.Errorf("lookups %d, log:\n%s", f.companionCalls, logs.String())
	}
	if got := envMap((*specs)[1].env)["QA_PREVIEWS"]; strings.Contains(got, "companion") || strings.Contains(got, "backends_not_deployed") || stage.roundCompanions != nil {
		t.Errorf("QA_PREVIEWS = %s", got)
	}
	if stage.backendDeployAllowance() != qaBackendDeployAllowance {
		t.Error("the allowance changed")
	}
}

// A reply with more companions than a Task deploys keeps the first three, in its
// order, named by their position.
func TestQACompanionsAtMostThree(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	var companions []task_previews.CompanionBackendV1
	for i := 0; i < 4; i++ {
		c := appServer()
		c.API = fmt.Sprintf("svc-%d", i)
		c.Repository.Name = fmt.Sprintf("acme/svc-%d", i)
		c.Links = nil
		companions = append(companions, c)
	}
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, companionReply: task_previews.CompanionBackendsReplyV1{Companions: companions}}
	stage, logs, specs, _ := companionStage(t, []string{"web"}, []string{"0-web"}, nil, f, origin)
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range qaPreviews(t, (*specs)[0])[1:] {
		names = append(names, p.Repository)
	}
	if !reflect.DeepEqual(names, []string{"companion-0-acme/svc-0", "companion-1-acme/svc-1", "companion-2-acme/svc-2"}) || len(stage.roundCompanions) != 3 {
		t.Errorf("companions = %v", names)
	}
	if !strings.Contains(logs.String(), "QA stage: a Task deploys at most 3 companion backends: no preview of svc-3 (acme/svc-3)\n") {
		t.Errorf("log:\n%s", logs.String())
	}
	if stage.backendDeployAllowance() != qaBackendDeployAllowance {
		t.Error("a stage with companions and no Task backend has no backend deploy allowance")
	}
}

// --- the companion's source ----------------------------------------------------------

// Each way the source can fail fails the companion's deploy in every round with
// its reason, marks its pairs "backend failed" and never carries the token; the
// source is looked up at most once.
func TestQACompanionSourceFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
		source func(t *testing.T, origin *companionOrigin) func(context.Context, tasks.RepositoryEntry) (string, string, error)
		reason string
		commit bool
	}{
		{name: "bad branch", branch: "main..x", reason: `the branch "main..x" isn't a branch name`},
		{name: "token lookup", reason: "the repository's installation token couldn't be looked up",
			source: func(*testing.T, *companionOrigin) func(context.Context, tasks.RepositoryEntry) (string, string, error) {
				return func(context.Context, tasks.RepositoryEntry) (string, string, error) {
					return "", companionToken, errors.New("token " + companionToken + " was refused")
				}
			}},
		{name: "fetch", reason: "main couldn't be fetched",
			source: func(t *testing.T, _ *companionOrigin) func(context.Context, tasks.RepositoryEntry) (string, string, error) {
				missing := filepath.Join(t.TempDir(), "oauth2:"+companionToken, "missing.git")
				return func(context.Context, tasks.RepositoryEntry) (string, string, error) {
					return missing, companionToken, nil
				}
			}},
		{name: "submodule", reason: "the repository has submodules", commit: true,
			source: func(t *testing.T, origin *companionOrigin) func(context.Context, tasks.RepositoryEntry) (string, string, error) {
				sub := filepath.Join(origin.dir, "theme")
				writeFile(t, filepath.Join(sub, "theme.css"), "body {}\n")
				runGit(t, sub, "init", "-q")
				runGit(t, sub, "add", "-A")
				runGit(t, sub, "commit", "-q", "-m", "the theme")
				runGit(t, origin.dir, "add", "theme")
				runGit(t, origin.dir, "commit", "-q", "-m", "a submodule")
				return func(context.Context, tasks.RepositoryEntry) (string, string, error) {
					return origin.dir, companionToken, nil
				}
			}},
		{name: "timeout", reason: "timed out after 300ms",
			source: func(*testing.T, *companionOrigin) func(context.Context, tasks.RepositoryEntry) (string, string, error) {
				return func(ctx context.Context, _ tasks.RepositoryEntry) (string, string, error) {
					<-ctx.Done()
					return "", "", ctx.Err()
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
			c := appServer()
			if tc.branch != "" {
				c.Repository.BaseBranch = tc.branch
			}
			f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{c}}}
			stage, logs, specs, fetches := companionStage(t, []string{"web"}, []string{"0-web"}, nil, f, origin)
			stage.companionTimeout = 300 * time.Millisecond
			lookups := 0
			if tc.source != nil {
				source := tc.source(t, origin)
				stage.mergeFetchSource = func(ctx context.Context, e tasks.RepositoryEntry) (string, string, error) {
					lookups++
					return source(ctx, e)
				}
			}
			stage.out = &qaOutput{Repositories: stage.repositories}
			stage.runQA = func(round int) (agentResult, error) {
				env, err := stage.qaSpawnEnv()
				if err != nil {
					return agentResult{}, err
				}
				return stage.deployAndSpawnQA(round, companionQAImage, t.TempDir(), env)
			}
			for round := 1; round <= 2; round++ {
				if _, err := stage.qaRound(round); err != nil {
					t.Fatal(err)
				}
				previews := qaPreviews(t, (*specs)[round-1])
				if len(previews) != 2 || previews[1].Error != tc.reason || !previews[1].Companion || previews[1].URL != "" ||
					!reflect.DeepEqual(previews[0].UnconnectedBackends, []string{companionName()}) {
					t.Errorf("round %d: QA_PREVIEWS = %+v", round, previews)
				}
				if w := stage.roundWiring; len(w) != 1 || w[0].Reason != qaWiringBackendFailed || !w[0].Companion {
					t.Errorf("round %d: wiring = %+v", round, w)
				}
				if _, set := f.siteVars["0-web"]["VITE_APP_URL"]; set {
					t.Errorf("round %d: the site points at a failed companion", round)
				}
			}
			if _, err := stage.finish(); err != nil {
				t.Fatal(err)
			}
			if lookups > 1 || *fetches > 0 || len(f.built) != 0 {
				t.Errorf("token lookups %d, default lookups %d, builds %d", lookups, *fetches, len(f.built))
			}
			qa := readQAFromJobOutput(stage.parameters)
			for _, r := range qa.Rounds {
				got := r.Companions
				if len(got) != 1 || got[0].Deployed || got[0].Error != tc.reason || (got[0].Commit != "") == !tc.commit || got[0].ServiceName != "svc-"+companionName() {
					t.Errorf("round %d: companions = %+v", r.Round, got)
				}
			}
			if !strings.Contains(logs.String(), "QA: the preview of app-server can't be built: "+tc.reason+"\n") ||
				strings.Count(logs.String(), "QA: the preview of app-server can't be built") != 1 {
				t.Errorf("log:\n%s", logs.String())
			}
			if deploy := strings.Index(logs.String(), "QA round 1: deploying the backend preview of companion-0-acme/app-server"); deploy >= 0 &&
				deploy < strings.Index(logs.String(), "QA: the preview of app-server can't be built") {
				t.Errorf("the deploy line comes before the source line:\n%s", logs.String())
			}
			jobOutput, _ := jobs.GetParameterValue[string](stage.parameters, parameters_enums.JobOutput)
			for name, text := range map[string]string{"log": logs.String(), "job output": jobOutput, "QA_PREVIEWS": envMap((*specs)[0].env)["QA_PREVIEWS"]} {
				if strings.Contains(text, companionToken) {
					t.Errorf("the %s carries the token:\n%s", name, text)
				}
			}
			stage.removeCompanionSources()
			assertEmptyDir(t, stage.fixTreesTmpRoot)
		})
	}
}

// Every git command of a companion's source runs in its own temporary
// repository with no system, global or inherited configuration and no
// alternates; its temporary directory goes when the stage ends.
func TestQACompanionGitRunsIsolated(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", "/elsewhere/objects")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor=touch /tmp/ran'")
	envLog := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "git")
	writeFile(t, script, "#!/bin/sh\nenv | grep '^GIT_' | sort >> "+envLog+"\necho --- >> "+envLog+"\nexec git \"$@\"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{appServer()}}}
	stage, _, _, _ := companionStage(t, []string{"web"}, []string{"0-web"}, nil, f, origin)
	stage.fixTreesGit = script
	// A directory named like the companion under the work dir is never read.
	writeFile(t, filepath.Join(stage.workDirHost, "companion-0-acme", "app-server", "planted"), "x\n")
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.built) != 1 || f.built[0]["Dockerfile"] != "FROM scratch\n" || f.built[0]["planted"] != "" {
		t.Fatalf("built = %v", f.built)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(strings.TrimSuffix(string(data), "---\n"), "---\n")
	if len(blocks) < 5 {
		t.Fatalf("%d git commands", len(blocks))
	}
	gitDir := filepath.Join(stage.companions[0].tmp, "source.git")
	for _, block := range blocks {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		for _, want := range []string{"GIT_DIR=" + gitDir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"} {
			if !containsLine(block, want) {
				t.Errorf("a git command ran without %s:\n%s", want, block)
			}
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") || strings.HasPrefix(line, "GIT_CONFIG_PARAMETERS=") || strings.HasPrefix(line, "GIT_OBJECT_DIRECTORY=") {
				t.Errorf("a git command ran with %s", line)
			}
		}
	}
	if !strings.HasPrefix(filepath.Base(stage.companions[0].tmp), "qa-companion-") || filepath.Dir(stage.companions[0].tmp) != stage.fixTreesTmpRoot {
		t.Errorf("temporary directory %s", stage.companions[0].tmp)
	}
	stage.removeCompanionSources()
	assertEmptyDir(t, stage.fixTreesTmpRoot)
}

// --- failures and the StackWiringV1 lookup ---------------------------------------------

// A failed StackWiringV1 lookup marks the Task's pairs "lookup failed" and
// leaves the companion's connected; a failed companion deploy marks its own
// "backend failed".
func TestQACompanionPairsAreTheirOwn(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), nil)}, wiringErr: errors.New("connection refused"),
		companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{appServer()}}}
	stage, _, _, _ := companionStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f, origin)
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	want := []qaWiringOutput{
		{Frontend: "web", API: "api", Reason: qaWiringLookupFailed},
		{Frontend: "web", API: "acme/app-server", Connected: true, FrontendVariables: []string{"VITE_APP_URL"}, APIVariables: []string{"ALLOWED_ORIGINS"}, Companion: true},
	}
	if !reflect.DeepEqual(stage.roundWiring, want) {
		t.Errorf("wiring = %+v", stage.roundWiring)
	}

	f.deployErr = map[string]error{"backend:" + companionName(): errors.New("the image build failed\nstep 3")}
	if _, err := runCompanionRound(t, stage, 2); err != nil {
		t.Fatal(err)
	}
	if w := stage.roundWiring[1]; w.Connected || w.Reason != qaWiringBackendFailed || !w.Companion {
		t.Errorf("wiring = %+v", stage.roundWiring)
	}
	if got := stage.roundCompanions; len(got) != 1 || got[0].Deployed || got[0].Error != "the image build failed" || got[0].Commit == "" {
		t.Errorf("companions = %+v", got)
	}
	if _, set := f.siteVars["0-web"]["VITE_APP_URL"]; set {
		t.Errorf("site variables = %v", f.siteVars["0-web"])
	}
}

// A companion that can't be planned is a failed backend entry with no service
// name, its pairs "backend failed"; its source is never fetched.
func TestQACompanionPlanFailed(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"},
		planErr:        map[string]error{"companion:" + companionName(): errors.New("deployment.io no longer deploys app-server from main as a web service")},
		companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{appServer()}}}
	stage, logs, specs, fetches := companionStage(t, []string{"web"}, []string{"0-web"}, nil, f, origin)
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	previews := qaPreviews(t, (*specs)[0])
	if previews[1].Error != "deployment.io no longer deploys app-server from main as a web service" || previews[1].Deployment != "app-server" {
		t.Errorf("previews = %+v", previews)
	}
	if got := stage.roundCompanions; len(got) != 1 || got[0].ServiceName != "" || got[0].Commit != "" || got[0].Deployed {
		t.Errorf("companions = %+v", got)
	}
	if w := stage.roundWiring; len(w) != 1 || w[0].Reason != qaWiringBackendFailed {
		t.Errorf("wiring = %+v", w)
	}
	if *fetches != 0 || !strings.Contains(logs.String(), "QA round 1: the backend preview of companion-0-acme/app-server failed to deploy: deployment.io no longer deploys") {
		t.Errorf("fetches %d, log:\n%s", *fetches, logs.String())
	}
}

// --- what QA_PREVIEWS carries ------------------------------------------------------------

// What agentbox would refuse is made to pass: a long deployment name and reason
// are cut, control characters removed, a site keeps 20 items, and a variable
// with a space or repeated is left out. The Job output keeps every item.
// backends_not_deployed is only on a deployed site with the item's deployment.
func TestQACompanionPreviewsPassAgentboxsRules(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	c := appServer()
	c.API = strings.Repeat("a", 199) + "\x07bc"
	var items []task_previews.CompanionNotDeployedV1
	item := func(repo, frontend, variable, api, reason string) {
		items = append(items, task_previews.CompanionNotDeployedV1{FrontendRepository: repo, Frontend: frontend, Variable: variable, API: api, Reason: reason})
	}
	item("0-web", "web", "VITE_SPACE URL", "", "x")
	item("0-web", "web", "VITE_A", "pay\nments", "first line\nsecond line")
	item("0-web", "web", "VITE_A", "", "repeated")
	item("0-web", "web", "VITE_LONG", strings.Repeat("p", 250), strings.Repeat("r", 350))
	for i := 0; i < 20; i++ {
		item("0-web", "web", fmt.Sprintf("VITE_%02d", i), "", task_previews.ReasonLimit)
	}
	item("0-web", "other-deployment", "VITE_OTHER", "", "x")
	item("1-docs", "docs", "VITE_DOCS", "", "x")
	f := &fakeStackOps{sites: map[string]string{"0-web": "web", "1-docs": "docs"},
		deployErr:      map[string]error{"site:1-docs": errors.New("build failed")},
		companionReply: task_previews.CompanionBackendsReplyV1{Companions: []task_previews.CompanionBackendV1{c}, NotDeployed: items}}
	stage, _, specs, _ := companionStage(t, []string{"web", "docs"}, []string{"0-web", "1-docs"}, nil, f, origin)
	if _, err := runCompanionRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	previews := qaPreviews(t, (*specs)[0])
	site := previews[0]
	if len(site.BackendsNotDeployed) != 20 {
		t.Fatalf("items = %d: %+v", len(site.BackendsNotDeployed), site.BackendsNotDeployed)
	}
	if got := site.BackendsNotDeployed[0]; got != (qaPreviewNotDeployed{Variable: "VITE_A", API: "payments", Reason: "first linesecond line"}) {
		t.Errorf("item 0 = %+v", got)
	}
	if got := site.BackendsNotDeployed[1]; got.Variable != "VITE_LONG" || len(got.API) != 200 || len(got.Reason) != 300 {
		t.Errorf("item 1 = %+v", got)
	}
	if got := site.BackendsNotDeployed[19]; got.Variable != "VITE_17" {
		t.Errorf("item 19 = %+v", got)
	}
	if previews[1].Error == "" || previews[1].BackendsNotDeployed != nil {
		t.Errorf("failed site = %+v", previews[1])
	}
	if d := previews[2].Deployment; d != strings.Repeat("a", 199)+"b" {
		t.Errorf("deployment = %q", d)
	}
	if len(stage.roundBackendsNotDeployed) != len(items) || stage.roundBackendsNotDeployed[1].Reason != "first line\nsecond line" ||
		stage.roundBackendsNotDeployed[len(items)-1].Frontend != "docs" || stage.roundCompanions[0].API != c.API {
		t.Errorf("job output items = %d", len(stage.roundBackendsNotDeployed))
	}
	// Encoded as the runner sends it.
	encoded, _ := json.Marshal(previews)
	if strings.Contains(string(encoded), `\n`) || strings.Contains(string(encoded), `\u0007`) {
		t.Errorf("QA_PREVIEWS carries a control character: %s", encoded)
	}
}

// The Job output's companion records carry names, branches and SHAs only.
func TestQACompanionOutputCarriesNamesOnly(t *testing.T) {
	origin := newCompanionOrigin(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, companionReply: task_previews.CompanionBackendsReplyV1{
		Companions:  []task_previews.CompanionBackendV1{appServer()},
		NotDeployed: []task_previews.CompanionNotDeployedV1{{FrontendRepository: "0-web", Frontend: "web", Variable: "VITE_PAY_URL", API: "payments", Reason: task_previews.ReasonOtherRegion}},
	}}
	stage, _, _, _ := companionStage(t, []string{"web"}, []string{"0-web"}, nil, f, origin)
	stage.out = &qaOutput{Repositories: stage.repositories}
	stage.runQA = func(round int) (agentResult, error) {
		env, err := stage.qaSpawnEnv()
		if err != nil {
			return agentResult{}, err
		}
		return stage.deployAndSpawnQA(round, companionQAImage, t.TempDir(), env)
	}
	if _, err := stage.qaRound(1); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.finish(); err != nil {
		t.Fatal(err)
	}
	var data struct {
		QA struct {
			Rounds []json.RawMessage `json:"rounds"`
		} `json:"qa"`
	}
	jobOutput, _ := jobs.GetParameterValue[string](stage.parameters, parameters_enums.JobOutput)
	if err := json.Unmarshal([]byte(jobOutput), &data); err != nil || len(data.QA.Rounds) != 1 {
		t.Fatalf("job output %s: %v", jobOutput, err)
	}
	var round struct {
		Companions          []map[string]interface{} `json:"companions"`
		BackendsNotDeployed []map[string]interface{} `json:"backends_not_deployed"`
		Wiring              []map[string]interface{} `json:"wiring"`
	}
	if err := json.Unmarshal(data.QA.Rounds[0], &round); err != nil {
		t.Fatal(err)
	}
	sha := origin.head(t)
	wantCompanion := map[string]interface{}{"api": "app-server", "repository": "acme/app-server", "branch": "main", "commit": sha, "service_name": "svc-" + companionName(), "deployed": true}
	wantItem := map[string]interface{}{"frontend": "web", "variable": "VITE_PAY_URL", "api": "payments", "reason": task_previews.ReasonOtherRegion}
	if len(round.Companions) != 1 || !reflect.DeepEqual(round.Companions[0], wantCompanion) ||
		len(round.BackendsNotDeployed) != 1 || !reflect.DeepEqual(round.BackendsNotDeployed[0], wantItem) ||
		len(round.Wiring) != 1 || round.Wiring[0]["companion"] != true || round.Wiring[0]["api"] != "acme/app-server" {
		t.Errorf("round = %s", data.QA.Rounds[0])
	}
	for _, s := range []string{"https://", "cloudfront.net", "github.com", origin.dir, companionToken} {
		if strings.Contains(string(data.QA.Rounds[0]), s) {
			t.Errorf("the round carries %q: %s", s, data.QA.Rounds[0])
		}
	}
}

// The budget counts a stage with a companion and no Task backend.
func TestQACompanionBackendDeployAllowance(t *testing.T) {
	stage := &qaStage{}
	if stage.backendDeployAllowance() != 0 {
		t.Error("an empty stage has an allowance")
	}
	stage.companions = []*qaCompanion{{name: companionName()}}
	if stage.backendDeployAllowance() != qaBackendDeployAllowance {
		t.Error("a stage with a companion has no allowance")
	}
}
