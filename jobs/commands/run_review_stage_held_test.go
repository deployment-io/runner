package commands

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// --- "fixed in loop" is the reviewer's answer, never a missing key ------------
//
// The bug these cover, in full. classify used to call a must-fix finding fixed
// when the next round did not report its key again, and the key is the
// reviewer's own free-text slug. A live Step ended with a Critical
// unauthenticated secret dump listed under "Fixed during review" on a NON-DRAFT
// pull request while the code still had it: the second round reported the same
// problem under a different slug, so the first key looked resolved and the
// second looked like a brand-new finding.
//
// agentbox now answers for each open finding by key (review_result.previous), so
// the runner asks instead of inferring.

// The live sequence, round by round: a reviewer that renames its own key every
// round and never says the problem is gone.
func TestTheRenamedFindingIsHeldAcrossEveryRoundAndOpensADraft(t *testing.T) {
	stage := heldLoopStage(t)
	rounds := []agentResult{
		// Round 1: the Critical finding that started it, under key A.
		reviewRoundResult(nil, reviewFinding{
			Key: "A", Parameter: "security", Severity: "critical",
			Location: "0-acme/api/debug.go:12", What: "GET /debug/env returns every secret without authentication",
		}),
		// Round 2: the SAME problem, reworded under key B — and A is still there.
		reviewRoundResult([]reviewPreviousFinding{{Key: "A", Status: "still_present", Note: "the endpoint still has no auth check"}},
			reviewFinding{
				Key: "B", Parameter: "security", Severity: "high",
				Location: "0-acme/api/debug.go:12", What: "the debug env handler is reachable unauthenticated",
			}),
		// Round 3: nothing but a note about naming, and A is STILL there.
		reviewRoundResult([]reviewPreviousFinding{{Key: "A", Status: "still_present"}},
			reviewFinding{
				Key: "C", Parameter: "maintainability", Severity: "info",
				Location: "0-acme/api/debug.go:3", What: "the handler name could be clearer",
			}),
	}
	reviewRuns := 0
	stage.runReview = func(round int) (agentResult, error) {
		reviewRuns++
		if round > len(rounds) {
			t.Fatalf("the loop ran review round %d, past the bounds", round)
		}
		return rounds[round-1], nil
	}

	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}
	if reviewRuns != len(rounds) {
		t.Fatalf("the loop ran %d review round(s), want %d", reviewRuns, len(rounds))
	}
	review := readReviewFromJobOutput(out)
	if review == nil {
		t.Fatal("no review was recorded")
	}
	if len(review.FixedInLoop) != 0 {
		t.Errorf("fixed_in_loop = %+v, want nothing: no round ever said the finding was resolved", review.FixedInLoop)
	}
	if !review.MustFixOpen {
		t.Error("must_fix_open = false while the Critical finding was still present — this is the non-draft pull request the bug produced")
	}
	// The pull request the Step would open: draft, with the marker in its title.
	if !(&taskOpenPR{review: review}).needsFixes() {
		t.Error("the pull request was not requested as a draft")
	}
	if got := prefixNeedsFixes("Add the debug endpoint"); !strings.HasPrefix(got, needsFixesTitlePrefix) {
		t.Errorf("title = %q, want the needs-fixes prefix", got)
	}
	// A is on every round's record from the one that opened it, held rather
	// than dropped, and its severity stays the one that gated it.
	for _, round := range review.Rounds[1:] {
		held := findingByKey(round.Findings, "A")
		if held == nil {
			t.Fatalf("round %d dropped the still-present finding: %+v", round.Round, round.Findings)
		}
		if !held.MustFix || !held.Held {
			t.Errorf("round %d: A = %+v, want it must-fix and held", round.Round, *held)
		}
		if held.New {
			t.Errorf("round %d marked A new — it has been on the record since round 1", round.Round)
		}
		if held.Severity != "critical" {
			t.Errorf("round %d: A's severity = %q, want the Critical it was opened at", round.Round, held.Severity)
		}
	}
	// The reworded re-report stands on its own; merging two reports that may or
	// may not be one problem is the guess this change exists to stop making.
	if findingByKey(review.Rounds[1].Findings, "B") == nil {
		t.Errorf("round 2's own finding B was lost: %+v", review.Rounds[1].Findings)
	}
	if note := findingByKey(review.Rounds[1].Findings, "A").Why; !strings.Contains(note, "Still present: the endpoint still has no auth check") {
		t.Errorf("A's why = %q, want the reviewer's note on what it still sees", note)
	}
}

// The clean case: an entry that says resolved, for a round that did not report
// that key again. A DIFFERENT security finding in the same file is not evidence
// about this one either way.
func TestAResolvedStatusIsWhatMovesAFindingToFixedInLoop(t *testing.T) {
	stage := classifyStageAfterRoundOne(reviewFindingOutput{
		Key: "A", Parameter: "security", Severity: "critical",
		Location: "0-acme/api/debug.go:12", What: "returns every secret without authentication", MustFix: true,
	})

	findings := stage.classify(reviewRoundResult(
		[]reviewPreviousFinding{{Key: "A", Status: "resolved", Note: "the handler now checks the session"}},
		reviewFinding{
			Key: "B", Parameter: "security", Severity: "high",
			Location: "0-acme/api/debug.go:40", What: "the handler logs the bearer token",
		}))

	if len(stage.fixedInLoop) != 1 || stage.fixedInLoop[0].Key != "A" {
		t.Errorf("fixed_in_loop = %+v, want the finding the reviewer said it resolved", stage.fixedInLoop)
	}
	if len(findings) != 1 || findings[0].Key != "B" || findings[0].Held {
		t.Errorf("findings = %+v, want only this round's own finding, unheld", findings)
	}
}

// The reviewer contradicting itself: resolved on the status line, reported again
// in the findings. The finding is the stronger evidence of the two.
func TestAResolvedStatusIsIgnoredWhenTheRoundReportsTheSameKeyAgain(t *testing.T) {
	stage := classifyStageAfterRoundOne(reviewFindingOutput{
		Key: "A", Parameter: "security", Severity: "critical",
		Location: "0-acme/api/debug.go:12", What: "returns every secret without authentication", MustFix: true,
	})

	findings := stage.classify(reviewRoundResult(
		[]reviewPreviousFinding{{Key: "A", Status: "resolved"}},
		reviewFinding{
			Key: "A", Parameter: "security", Severity: "critical",
			Location: "0-acme/api/debug.go:12", What: "returns every secret without authentication",
		}))

	if len(stage.fixedInLoop) != 0 {
		t.Errorf("fixed_in_loop = %+v, want nothing: the round reported the finding again", stage.fixedInLoop)
	}
	if len(findings) != 1 || !findings[0].Held || !findings[0].MustFix {
		t.Errorf("findings = %+v, want the re-reported finding held and must-fix", findings)
	}
}

// An open finding the reviewer did not mention at all is STILL PRESENT — the
// contract says so, and the alternative is the bug.
func TestAnOpenFindingMissingFromPreviousIsHeld(t *testing.T) {
	stage := classifyStageAfterRoundOne(
		reviewFindingOutput{Key: "A", Parameter: "security", Severity: "critical", Location: "0-acme/api/debug.go:12", What: "dumps every secret", MustFix: true},
		reviewFindingOutput{Key: "B", Parameter: "correctness", Severity: "high", Location: "0-acme/api/store.go:8", What: "the retry loop never terminates", MustFix: true},
	)

	// A non-empty previous that answers for B only.
	findings := stage.classify(reviewRoundResult([]reviewPreviousFinding{{Key: "B", Status: "resolved"}}))

	if len(stage.fixedInLoop) != 1 || stage.fixedInLoop[0].Key != "B" {
		t.Errorf("fixed_in_loop = %+v, want only the finding that was answered for", stage.fixedInLoop)
	}
	if len(findings) != 1 || findings[0].Key != "A" || !findings[0].Held || !findings[0].MustFix {
		t.Errorf("findings = %+v, want the unanswered finding held and must-fix", findings)
	}
}

// NO FALLBACK TO KEY MATCHING. agentbox omits previous both when the reviewer
// left the list out and when every entry it returned was unusable, so a
// fallback would reproduce the bug exactly. The cost is that an image without
// the field holds every open finding and opens a draft, which is the safe
// direction.
func TestANilPreviousHoldsEveryOpenFinding(t *testing.T) {
	stage := classifyStageAfterRoundOne(
		reviewFindingOutput{Key: "A", Parameter: "security", Severity: "critical", Location: "0-acme/api/debug.go:12", What: "dumps every secret", MustFix: true},
		reviewFindingOutput{Key: "B", Parameter: "correctness", Severity: "high", Location: "0-acme/api/store.go:8", What: "the retry loop never terminates", MustFix: true},
	)

	// A round that reports something else entirely and says nothing about
	// either open finding.
	findings := stage.classify(reviewRoundResult(nil, reviewFinding{
		Key: "C", Parameter: "security", Severity: "low", Location: "0-acme/api/debug.go:60", What: "the error message leaks the path",
	}))

	if len(stage.fixedInLoop) != 0 {
		t.Errorf("fixed_in_loop = %+v, want nothing: a round that reported no statuses resolved nothing", stage.fixedInLoop)
	}
	if len(mustFixOnly(findings)) != 2 {
		t.Errorf("findings = %+v, want both open findings held and routed back", findings)
	}
	for _, key := range []string{"A", "B"} {
		held := findingByKey(findings, key)
		if held == nil || !held.Held || !held.MustFix {
			t.Errorf("%s = %+v, want it held and must-fix", key, held)
		}
	}
	// And an entry with an EMPTY key is one agentbox dropped, so it answers for
	// nothing: the same as no entry at all.
	stage = classifyStageAfterRoundOne(reviewFindingOutput{Key: "A", Parameter: "security", Severity: "critical", What: "dumps every secret", MustFix: true})
	findings = stage.classify(reviewRoundResult([]reviewPreviousFinding{{Status: "resolved"}}))
	if len(stage.fixedInLoop) != 0 || len(findings) != 1 || !findings[0].Held {
		t.Errorf("a keyless previous entry resolved a finding: fixed = %+v, findings = %+v", stage.fixedInLoop, findings)
	}
}

// A held finding the round DID report — at a severity that gates nothing — is
// marked in place. Two copies of one problem would go back to the implementer
// twice and be read by a human as two.
func TestAReReportedHeldFindingIsMarkedInPlaceRatherThanDuplicated(t *testing.T) {
	stage := classifyStageAfterRoundOne(reviewFindingOutput{
		Key: "A", Parameter: "security", Severity: "critical",
		Location: "0-acme/api/debug.go:12", What: "dumps every secret", MustFix: true,
	})

	findings := stage.classify(reviewRoundResult(
		[]reviewPreviousFinding{{Key: "A", Status: "still_present", Note: "still unauthenticated"}},
		reviewFinding{
			Key: "A", Parameter: "security", Severity: "low",
			Location: "0-acme/api/debug.go:12", What: "the debug endpoint is a bit loose",
		}))

	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one entry for one problem", findings)
	}
	// Low is below the threshold, so this is the round's own marking being
	// overridden: the severity that gated the finding is the one it was opened
	// at, and a reviewer downgrading its own report does not release it.
	if !findings[0].MustFix || !findings[0].Held {
		t.Errorf("findings[0] = %+v, want the re-reported finding must-fix and held", findings[0])
	}
	if !strings.Contains(findings[0].Why, "Still present: still unauthenticated") {
		t.Errorf("why = %q, want the reviewer's note", findings[0].Why)
	}
	if len(stage.fixedInLoop) != 0 {
		t.Errorf("fixed_in_loop = %+v, want nothing", stage.fixedInLoop)
	}
}

// A finding held by one round and resolved by the next is FIXED, and reads that
// way. It went back into openMustFix marked held, so the flag has to come off on
// the way into fixedInLoop: "Fixed during review" with "still present after a
// fix round" underneath it is the two statements this change exists to keep
// apart, in one entry, on exactly the sequence it was built for.
func TestAFindingHeldByAnEarlierRoundIsNotStillPresentOnceItIsResolved(t *testing.T) {
	stage := classifyStageAfterRoundOne(reviewFindingOutput{
		Key: "A", Parameter: "security", Severity: "critical",
		Location: "0-acme/api/debug.go:12", What: "dumps every secret", MustFix: true,
	})

	// Round 2 holds A, and the loop carries it into round 3 still marked held.
	round2 := stage.classify(reviewRoundResult([]reviewPreviousFinding{{Key: "A", Status: "still_present"}}))
	if held := findingByKey(round2, "A"); held == nil || !held.Held {
		t.Fatalf("round 2 did not hold A: %+v", round2)
	}
	stage.recordRound(2, round2, agentResult{Turns: 5})
	stage.rememberOpenMustFix(mustFixOnly(round2))

	// Round 3 says it is gone and does not report it again.
	round3 := stage.classify(reviewRoundResult([]reviewPreviousFinding{{Key: "A", Status: "resolved"}}))
	stage.recordRound(3, round3, agentResult{Turns: 5})
	stage.rememberOpenMustFix(mustFixOnly(round3))

	if len(stage.fixedInLoop) != 1 || stage.fixedInLoop[0].Key != "A" {
		t.Fatalf("fixed_in_loop = %+v, want the finding the reviewer said it resolved", stage.fixedInLoop)
	}
	if stage.fixedInLoop[0].Held {
		t.Error("the fixed finding is still marked held — the pull request would list it as fixed and still present at once")
	}
	section := (&taskOpenPR{review: &reviewOutput{
		Participation: "on",
		Rounds:        stage.rounds,
		FixedInLoop:   stage.fixedInLoop,
	}}).reviewSection()
	if strings.Contains(section, "_Still present after a fix round._") {
		t.Errorf("a fixed finding is rendered as still present:\n%s", section)
	}
}

// --- what the round is asked ------------------------------------------------

// Round 1 has no previous round to ask about. From round 2 the open must-fix
// findings go in, KEYED BY findingKey — never by the raw Key field, which is
// empty whenever the reviewer supplied no key of its own and which agentbox
// would drop, holding a finding that may well have been fixed.
func TestReviewOpenFindingsIsAbsentInRoundOneAndCarriesTheOpenFindingsAfter(t *testing.T) {
	stage := reviewStageFor(t, implementerJobParameters(t))
	stage.rememberOpenMustFix([]reviewFindingOutput{
		{Key: "sec-1", Parameter: "security", Severity: "critical", Location: "0-acme/api/debug.go:12", What: "dumps every secret", Why: "anyone can read them", MustFix: true},
		// No key of its own: findingKey synthesises one, and that is what has
		// to cross the wire.
		{Parameter: "correctness", Severity: "high", Location: "0-acme/api/store.go:8", What: "the retry loop never terminates", MustFix: true},
	})

	first, err := stage.reviewSpawnEnv(1)
	if err != nil {
		t.Fatalf("reviewSpawnEnv(1): %s", err)
	}
	if _, present := envMap(first)["REVIEW_OPEN_FINDINGS"]; present {
		t.Error("round 1 was sent open findings — there is no previous round for it to answer about")
	}

	second, err := stage.reviewSpawnEnv(2)
	if err != nil {
		t.Fatalf("reviewSpawnEnv(2): %s", err)
	}
	payload := envMap(second)["REVIEW_OPEN_FINDINGS"]
	if payload == "" {
		t.Fatal("round 2 was sent no open findings, so it cannot answer for any of them")
	}
	var sent []reviewOpenFinding
	if err := json.Unmarshal([]byte(payload), &sent); err != nil {
		t.Fatalf("REVIEW_OPEN_FINDINGS is not the documented JSON array (%s): %s", err, payload)
	}
	if len(sent) != 2 {
		t.Fatalf("REVIEW_OPEN_FINDINGS carries %d finding(s), want both open ones: %s", len(sent), payload)
	}
	byKey := map[string]reviewOpenFinding{}
	for _, f := range sent {
		if f.Key == "" {
			t.Errorf("an open finding crossed the wire with no key — agentbox drops it, and the runner then holds it forever: %+v", f)
		}
		byKey[f.Key] = f
	}
	for key := range stage.openMustFix {
		if _, ok := byKey[key]; !ok {
			t.Errorf("the open finding keyed %q was not sent: %s", key, payload)
		}
	}
	if got := byKey["sec-1"]; got.Parameter != "security" || got.Severity != "critical" ||
		got.Location != "0-acme/api/debug.go:12" || got.What != "dumps every secret" {
		t.Errorf("the open finding was sent as %+v, want its parameter, severity, location and what", got)
	}
	// A round after one that left nothing open is sent nothing again.
	stage.rememberOpenMustFix(nil)
	third, err := stage.reviewSpawnEnv(3)
	if err != nil {
		t.Fatalf("reviewSpawnEnv(3): %s", err)
	}
	if _, present := envMap(third)["REVIEW_OPEN_FINDINGS"]; present {
		t.Error("a round with nothing open was still sent REVIEW_OPEN_FINDINGS")
	}
}

// The key the runner SENDS has to be the key agentbox echoes back, and agentbox
// cuts both to 120 runes. A longer key would come back matching nothing in
// openMustFix and its finding would be held for the rest of the loop however
// well it was fixed.
func TestFindingKeyNeverExceedsAgentboxsKeyCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		finding reviewFindingOutput
	}{
		{"the reviewer's own long key", reviewFindingOutput{Key: strings.Repeat("security-unauthenticated-secret-dump-", 20)}},
		{"a synthesised key over a long location and what", reviewFindingOutput{
			Parameter: "security",
			Location:  strings.Repeat("0-acme/api/internal/handlers/debug/", 10) + "env.go:12",
			What:      strings.Repeat("the endpoint returns every secret ", 10),
		}},
		{"a multi-byte key", reviewFindingOutput{Key: strings.Repeat("秘密の環境変数が漏れている", 20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := findingKey(tc.finding)
			if got := utf8.RuneCountInString(key); got > reviewFindingKeyMaxRunes {
				t.Errorf("findingKey returned %d runes, want at most %d — agentbox would echo back a key the runner cannot match", got, reviewFindingKeyMaxRunes)
			}
			if !utf8.ValidString(key) {
				t.Errorf("findingKey cut a multi-byte character in half: %q", key)
			}
		})
	}
	// The cap is a truncation, not a rewrite: a key that fits is untouched, so
	// it still matches what the reviewer sent.
	if got := findingKey(reviewFindingOutput{Key: "sec-1"}); got != "sec-1" {
		t.Errorf("findingKey(%q) = %q, want it unchanged", "sec-1", got)
	}
}

// --- the pull request --------------------------------------------------------

// A held finding under the must-fix heading says so. Without the line it reads
// as a finding nobody has tried to fix yet, which understates a problem that
// has already survived a fix round.
func TestTheReviewSectionSaysAHeldFindingIsStillPresent(t *testing.T) {
	review := completedReview(true,
		reviewFindingOutput{
			Parameter: "security", Severity: "critical", Location: "0-acme/api/debug.go:12",
			What:    "GET /debug/env returns every secret without authentication",
			Why:     "anyone on the internet can read them. Still present: the endpoint still has no auth check",
			MustFix: true, Held: true,
		},
		reviewFindingOutput{
			Parameter: "correctness", Severity: "high", Location: "0-acme/api/store.go:8",
			What: "the retry loop never terminates", MustFix: true,
		})

	section := (&taskOpenPR{review: review}).reviewSection()

	if !strings.Contains(section, "_Still present after a fix round._") {
		t.Errorf("the held finding is not marked still present:\n%s", section)
	}
	if strings.Count(section, "_Still present after a fix round._") != 1 {
		t.Errorf("the still-present line was written for a finding that is not held:\n%s", section)
	}
	if !strings.Contains(section, "Still present: the endpoint still has no auth check") {
		t.Errorf("the reviewer's note on what it still sees is missing:\n%s", section)
	}
	// And under the must-fix heading, above what is merely noted.
	if strings.Index(section, mustFixHeading(false)) > strings.Index(section, "_Still present after a fix round._") {
		t.Errorf("the held finding is not under the must-fix heading:\n%s", section)
	}
}

// The job log's own marker. "Still open" only ever meant a key turned up again;
// this is the reviewer's verdict on a finding it was asked about.
func TestTheJobLogMarksAHeldFindingStillPresent(t *testing.T) {
	var logs strings.Builder
	stage := &reviewStage{logsWriter: &logs}
	stage.logFullReview(&reviewOutput{Rounds: []reviewRoundOutput{{
		Round: 2, Completed: true,
		Findings: []reviewFindingOutput{
			{Parameter: "security", Severity: "critical", Location: "0-acme/api/debug.go:12", What: "dumps every secret", MustFix: true, Held: true},
			{Parameter: "security", Severity: "high", Location: "0-acme/api/debug.go:40", What: "logs the token", MustFix: true, New: true},
			{Parameter: "maintainability", Severity: "info", Location: "0-acme/api/debug.go:3", What: "unclear name"},
		},
	}}})

	if !strings.Contains(logs.String(), "[MUST FIX, still present]") {
		t.Errorf("the job log does not mark the held finding:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "[MUST FIX, new since the last round]") {
		t.Errorf("a round's own new finding lost its marker:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "[annotated, still open]") {
		t.Errorf("an annotated finding lost its marker:\n%s", logs.String())
	}
}

// --- helpers -----------------------------------------------------------------

// heldLoopStage is a Review stage whose fix runs all succeed, so what the loop
// does is decided entirely by what the review rounds report. The caller injects
// runReview.
func heldLoopStage(t *testing.T) *reviewStage {
	t.Helper()
	workDir := t.TempDir()
	// A checkout for the fix round's undo copy to take: without one the
	// snapshot fails, no fix runs, and the loop ends after its first round.
	writeFile(t, filepath.Join(workDir, "0-acme/api", "debug.go"), "package api\n")
	return &reviewStage{
		parameters:    map[string]interface{}{},
		workDirHost:   workDir,
		logsWriter:    io.Discard,
		participation: participationOn,
		thresholds:    map[uint]uint{1: 4, 2: 4},
		baseCommits:   map[string]string{"0-acme/api": "abc123"},
		deadline:      time.Now().Add(reviewStageBudget),
		copyTree:      testCopyTree,
		runFix:        func([]reviewFindingOutput) error { return nil },
	}
}

// classifyStageAfterRoundOne is a stage that has already run one round and is
// carrying the given must-fix findings into the next one — the state every
// fixed-or-held decision is made in.
func classifyStageAfterRoundOne(open ...reviewFindingOutput) *reviewStage {
	stage := &reviewStage{participation: participationOn, thresholds: map[uint]uint{1: 4, 2: 4}}
	stage.recordRound(1, open, agentResult{Turns: 4})
	stage.rememberOpenMustFix(open)
	return stage
}

// reviewRoundResult is one round's report: what it found, and its answer for
// each open finding it was sent.
func reviewRoundResult(previous []reviewPreviousFinding, findings ...reviewFinding) agentResult {
	return agentResult{Status: "success", Turns: 6, ReviewResult: &reviewResult{
		Findings: findings,
		Previous: previous,
		Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}, {Parameter: "correctness", State: "checked"}},
	}}
}

func findingByKey(findings []reviewFindingOutput, key string) *reviewFindingOutput {
	for i, f := range findings {
		if findingKey(f) == key {
			return &findings[i]
		}
	}
	return nil
}
