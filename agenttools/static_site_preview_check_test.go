package agenttools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

func TestCheckRunnerBuiltPreview(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "0-acme/web"), 0o755); err != nil {
		t.Fatal(err)
	}
	site := task_previews.StaticSiteBuildSettingsV1{DeploymentName: "web", BuildCommand: "npm run build", PublishDirectory: "dist"}
	deps := func(reply task_previews.StaticSiteBuildSettingsReplyV1, lookupErr error, detected bool) DeployStaticSitePreviewDeps {
		return DeployStaticSitePreviewDeps{
			WorkDirHost:  workDir,
			Repositories: testRepositories,
			BuildSettings: func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
				return reply, lookupErr
			},
			DetectSite: func(string) (DetectedSite, string, bool) {
				if !detected {
					return DetectedSite{}, "no package.json", false
				}
				return DetectedSite{BuildCommand: "npm run build", PublishDirectory: "dist"}, "", true
			},
		}
	}
	for _, tc := range []struct {
		name       string
		deps       DeployStaticSitePreviewDeps
		wantOK     bool
		wantReason string
		wantLookup bool
	}{
		{"a managed static site", deps(task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{site}}, nil, false), true, "", false},
		{"detected settings", deps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil, true), true, "", false},
		{"nothing detected", deps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil, false), false, "no package.json", false},
		{"a site with no build command", deps(task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{{DeploymentName: "web", PublishDirectory: "dist"}}}, nil, false), false, "no build command", false},
		{"the lookup failed", deps(task_previews.StaticSiteBuildSettingsReplyV1{}, errors.New("connection refused"), true), false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckRunnerBuiltPreview(tc.deps, "0-acme/web")
			if got.OK != tc.wantOK || (got.LookupErr != nil) != tc.wantLookup || !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("check = %+v, want ok=%t reason~%q lookup error=%t", got, tc.wantOK, tc.wantReason, tc.wantLookup)
			}
		})
	}
}
