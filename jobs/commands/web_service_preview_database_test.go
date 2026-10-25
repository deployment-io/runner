package commands

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const testDatabasePreviewID = "64f0c0ffee0123456789db01"

func testPreviewDatabase(engine, version string, variables ...string) *agenttools.PreviewDatabaseDeploy {
	e, _ := task_previews.LookupPreviewDatabaseEngine(engine)
	image, _ := e.Image(version)
	if version == "" {
		version = e.DefaultVersion
	}
	return &agenttools.PreviewDatabaseDeploy{
		PreviewID:  testDatabasePreviewID,
		ServiceKey: "acme-api-services-api-1a2b3c4d-db",
		PreviewDatabasePlan: agenttools.PreviewDatabasePlan{
			Engine: engine, Version: version, Image: image, Port: e.Port, Variables: variables, Source: "detected",
		},
		PreDeployCommand: "./migrate",
	}
}

func testPreviewDatabaseParametersInput(buildDir string) previewDatabaseParametersInput {
	in := testWebPreviewInput(buildDir)
	return previewDatabaseParametersInput{
		OrganizationIDNamespace: "orgns",
		OrganizationIdFromJob:   testWebOrgID,
		Region:                  1,
		Database:                *testPreviewDatabase("postgres", "", "DATABASE_URL"),
		Settings:                in.Settings,
		BuildDir:                buildDir,
		SecurityGroupIDs:        []string{"sg-pvsg"},
	}
}

func TestBuildPreviewDatabaseParametersKeysAndTypes(t *testing.T) {
	p, err := buildPreviewDatabaseParameters(testPreviewDatabaseParametersInput("/tmp/orgns/preview-db-123"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[parameters_enums.Key]interface{}{
		parameters_enums.OrganizationIDNamespace:    "orgns",
		parameters_enums.OrganizationIdFromJob:      testWebOrgID,
		parameters_enums.DeploymentID:               testDatabasePreviewID,
		parameters_enums.DeploymentName:             "pv-acme-api-services-api-1a2b3c4d-db",
		parameters_enums.EnvironmentName:            "previews",
		parameters_enums.EnvironmentID:              testWebOrgID,
		parameters_enums.BuildID:                    "preview-db-123",
		parameters_enums.Region:                     int64(1),
		parameters_enums.Port:                       int64(5432),
		parameters_enums.Cpu:                        "256",
		parameters_enums.Memory:                     "512",
		parameters_enums.VpcID:                      "vpc-1",
		parameters_enums.VpcCidr:                    "10.0.0.0/16",
		parameters_enums.PrivateSubnets:             primitive.A{"subnet-c"},
		parameters_enums.EcsClusterArn:              "arn:cluster",
		parameters_enums.EcsTaskExecutionRoleArn:    "arn:role",
		parameters_enums.EnvironmentVariables:       "POSTGRES_DB=preview\nPOSTGRES_PASSWORD=preview\nPOSTGRES_USER=preview\n",
		parameters_enums.DockerRepositoryUriWithTag: "public.ecr.aws/docker/library/postgres:16",
		parameters_enums.TaskSecurityGroupIds:       primitive.A{"sg-pvsg"},
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
	for _, never := range []parameters_enums.Key{
		parameters_enums.TaskDefinitionArn, parameters_enums.IsPreview, parameters_enums.RestartOnly,
		parameters_enums.JobID, parameters_enums.PreDeployCommand, parameters_enums.IsPrivateRegistry,
		parameters_enums.SecretName, parameters_enums.RepoProviderToken,
	} {
		if _, ok := p[mustKey(t, never)]; ok {
			t.Errorf("%s is set", never)
		}
	}
	// What the commands read it with.
	if groups, err := getTaskSecurityGroupIds(p); err != nil || !reflect.DeepEqual(groups, []string{"sg-pvsg"}) {
		t.Errorf("groups = %v, %v", groups, err)
	}
	if dir, err := commandUtils.GetRepositoryDirectoryPath(p); err != nil || dir != "/tmp/orgns/preview-db-123" {
		t.Errorf("repository directory = %q, %v", dir, err)
	}
	if host, err := previewDatabaseHost(p, "previews-1b2c"); err != nil || host != "pv-acme-api-services-api-1a2b3c4d-db-db01.previews-1b2c" {
		t.Errorf("host = %q, %v", host, err)
	}
}

func TestBuildPreviewDatabaseParametersPerEngine(t *testing.T) {
	for _, tc := range []struct {
		engine, version, image, memory, env string
		port                                int64
	}{
		{"mysql", "8.0", "public.ecr.aws/docker/library/mysql:8.0", "1024",
			"MYSQL_DATABASE=preview\nMYSQL_PASSWORD=preview\nMYSQL_RANDOM_ROOT_PASSWORD=yes\nMYSQL_USER=preview\n", 3306},
		{"mongodb", "", "public.ecr.aws/docker/library/mongo:7.0", "1024",
			"MONGO_INITDB_DATABASE=preview\nMONGO_INITDB_ROOT_PASSWORD=preview\nMONGO_INITDB_ROOT_USERNAME=preview\n", 27017},
	} {
		in := testPreviewDatabaseParametersInput("/tmp/orgns/b")
		in.Database = *testPreviewDatabase(tc.engine, tc.version, "DATABASE_URL")
		p, err := buildPreviewDatabaseParameters(in)
		if err != nil {
			t.Fatal(err)
		}
		if paramValue(t, p, parameters_enums.DockerRepositoryUriWithTag) != tc.image || paramValue(t, p, parameters_enums.Memory) != tc.memory ||
			paramValue(t, p, parameters_enums.Port) != tc.port || paramValue(t, p, parameters_enums.EnvironmentVariables) != tc.env {
			t.Errorf("%s: %v", tc.engine, p)
		}
	}
}

func TestBuildPreviewDatabaseParametersRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*previewDatabaseParametersInput)
		wantErr string
	}{
		{"unlisted engine", func(in *previewDatabaseParametersInput) { in.Database.Engine = "redis" }, `"redis" isn't a preview database engine`},
		{"unlisted version", func(in *previewDatabaseParametersInput) { in.Database.Version = "12" }, "postgres 12 is not a supported"},
		{"another image", func(in *previewDatabaseParametersInput) { in.Database.Image = "evil/postgres:16" }, "isn't the allowlisted public.ecr.aws/docker/library/postgres:16"},
		{"no security group", func(in *previewDatabaseParametersInput) { in.SecurityGroupIDs = nil }, "no security group"},
		{"namespace", func(in *previewDatabaseParametersInput) { in.Settings.NamespaceName = "previews-zzzz" }, "The preview namespace doesn't match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testPreviewDatabaseParametersInput("/tmp/orgns/b")
			tc.change(&in)
			if _, err := buildPreviewDatabaseParameters(in); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// The database deploy's build directory is a fresh empty one under
// /tmp/<OrganizationIDNamespace>/, and the map's repository directory is it.
func TestNewPreviewDatabaseParametersBuildDir(t *testing.T) {
	orgDir, err := os.MkdirTemp("/tmp", "org")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(orgDir) })
	api := map[string]interface{}{}
	jobs.SetParameterValue(api, parameters_enums.OrganizationIDNamespace, filepath.Base(orgDir))
	jobs.SetParameterValue(api, parameters_enums.OrganizationIdFromJob, testWebOrgID)
	jobs.SetParameterValue(api, parameters_enums.Region, int64(1))
	in := testWebPreviewInput("/unused")
	in.Database = testPreviewDatabase("postgres", "", "DATABASE_URL")
	first, err := newPreviewDatabaseParameters(api, in, "sg-pvsg")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newPreviewDatabaseParameters(api, in, "sg-pvsg")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := commandUtils.GetRepositoryDirectoryPath(first)
	other, _ := commandUtils.GetRepositoryDirectoryPath(second)
	if filepath.Dir(dir) != orgDir || dir == other {
		t.Errorf("build dirs %s and %s; want two fresh directories in %s", dir, other, orgDir)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("build dir %s: %v, %v", dir, entries, err)
	}
	if reflect.ValueOf(first).Pointer() == reflect.ValueOf(second).Pointer() || reflect.ValueOf(first).Pointer() == reflect.ValueOf(api).Pointer() {
		t.Error("the database must deploy on a fresh map")
	}
	// A refused map leaves no directory behind.
	in.Database.Image = "evil/postgres:16"
	if _, err := newPreviewDatabaseParameters(api, in, "sg-pvsg"); err == nil {
		t.Fatal("want a refusal")
	}
	if entries, _ := os.ReadDir(orgDir); len(entries) != 2 {
		t.Errorf("%d directories in %s, want 2", len(entries), orgDir)
	}
}

func TestPreviewDatabaseVariableValues(t *testing.T) {
	host := "pv-api-db-db01.previews-1b2c"
	names := []string{"DATABASE_URL", "DB_HOST", "DB_PORT", "DB_USER", "DB_USERNAME", "DB_PASSWORD", "DB_PASS", "DB_NAME",
		"DB_DATABASE", "MONGO_DB", "PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE"}
	for engine, url := range map[string]string{
		"postgres": "postgresql://preview:preview@" + host + ":5432/preview",
		"mysql":    "mysql://preview:preview@" + host + ":3306/preview",
		"mongodb":  "mongodb://preview:preview@" + host + ":27017/preview?authSource=admin",
	} {
		e, _ := task_previews.LookupPreviewDatabaseEngine(engine)
		got := previewDatabaseVariableValues(engine, host, e.Port, names)
		port := map[string]string{"postgres": "5432", "mysql": "3306", "mongodb": "27017"}[engine]
		want := map[string]string{
			"DATABASE_URL": url, "DB_HOST": host, "DB_PORT": port, "DB_USER": "preview", "DB_USERNAME": "preview",
			"DB_PASSWORD": "preview", "DB_PASS": "preview", "DB_NAME": "preview", "DB_DATABASE": "preview", "MONGO_DB": "preview",
			"PGHOST": host, "PGPORT": port, "PGUSER": "preview", "PGPASSWORD": "preview", "PGDATABASE": "preview",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v\nwant %v", engine, got, want)
		}
	}
	init := map[string]map[string]string{
		"postgres": {"POSTGRES_USER": "preview", "POSTGRES_PASSWORD": "preview", "POSTGRES_DB": "preview"},
		"mysql":    {"MYSQL_USER": "preview", "MYSQL_PASSWORD": "preview", "MYSQL_DATABASE": "preview", "MYSQL_RANDOM_ROOT_PASSWORD": "yes"},
		"mongodb":  {"MONGO_INITDB_ROOT_USERNAME": "preview", "MONGO_INITDB_ROOT_PASSWORD": "preview", "MONGO_INITDB_DATABASE": "preview"},
	}
	for engine, want := range init {
		if got := previewDatabaseInitVariables(engine); !reflect.DeepEqual(got, want) {
			t.Errorf("%s init variables = %v", engine, got)
		}
	}
}

// --- re-deploys ---------------------------------------------------------------

type fakePreviewDatabaseECS struct {
	calls       []string
	service     *ecsTypes.Service
	image       string
	describeErr error
}

func (f *fakePreviewDatabaseECS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	f.calls = append(f.calls, "DescribeServices "+strings.Join(in.Services, ","))
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.service == nil {
		return &ecs.DescribeServicesOutput{Failures: []ecsTypes.Failure{{Reason: aws.String("MISSING")}}}, nil
	}
	return &ecs.DescribeServicesOutput{Services: []ecsTypes.Service{*f.service}}, nil
}

func (f *fakePreviewDatabaseECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	f.calls = append(f.calls, "DescribeTaskDefinition "+aws.ToString(in.TaskDefinition))
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &ecsTypes.TaskDefinition{
		ContainerDefinitions: []ecsTypes.ContainerDefinition{{Image: aws.String(f.image)}},
	}}, nil
}

func TestDeployPreviewDatabaseIfChanged(t *testing.T) {
	const image = "public.ecr.aws/docker/library/postgres:16"
	active := &ecsTypes.Service{Status: aws.String("ACTIVE"), TaskDefinition: aws.String("arn:td:3"),
		NetworkConfiguration: &ecsTypes.NetworkConfiguration{AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{SecurityGroups: []string{"sg-pvsg"}}}}
	otherGroup := *active
	otherGroup.NetworkConfiguration = &ecsTypes.NetworkConfiguration{AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{SecurityGroups: []string{"sg-gone"}}}
	for _, tc := range []struct {
		name       string
		ecs        *fakePreviewDatabaseECS
		wantDeploy bool
		wantErr    bool
	}{
		{"first deploy", &fakePreviewDatabaseECS{}, true, false},
		{"unchanged", &fakePreviewDatabaseECS{service: active, image: image}, false, false},
		{"version changed", &fakePreviewDatabaseECS{service: active, image: "public.ecr.aws/docker/library/postgres:15"}, true, false},
		{"engine changed", &fakePreviewDatabaseECS{service: active, image: "public.ecr.aws/docker/library/mysql:8.4"}, true, false},
		{"in an earlier stack group", &fakePreviewDatabaseECS{service: &otherGroup, image: image}, true, false},
		{"inactive", &fakePreviewDatabaseECS{service: &ecsTypes.Service{Status: aws.String("INACTIVE"), TaskDefinition: aws.String("arn:td:3")}, image: image}, true, false},
		{"describe fails", &fakePreviewDatabaseECS{describeErr: errors.New("ThrottlingException")}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalClient, originalDeploy := getPreviewDatabaseECSClient, runPreviewDatabaseDeploy
			t.Cleanup(func() { getPreviewDatabaseECSClient, runPreviewDatabaseDeploy = originalClient, originalDeploy })
			getPreviewDatabaseECSClient = func(map[string]interface{}) (previewDatabaseECSAPI, error) { return tc.ecs, nil }
			deployed := 0
			runPreviewDatabaseDeploy = func(p map[string]interface{}, _ io.Writer) (map[string]interface{}, error) {
				deployed++
				return p, nil
			}
			p, err := buildPreviewDatabaseParameters(testPreviewDatabaseParametersInput("/tmp/orgns/b"))
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			err = deployPreviewDatabaseIfChanged(p, image, "sg-pvsg", &logs)
			if (err != nil) != tc.wantErr || (deployed == 1) != tc.wantDeploy {
				t.Errorf("err %v, deployed %d times; want deploy %v", err, deployed, tc.wantDeploy)
			}
			if tc.ecs.calls[0] != "DescribeServices es-"+testDatabasePreviewID {
				t.Errorf("calls %v", tc.ecs.calls)
			}
			if !tc.wantDeploy && !tc.wantErr && !strings.Contains(logs.String(), "keeping the running "+image) {
				t.Errorf("logs %q", logs.String())
			}
		})
	}
}

// --- the API's map --------------------------------------------------------------

func TestSetWebPreviewDatabaseParameters(t *testing.T) {
	in := testWebPreviewInput("/tmp/orgns/b")
	in.Database = testPreviewDatabase("postgres", "", "DATABASE_URL", "DIRECT_URL")
	p, err := buildWebPreviewParameters(webPreviewParametersInput{
		OrganizationIDNamespace: "orgns", OrganizationIdFromJob: testWebOrgID, Region: 1, Now: time.Unix(1700000000, 0), Deploy: in,
	})
	if err != nil {
		t.Fatal(err)
	}
	host := "pv-db-db01.previews-1b2c"
	if err := setWebPreviewDatabaseParameters(p, in, webPreviewDatabase{host: host, securityGroupID: "sg-pvsg", defaultSecurityGroupID: "sg-default"}); err != nil {
		t.Fatal(err)
	}
	url := "postgresql://preview:preview@" + host + ":5432/preview"
	wantEnv := "API_KEY=s3cret\nDATABASE_URL=" + url + "\nDIRECT_URL=" + url + "\nEMPTY=\nURL=https://x/?a=b\nZED=last\n"
	if got := paramValue(t, p, parameters_enums.EnvironmentVariables); got != wantEnv {
		t.Errorf("environment = %q\nwant %q", got, wantEnv)
	}
	if got := paramValue(t, p, parameters_enums.PreDeployCommand); got != "./migrate" {
		t.Errorf("PreDeployCommand = %v", got)
	}
	if got := paramValue(t, p, parameters_enums.TaskSecurityGroupIds); !reflect.DeepEqual(got, primitive.A{"sg-default", "sg-pvsg"}) {
		t.Errorf("TaskSecurityGroupIds = %v", got)
	}
	// The pre-deploy one-off task runs in the API's groups, so it reaches the database.
	runTask, err := getPreDeployRunTaskInput(p, "arn:cluster", "c", "./migrate")
	if err != nil || !reflect.DeepEqual(runTask.NetworkConfiguration.AwsvpcConfiguration.SecurityGroups, []string{"sg-default", "sg-pvsg"}) {
		t.Errorf("pre-deploy network = %+v, %v", runTask.NetworkConfiguration, err)
	}

	// A "use" setting's variable wins over Preview configuration's.
	in.Database.Variables = []string{"API_KEY"}
	in.Database.PreDeployCommand = ""
	p, _ = buildWebPreviewParameters(webPreviewParametersInput{OrganizationIDNamespace: "orgns", OrganizationIdFromJob: testWebOrgID, Region: 1, Deploy: in})
	if err := setWebPreviewDatabaseParameters(p, in, webPreviewDatabase{host: host, securityGroupID: "sg-pvsg", defaultSecurityGroupID: "sg-default"}); err != nil {
		t.Fatal(err)
	}
	if got := paramValue(t, p, parameters_enums.EnvironmentVariables).(string); !strings.Contains(got, "API_KEY="+url+"\n") {
		t.Errorf("environment = %q", got)
	}
	if _, ok := p[mustKey(t, parameters_enums.PreDeployCommand)]; ok {
		t.Error("PreDeployCommand set without one")
	}
}

// fakeDatabaseStep records the database step in the fake steps' order.
func (f *fakeWebPreviewSteps) databaseStep(fail error) func(string, map[string]interface{}, agenttools.WebServicePreviewDeployInput) (webPreviewDatabase, error) {
	return func(taskID string, _ map[string]interface{}, in agenttools.WebServicePreviewDeployInput) (webPreviewDatabase, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "database "+taskID+" "+in.Database.ServiceKey)
		if fail != nil {
			return webPreviewDatabase{}, fail
		}
		return webPreviewDatabase{host: "pv-db-db01.previews-1b2c", securityGroupID: "sg-pvsg", defaultSecurityGroupID: "sg-default"}, nil
	}
}

func TestWebPreviewDeployWithDatabase(t *testing.T) {
	buildDir, org := webPreviewBuildDir(t)
	fake := &fakeWebPreviewSteps{}
	steps := fake.steps()
	steps.database = fake.databaseStep(nil)
	d := testWebPreviewDeployer(org, steps)
	d.taskID = "task-1"
	in := testWebPreviewInput(buildDir)
	in.Database = testPreviewDatabase("postgres", "", "DATABASE_URL")
	if _, err := d.deploy(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.calls, ","); got != "database task-1 "+in.Database.ServiceKey+",build,upload,deploy,dns" {
		t.Errorf("steps = %s", got)
	}
	p := fake.seen[2]
	if got := paramValue(t, p, parameters_enums.TaskSecurityGroupIds); !reflect.DeepEqual(got, primitive.A{"sg-default", "sg-pvsg"}) {
		t.Errorf("TaskSecurityGroupIds = %v", got)
	}
	if paramValue(t, p, parameters_enums.PreDeployCommand) != "./migrate" ||
		!strings.Contains(paramValue(t, p, parameters_enums.EnvironmentVariables).(string), "DATABASE_URL=postgresql://preview:preview@pv-db-db01.previews-1b2c:5432/preview\n") {
		t.Errorf("the API's map = %v", p)
	}

	// The database failing fails the deploy before the API's steps.
	fake = &fakeWebPreviewSteps{}
	steps = fake.steps()
	steps.database = fake.databaseStep(errors.New("ECS said no"))
	_, err := testWebPreviewDeployer(org, steps).deploy(context.Background(), in)
	if err == nil || err.Error() != "The backend preview's deploy failed: the preview database: ECS said no" || len(fake.calls) != 1 {
		t.Errorf("got %v, steps %v", err, fake.calls)
	}
}

// Without a preview database the API's map is exactly as before: no pre-deploy
// command, no security groups, Preview configuration's variables only.
func TestWebPreviewDeployWithoutDatabaseIsUnchanged(t *testing.T) {
	buildDir, org := webPreviewBuildDir(t)
	fake := &fakeWebPreviewSteps{}
	steps := fake.steps()
	steps.database = fake.databaseStep(nil)
	in := testWebPreviewInput(buildDir)
	if _, err := testWebPreviewDeployer(org, steps).deploy(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.calls, ","); got != "build,upload,deploy,dns" {
		t.Errorf("steps = %s", got)
	}
	p := fake.seen[2]
	for _, never := range []parameters_enums.Key{parameters_enums.PreDeployCommand, parameters_enums.TaskSecurityGroupIds} {
		if _, ok := p[mustKey(t, never)]; ok {
			t.Errorf("%s is set without a preview database", never)
		}
	}
	if got := paramValue(t, p, parameters_enums.EnvironmentVariables); got != "API_KEY=s3cret\nEMPTY=\nURL=https://x/?a=b\nZED=last\n" {
		t.Errorf("environment = %q", got)
	}
}

// --- the network ----------------------------------------------------------------

func TestEcsNetworkConfigurationTaskSecurityGroups(t *testing.T) {
	p := privateServiceDeployParameters()
	network, err := getEcsServiceNetworkConfiguration(p)
	if err != nil || network.AwsvpcConfiguration.SecurityGroups != nil || !reflect.DeepEqual(network.AwsvpcConfiguration.Subnets, []string{"subnet-a"}) {
		t.Errorf("production network = %+v, %v; want no groups of its own", network.AwsvpcConfiguration, err)
	}
	ecsService := &fakeEcsService{service: &ecsTypes.Service{}}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	if err := updateEcsService(p, nil, testClusterArn, testTaskDefinitionArn, io.Discard, ecsServiceDeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if ecsService.updateInputs[0].NetworkConfiguration != nil {
		t.Error("a production update changed the service's network")
	}

	jobs.SetParameterValue(p, parameters_enums.TaskSecurityGroupIds, primitive.A{"sg-default", "sg-pvsg"})
	network, err = getEcsServiceNetworkConfiguration(p)
	if err != nil || !reflect.DeepEqual(network.AwsvpcConfiguration.SecurityGroups, []string{"sg-default", "sg-pvsg"}) {
		t.Errorf("preview network = %+v, %v", network.AwsvpcConfiguration, err)
	}
	if err := updateEcsService(p, nil, testClusterArn, testTaskDefinitionArn, io.Discard, ecsServiceDeployOptions{skipServiceStableWait: true}); err != nil {
		t.Fatal(err)
	}
	if got := ecsService.updateInputs[1].NetworkConfiguration; got == nil || !reflect.DeepEqual(got.AwsvpcConfiguration.SecurityGroups, []string{"sg-default", "sg-pvsg"}) {
		t.Errorf("the preview's update network = %+v", got)
	}
}

// A preview database whose engine changed updates its service with the new
// port's Service Connect mapping; every other update leaves it as it is.
func TestUpdateEcsServiceConnectOnlyForPreviewDatabase(t *testing.T) {
	p := privateServiceDeployParameters()
	ecsService := &fakeEcsService{service: &ecsTypes.Service{}}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	if err := updateEcsService(p, nil, testClusterArn, testTaskDefinitionArn, io.Discard, ecsServiceDeployOptions{registerDnsName: true}); err != nil {
		t.Fatal(err)
	}
	if ecsService.updateInputs[0].ServiceConnectConfiguration != nil {
		t.Error("a production update changed the service's Service Connect configuration")
	}
	jobs.SetParameterValue(p, parameters_enums.Port, int64(3306))
	if err := updateEcsService(p, nil, testClusterArn, testTaskDefinitionArn, io.Discard,
		ecsServiceDeployOptions{registerDnsName: true, updateServiceConnect: true}); err != nil {
		t.Fatal(err)
	}
	got := ecsService.updateInputs[1].ServiceConnectConfiguration
	want, _ := getPortMappingName(p)
	if got == nil || len(got.Services) != 1 || aws.ToString(got.Services[0].PortName) != want ||
		aws.ToInt32(got.Services[0].ClientAliases[0].Port) != 3306 || !strings.HasSuffix(want, "-3306") {
		t.Errorf("the preview database's Service Connect = %+v; want port mapping %s on 3306", got, want)
	}
}

// Only the preview database code sets TaskSecurityGroupIds: no production deploy
// ever runs in groups of its own.
func TestOnlyPreviewsSetTaskSecurityGroupIds(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetParameterValue" {
				return true
			}
			if key, ok := call.Args[1].(*ast.SelectorExpr); ok && key.Sel.Name == "TaskSecurityGroupIds" && file != "web_service_preview_database.go" {
				t.Errorf("%s sets TaskSecurityGroupIds", file)
			}
			return true
		})
	}
}

func TestPrivateServiceDeployEntryPointsOptions(t *testing.T) {
	original := runAwsPrivateServiceDeploy
	t.Cleanup(func() { runAwsPrivateServiceDeploy = original })
	var got []privateServiceDeployOptions
	runAwsPrivateServiceDeploy = func(p map[string]interface{}, _ io.Writer, opts privateServiceDeployOptions) (map[string]interface{}, error) {
		got = append(got, opts)
		return p, nil
	}
	if _, err := (&DeployAwsPrivateService{}).Run(map[string]interface{}{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := deployAwsPrivateServicePreview(map[string]interface{}{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []privateServiceDeployOptions{{addDefaultVpcIngressRule: true}, {updateServiceConnect: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options = %+v; want production editing the default group and the preview not", got)
	}
	if runPreviewDatabaseDeploy == nil || reflect.ValueOf(runPreviewDatabaseDeploy).Pointer() != reflect.ValueOf(deployAwsPrivateServicePreview).Pointer() {
		t.Error("the preview database must deploy through deployAwsPrivateServicePreview")
	}
}

// --- the stack security group -------------------------------------------------------

type fakeStackEC2 struct {
	calls        []string
	groups       []ec2Types.SecurityGroup // what DescribeSecurityGroups finds
	createErr    error
	authorizeErr error
	createInputs []*ec2.CreateSecurityGroupInput
	authorized   []*ec2.AuthorizeSecurityGroupIngressInput
	// createdByOther is a group a concurrent create made (found after DuplicateGroup).
	createdByOther *ec2Types.SecurityGroup
	deleteErr      error
}

func (f *fakeStackEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.calls = append(f.calls, "DeleteSecurityGroup "+aws.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, f.deleteErr
}

func (f *fakeStackEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	var filters []string
	for _, fl := range in.Filters {
		filters = append(filters, aws.ToString(fl.Name)+"="+strings.Join(fl.Values, ","))
	}
	f.calls = append(f.calls, "DescribeSecurityGroups "+strings.Join(filters, " "))
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.groups}, nil
}

func (f *fakeStackEC2) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	f.calls = append(f.calls, "CreateSecurityGroup "+aws.ToString(in.GroupName))
	f.createInputs = append(f.createInputs, in)
	if f.createErr != nil {
		if f.createdByOther != nil {
			f.groups = []ec2Types.SecurityGroup{*f.createdByOther}
		}
		return nil, f.createErr
	}
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-new")}, nil
}

func (f *fakeStackEC2) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.calls = append(f.calls, "AuthorizeSecurityGroupIngress "+aws.ToString(in.GroupId))
	f.authorized = append(f.authorized, in)
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, f.authorizeErr
}

func stackGroup(id string, selfPort int32) ec2Types.SecurityGroup {
	g := ec2Types.SecurityGroup{GroupId: aws.String(id), GroupName: aws.String("pvsg-task-1"), VpcId: aws.String("vpc-1")}
	if selfPort > 0 {
		g.IpPermissions = []ec2Types.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int32(selfPort), ToPort: aws.Int32(selfPort),
			UserIdGroupPairs: []ec2Types.UserIdGroupPair{{GroupId: aws.String(id)}},
		}}
	}
	return g
}

var testStack = previewStack{vpcID: "vpc-1", taskID: "task-1", port: 5432}

func TestEnsurePreviewStackSecurityGroupCreates(t *testing.T) {
	ec2Client := &fakeStackEC2{}
	id, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard)
	if err != nil || id != "sg-new" {
		t.Fatalf("got %q, %v", id, err)
	}
	want := []string{
		"DescribeSecurityGroups group-name=pvsg-task-1 vpc-id=vpc-1",
		"CreateSecurityGroup pvsg-task-1",
		"AuthorizeSecurityGroupIngress sg-new",
	}
	if !reflect.DeepEqual(ec2Client.calls, want) {
		t.Errorf("calls %v", ec2Client.calls)
	}
	in := ec2Client.createInputs[0]
	tags := map[string]string{}
	for _, tag := range in.TagSpecifications[0].Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if aws.ToString(in.VpcId) != "vpc-1" || tags["created by"] != "deployment.io" || tags["deployment.io preview task"] != "task-1" {
		t.Errorf("create input %+v, tags %v", in, tags)
	}
	permission := ec2Client.authorized[0].IpPermissions[0]
	if aws.ToInt32(permission.FromPort) != 5432 || aws.ToInt32(permission.ToPort) != 5432 || aws.ToString(permission.IpProtocol) != "tcp" ||
		len(permission.UserIdGroupPairs) != 1 || aws.ToString(permission.UserIdGroupPairs[0].GroupId) != "sg-new" ||
		len(permission.IpRanges) != 0 {
		t.Errorf("ingress %+v; want the port from the group itself only", permission)
	}
}

func TestEnsurePreviewStackSecurityGroupReuses(t *testing.T) {
	// Found with the rule: nothing to change.
	ec2Client := &fakeStackEC2{groups: []ec2Types.SecurityGroup{stackGroup("sg-1", 5432)}}
	if id, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err != nil || id != "sg-1" || len(ec2Client.calls) != 1 {
		t.Errorf("got %q, %v, calls %v", id, err, ec2Client.calls)
	}
	// Found without the rule for this port (the engine changed): the rule is added.
	ec2Client = &fakeStackEC2{groups: []ec2Types.SecurityGroup{stackGroup("sg-1", 3306)}}
	if id, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err != nil || id != "sg-1" || len(ec2Client.authorized) != 1 {
		t.Errorf("got %q, %v, calls %v", id, err, ec2Client.calls)
	}
	// A duplicate rule is already in place.
	ec2Client = &fakeStackEC2{groups: []ec2Types.SecurityGroup{stackGroup("sg-1", 0)}, authorizeErr: &smithy.GenericAPIError{Code: "InvalidPermission.Duplicate"}}
	if id, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err != nil || id != "sg-1" {
		t.Errorf("duplicate rule: got %q, %v", id, err)
	}
	// A group of that name in another VPC isn't this one.
	other := stackGroup("sg-other", 5432)
	other.VpcId = aws.String("vpc-2")
	ec2Client = &fakeStackEC2{groups: []ec2Types.SecurityGroup{other}}
	if id, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err != nil || id != "sg-new" {
		t.Errorf("another VPC's group: got %q, %v", id, err)
	}
}

func TestEnsurePreviewStackSecurityGroupRaceAndErrors(t *testing.T) {
	winner := stackGroup("sg-winner", 5432)
	ec2Client := &fakeStackEC2{createErr: &smithy.GenericAPIError{Code: "InvalidGroup.Duplicate"}, createdByOther: &winner}
	if id, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err != nil || id != "sg-winner" {
		t.Errorf("race: got %q, %v (calls %v)", id, err, ec2Client.calls)
	}
	ec2Client = &fakeStackEC2{createErr: &smithy.GenericAPIError{Code: "UnauthorizedOperation"}}
	if _, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err == nil {
		t.Error("a create error must fail")
	}
	ec2Client = &fakeStackEC2{groups: []ec2Types.SecurityGroup{stackGroup("sg-1", 0)}, authorizeErr: &smithy.GenericAPIError{Code: "RulesPerSecurityGroupLimitExceeded"}}
	if _, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err == nil {
		t.Error("an ingress error must fail")
	}
	if _, _, err := ensurePreviewStackSecurityGroup(&fakeStackEC2{}, previewStack{vpcID: "vpc-1", port: 5432}, io.Discard); err == nil {
		t.Error("no Task must fail")
	}
}

// A group this deploy created is removed again when the deploy fails before
// anything uses it: the deletes find the group only through the stack's services.
// A group that was already there, or one something already uses, is kept.
func TestPreviewStackGroupRemovedWhenTheDeployFailsFirst(t *testing.T) {
	// Created, but its rule can't be added.
	ec2Client := &fakeStackEC2{authorizeErr: &smithy.GenericAPIError{Code: "RulesPerSecurityGroupLimitExceeded"}}
	if _, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err == nil ||
		ec2Client.calls[len(ec2Client.calls)-1] != "DeleteSecurityGroup sg-new" {
		t.Errorf("err %v, calls %v", err, ec2Client.calls)
	}
	// Found, and its rule can't be added: not this deploy's to remove.
	ec2Client = &fakeStackEC2{groups: []ec2Types.SecurityGroup{stackGroup("sg-1", 0)}, authorizeErr: &smithy.GenericAPIError{Code: "RulesPerSecurityGroupLimitExceeded"}}
	if _, _, err := ensurePreviewStackSecurityGroup(ec2Client, testStack, io.Discard); err == nil || containsCall(ec2Client.calls, "DeleteSecurityGroup sg-1") {
		t.Errorf("err %v, calls %v", err, ec2Client.calls)
	}

	orgDir, err := os.MkdirTemp("/tmp", "org")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(orgDir) })
	api := map[string]interface{}{}
	jobs.SetParameterValue(api, parameters_enums.OrganizationIDNamespace, filepath.Base(orgDir))
	jobs.SetParameterValue(api, parameters_enums.OrganizationIdFromJob, testWebOrgID)
	jobs.SetParameterValue(api, parameters_enums.Region, int64(1))
	in := testWebPreviewInput("/unused")
	in.Database = testPreviewDatabase("postgres", "", "DATABASE_URL")
	originalClient, originalDeploy := getPreviewDatabaseECSClient, runPreviewDatabaseDeploy
	t.Cleanup(func() { getPreviewDatabaseECSClient, runPreviewDatabaseDeploy = originalClient, originalDeploy })
	getPreviewDatabaseECSClient = func(map[string]interface{}) (previewDatabaseECSAPI, error) { return &fakePreviewDatabaseECS{}, nil }
	deployErr := errors.New("RegisterTaskDefinition: AccessDenied")
	runPreviewDatabaseDeploy = func(map[string]interface{}, io.Writer) (map[string]interface{}, error) { return nil, deployErr }
	for _, tc := range []struct {
		name       string
		ec2        *fakeStackEC2
		wantDelete string
	}{
		{"created here", &fakeStackEC2{}, "DeleteSecurityGroup sg-new"},
		{"created here, already in use", &fakeStackEC2{deleteErr: &smithy.GenericAPIError{Code: "DependencyViolation"}}, "DeleteSecurityGroup sg-new"},
		{"already there", &fakeStackEC2{groups: []ec2Types.SecurityGroup{stackGroup("sg-1", 5432)}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			in.Logs = &logs
			if _, err := deployPreviewDatabaseInStack(tc.ec2, "task-1", api, in); !errors.Is(err, deployErr) {
				t.Fatalf("err %v", err)
			}
			last := tc.ec2.calls[len(tc.ec2.calls)-1]
			if gotDelete := strings.HasPrefix(last, "DeleteSecurityGroup"); gotDelete != (tc.wantDelete != "") || (gotDelete && last != tc.wantDelete) {
				t.Errorf("calls %v; want delete %q", tc.ec2.calls, tc.wantDelete)
			}
			if tc.ec2.deleteErr != nil && !strings.Contains(logs.String(), "something already uses it") {
				t.Errorf("logs:\n%s", logs.String())
			}
		})
	}
	// A deploy that succeeds keeps the group it created.
	runPreviewDatabaseDeploy = func(p map[string]interface{}, _ io.Writer) (map[string]interface{}, error) { return p, nil }
	ec2Client = &fakeStackEC2{}
	in.Logs = io.Discard
	db, err := deployPreviewDatabaseInStack(ec2Client, "task-1", api, in)
	if err != nil || db.securityGroupID != "sg-new" || db.host == "" || containsCall(ec2Client.calls, "DeleteSecurityGroup sg-new") {
		t.Errorf("db %+v, err %v, calls %v", db, err, ec2Client.calls)
	}
}

// --- deletes ----------------------------------------------------------------------

func taggedGroup(id string) ec2Types.SecurityGroup {
	return ec2Types.SecurityGroup{GroupId: aws.String(id), GroupName: aws.String("pvsg-task-1"),
		Tags: []ec2Types.Tag{{Key: aws.String("deployment.io preview task"), Value: aws.String("task-1")}}}
}

func stubSecurityGroupSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	original := previewSecurityGroupDeleteSleep
	t.Cleanup(func() { previewSecurityGroupDeleteSleep = original })
	var sleeps []time.Duration
	previewSecurityGroupDeleteSleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	return &sleeps
}

func previewStackFake() *fakeWebServiceAWS {
	return &fakeWebServiceAWS{
		serviceGroups: []string{"sg-default", "sg-pvsg", "sg-team"},
		groups: map[string]ec2Types.SecurityGroup{
			"sg-default": {GroupId: aws.String("sg-default"), GroupName: aws.String("default")},
			"sg-pvsg":    taggedGroup("sg-pvsg"),
			"sg-team":    {GroupId: aws.String("sg-team"), GroupName: aws.String("team"), Tags: []ec2Types.Tag{{Key: aws.String("created by"), Value: aws.String("deployment.io")}}},
		},
	}
}

// The web delete removes the stack's tagged group, last, and no other group.
func TestDeleteWebServiceRemovesTheStackGroupLast(t *testing.T) {
	stubSecurityGroupSleep(t)
	fake := previewStackFake()
	logs, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, productionDeleteCalls...),
		"DescribeSecurityGroups id=sg-default",
		"DescribeSecurityGroups id=sg-pvsg",
		"DeleteSecurityGroup sg-pvsg",
		"DescribeSecurityGroups id=sg-team",
	)
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(logs, "Deleting the preview's security group: sg-pvsg") {
		t.Errorf("logs:\n%s", logs)
	}
}

// A group the stack's other service still uses is retried every 15 s for 3
// minutes, then left to that service's delete, which still succeeds.
func TestDeletePreviewStackGroupStillInUse(t *testing.T) {
	sleeps := stubSecurityGroupSleep(t)
	fake := previewStackFake()
	inUse := &smithy.GenericAPIError{Code: "DependencyViolation"}
	fake.deleteGroupErrs = map[string][]error{"sg-pvsg": make([]error, 20)}
	for i := range fake.deleteGroupErrs["sg-pvsg"] {
		fake.deleteGroupErrs["sg-pvsg"][i] = inUse
	}
	logs, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
	if err != nil {
		t.Fatalf("the delete failed: %v", err)
	}
	deletes := 0
	for _, c := range fake.calls {
		if c == "DeleteSecurityGroup sg-pvsg" {
			deletes++
		}
	}
	if deletes != 13 || len(*sleeps) != 12 || (*sleeps)[0] != 15*time.Second {
		t.Errorf("%d deletes, sleeps %v; want 13 and 12 × 15s", deletes, *sleeps)
	}
	if !strings.Contains(logs, "Security group sg-pvsg is still in use; the preview's other service removes it") {
		t.Errorf("logs:\n%s", logs)
	}

	// Freed after two tries.
	*sleeps = nil
	fake = previewStackFake()
	fake.deleteGroupErrs = map[string][]error{"sg-pvsg": {inUse, inUse}}
	if _, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake); err != nil || len(*sleeps) != 2 {
		t.Errorf("err %v, sleeps %v", err, *sleeps)
	}

	// Already gone, or another error: logged, the delete finishes.
	for code, line := range map[string]string{
		"InvalidGroup.NotFound": "Security group sg-pvsg was already gone",
		"UnauthorizedOperation": "Couldn't delete security group sg-pvsg",
	} {
		fake = previewStackFake()
		fake.deleteGroupErrs = map[string][]error{"sg-pvsg": {&smithy.GenericAPIError{Code: code}}}
		logs, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
		if err != nil || !strings.Contains(logs, line) {
			t.Errorf("%s: err %v, logs:\n%s", code, err, logs)
		}
	}
}

// A service that can't be described before its delete isn't deleted: the stack
// group's id would be lost with it. The job fails, to be retried.
func TestDeleteWebServiceDescribeFailsDeletesNothing(t *testing.T) {
	stubSecurityGroupSleep(t)
	fake := previewStackFake()
	fake.errs = map[string]error{"DescribeServices": errors.New("ThrottlingException")}
	_, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
	if err == nil || !strings.Contains(err.Error(), "ThrottlingException") {
		t.Fatalf("err %v", err)
	}
	for _, c := range fake.calls {
		if strings.HasPrefix(c, "DeleteService") || strings.HasPrefix(c, "DeleteSecurityGroup") {
			t.Errorf("deleted after a failed describe: %v", fake.calls)
		}
	}
}

// A production service (in no group of its own) makes no security group call.
func TestDeleteWithoutStackGroupTouchesNoGroup(t *testing.T) {
	var deleted []string
	for _, groups := range [][]string{nil, {"sg-default"}} {
		fake := previewStackFake()
		fake.serviceGroups = groups
		if _, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake); err != nil {
			t.Fatal(err)
		}
		for _, c := range fake.calls {
			if strings.HasPrefix(c, "DeleteSecurityGroup") && c != "DeleteSecurityGroup sg-alb" {
				deleted = append(deleted, c)
			}
		}
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %v", deleted)
	}
}

func runPrivateServiceResourcesDelete(t *testing.T, parameters map[string]interface{}, fake *fakeWebServiceAWS) (string, error) {
	t.Helper()
	originalCloudMap := getServiceDiscoveryClient
	t.Cleanup(func() { getServiceDiscoveryClient = originalCloudMap })
	getServiceDiscoveryClient = func(map[string]interface{}) (serviceDiscoveryAPI, error) { return newFakeCloudMap(), nil }
	var logs bytes.Buffer
	err := deletePrivateServiceResources(parameters, deletePrivateServiceClients{ecs: fake, ecr: fake, ec2: fake}, &logs)
	return logs.String(), err
}

// A preview database's delete finishes though its ECR repository never existed,
// and removes the stack group after everything else.
func TestDeletePrivateServicePreviewDatabase(t *testing.T) {
	stubSecurityGroupSleep(t)
	fake := previewStackFake()
	fake.serviceGroups = []string{"sg-pvsg"}
	fake.errs = map[string]error{"DeleteRepository": &ecrTypes.RepositoryNotFoundException{Message: aws.String("none")}}
	logs, err := runPrivateServiceResourcesDelete(t, webServiceDeleteParameters(), fake)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []string{
		"DescribeServices es-deployment-1",
		"DeleteService es-deployment-1",
		"DescribeServices es-deployment-1",
		"ListTaskDefinitions td-deployment-1",
		"ListTaskDefinitions td-deployment-1-pre-deploy",
		"DeleteRepository ecr-org-1-deployment-1",
		"DescribeSecurityGroups id=sg-pvsg",
		"DeleteSecurityGroup sg-pvsg",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(logs, "ECR repository ecr-org-1-deployment-1 was already gone") {
		t.Errorf("logs:\n%s", logs)
	}
}

// An ECS service that's already gone skips the inactive wait; other errors still fail.
func TestDeletePrivateServiceAlreadyGone(t *testing.T) {
	fake := &fakeWebServiceAWS{errs: map[string]error{"DeleteService": &ecsTypes.ServiceNotFoundException{Message: aws.String("gone")}}}
	logs, err := runPrivateServiceResourcesDelete(t, webServiceDeleteParameters(), fake)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if fake.calls[0] != "DescribeServices es-deployment-1" || fake.calls[1] != "DeleteService es-deployment-1" || fake.calls[2] != "ListTaskDefinitions td-deployment-1" {
		t.Errorf("calls %v; want no inactive wait", fake.calls)
	}
	if !strings.Contains(logs, "ECS service es-deployment-1 was already gone") {
		t.Errorf("logs:\n%s", logs)
	}
	for _, operation := range []string{"DeleteService", "DeleteRepository"} {
		failure := &smithy.GenericAPIError{Code: "AccessDenied"}
		fake := &fakeWebServiceAWS{errs: map[string]error{operation: failure}}
		if _, err := runPrivateServiceResourcesDelete(t, webServiceDeleteParameters(), fake); !errors.Is(err, failure) {
			t.Errorf("%s: err = %v, want %v", operation, err, failure)
		}
	}
	// Deployed from an image: no repository call.
	parameters := webServiceDeleteParameters()
	jobs.SetParameterValue(parameters, parameters_enums.DeployedFromImage, true)
	fake = &fakeWebServiceAWS{}
	if _, err := runPrivateServiceResourcesDelete(t, parameters, fake); err != nil || containsCall(fake.calls, "DeleteRepository ecr-org-1-deployment-1") {
		t.Errorf("err %v, calls %v", err, fake.calls)
	}
}
