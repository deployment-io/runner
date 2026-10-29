package cloudwatch

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

type fakeClient struct {
	pages         []*cloudwatchlogs.DescribeLogGroupsOutput
	describeErrAt int // page index whose DescribeLogGroups call fails; -1 for none
	describeCalls []*cloudwatchlogs.DescribeLogGroupsInput
	createErr     error
	createCalls   []string
	putErrFor     map[string]error
	putCalls      map[string]int32
	putCallsInSeq []string
}

func newFakeClient() *fakeClient {
	return &fakeClient{describeErrAt: -1, putErrFor: map[string]error{}, putCalls: map[string]int32{}}
}

func (f *fakeClient) DescribeLogGroups(_ context.Context, params *cloudwatchlogs.DescribeLogGroupsInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error) {
	i := len(f.describeCalls)
	f.describeCalls = append(f.describeCalls, params)
	if i == f.describeErrAt {
		return nil, errors.New("access denied")
	}
	if i >= len(f.pages) {
		return &cloudwatchlogs.DescribeLogGroupsOutput{}, nil
	}
	return f.pages[i], nil
}

func (f *fakeClient) PutRetentionPolicy(_ context.Context, params *cloudwatchlogs.PutRetentionPolicyInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutRetentionPolicyOutput, error) {
	name := aws.ToString(params.LogGroupName)
	f.putCallsInSeq = append(f.putCallsInSeq, name)
	if err := f.putErrFor[name]; err != nil {
		return nil, err
	}
	f.putCalls[name] = aws.ToInt32(params.RetentionInDays)
	return &cloudwatchlogs.PutRetentionPolicyOutput{}, nil
}

func (f *fakeClient) CreateLogGroup(_ context.Context, params *cloudwatchlogs.CreateLogGroupInput,
	_ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error) {
	f.createCalls = append(f.createCalls, aws.ToString(params.LogGroupName))
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &cloudwatchlogs.CreateLogGroupOutput{}, nil
}

func group(name string, retention *int32) types.LogGroup {
	return types.LogGroup{LogGroupName: aws.String(name), RetentionInDays: retention}
}

func TestSetMissingRetention(t *testing.T) {
	f := newFakeClient()
	f.pages = []*cloudwatchlogs.DescribeLogGroupsOutput{
		{
			LogGroups: []types.LogGroup{group("org/a", nil), group("org/b", aws.Int32(30))},
			NextToken: aws.String("page2"),
		},
		{
			LogGroups: []types.LogGroup{group("org/c", nil), group("org/d", aws.Int32(3653))},
		},
	}
	updated, total := SetMissingRetention(context.Background(), f, "org/")
	if updated != 2 || total != 4 {
		t.Fatalf("got updated=%d total=%d, want 2 of 4", updated, total)
	}
	if len(f.describeCalls) != 2 {
		t.Fatalf("got %d DescribeLogGroups calls, want 2", len(f.describeCalls))
	}
	for _, in := range f.describeCalls {
		if aws.ToString(in.LogGroupNamePrefix) != "org/" {
			t.Errorf("got prefix %q, want %q", aws.ToString(in.LogGroupNamePrefix), "org/")
		}
	}
	if aws.ToString(f.describeCalls[1].NextToken) != "page2" {
		t.Errorf("second page requested with token %q, want page2", aws.ToString(f.describeCalls[1].NextToken))
	}
	if len(f.putCalls) != 2 || f.putCalls["org/a"] != 90 || f.putCalls["org/c"] != 90 {
		t.Errorf("got PutRetentionPolicy calls %v, want org/a and org/c with 90", f.putCalls)
	}
}

func TestSetMissingRetentionContinuesAfterPutError(t *testing.T) {
	f := newFakeClient()
	f.pages = []*cloudwatchlogs.DescribeLogGroupsOutput{
		{LogGroups: []types.LogGroup{group("org/a", nil), group("org/b", nil), group("org/c", nil)}},
	}
	f.putErrFor["org/b"] = errors.New("throttled")
	updated, total := SetMissingRetention(context.Background(), f, "org/")
	if updated != 2 || total != 3 {
		t.Fatalf("got updated=%d total=%d, want 2 of 3", updated, total)
	}
	if len(f.putCallsInSeq) != 3 {
		t.Errorf("got PutRetentionPolicy calls %v, want all three groups attempted", f.putCallsInSeq)
	}
}

func TestSetMissingRetentionStopsOnDescribeError(t *testing.T) {
	f := newFakeClient()
	f.pages = []*cloudwatchlogs.DescribeLogGroupsOutput{
		{LogGroups: []types.LogGroup{group("org/a", nil)}, NextToken: aws.String("page2")},
		{LogGroups: []types.LogGroup{group("org/b", nil)}},
	}
	f.describeErrAt = 1
	updated, total := SetMissingRetention(context.Background(), f, "org/")
	if updated != 1 || total != 1 {
		t.Fatalf("got updated=%d total=%d, want 1 of 1", updated, total)
	}
	if len(f.describeCalls) != 2 {
		t.Errorf("got %d DescribeLogGroups calls, want 2", len(f.describeCalls))
	}

	f = newFakeClient()
	f.describeErrAt = 0
	updated, total = SetMissingRetention(context.Background(), f, "org/")
	if updated != 0 || total != 0 || len(f.putCallsInSeq) != 0 {
		t.Fatalf("got updated=%d total=%d puts=%v, want nothing", updated, total, f.putCallsInSeq)
	}
}

func TestCreateLogGroupSetsRetention(t *testing.T) {
	f := newFakeClient()
	if err := createLogGroup(context.Background(), f, "org/dep"); err != nil {
		t.Fatal(err)
	}
	if len(f.createCalls) != 1 || f.putCalls["org/dep"] != 90 {
		t.Errorf("got create=%v put=%v, want org/dep created with 90 days", f.createCalls, f.putCalls)
	}
}

func TestCreateLogGroupExistingKeepsRetention(t *testing.T) {
	f := newFakeClient()
	f.createErr = &types.ResourceAlreadyExistsException{Message: aws.String("exists")}
	if err := createLogGroup(context.Background(), f, "org/dep"); err != nil {
		t.Fatal(err)
	}
	if len(f.putCallsInSeq) != 0 {
		t.Errorf("got PutRetentionPolicy calls %v, want none", f.putCallsInSeq)
	}
}

func TestCreateLogGroupErrors(t *testing.T) {
	f := newFakeClient()
	f.putErrFor["org/dep"] = errors.New("denied")
	if err := createLogGroup(context.Background(), f, "org/dep"); err == nil {
		t.Error("want error when PutRetentionPolicy fails")
	}

	f = newFakeClient()
	f.createErr = errors.New("denied")
	if err := createLogGroup(context.Background(), f, "org/dep"); err == nil {
		t.Error("want error when CreateLogGroup fails")
	}
	if len(f.putCallsInSeq) != 0 {
		t.Errorf("got PutRetentionPolicy calls %v, want none", f.putCallsInSeq)
	}
}
