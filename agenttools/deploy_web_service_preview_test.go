package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

func webService(name, region, root string) task_previews.WebServicePreviewSettingsV1 {
	return task_previews.WebServicePreviewSettingsV1{
		DeploymentName: name, EnvironmentName: "production", Region: region, RootDirectory: root,
		DockerFilePath: "Dockerfile", Port: 8080, HealthCheckPath: "/healthz", Cpu: "256", Memory: "512",
	}
}

func readyReply(services ...task_previews.WebServicePreviewSettingsV1) task_previews.WebServicePreviewSettingsReplyV1 {
	return task_previews.WebServicePreviewSettingsReplyV1{Services: services, NetworkReady: true, NamespaceName: "previews-abcd"}
}

func TestChooseWebService(t *testing.T) {
	const repo = "1-acme/api"
	api := webService("api", "us-east-1", "services/api")
	worker := webService("worker", "us-east-1", "./services/worker/")
	apiEU := webService("api-eu", "eu-west-1", "services/api")

	for _, tt := range []struct {
		name    string
		reply   task_previews.WebServicePreviewSettingsReplyV1
		root    *string
		want    string // DeploymentName
		wantErr string
	}{
		{name: "one service", reply: readyReply(api, apiEU), want: "api"},
		{name: "root directory, trimmed", reply: readyReply(api, worker), root: strPtr("/services/worker"), want: "worker"},
		{name: "root directory with ./", reply: readyReply(api, worker), root: strPtr("./services/api/"), want: "api"},
		{name: "no web service", reply: task_previews.WebServicePreviewSettingsReplyV1{NetworkReady: true},
			wantErr: "deployment.io doesn't deploy 1-acme/api as a web service, so it has no backend preview."},
		{name: "runtime-built only", reply: task_previews.WebServicePreviewSettingsReplyV1{RuntimeBuiltServices: []string{"api", "worker"}},
			wantErr: "api, worker build from a language runtime; backend previews need a Dockerfile."},
		{name: "network not ready", reply: task_previews.WebServicePreviewSettingsReplyV1{Services: []task_previews.WebServicePreviewSettingsV1{api},
			NetworkMissingReason: "deployment.io's network isn't set up in us-east-1 yet."},
			wantErr: "deployment.io's network isn't set up in us-east-1 yet."},
		{name: "other region", reply: readyReply(apiEU, webService("api-ap", "ap-south-1", "services/api")),
			wantErr: "1-acme/api's web service is deployed in ap-south-1, eu-west-1, not in this runner's region (us-east-1)."},
		{name: "several", reply: readyReply(api, worker),
			wantErr: "1-acme/api is deployed as several web services here (api, worker); pass root_directory to choose one."},
		{name: "no such root directory", reply: readyReply(api, worker), root: strPtr("web"),
			wantErr: `1-acme/api has no web service with root directory "web" here`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseWebService(tt.reply, repo, tt.root, "us-east-1")
			if tt.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Fatalf("got %+v, %v; want error %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got.DeploymentName != tt.want {
				t.Fatalf("got %+v, %v; want %s", got, err, tt.want)
			}
		})
	}
}

func TestBuildWebPreviewDistributionConfig(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := buildWebPreviewDistributionConfig("p123", "alb-p123-1.us-east-1.elb.amazonaws.com", now)
	if aws.ToString(c.CallerReference) != "preview-p123-1700000000" || aws.ToString(c.Comment) != "deployment.io backend preview p123" {
		t.Errorf("caller reference %q, comment %q", aws.ToString(c.CallerReference), aws.ToString(c.Comment))
	}
	if !aws.ToBool(c.Enabled) || c.PriceClass != cloudfrontTypes.PriceClassPriceClassAll || c.DefaultRootObject != nil {
		t.Errorf("enabled %v, price class %s, root object %v", aws.ToBool(c.Enabled), c.PriceClass, c.DefaultRootObject)
	}
	if aws.ToInt32(c.Origins.Quantity) != 1 || len(c.Origins.Items) != 1 {
		t.Fatalf("origins = %+v", c.Origins)
	}
	o := c.Origins.Items[0]
	if aws.ToString(o.DomainName) != "alb-p123-1.us-east-1.elb.amazonaws.com" || o.S3OriginConfig != nil || o.CustomOriginConfig == nil {
		t.Fatalf("origin = %+v", o)
	}
	co := o.CustomOriginConfig
	if aws.ToInt32(co.HTTPPort) != 80 || aws.ToInt32(co.HTTPSPort) != 443 || co.OriginProtocolPolicy != cloudfrontTypes.OriginProtocolPolicyHttpOnly {
		t.Errorf("custom origin = %+v", co)
	}
	b := c.DefaultCacheBehavior
	if aws.ToString(b.TargetOriginId) != aws.ToString(o.Id) || b.ViewerProtocolPolicy != cloudfrontTypes.ViewerProtocolPolicyRedirectToHttps {
		t.Errorf("behaviour targets %q (origin %q), viewer policy %s", aws.ToString(b.TargetOriginId), aws.ToString(o.Id), b.ViewerProtocolPolicy)
	}
	methods := map[cloudfrontTypes.Method]bool{}
	for _, m := range b.AllowedMethods.Items {
		methods[m] = true
	}
	for _, m := range []cloudfrontTypes.Method{"GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"} {
		if !methods[m] {
			t.Errorf("method %s isn't allowed", m)
		}
	}
	if aws.ToInt32(b.AllowedMethods.Quantity) != 7 || len(b.AllowedMethods.Items) != 7 {
		t.Errorf("allowed methods = %v", b.AllowedMethods.Items)
	}
	cached := b.AllowedMethods.CachedMethods
	if aws.ToInt32(cached.Quantity) != 2 || len(cached.Items) != 2 || cached.Items[0] != "GET" || cached.Items[1] != "HEAD" {
		t.Errorf("cached methods = %+v", cached)
	}
	if aws.ToString(b.CachePolicyId) != cachingDisabledPolicyID || aws.ToString(b.OriginRequestPolicyId) != "216adef6-5c7f-47e4-b989-5492eafa07d3" {
		t.Errorf("cache policy %q, origin request policy %q", aws.ToString(b.CachePolicyId), aws.ToString(b.OriginRequestPolicyId))
	}
	if b.Compress == nil || *b.Compress {
		t.Errorf("compress = %v, want false", b.Compress)
	}
}

type fakeWebCloudFront struct {
	mu      sync.Mutex
	creates []*cloudfront.CreateDistributionInput
	err     error
}

func (f *fakeWebCloudFront) CreateDistribution(_ context.Context, in *cloudfront.CreateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, in)
	if f.err != nil {
		return nil, f.err
	}
	return &cloudfront.CreateDistributionOutput{Distribution: &cloudfrontTypes.Distribution{
		Id: aws.String("E123"), ARN: aws.String("arn:aws:cloudfront::1:distribution/E123"), DomainName: aws.String("d123.cloudfront.net"),
	}}, nil
}

type fakeWebStore struct {
	mu       sync.Mutex
	existing PreviewState
	ensured  []string
	saved    []PreviewState
}

func (s *fakeWebStore) EnsurePreview(serviceName string) (string, PreviewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensured = append(s.ensured, serviceName)
	return "64f0c0ffee0123456789abcd", s.existing, nil
}

func (s *fakeWebStore) SavePreview(_ string, r PreviewState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, r)
}

// webDeps is a backend preview's deps over the repository 1-acme/api (with a
// Dockerfile at services/api), deploying nowhere.
func webDeps(t *testing.T, store *fakeWebStore, cf *fakeWebCloudFront, deploy func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error)) DeployWebServicePreviewDeps {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "task")
	writeTree(t, filepath.Join(workDir, "1-acme", "api"), map[string]string{"services/api/Dockerfile": "FROM scratch"})
	return DeployWebServicePreviewDeps{
		Region:       "us-east-1",
		WorkDirHost:  workDir,
		LogsWriter:   io.Discard,
		Store:        store,
		Record:       NewPreviewRecord(nil, io.Discard),
		Repositories: testRepositories,
		Settings: func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
			return readyReply(webService("api", "us-east-1", "services/api")), nil
		},
		Deploy:          deploy,
		BuildCloudFront: func() (WebPreviewCloudFront, error) { return cf, nil },
	}
}

func TestDeployWebServicePreviewFirstDeploy(t *testing.T) {
	store := &fakeWebStore{existing: PreviewState{EcsServiceArn: "arn:svc"}}
	cf := &fakeWebCloudFront{}
	var got WebServicePreviewDeployInput
	deps := webDeps(t, store, cf, func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		got = in
		if _, err := os.Stat(filepath.Join(in.BuildDir, in.RootDirectory, "Dockerfile")); err != nil {
			t.Errorf("the build copy has no Dockerfile during the deploy: %v", err)
		}
		io.WriteString(in.Logs, "Updated ECS service\n")
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err != nil {
		t.Fatal(err)
	}
	var res deployWebServicePreviewResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://d123.cloudfront.net" || res.HealthURL != "https://d123.cloudfront.net/healthz" ||
		res.Status != "deployed" || res.Deployment != "api (production)" || !strings.Contains(res.LogTail, "Updated ECS service") {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Note, "verify_preview_reachable") {
		t.Errorf("note = %q", res.Note)
	}
	if got.PreviewID != "64f0c0ffee0123456789abcd" || !strings.HasPrefix(got.ServiceKey, "acme-api-services-api-") || got.RootDirectory != "services/api" ||
		got.Existing.EcsServiceArn != "arn:svc" || got.Settings.NamespaceName != "previews-abcd" {
		t.Errorf("deploy input = %+v", got)
	}
	if !strings.HasPrefix(filepath.Base(got.BuildDir), "task-preview-build-") || filepath.Dir(got.BuildDir) != filepath.Dir(deps.WorkDirHost) {
		t.Errorf("build dir %s isn't a copy beside the work dir %s", got.BuildDir, deps.WorkDirHost)
	}
	if _, err := os.Stat(got.BuildDir); !os.IsNotExist(err) {
		t.Errorf("the build copy survived the deploy: %v", err)
	}
	if len(cf.creates) != 1 || aws.ToString(cf.creates[0].DistributionConfig.Origins.Items[0].DomainName) != "alb-x.elb.amazonaws.com" {
		t.Fatalf("CreateDistribution calls = %d", len(cf.creates))
	}
	want := PreviewState{CloudFrontDistributionID: "E123", CloudFrontDistributionArn: "arn:aws:cloudfront::1:distribution/E123", CloudFrontDomainName: "d123.cloudfront.net"}
	if len(store.saved) != 1 || store.saved[0] != want {
		t.Errorf("saved = %+v, want %+v", store.saved, want)
	}
	if hosts := strings.Join(deps.Record.Hosts(), ","); hosts != "d123.cloudfront.net" {
		t.Errorf("record hosts = %s", hosts)
	}
}

func TestDeployWebServicePreviewReusesDistribution(t *testing.T) {
	store := &fakeWebStore{existing: PreviewState{CloudFrontDistributionID: "E9", CloudFrontDomainName: "d9.cloudfront.net"}}
	cf := &fakeWebCloudFront{}
	deps := webDeps(t, store, cf, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	deps.BuildCloudFront = func() (WebPreviewCloudFront, error) { t.Error("a reuse built a CloudFront client"); return cf, nil }
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err != nil || !strings.Contains(out, `"url":"https://d9.cloudfront.net"`) {
		t.Fatalf("got %q, %v", out, err)
	}
	if len(cf.creates) != 0 || len(store.saved) != 0 {
		t.Errorf("a reuse created %d distributions and saved %+v", len(cf.creates), store.saved)
	}
}

func TestWebPreviewServiceKeyKeepsDistinctRootsApart(t *testing.T) {
	a, okA := webPreviewServiceKey("0-acme/api", "services/api-v1")
	b, okB := webPreviewServiceKey("0-acme/api", "services/api/v1")
	if !okA || !okB || a == b {
		t.Errorf("keys %q and %q, want two distinct keys", a, b)
	}
	if !strings.HasPrefix(a, "acme-api-services-api-v1-") || len(a) != len("acme-api-services-api-v1-")+8 {
		t.Errorf("key = %q", a)
	}
	if c, _ := webPreviewServiceKey("3-acme/api", "services/api-v1"); c != a {
		t.Errorf("the repository's index changed the key: %q vs %q", c, a)
	}
	if d, _ := webPreviewServiceKey("0-acme/api-services", "api-v1"); d == a {
		t.Errorf("distinct repositories share the key %q", d)
	}
}

// A deploy that finds no distribution on record yet reuses the one this run created,
// since SavePreview only queues the save.
func TestDeployWebServicePreviewReusesDistributionBeforeTheSaveLands(t *testing.T) {
	store := &fakeWebStore{}
	cf := &fakeWebCloudFront{}
	deps := webDeps(t, store, cf, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	deps.Distributions = NewWebPreviewDistributions()
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	for i := 0; i < 2; i++ {
		out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
		if err != nil || !strings.Contains(out, `"url":"https://d123.cloudfront.net"`) {
			t.Fatalf("deploy %d: %q, %v", i+1, out, err)
		}
	}
	if len(cf.creates) != 1 || len(store.saved) != 1 {
		t.Errorf("created %d distributions and saved %d, want one each", len(cf.creates), len(store.saved))
	}
}

func TestDeployWebServicePreviewFailures(t *testing.T) {
	refusal := errors.New("Preview configuration variable KEY contains a line break; backend previews can't pass it.")
	store := &fakeWebStore{}
	deps := webDeps(t, store, &fakeWebCloudFront{}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{}, refusal
	})
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	args := json.RawMessage(`{"repository":"1-acme/api"}`)
	if _, err := handleDeployWebServicePreview(context.Background(), deps, builds, args); err != refusal {
		t.Errorf("got %v, want the deploy's refusal as is", err)
	}

	cf := &fakeWebCloudFront{err: errors.New("AccessDenied")}
	deps = webDeps(t, store, cf, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	_, err := handleDeployWebServicePreview(context.Background(), deps, builds, args)
	if err == nil || !strings.HasPrefix(err.Error(), "The backend preview's deploy failed: ") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("CloudFront failure: got %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("a failed deploy saved %+v", store.saved)
	}

	// A refusal of the selection comes back before anything is ensured.
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		return task_previews.WebServicePreviewSettingsReplyV1{}, nil
	}
	store.ensured = nil
	_, err = handleDeployWebServicePreview(context.Background(), deps, builds, args)
	if err == nil || err.Error() != "deployment.io doesn't deploy 1-acme/api as a web service, so it has no backend preview." || len(store.ensured) != 0 {
		t.Errorf("got %v (ensured %v)", err, store.ensured)
	}
}

func TestDeployWebServicePreviewDeployingAttachesOnePerService(t *testing.T) {
	saved := previewBuildCallWait
	previewBuildCallWait = 50 * time.Millisecond
	defer func() { previewBuildCallWait = saved }()

	release := make(chan struct{})
	var mu sync.Mutex
	deploys, lookups := 0, 0
	store := &fakeWebStore{}
	deps := webDeps(t, store, &fakeWebCloudFront{}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		mu.Lock()
		deploys++
		mu.Unlock()
		<-release
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	settings := deps.Settings
	deps.Settings = func(u string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		mu.Lock()
		lookups++
		mu.Unlock()
		return settings(u)
	}
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	deploying := `{"status":"deploying","message":"The backend preview is still deploying (a new one takes about 5 minutes). Call deploy_web_service_preview again with the same arguments to wait for it."}`
	for _, args := range []string{`{"repository":"1-acme/api"}`, `{"repository":"1-acme/api"}`, `{"repository":"/work/1-acme/api","root_directory":"services/api"}`} {
		out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(args))
		if err != nil || out != deploying {
			t.Fatalf("call %s during the deploy: %q, %v", args, out, err)
		}
	}
	close(release)
	out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err != nil || !strings.Contains(out, `"status":"deployed"`) {
		t.Fatalf("call after the deploy: %q, %v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deploys != 1 {
		t.Errorf("deploys = %d, want 1: every call is the same service", deploys)
	}
	// The second call attached by its arguments; the third (other arguments, same
	// service) looked up and joined the running deploy.
	if lookups != 2 {
		t.Errorf("settings looked up %d times, want 2", lookups)
	}
}

func TestStaticPreviewBuildsStillAnswerBuilding(t *testing.T) {
	b := newPreviewBuilds()
	defer b.stop()
	if b.pending.Status != "building" || b.pending.Message != previewBuildingMessage {
		t.Errorf("static pending = %+v", b.pending)
	}
}

// DeployWebServicePreview is the tool's plan and deploy run synchronously: with a
// merge result it builds from it, and the URL lands in the shared record.
func TestDeployWebServicePreviewSynchronousBuildsFromTheMergeResult(t *testing.T) {
	store := &fakeWebStore{}
	cf := &fakeWebCloudFront{}
	deps := webDeps(t, store, cf, func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		if _, err := os.Stat(filepath.Join(in.BuildDir, "services/api/merged.txt")); err != nil {
			t.Errorf("the build isn't the merge result: %v", err)
		}
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	merge := &fakeMergeResult{ok: true, files: map[string]string{"services/api/Dockerfile": "FROM scratch", "services/api/merged.txt": "x"}}
	deps.MergeResult = merge
	deps.Distributions = NewWebPreviewDistributions()
	for i := 0; i < 2; i++ {
		got, err := DeployWebServicePreview(context.Background(), deps, "1-acme/api")
		if err != nil {
			t.Fatal(err)
		}
		if got.URL != "https://d123.cloudfront.net" || got.HealthURL != "https://d123.cloudfront.net/healthz" || got.Deployment != "api (production)" {
			t.Errorf("deploy %d = %+v", i+1, got)
		}
	}
	if len(merge.calls) != 2 || merge.calls[0] != "1-acme/api" {
		t.Errorf("merge result asked for %v", merge.calls)
	}
	if len(cf.creates) != 1 {
		t.Errorf("created %d distributions, want 1: the shared memo is reused", len(cf.creates))
	}
	if hosts := strings.Join(deps.Record.Hosts(), ","); hosts != "d123.cloudfront.net" {
		t.Errorf("record hosts = %s", hosts)
	}
}

func TestCheckWebServicePreview(t *testing.T) {
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		t.Error("a check deployed")
		return WebServicePreviewDeployOutput{}, nil
	})
	if err := CheckWebServicePreview(deps, "1-acme/api"); err != nil {
		t.Errorf("a managed Dockerfile web service was refused: %v", err)
	}
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		return readyReply(webService("api", "us-east-1", "a"), webService("worker", "us-east-1", "b")), nil
	}
	if err := CheckWebServicePreview(deps, "1-acme/api"); err == nil || !strings.Contains(err.Error(), "several web services") {
		t.Errorf("several web services: got %v", err)
	}
}
