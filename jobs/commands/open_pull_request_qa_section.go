package commands

import (
	"fmt"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// qaReasonMaxRunes caps the agent's status reason on the QA status line.
const qaReasonMaxRunes = 300

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

// qaWiringLine is one pair's line, for the Step log. all is
// the round's every pair: a "single origin" line names the site that has it.
func qaWiringLine(w qaWiringOutput, all []qaWiringOutput) string {
	if w.Connected {
		line := fmt.Sprintf("QA built %s's preview to call %s's preview (%s)", w.Frontend, w.API, strings.Join(w.FrontendVariables, ", "))
		if len(w.APIVariables) > 0 {
			return line + fmt.Sprintf(" and allowed its origin on %s's preview (%s).", w.API, strings.Join(w.APIVariables, ", "))
		}
		return line + fmt.Sprintf(". No production setting of %s lists %s's address, so %s's preview allows the origins Preview configuration gives it.", w.API, w.Frontend, w.API)
	}
	prefix := fmt.Sprintf("QA didn't connect %s's preview to %s's preview: ", w.Frontend, w.API)
	switch w.Reason {
	case qaWiringBackendFailed:
		return prefix + fmt.Sprintf("%s's preview failed to deploy.", w.API)
	case qaWiringBackendLeftOut:
		return prefix + fmt.Sprintf("%s's preview was left out, because it uses a Go module from another repository this Task also changes, and a preview builds against the released version.", w.API)
	case qaWiringSiteFailed:
		return prefix + fmt.Sprintf("%s's preview failed to deploy.", w.Frontend)
	case qaWiringLookupFailed:
		return prefix + "deployment.io couldn't look up their production settings."
	case qaWiringSingleOrigin:
		name := strings.Join(w.APIVariables, ", ")
		return prefix + fmt.Sprintf("%s's production setting %s holds a single origin, used by %s's preview.", w.API, name, singleOriginHolder(w, all))
	}
	return prefix + fmt.Sprintf("no production setting of %s holds %s's address.", w.Frontend, w.API)
}

// singleOriginHolder is the site whose origin w's backend variable holds: the
// connected pair on the same backend that set it.
func singleOriginHolder(w qaWiringOutput, all []qaWiringOutput) string {
	for _, other := range all {
		if !other.Connected || other.API != w.API {
			continue
		}
		for _, name := range other.APIVariables {
			if len(w.APIVariables) > 0 && name == w.APIVariables[0] {
				return other.Frontend
			}
		}
	}
	return "another site"
}
