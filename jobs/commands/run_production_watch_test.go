package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatch_types "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecs_types "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

func TestGetMapsRunProductionWatch(t *testing.T) {
	c, err := Get(commands_enums.RunProductionWatch)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(jobs.StoppableCommand); !ok {
		t.Fatal("RunProductionWatch is not a StoppableCommand")
	}
}

func TestProductionWatchWindowEnd(t *testing.T) {
	targets := []tasks.ProductionWatchTargetV1{{DeployEndedAt: 1000}, {DeployEndedAt: 3000}, {DeployEndedAt: 2000}}
	if got := productionWatchWindowEnd(targets, 5000); got != 3000+1800 {
		t.Errorf("window end = %d, want %d", got, 3000+1800)
	}
	// DeployEndedAt 0 counts as the Job's start.
	targets = append(targets, tasks.ProductionWatchTargetV1{})
	if got := productionWatchWindowEnd(targets, 5000); got != 5000+1800 {
		t.Errorf("window end with an unended target = %d, want %d", got, 5000+1800)
	}
}

func TestProductionWatchWindows(t *testing.T) {
	target := tasks.ProductionWatchTargetV1{DeployStartedAt: 6030, DeployEndedAt: 6330}
	after, before := productionWatchWindows(target, 6400, 6990)
	if after != (productionWatchWindow{6300, 6960}) {
		t.Errorf("after = %+v", after)
	}
	if before != (productionWatchWindow{6000 - 660, 6000}) {
		t.Errorf("before = %+v", before)
	}
	// DeployEndedAt 0 is the Job's start.
	after, before = productionWatchWindows(tasks.ProductionWatchTargetV1{DeployStartedAt: 5900}, 6010, 6150)
	if after != (productionWatchWindow{6000, 6120}) || before != (productionWatchWindow{5760, 5880}) {
		t.Errorf("unended: after = %+v, before = %+v", after, before)
	}
	// An "after" under a minute.
	after, _ = productionWatchWindows(target, 6400, 6350)
	if after.length() >= 60 {
		t.Errorf("after = %+v, want under 60 seconds", after)
	}
}

func watchReport(reqBefore, errBefore, reqAfter, errAfter int) tasks.ProductionWatchTargetReportDtoV1 {
	return tasks.ProductionWatchTargetReportDtoV1{RequestsBefore: reqBefore, Errors5xxBefore: errBefore, RequestsAfter: reqAfter, Errors5xxAfter: errAfter}
}

func TestProductionWatchErrorsUp(t *testing.T) {
	cases := []struct {
		name string
		r    tasks.ProductionWatchTargetReportDtoV1
		want bool
	}{
		{"below 5", watchReport(1000, 0, 100, 4), false},
		{"below 3x the before rate", watchReport(1000, 20, 1000, 50), false},
		{"below 1%", watchReport(100000, 0, 1000, 9), false},
		{"before rate 0", watchReport(1000, 0, 100, 5), true},
		{"all three met", watchReport(1000, 10, 500, 20), true},
		{"no traffic before", watchReport(0, 0, 200, 10), true},
		// 3 × 0.07 is 0.21000000000000002 in floating point.
		{"exactly 3x the before rate", watchReport(100, 7, 100, 21), true},
		{"exactly 1%", watchReport(100000, 0, 500, 5), true},
		// The cross products are past int64.
		{"huge counts", watchReport(5_000_000_000, 0, 5_000_000_000, 2_000_000_000), true},
		{"huge counts, same rate", watchReport(5_000_000_000, 2_000_000_000, 5_000_000_000, 2_000_000_000), false},
	}
	for _, c := range cases {
		if got := productionWatchErrorsUp(c.r); got != c.want {
			t.Errorf("%s: errorsUp = %v, want %v", c.name, got, c.want)
		}
	}
	if got := productionWatchErrorsDetail(watchReport(1000, 10, 500, 20)); got != "20 5xx of 500 requests since the deploy (4.0%), 10 of 1000 before" {
		t.Errorf("detail = %q", got)
	}
}

func TestProductionWatchVerdict(t *testing.T) {
	web := tasks.ProductionWatchTargetV1{Kind: tasks.ProductionWatchKindWebService}
	site := tasks.ProductionWatchTargetV1{Kind: tasks.ProductionWatchKindStaticSite}
	private := tasks.ProductionWatchTargetV1{Kind: tasks.ProductionWatchKindPrivateService}
	plenty := watchReport(1000, 0, 1000, 0)
	low := watchReport(1000, 0, 49, 0)
	cases := []struct {
		name    string
		targets []tasks.ProductionWatchTargetV1
		reports []tasks.ProductionWatchTargetReportDtoV1
		signal  bool
		want    string
	}{
		{"a signal", []tasks.ProductionWatchTargetV1{web}, []tasks.ProductionWatchTargetReportDtoV1{plenty}, true, tasks.ProductionWatchVerdictErrorsUp},
		{"errors on the final numbers", []tasks.ProductionWatchTargetV1{web}, []tasks.ProductionWatchTargetReportDtoV1{watchReport(1000, 0, 1000, 50)}, false, tasks.ProductionWatchVerdictErrorsUp},
		{"low traffic", []tasks.ProductionWatchTargetV1{web, site, private}, []tasks.ProductionWatchTargetReportDtoV1{low, low, {}}, false, tasks.ProductionWatchVerdictNotEnoughTraffic},
		{"one target with traffic", []tasks.ProductionWatchTargetV1{web, site}, []tasks.ProductionWatchTargetReportDtoV1{low, plenty}, false, tasks.ProductionWatchVerdictNoChange},
		{"private service only", []tasks.ProductionWatchTargetV1{private}, []tasks.ProductionWatchTargetReportDtoV1{{}}, false, tasks.ProductionWatchVerdictNoChange},
	}
	for _, c := range cases {
		if got := productionWatchVerdict(c.targets, c.reports, c.signal); got != c.want {
			t.Errorf("%s: verdict = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestClassifyCrashedTasks(t *testing.T) {
	const current, older = "arn:td:app:2", "arn:td:app:1"
	since := time.Unix(10000, 0)
	at := func(sec int64) *time.Time { ts := time.Unix(sec, 0); return &ts }
	task := func(td string, stoppedAt int64, code ecs_types.TaskStopCode, reasons ...string) ecs_types.Task {
		tk := ecs_types.Task{TaskDefinitionArn: aws.String(td), StoppedAt: at(stoppedAt), StopCode: code}
		for _, r := range reasons {
			tk.Containers = append(tk.Containers, ecs_types.Container{Reason: aws.String(r)})
		}
		return tk
	}
	cases := []struct {
		name       string
		task       ecs_types.Task
		want       int
		wantReason string
	}{
		{"essential container exited", task(current, 10000, ecs_types.TaskStopCodeEssentialContainerExited), 1, "EssentialContainerExited"},
		{"failed to start", task(current, 10100, ecs_types.TaskStopCodeTaskFailedToStart), 1, "TaskFailedToStart"},
		{"out of memory", task(current, 10100, ecs_types.TaskStopCodeServiceSchedulerInitiated, "OutOfMemoryError: Container killed due to memory usage"), 1, "OutOfMemory"},
		{"scheduler stop", task(current, 10100, ecs_types.TaskStopCodeServiceSchedulerInitiated), 0, ""},
		{"older task definition", task(older, 10100, ecs_types.TaskStopCodeEssentialContainerExited), 0, ""},
		{"stopped before the deploy ended", task(current, 9999, ecs_types.TaskStopCodeEssentialContainerExited), 0, ""},
	}
	for _, c := range cases {
		n, reason := classifyCrashedTasks([]ecs_types.Task{c.task}, current, since)
		if n != c.want || reason != c.wantReason {
			t.Errorf("%s: = %d %q, want %d %q", c.name, n, reason, c.want, c.wantReason)
		}
	}
}

func TestProductionWatchTwoInARow(t *testing.T) {
	var prev bool
	steps := []struct{ bad, want bool }{{true, false}, {false, false}, {true, false}, {true, true}, {true, true}, {false, false}}
	for i, s := range steps {
		if got := productionWatchTwoInARow(&prev, s.bad); got != s.want {
			t.Errorf("poll %d: = %v, want %v", i, got, s.want)
		}
	}
}

func TestProductionWatchSignalsOncePerRepositoryAndKind(t *testing.T) {
	var s productionWatchSignals
	if !s.raise("0-a/b", tasks.ProductionWatchSignalTaskCrashed, "1", 10) {
		t.Fatal("first signal not raised")
	}
	if s.raise("0-a/b", tasks.ProductionWatchSignalTaskCrashed, "2", 20) {
		t.Error("the same Repository and Kind raised twice")
	}
	if !s.raise("1-a/c", tasks.ProductionWatchSignalTaskCrashed, "1", 20) || !s.raise("0-a/b", tasks.ProductionWatchSignalErrorsUp, "x", 30) {
		t.Error("another Repository or Kind not raised")
	}
	if len(s.list) != 3 || s.list[0].Detail != "1" || s.list[0].SeenAt != 10 {
		t.Errorf("signals = %+v", s.list)
	}
}

func TestProductionWatchARNParsing(t *testing.T) {
	if got := loadBalancerDimension("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-lb/50dc6c495c0c9188"); got != "app/my-lb/50dc6c495c0c9188" {
		t.Errorf("load balancer dimension = %q", got)
	}
	if got := ecsServiceNameFromArn("arn:aws:ecs:us-east-1:123456789012:service/my-cluster/my-service"); got != "my-service" {
		t.Errorf("service name = %q", got)
	}
	if got := ecsServiceNameFromArn("arn:aws:ecs:us-east-1:123456789012:service/old-service"); got != "old-service" {
		t.Errorf("old-format service name = %q", got)
	}
	if got := productionWatchURLPath("https://example.com/health?token=x"); got != "/health" {
		t.Errorf("path = %q", got)
	}
	if got := productionWatchURLPath("https://example.com"); got != "/" {
		t.Errorf("empty path = %q", got)
	}
}

func TestCloudFront5xx(t *testing.T) {
	requests := map[int64]float64{60: 1000, 120: 500, 180: 10}
	rates := map[int64]float64{60: 1.5, 120: 2, 180: 0}
	if got := cloudFront5xx(requests, rates); got != 25 {
		t.Errorf("5xx = %d, want 25", got)
	}
}

// fakeCloudWatch answers GetMetricData with fixed values per query id.
type fakeCloudWatch struct {
	values map[string][]float64
	status map[string]cloudwatch_types.StatusCode
	inputs []*cloudwatch.GetMetricDataInput
}

func (f *fakeCloudWatch) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.inputs = append(f.inputs, in)
	out := &cloudwatch.GetMetricDataOutput{}
	for _, q := range in.MetricDataQueries {
		r := cloudwatch_types.MetricDataResult{Id: q.Id, StatusCode: cloudwatch_types.StatusCodeComplete}
		if code, ok := f.status[aws.ToString(q.Id)]; ok {
			r.StatusCode = code
		}
		for i, v := range f.values[aws.ToString(q.Id)] {
			r.Timestamps = append(r.Timestamps, in.StartTime.Add(time.Duration(i)*time.Minute))
			r.Values = append(r.Values, v)
		}
		out.MetricDataResults = append(out.MetricDataResults, r)
	}
	return out, nil
}

func TestReadLoadBalancerMetrics(t *testing.T) {
	cw := &fakeCloudWatch{values: map[string][]float64{
		"requests": {100, 50}, "target5xx": {2, 1}, "elb5xx": {1}, "p95": {0.1234},
	}}
	target := tasks.ProductionWatchTargetV1{LoadBalancerArn: "arn:aws:elasticloadbalancing:eu-west-1:1:loadbalancer/app/lb/abc"}
	got, err := readLoadBalancerMetrics(context.Background(), cw, target, productionWatchWindow{600, 720})
	if err != nil {
		t.Fatal(err)
	}
	if got != (productionWatchTraffic{Requests: 150, Errors5xx: 4, P95Ms: 123}) {
		t.Errorf("traffic = %+v", got)
	}
	in := cw.inputs[0]
	if in.StartTime.Unix() != 600 || in.EndTime.Unix() != 720 || len(in.MetricDataQueries) != 4 {
		t.Fatalf("input = %+v", in)
	}
	for _, q := range in.MetricDataQueries {
		if aws.ToString(q.MetricStat.Metric.Dimensions[0].Value) != "app/lb/abc" {
			t.Errorf("dimension = %q", aws.ToString(q.MetricStat.Metric.Dimensions[0].Value))
		}
		if aws.ToString(q.Id) == "p95" && (aws.ToString(q.MetricStat.Stat) != "p95" || aws.ToInt32(q.MetricStat.Period) != 120) {
			t.Errorf("p95 query = %+v", q.MetricStat)
		}
	}
}

// A query CloudWatch didn't answer fails the read rather than reading as 0.
func TestReadLoadBalancerMetricsFailsOnAnUnansweredQuery(t *testing.T) {
	for _, code := range []cloudwatch_types.StatusCode{cloudwatch_types.StatusCodeInternalError, cloudwatch_types.StatusCodeForbidden} {
		cw := &fakeCloudWatch{values: map[string][]float64{"requests": {100}}, status: map[string]cloudwatch_types.StatusCode{"target5xx": code}}
		target := tasks.ProductionWatchTargetV1{LoadBalancerArn: "arn:aws:elasticloadbalancing:eu-west-1:1:loadbalancer/app/lb/abc"}
		if _, err := readLoadBalancerMetrics(context.Background(), cw, target, productionWatchWindow{600, 720}); err == nil {
			t.Errorf("%s: err = nil", code)
		}
	}
}

// fakeECS serves one service and its stopped tasks.
type fakeECS struct {
	taskDefinition string
	stopped        []ecs_types.Task
	failures       []ecs_types.Failure
	listed         *ecs.ListTasksInput
}

func (f *fakeECS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	return &ecs.DescribeServicesOutput{Services: []ecs_types.Service{{TaskDefinition: aws.String(f.taskDefinition)}}}, nil
}

func (f *fakeECS) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	f.listed = in
	out := &ecs.ListTasksOutput{}
	for range f.stopped {
		out.TaskArns = append(out.TaskArns, "arn:task")
	}
	return out, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return &ecs.DescribeTasksOutput{Tasks: f.stopped, Failures: f.failures}, nil
}

func TestReadCrashedTasks(t *testing.T) {
	stoppedAt := time.Unix(2000, 0)
	f := &fakeECS{taskDefinition: "td:2", stopped: []ecs_types.Task{
		{TaskDefinitionArn: aws.String("td:2"), StoppedAt: &stoppedAt, StopCode: ecs_types.TaskStopCodeEssentialContainerExited},
		{TaskDefinitionArn: aws.String("td:2"), StoppedAt: &stoppedAt, StopCode: ecs_types.TaskStopCodeServiceSchedulerInitiated},
	}}
	target := tasks.ProductionWatchTargetV1{ClusterName: "c", EcsServiceArn: "arn:aws:ecs:eu-west-1:1:service/c/svc"}
	n, reason, err := readCrashedTasks(context.Background(), f, target, time.Unix(1000, 0))
	if err != nil || n != 1 || reason != "EssentialContainerExited" {
		t.Errorf("= %d %q %v", n, reason, err)
	}
	if aws.ToString(f.listed.ServiceName) != "svc" || f.listed.DesiredStatus != ecs_types.DesiredStatusStopped {
		t.Errorf("ListTasks input = %+v", f.listed)
	}
}

// A stopped task ECS couldn't describe fails the read rather than lowering the
// count.
func TestReadCrashedTasksFailsOnDescribeFailures(t *testing.T) {
	stoppedAt := time.Unix(2000, 0)
	f := &fakeECS{taskDefinition: "td:2",
		stopped:  []ecs_types.Task{{TaskDefinitionArn: aws.String("td:2"), StoppedAt: &stoppedAt, StopCode: ecs_types.TaskStopCodeEssentialContainerExited}},
		failures: []ecs_types.Failure{{Arn: aws.String("arn:task"), Reason: aws.String("MISSING")}}}
	target := tasks.ProductionWatchTargetV1{ClusterName: "c", EcsServiceArn: "arn:aws:ecs:eu-west-1:1:service/c/svc"}
	if _, _, err := readCrashedTasks(context.Background(), f, target, time.Unix(1000, 0)); err == nil {
		t.Error("err = nil")
	}
}

// fakeWatchAWS returns per-poll numbers for every target.
type fakeWatchAWS struct {
	traffic   productionWatchTraffic
	unhealthy []int // per poll; the last repeats
	crashed   int
	failRead  bool
	polls     int
}

func (f *fakeWatchAWS) loadBalancerMetrics(context.Context, tasks.ProductionWatchTargetV1, productionWatchWindow) (productionWatchTraffic, error) {
	if f.failRead {
		return productionWatchTraffic{}, errors.New("AccessDenied: arn:secret")
	}
	return f.traffic, nil
}

func (f *fakeWatchAWS) cloudFrontMetrics(ctx context.Context, t tasks.ProductionWatchTargetV1, w productionWatchWindow) (productionWatchTraffic, error) {
	return f.loadBalancerMetrics(ctx, t, w)
}

func (f *fakeWatchAWS) unhealthyTargets(context.Context, tasks.ProductionWatchTargetV1) (int, error) {
	f.polls++
	if len(f.unhealthy) == 0 {
		return 0, nil
	}
	return f.unhealthy[min(f.polls-1, len(f.unhealthy)-1)], nil
}

func (f *fakeWatchAWS) crashedTasks(context.Context, tasks.ProductionWatchTargetV1, time.Time) (int, string, error) {
	return f.crashed, "EssentialContainerExited", nil
}

// watchHarness runs a RunProductionWatch with a fake clock, fake AWS, fake
// GETs and a fake reporter.
type watchHarness struct {
	clock   time.Time
	aws     *fakeWatchAWS
	status  func(url string, poll int) int
	reply   func(update tasks.UpdateTaskProductionWatchDtoV1, n int) (bool, error)
	reports []tasks.UpdateTaskProductionWatchDtoV1
	waits   []time.Duration
	stop    chan struct{}
	logs    bytes.Buffer
}

func newWatchHarness(start int64) *watchHarness {
	return &watchHarness{clock: time.Unix(start, 0), aws: &fakeWatchAWS{}, stop: make(chan struct{})}
}

func (h *watchHarness) run(t *testing.T, targets []tasks.ProductionWatchTargetV1) error {
	t.Helper()
	raw, _ := json.Marshal(targets)
	params := map[string]interface{}{}
	_ = jobs.SetParameterValue(params, parameters_enums.OrganizationIDNamespace, "org")
	_ = jobs.SetParameterValue(params, parameters_enums.TaskID, "task")
	_ = jobs.SetParameterValue(params, parameters_enums.JobID, "job")
	_ = jobs.SetParameterValue(params, parameters_enums.ProductionWatchTargets, string(raw))
	return h.runWith(params)
}

func (h *watchHarness) runWith(params map[string]interface{}) error {
	polls := map[string]int{}
	rw := &RunProductionWatch{
		now: func() time.Time { return h.clock },
		wait: func(ctx context.Context, d time.Duration) bool {
			if ctx.Err() != nil {
				return false
			}
			h.waits = append(h.waits, d)
			h.clock = h.clock.Add(d)
			return true
		},
		newAWS: func(context.Context, []tasks.ProductionWatchTargetV1) (productionWatchAWS, error) { return h.aws, nil },
		get: func(_ context.Context, u string) (int, error) {
			polls[u]++
			if h.status != nil {
				if s := h.status(u, polls[u]); s != 0 {
					return s, nil
				}
				return 0, context.DeadlineExceeded
			}
			return 200, nil
		},
		sendWatch: func(org string, u tasks.UpdateTaskProductionWatchDtoV1) (bool, error) {
			h.reports = append(h.reports, u)
			if h.reply != nil {
				return h.reply(u, len(h.reports))
			}
			return false, nil
		},
	}
	rw.SetStopSignal(h.stop)
	_, err := rw.Run(params, &h.logs)
	return err
}

func webTarget(endedAt int64) tasks.ProductionWatchTargetV1 {
	return tasks.ProductionWatchTargetV1{
		Repository: "0-acme/api", Kind: tasks.ProductionWatchKindWebService, Region: region_enums.EuWest1,
		LoadBalancerArn: "arn:aws:elasticloadbalancing:eu-west-1:1:loadbalancer/app/lb/abc",
		TargetGroupArn:  "arn:tg", EcsServiceArn: "arn:aws:ecs:eu-west-1:1:service/c/api", ClusterName: "c",
		URLs:            []string{"https://api.example.com/health?k=v"},
		DeployStartedAt: endedAt - 300, DeployEndedAt: endedAt,
	}
}

func TestProductionWatchLoopReportsEveryMinuteAndDoneAtTheEnd(t *testing.T) {
	const start = 100020
	h := newWatchHarness(start)
	h.aws.traffic = productionWatchTraffic{Requests: 1000, Errors5xx: 1, P95Ms: 80}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(start)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 31 {
		t.Fatalf("reports = %d, want 31", len(h.reports))
	}
	for i, r := range h.reports {
		if r.TaskID != "task" || r.JobID != "job" || r.Report.WindowEndsAt != start+1800 {
			t.Fatalf("report %d = %+v", i, r)
		}
		if r.Report.PolledAt != start+int64(i)*60 {
			t.Errorf("report %d polled at %d", i, r.Report.PolledAt)
		}
		wantState := tasks.ProductionWatchStateRunning
		if i == 30 {
			wantState = tasks.ProductionWatchStateDone
		}
		if r.Report.State != wantState || (wantState == tasks.ProductionWatchStateRunning) != (r.Report.Verdict == "") {
			t.Errorf("report %d state %s verdict %q", i, r.Report.State, r.Report.Verdict)
		}
	}
	// The first poll's "after" is under a minute: no metrics read.
	if h.reports[0].Report.Targets[0].RequestsAfter != 0 || h.reports[1].Report.Targets[0].RequestsAfter != 1000 {
		t.Errorf("requests = %d, %d", h.reports[0].Report.Targets[0].RequestsAfter, h.reports[1].Report.Targets[0].RequestsAfter)
	}
	if v := h.reports[30].Report.Verdict; v != tasks.ProductionWatchVerdictNoChange {
		t.Errorf("verdict = %s", v)
	}
	logs := h.logs.String()
	if strings.Contains(logs, "api.example.com") || strings.Contains(logs, "arn:") {
		t.Errorf("logs name a host or an ARN:\n%s", logs)
	}
	if !strings.Contains(logs, "Production watch: acme/api: requests 1000 (before 1000), 5xx 1 (before 1), p95 80 ms (before 80 ms), unhealthy 0, crashed 0, failing URLs 0") ||
		!strings.Contains(logs, "verdict no_change") {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestProductionWatchLoopStartedAfterTheWindowPollsOnce(t *testing.T) {
	h := newWatchHarness(100000 + 1800 + 120)
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].Report.State != tasks.ProductionWatchStateDone {
		t.Fatalf("reports = %+v", h.reports)
	}
}

// A Job that starts past WindowEndsAt + 5 minutes would run past the cap even
// for one poll: it ends without reading or reporting.
func TestProductionWatchLoopStartedPastTheCapDoesNothing(t *testing.T) {
	h := newWatchHarness(100000 + 1800 + 300)
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 0 || h.aws.polls != 0 {
		t.Errorf("reports = %d, reads = %d", len(h.reports), h.aws.polls)
	}
}

// A report that never answers doesn't hold the watch past the stop signal.
func TestProductionWatchLoopStopSignalEndsAStalledReport(t *testing.T) {
	h := newWatchHarness(100000)
	never := make(chan struct{})
	defer close(never)
	h.reply = func(_ tasks.UpdateTaskProductionWatchDtoV1, n int) (bool, error) {
		if n == 2 {
			close(h.stop)
			<-never
		}
		return false, nil
	}
	done := make(chan error, 1)
	go func() { done <- h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch waited on a stalled report after the stop signal")
	}
}

// A logs writer that blocks doesn't hold the watch: lines queue, and once the
// writer recovers every one of them arrives, in order.
func TestProductionWatchLogDoesNotBlockOrDrop(t *testing.T) {
	b := &recordingBlockingWriter{release: make(chan struct{})}
	l := newProductionWatchLog(b)
	const n = 5000
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			l.Write([]byte(fmt.Sprintf("line %d\n", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked")
	}
	close(b.release)
	l.close()
	if len(b.lines) != n {
		t.Fatalf("lines = %d, want %d", len(b.lines), n)
	}
	for i, line := range b.lines {
		if line != fmt.Sprintf("line %d\n", i) {
			t.Fatalf("line %d = %q", i, line)
		}
	}
	if _, err := l.Write([]byte("late\n")); err == nil {
		t.Error("a write after close succeeded")
	}
}

// recordingBlockingWriter blocks every write until release is closed, then
// records it.
type recordingBlockingWriter struct {
	release chan struct{}
	lines   []string
}

func (b *recordingBlockingWriter) Write(p []byte) (int, error) {
	<-b.release
	b.lines = append(b.lines, string(p))
	return len(p), nil
}

func TestProductionWatchLoopStopInAReplyEndsIt(t *testing.T) {
	h := newWatchHarness(100000)
	h.reply = func(_ tasks.UpdateTaskProductionWatchDtoV1, n int) (bool, error) { return n == 3, nil }
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 3 {
		t.Errorf("reports = %d, want 3", len(h.reports))
	}
}

func TestProductionWatchLoopStopSignalEndsItWithoutAFinalReport(t *testing.T) {
	h := newWatchHarness(100000)
	h.reply = func(_ tasks.UpdateTaskProductionWatchDtoV1, n int) (bool, error) {
		if n == 2 {
			close(h.stop)
		}
		return false, nil
	}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(h.reports))
	}
	for _, r := range h.reports {
		if r.Report.State != tasks.ProductionWatchStateRunning {
			t.Errorf("a %s report after the stop signal", r.Report.State)
		}
	}
}

func TestProductionWatchLoopCarriesOnAfterAFailedRunningReport(t *testing.T) {
	h := newWatchHarness(100000)
	h.reply = func(_ tasks.UpdateTaskProductionWatchDtoV1, n int) (bool, error) {
		if n == 2 {
			return false, errors.New("disconnected")
		}
		return false, nil
	}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 31 || h.reports[30].Report.State != tasks.ProductionWatchStateDone {
		t.Errorf("reports = %d", len(h.reports))
	}
}

func TestProductionWatchLoopRetriesTheDoneReport(t *testing.T) {
	h := newWatchHarness(100000 + 1800 + 120)
	h.reply = func(tasks.UpdateTaskProductionWatchDtoV1, int) (bool, error) { return false, errors.New("down") }
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(100000)}); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1+productionWatchDoneRetries {
		t.Errorf("done reports sent = %d, want %d", len(h.reports), 1+productionWatchDoneRetries)
	}
	for _, d := range h.waits {
		if d != productionWatchDoneRetryWait {
			t.Errorf("wait = %s", d)
		}
	}
}

func TestProductionWatchLoopSignals(t *testing.T) {
	const start = 100020
	h := newWatchHarness(start)
	h.aws.unhealthy = []int{1, 0, 2, 2, 0}
	h.aws.traffic = productionWatchTraffic{Requests: 1000, Errors5xx: 100}
	// The URL fails on polls 2 and 4 (not in a row), then 6 and 7.
	h.status = func(_ string, poll int) int {
		switch poll {
		case 2, 4, 6:
			return 503
		case 7:
			return 0
		}
		return 200
	}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(start)}); err != nil {
		t.Fatal(err)
	}
	seenAt := func(r tasks.ProductionWatchReportDtoV1, kind string) (int64, string) {
		for _, s := range r.Signals {
			if s.Kind == kind {
				return s.SeenAt, s.Detail
			}
		}
		return 0, ""
	}
	// errors_up: before rate equals the after rate (10%), so it's not raised.
	if at, _ := seenAt(h.reports[30].Report, tasks.ProductionWatchSignalErrorsUp); at != 0 {
		t.Errorf("errors_up raised at %d", at)
	}
	// unhealthy_targets: polls 3 and 4 (index 2, 3) in a row.
	if at, d := seenAt(h.reports[3].Report, tasks.ProductionWatchSignalUnhealthyTargets); at != start+180 || d != "2 unhealthy targets" {
		t.Errorf("unhealthy_targets = %d %q", at, d)
	}
	if at, _ := seenAt(h.reports[2].Report, tasks.ProductionWatchSignalUnhealthyTargets); at != 0 {
		t.Error("unhealthy_targets raised after one bad poll")
	}
	// url_failing: polls 6 and 7 (index 5, 6).
	if at, _ := seenAt(h.reports[5].Report, tasks.ProductionWatchSignalURLFailing); at != 0 {
		t.Error("url_failing raised before two failures in a row")
	}
	if at, d := seenAt(h.reports[6].Report, tasks.ProductionWatchSignalURLFailing); at != start+360 || d != "GET /health got no answer (timed out)" {
		t.Errorf("url_failing = %d %q", at, d)
	}
	if got := h.reports[5].Report.Targets[0].FailingURLs; len(got) != 1 {
		t.Errorf("failing URLs = %v", got)
	}
	if got := h.reports[7].Report.Targets[0].FailingURLs; len(got) != 0 {
		t.Errorf("failing URLs after recovery = %v", got)
	}
	// Cumulative: every later report carries both, once each.
	for i := 6; i < len(h.reports); i++ {
		if n := len(h.reports[i].Report.Signals); n != 2 {
			t.Fatalf("report %d has %d signals", i, n)
		}
	}
	if v := h.reports[30].Report.Verdict; v != tasks.ProductionWatchVerdictErrorsUp {
		t.Errorf("verdict = %s", v)
	}
	if n := strings.Count(h.logs.String(), "unhealthy_targets:"); n != 1 {
		t.Errorf("unhealthy_targets logged %d times", n)
	}
}

func TestProductionWatchLoopFailedReadKeepsNumbers(t *testing.T) {
	const start = 100020
	h := newWatchHarness(start)
	h.aws.traffic = productionWatchTraffic{Requests: 1000, Errors5xx: 1}
	h.reply = func(_ tasks.UpdateTaskProductionWatchDtoV1, n int) (bool, error) {
		if n == 2 {
			// From the third poll on every read fails, with numbers that
			// would raise errors_up had they been read.
			h.aws.failRead = true
			h.aws.traffic = productionWatchTraffic{Requests: 10, Errors5xx: 10}
		}
		return false, nil
	}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(start)}); err != nil {
		t.Fatal(err)
	}
	last := h.reports[len(h.reports)-1].Report
	if last.Targets[0].RequestsAfter != 1000 || last.Targets[0].Errors5xxAfter != 1 {
		t.Errorf("numbers = %+v, want the second poll's", last.Targets[0])
	}
	if len(last.Signals) != 0 {
		t.Errorf("signals = %+v", last.Signals)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "acme/api: couldn't read the load balancer metrics") || strings.Contains(logs, "AccessDenied") {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestProductionWatchLoopCrashAndLowTraffic(t *testing.T) {
	const start = 100020
	h := newWatchHarness(start)
	h.aws.crashed = 2
	private := tasks.ProductionWatchTargetV1{Repository: "1-acme/worker", Kind: tasks.ProductionWatchKindPrivateService,
		Region: region_enums.EuWest1, EcsServiceArn: "arn:aws:ecs:eu-west-1:1:service/c/worker", ClusterName: "c", DeployEndedAt: start}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{private}); err != nil {
		t.Fatal(err)
	}
	r := h.reports[0].Report
	if len(r.Signals) != 1 || r.Signals[0].Kind != tasks.ProductionWatchSignalTaskCrashed ||
		r.Signals[0].Detail != "2 tasks of this version stopped (EssentialContainerExited)" || r.Signals[0].Repository != "1-acme/worker" {
		t.Errorf("signals = %+v", r.Signals)
	}

	h = newWatchHarness(start)
	h.aws.traffic = productionWatchTraffic{Requests: 10}
	if err := h.run(t, []tasks.ProductionWatchTargetV1{webTarget(start)}); err != nil {
		t.Fatal(err)
	}
	if v := h.reports[len(h.reports)-1].Report.Verdict; v != tasks.ProductionWatchVerdictNotEnoughTraffic {
		t.Errorf("verdict = %s", v)
	}
}

func TestProductionWatchRefusesBadParameters(t *testing.T) {
	base := func() map[string]interface{} {
		p := map[string]interface{}{}
		_ = jobs.SetParameterValue(p, parameters_enums.OrganizationIDNamespace, "org")
		_ = jobs.SetParameterValue(p, parameters_enums.TaskID, "task")
		_ = jobs.SetParameterValue(p, parameters_enums.JobID, "job")
		return p
	}
	cases := map[string]map[string]interface{}{}
	cases["no targets"] = base()
	p := base()
	_ = jobs.SetParameterValue(p, parameters_enums.ProductionWatchTargets, "not json")
	cases["targets that don't parse"] = p
	p = base()
	_ = jobs.SetParameterValue(p, parameters_enums.ProductionWatchTargets, "[]")
	cases["an empty array"] = p
	p = map[string]interface{}{}
	_ = jobs.SetParameterValue(p, parameters_enums.ProductionWatchTargets, `[{"repository":"0-a/b","kind":"web_service"}]`)
	cases["no ids"] = p
	for name, params := range cases {
		h := newWatchHarness(100000)
		if err := h.runWith(params); err == nil {
			t.Errorf("%s: err = nil", name)
		}
		if len(h.reports) != 0 {
			t.Errorf("%s: reported", name)
		}
		if n := strings.Count(h.logs.String(), "\n"); n != 1 {
			t.Errorf("%s: %d log lines", name, n)
		}
	}
}
