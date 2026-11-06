package commands

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

var (
	signInTestRoles = []task_previews.PreviewRoleV1{{Name: "admin", Description: "manages the shop"}, {Name: "viewer"}}
	signInTestSetup = &task_previews.PreviewSignInV1{Path: "/api/login", BodyFormat: "json", EmailField: "email", PasswordField: "password", Session: "cookie"}
)

// seeded is a backend deploy whose seed ended with outcome, with roles and
// signIn.
func seeded(outcome agenttools.PreviewSeedOutcome, roles []task_previews.PreviewRoleV1, signIn *task_previews.PreviewSignInV1) agenttools.WebServicePreviewDeployed {
	return agenttools.WebServicePreviewDeployed{PreviewID: "preview-0123456789", Seed: outcome, Roles: roles, SignIn: signIn}
}

// A backend's QA_PREVIEWS entry has its roles only after a seed that exited 0
// with roles and a sign-in request; QA_API_SIGN_IN_TOOL=1 and
// verify_api_sign_in come with them, beside the API tool.
func TestQAStackRoundRolesAndTheSignInTool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seed      agenttools.WebServicePreviewDeployed
		deployErr error
		want      bool
	}{
		{name: "a seed that exited 0", seed: seeded(agenttools.PreviewSeedSucceeded, signInTestRoles, signInTestSetup), want: true},
		{name: "no seed command", seed: seeded(agenttools.PreviewSeedNotRun, signInTestRoles, signInTestSetup)},
		{name: "a failed seed", seed: seeded(agenttools.PreviewSeedFailed, signInTestRoles, signInTestSetup)},
		{name: "no roles", seed: seeded(agenttools.PreviewSeedSucceeded, nil, signInTestSetup)},
		{name: "no sign-in request", seed: seeded(agenttools.PreviewSeedSucceeded, signInTestRoles, nil)},
		{name: "a failed deploy", seed: seeded(agenttools.PreviewSeedSucceeded, signInTestRoles, signInTestSetup), deployErr: errors.New("the service didn't start")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
				seeds: map[string]agenttools.WebServicePreviewDeployed{"1-api": tc.seed}}
			if tc.deployErr != nil {
				f.deployErr = map[string]error{"backend:1-api": tc.deployErr}
			}
			stage, logs, specs := stackStage(t, []string{"web", "api"}, []string{"0-web"}, []string{"1-api"}, f)
			if _, err := runStackRound(t, stage, 1); err != nil {
				t.Fatal(err)
			}
			spec := (*specs)[0]
			raw := envMap(spec.env)["QA_PREVIEWS"]
			var backend *qaPreview
			for _, p := range qaPreviews(t, spec) {
				if p.Kind == qaPreviewBackend {
					p := p
					backend = &p
				}
			}
			if backend == nil {
				t.Fatalf("no backend entry in %s", raw)
			}
			env := envMap(spec.env)
			_, set := env["QA_API_SIGN_IN_TOOL"]
			tools := toolNames(spec)
			if tc.want {
				want := []qaPreviewRole{{Name: "admin", Description: "manages the shop"}, {Name: "viewer", Description: ""}}
				if len(backend.Roles) != 2 || backend.Roles[0] != want[0] || backend.Roles[1] != want[1] {
					t.Errorf("roles = %+v", backend.Roles)
				}
				if !strings.Contains(raw, `"roles":[{"name":"admin","description":"manages the shop"},{"name":"viewer","description":""}]`) {
					t.Errorf("QA_PREVIEWS = %s", raw)
				}
				if env["QA_API_SIGN_IN_TOOL"] != "1" || !spec.apiSignInTool ||
					tools != "ping,verify_preview_reachable,verify_preview_rendered,verify_api_request,verify_api_sign_in" {
					t.Errorf("tools = %s, QA_API_SIGN_IN_TOOL = %q", tools, env["QA_API_SIGN_IN_TOOL"])
				}
				if len(spec.apiSignIns) != 1 || len(spec.apiSignIns[0].Users) != 2 || spec.apiSignIns[0].URL != stackHost("b", "1-api") {
					t.Errorf("sign-ins = %+v", spec.apiSignIns)
				}
			} else {
				if strings.Contains(raw, `"roles"`) {
					t.Errorf("QA_PREVIEWS = %s, want no roles", raw)
				}
				if set || spec.apiSignInTool || strings.Contains(tools, "verify_api_sign_in") {
					t.Errorf("tools = %s, QA_API_SIGN_IN_TOOL set = %v", tools, set)
				}
			}
			// Never an email or a password, in QA_PREVIEWS, the env or the log.
			for _, user := range stage.testUsers.Users("preview-0123456789") {
				for _, secret := range []string{user.Email, user.Password} {
					if strings.Contains(strings.Join(spec.env, "\n"), secret) || strings.Contains(logs.String(), secret) {
						t.Errorf("%q reached the env or the log", secret)
					}
				}
			}
		})
	}
}

// Production QA, implement runs and the older QA_BACKENDS path never serve it.
func TestOnlyAStackRoundServesTheSignInTool(t *testing.T) {
	workDir := t.TempDir()
	tools := buildImplementPreviewTools(commandUtils.TaskJobContext{OrganizationID: "o", TaskID: "t"}, webPreviewJobParameters(t), workDir, io.Discard)
	implement := implementSpawnSpec("img", workDir, "cache", nil, tools)
	implement.apiRequestTool, implement.apiSignInTool = true, true
	production := agentboxSpawnSpec{productionTools: &productionQATools{record: productionQARecord(productionTestTargets)}}
	for name, spec := range map[string]agentboxSpawnSpec{"implement": implement, "production QA": production} {
		if got := toolNames(spec); strings.Contains(got, "verify_api_sign_in") {
			t.Errorf("%s tools = %s", name, got)
		}
		if _, ok := envMap(spec.env)["QA_API_SIGN_IN_TOOL"]; ok {
			t.Errorf("%s sets QA_API_SIGN_IN_TOOL", name)
		}
	}
}
