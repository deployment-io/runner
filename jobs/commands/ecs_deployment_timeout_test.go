package commands

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

const testServingTaskDefinitionArn = "arn:aws:ecs:us-east-1:123456789012:task-definition/td-serving:3"

func earlierDeployment(id, taskDefinition string, state ecsTypes.DeploymentRolloutState, createdAt time.Time) ecsTypes.Deployment {
	return ecsTypes.Deployment{Id: aws.String(id), Status: aws.String("ACTIVE"), RolloutState: state,
		TaskDefinition: aws.String(taskDefinition), CreatedAt: aws.Time(createdAt), DesiredCount: 1, RunningCount: 1}
}

// stoppedDeployment is the PRIMARY deployment of a service a timed-out deploy
// stopped: ECS counts 0 of 0 tasks as COMPLETED.
func stoppedDeployment(id, taskDefinition string, createdAt time.Time) ecsTypes.Deployment {
	deployment := earlierDeployment(id, taskDefinition, ecsTypes.DeploymentRolloutStateCompleted, createdAt)
	deployment.DesiredCount, deployment.RunningCount = 0, 0
	return deployment
}

func TestRollbackTaskDefinition(t *testing.T) {
	base := time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	newPrimary := ecsTypes.Deployment{Id: aws.String(testUpdatedDeploymentID), Status: aws.String("PRIMARY"),
		RolloutState: ecsTypes.DeploymentRolloutStateCompleted, TaskDefinition: aws.String("td:new"), CreatedAt: aws.Time(base.Add(time.Hour))}
	tests := []struct {
		name        string
		deployments []ecsTypes.Deployment
		want        string
	}{
		{"none", nil, ""},
		{"only the new one", []ecsTypes.Deployment{newPrimary}, ""},
		{"one completed", []ecsTypes.Deployment{
			earlierDeployment("ecs-svc/1", "td:3", ecsTypes.DeploymentRolloutStateCompleted, base), newPrimary}, "td:3"},
		{"most recent completed wins; in progress and failed ignored", []ecsTypes.Deployment{
			earlierDeployment("ecs-svc/1", "td:1", ecsTypes.DeploymentRolloutStateCompleted, base),
			earlierDeployment("ecs-svc/3", "td:3", ecsTypes.DeploymentRolloutStateCompleted, base.Add(2*time.Minute)),
			earlierDeployment("ecs-svc/2", "td:2", ecsTypes.DeploymentRolloutStateCompleted, base.Add(time.Minute)),
			earlierDeployment("ecs-svc/4", "td:4", ecsTypes.DeploymentRolloutStateInProgress, base.Add(3*time.Minute)),
			earlierDeployment("ecs-svc/5", "td:5", ecsTypes.DeploymentRolloutStateFailed, base.Add(4*time.Minute)),
			newPrimary}, "td:3"},
		{"stopped completed one ignored", []ecsTypes.Deployment{
			earlierDeployment("ecs-svc/1", "td:1", ecsTypes.DeploymentRolloutStateCompleted, base),
			stoppedDeployment("ecs-svc/2", "td:2", base.Add(time.Minute)), newPrimary}, "td:1"},
		{"only a stopped one", []ecsTypes.Deployment{
			stoppedDeployment("ecs-svc/2", "td:2", base), newPrimary}, ""},
		{"no completed", []ecsTypes.Deployment{
			earlierDeployment("ecs-svc/4", "td:4", ecsTypes.DeploymentRolloutStateInProgress, base),
			earlierDeployment("ecs-svc/5", "td:5", ecsTypes.DeploymentRolloutStateFailed, base), newPrimary}, ""},
	}
	for _, tt := range tests {
		if got := rollbackTaskDefinition(tt.deployments, testUpdatedDeploymentID); got != tt.want {
			t.Errorf("%s: rollbackTaskDefinition = %q; want %q", tt.name, got, tt.want)
		}
	}
}

func timedOutEcsService(earlier ...ecsTypes.Deployment) *fakeEcsService {
	ecsService := &fakeEcsService{waitErr: errEcsDeploymentTimeout, earlier: earlier}
	ecsService.service = liveService(testNamespaceArn)
	return ecsService
}

// A redeploy that times out goes back to the version that was serving.
func TestTimedOutRedeployRollsBack(t *testing.T) {
	base := time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	ecsService := timedOutEcsService(
		earlierDeployment("ecs-svc/1", "td:1", ecsTypes.DeploymentRolloutStateCompleted, base),
		earlierDeployment("ecs-svc/2", testServingTaskDefinitionArn, ecsTypes.DeploymentRolloutStateCompleted, base.Add(time.Minute)),
		earlierDeployment("ecs-svc/3", "td:failed", ecsTypes.DeploymentRolloutStateFailed, base.Add(2*time.Minute)))
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	var logs bytes.Buffer
	err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn, &logs, ecsServiceDeployOptions{})
	if want := "timed out waiting for the ECS deployment to complete; rolled back to the previous version"; err == nil || err.Error() != want {
		t.Fatalf("error = %v; want %q", err, want)
	}
	if !errors.Is(err, errEcsDeploymentTimeout) {
		t.Errorf("error %v doesn't wrap errEcsDeploymentTimeout", err)
	}
	if len(ecsService.updateInputs) != 2 {
		t.Fatalf("UpdateService called %d times; want 2", len(ecsService.updateInputs))
	}
	in := ecsService.updateInputs[1]
	if aws.ToString(in.Service) != "es-deployment-1" || aws.ToString(in.Cluster) != testClusterArn {
		t.Errorf("rollback updated %s in %s", aws.ToString(in.Service), aws.ToString(in.Cluster))
	}
	if aws.ToString(in.TaskDefinition) != testServingTaskDefinitionArn || aws.ToInt32(in.DesiredCount) != 1 {
		t.Errorf("rollback TaskDefinition %q, DesiredCount %d; want %q, 1",
			aws.ToString(in.TaskDefinition), aws.ToInt32(in.DesiredCount), testServingTaskDefinitionArn)
	}
	if !reflect.DeepEqual(in.DeploymentConfiguration, productionServiceDeploymentConfiguration()) {
		t.Errorf("rollback DeploymentConfiguration = %+v; want the production one", in.DeploymentConfiguration)
	}
	if want := "Rolled back to " + testServingTaskDefinitionArn + ", the version that was running before this deploy.\n"; countLines(logs.String(), want) != 1 {
		t.Errorf("logs:\n%s\nwant %q", logs.String(), want)
	}
}

const stoppedLog = "Stopped the service: there's no earlier working version to roll back to. The next deploy starts it again.\n"

func checkStopped(t *testing.T, ecsService *fakeEcsService, wantUpdates int, logs string, err error) {
	t.Helper()
	if want := "timed out waiting for the ECS deployment to complete; stopped the service (no earlier working version)"; err == nil || err.Error() != want {
		t.Fatalf("error = %v; want %q", err, want)
	}
	if !errors.Is(err, errEcsDeploymentTimeout) {
		t.Errorf("error %v doesn't wrap errEcsDeploymentTimeout", err)
	}
	if len(ecsService.updateInputs) != wantUpdates {
		t.Fatalf("UpdateService called %d times; want %d", len(ecsService.updateInputs), wantUpdates)
	}
	in := ecsService.updateInputs[wantUpdates-1]
	if aws.ToString(in.Service) != "es-deployment-1" || aws.ToString(in.Cluster) != testClusterArn ||
		in.DesiredCount == nil || *in.DesiredCount != 0 || in.TaskDefinition != nil || in.DeploymentConfiguration != nil ||
		in.ServiceRegistries != nil || in.ServiceConnectConfiguration != nil || in.NetworkConfiguration != nil || in.PropagateTags != "" {
		t.Errorf("stop input = %+v; want DesiredCount 0 only", in)
	}
	if countLines(logs, stoppedLog) != 1 {
		t.Errorf("logs:\n%s\nwant %q", logs, stoppedLog)
	}
}

// A redeploy whose earlier deploys never completed has nothing to go back to.
func TestTimedOutRedeployWithoutCompletedDeploymentStops(t *testing.T) {
	base := time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	ecsService := timedOutEcsService(
		earlierDeployment("ecs-svc/1", "td:1", ecsTypes.DeploymentRolloutStateFailed, base),
		earlierDeployment("ecs-svc/2", "td:2", ecsTypes.DeploymentRolloutStateInProgress, base.Add(time.Minute)))
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	var logs bytes.Buffer
	err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn, &logs, ecsServiceDeployOptions{})
	checkStopped(t, ecsService, 2, logs.String(), err)
}

// The deploy after one that was stopped also times out: the stopped broken
// deployment (COMPLETED with 0 tasks) isn't a version to go back to.
func TestTimedOutRedeployAfterStopStopsAgain(t *testing.T) {
	base := time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	ecsService := timedOutEcsService(stoppedDeployment("ecs-svc/2", "td:broken", base))
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	var logs bytes.Buffer
	err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn, &logs, ecsServiceDeployOptions{})
	checkStopped(t, ecsService, 2, logs.String(), err)
}

func TestTimedOutFirstDeployStops(t *testing.T) {
	ecsService := &fakeEcsService{waitErr: errEcsDeploymentTimeout}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	var logs bytes.Buffer
	_, _, err := createEcsServiceIfNeeded(privateServiceDeployParameters(), nil, testClusterArn, "arn:tg", testTaskDefinitionArn,
		&logs, ecsServiceDeployOptions{})
	if len(ecsService.createInputs) != 1 {
		t.Fatalf("CreateService called %d times; want 1", len(ecsService.createInputs))
	}
	checkStopped(t, ecsService, 1, logs.String(), err)
}

func TestTimedOutDeployRollbackFails(t *testing.T) {
	ecsService := timedOutEcsService(earlierDeployment("ecs-svc/2", testServingTaskDefinitionArn,
		ecsTypes.DeploymentRolloutStateCompleted, time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)))
	ecsService.updateErrs = []error{nil, errors.New("throttled")}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	var logs bytes.Buffer
	err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn, &logs, ecsServiceDeployOptions{})
	if want := "timed out waiting for the ECS deployment to complete; rolling back failed: throttled"; err == nil || err.Error() != want {
		t.Fatalf("error = %v; want %q", err, want)
	}
	if !errors.Is(err, errEcsDeploymentTimeout) {
		t.Errorf("error %v doesn't wrap errEcsDeploymentTimeout", err)
	}
	if len(ecsService.updateInputs) != 2 {
		t.Errorf("UpdateService called %d times; want 2 (no retry)", len(ecsService.updateInputs))
	}
	if want := "Couldn't roll back after the time limit: throttled\n"; countLines(logs.String(), want) != 1 {
		t.Errorf("logs:\n%s\nwant %q", logs.String(), want)
	}
	if strings.Contains(logs.String(), "Rolled back to ") {
		t.Errorf("logs:\n%s\nsay it rolled back", logs.String())
	}
}

// FAILED and rolled-back deployments were already rolled back by ECS, and a
// preview database (no circuit breaker) is left as it is.
func TestOnlyTimedOutProductionDeploysAreSettled(t *testing.T) {
	completed := earlierDeployment("ecs-svc/2", testServingTaskDefinitionArn,
		ecsTypes.DeploymentRolloutStateCompleted, time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC))
	tests := []struct {
		name    string
		waitErr error
		opts    ecsServiceDeployOptions
	}{
		{"failed", errors.New("ECS deployment failed: tasks failed to start"), ecsServiceDeployOptions{}},
		{"rolled back", errors.New("ECS deployment ecs-svc/2222 was rolled back"), ecsServiceDeployOptions{}},
		{"preview database timed out", errEcsDeploymentTimeout,
			ecsServiceDeployOptions{registerDnsName: true, updateServiceConnect: true, noCircuitBreaker: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ecsService := timedOutEcsService(completed)
			ecsService.waitErr = tt.waitErr
			stubServiceAWS(t, ecsService, newFakeCloudMap())
			err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn, &bytes.Buffer{}, tt.opts)
			if err != tt.waitErr {
				t.Errorf("update error = %v; want the wait's %v", err, tt.waitErr)
			}
			if len(ecsService.updateInputs) != 1 {
				t.Errorf("UpdateService called %d times; want 1", len(ecsService.updateInputs))
			}

			ecsService = &fakeEcsService{waitErr: tt.waitErr}
			stubServiceAWS(t, ecsService, newFakeCloudMap())
			_, _, err = createEcsServiceIfNeeded(privateServiceDeployParameters(), nil, testClusterArn, "", testTaskDefinitionArn,
				&bytes.Buffer{}, tt.opts)
			if err != tt.waitErr {
				t.Errorf("create error = %v; want the wait's %v", err, tt.waitErr)
			}
			if len(ecsService.updateInputs) != 0 {
				t.Errorf("first deploy called UpdateService %d times; want 0", len(ecsService.updateInputs))
			}
		})
	}
}
