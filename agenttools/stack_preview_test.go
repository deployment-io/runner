package agenttools

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// stackSiteDeps is a static preview's deps over 0-acme/web, a site built with
// "npm run build" into dist, deploying nowhere; builds records each build's env.
func stackSiteDeps(t *testing.T, store PreviewStore, configuration task_previews.StaticSiteBuildSettingsReplyV1, builds *[][]string) DeployStaticSitePreviewDeps {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "task")
	writeTree(t, filepath.Join(workDir, "0-acme", "web"), map[string]string{"package.json": "{}"})
	var mu sync.Mutex
	return DeployStaticSitePreviewDeps{
		OrgID:        "org",
		Region:       "us-east-1",
		WorkDirHost:  workDir,
		LogsWriter:   io.Discard,
		Store:        store,
		Repositories: testRepositories,
		BuildClients: func() (*s3.Client, *cloudfront.Client, error) { return nil, nil, nil },
		BuildSettings: func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
			reply := configuration
			reply.Sites = []task_previews.StaticSiteBuildSettingsV1{site("web", "", "npm run build", "dist", false)}
			return reply, nil
		},
		BuildSite: func(_ context.Context, dir, _ string, env []string, _ io.Writer) error {
			mu.Lock()
			*builds = append(*builds, env)
			mu.Unlock()
			writeTree(t, filepath.Join(dir, "dist"), map[string]string{"index.html": "<h1>web</h1>", "assets/app.js": "x"})
			return nil
		},
	}
}

// fakeStaticCloud replaces the reservation and the upload with ones that create
// nothing: the reservation answers E1 at d1.cloudfront.net.
type fakeStaticCloud struct {
	reserves []StaticPreviewDeployInput
	deploys  []StaticPreviewDeployInput
}

func installFakeStaticCloud(t *testing.T) *fakeStaticCloud {
	t.Helper()
	f := &fakeStaticCloud{}
	savedReserve, savedDeploy := reservePreviewDist, deployStaticPreview
	t.Cleanup(func() { reservePreviewDist, deployStaticPreview = savedReserve, savedDeploy })
	reservePreviewDist = func(in StaticPreviewDeployInput, _ io.Writer) (StaticPreviewDeployResult, error) {
		f.reserves = append(f.reserves, in)
		return StaticPreviewDeployResult{DistributionID: "E1", DistributionArn: "arn:E1", DomainName: "d1.cloudfront.net"}, nil
	}
	deployStaticPreview = func(in StaticPreviewDeployInput, _ io.Writer) (StaticPreviewDeployResult, error) {
		f.deploys = append(f.deploys, in)
		if in.ExistingDistID != "" {
			return StaticPreviewDeployResult{DistributionID: in.ExistingDistID, UploadedKeys: []string{"assets/app.js", "index.html"}}, nil
		}
		return StaticPreviewDeployResult{DistributionID: "E2", DomainName: "d2.cloudfront.net", UploadedKeys: []string{"index.html"}}, nil
	}
	return f
}

// The distribution is reserved before the build, the build uploads into it, and a
// second round (whose EnsurePreview still returns no distribution) reuses it from
// the memo: one distribution, saved once.
func TestStaticSitePreviewReservedOnceAndReusedAcrossRounds(t *testing.T) {
	cloud := installFakeStaticCloud(t)
	store := &fakeWebStore{}
	var builds [][]string
	configuration := task_previews.StaticSiteBuildSettingsReplyV1{ConfigurationSet: true, Variables: map[string]string{"VITE_API_URL": "https://staging.example", "OTHER": "kept"}}
	deps := stackSiteDeps(t, store, configuration, &builds)
	deps.Distributions = NewWebPreviewDistributions()
	for round := 1; round <= 2; round++ {
		deps.Record = NewPreviewRecord(nil, io.Discard)
		plan, err := PlanStaticSitePreview(deps, "0-acme/web")
		if err != nil {
			t.Fatal(err)
		}
		if plan.DeploymentName != "web" || plan.Repository != "0-acme/web" {
			t.Errorf("plan = %+v", plan)
		}
		origin, err := ReserveStaticSitePreview(context.Background(), deps, plan)
		if err != nil || origin != "https://d1.cloudfront.net" {
			t.Fatalf("round %d: reserve = %q, %v", round, origin, err)
		}
		url, err := DeployStaticSitePreviewWith(context.Background(), deps, plan, map[string]string{"VITE_API_URL": "https://d9.cloudfront.net/api"})
		if err != nil || url != origin {
			t.Fatalf("round %d: deploy = %q, %v", round, url, err)
		}
		probe, _ := deps.Record.pendingProbe("d1.cloudfront.net")
		if round == 1 && probe != "assets/app.js" {
			t.Errorf("round 1: probe = %q, want the upload's (a reserved distribution counts as created)", probe)
		}
	}
	if len(cloud.reserves) != 1 || len(store.saved) != 1 {
		t.Errorf("reserved %d, saved %d; want one each", len(cloud.reserves), len(store.saved))
	}
	if len(cloud.deploys) != 2 || cloud.deploys[0].ExistingDistID != "E1" || cloud.deploys[1].ExistingDistID != "E1" {
		t.Errorf("deploys = %+v, want both into E1", cloud.deploys)
	}
	for _, env := range builds {
		if got := strings.Join(env, ","); got != "OTHER=kept,VITE_API_URL=https://d9.cloudfront.net/api" {
			t.Errorf("build env = %s", got)
		}
	}
}

// A distribution the preview record already has is reused, not re-created.
func TestReserveStaticSitePreviewReusesTheRecordsDistribution(t *testing.T) {
	cloud := installFakeStaticCloud(t)
	store := &fakeWebStore{existing: PreviewState{CloudFrontDistributionID: "E7", CloudFrontDomainName: "d7.cloudfront.net"}}
	var builds [][]string
	deps := stackSiteDeps(t, store, task_previews.StaticSiteBuildSettingsReplyV1{}, &builds)
	deps.Record = NewPreviewRecord(nil, io.Discard)
	plan, err := PlanStaticSitePreview(deps, "0-acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if origin, err := ReserveStaticSitePreview(context.Background(), deps, plan); err != nil || origin != "https://d7.cloudfront.net" {
		t.Fatalf("reserve = %q, %v", origin, err)
	}
	if len(cloud.reserves) != 0 || len(store.saved) != 0 {
		t.Errorf("reused distribution: reserved %d, saved %d", len(cloud.reserves), len(store.saved))
	}
	// The wired variables apply with no preview configuration set.
	if _, err := DeployStaticSitePreviewWith(context.Background(), deps, plan, map[string]string{"VITE_API_URL": "https://d9.cloudfront.net"}); err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 || strings.Join(builds[0], ",") != "VITE_API_URL=https://d9.cloudfront.net" {
		t.Errorf("builds = %v", builds)
	}
}

func TestPlanStaticSitePreviewOfADetectedSiteHasNoDeployment(t *testing.T) {
	var builds [][]string
	deps := stackSiteDeps(t, &fakeWebStore{}, task_previews.StaticSiteBuildSettingsReplyV1{}, &builds)
	deps.BuildSettings = func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
		return task_previews.StaticSiteBuildSettingsReplyV1{}, nil
	}
	deps.DetectSite = func(string) (DetectedSite, string, bool) {
		return DetectedSite{Framework: "Vite", BuildCommand: "npm run build", PublishDirectory: "dist"}, "", true
	}
	plan, err := PlanStaticSitePreview(deps, "0-acme/web")
	if err != nil || plan.DeploymentName != "" {
		t.Errorf("plan = %+v, %v; want a detected site with no deployment", plan, err)
	}
	deps.DetectSite = func(string) (DetectedSite, string, bool) { return DetectedSite{}, "no package.json", false }
	if _, err := PlanStaticSitePreview(deps, "0-acme/web"); err == nil || !strings.Contains(err.Error(), "no package.json") {
		t.Errorf("err = %v", err)
	}
}

// A wired variable replaces Preview configuration's of the same name; the preview
// database's variable wins over it, logged by name.
func TestDeployWebServicePreviewWithWiredVariables(t *testing.T) {
	var got WebServicePreviewDeployInput
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		got = in
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	var logs syncBuffer
	deps.LogsWriter = &logs
	deps.DatabaseStore = &fakeWebStore{}
	service := webService("api", "us-east-1", "services/api")
	service.PreviewDatabase = &task_previews.PreviewDatabaseV1{Mode: task_previews.PreviewDatabaseModeUse, Engine: "postgres", Version: "16", Variables: []string{"DATABASE_URL"}}
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		reply := readyReply(service)
		reply.Variables = map[string]string{"CORS_ORIGINS": "https://staging.example", "KEEP": "1"}
		return reply, nil
	}
	plan, err := PlanWebServicePreview(deps, "1-acme/api")
	if err != nil || plan.DeploymentName != "api" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	wired := map[string]string{"CORS_ORIGINS": "https://d1.cloudfront.net", "DATABASE_URL": "https://d1.cloudfront.net"}
	if _, err := DeployWebServicePreviewWith(context.Background(), deps, plan, wired); err != nil {
		t.Fatal(err)
	}
	v := got.Settings.Variables
	// The deployer puts the database's DATABASE_URL over the wired one, in the
	// environment and the build args (WiredNames).
	if v["CORS_ORIGINS"] != "https://d1.cloudfront.net" || v["KEEP"] != "1" {
		t.Errorf("variables = %v", v)
	}
	if fmt.Sprint(got.WiredNames) != "[CORS_ORIGINS DATABASE_URL]" {
		t.Errorf("wired names = %v", got.WiredNames)
	}
	if got.Database == nil {
		t.Fatal("no preview database")
	}
	if !strings.Contains(logs.String(), "the preview database's DATABASE_URL replaces the value QA set for it") {
		t.Errorf("the log doesn't name the replaced variable:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "d1.cloudfront.net") {
		t.Errorf("the log carries a wired value:\n%s", logs.String())
	}
}

func TestPreviewBuildEnvWired(t *testing.T) {
	c := task_previews.StaticSiteBuildSettingsReplyV1{ConfigurationSet: true, Variables: map[string]string{"A": "1", "B": "2"}}
	if got := strings.Join(previewBuildEnv(c, map[string]string{"B": "wired", "C": "3"}), ","); got != "A=1,B=wired,C=3" {
		t.Errorf("env = %s", got)
	}
	c.ConfigurationSet = false
	if got := strings.Join(previewBuildEnv(c, map[string]string{"B": "wired"}), ","); got != "B=wired" {
		t.Errorf("env without a configuration = %s", got)
	}
}

// A build that prints a wired variable doesn't put its value in the Step log.
func TestStaticSitePreviewBuildOutputHidesWiredValues(t *testing.T) {
	installFakeStaticCloud(t)
	var builds [][]string
	deps := stackSiteDeps(t, &fakeWebStore{}, task_previews.StaticSiteBuildSettingsReplyV1{}, &builds)
	build := deps.BuildSite
	deps.BuildSite = func(ctx context.Context, dir, command string, env []string, logs io.Writer) error {
		io.WriteString(logs, "VITE_API_URL is https://d9.cloudfront.net/sec")
		io.WriteString(logs, "ret-path/v1\nlast line https://d9.cloudfront.net/secret-path/v1")
		return build(ctx, dir, command, env, logs)
	}
	var logs syncBuffer
	deps.LogsWriter = &logs
	deps.Record = NewPreviewRecord(nil, io.Discard)
	plan, err := PlanStaticSitePreview(deps, "0-acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeployStaticSitePreviewWith(context.Background(), deps, plan, map[string]string{"VITE_API_URL": "https://d9.cloudfront.net/secret-path/v1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "secret-path") {
		t.Errorf("the log carries a wired value:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "VITE_API_URL is <VITE_API_URL>\nlast line <VITE_API_URL>") {
		t.Errorf("the build's output isn't in the log, redacted:\n%s", logs.String())
	}
}
