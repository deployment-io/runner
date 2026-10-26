package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	testClusterArn        = "arn:aws:ecs:eu-west-1:123456789012:cluster/c"
	testTaskDefinitionArn = "arn:aws:ecs:eu-west-1:123456789012:task-definition/td-deployment-1:8"
	testTaskArn           = "arn:aws:ecs:eu-west-1:123456789012:task/c/0123456789abcdef"
	// The sibling task definition the pre-deploy command runs from.
	testPreDeployTaskDefinitionArn = "arn:aws:ecs:eu-west-1:123456789012:task-definition/td-deployment-1-pre-deploy:3"
	// Carries a secret inline, as a customer's command may.
	testCommand = "DATABASE_URL=postgres://u:hunter2@db/app npm run migrate"
)

type fakePreDeployEcs struct {
	runTaskInputs  []*ecs.RunTaskInput
	runTaskOutput  *ecs.RunTaskOutput
	runTaskErr     error
	describeTasks  []ecsTypes.Task // one per DescribeTasks call; the last repeats
	describeCalls  int
	stopTaskInputs []*ecs.StopTaskInput
	stopTaskErrs   int // StopTask fails this many times before it succeeds
	services       []ecsTypes.Service
	stalled        bool   // every call blocks until its request is given up
	stalledCall    string // only the call of this name blocks

	describeTaskDefinitionInputs []*ecs.DescribeTaskDefinitionInput
	describeTaskDefinitionErr    error
	registerInputs               []*ecs.RegisterTaskDefinitionInput
	registerErr                  error
	deregisterInputs             []*ecs.DeregisterTaskDefinitionInput
	deregisterErr                error
}

func (f *fakePreDeployEcs) stall(ctx context.Context) error {
	return f.stallCall(ctx, "")
}

func (f *fakePreDeployEcs) stallCall(ctx context.Context, name string) error {
	if !f.stalled && (len(name) == 0 || f.stalledCall != name) {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakePreDeployEcs) DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	f.describeTaskDefinitionInputs = append(f.describeTaskDefinitionInputs, in)
	if err := f.stallCall(ctx, "DescribeTaskDefinition"); err != nil {
		return nil, err
	}
	if f.describeTaskDefinitionErr != nil {
		return nil, f.describeTaskDefinitionErr
	}
	return testTaskDefinitionDescription(), nil
}

func (f *fakePreDeployEcs) RegisterTaskDefinition(ctx context.Context, in *ecs.RegisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	f.registerInputs = append(f.registerInputs, in)
	if err := f.stallCall(ctx, "RegisterTaskDefinition"); err != nil {
		return nil, err
	}
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: &ecsTypes.TaskDefinition{
		TaskDefinitionArn: aws.String(testPreDeployTaskDefinitionArn),
	}}, nil
}

func (f *fakePreDeployEcs) DeregisterTaskDefinition(ctx context.Context, in *ecs.DeregisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error) {
	f.deregisterInputs = append(f.deregisterInputs, in)
	if err := f.stallCall(ctx, "DeregisterTaskDefinition"); err != nil {
		return nil, err
	}
	if f.deregisterErr != nil {
		return nil, f.deregisterErr
	}
	return &ecs.DeregisterTaskDefinitionOutput{}, nil
}

// testTaskDefinitionDescription is the task definition registerTaskDefinition
// registers, as DescribeTaskDefinition returns it, with every field the
// pre-deploy copy takes set.
func testTaskDefinitionDescription() *ecs.DescribeTaskDefinitionOutput {
	return &ecs.DescribeTaskDefinitionOutput{
		TaskDefinition: &ecsTypes.TaskDefinition{
			TaskDefinitionArn:  aws.String(testTaskDefinitionArn),
			Family:             aws.String("td-deployment-1"),
			Revision:           8,
			Status:             ecsTypes.TaskDefinitionStatusActive,
			Compatibilities:    []ecsTypes.Compatibility{ecsTypes.CompatibilityEc2, ecsTypes.CompatibilityFargate},
			RequiresAttributes: []ecsTypes.Attribute{{Name: aws.String("com.amazonaws.ecs.capability.logging-driver.awslogs")}},
			ContainerDefinitions: []ecsTypes.ContainerDefinition{
				{
					Name:        aws.String("c-deployment-1"),
					Image:       aws.String("123456789012.dkr.ecr.eu-west-1.amazonaws.com/app:build-1"),
					Essential:   aws.Bool(true),
					Environment: []ecsTypes.KeyValuePair{{Name: aws.String("PORT"), Value: aws.String("3000")}},
					LogConfiguration: &ecsTypes.LogConfiguration{
						LogDriver: ecsTypes.LogDriverAwslogs,
						Options:   map[string]string{"awslogs-group": "org-1/deployment-1", "awslogs-stream-prefix": "application/build-1"},
					},
				},
				{
					Name:       aws.String("sidecar"),
					Image:      aws.String("sidecar:1"),
					EntryPoint: []string{"/sidecar"},
					Command:    []string{"--serve"},
				},
			},
			Cpu:                     aws.String("256"),
			Memory:                  aws.String("512"),
			ExecutionRoleArn:        aws.String("arn:aws:iam::123456789012:role/execution"),
			TaskRoleArn:             aws.String("arn:aws:iam::123456789012:role/task"),
			NetworkMode:             ecsTypes.NetworkModeAwsvpc,
			RequiresCompatibilities: []ecsTypes.Compatibility{ecsTypes.CompatibilityFargate},
			RuntimePlatform: &ecsTypes.RuntimePlatform{
				CpuArchitecture:       ecsTypes.CPUArchitectureArm64,
				OperatingSystemFamily: ecsTypes.OSFamilyLinux,
			},
			EphemeralStorage:      &ecsTypes.EphemeralStorage{SizeInGiB: 30},
			InferenceAccelerators: []ecsTypes.InferenceAccelerator{{DeviceName: aws.String("d"), DeviceType: aws.String("t")}},
			Volumes:               []ecsTypes.Volume{{Name: aws.String("scratch")}},
			PlacementConstraints: []ecsTypes.TaskDefinitionPlacementConstraint{{
				Type:       ecsTypes.TaskDefinitionPlacementConstraintTypeMemberOf,
				Expression: aws.String("attribute:ecs.os-type == linux"),
			}},
			PidMode:            ecsTypes.PidModeTask,
			IpcMode:            ecsTypes.IpcModeTask,
			ProxyConfiguration: &ecsTypes.ProxyConfiguration{ContainerName: aws.String("sidecar")},
		},
		Tags: []ecsTypes.Tag{
			{Key: aws.String("Name"), Value: aws.String("td-deployment-1")},
			{Key: aws.String("created by"), Value: aws.String("deployment.io")},
		},
	}
}

func (f *fakePreDeployEcs) RunTask(ctx context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.runTaskInputs = append(f.runTaskInputs, in)
	if err := f.stallCall(ctx, "RunTask"); err != nil {
		return nil, err
	}
	if f.runTaskErr != nil {
		return nil, f.runTaskErr
	}
	if f.runTaskOutput != nil {
		return f.runTaskOutput, nil
	}
	return &ecs.RunTaskOutput{Tasks: []ecsTypes.Task{{TaskArn: aws.String(testTaskArn), LastStatus: aws.String("PROVISIONING")}}}, nil
}

func (f *fakePreDeployEcs) DescribeTasks(ctx context.Context, _ *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	if err := f.stall(ctx); err != nil {
		return nil, err
	}
	i := f.describeCalls
	if i >= len(f.describeTasks) {
		i = len(f.describeTasks) - 1
	}
	f.describeCalls++
	return &ecs.DescribeTasksOutput{Tasks: []ecsTypes.Task{f.describeTasks[i]}}, nil
}

func (f *fakePreDeployEcs) StopTask(ctx context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.stopTaskInputs = append(f.stopTaskInputs, in)
	if err := f.stall(ctx); err != nil {
		return nil, err
	}
	if len(f.stopTaskInputs) <= f.stopTaskErrs {
		return nil, errors.New("ServerException: try again")
	}
	return &ecs.StopTaskOutput{}, nil
}

func (f *fakePreDeployEcs) DescribeServices(ctx context.Context, _ *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if err := f.stall(ctx); err != nil {
		return nil, err
	}
	return &ecs.DescribeServicesOutput{Services: f.services}, nil
}

// fakePreDeployLogs serves pages of the stream; before streamAfter calls the
// stream doesn't exist yet.
type fakePreDeployLogs struct {
	streamAfter int
	pages       [][]string
	calls       int
	inputs      []*cloudwatchlogs.GetLogEventsInput
}

func (f *fakePreDeployLogs) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.calls++
	f.inputs = append(f.inputs, in)
	if f.calls <= f.streamAfter {
		return nil, &cwTypes.ResourceNotFoundException{Message: aws.String("The specified log stream does not exist.")}
	}
	page := 0
	if in.NextToken != nil {
		page = int(aws.ToString(in.NextToken)[0] - '0')
	}
	output := &cloudwatchlogs.GetLogEventsOutput{NextForwardToken: aws.String(string(rune('0' + page)))}
	if page < len(f.pages) {
		for _, line := range f.pages[page] {
			output.Events = append(output.Events, cwTypes.OutputLogEvent{Message: aws.String(line)})
		}
		output.NextForwardToken = aws.String(string(rune('0' + page + 1)))
	}
	return output, nil
}

func preDeployParameters(command string) map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "deployment-1")
	jobs.SetParameterValue(parameters, parameters_enums.BuildID, "build-1")
	jobs.SetParameterValue(parameters, parameters_enums.PrivateSubnets, primitive.A{"subnet-a", "subnet-b"})
	jobs.SetParameterValue(parameters, parameters_enums.EcsServiceArn, "arn:aws:ecs:eu-west-1:123456789012:service/c/es-deployment-1")
	if len(command) > 0 {
		jobs.SetParameterValue(parameters, parameters_enums.PreDeployCommand, command)
	}
	return parameters
}

func stoppedTask(exitCode *int32, stoppedReason string) ecsTypes.Task {
	return ecsTypes.Task{
		TaskArn:       aws.String(testTaskArn),
		LastStatus:    aws.String("STOPPED"),
		StoppedReason: aws.String(stoppedReason),
		Containers:    []ecsTypes.Container{{Name: aws.String("c-deployment-1"), ExitCode: exitCode}},
	}
}

func runningTask() ecsTypes.Task {
	return ecsTypes.Task{TaskArn: aws.String(testTaskArn), LastStatus: aws.String("RUNNING")}
}

// fakePreDeployClock advances by step on every reading and never sleeps.
func fakePreDeployClock(t *testing.T, step time.Duration) {
	t.Helper()
	originalNow, originalSleep := preDeployNow, preDeploySleep
	t.Cleanup(func() { preDeployNow, preDeploySleep = originalNow, originalSleep })
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	preDeployNow = func() time.Time {
		now = now.Add(step)
		return now
	}
	preDeploySleep = func(time.Duration) {}
}

func runPreDeploy(t *testing.T, parameters map[string]interface{}, ecsClient *fakePreDeployEcs,
	logsClient *fakePreDeployLogs) (string, error) {
	t.Helper()
	var logs bytes.Buffer
	err := runPreDeployCommand(parameters, ecsClient, logsClient, testClusterArn, testTaskDefinitionArn, &logs)
	if strings.Contains(logs.String(), testCommand) || strings.Contains(logs.String(), "hunter2") {
		t.Errorf("the deploy logs carry the command itself:\n%s", logs.String())
	}
	if err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error carries the command itself: %v", err)
	}
	for _, in := range ecsClient.registerInputs {
		if registered, _ := json.Marshal(in); strings.Contains(string(registered), "hunter2") {
			t.Errorf("a registered task definition carries the command itself: %s", registered)
		}
	}
	return logs.String(), err
}

func TestPreDeployRunTaskInput(t *testing.T) {
	parameters := preDeployParameters(testCommand)
	input, err := getPreDeployRunTaskInput(parameters, testClusterArn, "c-deployment-1", testCommand)
	if err != nil {
		t.Fatalf("getPreDeployRunTaskInput: %v", err)
	}
	if aws.ToString(input.Cluster) != testClusterArn {
		t.Errorf("Cluster = %q; want %q", aws.ToString(input.Cluster), testClusterArn)
	}
	if input.LaunchType != ecsTypes.LaunchTypeFargate {
		t.Errorf("LaunchType = %q; want FARGATE", input.LaunchType)
	}
	if aws.ToInt32(input.Count) != 1 {
		t.Errorf("Count = %d; want 1", aws.ToInt32(input.Count))
	}
	serviceNetwork, err := getEcsServiceNetworkConfiguration(parameters)
	if err != nil {
		t.Fatalf("getEcsServiceNetworkConfiguration: %v", err)
	}
	if !reflect.DeepEqual(input.NetworkConfiguration, serviceNetwork) {
		t.Errorf("NetworkConfiguration = %+v; want the service's %+v", input.NetworkConfiguration.AwsvpcConfiguration,
			serviceNetwork.AwsvpcConfiguration)
	}
	if got := input.NetworkConfiguration.AwsvpcConfiguration.Subnets; !reflect.DeepEqual(got, []string{"subnet-a", "subnet-b"}) {
		t.Errorf("Subnets = %v; want the private subnets", got)
	}
	overrides := input.Overrides.ContainerOverrides
	if len(overrides) != 1 || aws.ToString(overrides[0].Name) != "c-deployment-1" {
		t.Fatalf("ContainerOverrides = %+v; want one for c-deployment-1", overrides)
	}
	// The pre-deploy task definition's entrypoint, sh -c, takes it as one word.
	if want := []string{testCommand}; !reflect.DeepEqual(overrides[0].Command, want) {
		t.Errorf("Command = %q; want %q", overrides[0].Command, want)
	}
}

func TestPreDeployLogStreamName(t *testing.T) {
	got := getPreDeployLogStreamName("application/build-1", "c-deployment-1", testTaskArn)
	if want := "application/build-1/c-deployment-1/0123456789abcdef"; got != want {
		t.Errorf("stream = %q; want %q", got, want)
	}
}

func TestPreDeployExitZeroContinues(t *testing.T) {
	fakePreDeployClock(t, 10*time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask(), stoppedTask(aws.Int32(0), "Essential container in task exited")}}
	logsClient := &fakePreDeployLogs{streamAfter: 1, pages: [][]string{{"migrating 001", "migrating 002\n"}, {"done"}}}
	logs, err := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, logsClient)
	if err != nil {
		t.Fatalf("exit code 0 must continue the deploy: %v", err)
	}
	if len(ecsClient.runTaskInputs) != 1 {
		t.Fatalf("RunTask called %d times; want 1", len(ecsClient.runTaskInputs))
	}
	if len(ecsClient.stopTaskInputs) != 0 {
		t.Error("a finished task must not be stopped")
	}
	if len(ecsClient.describeTaskDefinitionInputs) != 1 || len(ecsClient.registerInputs) != 1 {
		t.Fatalf("DescribeTaskDefinition called %d times, RegisterTaskDefinition %d; want 1 each",
			len(ecsClient.describeTaskDefinitionInputs), len(ecsClient.registerInputs))
	}
	described := ecsClient.describeTaskDefinitionInputs[0]
	if aws.ToString(described.TaskDefinition) != testTaskDefinitionArn ||
		!reflect.DeepEqual(described.Include, []ecsTypes.TaskDefinitionField{ecsTypes.TaskDefinitionFieldTags}) {
		t.Errorf("described %q including %v; want %q with its tags", aws.ToString(described.TaskDefinition), described.Include,
			testTaskDefinitionArn)
	}
	runTask := ecsClient.runTaskInputs[0]
	if aws.ToString(runTask.TaskDefinition) != testPreDeployTaskDefinitionArn {
		t.Errorf("RunTask TaskDefinition = %q; want the pre-deploy %q", aws.ToString(runTask.TaskDefinition),
			testPreDeployTaskDefinitionArn)
	}
	if got := runTask.Overrides.ContainerOverrides[0].Command; !reflect.DeepEqual(got, []string{testCommand}) {
		t.Errorf("RunTask Command = %q; want [the command]", got)
	}
	assertPreDeployTaskDefinitionDeregistered(t, ecsClient)
	want := "Running the pre-deploy command\nmigrating 001\nmigrating 002\ndone\nPre-deploy command finished (exit code 0) in "
	if !strings.HasPrefix(logs, want) {
		t.Errorf("deploy logs =\n%s\nwant them to start with\n%s", logs, want)
	}
	if !strings.HasSuffix(logs, "s\n") {
		t.Errorf("deploy logs must end with the duration: %q", logs)
	}
	stream := "application/build-1/c-deployment-1/0123456789abcdef"
	for _, in := range logsClient.inputs {
		if aws.ToString(in.LogGroupName) != "org-1/deployment-1" || aws.ToString(in.LogStreamName) != stream {
			t.Errorf("read %s %s; want org-1/deployment-1 %s", aws.ToString(in.LogGroupName), aws.ToString(in.LogStreamName), stream)
		}
	}
}

func TestPreDeployMissingLogStreamIsNoOutput(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	logsClient := &fakePreDeployLogs{streamAfter: 100}
	logs, err := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, logsClient)
	if err != nil {
		t.Fatalf("a missing log stream must not fail the deploy: %v", err)
	}
	if strings.Contains(logs, "Couldn't read") || strings.Contains(logs, "does not exist") {
		t.Errorf("a missing log stream must read as no output:\n%s", logs)
	}
	if logsClient.calls < 2 {
		t.Errorf("the stream must be read again after the task stops; read %d times", logsClient.calls)
	}
}

func TestPreDeployFailures(t *testing.T) {
	for _, tt := range []struct {
		name          string
		ecsClient     *fakePreDeployEcs
		firstDeploy   bool
		want          string
		wantStopTask  bool
		wantFinishLog bool
	}{
		{
			name:          "non-zero exit",
			ecsClient:     &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(3), "Essential container in task exited")}},
			want:          "The pre-deploy command failed with exit code 3. The deploy was stopped; the previous version keeps running.",
			wantFinishLog: true,
		},
		{
			name: "RunTask started no task",
			ecsClient: &fakePreDeployEcs{runTaskOutput: &ecs.RunTaskOutput{Failures: []ecsTypes.Failure{{
				Reason: aws.String("RESOURCE:ENI"),
			}}}},
			want: "The pre-deploy command couldn't start: RESOURCE:ENI. The deploy was stopped; the previous version keeps running.",
		},
		{
			name: "no exit code",
			ecsClient: &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(nil,
				"CannotPullContainerError: pull image manifest has been retried 1 time(s)")}},
			want: "The pre-deploy command couldn't run: CannotPullContainerError: pull image manifest has been retried 1 time(s). " +
				"The deploy was stopped; the previous version keeps running.",
		},
		{
			name:         "timeout",
			ecsClient:    &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}},
			want:         "The pre-deploy command didn't finish within 30 minutes and was stopped. The deploy was stopped; the previous version keeps running.",
			wantStopTask: true,
		},
		{
			name:          "first deploy",
			ecsClient:     &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(1), "")}},
			firstDeploy:   true,
			want:          "The pre-deploy command failed with exit code 1. Nothing was deployed.",
			wantFinishLog: true,
		},
		{
			name:         "first deploy timeout",
			ecsClient:    &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}},
			firstDeploy:  true,
			want:         "The pre-deploy command didn't finish within 30 minutes and was stopped. Nothing was deployed.",
			wantStopTask: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakePreDeployClock(t, 4*time.Minute)
			parameters := preDeployParameters(testCommand)
			if tt.firstDeploy {
				delete(parameters, mustKey(t, parameters_enums.EcsServiceArn))
				tt.ecsClient.services = []ecsTypes.Service{{Status: aws.String("INACTIVE")}}
			}
			logs, err := runPreDeploy(t, parameters, tt.ecsClient, &fakePreDeployLogs{})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v; want %q", err, tt.want)
			}
			if stopped := len(tt.ecsClient.stopTaskInputs) == 1; stopped != tt.wantStopTask {
				t.Errorf("StopTask called %d times; want stopped = %v", len(tt.ecsClient.stopTaskInputs), tt.wantStopTask)
			}
			if tt.wantStopTask && aws.ToString(tt.ecsClient.stopTaskInputs[0].Task) != testTaskArn {
				t.Errorf("stopped %q; want %q", aws.ToString(tt.ecsClient.stopTaskInputs[0].Task), testTaskArn)
			}
			if finished := strings.Contains(logs, "Pre-deploy command finished (exit code"); finished != tt.wantFinishLog {
				t.Errorf("finish line in logs = %v; want %v:\n%s", finished, tt.wantFinishLog, logs)
			}
			assertPreDeployTaskDefinitionDeregistered(t, tt.ecsClient)
		})
	}
}

func TestPreDeployRunTaskErrorFails(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{runTaskErr: errors.New("AccessDeniedException: not authorized")}
	_, err := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{})
	want := "The pre-deploy command couldn't start: AccessDeniedException: not authorized. The deploy was stopped; the previous version keeps running."
	if err == nil || err.Error() != want {
		t.Errorf("error = %v; want %q", err, want)
	}
}

func TestPreDeploySkipped(t *testing.T) {
	restart := preDeployParameters(testCommand)
	jobs.SetParameterValue(restart, parameters_enums.RestartOnly, true)
	for name, parameters := range map[string]map[string]interface{}{
		"no parameter": preDeployParameters(""),
		"restart":      restart,
	} {
		ecsClient := &fakePreDeployEcs{}
		logs, err := runPreDeploy(t, parameters, ecsClient, &fakePreDeployLogs{})
		if err != nil || len(ecsClient.runTaskInputs) != 0 || len(logs) != 0 {
			t.Errorf("%s: err = %v, RunTask calls = %d, logs = %q; want a no-op", name, err, len(ecsClient.runTaskInputs), logs)
		}
	}
}

func mustKey(t *testing.T, k parameters_enums.Key) string {
	t.Helper()
	key, err := k.Key()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Both deploy commands hand the newly registered task definition to
// deployEcsServiceAfterPreDeploy (tested above with fakes) and return on its
// error, and don't create or update the service themselves. The rest of Run
// needs live AWS, so this pins it in the source. The web service's steps are
// deployAwsWebService, which its Run and the backend preview both run; the
// private service's are deployAwsPrivateService, which its Run and the preview
// database both run.
func TestDeployCommandsDeployTheServiceAfterThePreDeployCommand(t *testing.T) {
	for file, steps := range map[string]struct{ receiver, name string }{
		"deploy_aws_web_service.go":     {name: "deployAwsWebService"},
		"deploy_aws_private_service.go": {name: "deployAwsPrivateService"},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		var run *ast.FuncDecl
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != steps.name {
				continue
			}
			if steps.receiver == "" && fn.Recv == nil ||
				fn.Recv != nil && strings.Contains(receiverTypeName(fn.Recv.List[0].Type), steps.receiver) && steps.receiver != "" {
				run = fn
			}
		}
		if run == nil {
			t.Fatalf("%s: no %s %s", file, steps.receiver, steps.name)
		}

		position := map[string]int{}
		returnsOnError := false
		for i, stmt := range run.Body.List {
			ast.Inspect(stmt, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok {
						if _, seen := position[ident.Name]; !seen {
							position[ident.Name] = i
						}
					}
				}
				return true
			})
			if p, ok := position["deployEcsServiceAfterPreDeploy"]; ok && p == i && i+1 < len(run.Body.List) {
				returnsOnError = returnsWhenErrNotNil(run.Body.List[i+1])
			}
		}
		for _, name := range []string{"runPreDeployCommand", "createEcsServiceIfNeeded", "updateEcsService"} {
			if _, ok := position[name]; ok {
				t.Errorf("%s: Run calls %s itself; it must go through deployEcsServiceAfterPreDeploy", file, name)
			}
		}
		register, okRegister := position["registerTaskDefinition"]
		deploy, okDeploy := position["deployEcsServiceAfterPreDeploy"]
		if !okRegister || !okDeploy || register >= deploy {
			t.Errorf("%s: Run must call registerTaskDefinition, then deployEcsServiceAfterPreDeploy", file)
		}
		if !returnsOnError {
			t.Errorf("%s: Run must return when deployEcsServiceAfterPreDeploy's error isn't nil", file)
		}
	}
}

// returnsWhenErrNotNil is whether stmt is `if err != nil { return ... }`.
func returnsWhenErrNotNil(stmt ast.Stmt) bool {
	ifStmt, ok := stmt.(*ast.IfStmt)
	if !ok || ifStmt.Init != nil || len(ifStmt.Body.List) == 0 {
		return false
	}
	cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return false
	}
	x, okX := cond.X.(*ast.Ident)
	y, okY := cond.Y.(*ast.Ident)
	if !okX || !okY || x.Name != "err" || y.Name != "nil" {
		return false
	}
	_, ok = ifStmt.Body.List[0].(*ast.ReturnStmt)
	return ok
}

func receiverTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// fakeServiceSteps stands in for creating and updating the ECS service.
type fakeServiceSteps struct {
	created, updated   []string // task definition ARNs
	shouldUpdate       bool
	preDeployEcsClient *fakePreDeployEcs
	opts               []ecsServiceDeployOptions // what each step was called with
}

func stubServiceSteps(t *testing.T, steps *fakeServiceSteps) {
	t.Helper()
	originalClient, originalCreate, originalUpdate := preDeployEcsClient, createEcsServiceStep, updateEcsServiceStep
	t.Cleanup(func() {
		preDeployEcsClient, createEcsServiceStep, updateEcsServiceStep = originalClient, originalCreate, originalUpdate
	})
	preDeployEcsClient = func(*ecs.Client) preDeployEcsAPI { return steps.preDeployEcsClient }
	createEcsServiceStep = func(_ map[string]interface{}, _ *ecs.Client, _, _, taskDefinitionArn string,
		_ io.Writer, opts ecsServiceDeployOptions) (string, bool, error) {
		steps.created = append(steps.created, taskDefinitionArn)
		steps.opts = append(steps.opts, opts)
		return "", steps.shouldUpdate, nil
	}
	updateEcsServiceStep = func(_ map[string]interface{}, _ *ecs.Client, _, taskDefinitionArn string, _ io.Writer, opts ecsServiceDeployOptions) error {
		steps.updated = append(steps.updated, taskDefinitionArn)
		steps.opts = append(steps.opts, opts)
		return nil
	}
}

func TestDeployEcsServiceAfterPreDeploy(t *testing.T) {
	for _, tt := range []struct {
		name        string
		command     string
		ecsClient   *fakePreDeployEcs
		wantErr     bool
		wantRunTask int
	}{
		{name: "no pre-deploy command", ecsClient: &fakePreDeployEcs{}},
		{name: "exit 0", command: testCommand, wantRunTask: 1,
			ecsClient: &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}},
		{name: "non-zero exit", command: testCommand, wantRunTask: 1, wantErr: true,
			ecsClient: &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(2), "")}}},
		{name: "RunTask started no task", command: testCommand, wantRunTask: 1, wantErr: true,
			ecsClient: &fakePreDeployEcs{runTaskOutput: &ecs.RunTaskOutput{Failures: []ecsTypes.Failure{{Reason: aws.String("RESOURCE:ENI")}}}}},
		{name: "RunTask error", command: testCommand, wantRunTask: 1, wantErr: true,
			ecsClient: &fakePreDeployEcs{runTaskErr: errors.New("AccessDeniedException")}},
		{name: "no exit code", command: testCommand, wantRunTask: 1, wantErr: true,
			ecsClient: &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(nil, "CannotPullContainerError")}}},
		{name: "timeout", command: testCommand, wantRunTask: 1, wantErr: true,
			ecsClient: &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}}},
	} {
		for kind, targetGroupArn := range map[string]string{"web": "arn:aws:elasticloadbalancing:tg/web", "private": ""} {
			t.Run(tt.name+"/"+kind, func(t *testing.T) {
				fakePreDeployClock(t, 4*time.Minute)
				steps := &fakeServiceSteps{shouldUpdate: true, preDeployEcsClient: tt.ecsClient}
				tt.ecsClient.runTaskInputs = nil
				stubServiceSteps(t, steps)
				var logs bytes.Buffer
				err := deployEcsServiceAfterPreDeploy(preDeployParameters(tt.command), nil, &fakePreDeployLogs{},
					testClusterArn, targetGroupArn, testTaskDefinitionArn, &logs, ecsServiceDeployOptions{})
				if (err != nil) != tt.wantErr {
					t.Fatalf("error = %v; want error = %v", err, tt.wantErr)
				}
				if len(tt.ecsClient.runTaskInputs) != tt.wantRunTask {
					t.Errorf("RunTask called %d times; want %d", len(tt.ecsClient.runTaskInputs), tt.wantRunTask)
				}
				if tt.wantErr {
					if len(steps.created) != 0 || len(steps.updated) != 0 {
						t.Errorf("a failed pre-deploy command must leave the service alone; created %v, updated %v",
							steps.created, steps.updated)
					}
					return
				}
				want := []string{testTaskDefinitionArn}
				if !reflect.DeepEqual(steps.created, want) || !reflect.DeepEqual(steps.updated, want) {
					t.Errorf("created %v, updated %v; want the service moved to %v", steps.created, steps.updated, want)
				}
			})
		}
	}
}

func TestPreDeployStopTaskRetriedAndReported(t *testing.T) {
	fakePreDeployClock(t, 4*time.Minute)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}, stopTaskErrs: 1}
	_, err := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{})
	want := "The pre-deploy command didn't finish within 30 minutes and was stopped. The deploy was stopped; the previous version keeps running."
	if err == nil || err.Error() != want || len(ecsClient.stopTaskInputs) != 2 {
		t.Errorf("error = %v after %d StopTask calls; want %q after 2", err, len(ecsClient.stopTaskInputs), want)
	}

	ecsClient = &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}, stopTaskErrs: 100}
	_, err = runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{})
	want = "The pre-deploy command didn't finish within 30 minutes and couldn't be stopped: ServerException: try again. " +
		"The deploy was stopped; the previous version keeps running."
	if err == nil || err.Error() != want || len(ecsClient.stopTaskInputs) != preDeployStopTaskAttempts {
		t.Errorf("error = %v after %d StopTask calls; want %q after %d", err, len(ecsClient.stopTaskInputs), want,
			preDeployStopTaskAttempts)
	}
}

func TestPreDeployLogsReadPastEmptyPages(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(1), "")}}
	logsClient := &fakePreDeployLogs{pages: [][]string{{"migrating 001"}, {}, {}, {"error: relation exists"}}}
	logs, _ := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, logsClient)
	if !strings.Contains(logs, "migrating 001\nerror: relation exists\n") {
		t.Errorf("lines after an empty page are missing:\n%s", logs)
	}
}

// endlessPreDeployLogs always has another page, as a stream written faster
// than it's read.
type endlessPreDeployLogs struct{ calls int }

func (f *endlessPreDeployLogs) GetLogEvents(_ context.Context, _ *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.calls++
	return &cloudwatchlogs.GetLogEventsOutput{
		Events:           []cwTypes.OutputLogEvent{{Message: aws.String("line")}},
		NextForwardToken: aws.String(fmt.Sprint(f.calls)),
	}, nil
}

func TestPreDeployTimeoutNotHeldOffByEndlessOutput(t *testing.T) {
	fakePreDeployClock(t, 4*time.Minute)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}}
	logsClient := &endlessPreDeployLogs{}
	var logs bytes.Buffer
	err := runPreDeployCommand(preDeployParameters(testCommand), ecsClient, logsClient, testClusterArn, testTaskDefinitionArn, &logs)
	if err == nil || !strings.HasPrefix(err.Error(), "The pre-deploy command didn't finish within 30 minutes and was stopped.") {
		t.Fatalf("error = %v; want the timeout", err)
	}
	if len(ecsClient.stopTaskInputs) != 1 {
		t.Errorf("StopTask called %d times; want 1", len(ecsClient.stopTaskInputs))
	}
	// The read after the stop ends at its own deadline, one fake clock step on.
	if max := ecsClient.describeCalls*preDeployLogPagesPerPoll + 1; logsClient.calls > max {
		t.Errorf("read %d pages; want at most %d", logsClient.calls, max)
	}
	if !strings.Contains(logs.String(), "The pre-deploy command's output is incomplete") {
		t.Errorf("the unread output isn't reported:\n%s", logs.String())
	}
}

// finitePreDeployLogs has pages pages of one line each, then ends.
type finitePreDeployLogs struct {
	pages int
	calls int
}

func (f *finitePreDeployLogs) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.calls++
	page := 0
	if in.NextToken != nil {
		fmt.Sscan(aws.ToString(in.NextToken), &page)
	}
	if page >= f.pages {
		return &cloudwatchlogs.GetLogEventsOutput{NextForwardToken: in.NextToken}, nil
	}
	return &cloudwatchlogs.GetLogEventsOutput{
		Events:           []cwTypes.OutputLogEvent{{Message: aws.String(fmt.Sprintf("line %d", page))}},
		NextForwardToken: aws.String(fmt.Sprint(page + 1)),
	}, nil
}

func TestPreDeployLogsReadToTheEndAfterTheTaskStops(t *testing.T) {
	fakePreDeployClock(t, time.Millisecond)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(1), "")}}
	logsClient := &finitePreDeployLogs{pages: 5000}
	var logs bytes.Buffer
	runPreDeployCommand(preDeployParameters(testCommand), ecsClient, logsClient, testClusterArn, testTaskDefinitionArn, &logs)
	if !strings.Contains(logs.String(), "line 4999\n") || strings.Contains(logs.String(), "incomplete") {
		t.Errorf("the last lines are missing or reported missing:\n%s", logs.String()[logs.Len()-200:])
	}
}

// stalledPreDeployLogs never answers until its request is given up.
type stalledPreDeployLogs struct{}

func (f *stalledPreDeployLogs) GetLogEvents(ctx context.Context, _ *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// blockedWriter is a deploy log pipe nobody reads any more, after the first write.
type blockedWriter struct {
	writes  int
	release chan struct{}
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		<-w.release
	}
	return len(p), nil
}

func TestPreDeployTimeoutNotHeldOffByStalledLogs(t *testing.T) {
	fakePreDeployClock(t, 4*time.Minute)
	originalCall, originalWrite := preDeployLogCallTimeout, preDeployLogWriteTimeout
	t.Cleanup(func() { preDeployLogCallTimeout, preDeployLogWriteTimeout = originalCall, originalWrite })
	preDeployLogCallTimeout, preDeployLogWriteTimeout = 10*time.Millisecond, 10*time.Millisecond
	const timedOut = "The pre-deploy command didn't finish within 30 minutes and was stopped."

	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}}
	var logs bytes.Buffer
	err := runPreDeployCommand(preDeployParameters(testCommand), ecsClient, &stalledPreDeployLogs{}, testClusterArn,
		testTaskDefinitionArn, &logs)
	if err == nil || !strings.HasPrefix(err.Error(), timedOut) || len(ecsClient.stopTaskInputs) != 1 {
		t.Errorf("a stalled log read: error = %v after %d StopTask calls; want the timeout after 1", err,
			len(ecsClient.stopTaskInputs))
	}

	// "Running the pre-deploy command" gets through; the copied output blocks.
	ecsClient = &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}}
	writer := &blockedWriter{release: make(chan struct{})}
	err = runPreDeployCommand(preDeployParameters(testCommand), ecsClient, &endlessPreDeployLogs{}, testClusterArn,
		testTaskDefinitionArn, writer)
	close(writer.release)
	if err == nil || !strings.HasPrefix(err.Error(), timedOut) || len(ecsClient.stopTaskInputs) != 1 {
		t.Errorf("a blocked deploy log: error = %v after %d StopTask calls; want the timeout after 1", err,
			len(ecsClient.stopTaskInputs))
	}
}

func TestPreDeployFinishLineNotHeldByBlockedLogs(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	original := preDeployLogWriteTimeout
	t.Cleanup(func() { preDeployLogWriteTimeout = original })
	preDeployLogWriteTimeout = 10 * time.Millisecond

	// The output blocks the deploy logs; the command still succeeds and the
	// deploy goes on.
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	writer := &blockedWriter{release: make(chan struct{})}
	defer close(writer.release)
	err := runPreDeployCommand(preDeployParameters(testCommand), ecsClient,
		&fakePreDeployLogs{pages: [][]string{{"migrated"}}}, testClusterArn, testTaskDefinitionArn, writer)
	if err != nil {
		t.Errorf("exit 0 behind a blocked deploy log: error = %v; want nil", err)
	}
}

func TestPreDeployNotHeldByStalledEcsCalls(t *testing.T) {
	fakePreDeployClock(t, 4*time.Minute)
	original := preDeployEcsCallTimeout
	t.Cleanup(func() { preDeployEcsCallTimeout = original })
	preDeployEcsCallTimeout = 10 * time.Millisecond

	// DescribeTasks and StopTask never answer: the deadline still comes, and
	// each StopTask attempt is given up.
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask()}}
	parameters := preDeployParameters(testCommand)
	stalledAfterStart := &stalledAfterRunTask{fakePreDeployEcs: ecsClient}
	var logs bytes.Buffer
	err := runPreDeployCommand(parameters, stalledAfterStart, &fakePreDeployLogs{}, testClusterArn,
		testTaskDefinitionArn, &logs)
	if err == nil || !strings.HasPrefix(err.Error(), "The pre-deploy command didn't finish within 30 minutes and couldn't be stopped: ") ||
		len(ecsClient.stopTaskInputs) != preDeployStopTaskAttempts {
		t.Errorf("stalled ECS calls: error = %v after %d StopTask calls; want it couldn't be stopped after %d",
			err, len(ecsClient.stopTaskInputs), preDeployStopTaskAttempts)
	}

	// RunTask never answers: the deploy fails instead of waiting forever.
	ecsClient = &fakePreDeployEcs{stalledCall: "RunTask"}
	_, err = runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{})
	if err == nil || !strings.HasPrefix(err.Error(), "The pre-deploy command couldn't start: ") {
		t.Errorf("a stalled RunTask: error = %v; want it couldn't start", err)
	}
	assertPreDeployTaskDefinitionDeregistered(t, ecsClient)
}

// stalledAfterRunTask starts the task, then stalls every other ECS call.
type stalledAfterRunTask struct {
	*fakePreDeployEcs
}

func (f *stalledAfterRunTask) RunTask(ctx context.Context, in *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	output, err := f.fakePreDeployEcs.RunTask(ctx, in, optFns...)
	f.stalled = true
	return output, err
}

func TestSplitLongLogLines(t *testing.T) {
	long := strings.Repeat("é", preDeployMaxLogLineLength) // two bytes each
	got := splitLongLogLines("short\n" + long + "\n")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if lines[0] != "short" || strings.Join(lines[1:], "") != long {
		t.Fatalf("splitLongLogLines changed the text: %q", got)
	}
	for _, line := range lines {
		if len(line) > preDeployMaxLogLineLength || !utf8.ValidString(line) {
			t.Errorf("line of %d bytes (valid UTF-8: %v); want at most %d", len(line), utf8.ValidString(line),
				preDeployMaxLogLineLength)
		}
	}
	if got := splitLongLogLines("done"); got != "done\n" {
		t.Errorf("splitLongLogLines(%q) = %q", "done", got)
	}
}

// failingWriter is a deploy log pipe whose reader has gone away.
type failingWriter struct{ writes int }

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 { // "Running the pre-deploy command" gets through
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestPreDeployLogsStopCopyingWhenTheDeployLogsClose(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{runningTask(), stoppedTask(aws.Int32(0), "")}}
	logsClient := &fakePreDeployLogs{pages: [][]string{{"a", "b"}, {"c"}}}
	writer := &failingWriter{}
	err := runPreDeployCommand(preDeployParameters(testCommand), ecsClient, logsClient, testClusterArn, testTaskDefinitionArn, writer)
	if err != nil {
		t.Fatalf("the exit code decides the deploy, not the logs: %v", err)
	}
	if logsClient.calls != 1 {
		t.Errorf("read %d pages after the deploy logs closed; want to stop after 1", logsClient.calls)
	}
}

func assertPreDeployTaskDefinitionDeregistered(t *testing.T, ecsClient *fakePreDeployEcs) {
	t.Helper()
	if len(ecsClient.deregisterInputs) != 1 ||
		aws.ToString(ecsClient.deregisterInputs[0].TaskDefinition) != testPreDeployTaskDefinitionArn {
		var got []string
		for _, in := range ecsClient.deregisterInputs {
			got = append(got, aws.ToString(in.TaskDefinition))
		}
		t.Errorf("deregistered %q; want the pre-deploy %q once", got, testPreDeployTaskDefinitionArn)
	}
}

func TestPreDeployTaskDefinitionInput(t *testing.T) {
	described := testTaskDefinitionDescription()
	source := described.TaskDefinition
	input := getPreDeployTaskDefinitionInput(described, "td-deployment-1-pre-deploy", "c-deployment-1")
	if !reflect.DeepEqual(described, testTaskDefinitionDescription()) {
		t.Error("the described task definition was changed")
	}

	if got := aws.ToString(input.Family); got != "td-deployment-1-pre-deploy" {
		t.Errorf("Family = %q; want td-deployment-1-pre-deploy", got)
	}
	wantTags := []ecsTypes.Tag{
		{Key: aws.String("Name"), Value: aws.String("td-deployment-1-pre-deploy")},
		{Key: aws.String("created by"), Value: aws.String("deployment.io")},
	}
	if !reflect.DeepEqual(input.Tags, wantTags) {
		t.Errorf("Tags = %+v; want %+v", input.Tags, wantTags)
	}
	if len(input.ContainerDefinitions) != 2 {
		t.Fatalf("%d container definitions; want 2", len(input.ContainerDefinitions))
	}
	container := input.ContainerDefinitions[0]
	if !reflect.DeepEqual(container.EntryPoint, []string{"sh", "-c"}) || container.Command != nil {
		t.Errorf("c-deployment-1: EntryPoint = %q, Command = %q; want [sh -c] and none", container.EntryPoint, container.Command)
	}
	container.EntryPoint = nil
	if !reflect.DeepEqual(container, source.ContainerDefinitions[0]) {
		t.Errorf("c-deployment-1 = %+v; want %+v but for the entrypoint", container, source.ContainerDefinitions[0])
	}
	if !reflect.DeepEqual(input.ContainerDefinitions[1], source.ContainerDefinitions[1]) {
		t.Errorf("sidecar = %+v; want it as is", input.ContainerDefinitions[1])
	}

	// Every other input the task definition has is copied as is; the source
	// sets them all, so a field missed here shows up.
	inputValue, sourceValue := reflect.ValueOf(input).Elem(), reflect.ValueOf(source).Elem()
	for i := 0; i < inputValue.NumField(); i++ {
		name := inputValue.Type().Field(i).Name
		switch name {
		case "Family", "Tags", "ContainerDefinitions":
			continue
		}
		sourceField := sourceValue.FieldByName(name)
		if !sourceField.IsValid() || !inputValue.Type().Field(i).IsExported() {
			continue
		}
		if sourceField.IsZero() {
			t.Errorf("the test's source task definition doesn't set %s", name)
		}
		if !reflect.DeepEqual(inputValue.Field(i).Interface(), sourceField.Interface()) {
			t.Errorf("%s = %+v; want the source's %+v", name, inputValue.Field(i).Interface(), sourceField.Interface())
		}
	}

	// The source sets no requiresCompatibilities: none is added.
	described = testTaskDefinitionDescription()
	described.TaskDefinition.RequiresCompatibilities = nil
	input = getPreDeployTaskDefinitionInput(described, "td-deployment-1-pre-deploy", "c-deployment-1")
	if input.RequiresCompatibilities != nil {
		t.Errorf("RequiresCompatibilities = %v; want none, as the source", input.RequiresCompatibilities)
	}
}

func TestPreDeployTaskDefinitionNeverCarriesTheCommand(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	if _, err := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{}); err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	if len(ecsClient.registerInputs) != 1 {
		t.Fatalf("RegisterTaskDefinition called %d times; want 1", len(ecsClient.registerInputs))
	}
	registered, err := json.Marshal(ecsClient.registerInputs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(registered), "migrate") || strings.Contains(string(registered), "hunter2") {
		t.Errorf("the registered task definition carries the command: %s", registered)
	}
}

func TestPreDeployTaskDefinitionDeregisteredAfterRunTaskError(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{runTaskErr: errors.New("AccessDeniedException: not authorized")}
	runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{})
	assertPreDeployTaskDefinitionDeregistered(t, ecsClient)
}

func TestPreDeployDeregisterFailureOnlyNoted(t *testing.T) {
	const line = "Couldn't remove the pre-deploy task definition: ClientException: throttled\n"
	for _, tt := range []struct {
		name    string
		task    ecsTypes.Task
		wantErr string
	}{
		{name: "exit 0", task: stoppedTask(aws.Int32(0), "")},
		{name: "non-zero exit", task: stoppedTask(aws.Int32(3), ""),
			wantErr: "The pre-deploy command failed with exit code 3. The deploy was stopped; the previous version keeps running."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakePreDeployClock(t, time.Second)
			ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{tt.task},
				deregisterErr: errors.New("ClientException: throttled")}
			logs, err := runPreDeploy(t, preDeployParameters(testCommand), ecsClient, &fakePreDeployLogs{})
			if gotErr := fmt.Sprint(err); (err == nil) != (len(tt.wantErr) == 0) || (err != nil && gotErr != tt.wantErr) {
				t.Errorf("error = %v; want %q", err, tt.wantErr)
			}
			if strings.Count(logs, line) != 1 {
				t.Errorf("deploy logs =\n%s\nwant the line %q once", logs, line)
			}
			assertPreDeployTaskDefinitionDeregistered(t, ecsClient)
		})
	}
}

func TestPreDeployTaskDefinitionErrorsFailToStart(t *testing.T) {
	original := preDeployEcsCallTimeout
	t.Cleanup(func() { preDeployEcsCallTimeout = original })
	preDeployEcsCallTimeout = 10 * time.Millisecond
	const second = " The deploy was stopped; the previous version keeps running."
	for _, tt := range []struct {
		name      string
		ecsClient *fakePreDeployEcs
		want      string
	}{
		{name: "DescribeTaskDefinition error",
			ecsClient: &fakePreDeployEcs{describeTaskDefinitionErr: errors.New("ClientException: unable to describe")},
			want:      "The pre-deploy command couldn't start: ClientException: unable to describe." + second},
		{name: "RegisterTaskDefinition error",
			ecsClient: &fakePreDeployEcs{registerErr: errors.New("ClientException: too many revisions")},
			want:      "The pre-deploy command couldn't start: ClientException: too many revisions." + second},
		{name: "DescribeTaskDefinition stalled",
			ecsClient: &fakePreDeployEcs{stalledCall: "DescribeTaskDefinition"},
			want:      "The pre-deploy command couldn't start: context deadline exceeded." + second},
		{name: "RegisterTaskDefinition stalled",
			ecsClient: &fakePreDeployEcs{stalledCall: "RegisterTaskDefinition"},
			want:      "The pre-deploy command couldn't start: context deadline exceeded." + second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakePreDeployClock(t, time.Second)
			done := make(chan struct{})
			var err error
			go func() {
				defer close(done)
				_, err = runPreDeploy(t, preDeployParameters(testCommand), tt.ecsClient, &fakePreDeployLogs{})
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the pre-deploy command hangs on a stalled task definition call")
			}
			if err == nil || err.Error() != tt.want {
				t.Errorf("error = %v; want %q", err, tt.want)
			}
			if len(tt.ecsClient.runTaskInputs) != 0 {
				t.Error("RunTask was called without a pre-deploy task definition")
			}
			if len(tt.ecsClient.deregisterInputs) != 0 {
				t.Errorf("deregistered %d task definitions; none was registered", len(tt.ecsClient.deregisterInputs))
			}
		})
	}
}
