package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// RunReviewStage sits between RunAgentStep and CommitAndPush. It reviews the
// diff the implementer just produced against the Task's spec, routes findings
// at or above the org's must-fix threshold back to the implementer inside this
// same Job, and records what it found.
//
// THE STAGE IS FAIL-OPEN. A review that cannot complete — a round that failed,
// an agentbox image that predates review mode, a baseline that was never
// recorded — degrades the Step to exactly today's behaviour: commit, push,
// ordinary pull request. It never fails the Step and never discards work. The
// cost of that choice is that a systematically broken review is invisible
// except in the PR body and the job log, which is why every failure is
// RECORDED as coverage naming the reason rather than passed over in silence.
//
// It never routes anything back when participation is Advisory, and it returns
// immediately when participation is Off — a Task that opted out runs the chain
// it ran before this stage existed.
type RunReviewStage struct {
	// stopSignal is shared by the runner's outer loop, exactly as for
	// RunAgentStep: a user stop mid-round SIGTERMs the container and
	// surfaces types.ErrJobStoppedByUser, which routes to the existing stop
	// path rather than being reported as a failed review.
	stopSignal <-chan struct{}
	// progressSink forwards agentbox's live counters during a review round,
	// so the dashboard's turn and token counters keep moving through a stage
	// that can take minutes.
	progressSink func(jobs.LiveProgressV1)
}

func (rs *RunReviewStage) SetStopSignal(stop <-chan struct{}) { rs.stopSignal = stop }

func (rs *RunReviewStage) SetProgressSink(sink func(jobs.LiveProgressV1)) { rs.progressSink = sink }

// The loop's bounds.
//
// EVERY ONE OF THESE BECOMES POLICY LATER — an org setting beside the must-fix
// thresholds on AgentPolicy. They are constants here because the first release
// needs one defensible answer more than it needs a configuration surface, and
// because a bound nobody has tuned yet is better argued from a comment than
// from a database row nobody has written.
// reviewDiffDirName mirrors agentbox's review.DirName: the directory beside
// the repository checkouts where a review round's diff files live.
const reviewDiffDirName = ".review"

const (
	// maxMustFixRounds caps how many times a must-fix finding may be routed
	// back to the implementer, so at most three review runs happen in total.
	// Two is the point where a loop that is converging has converged and one
	// that is not has usually started arguing with itself.
	maxMustFixRounds = 2
	// reviewRunMaxTurns is a CEILING that catches a looping reviewer, not
	// the budget a review is expected to use. A review reads the diff files
	// agentbox wrote (one per repository, in pages when large), then the
	// callers of what changed, and every read is a turn: a careful review of
	// a one-file diff used 26, and a multi-repository change needs several
	// times that. Time and cost are bounded by reviewRunTimeout and the stage
	// budget; a cap tight enough to bind on a real review would end it with a
	// max-turns error, the round would be recorded as failed, and the pull
	// request would open unreviewed. agentbox states the budget in the review
	// prompt so a reviewer nearing it reports what it has.
	reviewRunMaxTurns = 80
	// reviewRunTimeout is the wall clock for one review run. Generous enough
	// for a large diff over a slow model, short enough that a stuck round
	// does not eat the stage budget.
	reviewRunTimeout = 20 * time.Minute
	// mustFixRunTimeout is the wall clock for one fix run. A fix run's TURN
	// cap is the implement run's own — the Task's MaxTurns, chosen by the
	// user for this Task's size — inherited through the spawn environment
	// rather than set here. A fix is an implement run scoped to a handful
	// of findings, so it usually stops early; the cap is a ceiling, and a
	// ceiling below the user's throws away a finished implementation when a
	// bounded cleanup runs out of room.
	mustFixRunTimeout = 30 * time.Minute
	// reviewStageBudget caps the whole stage — every review round and every
	// fix run together. The loop checks it BEFORE starting a round and exits
	// through the escape hatch rather than starting one it cannot finish: a
	// round killed by the budget costs its tokens and produces nothing.
	reviewStageBudget = 90 * time.Minute
)

// Participation values as the runner sees them — the resolved
// review_enums.Participation, stamped as an int64 at Job creation. Mirrored by
// number rather than imported because the runner cannot import kit; kit owns
// the test that pins the numbering.
const (
	participationOn       = 1
	participationAdvisory = 2
	participationOff      = 3
)

func (rs *RunReviewStage) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkStepDone(parameters, err)
		}
	}()
	participation := readReviewParticipation(parameters, logsWriter)
	if participation == participationOff {
		return parameters, nil
	}
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	// The per-Step cache volume is created here and removed on the way out:
	// RunAgentStep removed its own when it returned, and a fix run needs the
	// shelf back. Same name, so it is the Step's own cache rather than a
	// second one — and the removal keeps a Step from leaking a volume whether
	// or not a fix round happened.
	cacheVolume := cacheVolumeName(ctx)
	if err := createCacheVolume(cacheVolume); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not create the review stage's cache volume: %s\n", err))
	}
	defer removeCacheVolume(cacheVolume)
	// The stage parks directories beside the work dir — the implementer's
	// stash and one per round. Each round removes its own on the way out;
	// this sweeps whatever a failed restore left, however the stage ends,
	// so a Step cannot leak host directories nothing else ever collects.
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(ctx.OrganizationID, ctx.TaskID)
	defer cleanupReviewStageSiblings(workDirHost)
	// agentbox writes the round's diff files under <work dir>/.review and
	// removes them itself when the run ends; this is the safety net for a
	// round killed before it could, so a fix run or the next Step never
	// finds a stale diff beside the checkouts.
	defer os.RemoveAll(filepath.Join(workDirHost, reviewDiffDirName))
	// Resolved ONCE, before the loop: every round of this stage runs on the
	// same reviewer, and so does every round's record.
	reviewerParams, reviewerFailure := reviewerParameters(parameters)
	stage := &reviewStage{
		ctx:             ctx,
		parameters:      parameters,
		reviewerParams:  reviewerParams,
		reviewerFailure: reviewerFailure,
		logsWriter:      logsWriter,
		participation:   participation,
		thresholds:      decodeMustFixThresholds(parameters, logsWriter),
		baseCommits:     readBaseCommitsFromJobOutput(parameters),
		deadline:        time.Now().Add(reviewStageBudget),
		stopSignal:      rs.stopSignal,
		progressSink:    rs.progressSink,
	}
	return stage.run()
}

// reviewStage holds one Step's Review stage. A struct rather than a long
// parameter list: the loop, each round and the reporting all read the same
// half-dozen inputs.
type reviewStage struct {
	ctx        commandUtils.TaskJobContext
	parameters map[string]interface{}
	// reviewerParams is the parameter view REVIEW rounds run under — see
	// reviewerParameters. Nil when the Job names no reviewer, which is what
	// makes reviewerView fall back to parameters and the stage behave exactly
	// as it did before reviewers existed. Fix rounds never read it: a fix is
	// an implement run and keeps the Task's own model, agent and credentials.
	reviewerParams map[string]interface{}
	// reviewerFailure is why the named reviewer cannot run, or "". Set when a
	// reviewer was named but its credentials were not resolved at pickup.
	reviewerFailure string
	logsWriter      io.Writer
	participation   int64
	thresholds      map[uint]uint
	baseCommits     map[string]string
	deadline        time.Time
	stopSignal      <-chan struct{}
	progressSink    func(jobs.LiveProgressV1)

	rounds      []reviewRoundOutput
	fixedInLoop []reviewFindingOutput
	// lastTurnCap is the MAX_TURNS the most recent review round was actually
	// spawned with, read back from its environment rather than assumed from
	// the constant, so the log reports what the container received.
	lastTurnCap string
	// vendored records that the per-Step dependency cache has been refilled
	// for a fix run — see ensureVendoredCache. Once per stage, not once per
	// round.
	vendored bool
	// openMustFix carries the previous round's must-fix findings, keyed by
	// finding key, so the next round can be classified as fixed / still open
	// / new rather than as an undifferentiated list.
	openMustFix map[string]reviewFindingOutput
	// seen carries EVERY finding key any earlier round reported, must-fix or
	// not. openMustFix answers "was this one routed back"; this answers "have
	// we seen this at all", which is what makes a finding NEW.
	seen map[string]bool
}

// run is the loop. It always returns nil for the Step: a review failure is
// recorded, never fatal. The ONE exception is a user stop, which is not a
// review failure at all and must reach the outer loop's stop path.
func (s *reviewStage) run() (map[string]interface{}, error) {
	if len(s.baseCommits) == 0 {
		// No baseline means no diff, and a review of no diff would report a
		// clean bill of health for work it never saw. Record that and let the
		// Step proceed exactly as it would have without this stage.
		s.recordFailedRound(1, "no start-of-run commit was recorded for any repository", agentResult{})
		return s.finish()
	}
	if s.reviewerFailure != "" {
		// FAIL-OPEN, like every other review failure: recorded as coverage
		// naming the reason, never a failed Step. What it must NOT do is run
		// the review on the implementer's model instead — the record would
		// then name a reviewer that did not review.
		io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: %s — continuing without a review\n", s.reviewerFailure))
		s.recordFailedRound(1, s.reviewerFailure, agentResult{})
		return s.finish()
	}
	if s.participation == participationAdvisory {
		io.WriteString(s.logsWriter, "Review stage: participation is advisory — every finding will be annotated on the pull request, nothing is routed back to the agent, and nothing holds the pull request\n")
	}
	s.openMustFix = map[string]reviewFindingOutput{}
	round := 1
	mustFixRounds := 0
	for {
		if !s.canAfford(reviewRunTimeout) {
			io.WriteString(s.logsWriter, "Review stage: not enough of the stage budget left for another review round — stopping here\n")
			break
		}
		s.reportStage(tasks.StageReview)
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d: reviewing the change against the Task's spec\n", round))
		result, err := s.runReviewRound(round)
		if errors.Is(err, types.ErrJobStoppedByUser) {
			// A user stop is not a failed review. Attribute what the round
			// spent and hand the stop to the outer loop's existing path.
			_ = accumulateReviewRunUsage(s.parameters, s.reviewerView(), result)
			return s.parameters, err
		}
		_ = accumulateReviewRunUsage(s.parameters, s.reviewerView(), result)
		if reason := reviewRoundFailure(result, err); reason != "" {
			io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d did not complete: %s — continuing without it\n", round, reason))
			s.recordFailedRound(round, reason, result)
			break
		}
		findings := s.classify(result)
		s.recordRound(round, findings, result)
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d completed: %d finding(s) in %d turn(s) of %s\n", round, len(findings), result.Turns, s.lastTurnCap))
		// Nothing to route back — including every advisory review, whose
		// findings are annotations by definition (see classify).
		mustFix := mustFixOnly(findings)
		if len(mustFix) == 0 {
			s.rememberOpenMustFix(nil)
			break
		}
		s.rememberOpenMustFix(mustFix)
		if mustFixRounds >= maxMustFixRounds {
			io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: %d must-fix finding(s) still open after %d fix round(s) — handing the evidence to a human on the pull request\n", len(mustFix), mustFixRounds))
			break
		}
		if !s.canAfford(mustFixRunTimeout + reviewRunTimeout) {
			io.WriteString(s.logsWriter, "Review stage: not enough of the stage budget left for a fix round and the review that would follow it — handing the evidence to a human on the pull request\n")
			break
		}
		s.reportStage(tasks.StageImplement)
		if err := s.runMustFixRound(mustFix); err != nil {
			if errors.Is(err, types.ErrJobStoppedByUser) {
				return s.parameters, err
			}
			// A fix run that broke the build fails the Step BEFORE
			// CommitAndPush, exactly as the implement run's verify gate
			// does — the whole point of the gate is that broken code never
			// reaches a commit, and a fix is code like any other.
			return s.parameters, err
		}
		mustFixRounds++
		round++
	}
	return s.finish()
}

// canAfford reports whether the stage budget can accommodate a run of this
// length. Checked BEFORE starting a round: a round the budget cuts short
// spends its tokens and produces nothing.
func (s *reviewStage) canAfford(d time.Duration) bool {
	return time.Now().Add(d).Before(s.deadline)
}

// finish writes the review block, reports the final result to the control
// plane, and returns. Always nil error — see run.
func (s *reviewStage) finish() (map[string]interface{}, error) {
	out := &reviewOutput{
		Participation: participationName(s.participation),
		Rounds:        s.rounds,
		FixedInLoop:   s.fixedInLoop,
		MustFixOpen:   len(s.openMustFix) > 0,
	}
	s.logFullReview(out)
	if err := mergeReviewIntoJobOutput(s.parameters, out); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not record the review on the job output: %s\n", err))
	}
	s.reportReviewResult(out)
	return s.parameters, nil
}

// classify turns one round's raw findings into the Step's record: each finding
// marked must-fix or not, and the previous round's must-fix findings that are
// no longer present recorded as fixed.
//
// Pairing is by Finding.Key, with a synthesised key when the producer omitted
// one — parameter, location and the first 80 runes of what, which is enough to
// recognise the same finding after a fix attempt without being so specific
// that a reworded report looks like a new problem.
func (s *reviewStage) classify(result agentResult) []reviewFindingOutput {
	if result.ReviewResult == nil {
		// Unreachable on the live path — reviewRoundFailure ends the loop for
		// a round with no review_result — but classify must not be the thing
		// that turns a missing report into a crashed Step.
		return nil
	}
	findings := make([]reviewFindingOutput, 0, len(result.ReviewResult.Findings))
	present := map[string]bool{}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	firstRound := len(s.rounds) == 0
	for _, f := range result.ReviewResult.Findings {
		out := reviewFindingOutput{
			Key:       f.Key,
			Parameter: f.Parameter,
			Severity:  f.Severity,
			Location:  f.Location,
			What:      f.What,
			Why:       f.Why,
			Stage:     f.Stage,
			Pass:      f.Pass,
			// Advisory marks NOTHING must-fix. The threshold still exists and
			// the finding may well meet it, but a Task whose review is
			// advisory opted out of the must-fix half — so recording the
			// finding as must-fix would make the PR body say a change is
			// blocked when nothing is blocking it.
			MustFix: s.participation == participationOn && s.isMustFix(f),
		}
		key := findingKey(out)
		// Nothing is "new" in the first round — everything is. The flag only
		// means something once there is a round to be new since.
		out.New = !firstRound && !s.seen[key]
		findings = append(findings, out)
		present[key] = true
	}
	for key := range present {
		s.seen[key] = true
	}
	for key, previous := range s.openMustFix {
		if !present[key] {
			s.fixedInLoop = append(s.fixedInLoop, previous)
		}
	}
	return findings
}

// isMustFix is the whole gate, and it is the RUNNER's decision: the severity
// and parameter names are mapped through deployment-runner-kit's wire mirror
// and compared with the thresholds stamped into the Job at creation.
//
// A finding whose parameter or severity does not parse is ANNOTATED, never
// treated as must-fix. A threshold cannot be looked up for a parameter nobody
// can name, and routing an unreadable finding back to the implementer would
// spend a fix round on a report it cannot act on.
//
// An absent or zero threshold is never met at any severity, including
// Critical. That is the annotate-only case, and it is deliberately not the
// same as "Info and above".
func (s *reviewStage) isMustFix(f reviewFinding) bool {
	parameter, ok := tasks.ReviewParameterValue(f.Parameter)
	if !ok {
		return false
	}
	severity, ok := tasks.ReviewSeverityValue(f.Severity)
	if !ok {
		return false
	}
	threshold, ok := s.thresholds[parameter]
	if !ok || threshold == 0 {
		return false
	}
	return severity >= threshold
}

func (s *reviewStage) rememberOpenMustFix(findings []reviewFindingOutput) {
	s.openMustFix = map[string]reviewFindingOutput{}
	for _, f := range findings {
		s.openMustFix[findingKey(f)] = f
	}
}

func (s *reviewStage) recordRound(round int, findings []reviewFindingOutput, result agentResult) {
	var coverage []reviewCoverage
	if result.ReviewResult != nil {
		coverage = result.ReviewResult.Coverage
	}
	s.rounds = append(s.rounds, reviewRoundOutput{
		Round:      round,
		Findings:   findings,
		Coverage:   coverageOutputs(coverage),
		AgentType:  s.jobAgentType(),
		Model:      s.jobModel(),
		TokenUsage: result.TokenUsage,
		CostUSD:    result.CostUSD,
		Turns:      result.Turns,
		Completed:  true,
	})
}

// recordFailedRound records a round that produced no usable report. Coverage
// is NotChecked for EVERY parameter with the failure as its reason — the
// record has to say that nothing was examined and why, because the alternative
// is a PR that looks reviewed.
//
// Findings from earlier rounds that DID complete are untouched: they are still
// rendered as fixed or annotated. Only the coverage of this round is the
// all-NotChecked record.
func (s *reviewStage) recordFailedRound(round int, reason string, result agentResult) {
	s.rounds = append(s.rounds, reviewRoundOutput{
		Round:      round,
		Coverage:   notCheckedCoverage(reason),
		Turns:      result.Turns,
		AgentType:  s.jobAgentType(),
		Model:      s.jobModel(),
		TokenUsage: result.TokenUsage,
		CostUSD:    result.CostUSD,
		Completed:  false,
		Error:      reason,
	})
	// A round that did not complete asserts nothing. Anything a previous
	// round left open stays open only if a previous round said so; a failed
	// round never opens a must-fix finding of its own.
	s.rememberOpenMustFix(nil)
}

// reviewerView is the parameter view a REVIEW round runs under: the reviewer's
// when the Job names one, the Job's own otherwise.
//
// One accessor rather than a check at every call site, because every one of
// them has to give the same answer — the env the round spawns with, the agent
// and model its record names, and the model its cost is priced against. A
// record that named a reviewer the spawn did not use would be worse than no
// record at all.
func (s *reviewStage) reviewerView() map[string]interface{} {
	if s.reviewerParams != nil {
		return s.reviewerParams
	}
	return s.parameters
}

// jobAgentType / jobModel answer "who reviewed", so they read the REVIEWER's
// view. With no reviewer named that is the Job's own agent and model, which is
// what these have always returned.
func (s *reviewStage) jobAgentType() string {
	v, _ := jobs.GetParameterValue[string](s.reviewerView(), parameters_enums.AgentType)
	return v
}

func (s *reviewStage) jobModel() string {
	v, _ := jobs.GetParameterValue[string](s.reviewerView(), parameters_enums.Model)
	return v
}

// reportStage tells the control plane which stage is running, so a Step that
// has been going for twenty minutes reads as Review rather than an unexplained
// stretch of Running. Best-effort: logged and non-fatal, because a Step's work
// does not depend on the card being right.
func (s *reviewStage) reportStage(stage string) {
	reportTaskStepStage(s.parameters, s.ctx, stage, s.logsWriter)
}

// reportTaskStepStage is the shared reporter: RunAgentStep calls it when the
// implementer starts, and the Review stage calls it at each transition.
//
// Best-effort by design — logged, non-fatal. The Step's work does not depend
// on the card being right, and a control plane that is briefly unreachable
// must not cost a Task its run.
func reportTaskStepStage(parameters map[string]interface{}, ctx commandUtils.TaskJobContext, stage string, logsWriter io.Writer) {
	jobID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	err := client.Get().MarkTaskStepStage(ctx.OrganizationID, tasks.UpdateTaskStepStageDtoV1{
		TaskID:    ctx.TaskID,
		StepIndex: int(ctx.StepIndex),
		JobID:     jobID,
		Stage:     stage,
	})
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not report the %s stage: %s\n", stage, err))
	}
}

// reportReviewResult sends the FINAL round's result to deployment-server to be
// stored on the Step, carrying that round's own agent type and model as the
// review run's provenance — the REVIEWER's when the Task named one, the Job's
// own otherwise. Either way it names what actually produced these findings.
func (s *reviewStage) reportReviewResult(out *reviewOutput) {
	if len(out.Rounds) == 0 {
		return
	}
	final := out.Rounds[len(out.Rounds)-1]
	jobID, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.JobID)
	dto := tasks.UpdateTaskStepReviewDtoV1{
		TaskID:    s.ctx.TaskID,
		StepIndex: int(s.ctx.StepIndex),
		JobID:     jobID,
		Result: tasks.ReviewResultDtoV1{
			Findings:    reviewFindingDtos(final.Findings),
			Coverage:    reviewCoverageDtos(final.Coverage),
			MustFixOpen: out.MustFixOpen,
			AgentType:   final.AgentType,
			Model:       final.Model,
			TokenUsage: tasks.ReviewTokenUsageDtoV1{
				InputTokens:         final.TokenUsage.InputTokens,
				OutputTokens:        final.TokenUsage.OutputTokens,
				CacheReadTokens:     final.TokenUsage.CacheReadTokens,
				CacheCreationTokens: final.TokenUsage.CacheCreationTokens,
			},
		},
	}
	if final.CostUSD != nil {
		dto.Result.CostUSD = *final.CostUSD
	}
	if err := client.Get().UpdateTaskStepReview(s.ctx.OrganizationID, dto); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not report the review result: %s\n", err))
	}
}

// logFullReview writes the COMPLETE, untruncated review to the job log. The PR
// body renders a bounded selection; this is where the rest lives, so a
// reviewer who wants everything has somewhere to go.
func (s *reviewStage) logFullReview(out *reviewOutput) {
	var b strings.Builder
	b.WriteString("\n--- Review stage ---\n")
	for _, round := range out.Rounds {
		if !round.Completed {
			b.WriteString(fmt.Sprintf("Round %d: did not complete — %s\n", round.Round, round.Error))
		} else {
			b.WriteString(fmt.Sprintf("Round %d: %d finding(s), %d turn(s)\n", round.Round, len(round.Findings), round.Turns))
		}
		for _, f := range round.Findings {
			marker := "annotated"
			if f.MustFix {
				marker = "MUST FIX"
			}
			if f.New {
				marker += ", new since the last round"
			} else if round.Round > 1 {
				marker += ", still open"
			}
			b.WriteString(fmt.Sprintf("  [%s] %s/%s at %s — %s\n", marker, f.Parameter, f.Severity, f.Location, f.What))
			if f.Why != "" {
				b.WriteString(fmt.Sprintf("      why: %s\n", f.Why))
			}
		}
		for _, c := range round.Coverage {
			line := fmt.Sprintf("  coverage: %s — %s", c.Parameter, c.State)
			if c.Reason != "" {
				line += " (" + c.Reason + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	for _, f := range out.FixedInLoop {
		b.WriteString(fmt.Sprintf("  [fixed in loop] %s/%s at %s — %s\n", f.Parameter, f.Severity, f.Location, f.What))
	}
	b.WriteString(fmt.Sprintf("Must-fix findings still open: %t\n", out.MustFixOpen))
	io.WriteString(s.logsWriter, b.String())
}

// reviewRoundFailure names why a round produced no usable report, or "" when
// it did. Every shape here ENDS THE LOOP and is never fatal.
//
// The older-agentbox case is called out by name. An image that predates review
// mode rejects AGENT_MODE=review at startup and writes a config failure —
// which is not a broken review, it is a deployment that has not caught up, and
// saying so is the difference between an ops question with an answer and one
// without.
func reviewRoundFailure(result agentResult, err error) string {
	if err != nil {
		if isOlderAgentboxRejection(result) {
			return olderAgentboxReason
		}
		return err.Error()
	}
	if isOlderAgentboxRejection(result) {
		return olderAgentboxReason
	}
	if result.Status != "success" {
		if strings.TrimSpace(result.Error) != "" {
			return fmt.Sprintf("the review run reported status %q: %s", result.Status, result.Error)
		}
		return fmt.Sprintf("the review run reported status %q", result.Status)
	}
	if result.ReviewResult == nil {
		return "the review run produced no review_result"
	}
	return ""
}

// olderAgentboxReason is the coverage reason for the named special case: the
// image cannot run the stage at all.
const olderAgentboxReason = "the agentbox image predates the Review stage, so no review ran"

// isOlderAgentboxRejection detects agentbox's own startup rejection of an
// AGENT_MODE it does not know. agentbox writes a result.json for it (the
// config-error path), so this is read from the payload rather than guessed
// from an exit code.
func isOlderAgentboxRejection(result agentResult) bool {
	message := strings.ToLower(result.Error)
	return strings.Contains(message, "agent_mode") && strings.Contains(message, "invalid")
}

// notCheckedCoverage is the coverage record for a round that examined nothing:
// every parameter, NotChecked, with the same reason.
func notCheckedCoverage(reason string) []reviewCoverageOutput {
	parameters := []string{
		"security", "correctness", "spec conformance", "testing",
		"deploy readiness", "performance", "maintainability", "reliability",
	}
	out := make([]reviewCoverageOutput, 0, len(parameters))
	for _, parameter := range parameters {
		out = append(out, reviewCoverageOutput{
			Parameter: parameter,
			State:     "not checked",
			Reason:    "review run failed: " + reason,
		})
	}
	return out
}

func coverageOutputs(in []reviewCoverage) []reviewCoverageOutput {
	out := make([]reviewCoverageOutput, 0, len(in))
	for _, c := range in {
		out = append(out, reviewCoverageOutput{Parameter: c.Parameter, State: c.State, Reason: c.Reason})
	}
	return out
}

func reviewFindingDtos(in []reviewFindingOutput) []tasks.ReviewFindingDtoV1 {
	out := make([]tasks.ReviewFindingDtoV1, 0, len(in))
	for _, f := range in {
		out = append(out, tasks.ReviewFindingDtoV1{
			Key: f.Key, Parameter: f.Parameter, Severity: f.Severity,
			Location: f.Location, What: f.What, Why: f.Why, Stage: f.Stage, Pass: f.Pass,
		})
	}
	return out
}

func reviewCoverageDtos(in []reviewCoverageOutput) []tasks.ReviewCoverageDtoV1 {
	out := make([]tasks.ReviewCoverageDtoV1, 0, len(in))
	for _, c := range in {
		out = append(out, tasks.ReviewCoverageDtoV1{Parameter: c.Parameter, State: c.State, Reason: c.Reason})
	}
	return out
}

func mustFixOnly(findings []reviewFindingOutput) []reviewFindingOutput {
	var out []reviewFindingOutput
	for _, f := range findings {
		if f.MustFix {
			out = append(out, f)
		}
	}
	return out
}

// findingKey is how the same finding is recognised across rounds. The
// producer's own key when it supplied one; otherwise parameter, location and
// the first 80 runes of what — specific enough to distinguish two problems in
// one file, loose enough that a reworded report of the same problem still
// pairs.
func findingKey(f reviewFindingOutput) string {
	if key := strings.TrimSpace(f.Key); key != "" {
		return key
	}
	what := []rune(strings.TrimSpace(f.What))
	if len(what) > 80 {
		what = what[:80]
	}
	return strings.ToLower(strings.TrimSpace(f.Parameter) + "|" + strings.TrimSpace(f.Location) + "|" + string(what))
}

func participationName(participation int64) string {
	switch participation {
	case participationOn:
		return "on"
	case participationAdvisory:
		return "advisory"
	case participationOff:
		return "off"
	}
	return ""
}

// readReviewParticipation reads the RESOLVED participation stamped at Job
// creation. An absent or unreadable value means Off: every Job created before
// this stage existed carries no such parameter, and those Steps must behave
// exactly as they did.
//
// A value that is PRESENT but not a usable int64 is a different thing entirely
// — a stamping bug, or a parameter that crossed the wire as a float64 — and it
// is logged. Silently reading the same as "this Task opted out" is how a
// mistyped parameter turns the whole stage off across every Task without
// anyone noticing that it had ever been on.
func readReviewParticipation(parameters map[string]interface{}, logsWriter io.Writer) int64 {
	// Keyed through Key(), which is the persisted decimal string the
	// parameter map actually uses — not String(), which is the display name.
	key, err := parameters_enums.ReviewParticipation.Key()
	if err != nil {
		return participationOff
	}
	raw, present := parameters[key]
	if !present {
		return participationOff
	}
	v, err := jobs.GetParameterValue[int64](parameters, parameters_enums.ReviewParticipation)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: the review participation parameter is present but unreadable (%T: %v) — running this Step without the Review stage\n", raw, err))
		return participationOff
	}
	if v < participationOn || v > participationOff {
		io.WriteString(logsWriter, fmt.Sprintf("warning: the review participation parameter is %d, which names no participation mode — running this Step without the Review stage\n", v))
		return participationOff
	}
	return v
}

// decodeMustFixThresholds reads the org's resolved policy: parameter value to
// severity value, both decimal, e.g. {"1":4,"2":4}.
//
// An unreadable payload yields an empty map, which is annotate-only for every
// parameter — the review still runs and still reports, it just gates nothing.
// That is the right direction to fail: the alternative is inventing a
// threshold nobody wrote and holding a pull request on it.
func decodeMustFixThresholds(parameters map[string]interface{}, logsWriter io.Writer) map[uint]uint {
	out := map[uint]uint{}
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.ReviewMustFixThresholds)
	if err != nil || strings.TrimSpace(raw) == "" {
		return out
	}
	decoded := map[string]uint{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not read the review must-fix thresholds (%s) — every finding will be annotated\n", err))
		return out
	}
	for key, severity := range decoded {
		parameter, err := strconv.Atoi(key)
		if err != nil || parameter <= 0 {
			continue
		}
		out[uint(parameter)] = severity
	}
	return out
}

// readBaseCommitsFromJobOutput pulls the base_commits block CheckoutRepository
// wrote, keyed by the directory path relative to /work — which is how the
// review container sees each repository.
func readBaseCommitsFromJobOutput(parameters map[string]interface{}) map[string]string {
	out := map[string]string{}
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return out
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return out
	}
	for _, bc := range data.BaseCommits {
		if bc.Dir == "" || bc.CommitSHA == "" {
			continue
		}
		out[bc.Dir] = bc.CommitSHA
	}
	return out
}

// mergeReviewIntoJobOutput writes the review block onto the accumulated
// envelope, where OpenPullRequest reads it to render the Review section.
func mergeReviewIntoJobOutput(parameters map[string]interface{}, out *reviewOutput) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.Review = out
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// readReviewFromJobOutput pulls the review block back off the envelope. Nil
// when the stage did not run or the payload is unreadable — OpenPullRequest
// renders no Review section in that case, which is the pre-stage behaviour.
func readReviewFromJobOutput(parameters map[string]interface{}) *reviewOutput {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil
	}
	return data.Review
}
