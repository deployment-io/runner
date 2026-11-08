package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// recordingEventsRPC stands in for deployment-server's Jobs.AppendTaskEventsV1.
type recordingEventsRPC struct {
	mu      sync.Mutex
	batches []tasks.AppendTaskEventsArgsV1
	delay   time.Duration
}

func (r *recordingEventsRPC) AppendTaskEvents(_ string, args tasks.AppendTaskEventsArgsV1) error {
	time.Sleep(r.delay)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, args)
	return nil
}

func (r *recordingEventsRPC) events() []tasks.TaskEventV1 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []tasks.TaskEventV1
	for _, b := range r.batches {
		out = append(out, b.Events...)
	}
	return out
}

// recordTaskEvents points new senders at a recording server and returns it.
func recordTaskEvents(t *testing.T) *recordingEventsRPC {
	t.Helper()
	rpc := &recordingEventsRPC{}
	original := commandUtils.TaskEventsRPC
	commandUtils.TaskEventsRPC = func() commandUtils.TaskEventsClient { return rpc }
	t.Cleanup(func() { commandUtils.TaskEventsRPC = original })
	return rpc
}

// sentEvents closes the job's sender and returns every event it sent.
func sentEvents(t *testing.T, rpc *recordingEventsRPC, jobID string) []tasks.TaskEventV1 {
	t.Helper()
	commandUtils.CloseTaskEvents(jobID, 5*time.Second)
	return rpc.events()
}

func eventTypes(events []tasks.TaskEventV1) []string {
	var out []string
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s/%s/%d", e.Type, e.Stage, e.Round))
	}
	return out
}

// --- MarkStepDone ---------------------------------------------------------------

// MarkStepDone is every Step exit path's cleanup, and each path waits for it
// before the job's result is sent: so the sender is closed — its events sent —
// by the time it is done, whatever the Job's error and whatever it lacks.
func TestMarkStepDoneClosesTheJobsSenderBeforeItIsDone(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		orgID string
		task  string
	}{
		{name: "success", orgID: "org-1", task: "task-1"},
		{name: "error", err: errors.New("error running agentbox"), orgID: "org-1", task: "task-1"},
		{name: "stop", err: types.ErrJobStoppedByUser, orgID: "org-1", task: "task-1"},
		{name: "failed command", err: errors.New("error getting command"), orgID: "org-1", task: "task-1"},
		{name: "no org id", orgID: "", task: "task-1"},
		{name: "no task id", orgID: "org-1", task: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpc := recordTaskEvents(t)
			rpc.delay = 100 * time.Millisecond
			jobID := "job-" + strings.ReplaceAll(tc.name, " ", "-")
			parameters := map[string]interface{}{}
			jobs.SetParameterValue(parameters, parameters_enums.JobID, jobID)
			if tc.orgID != "" {
				jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, tc.orgID)
			}
			if tc.task != "" {
				jobs.SetParameterValue(parameters, parameters_enums.TaskID, tc.task)
			}
			sender := commandUtils.TaskEvents(commandUtils.TaskJobContext{OrganizationID: "org-1", TaskID: "task-1"}, jobID)
			sender.Emit(implementFinishedEvent(agentResult{Turns: 3}))

			<-MarkStepDone(parameters, tc.err)

			if got := rpc.events(); len(got) != 1 || got[0].Type != tasks.TaskEventImplementFinished {
				t.Fatalf("sent before MarkStepDone was done: %v", eventTypes(got))
			}
			if commandUtils.TaskEvents(commandUtils.TaskJobContext{TaskID: "task-1"}, jobID) == sender {
				t.Error("the sender is still registered for the job")
			}
			commandUtils.CloseTaskEvents(jobID, time.Second)
		})
	}
}

// --- checkout and implement ------------------------------------------------------

func TestCheckoutDoneEvent(t *testing.T) {
	e := checkoutDoneEvent([]tasks.RepositoryEntry{{Name: "acme/web"}, {Name: "acme/api"}})
	if e.Type != tasks.TaskEventCheckoutDone || e.Stage != "setup" || e.Round != 0 || e.Repo != "" {
		t.Errorf("envelope = %+v", e)
	}
	if !reflect.DeepEqual(e.Checkout.Repos, []string{"acme/web", "acme/api"}) {
		t.Errorf("repos = %v", e.Checkout.Repos)
	}
	if e.Now != "Setting up · checked out 2 repositories" {
		t.Errorf("now = %q", e.Now)
	}
	if !e.Bound() {
		t.Error("deployment-server would drop the event")
	}
}

func TestSelfCheckPayload(t *testing.T) {
	cases := []struct {
		vr   *verifyResult
		want *tasks.TaskEventSelfCheckV1
	}{
		{nil, nil},
		{&verifyResult{Ran: true, Passed: true, Command: "go test ./...", DurationMs: 12400, StdoutTail: "ok"},
			&tasks.TaskEventSelfCheckV1{Commands: []string{"go test ./..."}, Passed: true, Summary: "passed in 12s"}},
		{&verifyResult{Ran: true, Passed: false, Command: "npm test", DurationMs: 61000, StderrTail: "FAIL secret-output"},
			&tasks.TaskEventSelfCheckV1{Commands: []string{"npm test"}, Passed: false, Summary: "failed in 1m1s"}},
		{&verifyResult{Ran: false, SkippedReason: "docs-only change"},
			&tasks.TaskEventSelfCheckV1{Passed: false, Summary: "docs-only change"}},
	}
	for _, tc := range cases {
		if got := selfCheckPayload(tc.vr); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("selfCheckPayload(%+v) = %+v, want %+v", tc.vr, got, tc.want)
		}
	}
	if _, ok := selfCheckEvent(tasks.TaskEventStageImplement, 0, nil); ok {
		t.Error("a run without a verify_result got a self_check event")
	}
	e, ok := selfCheckEvent(tasks.TaskEventStageImplement, 0, cases[1].vr)
	if !ok || e.Type != tasks.TaskEventSelfCheck || e.Stage != "implement" || !e.Bound() {
		t.Errorf("self_check = %+v", e)
	}
}

func TestImplementEvents(t *testing.T) {
	start := stageStartedEvent(tasks.TaskEventStageImplement, 0, implementStartedNow)
	if start.Type != tasks.TaskEventStageStarted || start.Stage != "implement" || start.Now != "Implement · the agent is working" || !start.Bound() {
		t.Errorf("stage_started = %+v", start)
	}
	e := implementFinishedEvent(agentResult{Turns: 14, ChangesSummary: "Added the export endpoint",
		TokenUsage: tokenUsage{InputTokens: 1200, OutputTokens: 300}})
	want := &tasks.TaskEventImplementV1{Turns: 14, InputTokens: 1200, OutputTokens: 300, ChangesSummary: "Added the export endpoint"}
	if !reflect.DeepEqual(e.Implement, want) || e.Now != "Implement · done in 14 turns" || e.Stage != "implement" || !e.Bound() {
		t.Errorf("implement_finished = %+v / %+v", e, e.Implement)
	}
	if got := implementOutcome(nil); got != "Done" {
		t.Errorf("outcome = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := implementOutcome(fmt.Errorf("agent step did not succeed: %s\nsecond line", long)); !strings.HasPrefix(got, "Failed: agent step did not succeed: ") ||
		strings.Contains(got, "second line") || len([]rune(strings.TrimPrefix(got, "Failed: "))) != taskEventOutcomeMaxRunes {
		t.Errorf("outcome = %q", got)
	}
}

// --- review ---------------------------------------------------------------------

func TestReviewRoundFinishedEventCarriesOnlyNewFindings(t *testing.T) {
	// Round 2: sec-1 (from round 1) is held — in the round's list but only
	// reported through previous — and perf-1 is new.
	findings := []reviewFindingOutput{
		{Key: "sec-1", Parameter: "security", Severity: "high", What: "no session check", MustFix: true, SentBack: true, Held: true},
		{Key: "perf-1", Parameter: "performance", Severity: "low", Location: "api/list.go:9", What: "N+1 query", Why: "slow"},
	}
	result := agentResult{ReviewResult: &reviewResult{
		Previous: []reviewPreviousFinding{{Key: "sec-1", Status: "still_present", Note: "still unchecked"}, {Key: "sec-2", Status: "Resolved"}, {Key: "odd", Status: "unknown"}},
		Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}, {Parameter: "tests", State: "not checked", Reason: "no tests"}},
	}}
	e := reviewRoundFinishedEvent(2, findings, map[string]bool{"sec-1": true, "sec-2": true}, result)
	if e.Type != tasks.TaskEventReviewRoundFinished || e.Stage != "review" || e.Round != 2 {
		t.Errorf("envelope = %+v", e)
	}
	wantFindings := []tasks.TaskEventFindingV1{{Key: "perf-1", Parameter: "performance", Severity: "low", Location: "api/list.go:9", What: "N+1 query", Why: "slow"}}
	if !reflect.DeepEqual(e.ReviewRound.Findings, wantFindings) {
		t.Errorf("findings = %+v", e.ReviewRound.Findings)
	}
	wantPrevious := []tasks.TaskEventFindingStatusV1{{Key: "sec-1", Status: "still_present"}, {Key: "sec-2", Status: "resolved"}, {Key: "odd", Status: "still_present"}}
	if !reflect.DeepEqual(e.ReviewRound.Previous, wantPrevious) {
		t.Errorf("previous = %+v", e.ReviewRound.Previous)
	}
	if len(e.ReviewRound.Coverage) != 2 || e.ReviewRound.Coverage[1] != (tasks.TaskEventCoverageV1{Parameter: "tests", State: "not checked", Reason: "no tests"}) {
		t.Errorf("coverage = %+v", e.ReviewRound.Coverage)
	}
	if e.Now != "Review · round 2: 2 findings (1 must-fix)" {
		t.Errorf("now = %q", e.Now)
	}
	if !e.Bound() {
		t.Error("deployment-server would drop the event")
	}

	// A round whose earlier findings are only in previous: nothing new.
	held := reviewRoundFinishedEvent(2, findings[:1], map[string]bool{"sec-1": true}, result)
	if len(held.ReviewRound.Findings) != 0 || len(held.ReviewRound.Previous) != 3 {
		t.Errorf("held-only round = %+v", held.ReviewRound)
	}
}

func TestFixRoundFinishedEvent(t *testing.T) {
	run := &agentResult{ChangesSummary: "Checked the session", VerifyResult: &verifyResult{Ran: true, Passed: true, Command: "go test ./...", DurationMs: 3000}}
	e := fixRoundFinishedEvent(tasks.TaskEventStageReview, 1, []reviewFindingOutput{{Key: "sec-1"}, {Parameter: "security", Location: "a.go:1", What: "x"}}, run)
	if e.Type != tasks.TaskEventFixRoundFinished || e.Stage != "review" || e.Round != 1 || len(e.FixRound.Addressed) != 2 || e.FixRound.Addressed[0] != "sec-1" {
		t.Errorf("fix_round_finished = %+v / %+v", e, e.FixRound)
	}
	if e.FixRound.ChangesSummary != "Checked the session" || e.FixRound.SelfCheck == nil || e.FixRound.SelfCheck.Summary != "passed in 3s" {
		t.Errorf("payload = %+v", e.FixRound)
	}
	if none := fixRoundFinishedEvent(tasks.TaskEventStageQA, 1, nil, nil); none.FixRound.SelfCheck != nil || none.FixRound.ChangesSummary != "" || !none.Bound() {
		t.Errorf("a fix that never ran = %+v", none.FixRound)
	}
}

func TestReviewStageOutcome(t *testing.T) {
	completed := reviewRoundOutput{Round: 1, Completed: true}
	cases := []struct {
		out  *reviewOutput
		want string
	}{
		{&reviewOutput{}, "Skipped: " + reviewNoRoundReason},
		{&reviewOutput{Rounds: []reviewRoundOutput{{Round: 1, Error: "no start-of-run commit was recorded for any repository"}}}, "Skipped: no start-of-run commit was recorded for any repository"},
		{&reviewOutput{Rounds: []reviewRoundOutput{completed}, MustFixOpen: true}, "Must-fix open"},
		{&reviewOutput{Rounds: []reviewRoundOutput{completed, completed}, FixedInLoop: []reviewFindingOutput{{Key: "a"}, {Key: "b"}}}, "2 fixed"},
		{&reviewOutput{Rounds: []reviewRoundOutput{completed}}, "No findings"},
	}
	for _, tc := range cases {
		if got := reviewStageOutcome(tc.out, reviewNoRoundReason); got != tc.want {
			t.Errorf("outcome = %q, want %q", got, tc.want)
		}
	}
}

// The Review loop's events, in order: a round that finds a must-fix finding,
// its fix round, and a round that finds it resolved.
func TestTheReviewLoopSendsItsEvents(t *testing.T) {
	rpc := recordTaskEvents(t)
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	writeFile(t, filepath.Join(repoDir, "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, nil)
	stage.ctx = commandUtils.TaskJobContext{OrganizationID: "org-1", TaskID: "task-1"}
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-review-loop")
	first := stage.runReview
	stage.runReview = func(round int) (agentResult, error) {
		if round == 1 {
			return first(round)
		}
		return agentResult{Status: "success", ReviewResult: &reviewResult{
			Previous: []reviewPreviousFinding{{Key: "sec-1", Status: "resolved"}},
			Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}},
		}}, nil
	}
	stage.runFix = func([]reviewFindingOutput) error {
		writeFile(t, filepath.Join(repoDir, "handler.go"), "fixed\n")
		stage.finishedFix = &agentResult{Status: "success", ChangesSummary: "Checked the caller's session",
			VerifyResult: &verifyResult{Ran: true, Passed: true, Command: "go test ./...", DurationMs: 9000}}
		return nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-review-loop")
	want := []string{
		"stage_started/review/1", "review_round_finished/review/1",
		"stage_started/implement/1", "fix_round_finished/review/1",
		"stage_started/review/2", "review_round_finished/review/2",
		"stage_finished/review/0",
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	if events[0].Now != "Review · round 1" || events[2].Now != "Fixing round 1's must-fix findings" {
		t.Errorf("now lines = %q, %q", events[0].Now, events[2].Now)
	}
	r1 := events[1].ReviewRound
	if len(r1.Findings) != 1 || r1.Findings[0].Key != "sec-1" || !r1.Findings[0].MustFix || events[1].Now != "Review · round 1: 1 findings (1 must-fix)" {
		t.Errorf("round 1 = %+v, now %q", r1, events[1].Now)
	}
	fix := events[3].FixRound
	if !reflect.DeepEqual(fix.Addressed, []string{"sec-1"}) || fix.ChangesSummary != "Checked the caller's session" || fix.SelfCheck.Summary != "passed in 9s" {
		t.Errorf("fix round = %+v", fix)
	}
	r2 := events[5].ReviewRound
	if len(r2.Findings) != 0 || !reflect.DeepEqual(r2.Previous, []tasks.TaskEventFindingStatusV1{{Key: "sec-1", Status: "resolved"}}) {
		t.Errorf("round 2 = %+v", r2)
	}
	if events[6].StageDetail.Outcome != "1 fixed" {
		t.Errorf("outcome = %q", events[6].StageDetail.Outcome)
	}
}

// --- QA -------------------------------------------------------------------------

func TestQACriterionAndRoundEvents(t *testing.T) {
	out := qaRoundOutput{Round: 1, Status: tasks.QAStatusCompleted, Criteria: []qaCriterion{
		{Criterion: "The home page shows a Sign up button", State: tasks.QACriterionPassed},
		{Criterion: "Signing up sends a welcome email", State: tasks.QACriterionNotChecked, Reason: "no mail preview"},
		{Criterion: "The pricing page lists three plans", State: tasks.QACriterionFailed, Reason: "two plans"},
	}}
	criteria := qaCriterionEvents(out)
	if len(criteria) != 3 {
		t.Fatalf("%d criterion events", len(criteria))
	}
	for i, e := range criteria {
		if e.Type != tasks.TaskEventQACriterionFinished || e.Stage != "qa" || e.Round != 1 || e.QACriterion.Criterion != i ||
			e.QACriterion.Text != out.Criteria[i].Criterion || e.QACriterion.State != out.Criteria[i].State || e.QACriterion.Reason != out.Criteria[i].Reason || !e.Bound() {
			t.Errorf("criterion %d = %+v / %+v", i, e, e.QACriterion)
		}
	}
	round := qaRoundFinishedEvent(out, false)
	want := tasks.TaskEventQARoundV1{Final: false, Passed: 1, Failed: 1, NotChecked: 1, Summary: "QA round 1: completed — 1 passed, 1 failed, 1 not checked"}
	if *round.QARound != want || round.Now != "QA · round 1: 1 of 3 passed" || round.Type != tasks.TaskEventQARoundFinished || !round.Bound() {
		t.Errorf("qa_round_finished = %+v / %+v", round, round.QARound)
	}
	smoke := qaRoundFinishedEvent(qaRoundOutput{Round: 2, Status: tasks.QAStatusCompleted, NoCriteria: true}, true)
	if !smoke.QARound.Final || smoke.Now != "QA · round 2: completed — no acceptance criteria; checked the preview loads, 0 finding(s)" {
		t.Errorf("smoke round = %+v, now %q", smoke.QARound, smoke.Now)
	}
	if start := qaRoundStartedEvent(2); start.Now != "QA · round 2" || start.Stage != "qa" || start.Round != 2 {
		t.Errorf("stage_started = %+v", start)
	}
}

func TestQAStageOutcome(t *testing.T) {
	passed := qaCriterion{State: tasks.QACriterionPassed}
	cases := []struct {
		out  *qaOutput
		want string
	}{
		{nil, "Skipped: no repository changed"},
		{&qaOutput{Status: tasks.QAStatusSkipped, StatusReason: "no buildable repository changed"}, "Skipped: no buildable repository changed"},
		{&qaOutput{Status: tasks.QAStatusCouldNotRun, StatusReason: "no time left"}, "Couldn't run: no time left"},
		{&qaOutput{Status: tasks.QAStatusCompleted, NoCriteria: true, Rounds: []qaRoundOutput{{Round: 1}}}, "QA: no acceptance criteria; checked the preview loads"},
		{&qaOutput{Status: tasks.QAStatusCompleted, Rounds: []qaRoundOutput{{Round: 1, Criteria: []qaCriterion{passed, {State: tasks.QACriterionFailed}}}, {Round: 2, Criteria: []qaCriterion{passed, passed}}}}, "QA: 2 of 2 passed"},
	}
	for _, tc := range cases {
		if got := qaStageOutcome(tc.out); got != tc.want {
			t.Errorf("outcome = %q, want %q", got, tc.want)
		}
	}
}

// The QA loop's events, in order: round 1 with a failed criterion, its fix
// round, and the final round.
func TestTheQALoopSendsItsEvents(t *testing.T) {
	rpc := recordTaskEvents(t)
	stage, workDir := changedQAStage(t, nil)
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-qa-loop")
	withReviewRecorded(t, stage, participationOff)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed, pricingPassed), qaCompleted(signUpPassed, pricingPassed))
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-qa-loop")
	// Round 1's tally waits until round 2 starts: only then is it known not to
	// be the reported round.
	want := []string{
		"stage_started/qa/1", "qa_criterion_finished/qa/1", "qa_criterion_finished/qa/1",
		"stage_started/implement/1", "fix_round_finished/qa/1", "qa_round_finished/qa/1",
		"stage_started/qa/2", "qa_criterion_finished/qa/2", "qa_criterion_finished/qa/2", "qa_round_finished/qa/2",
		"stage_finished/qa/0",
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	if r1 := events[5]; r1.QARound.Final || r1.QARound.Failed != 1 || r1.Now != "QA · round 1: 1 of 2 passed" {
		t.Errorf("round 1 = %+v, now %q", r1.QARound, r1.Now)
	}
	if fix := events[4].FixRound; fix.ChangesSummary != "added the button" {
		t.Errorf("fix round = %+v", fix)
	}
	if r2 := events[9]; !r2.QARound.Final || r2.QARound.Passed != 2 {
		t.Errorf("round 2 = %+v", r2.QARound)
	}
	if got := events[10].StageDetail.Outcome; got != "QA: 2 of 2 passed" {
		t.Errorf("outcome = %q", got)
	}
}

// A round 1 sent back for a fix that changes nothing is the reported round:
// its tally says Final.
func TestAQARoundWhoseFixIsNotKeptIsFinal(t *testing.T) {
	rpc := recordTaskEvents(t)
	stage, workDir := changedQAStage(t, nil)
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-qa-not-kept")
	withReviewRecorded(t, stage, participationOff)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed, pricingPassed))
	stage.runFix = func(string) (agentResult, error) {
		return agentResult{Status: "success", ChangesSummary: "nothing to change"}, nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-qa-not-kept")
	want := []string{
		"stage_started/qa/1", "qa_criterion_finished/qa/1", "qa_criterion_finished/qa/1",
		"stage_started/implement/1", "fix_round_finished/qa/1", "qa_round_finished/qa/1",
		"stage_finished/qa/0",
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	if r1 := events[5].QARound; !r1.Final || r1.Failed != 1 {
		t.Errorf("round 1 = %+v, want the final round", r1)
	}
}

// A fix that ends in an error (here a user stop) reports no QA result, and
// round 1's tally still goes, not final.
func TestAQARoundWhoseFixIsStoppedStillSendsItsTally(t *testing.T) {
	rpc := recordTaskEvents(t)
	stage, workDir := changedQAStage(t, nil)
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-qa-fix-stopped")
	withReviewRecorded(t, stage, participationOff)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed, pricingPassed))
	stage.runFix = func(string) (agentResult, error) {
		return agentResult{}, types.ErrJobStoppedByUser
	}
	if _, err := stage.run(); err == nil {
		t.Fatal("a stopped fix did not stop the stage")
	}
	events := sentEvents(t, rpc, "job-qa-fix-stopped")
	got := eventTypes(events)
	if len(got) == 0 || got[len(got)-1] != "qa_round_finished/qa/1" {
		t.Fatalf("events = %v; want round 1's tally last", got)
	}
	if r1 := events[len(events)-1].QARound; r1.Final || r1.Failed != 1 {
		t.Errorf("round 1 = %+v, want not final", r1)
	}
}

// The review round the QA stage runs on its fix sends its own round events,
// and the review's outcome again.
func TestTheReviewRoundOnAQAFixSendsItsEvents(t *testing.T) {
	rpc := recordTaskEvents(t)
	stage, workDir := changedQAStage(t, nil)
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-qa-review")
	withReviewRecorded(t, stage, participationOn)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed, pricingPassed), qaCompleted(signUpPassed, pricingPassed))
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-qa-review")
	want := []string{
		"stage_started/qa/1", "qa_criterion_finished/qa/1", "qa_criterion_finished/qa/1",
		"stage_started/implement/1", "fix_round_finished/qa/1",
		"stage_started/review/2", "review_round_finished/review/2", "stage_finished/review/0", "qa_round_finished/qa/1",
		"stage_started/qa/2", "qa_criterion_finished/qa/2", "qa_criterion_finished/qa/2", "qa_round_finished/qa/2",
		"stage_finished/qa/0",
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	round := events[6].ReviewRound
	if len(round.Findings) != 0 || len(round.Coverage) != 1 || round.Coverage[0].Parameter != "security" {
		t.Errorf("review round = %+v", round)
	}
}

func TestAReviewThatIsOffSendsASkippedStage(t *testing.T) {
	rpc := recordTaskEvents(t)
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.JobID, "job-review-off")
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.TaskID, "task-1")
	jobs.SetParameterValue(parameters, parameters_enums.TaskTitle, "Add sign-up")
	jobs.SetParameterValue[int64](parameters, parameters_enums.StepIndex, 0)
	jobs.SetParameterValue(parameters, parameters_enums.TaskBranchName, "tasks/add-sign-up")
	jobs.SetParameterValue(parameters, parameters_enums.Repositories, `[{"name":"acme/site"}]`)
	jobs.SetParameterValue[int64](parameters, parameters_enums.ReviewParticipation, participationOff)
	if _, err := (&RunReviewStage{}).Run(parameters, io.Discard); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-review-off")
	if len(events) != 1 || events[0].Type != tasks.TaskEventStageFinished || events[0].Stage != "review" ||
		events[0].StageDetail.Outcome != "Skipped: running this Step without the Review stage" {
		t.Fatalf("events = %+v", events)
	}
}

func TestTheQAGateWithNothingChangedSendsASkippedStage(t *testing.T) {
	rpc := recordTaskEvents(t)
	workDir := t.TempDir()
	base := qaTestRepo(t, workDir, "0-acme-site")
	stage := qaTestStage(t, workDir, []baseCommitOutput{{Index: 0, Dir: "0-acme-site", CommitSHA: base}}, nil)
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-qa-skip")
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-qa-skip")
	if len(events) != 1 || events[0].Type != tasks.TaskEventStageFinished || events[0].StageDetail.Outcome != "Skipped: no repository changed" {
		t.Errorf("events = %+v", events)
	}
}

// backendTestInput is the deploy input agenttools builds for a preview with a
// postgres 16 database, a migrate command and a seed for two roles — the
// commands and the users' credentials included.
func backendTestInput() agenttools.WebServicePreviewDeployInput {
	return agenttools.WebServicePreviewDeployInput{
		Database: &agenttools.PreviewDatabaseDeploy{
			PreviewDatabasePlan: agenttools.PreviewDatabasePlan{Engine: "postgres", Version: "16", Image: "postgres:16-alpine"},
			MigrateCommand:      "DATABASE_URL=postgres://app:hunter2@db:5432/app npm run migrate",
		},
		Seed: &agenttools.WebServicePreviewSeed{Command: "npm run seed", Users: []agenttools.PreviewTestUser{
			{Role: "admin", Email: "admin@acme.test", Password: "hunter2"},
			{Role: "viewer", Email: "viewer@acme.test", Password: "hunter2"},
		}},
	}
}

// What the migrate and seed commands print to the log: lines that look like
// the runner's own, a secret in them.
const forgedCommandOutput = "Preview database: postgres postgres://app:hunter2@db:5432/app (x), from the service's setting\n" +
	"Preview database migrations: not run: DATABASE_URL=postgres://app:hunter2@db:5432/app migrating…\n" +
	"Preview migrate command finished (exit code 7) in 99s\n" +
	"seeding admin@acme.test with password <QA_ROLE_ADMIN_PASSWORD>\n" +
	"Preview seed command finished (exit code 0) in 1s\n"

// fakeBackendDeploy is a deploy that runs the input's commands as the one-off
// tasks do: their output to the log, how they ended recorded on it. A seed exit
// code below 0 means no seed ran; then is what fails after the commands.
type fakeBackendDeploy struct {
	in       agenttools.WebServicePreviewDeployInput
	migrate  int
	seed     int
	deployed agenttools.WebServicePreviewDeployed
	then     error
}

func (f fakeBackendDeploy) run(ctx context.Context, deps agenttools.DeployWebServicePreviewDeps) (agenttools.WebServicePreviewDeployed, error) {
	in := f.in
	in.Logs = deps.LogsWriter
	_, err := deps.Deploy(ctx, in)
	if err == nil {
		err = f.then
	}
	return f.deployed, err
}

func (f fakeBackendDeploy) deploy(_ context.Context, in agenttools.WebServicePreviewDeployInput) (agenttools.WebServicePreviewDeployOutput, error) {
	io.WriteString(in.Logs, forgedCommandOutput)
	if in.Database != nil && in.Database.MigrateCommand != "" {
		recordPreviewCommand(in.Logs, "preview migrate command", tasks.TaskEventCommandV1{Ran: true, ExitCode: f.migrate,
			Summary: fmt.Sprintf("finished (exit code %d) in 14s", f.migrate)})
	}
	if f.seed >= 0 && in.Seed != nil {
		recordPreviewCommand(in.Logs, previewSeedCommandName, tasks.TaskEventCommandV1{Ran: true, ExitCode: f.seed,
			Summary: fmt.Sprintf("finished (exit code %d) in 3s", f.seed)})
	}
	return agenttools.WebServicePreviewDeployOutput{}, nil
}

func backendTestStage(t *testing.T, logs io.Writer, f fakeBackendDeploy) *qaStage {
	t.Helper()
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.JobID, "job-preview")
	return &qaStage{
		ctx:          commandUtils.TaskJobContext{OrganizationID: "org-1", TaskID: "task-1", Entries: []tasks.RepositoryEntry{{Name: "acme/api"}}},
		parameters:   parameters,
		logsWriter:   logs,
		backends:     []string{"0-acme/api"},
		repositories: []string{"0-acme/web"},
		eventRound:   1,
		webPreviewDeps: func(*agenttools.PreviewRecord) *agenttools.DeployWebServicePreviewDeps {
			return &agenttools.DeployWebServicePreviewDeps{LogsWriter: logs, Deploy: f.deploy}
		},
		deployBackend: func(ctx context.Context, deps agenttools.DeployWebServicePreviewDeps, _ string) (agenttools.WebServicePreviewDeployed, error) {
			return f.run(ctx, deps)
		},
	}
}

// The event says what the deploy did, never what its commands printed: their
// output, faked runner lines included, reaches the job log and nothing else.
func TestABackendPreviewDeploySendsWhatItsDeployDid(t *testing.T) {
	rpc := recordTaskEvents(t)
	var logs strings.Builder
	stage := backendTestStage(t, &logs, fakeBackendDeploy{in: backendTestInput(), migrate: 0, seed: 0,
		deployed: agenttools.WebServicePreviewDeployed{URL: "https://api-preview.example.com", Seed: agenttools.PreviewSeedSucceeded}})
	if _, err := stage.deployBackends("QA round 1", &agenttools.DeployStaticSitePreviewDeps{Record: &agenttools.PreviewRecord{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), forgedCommandOutput) {
		t.Error("the commands' output did not reach the job log")
	}
	events := sentEvents(t, rpc, "job-preview")
	if len(events) != 1 {
		t.Fatalf("events = %v", eventTypes(events))
	}
	e := events[0]
	want := tasks.TaskEventPreviewDeployV1{
		Kind: "backend", URL: "https://api-preview.example.com", OK: true, DatabaseEngine: "postgres 16",
		Migrate: &tasks.TaskEventCommandV1{Ran: true, ExitCode: 0, Summary: "finished (exit code 0) in 14s"},
		Seed:    &tasks.TaskEventCommandV1{Ran: true, ExitCode: 0, Summary: "finished (exit code 0) in 3s"},
		Roles:   []string{"admin", "viewer"},
	}
	if !reflect.DeepEqual(*e.PreviewDeploy, want) {
		t.Errorf("preview deploy = %+v\nwant %+v", *e.PreviewDeploy, want)
	}
	if e.Type != tasks.TaskEventPreviewDeployFinished || e.Stage != "qa" || e.Round != 1 || e.Repo != "acme/api" || e.Now != "QA · preview of acme/api deployed" {
		t.Errorf("envelope = %+v", e)
	}
	assertNoSecrets(t, events)
}

// A deploy that fails after its seed succeeded still names the roles seeded.
func TestAFailedBackendPreviewDeploySendsItsFirstLine(t *testing.T) {
	rpc := recordTaskEvents(t)
	in := backendTestInput()
	in.Database.MigrateCommand = ""
	stage := backendTestStage(t, io.Discard, fakeBackendDeploy{in: in, seed: 0,
		then: errors.New("The backend preview's deploy failed: the service didn't become healthy\nlog tail: password=hunter2")})
	if _, err := stage.deployBackends("QA round 1", &agenttools.DeployStaticSitePreviewDeps{Record: &agenttools.PreviewRecord{}}); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-preview")
	if len(events) != 1 {
		t.Fatalf("events = %v", eventTypes(events))
	}
	p := events[0].PreviewDeploy
	if p.OK || p.URL != "" || p.Error != "The backend preview's deploy failed: the service didn't become healthy" {
		t.Errorf("preview deploy = %+v", p)
	}
	if p.Migrate == nil || p.Migrate.Ran || p.Migrate.Summary != "not run: no preview migrate command is set for this service (set one in Resources → Services)" {
		t.Errorf("migrate = %+v", p.Migrate)
	}
	if p.Seed == nil || !p.Seed.Ran || p.Seed.ExitCode != 0 || !reflect.DeepEqual(p.Roles, []string{"admin", "viewer"}) {
		t.Errorf("seed = %+v, roles %v", p.Seed, p.Roles)
	}
	if events[0].Now != "QA · preview of acme/api failed" {
		t.Errorf("now = %q", events[0].Now)
	}
	assertNoSecrets(t, events)
}

// A seed that failed seeds no roles.
func TestAFailedSeedNamesNoRoles(t *testing.T) {
	_, facts := recordPreviewDeploy(agenttools.DeployWebServicePreviewDeps{})
	facts.start(backendTestInput())
	facts.recordCommand(previewSeedCommandName, tasks.TaskEventCommandV1{Ran: true, ExitCode: 1, Summary: "finished (exit code 1) in 2s"})
	if d := facts.result(agenttools.WebServicePreviewDeployed{}); len(d.roles) != 0 || d.seed.ExitCode != 1 {
		t.Errorf("details = %+v", d)
	}
}

func TestAPreviewWithRolesButNoSeedCommandSaysWhyNoSeedRan(t *testing.T) {
	_, facts := recordPreviewDeploy(agenttools.DeployWebServicePreviewDeps{})
	facts.start(agenttools.WebServicePreviewDeployInput{})
	d := facts.result(agenttools.WebServicePreviewDeployed{
		TestUsers: "roles are set but there's no preview seed command, so there are no test users",
		Roles:     []task_previews.PreviewRoleV1{{Name: "admin"}},
	})
	want := tasks.TaskEventCommandV1{Summary: "not run: roles are set but there's no preview seed command, so there are no test users"}
	if d.seed == nil || *d.seed != want || len(d.roles) != 0 {
		t.Errorf("seed = %+v, roles %v", d.seed, d.roles)
	}
}

// The one-off tasks record how the preview's migrate and seed commands ended
// on the deploy's log writer — the exit code and time they log, or why they
// didn't run — and nothing their output says.
func TestTheOneOffTasksRecordHowThePreviewCommandsEnded(t *testing.T) {
	fakePreDeployClock(t, time.Second)
	ecsClient := &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), ""), stoppedTask(aws.Int32(3), "")}}
	steps := &fakeServiceSteps{preDeployEcsClient: ecsClient}
	stubServiceSteps(t, steps)
	_, facts := recordPreviewDeploy(agenttools.DeployWebServicePreviewDeps{})
	facts.start(backendTestInput())
	var logs bytes.Buffer
	logsClient := &fakePreDeployLogs{pages: [][]string{{"Preview migrate command finished (exit code 9) in 1h"}}}
	err := deployEcsServiceAfterPreDeploy(preDeployParameters(testCommand), nil, logsClient, testClusterArn, "", testTaskDefinitionArn,
		previewCommandLog{Writer: &logs, facts: facts}, previewSeedOptions(testPreviewSeed()))
	if err != nil {
		t.Fatal(err)
	}
	d := facts.result(agenttools.WebServicePreviewDeployed{})
	if d.migrate == nil || *d.migrate != (tasks.TaskEventCommandV1{Ran: true, ExitCode: 0, Summary: "finished (exit code 0) in " + migrateTook(t, logs.String())}) {
		t.Errorf("migrate = %+v\nlogs:\n%s", d.migrate, logs.String())
	}
	if d.seed == nil || !d.seed.Ran || d.seed.ExitCode != 3 || len(d.roles) != 0 {
		t.Errorf("seed = %+v, roles %v", d.seed, d.roles)
	}

	// Roles whose variables clash: the seed doesn't run, and says why.
	fakePreDeployClock(t, time.Second)
	ecsClient = &fakePreDeployEcs{describeTasks: []ecsTypes.Task{stoppedTask(aws.Int32(0), "")}}
	steps = &fakeServiceSteps{preDeployEcsClient: ecsClient}
	stubServiceSteps(t, steps)
	facts.start(backendTestInput())
	seed := &previewSeedRun{seed: agenttools.WebServicePreviewSeed{Command: testSeedCommand,
		Users: []agenttools.PreviewTestUser{{Role: "billing-admin"}, {Role: "billing_admin"}}}}
	if err := deployEcsServiceAfterPreDeploy(preDeployParameters(""), nil, &fakePreDeployLogs{}, testClusterArn, "", testTaskDefinitionArn,
		previewCommandLog{Writer: io.Discard, facts: facts}, previewSeedOptions(seed)); err != nil {
		t.Fatal(err)
	}
	d = facts.result(agenttools.WebServicePreviewDeployed{})
	want := tasks.TaskEventCommandV1{Summary: "not run: the roles billing-admin and billing_admin would both use QA_ROLE_BILLING_ADMIN_EMAIL and QA_ROLE_BILLING_ADMIN_PASSWORD"}
	if d.seed == nil || *d.seed != want {
		t.Errorf("seed = %+v", d.seed)
	}

	// A seed that couldn't start.
	fakePreDeployClock(t, time.Second)
	ecsClient = &fakePreDeployEcs{runTaskErr: errors.New("AccessDeniedException")}
	steps = &fakeServiceSteps{preDeployEcsClient: ecsClient}
	stubServiceSteps(t, steps)
	facts.start(backendTestInput())
	if err := deployEcsServiceAfterPreDeploy(preDeployParameters(""), nil, &fakePreDeployLogs{}, testClusterArn, "", testTaskDefinitionArn,
		previewCommandLog{Writer: io.Discard, facts: facts}, previewSeedOptions(testPreviewSeed())); err != nil {
		t.Fatal(err)
	}
	d = facts.result(agenttools.WebServicePreviewDeployed{})
	if d.seed == nil || d.seed.Ran || d.seed.Summary != "not run: couldn't start: AccessDeniedException" {
		t.Errorf("seed = %+v", d.seed)
	}
}

// migrateTook is the time the migrate command's own "finished" line logs.
func migrateTook(t *testing.T, logs string) string {
	t.Helper()
	const prefix = "Preview migrate command finished (exit code 0) in "
	i := strings.Index(logs, prefix)
	if i < 0 {
		t.Fatalf("no migrate line in\n%s", logs)
	}
	took, _, _ := strings.Cut(logs[i+len(prefix):], "\n")
	return took
}

// Off the stack path the agent deploys the sites: each deploy_static_site_preview
// call that ends deployed or failed is heard, under the repository as the tool
// resolves it; a "building" answer or a call the agent gave up on is not.
func TestObserveSiteDeploys(t *testing.T) {
	srv := agentmcp.New("test", "0")
	answers := []struct {
		out string
		err error
	}{
		{`{"url":"","status":"building"}`, nil},
		{`{"url":"https://d123.cloudfront.net","status":"deployed"}`, nil},
		{"", errors.New("the build failed\nnpm ERR! token=abc")},
	}
	call := 0
	srv.Register(agentmcp.Tool{Name: "deploy_static_site_preview", Handler: func(context.Context, json.RawMessage) (string, error) {
		a := answers[call]
		call++
		return a.out, a.err
	}})
	type heard struct {
		repository, url string
		err             error
	}
	var got []heard
	finish := observeSiteDeploys(srv, func(repository, url string, err error) { got = append(got, heard{repository, url, err}) })
	tool, _ := srv.Lookup("deploy_static_site_preview")
	for range answers {
		tool.Handler(context.Background(), json.RawMessage(`{"repository":"/work/0-acme/web/"}`))
	}
	finish()
	if len(got) != 2 || got[0] != (heard{"0-acme/web", "https://d123.cloudfront.net", nil}) || got[1].err == nil || got[1].url != "" {
		t.Fatalf("heard %+v", got)
	}
	e := previewDeployFinishedEvent(1, previewDeployResult{repo: "acme/web", kind: qaPreviewSite, err: got[1].err})
	if e.PreviewDeploy.OK || e.PreviewDeploy.Error != "the build failed" || e.Now != "QA · preview of acme/web failed" {
		t.Errorf("failed site = %+v, now %q", e.PreviewDeploy, e.Now)
	}
}

// A build the agent left at "building" is heard once the run ends: finish asks
// the tool for its result with the call's own arguments.
func TestObserveSiteDeploysHearsABuildTheAgentNeverCameBackFor(t *testing.T) {
	srv := agentmcp.New("test", "0")
	done := false
	var asked []string
	srv.Register(agentmcp.Tool{Name: "deploy_static_site_preview", Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
		asked = append(asked, string(args))
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case !done:
			return `{"url":"","status":"building"}`, nil
		}
		return `{"url":"https://d9.cloudfront.net","status":"deployed"}`, nil
	}})
	var got []string
	finish := observeSiteDeploys(srv, func(repository, url string, err error) { got = append(got, repository+" "+url) })
	tool, _ := srv.Lookup("deploy_static_site_preview")
	tool.Handler(context.Background(), json.RawMessage(`{"repository":"1-acme/docs","root_directory":"site"}`))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tool.Handler(cancelled, json.RawMessage(`{"repository":"1-acme/docs","root_directory":"site"}`))
	if len(got) != 0 {
		t.Fatalf("heard %v before the build ended", got)
	}
	done = true
	finish()
	if !reflect.DeepEqual(got, []string{"1-acme/docs https://d9.cloudfront.net"}) {
		t.Errorf("heard %v", got)
	}
	if last := asked[len(asked)-1]; last != `{"repository":"1-acme/docs","root_directory":"site"}` {
		t.Errorf("finish asked with %s", last)
	}
	finish()
	if len(got) != 1 {
		t.Errorf("a heard build was heard again: %v", got)
	}
}

// Only a Task repository's directory names a preview.
func TestTaskEntryName(t *testing.T) {
	s := &qaStage{ctx: commandUtils.TaskJobContext{Entries: []tasks.RepositoryEntry{{Name: "acme/web"}, {Name: "acme/api"}}}}
	if name, ok := s.taskEntryName("1-acme/api"); !ok || name != "acme/api" {
		t.Errorf("1-acme/api = %q, %v", name, ok)
	}
	for _, dir := range []string{"work/0-acme/web", "2-acme/api", "acme/web"} {
		if _, ok := s.taskEntryName(dir); ok {
			t.Errorf("%s names a Task repository", dir)
		}
	}
}

// A fix attempt that runs no fix carries no earlier fix's summary.
func TestAFixAttemptThatRunsNoFixForgetsTheLastFixRun(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, nil)
	stage.lastFixRun = &agentResult{ChangesSummary: "the first fix"}
	stage.copyTree = func(string, string) error { return errors.New("no space left on device") }
	if _, err := stage.attemptFix(2, nil); err != nil {
		t.Fatal(err)
	}
	if e := fixRoundFinishedEvent(tasks.TaskEventStageReview, 2, nil, stage.lastFixRun); e.FixRound.ChangesSummary != "" || e.FixRound.SelfCheck != nil {
		t.Errorf("fix round 2 = %+v", e.FixRound)
	}
}

func TestASitePreviewDeployEvent(t *testing.T) {
	e := previewDeployFinishedEvent(2, previewDeployResult{repo: "acme/web", kind: qaPreviewSite, url: "https://d123.cloudfront.net"})
	if e.Round != 2 || e.Repo != "acme/web" || e.PreviewDeploy.Kind != "site" || !e.PreviewDeploy.OK || e.PreviewDeploy.Migrate != nil || !e.Bound() {
		t.Errorf("site deploy = %+v / %+v", e, e.PreviewDeploy)
	}
}

// --- pull requests -------------------------------------------------------------

type eventsPullRequestRPC struct {
	replies map[string]oauth.OpenPullRequestDtoV1
	posted  int
	err     error
}

func (r *eventsPullRequestRPC) OpenPullRequest(_ string, args oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error) {
	if r.err != nil {
		return oauth.OpenPullRequestDtoV1{}, r.err
	}
	return r.replies[args.RepoName], nil
}

func (r *eventsPullRequestRPC) PostPullRequestReview(string, oauth.PostPullRequestReviewArgsV1) (oauth.PostPullRequestReviewDtoV1, error) {
	return oauth.PostPullRequestReviewDtoV1{Posted: r.posted}, nil
}

func eventsTestPR(rpc *eventsPullRequestRPC, jobID string) *taskOpenPR {
	return &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", BranchName: "tasks/x",
			Entries: []tasks.RepositoryEntry{
				{Name: "acme/web", BaseBranch: "main", InstallationID: "inst-1"},
				{Name: "acme/api", BaseBranch: "main", InstallationID: "inst-1"},
				{Name: "acme/docs", BaseBranch: "main", InstallationID: "inst-1"},
			},
		},
		jobID:        jobID,
		logsWriter:   io.Discard,
		agentPRTitle: "Add OAuth login",
		pullRequests: rpc,
	}
}

func TestOpeningPullRequestsSendsTheirEvents(t *testing.T) {
	events := recordTaskEvents(t)
	rpc := &eventsPullRequestRPC{replies: map[string]oauth.OpenPullRequestDtoV1{
		"acme/web": {URL: "https://github.com/acme/web/pull/7", Number: 7},
		"acme/api": {URL: "https://github.com/acme/api/pull/3", Number: 3, AlreadyExisted: true},
	}}
	opr := eventsTestPR(rpc, "job-pr")
	if _, err := opr.openAll(map[int]bool{0: true, 1: true}); err != nil {
		t.Fatal(err)
	}
	got := sentEvents(t, events, "job-pr")
	if types := eventTypes(got); !reflect.DeepEqual(types, []string{"stage_started/pr/0", "pr_opened/pr/0", "pr_opened/pr/0", "stage_finished/pr/0"}) {
		t.Fatalf("events = %v", types)
	}
	if got[0].Now != "Opening pull requests" {
		t.Errorf("now = %q", got[0].Now)
	}
	web := got[1]
	if web.Repo != "acme/web" || web.Now != "Pull request #7 opened" ||
		*web.PullRequest != (tasks.TaskEventPullRequestV1{Number: 7, URL: "https://github.com/acme/web/pull/7", Title: "Add OAuth login"}) {
		t.Errorf("web = %+v / %+v", web, web.PullRequest)
	}
	api := got[2]
	if api.Repo != "acme/api" || api.Now != "Pull request #3 updated" || !api.PullRequest.Updated {
		t.Errorf("api = %+v / %+v", api, api.PullRequest)
	}
	if got[3].StageDetail.Outcome != "1 opened, 1 updated" {
		t.Errorf("outcome = %q", got[3].StageDetail.Outcome)
	}
	assertNoSecrets(t, got)
}

func TestPullRequestStageOutcomes(t *testing.T) {
	events := recordTaskEvents(t)
	opr := eventsTestPR(&eventsPullRequestRPC{}, "job-pr-none")
	if _, err := opr.openAll(map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	got := sentEvents(t, events, "job-pr-none")
	if len(got) != 2 || got[1].StageDetail.Outcome != "No pull request: no repository had changes" {
		t.Errorf("events = %+v", got)
	}

	events = recordTaskEvents(t)
	opr = eventsTestPR(&eventsPullRequestRPC{err: errors.New("422 Validation Failed\nbody: {...}")}, "job-pr-fail")
	if _, err := opr.openAll(map[int]bool{1: true}); err == nil {
		t.Fatal("openAll succeeded")
	}
	got = sentEvents(t, events, "job-pr-fail")
	if len(got) != 2 || got[1].StageDetail.Outcome != "No pull request: error opening PR for repo acme/api: 422 Validation Failed" {
		t.Errorf("events = %+v", got)
	}

	for _, tc := range []struct {
		opened, updated int
		want            string
	}{{2, 0, "2 opened"}, {0, 1, "1 updated"}, {1, 2, "1 opened, 2 updated"}} {
		if got := prStageOutcome(tc.opened, tc.updated, nil); got != tc.want {
			t.Errorf("outcome = %q, want %q", got, tc.want)
		}
	}
}

func TestPROpenedEventDraftAndInlineComments(t *testing.T) {
	e := prOpenedEvent(prOpened{repo: "acme/api", reply: oauth.OpenPullRequestDtoV1{Number: 4, URL: "https://github.com/acme/api/pull/4"},
		title: "[Needs fixes] Add OAuth login", draftRequested: true, inlineComments: 5})
	if !e.PullRequest.Draft || e.PullRequest.InlineComments != 5 || e.PullRequest.Title != "[Needs fixes] Add OAuth login" || !e.Bound() {
		t.Errorf("pr_opened = %+v", e.PullRequest)
	}
	unsupported := prOpenedEvent(prOpened{repo: "acme/api", reply: oauth.OpenPullRequestDtoV1{Number: 4, DraftUnsupported: true}, draftRequested: true})
	if unsupported.PullRequest.Draft {
		t.Error("a draft the provider could not honour was reported as a draft")
	}
}

// --- what never goes in an event -------------------------------------------------

var eventSecrets = []string{"hunter2", "admin@acme.test", "QA_ROLE_ADMIN_PASSWORD", "DATABASE_URL", "sk-ant", "ghs_", "FAIL secret-output", "body:", "migrating…", "Running the preview"}

func assertNoSecrets(t *testing.T, events []tasks.TaskEventV1) {
	t.Helper()
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range eventSecrets {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("an event carries %q: %s", secret, encoded)
		}
	}
}

func TestNoBuiltEventCarriesASecret(t *testing.T) {
	vr := &verifyResult{Ran: true, Passed: false, Command: "go test ./...", StdoutTail: "FAIL secret-output", StderrTail: "DATABASE_URL=postgres://app:hunter2@db"}
	run := agentResult{Turns: 2, ChangesSummary: "Added login", VerifyResult: vr,
		TokenUsage: tokenUsage{InputTokens: 10}, Error: "sk-ant-oops", DeniedHosts: []string{"ghs_token.example"}}
	_, facts := recordPreviewDeploy(agenttools.DeployWebServicePreviewDeps{})
	facts.start(backendTestInput())
	recordPreviewCommandNotRun(previewCommandLog{Writer: io.Discard, facts: facts}, previewSeedCommandName, "The preview seed command couldn't start: AccessDeniedException.")
	details := facts.result(agenttools.WebServicePreviewDeployed{Seed: agenttools.PreviewSeedSucceeded, Roles: []task_previews.PreviewRoleV1{{Name: "admin", Description: "hunter2"}}})
	self, _ := selfCheckEvent(tasks.TaskEventStageImplement, 0, vr)
	events := []tasks.TaskEventV1{
		self,
		implementFinishedEvent(run),
		fixRoundFinishedEvent(tasks.TaskEventStageQA, 1, []reviewFindingOutput{{Key: "qa-1"}}, &run),
		previewDeployFinishedEvent(1, previewDeployResult{repo: "acme/api", kind: qaPreviewBackend, url: "https://x.example.com", details: details}),
		prOpenedEvent(prOpened{repo: "acme/api", reply: oauth.OpenPullRequestDtoV1{Number: 1}, title: "Add login"}),
	}
	assertNoSecrets(t, events)
	for _, e := range events {
		if !e.Bound() {
			t.Errorf("deployment-server would drop %s", e.Type)
		}
	}
}

// On the stack path every preview the round deploys — site or backend, deployed
// or failed — gets its event.
func TestQAStackRoundSendsAnEventPerPreview(t *testing.T) {
	rpc := recordTaskEvents(t)
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links:     []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), originVar("CORS_ORIGINS", ","))},
		deployErr: map[string]error{"backend:1-api": errors.New("The backend preview's deploy failed: the image build failed\nlog tail")}}
	stage, _, _ := stackStage(t, []string{"acme/web", "acme/api"}, []string{"0-web"}, []string{"1-api"}, f)
	stage.ctx.Entries = []tasks.RepositoryEntry{{Name: "web"}, {Name: "api"}}
	jobs.SetParameterValue(stage.parameters, parameters_enums.JobID, "job-stack")
	stage.eventRound = 1
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	events := sentEvents(t, rpc, "job-stack")
	if len(events) != 2 {
		t.Fatalf("events = %v", eventTypes(events))
	}
	backend, site := events[0], events[1]
	if backend.Repo != "api" || backend.PreviewDeploy.Kind != "backend" || backend.PreviewDeploy.OK ||
		backend.PreviewDeploy.Error != "The backend preview's deploy failed: the image build failed" || backend.Now != "QA · preview of api failed" {
		t.Errorf("backend = %+v / %+v", backend, backend.PreviewDeploy)
	}
	if site.Repo != "web" || site.PreviewDeploy.Kind != "site" || !site.PreviewDeploy.OK ||
		site.PreviewDeploy.URL != "https://s-0-web.cloudfront.net" || site.Round != 1 {
		t.Errorf("site = %+v / %+v", site, site.PreviewDeploy)
	}
}
