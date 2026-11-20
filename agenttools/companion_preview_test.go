package agenttools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// testCompanion is the companion "companion-0-acme/app-server", the deployment
// app-server deployed from main, whose source is files.
func testCompanion(t *testing.T, files map[string]string) CompanionWebService {
	return CompanionWebService{
		Repository: "companion-0-acme/app-server",
		Entry:      tasks.RepositoryEntry{Name: "acme/app-server", CloneURL: "https://github.com/acme/app-server.git", BaseBranch: "main"},
		API:        "app-server",
		Checkout: func(_ context.Context, dir string) error {
			writeTree(t, dir, files)
			return nil
		},
	}
}

// A companion's service is the one with its name deploying its branch, chosen
// by region and, among copies, the Production one; its settings are looked up
// by its clone URL, never resolved among the Task's repositories.
func TestPlanCompanionWebServicePreviewChoosesByNameBranchAndRegion(t *testing.T) {
	prod := webService("app-server", "us-east-1", "")
	prod.Branch, prod.ProductionEnvironment = "main", true
	staging := webService("app-server", "us-east-1", "")
	staging.Branch, staging.EnvironmentName = "main", "staging"
	other := webService("app-server", "us-east-1", "")
	other.Branch = "develop"
	eu := webService("app-server", "eu-west-1", "")
	eu.Branch = "main"
	worker := webService("worker", "us-east-1", "")
	worker.Branch = "main"
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, nil)
	deps.Deploy = func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{}, nil
	}
	var asked []string
	deps.Settings = func(cloneURL string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		asked = append(asked, cloneURL)
		return readyReply(other, worker, eu, staging, prod), nil
	}
	plan, err := PlanCompanionWebServicePreview(deps, testCompanion(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Repository != "companion-0-acme/app-server" || plan.DeploymentName != "app-server" || plan.plan.service.EnvironmentName != "production" || plan.plan.service.Branch != "main" {
		t.Errorf("plan = %+v", plan)
	}
	if want, _ := webPreviewServiceKey("companion-0-acme/app-server", ""); plan.ServiceName != want || !strings.HasPrefix(want, "companion-0-acme-app-server-") {
		t.Errorf("service name = %q, want %q", plan.ServiceName, want)
	}
	if strings.Join(asked, ",") != "https://github.com/acme/app-server.git" {
		t.Errorf("settings asked for %v", asked)
	}

	// Nothing with the name and branch left.
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		return readyReply(other, worker), nil
	}
	if _, err := PlanCompanionWebServicePreview(deps, testCompanion(t, nil)); err == nil || err.Error() != "deployment.io no longer deploys app-server from main as a web service" {
		t.Errorf("err = %v", err)
	}
	// Only another region left: chooseWebService's own refusal.
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		return readyReply(eu), nil
	}
	if _, err := PlanCompanionWebServicePreview(deps, testCompanion(t, nil)); err == nil || !strings.Contains(err.Error(), "not in this runner's region (us-east-1)") {
		t.Errorf("err = %v", err)
	}
}

// A companion's preview builds from its own checkout, in a new directory beside
// the work dir with .git, node_modules and escaping symlinks removed, and removed
// after the deploy; a directory named like it under the work dir is never read.
func TestDeployCompanionWebServicePreviewBuildsFromItsCheckout(t *testing.T) {
	var got WebServicePreviewDeployInput
	var seen map[string]bool
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		got = in
		seen = map[string]bool{}
		filepath.WalkDir(in.BuildDir, func(p string, _ os.DirEntry, _ error) error {
			rel, _ := filepath.Rel(in.BuildDir, p)
			seen[filepath.ToSlash(rel)] = true
			return nil
		})
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	service := webService("app-server", "us-east-1", "")
	service.Branch = "main"
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) { return readyReply(service), nil }
	writeTree(t, filepath.Join(deps.WorkDirHost, "companion-0-acme", "app-server"), map[string]string{"Dockerfile": "FROM work-dir", "planted": "x"})
	c := testCompanion(t, map[string]string{"Dockerfile": "FROM scratch", ".git/config": "x", "web/node_modules/a.js": "x"})
	checkout := c.Checkout
	c.Checkout = func(ctx context.Context, dir string) error {
		if err := checkout(ctx, dir); err != nil {
			return err
		}
		return os.Symlink("/etc/passwd", filepath.Join(dir, "escape"))
	}
	plan, err := PlanCompanionWebServicePreview(deps, c)
	if err != nil {
		t.Fatal(err)
	}
	d, err := DeployWebServicePreviewWith(context.Background(), deps, plan, map[string]string{"CORS_ORIGINS": "https://d1.cloudfront.net"})
	if err != nil {
		t.Fatal(err)
	}
	if d.URL == "" || got.ServiceKey != plan.ServiceName || got.Settings.Variables["CORS_ORIGINS"] != "https://d1.cloudfront.net" {
		t.Errorf("deployed %+v with input %+v", d, got)
	}
	if !strings.HasPrefix(filepath.Base(got.BuildDir), "task-preview-build-") || filepath.Dir(got.BuildDir) != filepath.Dir(deps.WorkDirHost) {
		t.Errorf("build dir %s isn't beside the work dir %s", got.BuildDir, deps.WorkDirHost)
	}
	if !seen["Dockerfile"] || seen["planted"] || seen[".git"] || seen["web/node_modules"] || seen["escape"] {
		t.Errorf("the build directory held %v", seen)
	}
	if _, err := os.Stat(got.BuildDir); !os.IsNotExist(err) {
		t.Errorf("the build directory survived the deploy: %v", err)
	}
}

// A checkout that fails fails the deploy: nothing is deployed and nothing falls
// back to the work dir.
func TestDeployCompanionWebServicePreviewFailedCheckout(t *testing.T) {
	deployed := false
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		deployed = true
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	service := webService("app-server", "us-east-1", "")
	service.Branch = "main"
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) { return readyReply(service), nil }
	writeTree(t, filepath.Join(deps.WorkDirHost, "companion-0-acme", "app-server"), map[string]string{"Dockerfile": "FROM work-dir"})
	var dir string
	c := testCompanion(t, nil)
	c.Checkout = func(_ context.Context, d string) error {
		dir = d
		return errors.New("the repository has submodules")
	}
	plan, err := PlanCompanionWebServicePreview(deps, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeployWebServicePreviewWith(context.Background(), deps, plan, nil); err == nil || !strings.Contains(err.Error(), "the repository has submodules") {
		t.Errorf("err = %v", err)
	}
	if deployed {
		t.Error("a failed checkout deployed")
	}
	if _, err := os.Stat(dir); dir == "" || !os.IsNotExist(err) {
		t.Errorf("the build directory %q survived: %v", dir, err)
	}
}

// A planned site carries its deployment's environment; a detected one none.
func TestPlanStaticSitePreviewCarriesTheEnvironment(t *testing.T) {
	var builds [][]string
	deps := stackSiteDeps(t, &fakeWebStore{}, task_previews.StaticSiteBuildSettingsReplyV1{}, &builds)
	plan, err := PlanStaticSitePreview(deps, "0-acme/web")
	if err != nil || plan.DeploymentName != "web" || plan.EnvironmentName != "production" {
		t.Errorf("plan = %+v, %v", plan, err)
	}
	deps.BuildSettings = func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
		return task_previews.StaticSiteBuildSettingsReplyV1{}, nil
	}
	deps.DetectSite = func(string) (DetectedSite, string, bool) {
		return DetectedSite{Framework: "Vite", BuildCommand: "npm run build", PublishDirectory: "dist"}, "", true
	}
	if plan, err := PlanStaticSitePreview(deps, "0-acme/web"); err != nil || plan.EnvironmentName != "" {
		t.Errorf("detected plan = %+v, %v", plan, err)
	}
}
