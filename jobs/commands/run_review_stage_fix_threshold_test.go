package commands

import (
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// --- two thresholds: FIX sends a finding back, HOLD keeps the PR a draft -----
//
// Under the platform default Security and Correctness are sent back at Low (2)
// and hold at Medium (3). A Low gets a fix attempt; if it survives it is noted
// on the pull request and does not make it a draft.

func splitThresholdStage(t *testing.T) *reviewStage {
	t.Helper()
	stage := heldLoopStage(t)
	stage.thresholds = map[uint]uint{1: 3, 2: 3}
	stage.fixThresholds = map[uint]uint{1: 2, 2: 2}
	return stage
}

func lowFinding(key string) reviewFinding {
	return reviewFinding{Key: key, Parameter: "security", Severity: "low", Location: "0-acme/api/debug.go:20", What: "the error message echoes the request path"}
}

func mediumFinding(key string) reviewFinding {
	return reviewFinding{Key: key, Parameter: "correctness", Severity: "medium", Location: "0-acme/api/debug.go:30", What: "the retry ignores the context deadline"}
}

func runScripted(t *testing.T, stage *reviewStage, rounds []agentResult) *reviewOutput {
	t.Helper()
	stage.runReview = func(round int) (agentResult, error) {
		if round > len(rounds) {
			t.Fatalf("the loop ran review round %d, past the scripted %d", round, len(rounds))
		}
		return rounds[round-1], nil
	}
	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}
	review := readReviewFromJobOutput(out)
	if review == nil {
		t.Fatal("no review was recorded")
	}
	return review
}

// A Job from an older control plane carries no ReviewFixThresholds: the fix
// thresholds ARE the hold thresholds, so a Low below a Medium hold is only
// noted and never costs a fix round — exactly as before the split.
func TestAbsentFixThresholdsBehaveExactlyAsBefore(t *testing.T) {
	if got := decodeFixThresholds(map[string]interface{}{}, io.Discard); got != nil {
		t.Fatalf("decodeFixThresholds with the parameter absent = %v, want nil (fall back to the hold thresholds)", got)
	}

	stage := heldLoopStage(t)
	stage.thresholds = map[uint]uint{1: 3, 2: 3}
	fixRuns := 0
	stage.runFix = func([]reviewFindingOutput) error { fixRuns++; return nil }
	review := runScripted(t, stage, []agentResult{reviewRoundResult(nil, lowFinding("L"))})

	if fixRuns != 0 {
		t.Errorf("a Low below the hold threshold was sent back %d time(s) with no fix thresholds on the Job", fixRuns)
	}
	f := review.Rounds[0].Findings[0]
	if f.SentBack || f.MustFix {
		t.Errorf("finding = %+v, want neither sent back nor must-fix", f)
	}
	if review.MustFixOpen {
		t.Error("must_fix_open = true for a Low under a Medium hold")
	}

	// And a Medium is still sent back and holds, as it always was.
	mustFix := classifyStage(nil, participationOn).classify(reviewRoundResult(nil, mediumFinding("M")))
	if !mustFix[0].SentBack || !mustFix[0].MustFix {
		t.Errorf("a Medium = %+v, want sent back and must-fix", mustFix[0])
	}
}

func TestDecodeFixThresholds(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewFixThresholds, `{"1":2,"2":2}`)
	got := decodeFixThresholds(parameters, io.Discard)
	if len(got) != 2 || got[1] != 2 || got[2] != 2 {
		t.Errorf("decodeFixThresholds = %v, want map[1:2 2:2]", got)
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewFixThresholds, `{}`)
	if got := decodeFixThresholds(parameters, io.Discard); got == nil || len(got) != 0 {
		t.Errorf("an empty object decoded as %v, want an empty non-nil map", got)
	}
}

func classifyStage(fix map[uint]uint, participation int64) *reviewStage {
	return &reviewStage{participation: participation, thresholds: map[uint]uint{1: 3, 2: 3}, fixThresholds: fix}
}

// Info is never sent back by the default; Low is sent back without holding;
// Medium is sent back and holds.
func TestClassifyAppliesTheFixAndHoldThresholds(t *testing.T) {
	findings := classifyStage(map[uint]uint{1: 2, 2: 2}, participationOn).classify(reviewRoundResult(nil,
		reviewFinding{Key: "I", Parameter: "security", Severity: "info", What: "a comment is stale"},
		lowFinding("L"),
		mediumFinding("M"),
	))
	want := map[string][2]bool{"I": {false, false}, "L": {true, false}, "M": {true, true}}
	for _, f := range findings {
		w := want[f.Key]
		if f.SentBack != w[0] || f.MustFix != w[1] {
			t.Errorf("%s (%s) = sent back %t, must-fix %t; want %t, %t", f.Key, f.Severity, f.SentBack, f.MustFix, w[0], w[1])
		}
	}
}

// Advisory sends nothing back and holds nothing, whatever the thresholds.
func TestAdvisorySendsNothingBack(t *testing.T) {
	findings := classifyStage(map[uint]uint{1: 2, 2: 2}, participationAdvisory).classify(reviewRoundResult(nil, lowFinding("L"), mediumFinding("M")))
	for _, f := range findings {
		if f.SentBack || f.MustFix {
			t.Errorf("advisory marked %s sent back %t, must-fix %t", f.Key, f.SentBack, f.MustFix)
		}
	}
	if len(sentBackOnly(findings)) != 0 {
		t.Error("advisory routed findings back")
	}
}

// A Low is sent back with the Medium in ONE fix prompt, and when the next round
// says it is resolved it is fixed in loop like any must-fix finding.
func TestALowIsSentBackAndFixedInLoop(t *testing.T) {
	stage := splitThresholdStage(t)
	var sent [][]reviewFindingOutput
	fix := stage.runFix
	stage.runFix = func(findings []reviewFindingOutput) error {
		sent = append(sent, findings)
		return fix(findings)
	}
	review := runScripted(t, stage, []agentResult{
		reviewRoundResult(nil, lowFinding("L"), mediumFinding("M")),
		reviewRoundResult([]reviewPreviousFinding{{Key: "L", Status: "resolved"}, {Key: "M", Status: "resolved"}}),
	})

	if len(sent) != 1 || len(sent[0]) != 2 {
		t.Fatalf("fix runs were sent %v, want one run with both findings", sent)
	}
	prompt := buildMustFixPrompt("do the thing", "", sent[0])
	if !strings.Contains(prompt, "(low)") || !strings.Contains(prompt, "(medium)") {
		t.Errorf("the fix prompt does not list the Low and the Medium together:\n%s", prompt)
	}
	if len(review.FixedInLoop) != 2 {
		t.Errorf("fixed_in_loop = %+v, want both findings", review.FixedInLoop)
	}
	if review.MustFixOpen {
		t.Error("must_fix_open = true after every finding was resolved")
	}
	section := (&taskOpenPR{review: review}).reviewSection()
	if !strings.Contains(section, "_Fixed during review_") || !strings.Contains(section, "echoes the request path") {
		t.Errorf("the fixed Low is not under Fixed during review:\n%s", section)
	}
}

// A Low the reviewer keeps saying is still present runs the loop to its bounds
// and is then NOTED: no draft, no prefix, no "They need a human".
func TestALowStillOpenAfterTheBoundsIsNotedAndHoldsNothing(t *testing.T) {
	stillThere := []reviewPreviousFinding{{Key: "L", Status: "still_present"}}
	review := runScripted(t, splitThresholdStage(t), []agentResult{
		reviewRoundResult(nil, lowFinding("L")),
		reviewRoundResult(stillThere),
		reviewRoundResult(stillThere),
	})

	if len(review.Rounds) != maxMustFixRounds+1 {
		t.Fatalf("the loop ran %d round(s), want %d — a sent-back Low keeps it going", len(review.Rounds), maxMustFixRounds+1)
	}
	held := findingByKey(review.Rounds[len(review.Rounds)-1].Findings, "L")
	if held == nil || !held.SentBack || !held.Held || held.MustFix {
		t.Fatalf("the held Low = %+v, want sent back, held, and not must-fix", held)
	}
	if review.MustFixOpen {
		t.Error("must_fix_open = true for a Low below the hold threshold")
	}
	opener := &taskOpenPR{review: review}
	if opener.needsFixes() {
		t.Error("a surviving Low made the pull request a draft")
	}
	section := opener.reviewSection()
	if !strings.Contains(section, "_Noted after a fix attempt_") {
		t.Errorf("the surviving Low is not under Noted after a fix attempt:\n%s", section)
	}
	if strings.Contains(section, mustFixHeading(false)) || strings.Contains(section, "They need a human") {
		t.Errorf("the section says the Low must be fixed:\n%s", section)
	}
}

// A Medium still open at the bounds holds the pull request, beside a Low that
// is only noted.
func TestAMediumStillOpenHoldsThePullRequest(t *testing.T) {
	stillThere := []reviewPreviousFinding{{Key: "L", Status: "still_present"}, {Key: "M", Status: "still_present"}}
	review := runScripted(t, splitThresholdStage(t), []agentResult{
		reviewRoundResult(nil, lowFinding("L"), mediumFinding("M")),
		reviewRoundResult(stillThere),
		reviewRoundResult(stillThere),
	})
	if !review.MustFixOpen || !(&taskOpenPR{review: review}).needsFixes() {
		t.Error("an open Medium did not hold the pull request")
	}
	section := (&taskOpenPR{review: review}).reviewSection()
	mustFixAt := strings.Index(section, mustFixHeading(false))
	notedAt := strings.Index(section, "_Noted after a fix attempt_")
	if mustFixAt < 0 || notedAt < 0 || mustFixAt > notedAt {
		t.Errorf("want the Medium under the must-fix heading and the Low under Noted after a fix attempt:\n%s", section)
	}
}

// A must-fix Medium the reviewer re-reports at Low keeps holding: the class is
// the one the round that opened it gave it.
func TestAMediumReReportedAtLowStillHolds(t *testing.T) {
	downgraded := mediumFinding("M")
	downgraded.Severity = "low"
	stillThere := []reviewPreviousFinding{{Key: "M", Status: "still_present"}}
	review := runScripted(t, splitThresholdStage(t), []agentResult{
		reviewRoundResult(nil, mediumFinding("M")),
		reviewRoundResult(stillThere, downgraded),
		reviewRoundResult(stillThere, downgraded),
	})
	for _, round := range review.Rounds[1:] {
		f := findingByKey(round.Findings, "M")
		if f == nil || !f.MustFix || !f.SentBack || !f.Held {
			t.Errorf("round %d: M = %+v, want it still must-fix, sent back and held", round.Round, f)
		}
	}
	if !review.MustFixOpen {
		t.Error("a Medium downgraded to Low by the reviewer stopped holding the pull request")
	}
}

// A held Low re-reported higher never starts holding the pull request.
func TestAHeldLowReReportedHigherDoesNotStartHolding(t *testing.T) {
	raised := lowFinding("L")
	raised.Severity = "high"
	stillThere := []reviewPreviousFinding{{Key: "L", Status: "still_present"}}
	review := runScripted(t, splitThresholdStage(t), []agentResult{
		reviewRoundResult(nil, lowFinding("L")),
		reviewRoundResult(stillThere, raised),
		reviewRoundResult(stillThere, raised),
	})
	if review.MustFixOpen {
		t.Error("a held Low re-reported higher started holding the pull request")
	}
}

// A fix run that changed nothing ends the loop. With only Lows open that is
// not a draft, and the line explaining it sits under the Lows' own group.
func TestANoChangeStopWithOnlyLowsOpenIsNotADraft(t *testing.T) {
	stage := splitThresholdStage(t)
	stage.runFix = func([]reviewFindingOutput) error { return nil }
	review := runScripted(t, stage, []agentResult{reviewRoundResult(nil, lowFinding("L"))})

	if !review.StoppedNoChange {
		t.Fatal("the loop did not stop on a fix run that changed nothing")
	}
	if review.MustFixOpen || (&taskOpenPR{review: review}).needsFixes() {
		t.Error("a no-change stop with only a Low open made the pull request a draft")
	}
	section := (&taskOpenPR{review: review}).reviewSection()
	notedAt := strings.Index(section, "_Noted after a fix attempt_")
	lineAt := strings.Index(section, "_The last fix round changed nothing; the description says why._")
	if notedAt < 0 || lineAt < notedAt {
		t.Errorf("the no-change line is not under Noted after a fix attempt:\n%s", section)
	}
	if strings.Contains(section, "They need a human") {
		t.Errorf("the closing must-fix line was written with nothing holding:\n%s", section)
	}
}

// With a Medium among the findings the fix run was sent, the no-change line
// stays under the must-fix heading, ahead of the noted Lows.
func TestANoChangeStopWithAMediumOpenKeepsItsLineUnderMustFix(t *testing.T) {
	stage := splitThresholdStage(t)
	stage.runFix = func([]reviewFindingOutput) error { return nil }
	review := runScripted(t, stage, []agentResult{reviewRoundResult(nil, lowFinding("L"), mediumFinding("M"))})

	if !review.MustFixOpen {
		t.Fatal("an open Medium did not hold after a no-change stop")
	}
	section := (&taskOpenPR{review: review}).reviewSection()
	lineAt := strings.Index(section, "_The last fix round changed nothing")
	notedAt := strings.Index(section, "_Noted after a fix attempt_")
	if lineAt < 0 || notedAt < 0 || lineAt > notedAt {
		t.Errorf("the no-change line is not under the must-fix heading:\n%s", section)
	}
	if !strings.Contains(section, "These findings are still open. They need a human.") {
		t.Errorf("the closing line is missing with a must-fix finding open:\n%s", section)
	}
}

func TestTheJobLogMarksSentBackFindings(t *testing.T) {
	var logs strings.Builder
	stage := &reviewStage{logsWriter: &logs}
	stage.logFullReview(&reviewOutput{Rounds: []reviewRoundOutput{
		{Round: 1, Completed: true, Findings: []reviewFindingOutput{
			{Parameter: "security", Severity: "low", What: "echoes the path", SentBack: true},
		}},
		{Round: 2, Completed: true, Findings: []reviewFindingOutput{
			{Parameter: "security", Severity: "low", What: "echoes the path", SentBack: true, Held: true},
			{Parameter: "correctness", Severity: "medium", What: "ignores the deadline", SentBack: true, MustFix: true, Held: true},
		}},
	}})
	for _, want := range []string{"[SENT BACK]", "[SENT BACK, still present]", "[MUST FIX, still present]"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the job log has no %s marker:\n%s", want, logs.String())
		}
	}
}
