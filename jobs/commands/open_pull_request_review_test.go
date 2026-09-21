package commands

import (
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/oauth"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

func reviewTestOpener(review *reviewOutput) *taskOpenPR {
	return &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", StepIndex: 0,
		},
		agentPRTitle: "Add OAuth login to auth-service",
		agentSummary: "Added the login endpoint.",
		review:       review,
	}
}

func completedReview(mustFixOpen bool, findings ...reviewFindingOutput) *reviewOutput {
	return &reviewOutput{
		Participation: "on",
		MustFixOpen:   mustFixOpen,
		Rounds: []reviewRoundOutput{{
			Round:     1,
			Completed: true,
			Findings:  findings,
			Coverage: []reviewCoverageOutput{
				{Parameter: "security", State: "checked"},
				{Parameter: "correctness", State: "checked"},
				{Parameter: "spec conformance", State: "not checked", Reason: "no pass for this parameter in this release"},
				{Parameter: "testing", State: "not checked", Reason: "no pass for this parameter in this release"},
				{Parameter: "deploy readiness", State: "not checked", Reason: "no pass for this parameter in this release"},
				{Parameter: "performance", State: "not checked", Reason: "no pass for this parameter in this release"},
				{Parameter: "maintainability", State: "not checked", Reason: "no pass for this parameter in this release"},
				{Parameter: "reliability", State: "not checked", Reason: "no pass for this parameter in this release"},
			},
		}},
	}
}

// The Review section is what a reviewer reads instead of the job log. It has
// to say what ran, what is still wrong, what the loop already fixed, and what
// nobody looked at.
func TestReviewSectionRendersFindingsInReaderPriorityOrder(t *testing.T) {
	review := completedReview(true,
		reviewFindingOutput{Parameter: "correctness", Severity: "low", Location: "sum.go:9", What: "off by one", Why: "wrong total"},
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", Why: "any caller can read another org's data", MustFix: true},
	)
	review.FixedInLoop = []reviewFindingOutput{
		{Parameter: "security", Severity: "high", Location: "auth.go:12", What: "token logged in plaintext", MustFix: true},
	}

	_, body := reviewTestOpener(review).buildPRTitleAndBody()

	stillOpen := strings.Index(body, "no session check")
	fixed := strings.Index(body, "token logged in plaintext")
	annotated := strings.Index(body, "off by one")
	if stillOpen < 0 || fixed < 0 || annotated < 0 {
		t.Fatalf("a finding group is missing from the body:\n%s", body)
	}
	if !(stillOpen < fixed && fixed < annotated) {
		t.Errorf("findings are out of order — still-open must-fix, then fixed, then annotated:\n%s", body)
	}
	if !strings.Contains(body, "Passes run: security, correctness.") {
		t.Errorf("the body does not say which passes ran:\n%s", body)
	}
	// One coverage line naming ALL EIGHT parameters, so "no findings" cannot
	// be confused with "nobody looked".
	for _, parameter := range []string{"security", "correctness", "spec conformance", "testing",
		"deploy readiness", "performance", "maintainability", "reliability"} {
		if !strings.Contains(body, parameter+":") {
			t.Errorf("the coverage line omits %q:\n%s", parameter, body)
		}
	}
	if !strings.Contains(body, "They need a human.") {
		t.Errorf("an open must-fix finding is not called out:\n%s", body)
	}
}

// The existing sections must render exactly as before — the Review section is
// an addition, not a replacement.
func TestReviewSectionLeavesTheExistingBodyAlone(t *testing.T) {
	opener := reviewTestOpener(nil)
	_, body := opener.buildPRTitleAndBody()

	for _, want := range []string{"Generated-By: deployment.io Tasks", "Task: My Task", "Added the login endpoint."} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lost %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "**Review**") {
		t.Errorf("a Task with no Review stage got a Review section:\n%s", body)
	}
}

// A review that could not complete must not leave a body that reads as a clean
// review — the whole cost of failing open is paid here.
func TestReviewSectionSaysWhenTheReviewDidNotComplete(t *testing.T) {
	review := &reviewOutput{
		Participation: "on",
		Rounds: []reviewRoundOutput{{
			Round:     1,
			Completed: false,
			Error:     "the review run produced no review_result",
			Coverage:  notCheckedCoverage("the review run produced no review_result"),
		}},
	}

	_, body := reviewTestOpener(review).buildPRTitleAndBody()
	if !strings.Contains(body, "The review did not complete") || !strings.Contains(body, "no review_result") {
		t.Errorf("the body does not say the review failed or why:\n%s", body)
	}
	if !strings.Contains(body, "not checked") {
		t.Errorf("the body does not record that nothing was examined:\n%s", body)
	}
}

// Findings from earlier rounds that DID complete are still rendered, even
// though the persisted coverage is the failed round's all-NotChecked record.
func TestReviewSectionKeepsEarlierRoundsFindingsWhenTheLastRoundFails(t *testing.T) {
	review := completedReview(false,
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	)
	review.Rounds = append(review.Rounds, reviewRoundOutput{
		Round: 2, Completed: false, Error: "the review run reported status \"timeout\"",
		Coverage: notCheckedCoverage("the review run reported status \"timeout\""),
	})

	_, body := reviewTestOpener(review).buildPRTitleAndBody()
	if !strings.Contains(body, "no session check") {
		t.Errorf("round 1's findings were dropped because round 2 failed:\n%s", body)
	}
	if !strings.Contains(body, "did not complete") {
		t.Errorf("the failed final round is not mentioned:\n%s", body)
	}
	// And they are NOT presented as the current verdict. A fix run may well
	// have addressed them since; the round that would have confirmed it is
	// the one that failed, so "still open" asserts something nobody checked.
	if strings.Contains(body, "Still open and must be fixed") {
		t.Errorf("an earlier round's findings are labelled as still open after the last round failed:\n%s", body)
	}
	if !strings.Contains(body, "fix not verified") {
		t.Errorf("the body does not say these findings are unverified:\n%s", body)
	}
}

// When the last round DID complete, its must-fix findings are the current
// verdict on the current tree and are labelled as such.
func TestReviewSectionLabelsMustFixFindingsAsStillOpenWhenTheLastRoundCompleted(t *testing.T) {
	review := completedReview(true,
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	)

	_, body := reviewTestOpener(review).buildPRTitleAndBody()
	if !strings.Contains(body, "Still open and must be fixed") {
		t.Errorf("a completed round's must-fix findings are not labelled as still open:\n%s", body)
	}
	if strings.Contains(body, "fix not verified") {
		t.Errorf("a completed round's verdict is hedged as unverified:\n%s", body)
	}
}

// The section is bounded: a review that reported forty things renders twenty
// and says how many it left out, with the rest in the job log.
func TestReviewSectionBoundsWhatItRendersAndSaysWhatItOmitted(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 0; i < 40; i++ {
		findings = append(findings, reviewFindingOutput{
			Parameter: "correctness", Severity: "low",
			Location: strings.Repeat("l", 400),
			What:     strings.Repeat("w", 900),
			Why:      strings.Repeat("y", 900),
		})
	}
	review := completedReview(false, findings...)

	_, body := reviewTestOpener(review).buildPRTitleAndBody()
	if strings.Count(body, "**correctness / low**") > reviewSectionMaxItems {
		t.Errorf("the body rendered more than %d findings", reviewSectionMaxItems)
	}
	if !strings.Contains(body, "further finding(s) are not shown here") {
		t.Errorf("the body does not say what it omitted:\n%s", body[:500])
	}
	if strings.Contains(body, strings.Repeat("l", reviewLocationMaxRunes+1)) {
		t.Error("a finding's location was rendered past its cap")
	}
	if strings.Contains(body, strings.Repeat("w", reviewDetailMaxRunes+1)) {
		t.Error("a finding's what was rendered past its cap")
	}
}

// A re-run renders THIS attempt's review, not the previous one's. The provider
// now applies the body on the already-open path, so a stale section would
// outlive the fixes that resolved it — which is the defect this whole change
// closes for the Verification section too.
func TestReviewSectionOnARerunRendersTheCurrentAttempt(t *testing.T) {
	firstAttempt := completedReview(true,
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	)
	_, firstBody := reviewTestOpener(firstAttempt).buildPRTitleAndBody()
	if !strings.Contains(firstBody, "no session check") {
		t.Fatalf("attempt one's body is wrong:\n%s", firstBody)
	}

	secondAttempt := completedReview(false)
	secondAttempt.FixedInLoop = []reviewFindingOutput{
		{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	}
	_, secondBody := reviewTestOpener(secondAttempt).buildPRTitleAndBody()

	if strings.Contains(secondBody, "They need a human.") {
		t.Errorf("attempt two's body still says findings are open:\n%s", secondBody)
	}
	if !strings.Contains(secondBody, "Fixed during review") {
		t.Errorf("attempt two's body does not report the fix:\n%s", secondBody)
	}
}

// --- the needs-fixes signal -------------------------------------------------

// Four paths, one rule: the title says "needs fixes" exactly when the draft
// could not say it.
func TestNeedsFixesTitleForEachProviderPath(t *testing.T) {
	const title = "Add OAuth login to auth-service"
	for _, tc := range []struct {
		name       string
		needsFixes bool
		dto        oauth.OpenPullRequestDtoV1
		wantPrefix bool
	}{
		{
			name:       "draft honoured on a new pull request: the draft state is the signal",
			needsFixes: true,
			dto:        oauth.OpenPullRequestDtoV1{URL: "u", Number: 7},
			wantPrefix: false,
		},
		{
			name:       "draft unsupported: the title is the only place left",
			needsFixes: true,
			dto:        oauth.OpenPullRequestDtoV1{URL: "u", Number: 7, DraftUnsupported: true},
			wantPrefix: true,
		},
		{
			name:       "already existed: draft state cannot be set after creation",
			needsFixes: true,
			dto:        oauth.OpenPullRequestDtoV1{URL: "u", Number: 7, AlreadyExisted: true},
			wantPrefix: true,
		},
		{
			name:       "no open must-fix finding: nothing to say",
			needsFixes: false,
			dto:        oauth.OpenPullRequestDtoV1{URL: "u", Number: 7, AlreadyExisted: true},
			wantPrefix: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, needed := needsFixesTitleFor(tc.needsFixes, tc.dto, title)
			if needed != tc.wantPrefix {
				t.Errorf("needed = %t, want %t", needed, tc.wantPrefix)
			}
			if tc.wantPrefix && !strings.HasPrefix(got, needsFixesTitlePrefix) {
				t.Errorf("title = %q, want the needs-fixes prefix", got)
			}
			if !tc.wantPrefix && got != title {
				t.Errorf("title = %q, want it untouched", got)
			}
		})
	}
}

// The prefix is applied BEFORE capping and survives it: a title truncated down
// to the point where the marker is gone would look like an ordinary PR.
func TestPrefixNeedsFixesSurvivesCappingAndIsNeverAppliedTwice(t *testing.T) {
	long := strings.Repeat("a very long title ", 10)
	got := prefixNeedsFixes(long)
	if !strings.HasPrefix(got, needsFixesTitlePrefix) {
		t.Errorf("title = %q, want the prefix kept through capping", got)
	}
	if len([]rune(got)) > prTitleMaxRunes {
		t.Errorf("title is %d runes, want at most %d", len([]rune(got)), prTitleMaxRunes)
	}

	twice := prefixNeedsFixes(prefixNeedsFixes("Add OAuth login"))
	if strings.Count(twice, needsFixesTitlePrefix) != 1 {
		t.Errorf("title = %q, want the prefix exactly once on a re-run", twice)
	}
}

// A Step whose findings were resolved on the second attempt must not keep a
// title announcing that it needs fixes.
func TestSubjectDropsAPreviousAttemptsNeedsFixesPrefix(t *testing.T) {
	opener := reviewTestOpener(completedReview(false))
	opener.agentPRTitle = needsFixesTitlePrefix + "Add OAuth login to auth-service"

	subject, _ := opener.buildPRTitleAndBody()
	if strings.Contains(subject, needsFixesTitlePrefix) {
		t.Errorf("subject = %q, want the previous attempt's prefix removed", subject)
	}
}

// Advisory annotates and holds nothing: no draft is requested, and no title
// prefix is applied, even when findings meet the threshold.
func TestAdvisoryNeverRequestsADraftOrPrefixesTheTitle(t *testing.T) {
	review := completedReview(true,
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	)
	review.Participation = "advisory"
	opener := reviewTestOpener(review)

	if opener.needsFixes() {
		t.Error("an advisory review asked for a draft — advisory routes nothing back and holds nothing")
	}
	_, body := opener.buildPRTitleAndBody()
	if !strings.Contains(body, "no session check") {
		t.Errorf("an advisory review's findings are not annotated in the body:\n%s", body)
	}
}
