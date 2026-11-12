package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// browserSeeded is a backend deploy whose seed exited 0, with roles, a browser
// sign-in page and no API sign-in request.
func browserSeeded(previewID string, roles []task_previews.PreviewRoleV1, path string) agenttools.WebServicePreviewDeployed {
	return agenttools.WebServicePreviewDeployed{PreviewID: previewID, Seed: agenttools.PreviewSeedSucceeded, Roles: roles, BrowserSignInPath: path}
}

// browserLink links the site 0-web to the backend repo (named name).
func browserLink(repo, name string) task_previews.StackLinkV1 {
	return task_previews.StackLinkV1{FrontendRepository: "0-web", Frontend: "web", APIRepository: repo, API: name, APIURLVariables: urlVar("VITE_"+strings.ToUpper(name)+"_URL", "")}
}

// browserStackRound runs one round of the site 0-web with the backends 1-api
// and 2-auth, through f, and returns its spec and QA_PREVIEWS.
func browserStackRound(t *testing.T, f *fakeStackOps) (*qaStage, agentboxSpawnSpec, string, *lockedBuffer) {
	t.Helper()
	f.sites, f.backends = map[string]string{"0-web": "web"}, map[string]string{"1-api": "api", "2-auth": "auth"}
	stage, logs, specs := stackStage(t, []string{"web", "api", "auth"}, []string{"0-web"}, []string{"1-api", "2-auth"}, f)
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	spec := (*specs)[0]
	return stage, spec, envMap(spec.env)["QA_PREVIEWS"], logs
}

// errBrowserReached is the fake browser's answer: the check got past as.
var errBrowserReached = errors.New("the browser was reached")

// callRenderedAs calls the spec's verify_preview_rendered with as on url, with
// a browser that records input.json and answers errBrowserReached.
func callRenderedAs(t *testing.T, spec agentboxSpawnSpec, url, as string) (map[string]any, error) {
	t.Helper()
	deps := *spec.previewDeps
	var input map[string]any
	deps.VerifyBrowser = func(_ context.Context, req agenttools.VerifyBrowserRequest, _ io.Writer) (agenttools.VerifyBrowserOutcome, error) {
		_ = json.Unmarshal(req.Input, &input)
		return agenttools.VerifyBrowserOutcome{}, errBrowserReached
	}
	spec.previewDeps = &deps
	srv := agentmcp.New("test", "0")
	tools := registerAgentTools(srv, spec)
	defer tools.stop()
	tool, ok := srv.Lookup("verify_preview_rendered")
	if !ok {
		t.Fatal("no verify_preview_rendered")
	}
	args, _ := json.Marshal(map[string]string{"url": url, "as": as})
	_, err := tool.Handler(context.Background(), args)
	return input, err
}

// A connected backend with a seed that exited 0, roles and a browser sign-in
// page gives the site its roles, in order and with the backend, and the round
// its set; one without an API sign-in request gets no backend roles and no
// verify_api_sign_in.
func TestQAStackRoundBrowserRoles(t *testing.T) {
	f := &fakeStackOps{
		links: []task_previews.StackLinkV1{browserLink("1-api", "api"), browserLink("2-auth", "auth")},
		seeds: map[string]agenttools.WebServicePreviewDeployed{
			"1-api":  browserSeeded("preview-api-00000001", signInTestRoles, " /signin "),
			"2-auth": browserSeeded("preview-auth-0000002", []task_previews.PreviewRoleV1{{Name: "admin", Description: "runs auth"}}, "/login"),
		},
	}
	stage, spec, raw, logs := browserStackRound(t, f)
	const wantSite = `{"repository":"0-web","kind":"site","url":"https://s-0-web.cloudfront.net","connected_backends":["1-api","2-auth"],` +
		`"browser_roles":[{"name":"admin","description":"manages the shop","backend":"1-api"},{"name":"viewer","description":"","backend":"1-api"},{"name":"admin","description":"runs auth","backend":"2-auth"}]}`
	if !strings.Contains(raw, wantSite) {
		t.Errorf("QA_PREVIEWS = %s\nwant the site %s", raw, wantSite)
	}
	if strings.Contains(raw, `"roles"`) {
		t.Errorf("QA_PREVIEWS = %s, want no backend roles", raw)
	}
	env := envMap(spec.env)
	if _, set := env["QA_API_SIGN_IN_TOOL"]; set || spec.apiSignInTool || strings.Contains(toolNames(spec), "verify_api_sign_in") {
		t.Errorf("tools = %s, env %v", toolNames(spec), env)
	}
	sites := spec.browserSignIns
	if len(sites) != 1 || sites[0].URL != stackHost("s", "0-web") || len(sites[0].Backends) != 2 {
		t.Fatalf("set = %+v", sites)
	}
	api, auth := sites[0].Backends[0], sites[0].Backends[1]
	if api.Repository != "1-api" || api.SignInPath != "/signin" || len(api.Users) != 2 || api.Users[0].Role != "admin" || api.Users[1].Role != "viewer" ||
		auth.Repository != "2-auth" || auth.SignInPath != "/login" || len(auth.Users) != 1 {
		t.Errorf("set = %+v", sites)
	}
	if len(spec.apiPasswords) == 0 {
		t.Error("no passwords to redact")
	}
	// Its verify_preview_rendered takes as: the viewer's test user signs in on
	// the site's host.
	input, err := callRenderedAs(t, spec, stackHost("s", "0-web")+"/", "viewer")
	if err == nil || !strings.Contains(err.Error(), errBrowserReached.Error()) {
		t.Errorf("as viewer: %v", err)
	}
	viewer := api.Users[1]
	signIn, _ := input["sign_in"].(map[string]any)
	if signIn["url"] != stackHost("s", "0-web")+"/signin" || signIn["email"] != viewer.Email || signIn["password"] != viewer.Password {
		t.Errorf("sign_in = %v", signIn)
	}
	if _, err := callRenderedAs(t, spec, stackHost("s", "0-web")+"/", "admin"); err == nil || !strings.Contains(err.Error(), "role admin is a role of more than one backend this site preview calls (1-api, 2-auth)") {
		t.Errorf("as admin: %v", err)
	}
	// Never an email or a password, in QA_PREVIEWS, the env or the log.
	for _, id := range []string{"preview-api-00000001", "preview-auth-0000002"} {
		for _, user := range stage.testUsers.Users(id) {
			for _, secret := range []string{user.Email, user.Password} {
				if strings.Contains(strings.Join(spec.env, "\n"), secret) || strings.Contains(logs.String(), secret) {
					t.Errorf("%q reached the env or the log", secret)
				}
			}
		}
	}
}

// A backend with both an API sign-in request and a browser sign-in page has its
// roles on its own entry, as today, and on the site's browser_roles.
func TestQAStackRoundBrowserAndAPIRoles(t *testing.T) {
	d := seeded(agenttools.PreviewSeedSucceeded, signInTestRoles, signInTestSetup)
	d.BrowserSignInPath = "/signin"
	f := &fakeStackOps{links: []task_previews.StackLinkV1{browserLink("1-api", "api")}, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": d}}
	_, spec, raw, _ := browserStackRound(t, f)
	for _, want := range []string{
		`"browser_roles":[{"name":"admin","description":"manages the shop","backend":"1-api"},{"name":"viewer","description":"","backend":"1-api"}]`,
		`{"repository":"1-api","kind":"backend","url":"https://b-1-api.cloudfront.net","health_url":"https://b-1-api.cloudfront.net/healthz","roles":[{"name":"admin","description":"manages the shop"},{"name":"viewer","description":""}]}`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("QA_PREVIEWS = %s\nlacks %s", raw, want)
		}
	}
	if envMap(spec.env)["QA_API_SIGN_IN_TOOL"] != "1" || len(spec.apiSignIns) != 1 || len(spec.browserSignIns) != 1 {
		t.Errorf("api sign-ins %d, browser sign-ins %d, env %v", len(spec.apiSignIns), len(spec.browserSignIns), envMap(spec.env))
	}
}

// No browser roles and no set when the backend has no sign-in page, its seed
// failed, it has no roles, the pair isn't connected, or the backend or the site
// failed — and verify_preview_rendered then refuses as.
func TestQAStackRoundNoBrowserRoles(t *testing.T) {
	ok := browserSeeded("preview-api-00000001", signInTestRoles, "/signin")
	failedSeed := ok
	failedSeed.Seed = agenttools.PreviewSeedFailed
	links := []task_previews.StackLinkV1{browserLink("1-api", "api")}
	for _, tc := range []struct {
		name  string
		f     *fakeStackOps
		noURL bool
	}{
		{name: "no sign-in page", f: &fakeStackOps{links: links, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": browserSeeded("preview-api-00000001", signInTestRoles, "  ")}}},
		{name: "a failed seed", f: &fakeStackOps{links: links, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": failedSeed}}},
		{name: "no roles", f: &fakeStackOps{links: links, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": browserSeeded("preview-api-00000001", nil, "/signin")}}},
		{name: "not connected", f: &fakeStackOps{links: []task_previews.StackLinkV1{browserLink("2-auth", "auth")}, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": ok}}},
		{name: "a failed backend", f: &fakeStackOps{links: links, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": ok}, deployErr: map[string]error{"backend:1-api": errors.New("the service didn't start")}}},
		{name: "a failed site", noURL: true, f: &fakeStackOps{links: links, seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": ok}, deployErr: map[string]error{"site:0-web": errors.New("the build failed")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, spec, raw, _ := browserStackRound(t, tc.f)
			if strings.Contains(raw, "browser_roles") || len(spec.browserSignIns) != 0 {
				t.Errorf("QA_PREVIEWS = %s, set = %+v", raw, spec.browserSignIns)
			}
			if tc.noURL {
				return
			}
			if _, err := callRenderedAs(t, spec, stackHost("s", "0-web")+"/", "admin"); err == nil || !strings.Contains(err.Error(), "as works only on a site preview that lists browser sign-in roles") {
				t.Errorf("as: %v", err)
			}
		})
	}
}

// Only a stack-path QA round's verify_preview_rendered takes as: an implement
// run's never does, even with a set on its spec.
func TestOnlyAStackRoundTakesAs(t *testing.T) {
	workDir := t.TempDir()
	tools := buildImplementPreviewTools(commandUtils.TaskJobContext{OrganizationID: "o", TaskID: "t"}, webPreviewJobParameters(t), workDir, io.Discard)
	implement := implementSpawnSpec("img", workDir, "cache", nil, tools)
	implement.browserSignIns = []agenttools.BrowserSignInSite{{URL: "https://s-0-web.cloudfront.net", Backends: []agenttools.BrowserSignInBackend{
		{Repository: "1-api", SignInPath: "/signin", Users: []agenttools.PreviewTestUser{{Role: "admin", Email: "qa-admin@example.com", Password: "p"}}},
	}}}
	if _, err := callRenderedAs(t, implement, "https://s-0-web.cloudfront.net/", "admin"); err == nil || !strings.Contains(err.Error(), "as works only on a site preview that lists browser sign-in roles") {
		t.Errorf("implement run: %v", err)
	}
}
