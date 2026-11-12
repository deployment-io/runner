package commands

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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
		// No fix run was kept after the round, so it reviewed the committed tree.
		FinalTreeReviewed: true,
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

// "Needs you" lists what a reviewer has to act on, must-fix first. What was
// merely noted and what the loop already fixed are on the Task page.
func TestNeedsYouRendersOpenFindingsInReaderPriorityOrder(t *testing.T) {
	review := completedReview(true,
		reviewFindingOutput{Parameter: "correctness", Severity: "low", Location: "sum.go:9", What: "off by one", Why: "wrong total"},
		reviewFindingOutput{Parameter: "correctness", Severity: "medium", Location: "list.go:3", What: "nil slice returned", SentBack: true},
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", Why: "any caller can read another org's data", MustFix: true},
	)
	review.FixedInLoop = []reviewFindingOutput{
		{Parameter: "security", Severity: "high", Location: "auth.go:12", What: "token logged in plaintext", MustFix: true},
	}

	_, body := reviewTestOpener(review).buildPRTitleAndBody()

	requireInOrder(t, body,
		"Added the login endpoint.",
		"**Needs you**\n\n_Still open and must be fixed_\n\n",
		"no session check",
		"_"+stillOpenBelowHoldHeading+"_",
		"nil slice returned",
		"These findings are still open. They need a human.",
	)
	for _, absent := range []string{"off by one", "token logged in plaintext", "Passes run", "Coverage", "Fixed during review", "_Noted_"} {
		if strings.Contains(body, absent) {
			t.Errorf("the body carries %q, which is on the Task page:\n%s", absent, body)
		}
	}
}

// Review participation Off: no tally and no "Needs you".
func TestNeedsYouIsLeftOutWithoutAReview(t *testing.T) {
	_, body := reviewTestOpener(nil).buildPRTitleAndBody()

	for _, want := range []string{"Generated-By: deployment.io Tasks", "Task: My Task", "Added the login endpoint."} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lost %q:\n%s", want, body)
		}
	}
	for _, absent := range []string{"**Review:**", "**Needs you**"} {
		if strings.Contains(body, absent) {
			t.Errorf("a Task with no Review stage got %q:\n%s", absent, body)
		}
	}
}

// A review with nothing open — clean, or only noted or fixed findings — needs
// nobody, so there is no "Needs you".
func TestNeedsYouIsLeftOutWhenNothingIsOpen(t *testing.T) {
	notedOnly := completedReview(false, reviewFindingOutput{Parameter: "correctness", Severity: "low", What: "off by one"})
	fixedOnly := completedReview(false)
	fixedOnly.FixedInLoop = []reviewFindingOutput{{Parameter: "security", Severity: "high", What: "token logged", MustFix: true}}
	declined := completedReview(false, reviewFindingOutput{Parameter: "correctness", Severity: "low", What: "off by one"})
	declined.StoppedNoChange = true
	for name, review := range map[string]*reviewOutput{
		"clean":                 completedReview(false),
		"noted only":            notedOnly,
		"fixed only":            fixedOnly,
		"noted, last fix no-op": declined,
		"no rounds":             {Participation: "on"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reviewTestOpener(review).needsYouSection(); got != "" {
				t.Errorf("needsYouSection = %q, want none", got)
			}
		})
	}
}

// A review that could not complete must not leave a body that reads as a clean
// review — the whole cost of failing open is paid here.
func TestNeedsYouSaysWhenTheReviewDidNotComplete(t *testing.T) {
	review := &reviewOutput{
		Participation: "on",
		Rounds: []reviewRoundOutput{{
			Round:     1,
			Completed: false,
			Error:     "the review run produced no review_result.",
			Coverage:  notCheckedCoverage("the review run produced no review_result"),
		}},
	}

	got := reviewTestOpener(review).needsYouSection()
	want := "**Needs you**\n\nThe review did not complete: the review run produced no review_result. " +
		"The change was committed and this pull request opened anyway; nothing here has been gated on a review verdict.\n"
	if got != want {
		t.Errorf("needsYouSection =\n%q\nwant\n%q", got, want)
	}
	_, body := reviewTestOpener(review).buildPRTitleAndBody()
	requireInOrder(t, body, "**Review:** did not complete — details on the Task page.", "Added the login endpoint.", want)
}

// Findings from earlier rounds that DID complete are still rendered, even
// though the last round failed.
func TestNeedsYouKeepsEarlierRoundsFindingsWhenTheLastRoundFails(t *testing.T) {
	review := completedReview(false,
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	)
	review.Rounds = append(review.Rounds, reviewRoundOutput{
		Round: 2, Completed: false, Error: "the review run reported status \"timeout\"",
		Coverage: notCheckedCoverage("the review run reported status \"timeout\""),
	})

	_, body := reviewTestOpener(review).buildPRTitleAndBody()
	requireInOrder(t, body, "**Needs you**", "did not complete", "no session check")
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
func TestNeedsYouLabelsMustFixFindingsAsStillOpenWhenTheLastRoundCompleted(t *testing.T) {
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

// The part is bounded: a review that left forty things open renders at most
// twenty and says how many it left out, with the rest on the Task page.
func TestNeedsYouBoundsWhatItRendersAndSaysWhatItOmitted(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 0; i < 40; i++ {
		findings = append(findings, reviewFindingOutput{
			Parameter: "correctness", Severity: "low", SentBack: true,
			Location: strings.Repeat("l", 400),
			What:     strings.Repeat("w", 900),
			Why:      strings.Repeat("y", 900),
		})
	}
	review := completedReview(false, findings...)

	section := reviewTestOpener(review).needsYouSection()
	shown := strings.Count(section, "**correctness / low**")
	if shown > reviewSectionMaxItems {
		t.Errorf("the part rendered more than %d findings", reviewSectionMaxItems)
	}
	if want := fmt.Sprintf("_%d further finding(s) are not shown here — the full review is on the Task page._", 40-shown); !strings.Contains(section, want) {
		t.Errorf("the part does not say %q:\n%s", want, section)
	}
	if n := utf8.RuneCountInString(section); n > reviewSectionMaxRunes+100 {
		t.Errorf("the part is %d runes, over its cap", n)
	}
	if strings.Contains(section, strings.Repeat("l", reviewLocationMaxRunes+1)) {
		t.Error("a finding's location was rendered past its cap")
	}
	if strings.Contains(section, strings.Repeat("w", reviewDetailMaxRunes+1)) {
		t.Error("a finding's what was rendered past its cap")
	}
}

// The overflow line counts only the two groups "Needs you" lists: noted and
// fixed findings are on the Task page, not "further findings" left out here.
func TestNeedsYouOverflowCountsOnlyItsGroups(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 0; i < 22; i++ {
		findings = append(findings, reviewFindingOutput{Parameter: "security", Severity: "high", What: fmt.Sprintf("must %d", i), MustFix: true})
	}
	findings = append(findings, reviewFindingOutput{Parameter: "correctness", Severity: "low", What: "below", SentBack: true})
	for i := 0; i < 7; i++ {
		findings = append(findings, reviewFindingOutput{Parameter: "correctness", Severity: "low", What: fmt.Sprintf("noted %d", i)})
	}
	review := completedReview(true, findings...)
	review.FixedInLoop = []reviewFindingOutput{{Parameter: "security", Severity: "high", What: "fixed", MustFix: true}}

	section := reviewTestOpener(review).needsYouSection()
	// 23 open, 20 shown.
	if !strings.Contains(section, "\n_3 further finding(s) are not shown here — the full review is on the Task page._\n") {
		t.Errorf("the overflow line does not count 3:\n%s", section)
	}
	if strings.Contains(section, "noted ") || strings.Contains(section, "  fixed\n") {
		t.Errorf("a noted or fixed finding was rendered:\n%s", section)
	}
}

// A re-run renders THIS attempt's review, not the previous one's. The provider
// now applies the body on the already-open path, so a stale "Needs you" would
// outlive the fixes that resolved it.
func TestNeedsYouOnARerunRendersTheCurrentAttempt(t *testing.T) {
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

	if strings.Contains(secondBody, "**Needs you**") || strings.Contains(secondBody, "no session check") {
		t.Errorf("attempt two's body still says findings are open:\n%s", secondBody)
	}
	if !strings.HasPrefix(secondBody, "**Review:** 1 found · 1 fixed before this PR\n\n") {
		t.Errorf("attempt two's tally does not report the fix:\n%s", secondBody)
	}
}

// --- the needs-fixes signal -------------------------------------------------

// An unresolved must-fix finding prefixes the title on EVERY provider path,
// draft or not. The draft is the guard, the title is the signal, and the two
// are not the same thing: a list, a notification and an email show the title
// and not the draft state, and "Ready for review" drops the draft state while
// the findings stay open.
func TestOpenerPrefixesTheTitleWheneverAMustFixFindingIsOpen(t *testing.T) {
	review := completedReview(true,
		reviewFindingOutput{Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true},
	)
	opener := reviewTestOpener(review)
	if !opener.needsFixes() {
		t.Fatal("an open must-fix finding did not ask for fixes")
	}
	title, _ := opener.buildPRTitleAndBody()
	if strings.HasPrefix(title, needsFixesTitlePrefix) {
		t.Fatalf("buildPRTitleAndBody applied the prefix itself (%q); openOne applies it once, after the subject is built", title)
	}
	if got := prefixNeedsFixes(title); !strings.HasPrefix(got, needsFixesTitlePrefix) || !strings.HasSuffix(got, strings.TrimSpace(title)) {
		t.Errorf("prefixed title = %q, want the marker in front of %q", got, title)
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
