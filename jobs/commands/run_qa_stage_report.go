package commands

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
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
		for i, c := range final.Criteria {
			dto.Criteria = append(dto.Criteria, tasks.QACriterionDtoV1{Criterion: c.Criterion, State: c.State, Reason: c.Reason, EvidenceIDs: s.evidenceIDs[i]})
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
		if len(out.Repositories) > 0 {
			b.WriteString(fmt.Sprintf("Repositories QA'd: %s\n", strings.Join(out.Repositories, ", ")))
		}
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

const (
	// qaEvidencePerCriterion is how many screenshots one criterion cites, at
	// most, as deployment-server keeps.
	qaEvidencePerCriterion = 3
	// qaEvidenceMaxCriterion is how many criteria a screenshot's Criteria may
	// index (kit's qa_models.MaxCriteria): a position at or past it is left out.
	qaEvidenceMaxCriterion = 50
)

// qaEvidenceCheck is one cited check to upload: its id and the positions of
// the criteria citing it.
type qaEvidenceCheck struct {
	checkID  string
	criteria []int
}

// uploadQAEvidence stores the FINAL round's cited screenshots on
// deployment-server and records, in evidenceIDs, each criterion's stored ids
// in its citation order. Each cited check the round's store holds is uploaded
// once, sequentially, in the order first cited; the first upload that fails
// stops the rest, and the criteria keep the ids stored before it. Earlier
// rounds' screenshots are never uploaded.
func (s *qaStage) uploadQAEvidence() {
	final := s.out.finalRound()
	if final == nil || len(final.Criteria) == 0 {
		return
	}
	store := s.screenshots[final.Round]
	cited, checks := qaCitedChecks(final.Criteria, store)
	if len(checks) == 0 {
		return
	}
	upload := s.uploadEvidence
	if upload == nil {
		upload = func(u tasks.UploadQAEvidenceDtoV1) (string, error) {
			return client.Get().UploadQAEvidence(s.ctx.OrganizationID, u)
		}
	}
	jobID, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.JobID)
	stored := map[string]string{}
	for _, check := range checks {
		id, err := s.uploadOneScreenshot(upload, store, final.Round, jobID, check)
		if err != nil {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA: couldn't store a screenshot (%s); QA's remaining screenshots were not stored\n", err))
			break
		}
		stored[check.checkID] = id
	}
	s.evidenceIDs = map[int][]string{}
	for i, checkIDs := range cited {
		for _, checkID := range checkIDs {
			if id, ok := stored[checkID]; ok {
				s.evidenceIDs[i] = append(s.evidenceIDs[i], id)
			}
		}
	}
	if len(stored) == len(checks) {
		io.WriteString(s.logsWriter, fmt.Sprintf("QA: stored %d screenshots for %d criteria\n", len(stored), len(s.evidenceIDs)))
	}
}

// uploadOneScreenshot uploads one check's screenshot and returns its stored id.
func (s *qaStage) uploadOneScreenshot(upload func(tasks.UploadQAEvidenceDtoV1) (string, error), store *agenttools.QAScreenshots, round int, jobID string, check qaEvidenceCheck) (string, error) {
	image, width, height, err := store.Get(check.checkID)
	if err != nil {
		return "", err
	}
	id, err := upload(tasks.UploadQAEvidenceDtoV1{
		TaskID:    s.ctx.TaskID,
		StepIndex: int(s.ctx.StepIndex),
		JobID:     jobID,
		Round:     round,
		CheckID:   check.checkID,
		Criteria:  check.criteria,
		Image:     image,
		Width:     width,
		Height:    height,
	})
	if err == nil && id == "" {
		err = errors.New("deployment.io returned no id for the screenshot")
	}
	return id, err
}

// qaCitedChecks is, per criterion position, the check ids it cites that store
// holds a screenshot for (duplicates dropped, at most qaEvidencePerCriterion),
// and the distinct checks to upload, in the order first cited, each with the
// positions citing it below qaEvidenceMaxCriterion. A check cited only at or
// past that position isn't uploaded.
func qaCitedChecks(criteria []qaCriterion, store *agenttools.QAScreenshots) (map[int][]string, []qaEvidenceCheck) {
	cited := map[int][]string{}
	var checks []qaEvidenceCheck
	index := map[string]int{}
	for i, c := range criteria {
		var ids []string
		for _, checkID := range c.Evidence {
			if len(ids) == qaEvidencePerCriterion {
				break
			}
			if slices.Contains(ids, checkID) || !store.Has(checkID) {
				continue
			}
			ids = append(ids, checkID)
			if i >= qaEvidenceMaxCriterion {
				continue
			}
			if j, ok := index[checkID]; ok {
				checks[j].criteria = append(checks[j].criteria, i)
				continue
			}
			index[checkID] = len(checks)
			checks = append(checks, qaEvidenceCheck{checkID: checkID, criteria: []int{i}})
		}
		if len(ids) > 0 {
			cited[i] = ids
		}
	}
	return cited, checks
}
