package commands

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// Bounds on the "QA on a preview" section of a pull-request body. Half the
// Review section's: QA is advisory, so it gets a smaller share of a reviewer's
// first screen, and it is the first part dropped when the body has to fit.
const (
	qaSectionMaxRunes = 4000
	qaSectionMaxItems = 20
	// qaSectionReserveRunes is held back from the items' allowance for the
	// overflow line and the closing of a collapsed block, so the note saying
	// how much was left out is never itself what gets left out.
	qaSectionReserveRunes = 300
	// qaReasonMaxRunes caps the agent's status reason on the status line.
	qaReasonMaxRunes = 300
)

// qaStatusLine is the one line under the review tally that says what QA on a
// preview found. Empty when the QA stage left no record on the Job, or its
// status is one this runner does not know.
//
// The counts are the FINAL round's: after a fix run, round 1's failures that
// now pass are reported as passed, and the fixed suffix says how many.
func qaStatusLine(qa *qaOutput) string {
	if qa == nil {
		return ""
	}
	const prefix = "**QA on a preview:** "
	switch qa.Status {
	case tasks.QAStatusSkipped:
		return prefix + "skipped" + qaReasonSuffix(qa.StatusReason)
	case tasks.QAStatusCouldNotRun:
		return prefix + "couldn't run" + qaReasonSuffix(qa.StatusReason)
	case tasks.QAStatusCompleted:
	default:
		return ""
	}
	final := qa.finalRound()
	if qa.NoCriteria || final == nil || len(final.Criteria) == 0 {
		line := prefix + "no acceptance criteria — checked the preview loads"
		if final != nil && len(final.Findings) > 0 {
			line += " · " + pluralise(len(final.Findings), "problem found", "problems found")
		}
		return line
	}
	passed, failed, notChecked := countCriteria(final.Criteria)
	line := fmt.Sprintf("%s%d of %d acceptance criteria passed", prefix, passed, len(final.Criteria))
	if failed > 0 {
		line += fmt.Sprintf(" · %d failed", failed)
	}
	if notChecked > 0 {
		line += fmt.Sprintf(" · %d not checked", notChecked)
	}
	if len(qa.FixedByQA) > 0 {
		line += fmt.Sprintf(" · %d fixed before this PR", len(qa.FixedByQA))
	}
	return line
}

// qaReasonSuffix renders the agent's reason as " — <reason>", plain text on
// one line, or "" when there is none.
func qaReasonSuffix(reason string) string {
	if reason = qaPlainText(reason, qaReasonMaxRunes); reason == "" {
		return ""
	}
	return " — " + reason
}

func countCriteria(criteria []qaCriterion) (passed, failed, notChecked int) {
	for _, c := range criteria {
		switch c.State {
		case tasks.QACriterionPassed:
			passed++
		case tasks.QACriterionFailed:
			failed++
		default:
			// An unknown state is not a pass: counting it as not checked keeps
			// the line from claiming more than QA said.
			notChecked++
		}
	}
	return passed, failed, notChecked
}

func pluralise(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// qaPlainText makes agent text safe to drop into a bullet or a line: one line,
// capped, and neutralised so it cannot ping, link or open markup.
func qaPlainText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return neutraliseModelText(capRunes(s, n))
}

// qaSection renders "QA on a preview": what QA found wrong, in the reader's
// priority order — the failed criteria with the problems QA found, then,
// collapsed, the criteria nobody checked, what QA's fix run fixed, and QA's
// observations — after a line per repository saying what QA's previews were
// built from: the branch merged with the base branch, or the branch as built.
//
// Empty unless there is something to show. A skipped stage is said in full by
// its status line, and so is one that couldn't run and reported nothing (bar
// what its previews were built from, shown on its own) — but a
// QA run that stopped part-way (a timeout, a turn cap) can still have reported
// verdicts, findings and observations, and those are shown under a lead-in
// that says the run did not finish. A clean pass needs no section unless it
// has those lines. It never holds the pull request, and says so.
func (opr *taskOpenPR) qaSection() string {
	qa := opr.qa
	final := qa.finalRound()
	if final == nil {
		return ""
	}
	partial, builtOnly := false, false
	switch qa.Status {
	case tasks.QAStatusCompleted:
	case tasks.QAStatusCouldNotRun:
		// A run that reported nothing still built previews: what they were
		// built from is shown on its own.
		builtOnly = !qaReportedEvidence(final)
		if builtOnly && len(final.MergeResults) == 0 {
			return ""
		}
		partial = true
	default:
		return ""
	}
	failed, notChecked := splitCriteria(final.Criteria)
	outcome := qaFixOutcomeLine(qa)
	built := qaMergeResultItems(final.MergeResults)
	fixedItems := qa.FixedByQA
	if builtOnly {
		failed, notChecked, fixedItems, outcome = nil, nil, nil, ""
	}
	if len(built)+len(failed)+len(final.Findings)+len(notChecked)+len(fixedItems)+len(final.Observations) == 0 && outcome == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n---\n")
	sb.WriteString("**QA on a preview**\n\n")
	if builtOnly {
		sb.WriteString("QA did not finish (see the QA line above) and reported nothing. It is advisory: it does not hold this pull request.\n")
	} else if partial {
		sb.WriteString("QA did not finish (see the QA line above); this is what it reported before it stopped. It is advisory: it does not hold this pull request.\n")
	} else {
		sb.WriteString("QA checked the change on a preview against the Task's acceptance criteria. It is advisory: it does not hold this pull request.\n")
	}

	budget := &renderBudget{items: qaSectionMaxItems, runes: qaSectionMaxRunes - utf8.RuneCountInString(sb.String()) - qaSectionReserveRunes - utf8.RuneCountInString(outcome)}
	total := len(built) + len(failed) + len(final.Findings) + len(notChecked) + len(fixedItems) + len(final.Observations)
	if len(built) > 0 {
		writeQAItems(&sb, "\n", built, budget)
	}
	writeFailedCriteria(&sb, failed, final.Findings, budget)
	sb.WriteString(outcome)
	sb.WriteString(qaCollapsedList(fmt.Sprintf("Not checked (%d)", len(notChecked)), notCheckedItems(notChecked), budget))
	sb.WriteString(qaCollapsedList(fmt.Sprintf("Fixed by QA (%d)", len(fixedItems)), fixedItems, budget))
	sb.WriteString(qaCollapsedList(fmt.Sprintf("Observations (%d)", len(final.Observations)), final.Observations, budget))
	if omitted := total - budget.rendered; omitted > 0 {
		sb.WriteString(fmt.Sprintf("\n_%d further QA item(s) are not shown here._\n", omitted))
	}
	return boundQASection(sb.String())
}

// qaMergeResultItems says, per repository the final round built a preview of,
// what QA checked: the branch merged with the base branch at a commit, or the
// branch as built and why.
func qaMergeResultItems(results []qaMergeResultOutput) []string {
	items := make([]string, 0, len(results))
	for _, r := range results {
		var line string
		if r.Merged {
			line = fmt.Sprintf("QA checked %s merged with %s at %s.", r.Repository, r.BaseBranch, shortCommit(r.BaseCommit))
		} else {
			line = fmt.Sprintf("QA checked %s's branch as built: %s.", r.Repository, r.Reason)
		}
		if text := qaPlainText(line, reviewDetailMaxRunes); text != "" {
			items = append(items, "- "+text+"\n")
		}
	}
	return items
}

// qaReportedEvidence is whether a QA round that couldn't run still reported
// something of its own: a verdict on a criterion, a finding or an observation.
// A round that only carries agentbox's placeholders (every criterion
// not_checked because no trailer came back) reported nothing.
func qaReportedEvidence(r *qaRoundOutput) bool {
	if len(r.Findings) > 0 || len(r.Observations) > 0 {
		return true
	}
	for _, c := range r.Criteria {
		if c.State == tasks.QACriterionPassed || c.State == tasks.QACriterionFailed {
			return true
		}
	}
	return false
}

// splitCriteria separates the failed criteria from the ones QA did not check.
// Passed criteria are counted on the status line and not listed.
func splitCriteria(criteria []qaCriterion) (failed, notChecked []qaCriterion) {
	for _, c := range criteria {
		switch c.State {
		case tasks.QACriterionPassed:
		case tasks.QACriterionFailed:
			failed = append(failed, c)
		default:
			notChecked = append(notChecked, c)
		}
	}
	return failed, notChecked
}

// writeFailedCriteria lists the failed criteria with QA's reason, then the
// problems QA found. A finding carries no link to the criterion it bears on,
// so the round's findings are listed once, under the failed criteria, rather
// than guessed onto one of them. With no criteria (a smoke check) they stand
// on their own.
func writeFailedCriteria(sb *strings.Builder, failed []qaCriterion, findings []reviewFindingOutput, budget *renderBudget) {
	if len(failed) > 0 && !budget.exhausted() {
		writeQAItems(sb, "\n_Failed_\n\n", criterionItems(failed), budget)
	}
	if len(findings) > 0 && !budget.exhausted() {
		items := make([]string, 0, len(findings))
		for _, f := range findings {
			items = append(items, qaFindingItem(f))
		}
		writeQAItems(sb, "\n_Problems QA found_\n\n", items, budget)
	}
}

// writeQAItems writes a heading and then items until either budget runs out.
// Each item is already rendered, newline-terminated markdown.
func writeQAItems(sb *strings.Builder, heading string, items []string, budget *renderBudget) {
	sb.WriteString(heading)
	budget.runes -= utf8.RuneCountInString(heading)
	for _, item := range items {
		if budget.exhausted() {
			return
		}
		sb.WriteString(item)
		budget.items--
		budget.runes -= utf8.RuneCountInString(item)
		budget.rendered++
	}
}

// qaCollapsedList renders plain-text items as a collapsed bulleted list,
// spending what the visible part left of the budget. Empty when there is
// nothing to show or no budget left to show any of it — the overflow line
// counts what was not rendered.
func qaCollapsedList(summary string, items []string, budget *renderBudget) string {
	if len(items) == 0 || budget.exhausted() {
		return ""
	}
	bullets := make([]string, 0, len(items))
	for _, item := range items {
		if text := qaPlainText(item, reviewDetailMaxRunes); text != "" {
			bullets = append(bullets, "- "+text+"\n")
		}
	}
	var sb strings.Builder
	before := budget.rendered
	writeQAItems(&sb, "\n<details><summary>"+summary+"</summary>\n\n", bullets, budget)
	if budget.rendered == before {
		return ""
	}
	sb.WriteString("\n</details>\n")
	return sb.String()
}

// criterionItems renders failed criteria, each with QA's reason under it.
func criterionItems(criteria []qaCriterion) []string {
	items := make([]string, 0, len(criteria))
	for _, c := range criteria {
		item := "- " + qaPlainText(c.Criterion, reviewDetailMaxRunes) + "\n"
		if reason := qaPlainText(c.Reason, reviewDetailMaxRunes); reason != "" {
			item += "  " + reason + "\n"
		}
		items = append(items, item)
	}
	return items
}

// notCheckedItems is the not-checked criteria as "criterion — reason" text,
// rendered plain by qaCollapsedList.
func notCheckedItems(criteria []qaCriterion) []string {
	items := make([]string, 0, len(criteria))
	for _, c := range criteria {
		item := c.Criterion
		if reason := strings.TrimSpace(c.Reason); reason != "" {
			item += " — " + reason
		}
		items = append(items, item)
	}
	return items
}

// qaFindingItem renders one QA finding: where, what, and why it matters. The
// location goes in a code span, which renders nothing inside it, so it is only
// stripped of backticks that would close the span early.
func qaFindingItem(f reviewFindingOutput) string {
	var sb strings.Builder
	sb.WriteString("-")
	if location := strings.Join(strings.Fields(strings.ReplaceAll(f.Location, "`", "'")), " "); location != "" {
		sb.WriteString(fmt.Sprintf(" `%s`", capRunes(location, reviewLocationMaxRunes)))
	}
	what := qaPlainText(f.What, reviewDetailMaxRunes)
	if what == "" {
		what = "(no description)"
	}
	sb.WriteString(" " + what + "\n")
	if why := qaPlainText(f.Why, reviewDetailMaxRunes); why != "" {
		sb.WriteString("  _Why it matters:_ " + why + "\n")
	}
	return sb.String()
}

// qaFixOutcomeLine says, briefly, how QA's fix run ended when it did not
// simply fix something: it could not be attempted, did not complete (and was
// undone), or ran and declined to change anything. "" otherwise.
func qaFixOutcomeLine(qa *qaOutput) string {
	switch {
	case qa.FixNotAttempted:
		return "\nQA did not attempt a fix: the change could not be copied first, so a failed fix could not have been undone.\n"
	case strings.TrimSpace(qa.FixError) != "":
		return fmt.Sprintf("\nQA's fix attempt did not complete (%s); the change is shown as it was before that attempt.\n",
			qaPlainText(qa.FixError, reviewDetailMaxRunes))
	case qa.StoppedNoChange:
		explanation := qaPlainText(qa.DeclinedExplanation, reviewDetailMaxRunes)
		if explanation == "" {
			return "\n_QA's fix run changed nothing._\n"
		}
		return "\n_QA's fix run changed nothing. The agent's explanation:_ " + explanation + "\n"
	}
	return ""
}

// boundQASection is the last guard, as boundSection is for the Review
// section: whatever the per-item caps allowed, the section stays within its
// rune budget, and says so when it is cut.
func boundQASection(section string) string {
	if utf8.RuneCountInString(section) <= qaSectionMaxRunes {
		return section
	}
	const note = "\n\n_The QA section was truncated._\n"
	allowance := qaSectionMaxRunes - utf8.RuneCountInString(note) - utf8.RuneCountInString(detailsCloseTag)
	return closeOpenDetails(string([]rune(section)[:allowance])) + note
}
