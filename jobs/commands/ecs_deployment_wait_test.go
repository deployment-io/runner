package commands

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

const testWaitDeploymentID = "ecs-svc/9999"

// fakeEcsRollout answers DescribeServices with each of describes in turn (the
// last one again once they run out), or with describeErr.
type fakeEcsRollout struct {
	describes   []*ecs.DescribeServicesOutput
	describeErr error
	calls       int
	latency     func(time.Duration) // advances the fake clock by each call's latency
	deadlines   []time.Time         // each call's context deadline
}

func (f *fakeEcsRollout) DescribeServices(ctx context.Context, _ *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	f.calls++
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if f.latency != nil {
		f.latency(5 * time.Second)
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return f.describes[min(f.calls, len(f.describes))-1], nil
}

func (f *fakeEcsRollout) CreateService(context.Context, *ecs.CreateServiceInput, ...func(*ecs.Options)) (*ecs.CreateServiceOutput, error) {
	return nil, errors.New("unexpected CreateService")
}

func (f *fakeEcsRollout) UpdateService(context.Context, *ecs.UpdateServiceInput, ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	return nil, errors.New("unexpected UpdateService")
}

// rollout is a service whose deployments are the waited-on one in state (with
// reason) beside an older ACTIVE one, at runningCount == desiredCount.
func rollout(state ecsTypes.DeploymentRolloutState, reason string) *ecs.DescribeServicesOutput {
	return describedService("ACTIVE",
		ecsTypes.Deployment{Id: aws.String(testWaitDeploymentID), Status: aws.String("PRIMARY"), RolloutState: state,
			RolloutStateReason: aws.String(reason), RunningCount: 1, DesiredCount: 1})
}

func describedService(status string, deployments ...ecsTypes.Deployment) *ecs.DescribeServicesOutput {
	return &ecs.DescribeServicesOutput{Services: []ecsTypes.Service{{
		ServiceArn: aws.String(testEcsServiceArn), Status: aws.String(status),
		RunningCount: 1, DesiredCount: 1, Deployments: deployments,
	}}}
}

// fakeEcsDeploymentClock makes the wait poll every 15 seconds for at most a
// minute, without sleeping, on a fake clock that sleeping advances. It returns
// how long the wait slept and a function that advances the clock (latency).
func fakeEcsDeploymentClock(t *testing.T) (*time.Duration, func(time.Duration)) {
	t.Helper()
	originalInterval, originalLimit, originalSleep, originalNow := ecsDeploymentPollInterval, ecsDeploymentWaitLimit, ecsDeploymentSleep, ecsDeploymentNow
	t.Cleanup(func() {
		ecsDeploymentPollInterval, ecsDeploymentWaitLimit, ecsDeploymentSleep, ecsDeploymentNow = originalInterval, originalLimit, originalSleep, originalNow
	})
	var slept time.Duration
	now := time.Now()
	ecsDeploymentPollInterval, ecsDeploymentWaitLimit = 15*time.Second, time.Minute
	ecsDeploymentNow = func() time.Time { return now }
	ecsDeploymentSleep = func(d time.Duration) { slept += d; now = now.Add(d) }
	return &slept, func(d time.Duration) { now = now.Add(d) }
}

func TestWaitForEcsDeployment(t *testing.T) {
	for _, tt := range []struct {
		name      string
		describes []*ecs.DescribeServicesOutput
		wantErr   string // "" is success
		wantCalls int
	}{
		{name: "completed", wantCalls: 1,
			describes: []*ecs.DescribeServicesOutput{rollout(ecsTypes.DeploymentRolloutStateCompleted, "ECS deployment ecs-svc/9999 completed.")}},
		{name: "in progress then completed", wantCalls: 3, describes: []*ecs.DescribeServicesOutput{
			rollout(ecsTypes.DeploymentRolloutStateInProgress, ""), rollout(ecsTypes.DeploymentRolloutStateInProgress, ""),
			rollout(ecsTypes.DeploymentRolloutStateCompleted, "")}},
		{name: "failed", wantCalls: 1, wantErr: "ECS deployment failed: ECS deployment circuit breaker: tasks failed to start.",
			describes: []*ecs.DescribeServicesOutput{rollout(ecsTypes.DeploymentRolloutStateFailed, "ECS deployment circuit breaker: tasks failed to start.")}},
		{name: "in progress then failed", wantCalls: 2, wantErr: "ECS deployment failed: tasks failed",
			describes: []*ecs.DescribeServicesOutput{rollout(ecsTypes.DeploymentRolloutStateInProgress, ""),
				rollout(ecsTypes.DeploymentRolloutStateFailed, "tasks failed")}},
		{name: "rolled back", wantCalls: 2, wantErr: "ECS deployment ecs-svc/9999 was rolled back",
			describes: []*ecs.DescribeServicesOutput{rollout(ecsTypes.DeploymentRolloutStateInProgress, ""),
				describedService("ACTIVE", ecsTypes.Deployment{Id: aws.String("ecs-svc/0000"), Status: aws.String("PRIMARY"),
					RolloutState: ecsTypes.DeploymentRolloutStateCompleted})}},
		// DescribeServices lags the create/update: a deployment not listed yet is
		// still starting, so the wait keeps polling until it shows up.
		{name: "not listed yet then completed", wantCalls: 2, describes: []*ecs.DescribeServicesOutput{
			describedService("ACTIVE", ecsTypes.Deployment{Id: aws.String("ecs-svc/0000"), Status: aws.String("PRIMARY"),
				RolloutState: ecsTypes.DeploymentRolloutStateCompleted}),
			rollout(ecsTypes.DeploymentRolloutStateCompleted, "")}},
		{name: "never listed", wantCalls: 5, wantErr: "timed out waiting for the ECS deployment to complete",
			describes: []*ecs.DescribeServicesOutput{describedService("ACTIVE", ecsTypes.Deployment{Id: aws.String("ecs-svc/0000"),
				Status: aws.String("PRIMARY"), RolloutState: ecsTypes.DeploymentRolloutStateCompleted})}},
		{name: "inactive", wantCalls: 1, wantErr: "is INACTIVE",
			describes: []*ecs.DescribeServicesOutput{describedService("INACTIVE")}},
		{name: "draining", wantCalls: 1, wantErr: "is DRAINING",
			describes: []*ecs.DescribeServicesOutput{describedService("DRAINING")}},
		{name: "missing", wantCalls: 1, wantErr: "is missing",
			describes: []*ecs.DescribeServicesOutput{{Failures: []ecsTypes.Failure{{Arn: aws.String(testEcsServiceArn), Reason: aws.String("MISSING")}}}}},
		// Today's false success: one deployment, runningCount == desiredCount, but
		// ECS hasn't seen it reach steady state. Never nil; the limit ends it.
		{name: "running but in progress", wantCalls: 5, wantErr: "timed out waiting for the ECS deployment to complete",
			describes: []*ecs.DescribeServicesOutput{rollout(ecsTypes.DeploymentRolloutStateInProgress, "")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			slept, _ := fakeEcsDeploymentClock(t)
			client := &fakeEcsRollout{describes: tt.describes}
			var logs bytes.Buffer
			err := waitForEcsDeployment(client, &ecs.DescribeServicesInput{}, testWaitDeploymentID, &logs)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v; want %q", err, tt.wantErr)
			}
			if client.calls != tt.wantCalls || *slept != time.Duration(tt.wantCalls-1)*15*time.Second {
				t.Errorf("%d DescribeServices calls after sleeping %s; want %d, 15s apart", client.calls, *slept, tt.wantCalls)
			}
			if strings.HasPrefix(tt.wantErr, "ECS deployment") && !strings.Contains(logs.String(), tt.wantErr) {
				t.Errorf("logs %q; want the reason %q", logs.String(), tt.wantErr)
			}
		})
	}
}

func TestWaitForEcsDeploymentDescribeError(t *testing.T) {
	fakeEcsDeploymentClock(t)
	client := &fakeEcsRollout{describeErr: errors.New("AccessDeniedException")}
	err := waitForEcsDeployment(client, &ecs.DescribeServicesInput{}, testWaitDeploymentID, &bytes.Buffer{})
	if err == nil || err.Error() != "AccessDeniedException" || client.calls != 1 {
		t.Errorf("error = %v after %d calls; want the API error after one", err, client.calls)
	}
}

// The limit is wall-clock time: DescribeServices latency counts toward it, and
// each call carries the wait's deadline.
func TestWaitForEcsDeploymentLimitIncludesDescribeTime(t *testing.T) {
	slept, advance := fakeEcsDeploymentClock(t)
	client := &fakeEcsRollout{describes: []*ecs.DescribeServicesOutput{rollout(ecsTypes.DeploymentRolloutStateInProgress, "")},
		latency: advance}
	start := ecsDeploymentNow()
	err := waitForEcsDeployment(client, &ecs.DescribeServicesInput{}, testWaitDeploymentID, &bytes.Buffer{})
	if err == nil || err.Error() != "timed out waiting for the ECS deployment to complete" {
		t.Fatalf("error = %v; want the timeout", err)
	}
	// Calls at 0s, 20s, 40s and 60s (5s each, then 15s apart) — four, not the
	// five that 15s polls alone fit in a minute; at 65s another poll would
	// start after the minute.
	if client.calls != 4 || *slept != 45*time.Second {
		t.Errorf("%d DescribeServices calls after sleeping %s; want 4 after 45s", client.calls, *slept)
	}
	for _, deadline := range client.deadlines {
		if !deadline.Equal(start.Add(time.Minute)) {
			t.Errorf("DescribeServices deadline %v; want %v", deadline, start.Add(time.Minute))
		}
	}
}

// A DescribeServices call cut off by the deadline ends the wait as a timeout.
func TestWaitForEcsDeploymentDescribeDeadline(t *testing.T) {
	fakeEcsDeploymentClock(t)
	client := &fakeEcsRollout{describeErr: context.DeadlineExceeded}
	err := waitForEcsDeployment(client, &ecs.DescribeServicesInput{}, testWaitDeploymentID, &bytes.Buffer{})
	if err == nil || err.Error() != "timed out waiting for the ECS deployment to complete" {
		t.Errorf("error = %v; want the timeout", err)
	}
}

// Create and update wait on the deployment their own call started.
func TestServiceDeploysWaitOnTheirPrimaryDeployment(t *testing.T) {
	ecsService := &fakeEcsService{}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	parameters := privateServiceDeployParameters()
	if _, _, err := createEcsServiceIfNeeded(parameters, nil, testClusterArn, "arn:tg", testTaskDefinitionArn,
		&bytes.Buffer{}, ecsServiceDeployOptions{}); err != nil {
		t.Fatal(err)
	}
	ecsService.service = liveService(testNamespaceArn)
	if err := updateEcsService(parameters, nil, testClusterArn, testTaskDefinitionArn, &bytes.Buffer{}, ecsServiceDeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{testCreatedDeploymentID, testUpdatedDeploymentID}; !reflect.DeepEqual(ecsService.waits, want) {
		t.Errorf("waited on %v; want %v", ecsService.waits, want)
	}
}

func TestServiceDeploysWithoutPrimaryDeploymentFail(t *testing.T) {
	ecsService := &fakeEcsService{noPrimary: true}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	parameters := privateServiceDeployParameters()
	want := "ECS returned no primary deployment for es-deployment-1"
	if _, _, err := createEcsServiceIfNeeded(parameters, nil, testClusterArn, "arn:tg", testTaskDefinitionArn,
		&bytes.Buffer{}, ecsServiceDeployOptions{}); err == nil || err.Error() != want {
		t.Errorf("create error = %v; want %q", err, want)
	}
	ecsService.service = liveService(testNamespaceArn)
	if err := updateEcsService(parameters, nil, testClusterArn, testTaskDefinitionArn, &bytes.Buffer{}, ecsServiceDeployOptions{}); err == nil || err.Error() != want {
		t.Errorf("update error = %v; want %q", err, want)
	}
	if len(ecsService.waits) != 0 {
		t.Errorf("waited on %v", ecsService.waits)
	}
}

// A first deploy whose wait fails has already recorded its service, DNS name
// and namespace, so the next deploy has them.
func TestFailedFirstDeployHasSavedItsService(t *testing.T) {
	ecsService := &fakeEcsService{waitErr: errors.New("ECS deployment failed: tasks failed to start")}
	stubServiceAWS(t, ecsService, newFakeCloudMap())
	stubbed := commandUtils.UpdateDeploymentsPipeline
	var mu sync.Mutex
	var updates []deployments.UpdateDeploymentDtoV1
	commandUtils.UpdateDeploymentsPipeline, _ = goPipeline.NewPipeline(5, time.Hour, func(_ string, u []deployments.UpdateDeploymentDtoV1) {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, u...)
	})
	var logs bytes.Buffer
	_, _, err := createEcsServiceIfNeeded(privateServiceDeployParameters(), nil, testClusterArn, "arn:tg", testTaskDefinitionArn,
		&logs, ecsServiceDeployOptions{})
	commandUtils.UpdateDeploymentsPipeline.Shutdown()
	commandUtils.UpdateDeploymentsPipeline = stubbed
	if err == nil || err != ecsService.waitErr {
		t.Fatalf("error = %v; want the wait's", err)
	}
	if len(updates) != 1 || updates[0].ID != "deployment-1" || updates[0].EcsServiceArn != testEcsServiceArn ||
		updates[0].DnsName == "" || updates[0].NamespaceName == "" {
		t.Errorf("deployment updates %+v; want the service ARN, DNS name and namespace", updates)
	}
	created, waiting := strings.Index(logs.String(), "Created ECS service: "), strings.Index(logs.String(), "Waiting for ECS deployment "+testCreatedDeploymentID)
	if created < 0 || waiting < created {
		t.Errorf("logs:\n%s\nwant the service created before the wait", logs.String())
	}
}
