package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

var testPreviewRoles = []task_previews.PreviewRoleV1{{Name: "owner"}, {Name: "billing-admin", Description: "pays"}}

func TestPreviewTestUsersSameWithinAHolder(t *testing.T) {
	users := NewPreviewTestUsers()
	first, err := users.For("64f0c0ffee0123456789abcd", testPreviewRoles)
	if err != nil {
		t.Fatal(err)
	}
	again, err := users.For("64f0c0ffee0123456789abcd", testPreviewRoles[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Role != "owner" || first[1].Role != "billing-admin" {
		t.Fatalf("users = %+v, want one per role in their order", first)
	}
	if again[0] != first[0] {
		t.Errorf("a later deploy of the preview got %+v, want %+v", again[0], first[0])
	}
	if first[0].Email != "qa-owner-6789abcd@example.com" ||
		first[1].Email != "qa-billing-admin-6789abcd@example.com" {
		t.Errorf("emails = %q, %q", first[0].Email, first[1].Email)
	}
	if first[0].Password == first[1].Password {
		t.Error("two roles share a password")
	}
	other, err := users.For("64f0c0ffee0123456789ffff", testPreviewRoles[:1])
	if err != nil {
		t.Fatal(err)
	}
	if other[0].Email != "qa-owner-6789ffff@example.com" || other[0].Password == first[0].Password {
		t.Errorf("another preview's owner = %+v", other[0])
	}
	if got := users.Users("64f0c0ffee0123456789abcd"); len(got) != 2 || got[0].Role != "billing-admin" || got[1] != first[0] {
		t.Errorf("Users = %+v", got)
	}
}

func TestPreviewTestUsersNewPasswordsInAnotherHolder(t *testing.T) {
	a, err := NewPreviewTestUsers().For("64f0c0ffee0123456789abcd", testPreviewRoles)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPreviewTestUsers().For("64f0c0ffee0123456789abcd", testPreviewRoles)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		if a[i].Email != b[i].Email {
			t.Errorf("%s: emails %q and %q differ between jobs", a[i].Role, a[i].Email, b[i].Email)
		}
		if a[i].Password == b[i].Password {
			t.Errorf("%s: a later job reused the password", a[i].Role)
		}
	}
}

func TestPreviewTestUserPassword(t *testing.T) {
	seen := map[byte]bool{}
	for i := 0; i < 200; i++ {
		password, err := previewTestUserPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(password) != 24 {
			t.Fatalf("password %q has %d characters, want 24", password, len(password))
		}
		for j := 0; j < len(password); j++ {
			c := password[j]
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				t.Fatalf("password %q has %q, outside [A-Za-z0-9]", password, c)
			}
			seen[c] = true
		}
	}
	if len(seen) != 62 {
		t.Errorf("200 passwords used %d of the 62 characters", len(seen))
	}
}

func TestPreviewRoleEnvKey(t *testing.T) {
	for role, want := range map[string]string{
		"owner":         "OWNER",
		"billing-admin": "BILLING_ADMIN",
		"read_only2":    "READ_ONLY2",
		"a.b c":         "A_B_C",
	} {
		if got := PreviewRoleEnvKey(role); got != want {
			t.Errorf("PreviewRoleEnvKey(%q) = %q, want %q", role, got, want)
		}
	}
}

func TestPlanPreviewSeed(t *testing.T) {
	service := webService("api", "us-east-1", "")
	service.PreviewRoles = testPreviewRoles
	if seed, err := planPreviewSeed(DeployWebServicePreviewDeps{}, "64f0c0ffee0123456789abcd", service); seed != nil || err != nil {
		t.Errorf("no seed command: %+v, %v; want none", seed, err)
	}
	service.PreviewSeedCommand = "npm run seed"
	holder := NewPreviewTestUsers()
	seed, err := planPreviewSeed(DeployWebServicePreviewDeps{TestUsers: holder}, "64f0c0ffee0123456789abcd", service)
	if err != nil || seed == nil || seed.Command != "npm run seed" || len(seed.Users) != 2 {
		t.Fatalf("seed = %+v, %v", seed, err)
	}
	if got := holder.Users("64f0c0ffee0123456789abcd"); len(got) != 2 {
		t.Errorf("the deps' holder has %+v; want the seed's users", got)
	}
}

func TestPreviewTestUsersSummary(t *testing.T) {
	roles := []task_previews.PreviewRoleV1{{Name: "owner"}, {Name: "viewer"}}
	for _, tt := range []struct {
		name    string
		command string
		roles   []task_previews.PreviewRoleV1
		outcome PreviewSeedOutcome
		want    string
	}{
		{"seeded", "npm run seed", roles, PreviewSeedSucceeded, "seeded owner, viewer"},
		{"no roles", "npm run seed", nil, PreviewSeedSucceeded, "ran the preview seed command (no roles)"},
		{"failed", "npm run seed", roles, PreviewSeedFailed, "the preview seed command failed, so there are no test users"},
		{"failed, no roles", "npm run seed", nil, PreviewSeedFailed, "the preview seed command failed, so there are no test users"},
		{"roles, no command", "", roles, PreviewSeedNotRun, "roles are set but there's no preview seed command, so there are no test users"},
		{"neither", "", nil, PreviewSeedNotRun, ""},
	} {
		service := webService("api", "us-east-1", "")
		service.PreviewSeedCommand, service.PreviewRoles = tt.command, tt.roles
		if got := previewTestUsersSummary(service, tt.outcome); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// The deploy gets the seed and its users; the Step log and the tool result get
// the summary, never an email or a password.
func TestDeployWebServicePreviewReportsTestUsers(t *testing.T) {
	roles := []task_previews.PreviewRoleV1{{Name: "owner"}, {Name: "viewer"}}
	for _, tt := range []struct {
		name    string
		command string
		roles   []task_previews.PreviewRoleV1
		outcome PreviewSeedOutcome
		want    string
	}{
		{"seeded", "npm run seed", roles, PreviewSeedSucceeded, "seeded owner, viewer"},
		{"no roles", "npm run seed", nil, PreviewSeedSucceeded, "ran the preview seed command (no roles)"},
		{"failed", "npm run seed", roles, PreviewSeedFailed, "the preview seed command failed, so there are no test users"},
		{"roles, no command", "", roles, PreviewSeedNotRun, "roles are set but there's no preview seed command, so there are no test users"},
		{"neither", "", nil, PreviewSeedNotRun, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got WebServicePreviewDeployInput
			deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
				got = in
				return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com", Seed: tt.outcome}, nil
			})
			service := webService("api", "us-east-1", "services/api")
			service.PreviewSeedCommand, service.PreviewRoles = tt.command, tt.roles
			deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) { return readyReply(service), nil }
			var stepLog bytes.Buffer
			deps.LogsWriter = &stepLog
			deps.TestUsers = NewPreviewTestUsers()
			builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
			defer builds.stop()
			out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
			if err != nil {
				t.Fatal(err)
			}
			if tt.command == "" {
				if got.Seed != nil {
					t.Errorf("no seed command, yet the deploy got a seed: %+v", got.Seed)
				}
			} else if got.Seed == nil || got.Seed.Command != tt.command || len(got.Seed.Users) != len(tt.roles) {
				t.Errorf("deploy seed = %+v", got.Seed)
			}
			var res map[string]any
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatal(err)
			}
			line := "Preview test users: " + tt.want + "\n"
			if tt.want == "" {
				if _, ok := res["test_users"]; ok || strings.Contains(stepLog.String(), "Preview test users") {
					t.Errorf("neither set: test_users = %v, log:\n%s", res["test_users"], stepLog.String())
				}
			} else if res["test_users"] != tt.want || strings.Count(stepLog.String(), line) != 1 {
				t.Errorf("test_users = %v, log:\n%s\nwant %q", res["test_users"], stepLog.String(), tt.want)
			}
			if got.Seed != nil {
				for _, u := range got.Seed.Users {
					for _, secret := range []string{u.Email, u.Password} {
						if strings.Contains(out, secret) || strings.Contains(stepLog.String(), secret) {
							t.Errorf("%q reached the tool result or the Step log", secret)
						}
					}
				}
			}
		})
	}
}

// A deploy that fails reports as before, without the test users.
func TestDeployWebServicePreviewFailureHasNoTestUsers(t *testing.T) {
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{}, WebPreviewDeployFailed(errors.New("The preview migrate command failed with exit code 1. Nothing was deployed."))
	})
	service := webService("api", "us-east-1", "services/api")
	service.PreviewSeedCommand, service.PreviewRoles = "npm run seed", testPreviewRoles
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) { return readyReply(service), nil }
	var stepLog bytes.Buffer
	deps.LogsWriter = &stepLog
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	_, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err == nil || strings.Contains(err.Error(), "test users") || strings.Contains(stepLog.String(), "Preview test users") {
		t.Errorf("err = %v, log:\n%s", err, stepLog.String())
	}
}

// A deploy whose CloudFront step fails after the seed ran has no test users line.
func TestDeployWebServicePreviewDistributionFailureHasNoTestUsers(t *testing.T) {
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{err: errors.New("AccessDenied")}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com", Seed: PreviewSeedSucceeded}, nil
	})
	service := webService("api", "us-east-1", "services/api")
	service.PreviewSeedCommand, service.PreviewRoles = "npm run seed", testPreviewRoles
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) { return readyReply(service), nil }
	deps.TestUsers = NewPreviewTestUsers()
	var stepLog bytes.Buffer
	deps.LogsWriter = &stepLog
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	_, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "test users") ||
		strings.Contains(stepLog.String(), "Preview test users") {
		t.Errorf("err = %v, log:\n%s", err, stepLog.String())
	}
}
