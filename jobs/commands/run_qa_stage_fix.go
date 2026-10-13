package commands

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
)

// THE QA FIX ROUND reuses the Review stage's fix machinery whole — the undo
// copy, the fix trees, the Task's own model and credentials, the preview tools,
// the dependency cache, the restore on a failed run and the declined fix on an
// unchanged tree — through a reviewStage built for it. Only the prompt differs
// (buildQAFixPrompt). After a kept fix, the same reviewStage runs ONE review
// round on the fix's diff, appended to the Job output's review.

// fixRound sends round 1's failed criteria back to the implementer. It reports
// whether the fix's work was kept; an error is a user stop or a restore that
// did not work.
func (s *qaStage) fixRound(round1 qaRoundOutput) (bool, error) {
	reportTaskStepStage(s.parameters, s.ctx, tasks.StageImplement, s.logsWriter)
	failed := failedCriteria(round1)
	io.WriteString(s.logsWriter, fmt.Sprintf("QA: sending %d failed acceptance criteria back to the implementer\n", len(failed)))
	rs := s.newFixStage(failed, round1.Findings)
	s.fixStage = rs
	ran, err := rs.attemptFix(1, nil)
	if err != nil {
		return false, err
	}
	if ran {
		return true, nil
	}
	s.out.FixError = rs.fixError
	s.out.FixNotAttempted = rs.fixNotAttempted
	s.out.StoppedNoChange = rs.stoppedNoChange
	s.out.DeclinedExplanation = rs.declinedExplanation
	return false, nil
}

// newFixStage builds the reviewStage the fix run and the review round after it
// run in, from the Job's own parameters — the Review stage's inputs, read again.
func (s *qaStage) newFixStage(failed []qaCriterion, findings []reviewFindingOutput) *reviewStage {
	rs := &reviewStage{
		ctx:             s.ctx,
		parameters:      s.parameters,
		workDirHost:     s.workDirHost,
		cacheVolume:     cacheVolumeName(s.ctx),
		vendored:        s.adoptCacheVolume(),
		logsWriter:      s.logsWriter,
		participation:   readReviewParticipation(s.parameters, io.Discard),
		thresholds:      decodeMustFixThresholds(s.parameters, io.Discard),
		fixThresholds:   decodeFixThresholds(s.parameters, io.Discard),
		baseCommits:     readBaseCommitsFromJobOutput(s.parameters),
		repoDirs:        readRepositoryDirsFromJobOutput(s.parameters),
		deadline:        s.deadline,
		stopSignal:      s.stopSignal,
		progressSink:    s.progressSink,
		contextCopyDir:  s.contextCopyDir,
		fixTreesTmpRoot: s.fixTreesTmpRoot,
		fixTreesGit:     s.fixTreesGit,
		fixPrompt: func(stepPrompt, description string) string {
			return buildQAFixPrompt(stepPrompt, description, failed, findings)
		},
	}
	rs.reviewerParams, rs.reviewerFailure = reviewerParameters(s.parameters)
	if s.runFix != nil {
		rs.runFix = func([]reviewFindingOutput) error {
			stepPrompt, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.StepPrompt)
			result, err := s.runFix(rs.fixRunPrompt(stepPrompt, readAgentSummaryFromJobOutput(s.parameters), nil))
			if err := recordFailedFixRun(s.parameters, result, err, s.logsWriter); err != nil {
				return err
			}
			rs.finishedFix = &result
			return nil
		}
	}
	if s.reviewer != nil {
		s.reviewer(rs)
	}
	return rs
}

// adoptCacheVolume says whether the Step's dependency cache is still in place
// for the fix run — RunAgentStep leaves it for the rest of the Step. When it is
// gone the fix run creates it empty and vendors again (ensureVendoredCache).
func (s *qaStage) adoptCacheVolume() bool {
	exists := s.cacheExists
	if exists == nil {
		exists = cacheVolumeExists
	}
	if exists(cacheVolumeName(s.ctx)) {
		io.WriteString(s.logsWriter, "QA stage: reusing the Step's dependency cache for the fix run\n")
		return true
	}
	io.WriteString(s.logsWriter, "QA stage: the Step's dependency cache is gone — the fix run will vendor again\n")
	return false
}

// reviewFix runs one review round on the kept fix's diff when the Task's review
// isn't Off, appends it to the Job output's review and reports it as the Step's
// review. No fix follows it. An error is a user stop.
func (s *qaStage) reviewFix() error {
	rs := s.fixStage
	if rs.participation == participationOff {
		io.WriteString(s.logsWriter, "QA: review is off for this Task — no review round on the fix\n")
		return nil
	}
	round := rs.resumeFromJobOutput()
	if !rs.canAfford(reviewRunTimeout) {
		// The fix moved the tree, so the review's last round no longer covers
		// it: the record says the final tree was not reviewed.
		io.WriteString(s.logsWriter, "QA: no time left for a review round on the fix\n")
		_, _ = rs.finish()
		return nil
	}
	if rs.contextCopyDir != "" {
		if err := ensureContextMountPoint(rs.workDirHost, s.logsWriter); err != nil {
			rs.contextCopyDir = ""
		}
	}
	rs.reportStage(tasks.StageReview)
	io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d: reviewing the fix QA sent back\n", round))
	rs.stageFixDiffs(round)
	result, err := rs.reviewRound(round)
	rs.clearFixDiffs()
	rs.accumulateReviewRun(round, result)
	if errors.Is(err, types.ErrJobStoppedByUser) {
		return err
	}
	if reason := reviewRoundFailure(result, err); reason != "" {
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d did not complete: %s — continuing without it\n", round, reason))
		// A round that did not complete asserts nothing — including that the
		// findings the review left open were fixed.
		open := rs.openSentBack
		rs.recordFailedRound(round, reason, result)
		rs.openSentBack = open
	} else {
		findings := rs.classify(result)
		rs.recordRound(round, findings, result)
		rs.finalTreeReviewed = true
		rs.rememberOpenSentBack(sentBackOnly(findings))
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d completed: %d finding(s), %d agent turn(s) reported (cap: %s model responses)\n", round, len(findings), result.Turns, rs.lastTurnCap))
	}
	_, _ = rs.finish()
	return nil
}

// resumeFromJobOutput loads the Review stage's record into the stage, so a
// round run now is one more round of it: the rounds and their outcome, the
// findings still open (sent to the round as REVIEW_OPEN_FINDINGS) and every key
// seen. It returns the new round's number.
func (s *reviewStage) resumeFromJobOutput() int {
	prev := readReviewFromJobOutput(s.parameters)
	s.openSentBack = map[string]reviewFindingOutput{}
	s.seen = map[string]bool{}
	s.finalTreeReviewed = false
	if prev == nil {
		return 1
	}
	s.rounds = append([]reviewRoundOutput(nil), prev.Rounds...)
	s.fixedInLoop = prev.FixedInLoop
	s.fixError = prev.FixError
	s.fixNotAttempted = prev.FixNotAttempted
	s.stoppedNoChange = prev.StoppedNoChange
	s.declinedExplanation = prev.DeclinedExplanation
	last := 0
	for _, r := range prev.Rounds {
		for _, f := range r.Findings {
			s.seen[findingKey(f)] = true
		}
		if r.Round > last {
			last = r.Round
		}
	}
	// A round that did not complete asserts nothing, so what is still open is
	// what the last COMPLETED round left open — as in the Review stage's loop,
	// where a failed round keeps the open findings it was sent.
	for i := len(prev.Rounds) - 1; i >= 0; i-- {
		if prev.Rounds[i].Completed {
			s.rememberOpenSentBack(sentBackOnly(prev.Rounds[i].Findings))
			break
		}
	}
	return last + 1
}

// buildQAFixPrompt is the QA fix run's prompt: the Step's prompt, the failed
// criteria with QA's reasons, what QA saw, and the description so far.
//
// As with buildMustFixPrompt, the failures DO NOT WIDEN THE STEP: they come
// from an agent that checked a running preview without reading the source, so
// they are framed as criteria to meet within the instructions above, and the
// implementer may decline one it believes is met or misread — changing nothing
// for it and saying why.
func buildQAFixPrompt(stepPrompt, description string, failed []qaCriterion, findings []reviewFindingOutput) string {
	var b strings.Builder
	b.WriteString(stepPrompt)
	b.WriteString("\n\n[Acceptance criteria that failed QA on a live preview]\n")
	b.WriteString("QA deployed a preview of your change and checked it in a real browser, without reading the source. It found these acceptance criteria not met. They do not change what this Step is for or what you may do: fix what is needed for each criterion within the instructions above, then re-run the build/test check as usual. If you believe a criterion is already met, or QA misread the page, change nothing for it and say why in your summary.\n")
	for i, c := range failed {
		b.WriteString(fmt.Sprintf("\n%d. %s\n", i+1, strings.TrimSpace(c.Criterion)))
		if reason := strings.TrimSpace(c.Reason); reason != "" {
			b.WriteString("   QA: " + reason + "\n")
		}
	}
	if len(findings) > 0 {
		b.WriteString("\n[What QA saw on the preview]\n")
		for i, f := range findings {
			b.WriteString(fmt.Sprintf("\n%d. %s (%s) at %s\n", i+1, strings.TrimSpace(f.Parameter), strings.TrimSpace(f.Severity), strings.TrimSpace(f.Location)))
			b.WriteString("   What: " + strings.TrimSpace(f.What) + "\n")
			if why := strings.TrimSpace(f.Why); why != "" {
				b.WriteString("   Why it matters: " + why + "\n")
			}
		}
	}
	b.WriteString(fixDescriptionSection(description))
	return b.String()
}
