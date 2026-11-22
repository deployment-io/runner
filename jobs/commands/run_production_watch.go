package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatch_types "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecs_types "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
)

// RunProductionWatch is the only command of a production watch Job: for
// tasks.ProductionWatchWindowSeconds after a Task's deploy it polls the
// deployed targets' AWS metrics and production URLs about once a minute and
// reports each poll (Jobs.UpdateTaskProductionWatchV1). The final poll, at the
// window's end, carries the verdict.
//
// It reads metrics and GETs only: no container, no checkout, no logs. It never
// touches the Task's working directory (common.go skips MarkStepDone for it),
// and its Job has no output.
type RunProductionWatch struct {
	stopSignal <-chan struct{}

	// Tests only; nil is the real thing.
	now       func() time.Time
	wait      func(ctx context.Context, d time.Duration) bool
	newAWS    func(ctx context.Context, targets []tasks.ProductionWatchTargetV1) (productionWatchAWS, error)
	get       func(ctx context.Context, rawURL string) (int, error)
	sendWatch func(organizationID string, update tasks.UpdateTaskProductionWatchDtoV1) (bool, error)
}

func (rw *RunProductionWatch) SetStopSignal(stop <-chan struct{}) { rw.stopSignal = stop }

const (
	productionWatchPollInterval = 60 * time.Second
	// productionWatchOvertime is how long past its window a watch may run.
	productionWatchOvertime = 5 * time.Minute
	// The done report is retried productionWatchDoneRetries times,
	// productionWatchDoneRetryWait apart.
	productionWatchDoneRetries   = 3
	productionWatchDoneRetryWait = 10 * time.Second
	// productionWatchMinTraffic is the fewest requests after the deploy that
	// tell anything.
	productionWatchMinTraffic = 50
	// The errors_up rule: at least productionWatchErrorsMin 5xx after the
	// deploy, at a rate of at least productionWatchErrorsMinRate, and at
	// least productionWatchErrorsFactor times the rate before.
	productionWatchErrorsMin = 5
	// The minimum rate is 1/productionWatchErrorsMinRateDen (1%).
	productionWatchErrorsMinRateDen = 100
	productionWatchErrorsFactor     = 3
)

func (rw *RunProductionWatch) Run(parameters map[string]interface{}, jobLogsWriter io.Writer) (map[string]interface{}, error) {
	logs := newProductionWatchLog(jobLogsWriter)
	defer logs.close()
	var logsWriter io.Writer = logs
	w, err := rw.newWatch(parameters, logsWriter)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Production watch: %s\n", err))
		return parameters, err
	}
	if !w.now().Before(time.Unix(w.deadline, 0)) {
		// Even the one poll of a late Job would run past the cap.
		io.WriteString(logsWriter, "Production watch: the window ended more than 5 minutes ago; nothing to watch\n")
		return parameters, nil
	}
	stopCtx, cancelStop := context.WithCancel(context.Background())
	defer cancelStop()
	go func() {
		select {
		case <-rw.stopSignal:
			cancelStop()
		case <-stopCtx.Done():
		}
	}()
	// The cap is relative to the clock so a fake one in tests keeps it.
	ctx, cancel := context.WithTimeout(stopCtx, time.Unix(w.deadline, 0).Sub(w.now()))
	defer cancel()
	reader, err := w.rw.newAWSReader(ctx, w.targets)
	if err != nil {
		io.WriteString(logsWriter, "Production watch: couldn't load the AWS configuration\n")
		return parameters, errors.New("couldn't load the AWS configuration")
	}
	w.aws = reader
	w.run(ctx)
	return parameters, nil
}

// productionWatch is one Job's watch: its targets, their numbers so far and
// the signals raised.
type productionWatch struct {
	rw           *RunProductionWatch
	logsWriter   io.Writer
	orgID        string
	taskID       string
	jobID        string
	targets      []tasks.ProductionWatchTargetV1
	states       []*productionWatchTargetState
	signals      productionWatchSignals
	start        int64
	windowEndsAt int64
	// deadline is when the watch ends whatever happens.
	deadline int64
	aws      productionWatchAWS
}

// productionWatchTargetState is one target's numbers so far and what its
// two-in-a-row signals saw at the previous poll.
type productionWatchTargetState struct {
	report        tasks.ProductionWatchTargetReportDtoV1
	wasUnhealthy  bool
	failingBefore map[string]bool
}

func (rw *RunProductionWatch) newWatch(parameters map[string]interface{}, logsWriter io.Writer) (*productionWatch, error) {
	orgID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	taskID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
	jobID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	if orgID == "" || taskID == "" || jobID == "" {
		return nil, errors.New("the production watch has no organization, task or job id")
	}
	targets, err := parseProductionWatchTargets(parameters)
	if err != nil {
		return nil, err
	}
	w := &productionWatch{rw: rw, logsWriter: logsWriter, orgID: orgID, taskID: taskID, jobID: jobID, targets: targets}
	w.start = w.now().Unix()
	w.windowEndsAt = productionWatchWindowEnd(targets, w.start)
	w.deadline = w.windowEndsAt + int64(productionWatchOvertime/time.Second)
	for _, t := range targets {
		w.states = append(w.states, &productionWatchTargetState{report: tasks.ProductionWatchTargetReportDtoV1{Repository: t.Repository}})
	}
	return w, nil
}

// parseProductionWatchTargets reads the ProductionWatchTargets parameter: a
// non-empty JSON array of targets, each with a repository and a known kind.
func parseProductionWatchTargets(parameters map[string]interface{}) ([]tasks.ProductionWatchTargetV1, error) {
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.ProductionWatchTargets)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, errors.New("the production watch has no targets")
	}
	var targets []tasks.ProductionWatchTargetV1
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return nil, errors.New("the production watch targets are not valid JSON")
	}
	if len(targets) == 0 {
		return nil, errors.New("the production watch has no targets")
	}
	for _, t := range targets {
		switch t.Kind {
		case tasks.ProductionWatchKindWebService, tasks.ProductionWatchKindPrivateService, tasks.ProductionWatchKindStaticSite:
		default:
			return nil, errors.New("a production watch target has an unknown kind")
		}
		if strings.TrimSpace(t.Repository) == "" {
			return nil, errors.New("a production watch target has no repository")
		}
	}
	return targets, nil
}

func (w *productionWatch) now() time.Time {
	if w.rw.now != nil {
		return w.rw.now()
	}
	return time.Now()
}

func (rw *RunProductionWatch) newAWSReader(ctx context.Context, targets []tasks.ProductionWatchTargetV1) (productionWatchAWS, error) {
	if rw.newAWS != nil {
		return rw.newAWS(ctx, targets)
	}
	reader, err := newAWSProductionWatchReader(ctx, targets)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

// stopped is true once the stop signal has fired.
func (w *productionWatch) stopped() bool {
	select {
	case <-w.rw.stopSignal:
		return true
	default:
		return false
	}
}

// sleep waits d, and is false when ctx (the stop signal or the cap) ends first.
func (w *productionWatch) sleep(ctx context.Context, d time.Duration) bool {
	if w.rw.wait != nil {
		return w.rw.wait(ctx, d)
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// send reports one poll, giving up when ctx (the stop signal or the cap) ends
// first; the call itself carries on in the background, its result dropped.
func (w *productionWatch) send(ctx context.Context, update tasks.UpdateTaskProductionWatchDtoV1) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	type sent struct {
		stop bool
		err  error
	}
	result := make(chan sent, 1)
	go func() {
		var r sent
		if w.rw.sendWatch != nil {
			r.stop, r.err = w.rw.sendWatch(w.orgID, update)
		} else {
			r.stop, r.err = client.Get().UpdateTaskProductionWatch(w.orgID, update)
		}
		result <- r
	}()
	select {
	case r := <-result:
		return r.stop, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// run polls at start, then every productionWatchPollInterval, until the
// window ends; that poll is the final one and reports done. It ends at once on
// the stop signal (with no report), on Stop in a reply, or at the deadline.
func (w *productionWatch) run(ctx context.Context) {
	for {
		now := w.now().Unix()
		final := now >= w.windowEndsAt
		w.poll(ctx, now)
		if w.stopped() {
			io.WriteString(w.logsWriter, "Production watch: stopped\n")
			return
		}
		report := w.report(now, final)
		update := tasks.UpdateTaskProductionWatchDtoV1{TaskID: w.taskID, JobID: w.jobID, Report: report}
		if final {
			io.WriteString(w.logsWriter, fmt.Sprintf("Production watch: verdict %s\n", report.Verdict))
			w.sendDone(ctx, update)
			return
		}
		stop, err := w.send(ctx, update)
		if err != nil {
			io.WriteString(w.logsWriter, "Production watch: couldn't send this poll's report; the next one carries it\n")
		} else if stop {
			io.WriteString(w.logsWriter, "Production watch: deployment.io ended the watch\n")
			return
		}
		next := min(now+int64(productionWatchPollInterval/time.Second), w.windowEndsAt)
		if !w.sleep(ctx, time.Unix(next, 0).Sub(w.now())) || w.stopped() {
			if w.stopped() {
				io.WriteString(w.logsWriter, "Production watch: stopped\n")
			} else {
				io.WriteString(w.logsWriter, "Production watch: ran out of time\n")
			}
			return
		}
	}
}

// sendDone sends the done report, retrying productionWatchDoneRetries times.
func (w *productionWatch) sendDone(ctx context.Context, update tasks.UpdateTaskProductionWatchDtoV1) {
	for attempt := 0; ; attempt++ {
		if _, err := w.send(ctx, update); err == nil {
			return
		}
		if attempt >= productionWatchDoneRetries || ctx.Err() != nil {
			io.WriteString(w.logsWriter, "Production watch: couldn't send the final report\n")
			return
		}
		if !w.sleep(ctx, productionWatchDoneRetryWait) || w.stopped() {
			io.WriteString(w.logsWriter, "Production watch: couldn't send the final report before the watch ended\n")
			return
		}
	}
}

// report is the cumulative report of the poll at now.
func (w *productionWatch) report(now int64, final bool) tasks.ProductionWatchReportDtoV1 {
	r := tasks.ProductionWatchReportDtoV1{
		State:        tasks.ProductionWatchStateRunning,
		Signals:      append([]tasks.ProductionWatchSignalDtoV1(nil), w.signals.list...),
		PolledAt:     now,
		WindowEndsAt: w.windowEndsAt,
	}
	for _, s := range w.states {
		t := s.report
		t.FailingURLs = append([]string(nil), s.report.FailingURLs...)
		r.Targets = append(r.Targets, t)
	}
	if final {
		r.State = tasks.ProductionWatchStateDone
		r.Verdict = productionWatchVerdict(w.targets, r.Targets, len(w.signals.list) > 0)
	}
	return r
}

// poll reads every target's numbers at now, raises the signals they show and
// logs them.
func (w *productionWatch) poll(ctx context.Context, now int64) {
	for i, t := range w.targets {
		if ctx.Err() != nil {
			return
		}
		s := w.states[i]
		name := productionQARepositoryName(t.Repository)
		switch t.Kind {
		case tasks.ProductionWatchKindWebService:
			w.pollTraffic(ctx, t, s, now, w.aws.loadBalancerMetrics, "the load balancer metrics")
			w.pollUnhealthy(ctx, t, s, now)
			w.pollCrashed(ctx, t, s, now)
			w.pollURLs(ctx, t, s, now)
		case tasks.ProductionWatchKindPrivateService:
			w.pollCrashed(ctx, t, s, now)
		case tasks.ProductionWatchKindStaticSite:
			w.pollTraffic(ctx, t, s, now, w.aws.cloudFrontMetrics, "the CloudFront metrics")
			w.pollURLs(ctx, t, s, now)
		}
		r := s.report
		io.WriteString(w.logsWriter, fmt.Sprintf("Production watch: %s: requests %d (before %d), 5xx %d (before %d), p95 %d ms (before %d ms), unhealthy %d, crashed %d, failing URLs %d\n",
			name, r.RequestsAfter, r.RequestsBefore, r.Errors5xxAfter, r.Errors5xxBefore, r.P95MsAfter, r.P95MsBefore,
			r.UnhealthyTargets, r.CrashedTasks, len(r.FailingURLs)))
	}
}

// productionWatchTraffic is one window's request numbers.
type productionWatchTraffic struct {
	Requests  int
	Errors5xx int
	P95Ms     int
}

type productionWatchMetricsRead func(ctx context.Context, t tasks.ProductionWatchTargetV1, window productionWatchWindow) (productionWatchTraffic, error)

// pollTraffic reads the target's numbers after and before the deploy, and
// raises errors_up when they show it. A failed read keeps the previous numbers.
func (w *productionWatch) pollTraffic(ctx context.Context, t tasks.ProductionWatchTargetV1, s *productionWatchTargetState, now int64,
	read productionWatchMetricsRead, what string) {
	after, before := productionWatchWindows(t, w.start, now)
	if after.length() < 60 {
		return
	}
	if t.Kind == tasks.ProductionWatchKindWebService && t.LoadBalancerArn == "" ||
		t.Kind == tasks.ProductionWatchKindStaticSite && t.CloudfrontDistributionID == "" {
		return
	}
	a, err := read(ctx, t, after)
	if err != nil {
		w.logReadFailed(t, what)
		return
	}
	b, err := read(ctx, t, before)
	if err != nil {
		w.logReadFailed(t, what)
		return
	}
	r := &s.report
	r.RequestsAfter, r.Errors5xxAfter, r.P95MsAfter = a.Requests, a.Errors5xx, a.P95Ms
	r.RequestsBefore, r.Errors5xxBefore, r.P95MsBefore = b.Requests, b.Errors5xx, b.P95Ms
	if productionWatchErrorsUp(*r) {
		w.raise(t, tasks.ProductionWatchSignalErrorsUp, productionWatchErrorsDetail(*r), now)
	}
}

// pollUnhealthy counts the target group's unhealthy targets; two polls in a
// row with any raise unhealthy_targets.
func (w *productionWatch) pollUnhealthy(ctx context.Context, t tasks.ProductionWatchTargetV1, s *productionWatchTargetState, now int64) {
	if t.TargetGroupArn == "" {
		return
	}
	n, err := w.aws.unhealthyTargets(ctx, t)
	if err != nil {
		w.logReadFailed(t, "the target health")
		return
	}
	s.report.UnhealthyTargets = n
	if productionWatchTwoInARow(&s.wasUnhealthy, n > 0) {
		w.raise(t, tasks.ProductionWatchSignalUnhealthyTargets, fmt.Sprintf("%d unhealthy targets", n), now)
	}
}

// pollCrashed counts the tasks of the service's current version that crashed
// since the deploy; any raise task_crashed.
func (w *productionWatch) pollCrashed(ctx context.Context, t tasks.ProductionWatchTargetV1, s *productionWatchTargetState, now int64) {
	if t.EcsServiceArn == "" || t.ClusterName == "" {
		return
	}
	since := t.DeployEndedAt
	if since == 0 {
		since = w.start
	}
	n, reason, err := w.aws.crashedTasks(ctx, t, time.Unix(since, 0))
	if err != nil {
		w.logReadFailed(t, "the service's stopped tasks")
		return
	}
	s.report.CrashedTasks = n
	if n > 0 {
		w.raise(t, tasks.ProductionWatchSignalTaskCrashed, fmt.Sprintf("%d tasks of this version stopped (%s)", n, reason), now)
	}
}

// pollURLs GETs every URL of the target once; the same URL failing on two
// polls in a row raises url_failing.
func (w *productionWatch) pollURLs(ctx context.Context, t tasks.ProductionWatchTargetV1, s *productionWatchTargetState, now int64) {
	get := w.rw.get
	if get == nil {
		get = getProductionQAHealth
	}
	failing := map[string]bool{}
	var failingURLs []string
	for _, u := range t.URLs {
		if ctx.Err() != nil {
			// Stopped or out of time: what wasn't checked isn't failing.
			break
		}
		status, err := get(ctx, u)
		if err == nil && status >= productionQAHealthMinStatus && status <= productionQAHealthMaxStatus {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		failing[u] = true
		failingURLs = append(failingURLs, u)
		if s.failingBefore[u] {
			detail := fmt.Sprintf("GET %s answered %d", productionWatchURLPath(u), status)
			if err != nil {
				detail = fmt.Sprintf("GET %s got no answer (%s)", productionWatchURLPath(u), healthCheckError(err))
			}
			w.raise(t, tasks.ProductionWatchSignalURLFailing, detail, now)
		}
	}
	s.failingBefore = failing
	s.report.FailingURLs = failingURLs
}

func (w *productionWatch) logReadFailed(t tasks.ProductionWatchTargetV1, what string) {
	io.WriteString(w.logsWriter, fmt.Sprintf("Production watch: %s: couldn't read %s\n", productionQARepositoryName(t.Repository), what))
}

func (w *productionWatch) raise(t tasks.ProductionWatchTargetV1, kind, detail string, now int64) {
	if w.signals.raise(t.Repository, kind, detail, now) {
		io.WriteString(w.logsWriter, fmt.Sprintf("Production watch: %s: %s: %s\n", productionQARepositoryName(t.Repository), kind, detail))
	}
}

// productionWatchSignals are the signals raised so far, the first of each
// Repository and Kind, in the order raised.
type productionWatchSignals struct {
	list []tasks.ProductionWatchSignalDtoV1
}

// raise records a signal unless one of its Repository and Kind was raised
// before, and reports whether it did.
func (s *productionWatchSignals) raise(repository, kind, detail string, seenAt int64) bool {
	for _, existing := range s.list {
		if existing.Repository == repository && existing.Kind == kind {
			return false
		}
	}
	s.list = append(s.list, tasks.ProductionWatchSignalDtoV1{Repository: repository, Kind: kind, Detail: detail, SeenAt: seenAt})
	return true
}

// productionWatchTwoInARow records whether this poll is bad and reports
// whether the previous one was too.
func productionWatchTwoInARow(previous *bool, bad bool) bool {
	both := *previous && bad
	*previous = bad
	return both
}

// productionWatchWindowEnd is the latest DeployEndedAt among the targets (0
// counts as the Job's start) plus tasks.ProductionWatchWindowSeconds.
func productionWatchWindowEnd(targets []tasks.ProductionWatchTargetV1, jobStart int64) int64 {
	var latest int64
	for _, t := range targets {
		ended := t.DeployEndedAt
		if ended == 0 {
			ended = jobStart
		}
		latest = max(latest, ended)
	}
	return latest + tasks.ProductionWatchWindowSeconds
}

// productionWatchWindow is [Start, End) in unix seconds.
type productionWatchWindow struct {
	Start int64
	End   int64
}

func (w productionWatchWindow) length() int64 { return w.End - w.Start }

// productionWatchWindows are a target's windows at now: after is
// [DeployEndedAt, now] and before the window of the same length ending at
// DeployStartedAt, both rounded down to the minute. A DeployEndedAt of 0 is the
// Job's start; a DeployStartedAt of 0 is the deploy's end.
func productionWatchWindows(t tasks.ProductionWatchTargetV1, jobStart, now int64) (after, before productionWatchWindow) {
	ended := t.DeployEndedAt
	if ended == 0 {
		ended = jobStart
	}
	started := t.DeployStartedAt
	if started == 0 || started > ended {
		started = ended
	}
	after = productionWatchWindow{Start: floorMinute(ended), End: floorMinute(now)}
	if after.End < after.Start {
		after.End = after.Start
	}
	before = productionWatchWindow{End: floorMinute(started)}
	before.Start = before.End - after.length()
	return after, before
}

func floorMinute(unix int64) int64 { return unix - ((unix%60)+60)%60 }

// productionWatchRate is errors per request as the fraction num/den; errors
// with no requests count as every request failing.
func productionWatchRate(errors5xx, requests int) (num, den int64) {
	if requests <= 0 {
		if errors5xx > 0 {
			return 1, 1
		}
		return 0, 1
	}
	return int64(errors5xx), int64(requests)
}

// productionWatchErrorsUp is the errors_up rule on a target's numbers. The
// rates are compared as fractions, so a rate exactly at a boundary passes.
func productionWatchErrorsUp(r tasks.ProductionWatchTargetReportDtoV1) bool {
	if r.Errors5xxAfter < productionWatchErrorsMin {
		return false
	}
	afterNum, afterDen := productionWatchRate(r.Errors5xxAfter, r.RequestsAfter)
	beforeNum, beforeDen := productionWatchRate(r.Errors5xxBefore, r.RequestsBefore)
	// big.Int: the cross products of two int64 counts can overflow int64.
	mul := func(a ...int64) *big.Int {
		p := big.NewInt(1)
		for _, n := range a {
			p.Mul(p, big.NewInt(n))
		}
		return p
	}
	return mul(afterNum, productionWatchErrorsMinRateDen).Cmp(mul(afterDen)) >= 0 &&
		mul(afterNum, beforeDen).Cmp(mul(productionWatchErrorsFactor, beforeNum, afterDen)) >= 0
}

func productionWatchErrorsDetail(r tasks.ProductionWatchTargetReportDtoV1) string {
	num, den := productionWatchRate(r.Errors5xxAfter, r.RequestsAfter)
	pct := strconv.FormatFloat(100*float64(num)/float64(den), 'f', 1, 64)
	return fmt.Sprintf("%d 5xx of %d requests since the deploy (%s%%), %d of %d before",
		r.Errors5xxAfter, r.RequestsAfter, pct, r.Errors5xxBefore, r.RequestsBefore)
}

// productionWatchVerdict is the final report's verdict. Latency is never
// judged.
func productionWatchVerdict(targets []tasks.ProductionWatchTargetV1, reports []tasks.ProductionWatchTargetReportDtoV1, anySignal bool) string {
	if anySignal {
		return tasks.ProductionWatchVerdictErrorsUp
	}
	for _, r := range reports {
		if productionWatchErrorsUp(r) {
			return tasks.ProductionWatchVerdictErrorsUp
		}
	}
	withTraffic, lowTraffic := 0, 0
	for i, t := range targets {
		if t.Kind != tasks.ProductionWatchKindWebService && t.Kind != tasks.ProductionWatchKindStaticSite {
			continue
		}
		withTraffic++
		if i < len(reports) && reports[i].RequestsAfter < productionWatchMinTraffic {
			lowTraffic++
		}
	}
	if withTraffic > 0 && lowTraffic == withTraffic {
		return tasks.ProductionWatchVerdictNotEnoughTraffic
	}
	return tasks.ProductionWatchVerdictNoChange
}

// productionWatchURLPath is a URL's path only, never its host or query.
func productionWatchURLPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.EscapedPath() == "" {
		return "/"
	}
	return u.EscapedPath()
}

// loadBalancerDimension is the LoadBalancer dimension of an ALB's metrics: the
// part of its ARN after "loadbalancer/".
func loadBalancerDimension(arn string) string {
	const marker = "loadbalancer/"
	if i := strings.Index(arn, marker); i >= 0 {
		return arn[i+len(marker):]
	}
	return ""
}

// ecsServiceNameFromArn is the service name: the last segment of its ARN.
func ecsServiceNameFromArn(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return ""
}

// productionWatchAWS is the watch's AWS reads.
type productionWatchAWS interface {
	loadBalancerMetrics(ctx context.Context, t tasks.ProductionWatchTargetV1, window productionWatchWindow) (productionWatchTraffic, error)
	cloudFrontMetrics(ctx context.Context, t tasks.ProductionWatchTargetV1, window productionWatchWindow) (productionWatchTraffic, error)
	unhealthyTargets(ctx context.Context, t tasks.ProductionWatchTargetV1) (int, error)
	crashedTasks(ctx context.Context, t tasks.ProductionWatchTargetV1, since time.Time) (int, string, error)
}

// The AWS API calls the watch makes, as small interfaces for fakes.
type watchCloudWatchAPI interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

type watchECSAPI interface {
	DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	ListTasks(ctx context.Context, in *ecs.ListTasksInput, optFns ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(ctx context.Context, in *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
}

type watchELBAPI interface {
	DescribeTargetHealth(ctx context.Context, in *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error)
}

type watchRegionClients struct {
	cloudwatch watchCloudWatchAPI
	ecs        watchECSAPI
	elb        watchELBAPI
}

// awsProductionWatchReader reads through clients built once per Job.
type awsProductionWatchReader struct {
	regions    map[string]watchRegionClients
	cloudfront watchCloudWatchAPI
}

// newAWSProductionWatchReader loads the AWS config once and builds every
// client from it: one set per target region, and one CloudWatch client in
// us-east-1 for CloudFront's metrics.
func newAWSProductionWatchReader(ctx context.Context, targets []tasks.ProductionWatchTargetV1) (*awsProductionWatchReader, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	reader := &awsProductionWatchReader{
		regions:    map[string]watchRegionClients{},
		cloudfront: cloudwatch.NewFromConfig(cfg, func(o *cloudwatch.Options) { o.Region = cloudfrontRegion }),
	}
	for _, t := range targets {
		region := t.Region.String()
		if region == "" {
			continue
		}
		if _, ok := reader.regions[region]; ok {
			continue
		}
		reader.regions[region] = watchRegionClients{
			cloudwatch: cloudwatch.NewFromConfig(cfg, func(o *cloudwatch.Options) { o.Region = region }),
			ecs:        ecs.NewFromConfig(cfg, func(o *ecs.Options) { o.Region = region }),
			elb:        elasticloadbalancingv2.NewFromConfig(cfg, func(o *elasticloadbalancingv2.Options) { o.Region = region }),
		}
	}
	return reader, nil
}

var errProductionWatchNoRegion = errors.New("no clients for the target's region")

func (a *awsProductionWatchReader) region(t tasks.ProductionWatchTargetV1) (watchRegionClients, error) {
	c, ok := a.regions[t.Region.String()]
	if !ok {
		return c, errProductionWatchNoRegion
	}
	return c, nil
}

func watchMetricQuery(id, namespace, metric, stat string, period int32, dimensions ...cloudwatch_types.Dimension) cloudwatch_types.MetricDataQuery {
	return cloudwatch_types.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &cloudwatch_types.MetricStat{
			Metric: &cloudwatch_types.Metric{Namespace: aws.String(namespace), MetricName: aws.String(metric), Dimensions: dimensions},
			Period: aws.Int32(period),
			Stat:   aws.String(stat),
		},
		ReturnData: aws.Bool(true),
	}
}

// getWatchMetricData runs the queries over window and returns each query's
// datapoints by timestamp. A query CloudWatch couldn't answer (InternalError,
// Forbidden), or more pages than it reads, fails the whole read: an empty
// result would otherwise read as 0.
func getWatchMetricData(ctx context.Context, c watchCloudWatchAPI, window productionWatchWindow,
	queries []cloudwatch_types.MetricDataQuery) (map[string]map[int64]float64, error) {
	start, end := time.Unix(window.Start, 0), time.Unix(window.End, 0)
	in := &cloudwatch.GetMetricDataInput{StartTime: &start, EndTime: &end, MetricDataQueries: queries}
	values := map[string]map[int64]float64{}
	for page := 0; page < 10; page++ {
		out, err := c.GetMetricData(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, r := range out.MetricDataResults {
			if r.StatusCode != cloudwatch_types.StatusCodeComplete && r.StatusCode != cloudwatch_types.StatusCodePartialData {
				return nil, errors.New("a metric query wasn't answered")
			}
			id := aws.ToString(r.Id)
			if values[id] == nil {
				values[id] = map[int64]float64{}
			}
			for i, ts := range r.Timestamps {
				if i < len(r.Values) {
					values[id][ts.Unix()] = r.Values[i]
				}
			}
		}
		if out.NextToken == nil {
			return values, nil
		}
		in.NextToken = out.NextToken
	}
	return nil, errors.New("too many pages of metrics")
}

func sumWatchValues(values map[int64]float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum
}

// loadBalancerMetrics reads the window's requests, 5xx (target + ELB) and p95
// response time from the target's load balancer, in one GetMetricData call.
//
// The window has its own call because a GetMetricData call has one time
// range: the p95's single period must start at the window's start.
func (a *awsProductionWatchReader) loadBalancerMetrics(ctx context.Context, t tasks.ProductionWatchTargetV1, window productionWatchWindow) (productionWatchTraffic, error) {
	c, err := a.region(t)
	if err != nil {
		return productionWatchTraffic{}, err
	}
	return readLoadBalancerMetrics(ctx, c.cloudwatch, t, window)
}

func readLoadBalancerMetrics(ctx context.Context, c watchCloudWatchAPI, t tasks.ProductionWatchTargetV1, window productionWatchWindow) (productionWatchTraffic, error) {
	const namespace = "AWS/ApplicationELB"
	dim := cloudwatch_types.Dimension{Name: aws.String("LoadBalancer"), Value: aws.String(loadBalancerDimension(t.LoadBalancerArn))}
	p95Period := int32((window.length() + 59) / 60 * 60)
	values, err := getWatchMetricData(ctx, c, window, []cloudwatch_types.MetricDataQuery{
		watchMetricQuery("requests", namespace, "RequestCount", "Sum", 60, dim),
		watchMetricQuery("target5xx", namespace, "HTTPCode_Target_5XX_Count", "Sum", 60, dim),
		watchMetricQuery("elb5xx", namespace, "HTTPCode_ELB_5XX_Count", "Sum", 60, dim),
		watchMetricQuery("p95", namespace, "TargetResponseTime", "p95", p95Period, dim),
	})
	if err != nil {
		return productionWatchTraffic{}, err
	}
	traffic := productionWatchTraffic{
		Requests:  int(math.Round(sumWatchValues(values["requests"]))),
		Errors5xx: int(math.Round(sumWatchValues(values["target5xx"]) + sumWatchValues(values["elb5xx"]))),
	}
	// One period covers the window; take the highest should CloudWatch split it.
	for _, seconds := range values["p95"] {
		traffic.P95Ms = max(traffic.P95Ms, int(math.Round(seconds*1000)))
	}
	return traffic, nil
}

// cloudFrontMetrics reads the window's requests and 5xx from the target's
// distribution; CloudFront publishes no 5xx count, so it is the sum over the
// window's minutes of Requests × 5xxErrorRate / 100.
func (a *awsProductionWatchReader) cloudFrontMetrics(ctx context.Context, t tasks.ProductionWatchTargetV1, window productionWatchWindow) (productionWatchTraffic, error) {
	const namespace = "AWS/CloudFront"
	dims := []cloudwatch_types.Dimension{
		{Name: aws.String("DistributionId"), Value: aws.String(t.CloudfrontDistributionID)},
		{Name: aws.String("Region"), Value: aws.String("Global")},
	}
	values, err := getWatchMetricData(ctx, a.cloudfront, window, []cloudwatch_types.MetricDataQuery{
		watchMetricQuery("requests", namespace, "Requests", "Sum", 60, dims...),
		watchMetricQuery("rate5xx", namespace, "5xxErrorRate", "Average", 60, dims...),
	})
	if err != nil {
		return productionWatchTraffic{}, err
	}
	return productionWatchTraffic{
		Requests:  int(math.Round(sumWatchValues(values["requests"]))),
		Errors5xx: cloudFront5xx(values["requests"], values["rate5xx"]),
	}, nil
}

// cloudFront5xx is the sum over the periods of requests × 5xx rate (a
// percentage) / 100, rounded.
func cloudFront5xx(requests, ratePct map[int64]float64) int {
	var sum float64
	for ts, rate := range ratePct {
		sum += requests[ts] * rate / 100
	}
	return int(math.Round(sum))
}

// unhealthyTargets counts the target group's targets in state "unhealthy".
func (a *awsProductionWatchReader) unhealthyTargets(ctx context.Context, t tasks.ProductionWatchTargetV1) (int, error) {
	c, err := a.region(t)
	if err != nil {
		return 0, err
	}
	out, err := c.elb.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(t.TargetGroupArn)})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range out.TargetHealthDescriptions {
		if d.TargetHealth != nil && d.TargetHealth.State == "unhealthy" {
			n++
		}
	}
	return n, nil
}

// crashedTasks counts the service's stopped tasks of its current task
// definition that crashed at or after since (first page of stopped tasks).
func (a *awsProductionWatchReader) crashedTasks(ctx context.Context, t tasks.ProductionWatchTargetV1, since time.Time) (int, string, error) {
	c, err := a.region(t)
	if err != nil {
		return 0, "", err
	}
	return readCrashedTasks(ctx, c.ecs, t, since)
}

func readCrashedTasks(ctx context.Context, c watchECSAPI, t tasks.ProductionWatchTargetV1, since time.Time) (int, string, error) {
	services, err := c.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(t.ClusterName), Services: []string{t.EcsServiceArn}})
	if err != nil {
		return 0, "", err
	}
	if len(services.Services) == 0 || aws.ToString(services.Services[0].TaskDefinition) == "" {
		return 0, "", errors.New("service not found")
	}
	taskDefinition := aws.ToString(services.Services[0].TaskDefinition)
	list, err := c.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:       aws.String(t.ClusterName),
		ServiceName:   aws.String(ecsServiceNameFromArn(t.EcsServiceArn)),
		DesiredStatus: ecs_types.DesiredStatusStopped,
	})
	if err != nil {
		return 0, "", err
	}
	if len(list.TaskArns) == 0 {
		return 0, "", nil
	}
	described, err := c.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(t.ClusterName), Tasks: list.TaskArns})
	if err != nil {
		return 0, "", err
	}
	if len(described.Failures) > 0 {
		// A task it couldn't describe may be a crash: keep the previous count.
		return 0, "", errors.New("some stopped tasks weren't described")
	}
	n, reason := classifyCrashedTasks(described.Tasks, taskDefinition, since)
	return n, reason, nil
}

// classifyCrashedTasks counts the tasks of taskDefinition that stopped at or
// after since because an essential container exited, the task failed to
// start or a container ran out of memory — a scheduler stop (scale-in, a
// deployment) isn't a crash. reason is the first crash's stop code, or
// OutOfMemory.
func classifyCrashedTasks(stopped []ecs_types.Task, taskDefinition string, since time.Time) (int, string) {
	n, reason := 0, ""
	for _, task := range stopped {
		if aws.ToString(task.TaskDefinitionArn) != taskDefinition || task.StoppedAt == nil || task.StoppedAt.Before(since) {
			continue
		}
		why := ""
		for _, container := range task.Containers {
			if strings.Contains(aws.ToString(container.Reason), "OutOfMemory") {
				why = "OutOfMemory"
				break
			}
		}
		if why == "" && (task.StopCode == ecs_types.TaskStopCodeEssentialContainerExited || task.StopCode == ecs_types.TaskStopCodeTaskFailedToStart) {
			why = string(task.StopCode)
		}
		if why == "" {
			continue
		}
		n++
		if reason == "" {
			reason = why
		}
	}
	return n, reason
}

// productionWatchLog writes the watch's log lines to the Job's logs writer
// from its own goroutine, queueing them without limit: that writer blocks
// while deployment.io is unreachable, and the watch must still end on its stop
// signal and its cap. No line is dropped.
type productionWatchLog struct {
	mu     sync.Mutex
	cond   *sync.Cond
	lines  []string
	closed bool
	done   chan struct{}
}

func newProductionWatchLog(w io.Writer) *productionWatchLog {
	l := &productionWatchLog{done: make(chan struct{})}
	l.cond = sync.NewCond(&l.mu)
	go func() {
		defer close(l.done)
		for {
			l.mu.Lock()
			for len(l.lines) == 0 && !l.closed {
				l.cond.Wait()
			}
			if len(l.lines) == 0 {
				l.mu.Unlock()
				return
			}
			line := l.lines[0]
			l.lines = l.lines[1:]
			l.mu.Unlock()
			io.WriteString(w, line)
		}
	}()
	return l
}

func (l *productionWatchLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, io.ErrClosedPipe
	}
	l.lines = append(l.lines, string(p))
	l.cond.Signal()
	return len(p), nil
}

// close waits until every queued line is in the Job's logs writer, so none is
// lost when the Job closes that writer. While deployment.io is unreachable
// that is as long as any Job's last line waits; the watch itself has already
// ended. Nothing may write after it.
func (l *productionWatchLog) close() {
	l.mu.Lock()
	l.closed = true
	l.cond.Signal()
	l.mu.Unlock()
	<-l.done
}
