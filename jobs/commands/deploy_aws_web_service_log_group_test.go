package commands

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwlTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

type fakeLogGroupCreator struct {
	createErr   error
	createCalls []string
	putCalls    map[string]int32
}

func (f *fakeLogGroupCreator) CreateLogGroup(_ context.Context, in *cloudwatchlogs.CreateLogGroupInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error) {
	f.createCalls = append(f.createCalls, aws.ToString(in.LogGroupName))
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &cloudwatchlogs.CreateLogGroupOutput{}, nil
}

func (f *fakeLogGroupCreator) PutRetentionPolicy(_ context.Context, in *cloudwatchlogs.PutRetentionPolicyInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutRetentionPolicyOutput, error) {
	if f.putCalls == nil {
		f.putCalls = map[string]int32{}
	}
	f.putCalls[aws.ToString(in.LogGroupName)] = aws.ToInt32(in.RetentionInDays)
	return &cloudwatchlogs.PutRetentionPolicyOutput{}, nil
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func TestEnsureTaskLogGroupCreatesMissingGroupWithRetention(t *testing.T) {
	logs := captureLog(t)
	f := &fakeLogGroupCreator{}
	ensureTaskLogGroup(context.Background(), f, taskLogConfiguration("org-1/deployment-1", "us-east-1", "application/build-1"))
	if len(f.createCalls) != 1 || f.createCalls[0] != "org-1/deployment-1" || f.putCalls["org-1/deployment-1"] != 90 {
		t.Errorf("got create=%v put=%v, want org-1/deployment-1 created with 90 days", f.createCalls, f.putCalls)
	}
	if logs.Len() != 0 {
		t.Errorf("got log %q, want none", logs.String())
	}
}

func TestEnsureTaskLogGroupExistingGroupKeepsRetention(t *testing.T) {
	logs := captureLog(t)
	f := &fakeLogGroupCreator{createErr: &cwlTypes.ResourceAlreadyExistsException{Message: aws.String("exists")}}
	ensureTaskLogGroup(context.Background(), f, taskLogConfiguration("org-1/deployment-1", "us-east-1", "application/build-1"))
	if len(f.putCalls) != 0 {
		t.Errorf("got PutRetentionPolicy calls %v, want none", f.putCalls)
	}
	if logs.Len() != 0 {
		t.Errorf("got log %q, want none", logs.String())
	}
}

func TestEnsureTaskLogGroupOtherErrorIsLogged(t *testing.T) {
	logs := captureLog(t)
	f := &fakeLogGroupCreator{createErr: errors.New("access denied")}
	ensureTaskLogGroup(context.Background(), f, taskLogConfiguration("org-1/deployment-1", "us-east-1", "application/build-1"))
	if len(f.putCalls) != 0 {
		t.Errorf("got PutRetentionPolicy calls %v, want none", f.putCalls)
	}
	got := logs.String()
	if strings.Count(got, "\n") != 1 ||
		!strings.HasPrefix(got, "log group retention: couldn't create org-1/deployment-1 before registering the task definition: ") ||
		!strings.Contains(got, "access denied") {
		t.Errorf("got log %q, want one line naming the group and the error", got)
	}
}

func TestEnsureTaskLogGroupNamesTheTaskDefinitionsAwslogsGroup(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "deployment-1")
	logGroupName, err := commandUtils.GetLogGroupName(parameters)
	if err != nil {
		t.Fatal(err)
	}
	logConfiguration := taskLogConfiguration(logGroupName, "us-east-1", "application/build-1")
	f := &fakeLogGroupCreator{}
	ensureTaskLogGroup(context.Background(), f, logConfiguration)
	if len(f.createCalls) != 1 || f.createCalls[0] != logConfiguration.Options["awslogs-group"] ||
		f.createCalls[0] != "org-1/deployment-1" {
		t.Errorf("ensured %v, task definition's awslogs-group is %q", f.createCalls, logConfiguration.Options["awslogs-group"])
	}
	if logConfiguration.Options["awslogs-create-group"] != "true" {
		t.Errorf("awslogs-create-group = %q, want true", logConfiguration.Options["awslogs-create-group"])
	}
}
