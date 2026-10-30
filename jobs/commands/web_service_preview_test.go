package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	testWebPreviewID = "64f0c0ffee0123456789abcd"
	testWebOrgID     = "6500000000000000000a1b2c"
)

func testWebPreviewInput(buildDir string) agenttools.WebServicePreviewDeployInput {
	return agenttools.WebServicePreviewDeployInput{
		PreviewID:     testWebPreviewID,
		ServiceKey:    "acme-api-services-api",
		BuildDir:      buildDir,
		RootDirectory: "services/api",
		Service: task_previews.WebServicePreviewSettingsV1{
			DeploymentName: "api", EnvironmentName: "production", Region: "us-east-1", RootDirectory: "services/api",
			DockerFilePath: "docker/Dockerfile.prod", DockerBuildArgs: []string{"A=prod-a", "API_KEY=prod-key", "URL=prod-url"},
			Port: 8080, HealthCheckPath: "/healthz", Cpu: "256", Memory: "512",
			PreDeployCommand: "./migrate", StartCommand: "./server",
		},
		Settings: task_previews.WebServicePreviewSettingsReplyV1{
			ConfigurationSet: true,
			Variables:        map[string]string{"ZED": "last", "API_KEY": "s3cret", "EMPTY": "", "URL": "https://x/?a=b"},
			Files:            []task_previews.PreviewFileV1{{Name: ".env", Contents: "A=b"}},
			Network: task_previews.WebServicePreviewNetworkV1{
				VpcID: "vpc-1", VpcCidr: "10.0.0.0/16", PublicSubnets: []string{"subnet-a", "subnet-b"},
				PrivateSubnets: []string{"subnet-c"}, EcsClusterArn: "arn:cluster", EcsTaskExecutionRoleArn: "arn:role",
			},
			NetworkReady:  true,
			NamespaceName: "previews-1b2c",
		},
		BuildLogs: io.Discard,
		Logs:      io.Discard,
	}
}

func testWebPreviewParametersInput(buildDir string) webPreviewParametersInput {
	return webPreviewParametersInput{
		OrganizationIDNamespace: "orgns",
		OrganizationIdFromJob:   testWebOrgID,
		Region:                  1,
		Now:                     time.Unix(1700000000, 0),
		Deploy:                  testWebPreviewInput(buildDir),
	}
}

func paramValue(t *testing.T, p map[string]interface{}, k parameters_enums.Key) interface{} {
	t.Helper()
	v, ok := p[mustKey(t, k)]
	if !ok {
		t.Fatalf("%s is missing", k)
	}
	return v
}

func TestBuildWebPreviewParametersKeysAndTypes(t *testing.T) {
	buildDir := "/tmp/orgns/task-preview-build-123"
	p, err := buildWebPreviewParameters(testWebPreviewParametersInput(buildDir))
	if err != nil {
		t.Fatal(err)
	}
	want := map[parameters_enums.Key]interface{}{
		parameters_enums.OrganizationIDNamespace: "orgns",
		parameters_enums.OrganizationIdFromJob:   testWebOrgID,
		parameters_enums.DeploymentID:            testWebPreviewID,
		parameters_enums.DeploymentName:          "pv-acme-api-services-api",
		parameters_enums.EnvironmentName:         "previews",
		parameters_enums.EnvironmentID:           testWebOrgID,
		parameters_enums.BuildID:                 "task-preview-build-123",
		parameters_enums.RepoDirectoryPath:       "/tmp/orgns/task-preview-build-123/services/api",
		parameters_enums.CommitHash:              "preview-1700000000",
		parameters_enums.DockerFilePath:          "docker/Dockerfile.prod",
		parameters_enums.DockerBuildArgs:         primitive.A{"API_KEY=s3cret", "URL=https://x/?a=b"},
		parameters_enums.Region:                  int64(1),
		parameters_enums.Port:                    int64(8080),
		parameters_enums.HealthCheckPath:         "/healthz",
		parameters_enums.Cpu:                     "256",
		parameters_enums.Memory:                  "512",
		parameters_enums.VpcID:                   "vpc-1",
		parameters_enums.VpcCidr:                 "10.0.0.0/16",
		parameters_enums.PublicSubnets:           primitive.A{"subnet-a", "subnet-b"},
		parameters_enums.PrivateSubnets:          primitive.A{"subnet-c"},
		parameters_enums.EcsClusterArn:           "arn:cluster",
		parameters_enums.EcsTaskExecutionRoleArn: "arn:role",
		parameters_enums.EnvironmentVariables:    "API_KEY=s3cret\nEMPTY=\nURL=https://x/?a=b\nZED=last\n",
	}
	for k, w := range want {
		got := paramValue(t, p, k)
		if reflect.TypeOf(got) != reflect.TypeOf(w) || !reflect.DeepEqual(got, w) {
			t.Errorf("%s = %#v (%T), want %#v (%T)", k, got, got, w, w)
		}
	}
	if len(p) != len(want) {
		t.Errorf("the map has %d keys, want %d: %v", len(p), len(want), p)
	}
	// The types the commands read them with.
	if _, err := jobs.GetParameterValue[primitive.A](p, parameters_enums.PrivateSubnets); err != nil {
		t.Error(err)
	}
	if _, err := commandUtils.GetDockerBuildArgs(p); err != nil {
		t.Error(err)
	}
	if v, err := jobs.GetParameterValue[int64](p, parameters_enums.Region); err != nil || v != 1 {
		t.Errorf("Region = %d, %v", v, err)
	}
	if image, err := getDockerImageNameAndTag(p); err != nil || image != "orgns-"+testWebPreviewID+":preview-1700000000" {
		t.Errorf("image = %q, %v", image, err)
	}
	if env, err := decodeEnvironmentVariablesToKeyValueSlice(paramValue(t, p, parameters_enums.EnvironmentVariables).(string)); err != nil || len(env) != 4 ||
		aws.ToString(env[2].Name) != "URL" || aws.ToString(env[2].Value) != "https://x/?a=b" {
		t.Errorf("environment = %+v, %v", env, err)
	}
	for _, never := range []parameters_enums.Key{
		parameters_enums.TaskDefinitionArn, parameters_enums.IsPreview, parameters_enums.RestartOnly,
		parameters_enums.JobID, parameters_enums.PreDeployCommand,
		parameters_enums.IsPrivateRegistry, parameters_enums.SecretName, parameters_enums.RepoProviderToken,
		parameters_enums.IsPrivateService, parameters_enums.EcrRepositoryUri, parameters_enums.AlbSecurityGroupId,
		parameters_enums.LoadBalancerArn, parameters_enums.TargetGroupArn, parameters_enums.EcsServiceArn,
	} {
		if _, ok := p[mustKey(t, never)]; ok {
			t.Errorf("%s is set", never)
		}
	}
}

func TestBuildWebPreviewParametersOptionalValues(t *testing.T) {
	in := testWebPreviewParametersInput("/tmp/orgns/b")
	in.Deploy.Service.DockerFilePath = ""
	in.Deploy.Service.DockerBuildArgs = nil
	in.Deploy.Settings.ConfigurationSet = false
	in.Deploy.Settings.Variables = nil
	p, err := buildWebPreviewParameters(in)
	if err != nil {
		t.Fatal(err)
	}
	if paramValue(t, p, parameters_enums.DockerFilePath) != "Dockerfile" || paramValue(t, p, parameters_enums.EnvironmentVariables) != "" {
		t.Errorf("Dockerfile %v, environment %q", p[mustKey(t, parameters_enums.DockerFilePath)], p[mustKey(t, parameters_enums.EnvironmentVariables)])
	}
	if _, ok := p[mustKey(t, parameters_enums.DockerBuildArgs)]; ok {
		t.Error("DockerBuildArgs is set without build args")
	}
}

// A preview is built with the preview configuration's values under the production
// build args' names, never with production's values: the agent writes the Dockerfile.
func TestWebPreviewBuildArgs(t *testing.T) {
	variables := map[string]string{"API_KEY": "preview-key", "EMPTY": "", "URL": "https://x/?a=b"}
	for _, tt := range []struct {
		name       string
		production []string
		args, left []string
	}{
		{name: "none", production: nil},
		{name: "value from the preview configuration", production: []string{"API_KEY=prod-key"}, args: []string{"API_KEY=preview-key"}},
		{name: "empty preview value", production: []string{"EMPTY=prod"}, args: []string{"EMPTY="}},
		{name: "no preview variable", production: []string{"NPM_TOKEN=prod-token"}, left: []string{"NPM_TOKEN"}},
		{name: "preview value with =", production: []string{"URL=prod"}, args: []string{"URL=https://x/?a=b"}},
		{name: "name only", production: []string{"API_KEY"}, args: []string{"API_KEY=preview-key"}},
		{name: "duplicates and blanks", production: []string{"API_KEY=a", " API_KEY =b", "=c", ""}, args: []string{"API_KEY=preview-key"}},
		{name: "order kept", production: []string{"NPM_TOKEN=t", "EMPTY=e", "API_KEY=k"}, args: []string{"EMPTY=", "API_KEY=preview-key"}, left: []string{"NPM_TOKEN"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args, left := webPreviewBuildArgs(tt.production, variables)
			if !reflect.DeepEqual(args, tt.args) || !reflect.DeepEqual(left, tt.left) {
				t.Errorf("args %q, left %q; want %q, %q", args, left, tt.args, tt.left)
			}
			for _, a := range append(args, left...) {
				for _, prod := range []string{"prod-key", "prod-token", "prod"} {
					if strings.HasSuffix(a, "="+prod) || a == prod {
						t.Errorf("production value %q reached the preview: %q", prod, a)
					}
				}
			}
		})
	}
}

func TestBuildWebPreviewParametersReuseHandles(t *testing.T) {
	for _, tt := range []struct {
		name     string
		existing agenttools.PreviewState
		want     map[parameters_enums.Key]string
	}{
		{name: "first deploy"},
		{name: "all", existing: agenttools.PreviewState{
			EcrRepositoryUri: "1.dkr.ecr/r", AlbSecurityGroupId: "sg-1", LoadBalancerArn: "lb", TargetGroupArn: "tg",
			EcsServiceArn: "svc", ListenerArnPort80: "l80", LoadBalancerDns: "lb.dns", CloudFrontDistributionID: "E1",
		}, want: map[parameters_enums.Key]string{
			parameters_enums.EcrRepositoryUri: "1.dkr.ecr/r", parameters_enums.AlbSecurityGroupId: "sg-1",
			parameters_enums.LoadBalancerArn: "lb", parameters_enums.TargetGroupArn: "tg", parameters_enums.EcsServiceArn: "svc",
		}},
		{name: "load balancer without target group", existing: agenttools.PreviewState{LoadBalancerArn: "lb", EcsServiceArn: "svc"},
			want: map[parameters_enums.Key]string{parameters_enums.EcsServiceArn: "svc"}},
		{name: "target group without load balancer", existing: agenttools.PreviewState{TargetGroupArn: "tg"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := testWebPreviewParametersInput("/tmp/orgns/b")
			in.Deploy.Existing = tt.existing
			p, err := buildWebPreviewParameters(in)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range []parameters_enums.Key{parameters_enums.EcrRepositoryUri, parameters_enums.AlbSecurityGroupId,
				parameters_enums.LoadBalancerArn, parameters_enums.TargetGroupArn, parameters_enums.EcsServiceArn} {
				got, ok := p[mustKey(t, k)]
				w, wantOK := tt.want[k]
				if ok != wantOK || ok && got != w {
					t.Errorf("%s = %v (set %v), want %q (set %v)", k, got, ok, w, wantOK)
				}
			}
		})
	}
}

func TestBuildWebPreviewParametersFreshMapEachTime(t *testing.T) {
	in := testWebPreviewParametersInput("/tmp/orgns/b")
	a, err := buildWebPreviewParameters(in)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetParameterValue(a, parameters_enums.EcrRepositoryUri, "set by a command")
	b, err := buildWebPreviewParameters(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b[mustKey(t, parameters_enums.EcrRepositoryUri)]; ok {
		t.Error("a second map carries what a command wrote into the first")
	}
	if len(in.Deploy.Service.DockerBuildArgs) != 3 || in.Deploy.Settings.Variables["API_KEY"] != "s3cret" {
		t.Error("building the map changed its input")
	}
}

func TestBuildWebPreviewParametersRefusesLineBreak(t *testing.T) {
	in := testWebPreviewParametersInput("/tmp/orgns/b")
	in.Deploy.Settings.Variables = map[string]string{"OK": "fine", "PRIVATE_KEY": "-----BEGIN\nsecretvalue"}
	p, err := buildWebPreviewParameters(in)
	want := "Preview configuration variable PRIVATE_KEY contains a line break; backend previews can't pass it."
	if err == nil || err.Error() != want || p != nil {
		t.Fatalf("got %v, %v; want %q", p, err, want)
	}
	if strings.Contains(err.Error(), "secretvalue") || strings.Contains(err.Error(), "BEGIN") {
		t.Error("the refusal shows the value")
	}
}

func TestWebPreviewDeploymentNameAndAlias(t *testing.T) {
	for key, want := range map[string]string{
		"acme-api":    "pv-acme-api",
		"Acme_API.v2": "pv-acme-api-v2",
		"acme-a-very-long-repository-name-services-api-gateway": "pv-acme-a-very-long-repository-name-serv",
	} {
		got := webPreviewDeploymentName(key)
		if got != want || len(got) > 40 {
			t.Errorf("webPreviewDeploymentName(%q) = %q (%d), want %q", key, got, len(got), want)
		}
		in := testWebPreviewParametersInput("/tmp/orgns/b")
		in.Deploy.ServiceKey = key
		p, err := buildWebPreviewParameters(in)
		if err != nil {
			t.Fatal(err)
		}
		alias, err := getInternalDnsName(p)
		if err != nil || alias != want+"-abcd" || len(alias) > 45 {
			t.Errorf("alias = %q, %v; want %q", alias, err, want+"-abcd")
		}
	}
}

func TestBuildWebPreviewParametersChecksTheNamespace(t *testing.T) {
	in := testWebPreviewParametersInput("/tmp/orgns/b")
	p, err := buildWebPreviewParameters(in)
	if err != nil {
		t.Fatal(err)
	}
	if ns, err := getNamespaceName(p); err != nil || ns != "previews-1b2c" {
		t.Errorf("namespace = %q, %v", ns, err)
	}
	in.Deploy.Settings.NamespaceName = "previews-zzzz"
	_, err = buildWebPreviewParameters(in)
	if err == nil || err.Error() != "The preview namespace doesn't match the server's (previews-1b2c vs previews-zzzz)." {
		t.Errorf("got %v", err)
	}
}

// fakeWebPreviewSteps records the steps a deploy runs.
type fakeWebPreviewSteps struct {
	mu    sync.Mutex
	calls []string
	logs  map[string]io.Writer
	fail  string // the step that fails
	seen  []map[string]interface{}
	// stackCreated says the stack group is new to this deploy; stackErr fails it.
	stackCreated bool
	stackErr     error
}

func (f *fakeWebPreviewSteps) step(name string) webPreviewStep {
	return func(p map[string]interface{}, logs io.Writer) (map[string]interface{}, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, name)
		f.logs[name] = logs
		f.seen = append(f.seen, p)
		if name == "upload" {
			jobs.SetParameterValue(p, parameters_enums.EcrRepositoryUri, "1.dkr.ecr/r")
		}
		if f.fail == name {
			return p, errors.New(name + " broke")
		}
		return p, nil
	}
}

func (f *fakeWebPreviewSteps) steps() webPreviewSteps {
	f.logs = map[string]io.Writer{}
	return webPreviewSteps{
		build: f.step("build"), upload: f.step("upload"), deploy: f.step("deploy"),
		stack: func(taskID string, _ map[string]interface{}, _ agenttools.WebServicePreviewDeployInput, port int64) (string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, fmt.Sprintf("stack %s %d", taskID, port))
			if f.stackErr != nil {
				return "", false, f.stackErr
			}
			return "sg-pvsg", f.stackCreated, nil
		},
		removeStack: func(_ map[string]interface{}, groupID string, _ io.Writer) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, "removeStack "+groupID)
		},
		loadBalancerDNS: func(p map[string]interface{}) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, "dns")
			if uri, _ := jobs.GetParameterValue[string](p, parameters_enums.EcrRepositoryUri); uri != "1.dkr.ecr/r" {
				return "", errors.New("the map the steps wrote into didn't reach the lookup")
			}
			return "alb-x.elb.amazonaws.com", nil
		},
	}
}

// webPreviewBuildDir is a build copy at /tmp/<org>/<task>-preview-build-1 holding the
// service's Dockerfile, and that org.
func webPreviewBuildDir(t *testing.T) (buildDir, org string) {
	t.Helper()
	orgDir, err := os.MkdirTemp("/tmp", "org")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(orgDir) })
	buildDir = filepath.Join(orgDir, "task-preview-build-1")
	if err := os.MkdirAll(filepath.Join(buildDir, "services/api/docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "services/api/docker/Dockerfile.prod"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	return buildDir, filepath.Base(orgDir)
}

func testWebPreviewDeployer(org string, steps webPreviewSteps) webPreviewDeployer {
	return webPreviewDeployer{organizationIDNamespace: org, organizationIdFromJob: testWebOrgID, region: 1, taskID: "task-1", steps: steps}
}

func TestWebPreviewDeployRunsProductionStepsInOrder(t *testing.T) {
	buildDir, org := webPreviewBuildDir(t)
	fake := &fakeWebPreviewSteps{}
	in := testWebPreviewInput(buildDir)
	var buildLogs, logs bytes.Buffer
	in.BuildLogs, in.Logs = &buildLogs, &logs
	out, err := testWebPreviewDeployer(org, fake.steps()).deploy(context.Background(), in)
	if err != nil || out.LoadBalancerDNS != "alb-x.elb.amazonaws.com" {
		t.Fatalf("got %+v, %v", out, err)
	}
	if got := strings.Join(fake.calls, ","); got != "stack task-1 0,build,upload,deploy,dns" {
		t.Errorf("steps = %s", got)
	}
	if fake.logs["build"] != &buildLogs || fake.logs["upload"] != &logs || fake.logs["deploy"] != &logs {
		t.Error("the build must write to the build log only, the other steps to the deploy log")
	}
	if reflect.ValueOf(fake.seen[0]).Pointer() != reflect.ValueOf(fake.seen[2]).Pointer() {
		t.Error("the steps don't share one map")
	}
	if dir, err := commandUtils.GetRepositoryDirectoryPath(fake.seen[0]); err != nil || dir != buildDir {
		t.Errorf("MarkDeploymentDone would remove %q (%v), want the build copy %s", dir, err, buildDir)
	}
	if b, err := os.ReadFile(filepath.Join(buildDir, "services/api/.env")); err != nil || string(b) != "A=b" {
		t.Errorf("secret file = %q, %v", b, err)
	}
}

// The names of build args left out go to the Step log only (BuildLogs), never a
// value and never the deploy log the agent's log tail is cut from.
func TestWebPreviewDeployLogsLeftOutBuildArgNames(t *testing.T) {
	buildDir, org := webPreviewBuildDir(t)
	fake := &fakeWebPreviewSteps{}
	in := testWebPreviewInput(buildDir)
	var buildLogs, logs bytes.Buffer
	in.BuildLogs, in.Logs = &buildLogs, &logs
	if _, err := testWebPreviewDeployer(org, fake.steps()).deploy(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	want := "Backend preview: build args left out, with no usable Preview configuration variable of the same name: A\n"
	if !strings.Contains(buildLogs.String(), want) {
		t.Errorf("build log %q, want it to contain %q", buildLogs.String(), want)
	}
	for _, out := range []string{buildLogs.String(), logs.String()} {
		if strings.Contains(out, "prod-") {
			t.Errorf("a production value was logged: %q", out)
		}
	}
	if strings.Contains(logs.String(), "build args left out") {
		t.Error("the left-out names reached the deploy log")
	}
	args, err := commandUtils.GetDockerBuildArgs(fake.seen[0])
	if err != nil || len(args) != 2 || args["API_KEY"] == nil || *args["API_KEY"] != "s3cret" ||
		args["URL"] == nil || *args["URL"] != "https://x/?a=b" {
		t.Errorf("the build got %v (%v), want only API_KEY and URL from the preview configuration", args, err)
	}
}

func TestWebPreviewDeployRefusesBeforeAnyStep(t *testing.T) {
	buildDir, org := webPreviewBuildDir(t)
	for _, tt := range []struct {
		name    string
		change  func(*agenttools.WebServicePreviewDeployInput, *webPreviewDeployer)
		wantErr string
	}{
		{name: "build dir that would resolve elsewhere", change: func(in *agenttools.WebServicePreviewDeployInput, _ *webPreviewDeployer) {
			in.BuildDir = filepath.Join(t.TempDir(), "task-preview-build-1")
		}, wantErr: "isn't where its deploy would clean up"},
		{name: "another org's directory", change: func(_ *agenttools.WebServicePreviewDeployInput, d *webPreviewDeployer) {
			d.organizationIDNamespace = "other"
		}, wantErr: "isn't where its deploy would clean up"},
		{name: "no Dockerfile", change: func(in *agenttools.WebServicePreviewDeployInput, _ *webPreviewDeployer) {
			in.Service.DockerFilePath = "Dockerfile"
		}, wantErr: `api's Dockerfile Dockerfile isn't in its root directory "services/api"`},
		{name: "Dockerfile outside the root directory", change: func(in *agenttools.WebServicePreviewDeployInput, _ *webPreviewDeployer) {
			if err := os.WriteFile(filepath.Join(buildDir, "services/Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
				t.Fatal(err)
			}
			in.Service.DockerFilePath = "../Dockerfile"
		}, wantErr: `api's Dockerfile ../Dockerfile isn't in its root directory "services/api"`},
		{name: "Dockerfile symlinked out of the root directory", change: func(in *agenttools.WebServicePreviewDeployInput, _ *webPreviewDeployer) {
			if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile.outside"), []byte("FROM scratch"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../Dockerfile.outside", filepath.Join(buildDir, "services/api/Dockerfile.link")); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
			in.Service.DockerFilePath = "Dockerfile.link"
		}, wantErr: `api's Dockerfile Dockerfile.link isn't in its root directory "services/api"`},
		{name: "line break", change: func(in *agenttools.WebServicePreviewDeployInput, _ *webPreviewDeployer) {
			in.Settings.Variables = map[string]string{"K": "a\nb"}
		}, wantErr: "Preview configuration variable K contains a line break"},
		{name: "namespace", change: func(in *agenttools.WebServicePreviewDeployInput, _ *webPreviewDeployer) {
			in.Settings.NamespaceName = "previews-other"
		}, wantErr: "The preview namespace doesn't match the server's"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeWebPreviewSteps{}
			d := testWebPreviewDeployer(org, fake.steps())
			in := testWebPreviewInput(buildDir)
			tt.change(&in, &d)
			_, err := d.deploy(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.HasPrefix(err.Error(), "The backend preview's deploy failed") {
				t.Fatalf("got %v, want the refusal %q", err, tt.wantErr)
			}
			if len(fake.calls) != 0 {
				t.Errorf("steps ran: %v", fake.calls)
			}
			if _, err := os.Stat(filepath.Join(buildDir, "services/api/.env")); !os.IsNotExist(err) {
				t.Error("secret files were written before the refusal")
			}
		})
	}
	if _, err := os.Stat(buildDir); err != nil {
		t.Errorf("a refusal touched the build copy: %v", err)
	}
}

func TestWebPreviewDeployStepFailure(t *testing.T) {
	buildDir, org := webPreviewBuildDir(t)
	fake := &fakeWebPreviewSteps{fail: "upload"}
	_, err := testWebPreviewDeployer(org, fake.steps()).deploy(context.Background(), testWebPreviewInput(buildDir))
	if err == nil || err.Error() != "The backend preview's deploy failed: upload broke" {
		t.Fatalf("got %v", err)
	}
	if got := strings.Join(fake.calls, ","); got != "stack task-1 0,build,upload" {
		t.Errorf("steps = %s", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake = &fakeWebPreviewSteps{}
	if _, err := testWebPreviewDeployer(org, fake.steps()).deploy(ctx, testWebPreviewInput(buildDir)); err == nil || len(fake.calls) != 0 {
		t.Errorf("a cancelled deploy: %v, steps %v", err, fake.calls)
	}
}

type fakeELB struct {
	names [][]string
}

func (f *fakeELB) DescribeLoadBalancers(_ context.Context, in *elasticloadbalancingv2.DescribeLoadBalancersInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error) {
	f.names = append(f.names, in.Names)
	return &elasticloadbalancingv2.DescribeLoadBalancersOutput{LoadBalancers: []elbTypes.LoadBalancer{{
		LoadBalancerName: aws.String(in.Names[0]), DNSName: aws.String(in.Names[0] + "-1.us-east-1.elb.amazonaws.com"),
	}}}, nil
}

func TestDescribeLoadBalancerDNSByName(t *testing.T) {
	for name, existing := range map[string]agenttools.PreviewState{
		"create": {},
		"reuse":  {LoadBalancerArn: "arn:lb", TargetGroupArn: "arn:tg", LoadBalancerDns: "stale.example"},
	} {
		in := testWebPreviewParametersInput("/tmp/orgns/b")
		in.Deploy.Existing = existing
		p, err := buildWebPreviewParameters(in)
		if err != nil {
			t.Fatal(err)
		}
		elb := &fakeELB{}
		dns, err := describeLoadBalancerDNS(elb, p)
		if err != nil || dns != "alb-"+testWebPreviewID+"-1.us-east-1.elb.amazonaws.com" {
			t.Errorf("%s: dns = %q, %v", name, dns, err)
		}
		if len(elb.names) != 1 || !reflect.DeepEqual(elb.names[0], []string{"alb-" + testWebPreviewID}) {
			t.Errorf("%s: described %v", name, elb.names)
		}
	}
}

// --- the skip-wait option -----------------------------------------------------

func TestWebServiceDeployEntryPointsOptions(t *testing.T) {
	original := runAwsWebServiceDeploy
	t.Cleanup(func() { runAwsWebServiceDeploy = original })
	var got []ecsServiceDeployOptions
	runAwsWebServiceDeploy = func(p map[string]interface{}, _ io.Writer, opts ecsServiceDeployOptions) (map[string]interface{}, error) {
		got = append(got, opts)
		return p, nil
	}
	if _, err := (&DeployAwsWebService{}).Run(map[string]interface{}{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := deployAwsWebServicePreview(map[string]interface{}{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []ecsServiceDeployOptions{{}, {skipServiceStableWait: true, previewStack: true, noCircuitBreaker: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options = %+v, want production waiting in the default group and the preview not waiting, in its stack group", got)
	}
	// And the options reach the service steps.
	steps := &fakeServiceSteps{shouldUpdate: true, preDeployEcsClient: &fakePreDeployEcs{}}
	stubServiceSteps(t, steps)
	if err := deployEcsServiceAfterPreDeploy(preDeployParameters(""), nil, &fakePreDeployLogs{},
		testClusterArn, "", testTaskDefinitionArn, io.Discard, ecsServiceDeployOptions{skipServiceStableWait: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps.opts, []ecsServiceDeployOptions{{skipServiceStableWait: true}, {skipServiceStableWait: true}}) {
		t.Errorf("service steps got %+v", steps.opts)
	}
}

// fakeEcsHTTP answers ECS's JSON API: UpdateService and DescribeServices.
type fakeEcsHTTP struct{}

func (fakeEcsHTTP) Do(req *http.Request) (*http.Response, error) {
	body := `{}`
	if strings.HasSuffix(req.Header.Get("X-Amz-Target"), ".DescribeServices") {
		body = `{"services":[{"serviceArn":"arn:aws:ecs:us-east-1:1:service/c/es-x"}]}`
	}
	if strings.HasSuffix(req.Header.Get("X-Amz-Target"), ".UpdateService") {
		body = `{"service":{"serviceArn":"arn:aws:ecs:us-east-1:1:service/c/es-x","deployments":[{"id":"ecs-svc/1","status":"PRIMARY"}]}}`
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func TestUpdateEcsServiceWaitsOnlyWithoutSkip(t *testing.T) {
	original := waitForEcsDeployment
	t.Cleanup(func() { waitForEcsDeployment = original })
	waits := 0
	waitForEcsDeployment = func(_ ecsServiceAPI, _ *ecs.DescribeServicesInput, deploymentID string, _ io.Writer) error {
		waits++
		if deploymentID != "ecs-svc/1" {
			t.Errorf("waited on deployment %q; want UpdateService's PRIMARY ecs-svc/1", deploymentID)
		}
		return nil
	}
	client := ecs.New(ecs.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: fakeEcsHTTP{}})
	p := map[string]interface{}{}
	jobs.SetParameterValue(p, parameters_enums.DeploymentID, "x")

	if err := updateEcsService(p, client, "arn:cluster", "arn:td", io.Discard, ecsServiceDeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Errorf("production update waited %d times, want 1", waits)
	}
	if err := updateEcsService(p, client, "arn:cluster", "arn:td", io.Discard, ecsServiceDeployOptions{skipServiceStableWait: true}); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Errorf("the preview's update waited (%d waits)", waits)
	}
	// An existing service from a prior deploy: createEcsServiceIfNeeded hands over
	// to the update without creating (or waiting) at all.
	jobs.SetParameterValue(p, parameters_enums.EcsServiceArn, "arn:svc")
	arn, update, err := createEcsServiceIfNeeded(p, client, "arn:cluster", "arn:tg", "arn:td", io.Discard, ecsServiceDeployOptions{skipServiceStableWait: true})
	if err != nil || arn != "arn:svc" || !update || waits != 1 {
		t.Errorf("got %q, %v, %v (waits %d)", arn, update, err, waits)
	}
}

// --- the store ----------------------------------------------------------------

func TestPreviewStoreMapsWebFields(t *testing.T) {
	reply := task_previews.EnsureTaskPreviewReplyV1{
		PreviewID: "p", ExistingDistID: "E1", ExistingDomain: "d1.cloudfront.net", DeploymentName: "task-preview-t-api",
		EcrRepositoryUri: "ecr", AlbSecurityGroupId: "sg", TargetGroupArn: "tg", ListenerArnPort80: "l80",
		LoadBalancerArn: "lb", LoadBalancerDns: "lb.dns", EcsServiceArn: "svc",
	}
	want := agenttools.PreviewState{
		CloudFrontDistributionID: "E1", CloudFrontDomainName: "d1.cloudfront.net", DeploymentName: "task-preview-t-api",
		EcrRepositoryUri: "ecr", AlbSecurityGroupId: "sg", TargetGroupArn: "tg", ListenerArnPort80: "l80",
		LoadBalancerArn: "lb", LoadBalancerDns: "lb.dns", EcsServiceArn: "svc",
	}
	if got := previewStateFromReply(reply); got != want {
		t.Errorf("state = %+v\nwant %+v", got, want)
	}
	dto := previewUpdateDto("p", agenttools.PreviewState{CloudFrontDistributionID: "E1", CloudFrontDistributionArn: "arn:E1", CloudFrontDomainName: "d1", EcsServiceArn: "svc"})
	if dto.ID != "p" || dto.CloudfrontDistributionID != "E1" || dto.CloudfrontDistributionArn != "arn:E1" ||
		dto.CloudfrontDistributionDomainName != "d1" || dto.EcsServiceArn != "svc" || dto.LoadBalancerArn != "" {
		t.Errorf("dto = %+v", dto)
	}
	deps := buildWebServicePreviewDeps(commandUtils.TaskJobContext{OrganizationID: "o", TaskID: "t"}, webPreviewJobParameters(t), t.TempDir(), io.Discard, nil)
	if store := deps.Store.(taskPreviewStore); store.serviceType != task_previews.ServiceTypeWebService {
		t.Errorf("the web store is bound to %q", store.serviceType)
	}
	// The preview database is a private service of the Task.
	if store := deps.DatabaseStore.(taskPreviewStore); store.serviceType != task_previews.ServiceTypePrivateService || store.taskID != "t" || store.orgID != "o" {
		t.Errorf("the database store is %+v", store)
	}
}

// --- registration ---------------------------------------------------------------

func webPreviewJobParameters(t *testing.T) map[string]interface{} {
	t.Helper()
	p := map[string]interface{}{}
	jobs.SetParameterValue(p, parameters_enums.OrganizationIDNamespace, "orgns")
	jobs.SetParameterValue(p, parameters_enums.OrganizationIdFromJob, testWebOrgID)
	return p
}

func toolNames(spec agentboxSpawnSpec) string {
	srv := agentmcp.New("test", "0")
	stop := registerAgentTools(srv, spec)
	defer stop()
	return strings.Join(srv.ToolNames(), ",")
}

func TestWebServicePreviewToolRegistration(t *testing.T) {
	workDir := t.TempDir()
	ctx := commandUtils.TaskJobContext{OrganizationID: "o", TaskID: "t"}
	tools := buildImplementPreviewTools(ctx, webPreviewJobParameters(t), workDir, io.Discard)
	if tools.web == nil || tools.web.Record != tools.static.Record || tools.web.Deploy == nil {
		t.Fatalf("web deps = %+v, want them sharing the static deps' record", tools.web)
	}

	implement := implementSpawnSpec("img", workDir, "cache", nil, tools)
	fix := (&reviewStage{ctx: ctx}).fixSpawnSpec("img", workDir, nil, tools)
	for name, spec := range map[string]agentboxSpawnSpec{"implement": implement, "fix": fix} {
		if got := toolNames(spec); !strings.Contains(got, "deploy_static_site_preview") || !strings.Contains(got, "deploy_web_service_preview") {
			t.Errorf("%s run tools = %s", name, got)
		}
	}

	stage, qaWorkDir := changedQAStage(t, nil)
	qa := stage.qaSpawnSpec("img", qaRoundDirPath(qaWorkDir, 1), nil, buildStaticSitePreviewDeps(stage.ctx, stage.parameters, qaWorkDir, io.Discard))
	production := agentboxSpawnSpec{productionTools: &productionQATools{record: productionQARecord(productionTestTargets)}}
	for name, spec := range map[string]agentboxSpawnSpec{"QA round": qa, "production QA": production} {
		if got := toolNames(spec); strings.Contains(got, "deploy_web_service_preview") {
			t.Errorf("%s tools = %s", name, got)
		}
	}
	// The QA stage builds the web deps to deploy backends itself, but never
	// hands them to a QA round's spawn; production QA doesn't build them at all.
	if qa.webPreviewDeps != nil {
		t.Error("a QA round's spawn carries the web deps")
	}
	src, err := os.ReadFile("run_qa_stage_round.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "webPreviewDeps:") || strings.Contains(string(src), "buildImplementPreviewTools") {
		t.Error("run_qa_stage_round.go puts the web deps on a spawn")
	}
	src, err = os.ReadFile("run_production_qa.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"webPreviewDeps", "buildImplementPreviewTools", "buildWebServicePreviewDeps"} {
		if strings.Contains(string(src), name) {
			t.Errorf("run_production_qa.go uses %s", name)
		}
	}
}
