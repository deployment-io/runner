package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
)

// RunQAStage is the QA stage: between RunReviewStage and CommitAndPush, a
// separate agent run deploys the Task's static-site previews and checks the
// spec's acceptance criteria in a real browser.
//
// THE QA RUN IS BLIND. It gets no diff, no implementer summary and no
// repository: its /work is a sibling directory holding only agentbox's output
// directories and the read-only deployment context. It deploys previews by
// repository name, and the runner builds them from the Task's working tree.
//
// THE STAGE IS ADVISORY. It never holds the pull request, never makes it a
// draft and never fails the Step. One fix round at most: when round 1 finds an
// acceptance criterion failed, the implementer gets one fix run, the review
// gets one round on the fix's diff, and a final QA round reports. A fix run
// that cannot finish is undone exactly as the Review stage's is — and a
// restore that does not work is the one path that fails the Step.
type RunQAStage struct {
	// stopSignal and progressSink are the outer loop's, exactly as for
	// RunReviewStage.
	stopSignal   <-chan struct{}
	progressSink func(jobs.LiveProgressV1)
}

func (rq *RunQAStage) SetStopSignal(stop <-chan struct{}) { rq.stopSignal = stop }

func (rq *RunQAStage) SetProgressSink(sink func(jobs.LiveProgressV1)) { rq.progressSink = sink }

const (
	// qaRunTimeout is the wall clock for one QA run: deploying the previews,
	// waiting for their builds and checking each criterion in a browser.
	qaRunTimeout = 20 * time.Minute
	// qaRunMaxTurns is a ceiling that catches a looping QA agent, as
	// reviewRunMaxTurns is for a reviewer.
	qaRunMaxTurns = 80
	// qaStageBudget caps the whole stage: both QA rounds, the fix run and the
	// review round after it. Checked before the fix round, which is only
	// started when everything after it fits, and again before the review
	// round and the final QA round, since the fix run can overrun its own
	// timeout (vendoring a missing cache).
	qaStageBudget = 100 * time.Minute
)

// qaNoStaticSiteReason is the skipped reason when no changed repository is a
// static site the runner can build a preview of.
const qaNoStaticSiteReason = "no static site in this change"

// qaOlderAgentboxReason names the older-image case: an agentbox before 1.9.32
// rejects AGENT_MODE=qa at startup.
const qaOlderAgentboxReason = "the agentbox image predates QA mode (needs 1.9.32 or later)"

// qaNoTimeForFinalRoundReason is the final round's status reason when a fix
// was kept but the stage's budget left no time to check the fixed tree.
const qaNoTimeForFinalRoundReason = "no time left to check the fixed tree in a final QA round"

func (rq *RunQAStage) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkStepDone(parameters, err)
		}
	}()
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(ctx.OrganizationID, ctx.TaskID)
	// Every directory the stage parks beside the work dir — the QA rounds',
	// the context copy, and what the fix run and its review round leave — goes
	// however the stage ends.
	defer cleanupQAStageSiblings(workDirHost)
	qaParams, qaFailure := qaParameters(parameters)
	stage := &qaStage{
		ctx:          ctx,
		parameters:   parameters,
		workDirHost:  workDirHost,
		logsWriter:   logsWriter,
		qaParams:     qaParams,
		qaFailure:    qaFailure,
		deadline:     time.Now().Add(qaStageBudget),
		stopSignal:   rq.stopSignal,
		progressSink: rq.progressSink,
	}
	return stage.run()
}

// qaParameters is the Job's parameters as the QA run sees them: the QA model,
// agent and credentials when the Job names a QA agent (QAAgentType), built
// exactly as the reviewer's view is. (nil, "") when it names none — the QA run
// then uses the Job's own agent, model and credentials.
func qaParameters(parameters map[string]interface{}) (map[string]interface{}, string) {
	return agentViewParameters(parameters, agentViewKeys{
		agentType: parameters_enums.QAAgentType,
		model:     parameters_enums.QAModel,
		provider:  parameters_enums.QAAgentProvider,
		envVars:   parameters_enums.QAAgentEnvVars,
		role:      "QA",
	})
}

// cleanupQAStageSiblings removes every host directory the QA stage parks
// beside the work dir, and what its fix run and review round may leave there
// and inside it.
func cleanupQAStageSiblings(workDirHost string) {
	base := strings.TrimRight(workDirHost, "/")
	for _, pattern := range []string{base + "-qa-round-*", base + "-qa-context-*"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, dir := range matches {
			_ = os.RemoveAll(dir)
		}
	}
	cleanupReviewStageSiblings(workDirHost)
	_ = os.RemoveAll(filepath.Join(workDirHost, reviewDiffDirName))
	_ = os.RemoveAll(filepath.Join(workDirHost, reviewFixDiffDirName))
}

// qaStage holds one Step's QA stage.
type qaStage struct {
	ctx         commandUtils.TaskJobContext
	parameters  map[string]interface{}
	workDirHost string
	logsWriter  io.Writer
	// qaParams is the QA view (qaParameters), nil when the Job names no QA
	// agent; qaFailure is why the named QA agent cannot run, or "".
	qaParams     map[string]interface{}
	qaFailure    string
	deadline     time.Time
	stopSignal   <-chan struct{}
	progressSink func(jobs.LiveProgressV1)
	// contextCopyDir is the stage's read-only copy of the deployment context,
	// bound at /work/context in every QA round and the review round, or "".
	contextCopyDir string
	// repositories is QA_REPOSITORIES: the changed, buildable repositories.
	repositories []string

	out *qaOutput
	// fixStage is the reviewStage the fix run ran in, and the review round
	// after a kept fix runs in.
	fixStage *reviewStage

	// Test seams; nil means the real thing. runQA is one QA round; runFix is
	// the fix run, handed its prompt; checkPreview is the buildability check
	// for one repository; cacheExists says whether the Step's cache volume is
	// still there; reviewer configures the review round's stage.
	runQA        func(round int) (agentResult, error)
	runFix       func(prompt string) (agentResult, error)
	checkPreview func(repository string) agenttools.RunnerBuiltPreviewCheck
	cacheExists  func(name string) bool
	reviewer     func(rs *reviewStage)
	// fixTreesTmpRoot and fixTreesGit are the fix trees' and the gate's
	// temporary root and git binary; "" means the defaults. Tests only.
	fixTreesTmpRoot string
	fixTreesGit     string
}

// qaView is the parameter view a QA round runs under.
func (s *qaStage) qaView() map[string]interface{} {
	if s.qaParams != nil {
		return s.qaParams
	}
	return s.parameters
}

func (s *qaStage) agentType() string {
	v, _ := jobs.GetParameterValue[string](s.qaView(), parameters_enums.AgentType)
	return v
}

func (s *qaStage) model() string {
	v, _ := jobs.GetParameterValue[string](s.qaView(), parameters_enums.Model)
	return v
}

// run is the gate, round 1, and the one fix round. It returns an error only for
// a user stop and for a fix run whose undo did not work.
func (s *qaStage) run() (map[string]interface{}, error) {
	repositories, status, reason := s.gate()
	if status == "" && len(repositories) == 0 {
		// Nothing changed: nothing to check and nothing to report.
		io.WriteString(s.logsWriter, "QA stage: no repository changed — no QA run\n")
		return s.parameters, nil
	}
	s.repositories = repositories
	s.out = &qaOutput{Repositories: repositories}
	if status != "" {
		s.out.Status, s.out.StatusReason = status, reason
		return s.finish()
	}
	release, reason := s.holdImage()
	defer release()
	if reason != "" {
		s.out.Status, s.out.StatusReason = tasks.QAStatusCouldNotRun, reason
		return s.finish()
	}
	defer s.prepareContextCopy()()
	round1, err := s.qaRound(1)
	if err != nil {
		return s.parameters, err
	}
	if !s.shouldSendBack(round1) {
		return s.finish()
	}
	if !s.canAfford(mustFixRunTimeout + reviewRunTimeout + qaRunTimeout) {
		io.WriteString(s.logsWriter, "QA: no time left for a fix round\n")
		return s.finish()
	}
	kept, err := s.fixRound(round1)
	if err != nil {
		return s.parameters, err
	}
	if !kept {
		return s.finish()
	}
	if err := s.reviewFix(); err != nil {
		return s.parameters, err
	}
	if !s.canAfford(qaRunTimeout) {
		// Round 1 checked the tree before the fix, so its verdicts are not the
		// committed tree's: the final round is recorded as one that could not run.
		io.WriteString(s.logsWriter, "QA: no time left for the final QA round\n")
		s.out.Rounds = append(s.out.Rounds, qaRoundOutput{
			Round:        2,
			Status:       tasks.QAStatusCouldNotRun,
			StatusReason: qaNoTimeForFinalRoundReason,
			AgentType:    s.agentType(),
			Model:        s.model(),
		})
		return s.finish()
	}
	final, err := s.qaRound(2)
	if err != nil {
		return s.parameters, err
	}
	s.out.FixedByQA = fixedByQA(round1, final)
	return s.finish()
}

// holdImage re-holds the agentbox image for the stage's runs, as
// RunReviewStage does, so a concurrent Step pulling a newer tag can't prune it
// between rounds; re-pulled only if it went missing. It returns the release
// and, when the image is unavailable, why the stage cannot run.
func (s *qaStage) holdImage() (func(), string) {
	noop := func() {}
	if s.runQA != nil {
		return noop, ""
	}
	imageRef, err := jobs.GetParameterValue[string](s.parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return noop, fmt.Sprintf("agentbox image missing: %s", err)
	}
	release := reclaim.MarkImageInUse(imageRef)
	if err := ensureAgentboxImage(imageRef); err != nil {
		return release, fmt.Sprintf("error pulling the agentbox image: %s", err)
	}
	return release, ""
}

// shouldSendBack is round 1's question: a completed check of real acceptance
// criteria with at least one failed. A smoke check (no criteria) is never sent
// back — without a comparison against the current deployment, a page error
// cannot be attributed to the change.
func (s *qaStage) shouldSendBack(round qaRoundOutput) bool {
	if round.Status != tasks.QAStatusCompleted || round.NoCriteria {
		return false
	}
	return len(failedCriteria(round)) > 0
}

func (s *qaStage) canAfford(d time.Duration) bool {
	return time.Now().Add(d).Before(s.deadline)
}

// qaRound runs one QA round, records it and folds its usage into the "qa"
// stage. A user stop is returned; everything else is a recorded round.
func (s *qaStage) qaRound(round int) (qaRoundOutput, error) {
	reportTaskStepStage(s.parameters, s.ctx, tasks.StageQA, s.logsWriter)
	io.WriteString(s.logsWriter, fmt.Sprintf("QA round %d: checking the acceptance criteria on a preview of %s\n", round, strings.Join(s.repositories, ", ")))
	var result agentResult
	var err error
	if s.runQA != nil {
		result, err = s.runQA(round)
	} else {
		result, err = s.runQARound(round)
	}
	s.accumulateQARun(round, result)
	if errors.Is(err, types.ErrJobStoppedByUser) {
		return qaRoundOutput{}, err
	}
	out := s.roundOutput(round, result, err)
	s.out.Rounds = append(s.out.Rounds, out)
	return out, nil
}

// accumulateQARun folds a QA round's usage and cost into the Step's totals
// under the "qa" stage. Its blocked hosts are the QA agent's own and go to the
// job log, never to the Step's record — see accumulateReviewRun.
func (s *qaStage) accumulateQARun(round int, result agentResult) {
	if len(result.DeniedHosts) > 0 {
		io.WriteString(s.logsWriter, fmt.Sprintf("QA round %d: the QA agent's requests to these hosts were blocked: %s\n",
			round, strings.Join(result.DeniedHosts, ", ")))
	}
	result.DeniedHosts = nil
	if err := accumulateReviewRunUsage(s.parameters, s.qaView(), usageStageQA, result); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not record QA round %d's usage: %s\n", round, err))
	}
}

// prepareContextCopy writes the stage's read-only copy of the deployment
// context, as the Review stage does, and returns its removal.
func (s *qaStage) prepareContextCopy() func() {
	base := filepath.Base(strings.TrimRight(s.workDirHost, "/"))
	dir, written, cleanup, reason := writeContextCopy(s.ctx, s.workDirHost, base+"-qa-context-*", s.logsWriter)
	if reason != "" {
		io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: deployment context unavailable (%s) — the QA run works without it\n", reason))
		return func() {}
	}
	s.contextCopyDir = dir
	io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: %d deployment context file(s) mounted read-only at /work/context\n", written))
	return func() {
		cleanup()
		s.contextCopyDir = ""
	}
}

// finish logs, records and reports the stage's final result. Always nil error.
func (s *qaStage) finish() (map[string]interface{}, error) {
	if final := s.out.finalRound(); final != nil {
		s.out.Status, s.out.StatusReason, s.out.NoCriteria = final.Status, final.StatusReason, final.NoCriteria
	}
	s.logFullQA()
	if err := mergeQAIntoJobOutput(s.parameters, s.out); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not record QA on the job output: %s\n", err))
	}
	s.reportQAResult()
	return s.parameters, nil
}
