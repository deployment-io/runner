package commands

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Bounds on the "Needs you" part of a pull-request body.
//
// A PR body is read by a human in a web page. Every finding the review
// produced is on the Task page and in the job log in full — so these caps
// decide what is worth a reviewer's first screen, not what is worth keeping.
// The overflow line names what was left out so nobody mistakes the shortened
// list for the whole one.
const (
	reviewSectionMaxRunes = 8000
	reviewSectionMaxItems = 20
	// reviewSectionReserveRunes is held back from the findings' allowance for
	// what has to come AFTER them: the overflow line and the must-fix note.
	// Without the reserve the findings would spend the whole budget and the
	// line explaining how much was omitted would itself be omitted.
	reviewSectionReserveRunes = 1200
	reviewLocationMaxRunes    = 200
	reviewDetailMaxRunes      = 300
)

// needsFixesTitlePrefix marks a pull request whose review left a must-fix
// finding open. It goes on every such pull request, draft or not: the draft
// guards against a merge, the title is what a list, a notification or an
// email shows. The trailing space is part of it: the prefix reads as a label
// in front of the title, not as a word glued to it.
const needsFixesTitlePrefix = "[Needs fixes] "

// prefixNeedsFixes puts the marker in front of a title, once.
//
// Stripping first is what keeps a re-run from stacking prefixes: the title is
// regenerated from the agent's own pr_title each attempt, and an agent that
// read the previous title off the pull request would hand it back with the
// prefix already on. Capping happens AFTER the prefix is applied, through the
// same capTitle the plain path uses, so the part that gets truncated away is
// the end of the title rather than the marker that explains it.
func prefixNeedsFixes(title string) string {
	return capTitle(needsFixesTitlePrefix+stripNeedsFixesPrefix(title), prTitleMaxRunes)
}

// stripNeedsFixesPrefix removes the marker if present. Called on every title,
// needs-fixes or not: a Step whose findings were resolved on the second
// attempt must not keep a title that says they were not.
func stripNeedsFixesPrefix(title string) string {
	trimmed := strings.TrimSpace(title)
	for strings.HasPrefix(trimmed, needsFixesTitlePrefix) {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, needsFixesTitlePrefix))
	}
	return trimmed
}

// needsYouSection renders "Needs you": what of the Review stage's record a
// reviewer has to act on before merging — the sentence saying the review did
// not complete, and the findings still open, the must-fix ones first. The
// rest of the review (what was noted, what the loop fixed, the coverage) is on
// the Task page.
//
// Empty when the Review stage did not run, or completed with nothing open and
// did not fail.
func (opr *taskOpenPR) needsYouSection() string {
	if opr.review == nil || len(opr.review.Rounds) == 0 {
		return ""
	}
	// Each block below opens with its own blank line.
	const heading = "**Needs you**\n"
	var sb strings.Builder
	failed := failedFinalRound(opr.review)
	if failed != nil {
		// Fail-open, said out loud. A review that could not complete must not
		// leave a body that reads as a clean review.
		// The reason is often an agent's own sentence, full stop included;
		// this sentence adds its own.
		reason := strings.TrimRight(failed.Error, ". ")
		sb.WriteString(fmt.Sprintf("\nThe review did not complete: %s. The change was committed and this pull request opened anyway; nothing here has been gated on a review verdict.\n", reason))
	}
	if latest := latestCompletedRound(opr.review); latest != nil {
		budget := reviewSectionMaxRunes - utf8.RuneCountInString(heading) - utf8.RuneCountInString(sb.String())
		sb.WriteString(opr.openFindings(latest, failed != nil, budget))
	}
	if sb.Len() == 0 {
		return ""
	}
	return boundSection(heading + sb.String())
}

// openFindings renders the must-fix group and the still-open-below-hold group
// of the latest completed round, with the line on how the last fix attempt
// ended, the overflow line and the closing must-fix note, within runes. Empty
// when both groups are.
func (opr *taskOpenPR) openFindings(latest *reviewRoundOutput, failed bool, runes int) string {
	stillOpen, notedAfterFix, _ := splitFindings(latest.Findings)
	if len(stillOpen)+len(notedAfterFix) == 0 {
		return ""
	}
	// The findings are UNVERIFIED when a fix ran after the round that reported
	// them and nothing re-checked it: the last round failed, or a kept fix run
	// was never reviewed. Calling them "still open" would assert something
	// nobody checked.
	unverified := failed || !opr.review.FinalTreeReviewed

	// The budgets are spent in reader-priority order, and the overflow line is
	// written from what is LEFT OVER, so the note that says how much was
	// dropped is never itself the thing that gets dropped.
	budget := &renderBudget{items: reviewSectionMaxItems, runes: runes - reviewSectionReserveRunes}
	// The line about how the last fix attempt ended goes under the group that
	// holds the findings that attempt was sent: the must-fix heading when any
	// of them holds the pull request, the below-hold heading otherwise.
	outcome := fixOutcomeLine(opr.review, strings.TrimSpace(opr.review.FixError))
	// The line is written whatever the findings leave, so its runes come out
	// of the findings' allowance first — a declining fix run's explanation can
	// be long, and must not push the section past its budget.
	budget.runes -= utf8.RuneCountInString(outcome)
	var sb strings.Builder
	writeFindingGroup(&sb, mustFixHeading(unverified), stillOpen, budget)
	if len(stillOpen) > 0 {
		sb.WriteString(outcome)
	}
	writeFindingGroup(&sb, belowHoldHeading(unverified), notedAfterFix, budget)
	if len(stillOpen) == 0 {
		sb.WriteString(outcome)
	}
	if omitted := len(stillOpen) + len(notedAfterFix) - budget.rendered; omitted > 0 {
		sb.WriteString(fmt.Sprintf("\n_%d further finding(s) are not shown here — the full review is on the Task page._\n", omitted))
	}
	// One closing line for every way the loop can end with a must-fix finding
	// open. Findings that did go through a fix round say so themselves
	// ("Still present after a fix round"), and the outcome line above says how
	// the last fix attempt ended.
	if opr.review.MustFixOpen {
		if unverified {
			sb.WriteString("\nThese findings were open at the last completed review. They need a human.\n")
		} else {
			sb.WriteString("\nThese findings are still open. They need a human.\n")
		}
	}
	return sb.String()
}

// reviewTally is the one line at the top of the body that says what the review
// did, computed from the same record — and counting exactly the findings — the
// Task page lists. Empty when the Review stage did not run.
func reviewTally(review *reviewOutput) string {
	if review == nil || len(review.Rounds) == 0 {
		return ""
	}
	if failedFinalRound(review) != nil {
		return "**Review:** did not complete — details on the Task page."
	}
	latest := latestCompletedRound(review)
	examined := false
	for _, c := range latest.Coverage {
		if coverageChecked(c.State) {
			examined = true
			break
		}
	}
	if !examined {
		return "**Review:** nothing was examined — details on the Task page."
	}
	if !review.FinalTreeReviewed {
		return "**Review:** the last fix was not re-checked — details on the Task page."
	}
	fixed, mustFix, noted := len(review.FixedInLoop), 0, 0
	for _, f := range latest.Findings {
		if f.MustFix {
			mustFix++
		} else {
			noted++
		}
	}
	total := fixed + mustFix + noted
	if total == 0 {
		return "**Review:** no findings."
	}
	line := fmt.Sprintf("**Review:** %d found", total)
	if fixed > 0 {
		line += fmt.Sprintf(" · %d fixed before this PR", fixed)
	}
	if mustFix > 0 {
		line += fmt.Sprintf(" · %d must be fixed before merge", mustFix)
	}
	if noted > 0 {
		line += fmt.Sprintf(" · %d noted", noted)
	}
	return line
}

// stillOpenBelowHoldHeading heads the findings that qualified to be sent back
// but sit below the hold threshold and are still open. It says only what is
// true of every one of them. Some went through a fix round and survived it
// (those carry "Still present after a fix round" themselves); others were
// first reported in the last round, after the fix rounds or the stage budget
// ran out, and never had a fix attempt. "Noted after a fix attempt" was false
// for the second kind.
const stillOpenBelowHoldHeading = "Still open (does not hold this pull request)"

// belowHoldHeading is stillOpenBelowHoldHeading, or — when the findings are
// unverified, see mustFixHeading — the heading that does not claim they are
// still open.
func belowHoldHeading(unverified bool) string {
	if unverified {
		return "Reported by the last completed review, fix not verified (does not hold this pull request)"
	}
	return stillOpenBelowHoldHeading
}

// fixOutcomeLine says how the last fix attempt ended, when that is not simply
// "it ran and the review looked again", or "".
//
// A fix run that did not finish was UNDONE, so the diff below this pull
// request is the implementer's own — not a half-applied cleanup. A reader
// comparing the findings against the change has to know that, or they will go
// looking for fix work that was deliberately rolled back.
func fixOutcomeLine(review *reviewOutput, fixError string) string {
	switch {
	case review.FixNotAttempted:
		return "\nNo fix was attempted: the change could not be copied first, so a failed fix could not have been undone.\n"
	case fixError != "":
		return fmt.Sprintf("\nA fix attempt did not complete (%s); the change is shown as it was before that attempt.\n",
			capRunes(fixError, reviewDetailMaxRunes))
	case review.StoppedNoChange:
		// The last fix run SUCCEEDED and left the code alone: it judged these
		// findings wrong, or fixing them contrary to the Step, and said so in its
		// summary. That explanation goes HERE, beside the findings it answers;
		// the description stays the implementer's account of the change. Without
		// the line the findings read as ones nobody has answered.
		explanation := strings.TrimSpace(review.DeclinedExplanation)
		if explanation == "" {
			return "\n_The last fix round changed nothing._\n"
		}
		return "\n_The last fix round changed nothing. The agent's explanation:_\n\n" +
			quoteBlock(neutraliseModelText(capRunes(explanation, declinedExplanationMaxRunes)))
	}
	return ""
}

// declinedExplanationMaxRunes caps a declining fix run's explanation: longer
// than one finding's detail, since it may answer several, and still a small
// part of the section's budget.
const declinedExplanationMaxRunes = reviewDetailMaxRunes * 4

// quoteBlock renders text as a Markdown block quote, every line of it, so a
// blank line in the text cannot end the quote early.
func quoteBlock(text string) string {
	var sb strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			sb.WriteString(">\n")
			continue
		}
		sb.WriteString("> " + line + "\n")
	}
	return sb.String()
}

// mustFixHeading names what the must-fix findings actually are, which depends
// on whether the last completed round reviewed the tree being committed.
//
// When it did, these findings are the last review's verdict on the current
// tree: still open, and they must be fixed. When it did not — the last round
// failed, or a kept fix run was never reviewed — a fix may well have addressed
// them since, and nothing confirmed it. Saying "still open" there asserts
// something nobody checked, and sends a reader looking for a problem that may
// no longer exist.
func mustFixHeading(unverified bool) string {
	if unverified {
		return "Reported by the last completed review, fix not verified"
	}
	return "Still open and must be fixed"
}

// latestCompletedRound returns the newest round that produced a report. A
// round that failed asserts nothing, so the findings a reader sees are the
// last ones anybody actually made — which is why a failed final round does not
// erase the earlier rounds' findings from the body.
func latestCompletedRound(review *reviewOutput) *reviewRoundOutput {
	for i := len(review.Rounds) - 1; i >= 0; i-- {
		if review.Rounds[i].Completed {
			return &review.Rounds[i]
		}
	}
	return nil
}

// failedFinalRound returns the last round when it did NOT complete. Only the
// last one matters here: an earlier round that failed and was followed by one
// that worked is history, not a caveat on this result.
func failedFinalRound(review *reviewOutput) *reviewRoundOutput {
	last := &review.Rounds[len(review.Rounds)-1]
	if last.Completed {
		return nil
	}
	return last
}

// sharedNotCheckedReason reports the one reason when every coverage entry is
// "not checked" with the same, non-empty reason — the record of a round that
// examined nothing.
func sharedNotCheckedReason(coverage []reviewCoverageOutput) (string, bool) {
	if len(coverage) < 2 {
		return "", false
	}
	reason := strings.TrimSpace(coverage[0].Reason)
	if reason == "" {
		return "", false
	}
	for _, c := range coverage {
		if !strings.EqualFold(strings.TrimSpace(c.State), "not checked") || strings.TrimSpace(c.Reason) != reason {
			return "", false
		}
	}
	return reason, true
}

// splitFindings separates a round's findings into the ones that must be fixed
// (they hold the pull request), the ones sent back for a fix below the hold
// threshold that are still open, and the ones that are merely reported.
func splitFindings(findings []reviewFindingOutput) (mustFix, notedAfterFix, annotated []reviewFindingOutput) {
	for _, f := range findings {
		switch {
		case f.MustFix:
			mustFix = append(mustFix, f)
		case f.SentBack:
			notedAfterFix = append(notedAfterFix, f)
		default:
			annotated = append(annotated, f)
		}
	}
	return mustFix, notedAfterFix, annotated
}

// renderBudget is what is left to spend on findings: a count and a rune
// allowance, shared across the groups in call order so the cap spends itself
// on the findings a reader needs first.
type renderBudget struct {
	items    int
	runes    int
	rendered int
}

func (b *renderBudget) exhausted() bool { return b.items <= 0 || b.runes <= 0 }

// writeFindingGroup renders findings under a heading until either budget runs
// out.
func writeFindingGroup(sb *strings.Builder, heading string, findings []reviewFindingOutput, budget *renderBudget) {
	if len(findings) == 0 || budget.exhausted() {
		return
	}
	header := fmt.Sprintf("\n_%s_\n\n", heading)
	sb.WriteString(header)
	budget.runes -= utf8.RuneCountInString(header)
	writeFindings(sb, findings, budget)
}

// writeFindings renders findings until either budget runs out.
func writeFindings(sb *strings.Builder, findings []reviewFindingOutput, budget *renderBudget) {
	for _, f := range findings {
		if budget.exhausted() {
			return
		}
		rendered := renderFinding(f)
		sb.WriteString(rendered)
		budget.items--
		budget.runes -= utf8.RuneCountInString(rendered)
		budget.rendered++
	}
}

func renderFinding(f reviewFindingOutput) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("- **%s / %s**", strings.TrimSpace(f.Parameter), strings.TrimSpace(f.Severity)))
	if location := strings.TrimSpace(f.Location); location != "" {
		sb.WriteString(fmt.Sprintf(" — `%s`", capRunes(location, reviewLocationMaxRunes)))
	}
	sb.WriteString("\n")
	if what := strings.TrimSpace(f.What); what != "" {
		sb.WriteString("  " + capRunes(what, reviewDetailMaxRunes) + "\n")
	}
	if why := strings.TrimSpace(f.Why); why != "" {
		sb.WriteString("  _Why it matters:_ " + capRunes(why, reviewDetailMaxRunes) + "\n")
	}
	// A held finding was routed back to the agent and the reviewer has since
	// said it is still there. Without the line it reads as a finding nobody has
	// tried to fix yet, which understates it.
	if f.Held {
		sb.WriteString("  _Still present after a fix round._\n")
		if note := strings.TrimSpace(f.StillPresentNote); note != "" {
			sb.WriteString("  _Reviewer's note:_ " + capRunes(note, reviewDetailMaxRunes) + "\n")
		}
	}
	return sb.String()
}

// boundSection is the last guard: whatever the per-finding caps allowed, the
// "Needs you" part as a whole stays within its rune budget, and says so when
// it is cut.
func boundSection(section string) string {
	if utf8.RuneCountInString(section) <= reviewSectionMaxRunes {
		return section
	}
	runes := []rune(section)
	// A block left open by the cut would swallow everything after it on
	// GitHub — the parts below, the trailer, and this very note.
	return closeOpenDetails(string(runes[:reviewSectionMaxRunes])) + "\n\n_This list was truncated — the full review is on the Task page._\n"
}

// capRunes truncates to n runes with an ellipsis, counting runes rather than
// bytes so a multi-byte character is never split.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
