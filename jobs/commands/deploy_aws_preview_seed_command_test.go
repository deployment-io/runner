package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/agenttools"
)

const (
	testSeedCommand    = "npm run seed"
	testOwnerPassword  = "OwnerPassw0rdOwnerPassw0"
	testViewerPassword = "ViewerPassw0rdViewerPass"
)

func testPreviewSeed() *previewSeedRun {
	return &previewSeedRun{seed: agenttools.WebServicePreviewSeed{
		Command: testSeedCommand,
		Users: []agenttools.PreviewTestUser{
			{Role: "owner", Email: "qa-owner-6789abcd@example.com", Password: testOwnerPassword},
			{Role: "billing-admin", Email: "qa-billing-admin-6789abcd@example.com", Password: testViewerPassword},
		},
	}}
}

func previewSeedOptions(seed *previewSeedRun) ecsServiceDeployOptions {
	return ecsServiceDeployOptions{skipServiceStableWait: true, previewStack: true, noCircuitBreaker: true, previewSeed: seed}
}

// seedDeploy runs deployEcsServiceAfterPreDeploy with ecsClient, recording how
// many tasks had been started when the service was created.
type seedDeploy struct {
	ecsClient          *fakePreDeployEcs
	logsClient         *fakePreDeployLogs
	steps              *fakeServiceSteps
	runTasksAtCreation []int
	logs               bytes.Buffer
}

func runSeedDeploy(t *testing.T, parameters map[string]interface{}, ecsClient *fakePreDeployEcs, opts ecsServiceDeployOptions) (*seedDeploy, error) {
	t.Helper()
	d := &seedDeploy{ecsClient: ecsClient, logsClient: &fakePreDeployLogs{}, steps: &fakeServiceSteps{preDeployEcsClient: ecsClient}}
	stubServiceSteps(t, d.steps)
	create := createEcsServiceStep
	createEcsServiceStep = func(p map[string]interface{}, c *ecs.Client, cluster, tg, td string, w io.Writer, o ecsServiceDeployOptions) (string, bool, error) {
		d.runTasksAtCreation = append(d.runTasksAtCreation, len(ecsClient.runTaskInputs))
		return create(p, c, cluster, tg, td, w, o)
	}
	err := deployEcsServiceAfterPreDeploy(parameters, nil, d.logsClient, testClusterArn, "", testTaskDefinitionArn, &d.logs, opts)
	return d, err
}

func environmentOf(in *ecs.RunTaskInput) map[string]string {
	env := map[string]string{}
	for _, kv := range in.Overrides.ContainerOverrides[0].Environment {
		env[aws.ToString(kv.Name)] = aws.ToString(kv.Value)
	}
	return env
}

func TestPreviewSeedRunsAfterTheMigrateCommandBeforeTheService(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	seed := testPreviewSeed()
	d, err := runSeedDeploy(t, preDeployParameters(testCommand), ecsClient, previewSeedOptions(seed))
	if err != nil {
		t.Fatal(err)
	}
	if len(ecsClient.runTaskInputs) != 2 {
		t.Fatalf("RunTask called %d times; want the migrate command, then the seed", len(ecsClient.runTaskInputs))
	}
	migrate, seeded := ecsClient.runTaskInputs[0], ecsClient.runTaskInputs[1]
	if got := migrate.Overrides.ContainerOverrides[0].Command; !reflect.DeepEqual(got, []string{testCommand}) {
		t.Errorf("first task's command = %v; want the migrate command", got)
	}
	if got := seeded.Overrides.ContainerOverrides[0].Command; !reflect.DeepEqual(got, []string{testSeedCommand}) {
		t.Errorf("second task's command = %v; want the seed command", got)
	}
	if !reflect.DeepEqual(d.runTasksAtCreation, []int{2}) {
		t.Errorf("tasks started when the service was created = %v; want both", d.runTasksAtCreation)
	}
	if seed.outcome != agenttools.PreviewSeedSucceeded {
		t.Errorf("outcome = %v; want succeeded", seed.outcome)
	}
	logs := d.logs.String()
	for _, want := range []string{"Running the preview migrate command\n", "Running the preview seed command\n",
		"Preview seed command finished (exit code 0) in "} {
		if !strings.Contains(logs, want) {
			t.Errorf("deploy logs =\n%s\nwant %q", logs, want)
		}
	}
	if strings.Index(logs, "Running the preview migrate command") > strings.Index(logs, "Running the preview seed command") {
		t.Errorf("the seed ran before the migrate command:\n%s", logs)
	}
	if strings.Contains(logs, testSeedCommand) {
		t.Errorf("the deploy logs carry the seed command itself:\n%s", logs)
	}
	if len(ecsClient.deregisterInputs) != 2 {
		t.Errorf("deregistered %d task definitions; want both one-off ones", len(ecsClient.deregisterInputs))
	}
}

func TestPreviewSeedTaskEnvironment(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	if _, err := runSeedDeploy(t, preDeployParameters(testCommand), ecsClient, previewSeedOptions(testPreviewSeed())); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"QA_ROLES":                       "owner billing-admin",
		"QA_ROLE_OWNER_EMAIL":            "qa-owner-6789abcd@example.com",
		"QA_ROLE_OWNER_PASSWORD":         testOwnerPassword,
		"QA_ROLE_BILLING_ADMIN_EMAIL":    "qa-billing-admin-6789abcd@example.com",
		"QA_ROLE_BILLING_ADMIN_PASSWORD": testViewerPassword,
	}
	if got := environmentOf(ecsClient.runTaskInputs[1]); !reflect.DeepEqual(got, want) {
		t.Errorf("seed task environment = %v; want %v", got, want)
	}
	if got := ecsClient.runTaskInputs[0].Overrides.ContainerOverrides[0].Environment; len(got) != 0 {
		t.Errorf("the migrate task got environment overrides %v", got)
	}
	for _, in := range ecsClient.registerInputs {
		registered, _ := json.Marshal(in)
		for _, secret := range []string{"QA_ROLE", testOwnerPassword, testViewerPassword} {
			if strings.Contains(string(registered), secret) {
				t.Errorf("a registered task definition carries %q: %s", secret, registered)
			}
		}
	}

	// No roles: QA_ROLES is "" and there's nothing else.
	fakePreDeployClock(t, time.Second)
	ecsClient = &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	seed := &previewSeedRun{seed: agenttools.WebServicePreviewSeed{Command: testSeedCommand}}
	if _, err := runSeedDeploy(t, preDeployParameters(""), ecsClient, previewSeedOptions(seed)); err != nil {
		t.Fatal(err)
	}
	if got := environmentOf(ecsClient.runTaskInputs[0]); !reflect.DeepEqual(got, map[string]string{"QA_ROLES": ""}) {
		t.Errorf("no roles: environment = %v", got)
	}
}

func TestPreviewMigrateFailureStopsBeforeTheSeed(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(1), "")}}
	seed := testPreviewSeed()
	d, err := runSeedDeploy(t, preDeployParameters(testCommand), ecsClient, previewSeedOptions(seed))
	if err == nil || !strings.HasPrefix(err.Error(), "The preview migrate command failed with exit code 1.") {
		t.Errorf("error = %v; want the migrate command's failure", err)
	}
	if len(ecsClient.runTaskInputs) != 1 || len(d.steps.created) != 0 || seed.outcome != agenttools.PreviewSeedNotRun {
		t.Errorf("RunTask %d times, created %v, outcome %v; want the deploy stopped before the seed",
			len(ecsClient.runTaskInputs), d.steps.created, seed.outcome)
	}
}

func TestPreviewSeedFailureStillDeploysTheService(t *testing.T) {
	for _, tt := range []struct {
		name       string
		migrate    string
		ecsClient  *fakePreDeployEcs
		clockStep  time.Duration
		wantPrefix string
	}{
		{name: "exit 1", migrate: testCommand, clockStep: time.Second,
			ecsClient:  &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), ""), stoppedTask(aws.Int32(1), "")}},
			wantPrefix: "The preview seed command failed with exit code 1."},
		{name: "couldn't start", clockStep: time.Second,
			ecsClient:  &fakePreDeployEcs{runTaskErr: errors.New("AccessDeniedException")},
			wantPrefix: "The preview seed command couldn't start: AccessDeniedException."},
		{name: "task definition", clockStep: time.Second,
			ecsClient:  &fakePreDeployEcs{registerErr: errors.New("ClientException")},
			wantPrefix: "The preview seed command couldn't start: ClientException."},
		{name: "timeout", clockStep: 4 * time.Minute,
			ecsClient:  &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}},
			wantPrefix: "The preview seed command didn't finish within 30 minutes and was stopped."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakePreDeployClock(t, tt.clockStep)
			seed := testPreviewSeed()
			d, err := runSeedDeploy(t, preDeployParameters(tt.migrate), tt.ecsClient, previewSeedOptions(seed))
			if err != nil {
				t.Fatalf("a failed seed stopped the deploy: %v", err)
			}
			if len(d.steps.created) != 1 {
				t.Errorf("created %v; want the service created anyway", d.steps.created)
			}
			if seed.outcome != agenttools.PreviewSeedFailed {
				t.Errorf("outcome = %v; want failed", seed.outcome)
			}
			want := tt.wantPrefix + " The preview has no test users; the deploy goes on.\n"
			if logs := d.logs.String(); strings.Count(logs, want) != 1 || strings.Contains(logs, "The deploy was stopped") {
				t.Errorf("deploy logs =\n%s\nwant %q once", logs, want)
			}
		})
	}
}

func TestPreviewSeedNotRun(t *testing.T) {
	restart := preDeployParameters("")
	jobs.SetParameterValue(restart, parameters_enums.RestartOnly, true)
	for _, tt := range []struct {
		name       string
		parameters map[string]interface{}
		seed       *previewSeedRun
		opts       func(*previewSeedRun) ecsServiceDeployOptions
	}{
		{"no seed", preDeployParameters(""), nil, previewSeedOptions},
		{"no seed command", preDeployParameters(""), &previewSeedRun{}, previewSeedOptions},
		{"blank seed command", preDeployParameters(""), &previewSeedRun{seed: agenttools.WebServicePreviewSeed{Command: "  "}}, previewSeedOptions},
		{"restart only", restart, testPreviewSeed(), previewSeedOptions},
		{"production deploy", preDeployParameters(""), testPreviewSeed(), func(seed *previewSeedRun) ecsServiceDeployOptions {
			return ecsServiceDeployOptions{previewSeed: seed}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakePreDeployClock(t, time.Second)
			ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
			d, err := runSeedDeploy(t, tt.parameters, ecsClient, tt.opts(tt.seed))
			if err != nil {
				t.Fatal(err)
			}
			if len(ecsClient.runTaskInputs) != 0 || d.logs.Len() != 0 || len(d.steps.created) != 1 {
				t.Errorf("RunTask %d times, logs %q, created %v; want only the service", len(ecsClient.runTaskInputs), d.logs.String(), d.steps.created)
			}
			if tt.seed != nil && tt.seed.outcome != agenttools.PreviewSeedNotRun {
				t.Errorf("outcome = %v; want not run", tt.seed.outcome)
			}
		})
	}
}

func TestPreviewSeedOutputHidesPasswords(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	d := &seedDeploy{ecsClient: ecsClient, steps: &fakeServiceSteps{preDeployEcsClient: ecsClient}}
	stubServiceSteps(t, d.steps)
	logsClient := &fakePreDeployLogs{pages: [][]string{{"created owner with " + testOwnerPassword, "viewer:" + testViewerPassword + "!"}}}
	err := deployEcsServiceAfterPreDeploy(preDeployParameters(""), nil, logsClient, testClusterArn, "", testTaskDefinitionArn, &d.logs,
		previewSeedOptions(testPreviewSeed()))
	if err != nil {
		t.Fatal(err)
	}
	logs := d.logs.String()
	for _, want := range []string{"created owner with <QA_ROLE_OWNER_PASSWORD>\n", "viewer:<QA_ROLE_BILLING_ADMIN_PASSWORD>!\n"} {
		if !strings.Contains(logs, want) {
			t.Errorf("deploy logs =\n%s\nwant %q", logs, want)
		}
	}
	if strings.Contains(logs, testOwnerPassword) || strings.Contains(logs, testViewerPassword) {
		t.Errorf("a password reached the deploy logs:\n%s", logs)
	}
}

// A password the output's line split would cut in two is hidden first.
func TestPreviewSeedOutputHidesPasswordsInLongLines(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	d := &seedDeploy{ecsClient: ecsClient, steps: &fakeServiceSteps{preDeployEcsClient: ecsClient}}
	stubServiceSteps(t, d.steps)
	long := strings.Repeat("x", preDeployMaxLogLineLength-12) + testOwnerPassword
	logsClient := &fakePreDeployLogs{pages: [][]string{{long}}}
	err := deployEcsServiceAfterPreDeploy(preDeployParameters(""), nil, logsClient, testClusterArn, "", testTaskDefinitionArn, &d.logs,
		previewSeedOptions(testPreviewSeed()))
	if err != nil {
		t.Fatal(err)
	}
	logs := d.logs.String()
	// The split may fall inside <QA_ROLE_OWNER_PASSWORD>, which hides nothing.
	if !strings.Contains(strings.ReplaceAll(logs, "\n", ""), "<QA_ROLE_OWNER_PASSWORD>") || strings.Contains(logs, testOwnerPassword[:12]) ||
		strings.Contains(logs, testOwnerPassword[12:]) {
		t.Errorf("deploy logs hold part of the password or not its name:\n%s", logs[len(logs)-200:])
	}
}

// Deploy logs that stop taking writes don't hold the deploy at the seed.
func TestPreviewSeedNotHeldByBlockedLogs(t *testing.T) {
	original := preDeployLogWriteTimeout
	t.Cleanup(func() { preDeployLogWriteTimeout = original })
	preDeployLogWriteTimeout = 10 * time.Millisecond
	for _, exitCode := range []int32{0, 1} {
		fakePreDeployClock(t, time.Second)
		ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(exitCode), "")}}
		steps := &fakeServiceSteps{preDeployEcsClient: ecsClient}
		stubServiceSteps(t, steps)
		writer := &blockedWriter{release: make(chan struct{})}
		seed := testPreviewSeed()
		done := make(chan error, 1)
		go func() {
			done <- deployEcsServiceAfterPreDeploy(preDeployParameters(""), nil,
				&fakePreDeployLogs{pages: [][]string{{"seeded"}}}, testClusterArn, "", testTaskDefinitionArn, writer, previewSeedOptions(seed))
		}()
		select {
		case err := <-done:
			if err != nil || len(steps.created) != 1 {
				t.Errorf("exit %d: error = %v, created %v; want the service created", exitCode, err, steps.created)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("exit %d: the deploy is held by the blocked deploy logs", exitCode)
		}
		close(writer.release)
	}
}

// Roles whose variables would share a name don't get a seed that can't tell
// them apart; the deploy goes on.
func TestPreviewSeedRolesSharingAKey(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	seed := testPreviewSeed()
	seed.seed.Users = append(seed.seed.Users, agenttools.PreviewTestUser{Role: "billing_admin", Email: "qa-billing_admin-6789abcd@example.com", Password: "BillingPassw0rdBillingPa"})
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	d, err := runSeedDeploy(t, preDeployParameters(""), ecsClient, previewSeedOptions(seed))
	if err != nil {
		t.Fatal(err)
	}
	want := "The preview seed command didn't run: the roles billing-admin and billing_admin would both use " +
		"QA_ROLE_BILLING_ADMIN_EMAIL and QA_ROLE_BILLING_ADMIN_PASSWORD. The preview has no test users; the deploy goes on.\n"
	if len(ecsClient.runTaskInputs) != 0 || seed.outcome != agenttools.PreviewSeedFailed || len(d.steps.created) != 1 ||
		d.logs.String() != want {
		t.Errorf("RunTask %d times, outcome %v, created %v, logs %q; want only %q and the service",
			len(ecsClient.runTaskInputs), seed.outcome, d.steps.created, d.logs.String(), want)
	}
}
