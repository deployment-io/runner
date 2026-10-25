package commands

import (
	"fmt"
	"io"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
)

// reportQAResult sends the stage's FINAL result to deployment-server to be
// stored on the Step: the gate's, round 1's or the final round's, with the QA
// usage summed over every QA round. Best-effort, like reportReviewResult.
func (s *qaStage) reportQAResult() {
	jobID, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.JobID)
	if err := client.Get().UpdateTaskStepQA(s.ctx.OrganizationID, tasks.UpdateTaskStepQADtoV1{
		TaskID:    s.ctx.TaskID,
		StepIndex: int(s.ctx.StepIndex),
		JobID:     jobID,
		Result:    s.qaResultDto(),
	}); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not report the QA result: %s\n", err))
	}
}

// qaResultDto is the final result as the control plane stores it.
func (s *qaStage) qaResultDto() tasks.QAResultDtoV1 {
	dto := tasks.QAResultDtoV1{
		Status:       s.out.Status,
		StatusReason: s.out.StatusReason,
		NoCriteria:   s.out.NoCriteria,
		AgentType:    s.agentType(),
		Model:        s.model(),
	}
	if final := s.out.finalRound(); final != nil {
		for _, c := range final.Criteria {
			dto.Criteria = append(dto.Criteria, tasks.QACriterionDtoV1{Criterion: c.Criterion, State: c.State, Reason: c.Reason})
		}
		dto.Findings = reviewFindingDtos(final.Findings)
	}
	var tokens tokenUsage
	for _, r := range s.out.Rounds {
		tokens = addTokenUsage(tokens, r.TokenUsage)
		if r.CostUSD != nil {
			dto.CostUSD += *r.CostUSD
		}
	}
	dto.TokenUsage = tasks.ReviewTokenUsageDtoV1{
		InputTokens:         tokens.InputTokens,
		OutputTokens:        tokens.OutputTokens,
		CacheReadTokens:     tokens.CacheReadTokens,
		CacheCreationTokens: tokens.CacheCreationTokens,
	}
	return dto
}

// logFullQA writes the stage's whole record to the job log, in the style of
// logFullReview: the pull request renders a bounded selection, this is where
// the rest lives.
func (s *qaStage) logFullQA() {
	out := s.out
	var b strings.Builder
	b.WriteString("\n--- QA stage ---\n")
	if len(out.Rounds) == 0 {
		b.WriteString(qaGateLogLine(out))
	} else {
		b.WriteString(fmt.Sprintf("Repositories QA'd: %s\n", strings.Join(out.Repositories, ", ")))
		if len(out.Backends) > 0 {
			b.WriteString(fmt.Sprintf("Backend previews QA'd: %s\n", strings.Join(out.Backends, ", ")))
		}
	}
	for _, round := range out.Rounds {
		b.WriteString(qaRoundLogLine(round))
		for _, c := range round.Criteria {
			line := fmt.Sprintf("  [%s] %s", c.State, c.Criterion)
			if c.Reason != "" {
				line += " — " + c.Reason
			}
			b.WriteString(line + "\n")
		}
		for _, f := range round.Findings {
			b.WriteString(fmt.Sprintf("  [finding] %s/%s at %s — %s\n", f.Parameter, f.Severity, f.Location, f.What))
			if f.Why != "" {
				b.WriteString(fmt.Sprintf("      why: %s\n", f.Why))
			}
		}
		for _, o := range round.Observations {
			b.WriteString(fmt.Sprintf("  [observation] %s\n", o))
		}
	}
	for _, c := range out.FixedByQA {
		b.WriteString(fmt.Sprintf("[fixed by QA] %s\n", c))
	}
	b.WriteString(qaFixLogLine(out))
	b.WriteString("QA: advisory — does not hold this pull request\n")
	io.WriteString(s.logsWriter, b.String())
}

// qaGateLogLine is the gate's outcome when no QA round ran.
func qaGateLogLine(out *qaOutput) string {
	switch out.Status {
	case tasks.QAStatusSkipped:
		return fmt.Sprintf("QA skipped: %s\n", out.StatusReason)
	case tasks.QAStatusCouldNotRun:
		return fmt.Sprintf("QA couldn't run: %s\n", out.StatusReason)
	}
	return fmt.Sprintf("QA: %s\n", out.Status)
}

// qaRoundLogLine is one round's headline: its tally, or why it has none.
func qaRoundLogLine(round qaRoundOutput) string {
	if round.Status != tasks.QAStatusCompleted {
		return fmt.Sprintf("QA round %d: %s — %s\n", round.Round, round.Status, round.StatusReason)
	}
	if round.NoCriteria {
		return fmt.Sprintf("QA round %d: %s — no acceptance criteria; checked the preview loads, %d finding(s)\n", round.Round, round.Status, len(round.Findings))
	}
	passed, failed, notChecked := 0, 0, 0
	for _, c := range round.Criteria {
		switch c.State {
		case tasks.QACriterionPassed:
			passed++
		case tasks.QACriterionFailed:
			failed++
		default:
			notChecked++
		}
	}
	return fmt.Sprintf("QA round %d: %s — %d passed, %d failed, %d not checked\n", round.Round, round.Status, passed, failed, notChecked)
}

// qaFixLogLine is the fix round's outcome, or "" when none was attempted.
func qaFixLogLine(out *qaOutput) string {
	switch {
	case out.FixNotAttempted:
		return fmt.Sprintf("QA fix: not attempted — %s\n", out.FixError)
	case out.FixError != "":
		return fmt.Sprintf("QA fix: did not complete (%s) — the change is exactly as it was before it\n", out.FixError)
	case out.StoppedNoChange:
		explanation := out.DeclinedExplanation
		if explanation == "" {
			explanation = "no explanation given"
		}
		return fmt.Sprintf("QA fix: changed no file — %s\n", explanation)
	case len(out.Rounds) > 1:
		return "QA fix: kept; the final QA round only reports\n"
	}
	return ""
}
