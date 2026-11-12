package commands

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/types"
)

func busyModelResult() agentResult {
	return agentResult{Status: "error", Turns: 1, Error: "Selected model is at capacity. Please try a different model."}
}

func cleanReviewResult() agentResult {
	return agentResult{Status: "success", Turns: 1, ReviewResult: &reviewResult{
		Findings: []reviewFinding{{
			Key: "maint-1", Parameter: "maintainability", Severity: "low",
			Location: "0-acme/api/handler.go:12", What: "a helper is duplicated",
		}},
		Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}},
	}}
}

// busyModelStage is a Review stage whose review attempts return results in
// order, and whose retry waits are recorded rather than slept.
func busyModelStage(t *testing.T, logs *strings.Builder, results ...agentResult) (*reviewStage, *[]time.Duration, *int) {
	t.Helper()
	stage := fixLoopStage(t.TempDir(), logs)
	calls := 0
	stage.runReview = func(int) (agentResult, error) {
		calls++
		if calls > len(results) {
			t.Fatalf("unexpected review attempt %d", calls)
		}
		return results[calls-1], nil
	}
	var waits []time.Duration
	stage.retryWait = func(d time.Duration) bool {
		waits = append(waits, d)
		return false
	}
	return stage, &waits, &calls
}

func TestIsBusyModelFailure(t *testing.T) {
	for _, tc := range []struct {
		result agentResult
		err    error
		want   bool
	}{
		{busyModelResult(), nil, true},
		{agentResult{Status: "error", Error: "Overloaded"}, nil, true},
		{agentResult{Status: "error", Error: "Rate limit exceeded"}, nil, true},
		{agentResult{Status: "error", Error: "rate_limit_error"}, nil, true},
		{agentResult{Status: "error", Error: "429 Too Many Requests"}, nil, true},
		{agentResult{Status: "error", Error: "API error (529)"}, nil, true},
		{agentResult{Status: "error", Error: "service temporarily unavailable"}, nil, true},
		{agentResult{Status: "error", Error: "request 14290 failed"}, nil, false},
		{agentResult{Status: "error", Error: "usage limit reached; rate limit"}, nil, false},
		{agentResult{Status: "error", Error: "429: quota exceeded"}, nil, false},
		{agentResult{Status: "error", Error: "authentication failed"}, nil, false},
		{agentResult{Status: "success", Error: "at capacity"}, nil, false},
		{busyModelResult(), errors.New("the review run timed out"), false},
		{agentResult{Status: "error", Error: "AGENT_MODE invalid; at capacity"}, nil, false},
	} {
		if got := isBusyModelFailure(tc.result, tc.err); got != tc.want {
			t.Errorf("isBusyModelFailure(%q, %v) = %t, want %t", tc.result.Error, tc.err, got, tc.want)
		}
	}
}

func TestReviewRoundBusyModelThenSuccessCompletes(t *testing.T) {
	var logs strings.Builder
	stage, waits, calls := busyModelStage(t, &logs, busyModelResult(), cleanReviewResult())
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || len(*waits) != 1 || (*waits)[0] != 30*time.Second {
		t.Errorf("attempts %d, waits %v; want 2 attempts and one 30s wait", *calls, *waits)
	}
	if len(stage.rounds) != 1 || !stage.rounds[0].Completed || stage.rounds[0].Round != 1 || len(stage.rounds[0].Findings) != 1 {
		t.Fatalf("rounds = %+v, want round 1 completed with the second attempt's finding", stage.rounds)
	}
	want := `Review round 1: the reviewer's model is busy (the review run reported status "error": Selected model is at capacity. Please try a different model.) — trying again in 30s (attempt 2 of 3)`
	if strings.Count(logs.String(), "the reviewer's model is busy") != 1 || !strings.Contains(logs.String(), want) {
		t.Errorf("the log lacks exactly one %q:\n%s", want, logs.String())
	}
	if strings.Contains(logs.String(), "did not complete") {
		t.Errorf("a retried round that completed is logged as failed:\n%s", logs.String())
	}
	if turns := jobOutputFor(t, stage.parameters).Agent.Turns; turns != 2 {
		t.Errorf("usage counted %d attempt(s), want 2", turns)
	}
}

func TestReviewRoundBusyModelThreeTimesFails(t *testing.T) {
	var logs strings.Builder
	stage, waits, calls := busyModelStage(t, &logs, busyModelResult(), busyModelResult(), busyModelResult())
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 || len(*waits) != 2 || (*waits)[0] != 30*time.Second || (*waits)[1] != 90*time.Second {
		t.Errorf("attempts %d, waits %v; want 3 attempts after 30s and 90s", *calls, *waits)
	}
	if strings.Count(logs.String(), "the reviewer's model is busy") != 2 || !strings.Contains(logs.String(), "trying again in 90s (attempt 3 of 3)") {
		t.Errorf("want two retry lines:\n%s", logs.String())
	}
	if len(stage.rounds) != 1 || stage.rounds[0].Completed {
		t.Fatalf("rounds = %+v, want one failed round", stage.rounds)
	}
	want := `the review run reported status "error": Selected model is at capacity. Please try a different model. — tried 3 times`
	if stage.rounds[0].Error != want {
		t.Errorf("reason = %q, want %q", stage.rounds[0].Error, want)
	}
	if !strings.Contains(logs.String(), "Review round 1 did not complete: "+want+" — continuing without it") {
		t.Errorf("the log lacks the failure line:\n%s", logs.String())
	}
	if turns := jobOutputFor(t, stage.parameters).Agent.Turns; turns != 3 {
		t.Errorf("usage counted %d attempt(s), want 3", turns)
	}
}

func TestReviewRoundBusyModelWithoutBudgetIsNotRetried(t *testing.T) {
	var logs strings.Builder
	stage, waits, calls := busyModelStage(t, &logs, busyModelResult())
	stage.deadline = time.Now().Add(reviewRunTimeout + 10*time.Second)
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(*waits) != 0 {
		t.Errorf("attempts %d, waits %v; want no retry", *calls, *waits)
	}
	if want := reviewRoundFailure(busyModelResult(), nil); len(stage.rounds) != 1 || stage.rounds[0].Error != want {
		t.Errorf("rounds = %+v, want today's reason %q", stage.rounds, want)
	}
}

func TestReviewRoundBusyModelBudgetRunsOutAfterOneRetry(t *testing.T) {
	var logs strings.Builder
	stage, _, calls := busyModelStage(t, &logs, busyModelResult(), busyModelResult())
	stage.retryWait = func(time.Duration) bool {
		// The first wait spends the budget a second retry would need.
		stage.deadline = time.Now().Add(reviewRunTimeout + 10*time.Second)
		return false
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || len(stage.rounds) != 1 || !strings.HasSuffix(stage.rounds[0].Error, " — tried 2 times") {
		t.Errorf("attempts %d, rounds %+v; want 2 attempts and a \"tried 2 times\" reason", *calls, stage.rounds)
	}
}

func TestReviewRoundOtherFailuresAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		result agentResult
		err    error
	}{
		{agentResult{}, errors.New("the review run timed out")},
		{agentResult{Status: "error", Error: "authentication failed: invalid API key"}, nil},
		{agentResult{Status: "error", Error: "usage limit reached, resets at 5pm"}, nil},
	} {
		var logs strings.Builder
		stage := fixLoopStage(t.TempDir(), &logs)
		calls := 0
		stage.runReview = func(int) (agentResult, error) { calls++; return tc.result, tc.err }
		stage.retryWait = func(time.Duration) bool { t.Error("waited for a retry"); return false }
		if _, err := stage.run(); err != nil {
			t.Fatal(err)
		}
		want := reviewRoundFailure(tc.result, tc.err)
		if calls != 1 || len(stage.rounds) != 1 || stage.rounds[0].Error != want {
			t.Errorf("%q: attempts %d, rounds %+v; want one attempt failing with %q", want, calls, stage.rounds, want)
		}
	}
}

func TestReviewRoundStopDuringRetryWait(t *testing.T) {
	var logs strings.Builder
	stage, _, calls := busyModelStage(t, &logs, busyModelResult())
	stop := make(chan struct{})
	close(stop)
	stage.stopSignal = stop
	stage.retryWait = nil // the real wait, which a stop ends at once
	if _, err := stage.run(); !errors.Is(err, types.ErrJobStoppedByUser) {
		t.Fatalf("run = %v, want the user stop", err)
	}
	if *calls != 1 || len(stage.rounds) != 0 {
		t.Errorf("attempts %d, rounds %+v; want one attempt and no recorded round", *calls, stage.rounds)
	}
	if turns := jobOutputFor(t, stage.parameters).Agent.Turns; turns != 1 {
		t.Errorf("usage counted %d attempt(s), want 1", turns)
	}
}

func TestQAFixReviewRoundRetriesABusyModel(t *testing.T) {
	var logs strings.Builder
	stage, workDir := changedQAStage(t, &logs)
	withReviewRecorded(t, stage, participationOn)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed), qaCompleted(signUpPassed))
	reviewer := stage.reviewer
	var waits []time.Duration
	stage.reviewer = func(rs *reviewStage) {
		reviewer(rs)
		review := rs.runReview
		attempt := 0
		rs.runReview = func(round int) (agentResult, error) {
			attempt++
			if attempt == 1 {
				ev.add("busy")
				return busyModelResult(), nil
			}
			return review(round)
		}
		rs.retryWait = func(d time.Duration) bool { waits = append(waits, d); return false }
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := "qa 1,fix,busy,review 2,qa 2"; strings.Join(ev.events, ",") != want {
		t.Errorf("events = %v, want %s", ev.events, want)
	}
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Errorf("waits = %v, want one 30s wait", waits)
	}
	if !strings.Contains(logs.String(), "Review round 2: the reviewer's model is busy (") {
		t.Errorf("the log lacks the retry line:\n%s", logs.String())
	}
	review := readReviewFromJobOutput(stage.parameters)
	if review == nil || len(review.Rounds) != 2 || !review.Rounds[1].Completed {
		t.Errorf("review = %+v, want round 2 completed", review)
	}
}

func TestReviewLogFoldsAFailedRoundsCoverage(t *testing.T) {
	var logs strings.Builder
	stage := &reviewStage{logsWriter: &logs}
	stage.logFullReview(&reviewOutput{Rounds: []reviewRoundOutput{
		{Round: 1, Completed: true, Coverage: []reviewCoverageOutput{{Parameter: "security", State: "checked"}, {Parameter: "testing", State: "not checked", Reason: "no tests"}}},
		{Round: 2, Error: "the review run timed out", Coverage: notCheckedCoverage("the review run timed out")},
	}})
	out := logs.String()
	if want := "  coverage: every parameter — not checked (review run failed: the review run timed out)\n"; strings.Count(out, want) != 1 {
		t.Errorf("the log lacks one %q:\n%s", want, out)
	}
	if strings.Count(out, "  coverage: ") != 3 || !strings.Contains(out, "  coverage: testing — not checked (no tests)\n") {
		t.Errorf("the completed round's coverage is not listed per parameter:\n%s", out)
	}
}

func TestDidNotCompleteSentenceHasOnePeriod(t *testing.T) {
	for reason, want := range map[string]string{
		"Selected model is at capacity. Please try a different model.": "The review did not complete: Selected model is at capacity. Please try a different model. The change was committed",
		"the review run timed out":                                     "The review did not complete: the review run timed out. The change was committed",
	} {
		review := &reviewOutput{Rounds: []reviewRoundOutput{{Round: 1, Error: reason, Coverage: notCheckedCoverage(reason)}}}
		section := reviewTestOpener(review).needsYouSection()
		if !strings.Contains(section, want) {
			t.Errorf("the section lacks %q:\n%s", want, section)
		}
	}
}
