package commands

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Bounds on the Review section of a pull-request body.
//
// A PR body is read by a human in a web page. Every finding the review
// produced is written to the job log in full, unconditionally, by the Review
// stage — so these caps decide what is worth a reviewer's first screen, not
// what is worth keeping. The overflow line names what was left out so nobody
// mistakes the shortened list for the whole one.
const (
	reviewSectionMaxRunes = 8000
	reviewSectionMaxItems = 20
	// reviewSectionReserveRunes is held back from the findings' allowance for
	// what has to come AFTER them: the overflow line, the must-fix note, and
	// the coverage line naming all eight parameters. Without the reserve the
	// findings would spend the whole budget and the line explaining how much
	// was omitted would itself be omitted.
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

// reviewSection renders the Review stage's record into the pull-request body.
//
// Order is deliberate and is the reader's priority order: what is still wrong
// and must be fixed, then what the loop already fixed (which explains why the
// change looks different from what the implementer first wrote), then what was
// merely noted. Coverage comes last, because it answers "what did you look
// at" — a question that only arises once the findings have been read.
//
// Empty when the Review stage did not run at all, so a Task with review
// participation Off produces exactly the body it produced before this stage
// existed.
func (opr *taskOpenPR) reviewSection() string {
	if opr.review == nil || len(opr.review.Rounds) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n---\n")
	sb.WriteString("**Review**\n\n")
	sb.WriteString(reviewedByLine(opr.review))

	latest := latestCompletedRound(opr.review)
	failed := failedFinalRound(opr.review)
	if failed != nil {
		// Fail-open, said out loud. A review that could not complete must not
		// leave a body that reads as a clean review.
		sb.WriteString(fmt.Sprintf("The review did not complete: %s. The change was committed and this pull request opened anyway; nothing here has been gated on a review verdict.\n\n", failed.Error))
	}
	if latest == nil {
		sb.WriteString(coverageLine(opr.review.Rounds[len(opr.review.Rounds)-1].Coverage))
		return boundSection(sb.String())
	}

	sb.WriteString(passesLine(latest.Coverage))
	stillOpen, annotated := splitFindings(latest.Findings)
	fixed := opr.review.FixedInLoop

	// Both budgets are spent in reader-priority order, and the overflow line
	// is written from what is LEFT OVER — so the note that says how much was
	// dropped is never itself the thing that gets dropped.
	budget := &renderBudget{items: reviewSectionMaxItems, runes: reviewSectionMaxRunes - utf8.RuneCountInString(sb.String()) - reviewSectionReserveRunes}
	writeFindingGroup(&sb, mustFixHeading(failed != nil), stillOpen, budget)
	writeFindingGroup(&sb, "Fixed during review", fixed, budget)
	writeFindingGroup(&sb, "Noted", annotated, budget)
	if omitted := len(stillOpen) + len(fixed) + len(annotated) - budget.rendered; omitted > 0 {
		sb.WriteString(fmt.Sprintf("\n_%d further finding(s) are not shown here — the full review is in the Step's job log._\n", omitted))
	}
	if opr.review.MustFixOpen {
		sb.WriteString("\nThese findings were routed back to the agent and are still open after the review's fix budget ran out. They need a human.\n")
	}
	sb.WriteString("\n" + coverageLine(latest.Coverage))
	return boundSection(sb.String())
}

// reviewedByLine says WHO reviewed, because that is no longer implied by the
// Task. A Task may run its review rounds on a model other than the one that
// implemented the change, and a reader weighing a finding — or the absence of
// one — is weighing the judgement of whichever model actually made it.
//
// The model is read from the rounds themselves rather than from the Job, so it
// names the reviewer that ran. Scanned newest-first because a failed final
// round still records who would have run; empty only for a review recorded by
// a runner older than this line, where saying nothing beats guessing.
//
// The round count comes along when there was more than one, since "two rounds"
// means the first round found something that had to be fixed — which is part
// of reading what follows.
func reviewedByLine(review *reviewOutput) string {
	var model string
	for i := len(review.Rounds) - 1; i >= 0 && model == ""; i-- {
		model = strings.TrimSpace(review.Rounds[i].Model)
	}
	if model == "" {
		return ""
	}
	if rounds := len(review.Rounds); rounds > 1 {
		return fmt.Sprintf("Reviewed by %s, %d rounds.\n\n", model, rounds)
	}
	return fmt.Sprintf("Reviewed by %s.\n\n", model)
}

// mustFixHeading names what the must-fix findings actually are, which depends
// on whether the LAST round completed.
//
// When it did, these findings are the last review's verdict on the current
// tree: still open, and they must be fixed. When it did not, they are an
// EARLIER round's verdict, and a fix run may well have addressed them since —
// the round that would have confirmed that is the one that failed. Saying
// "still open" there asserts something nobody checked, and sends a reader
// looking for a problem that may no longer exist.
func mustFixHeading(finalRoundFailed bool) string {
	if finalRoundFailed {
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

// passesLine names what actually ran, so "no findings" is readable as a
// verdict rather than as a silence.
func passesLine(coverage []reviewCoverageOutput) string {
	var ran []string
	for _, c := range coverage {
		if strings.EqualFold(strings.TrimSpace(c.State), "checked") {
			ran = append(ran, c.Parameter)
		}
	}
	if len(ran) == 0 {
		return "No pass examined this change — see the coverage line below for why.\n"
	}
	return fmt.Sprintf("Passes run: %s.\n", strings.Join(ran, ", "))
}

// coverageLine names ALL EIGHT parameters and what happened to each, on one
// line. Eight short answers beat a table nobody reads, and the point is that a
// parameter nothing ran for is visibly different from one that came back
// clean.
func coverageLine(coverage []reviewCoverageOutput) string {
	if len(coverage) == 0 {
		return ""
	}
	parts := make([]string, 0, len(coverage))
	for _, c := range coverage {
		part := fmt.Sprintf("%s: %s", c.Parameter, strings.ToLower(strings.TrimSpace(c.State)))
		if reason := strings.TrimSpace(c.Reason); reason != "" {
			part += " (" + reason + ")"
		}
		parts = append(parts, part)
	}
	return "Coverage — " + strings.Join(parts, "; ") + "\n"
}

// splitFindings separates a round's findings into the ones that must be fixed
// and the ones that are merely reported.
func splitFindings(findings []reviewFindingOutput) (mustFix, annotated []reviewFindingOutput) {
	for _, f := range findings {
		if f.MustFix {
			mustFix = append(mustFix, f)
			continue
		}
		annotated = append(annotated, f)
	}
	return mustFix, annotated
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
	return sb.String()
}

// boundSection is the last guard: whatever the per-finding caps allowed, the
// section as a whole stays within its rune budget, and says so when it is cut.
func boundSection(section string) string {
	if utf8.RuneCountInString(section) <= reviewSectionMaxRunes {
		return section
	}
	runes := []rune(section)
	return string(runes[:reviewSectionMaxRunes]) + "\n\n_The Review section was truncated — the full review is in the Step's job log._\n"
}

// capRunes truncates to n runes with an ellipsis, counting runes rather than
// bytes so a multi-byte character is never split.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
