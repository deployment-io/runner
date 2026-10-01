package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
// at or above the org's FIX threshold back to the implementer inside this same
// Job, and records what it found. Findings at or above the HOLD (must-fix)
// threshold that are still open when the loop ends hold the pull request; a
// sent-back finding below it that survives is noted and holds nothing.
//
// THE STAGE IS FAIL-OPEN. A review that cannot complete — a round that failed,
// an agentbox image that predates review mode, a baseline that was never
// recorded — degrades the Step to exactly today's behaviour: commit, push,
// ordinary pull request. It never fails the Step and never discards work. The
// cost of that choice is that a systematically broken review is invisible
// except in the PR body and the job log, which is why every failure is
// RECORDED as coverage naming the reason rather than passed over in silence.
//
// THAT RULE COVERS THE FIX RUN TOO. A fix run edits a tree that already passed
// its own verify gate, so it only starts once it can be undone: the repository
// directories are copied first, and a fix run that does not finish is rolled
// back to exactly that copy and its findings handed to a human on the pull
// request. See attemptFix and run_review_stage_fix_snapshot.go.
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
	// the budget a review is expected to use. It is passed as MAX_TURNS and
	// enforced by the agent's harness — for Claude Code, --max-turns, which
	// counts MODEL RESPONSES. One response can carry many tool calls in
	// parallel (a measured run made 13 calls in 3 responses), so this is not a
	// cap on reads. The turns an agent REPORTS afterwards (num_turns, recorded
	// on each round) are roughly one per tool call: a careful review of a
	// one-file diff reported 26 while staying under a 20-response cap.
	//
	// Time and cost are bounded by reviewRunTimeout and the stage budget. A
	// cap tight enough to bind on a real review would end it with a max-turns
	// error and no report, the round would be recorded as failed, and the pull
	// request would open unreviewed — so this errs high.
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
	// Nothing between here and stage.run() returns early, which is what makes the
	// stage's own defer the single owner of the cache volume below. This parse is
	// the exception, and it reads the very parameters RunAgentStep parsed a
	// moment ago from the same map — a Step that fails here never had an
	// implement run to leave a volume behind.
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	cacheVolume, cacheKept := adoptCacheVolume(ctx, cacheVolumeExists, createCacheVolume, logsWriter)
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
	// The fix diffs written for a round are removed when that round ends; this
	// is the net for a round that crashed before it could.
	defer os.RemoveAll(filepath.Join(workDirHost, reviewFixDiffDirName))
	// Resolved ONCE, before the loop: every round of this stage runs on the
	// same reviewer, and so does every round's record.
	reviewerParams, reviewerFailure := reviewerParameters(parameters)
	stage := &reviewStage{
		ctx:             ctx,
		parameters:      parameters,
		workDirHost:     workDirHost,
		cacheVolume:     cacheVolume,
		vendored:        cacheKept,
		reviewerParams:  reviewerParams,
		reviewerFailure: reviewerFailure,
		logsWriter:      logsWriter,
		participation:   participation,
		thresholds:      decodeMustFixThresholds(parameters, logsWriter),
		fixThresholds:   decodeFixThresholds(parameters, logsWriter),
		baseCommits:     readBaseCommitsFromJobOutput(parameters),
		repoDirs:        readRepositoryDirsFromJobOutput(parameters),
		deadline:        time.Now().Add(reviewStageBudget),
		stopSignal:      rs.stopSignal,
		progressSink:    rs.progressSink,
		// Stamped on the Job by the control plane, for the orgs being
		// measured only: a temporary measurement knob (see
		// run_review_stage_shadow.go).
		shadowEffort: readShadowEffort(parameters, logsWriter),
	}
	return stage.run()
}

// adoptCacheVolume takes over the Step's dependency cache for the stage, and
// says whether the implement run's own is still on it.
//
// THE VOLUME NOW SPANS THE WHOLE STEP. RunAgentStep leaves it in place when a
// review follows it (keepCacheVolumeForReview), so what this stage mounts still
// holds the implement run's vendored dependencies AND its build cache — which is
// what keeps the first review round from running a build with nothing vendored
// (a reviewer on a repository with private modules failed on blocked github.com
// fetches) and every fix run from compiling the project from scratch (one cold
// build outlived the fix agent's tool timeout and cost it four minutes). From
// here the stage owns the volume and removes it on every path out of run().
//
// A MISSING volume is the fallback: an implement run from an older runner, or one
// whose volume something else collected. It is created empty and the first fix
// run vendors into it, exactly as before. The two collaborators are injected so
// that handover can be exercised without a Docker daemon.
func adoptCacheVolume(ctx commandUtils.TaskJobContext, exists func(name string) bool, create func(name string) error, logsWriter io.Writer) (string, bool) {
	name := cacheVolumeName(ctx)
	if exists(name) {
		io.WriteString(logsWriter, "Review stage: reusing the dependency cache the implement run left in place\n")
		return name, true
	}
	io.WriteString(logsWriter, "Review stage: the implement run's dependency cache is gone — a fix run will vendor again\n")
	if err := create(name); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not create the review stage's cache volume: %s\n", err))
	}
	return name, false
}

// reviewStage holds one Step's Review stage. A struct rather than a long
// parameter list: the loop, each round and the reporting all read the same
// half-dozen inputs.
type reviewStage struct {
	ctx        commandUtils.TaskJobContext
	parameters map[string]interface{}
	// workDirHost is the host directory bind-mounted at /work — the one the
	// repository checkouts live under, and the one the stage's sibling
	// directories are named after.
	workDirHost string
	// cacheVolume is the per-Step dependency cache the implement run left in
	// place, and the stage's to remove: releaseCacheVolume drops it once, on
	// every path out of run(). Empty in a test that never mounts one.
	cacheVolume string
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
	// thresholds are the HOLD thresholds (ReviewMustFixThresholds): a finding
	// at or above one is must-fix, and one still open at the end holds the
	// pull request.
	thresholds map[uint]uint
	// fixThresholds are the FIX thresholds (ReviewFixThresholds): a finding at
	// or above one is sent back to the implementer. NIL means the parameter
	// was absent — a Job from an older control plane — and the fix thresholds
	// are the hold thresholds, exactly today's behaviour. See fixThresholdsOrHold.
	fixThresholds map[uint]uint
	baseCommits   map[string]string
	// repoDirs is EVERY checked-out repository directory, including one with
	// no recorded start commit (a new, empty repository). The fix run's undo
	// copies all of them: a repository missing from the copy would keep a
	// failed fix's edits while the pull request says the change was put back.
	repoDirs     []string
	deadline     time.Time
	stopSignal   <-chan struct{}
	progressSink func(jobs.LiveProgressV1)
	// shadowEffort is the Job's ReviewShadowEffort parameter, validated, or
	// "" — a TEMPORARY measurement knob the control plane stamps for the orgs
	// being measured; see run_review_stage_shadow.go.
	shadowEffort string

	rounds      []reviewRoundOutput
	fixedInLoop []reviewFindingOutput
	// lastTurnCap is the MAX_TURNS the most recent review round was actually
	// spawned with, read back from its environment rather than assumed from
	// the constant, so the log reports what the container received.
	lastTurnCap string
	// vendored records that the per-Step dependency cache holds this Step's
	// dependencies — either because the implement run left its volume in
	// place, which is the ordinary case, or because a fix round refilled it.
	// See ensureVendoredCache: at most one vendor phase per stage, and none at
	// all when the implement run's cache survived.
	vendored bool
	// cacheReleased guards releaseCacheVolume, so the volume is removed once
	// however many times the stage's paths out overlap.
	cacheReleased bool
	// openSentBack carries the previous round's SENT-BACK findings — must-fix
	// or not — keyed by finding key, so the next round can be classified as
	// fixed / still open / new rather than as an undifferentiated list. Each
	// entry keeps the class the round that opened it gave it: MustFix is what
	// decides whether it holds the pull request.
	openSentBack map[string]reviewFindingOutput
	// seen carries EVERY finding key any earlier round reported, sent back or
	// not. openSentBack answers "was this one routed back"; this answers "have
	// we seen this at all", which is what makes a finding NEW.
	seen map[string]bool
	// fixError is why a fix run did not finish, or "". Set only on the path
	// that put the tree back, so it also says what the change on the branch
	// is: the one the implement run produced, not a half-applied fix.
	fixError string
	// fixNotAttempted is true when the undo copy could not be taken, so no
	// fix run happened at all — worded differently on the pull request.
	fixNotAttempted bool
	// stoppedNoChange is true when the loop ended because a fix run finished
	// and changed no file. Not a failure of anything: the fix run succeeded and
	// its summary says why it left the findings alone. Recorded so the pull
	// request can say that rather than leaving the findings looking untouched.
	stoppedNoChange bool
	// finalTreeReviewed tracks whether the latest completed review round
	// reviewed the tree as it now stands. ONE RULE, not a list of exits: set
	// when a review round completes, cleared when a fix run's work is kept; a
	// rolled-back fix or a no-change stop leaves it alone. Whatever it holds
	// when the loop ends is the answer — see reviewOutput.FinalTreeReviewed.
	finalTreeReviewed bool

	// pendingFixDiffs are the last KEPT fix run's own diffs, per repository
	// directory, waiting for the round that directly follows it. Taken (and
	// cleared) by stageFixDiffs before that round, so they are never reused.
	pendingFixDiffs map[string]string
	// roundFixDiffs is the REVIEW_FIX_DIFFS payload for the round being run,
	// or "" — set by stageFixDiffs, cleared by clearFixDiffs.
	roundFixDiffs string
	// fixTreesTmpRoot and fixTreesGit override where the fix-tree snapshots
	// keep their temporary directory and which git they run; "" means the
	// system default. Tests only.
	fixTreesTmpRoot string
	fixTreesGit     string

	// The two container runs, the two halves of the fix run's undo and the
	// cache volume's removal, all injectable so the loop's own decisions can be
	// tested without a Docker daemon and without root. Nil means the real
	// thing. runShadow is the shadow review's run, handed the environment it
	// would be spawned with.
	runReview   func(round int) (agentResult, error)
	runFix      func(mustFix []reviewFindingOutput) error
	runShadow   func(env []string) (agentResult, error)
	copyTree    copyTreeFunc
	restoreDir  restoreDirFunc
	removeCache func(name string)
}

// releaseCacheVolume removes the Step's dependency cache. Deferred at the top
// of run(), so it happens EXACTLY ONCE on every path out of the stage — a
// finished loop, a failed or skipped review, a user stop, and a fix round that
// was rolled back — and never while a later run in the same Step still needs
// what is on it.
func (s *reviewStage) releaseCacheVolume() {
	if s.cacheVolume == "" || s.cacheReleased {
		return
	}
	s.cacheReleased = true
	remove := s.removeCache
	if remove == nil {
		remove = removeCacheVolume
	}
	remove(s.cacheVolume)
}

func (s *reviewStage) reviewRound(round int) (agentResult, error) {
	if s.runReview != nil {
		return s.runReview(round)
	}
	return s.runReviewRound(round)
}

func (s *reviewStage) fixRound(mustFix []reviewFindingOutput) error {
	if s.runFix != nil {
		return s.runFix(mustFix)
	}
	return s.runMustFixRound(mustFix)
}

// run is the loop. It returns nil for the Step in all but two cases: a review
// failure is recorded, never fatal, and a fix run that did not finish is undone
// and handed to a human rather than failing anything.
//
// The two exceptions are a user stop, which is not a review failure at all and
// must reach the outer loop's stop path, and a restore that did not work — see
// attemptFix.
func (s *reviewStage) run() (map[string]interface{}, error) {
	// The Step's dependency cache is the stage's to remove, however the stage
	// ends — see releaseCacheVolume.
	defer s.releaseCacheVolume()
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
	s.openSentBack = map[string]reviewFindingOutput{}
	round := 1
	mustFixRounds := 0
	for {
		if !s.canAfford(reviewRunTimeout) {
			io.WriteString(s.logsWriter, "Review stage: not enough of the stage budget left for another review round — stopping here\n")
			break
		}
		s.reportStage(tasks.StageReview)
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d: reviewing the change against the Task's spec\n", round))
		s.stageFixDiffs(round)
		result, err := s.reviewRound(round)
		s.clearFixDiffs()
		if errors.Is(err, types.ErrJobStoppedByUser) {
			// A user stop is not a failed review. Attribute what the round
			// spent and hand the stop to the outer loop's existing path.
			s.accumulateReviewRun(round, result)
			return s.parameters, err
		}
		s.accumulateReviewRun(round, result)
		if reason := reviewRoundFailure(result, err); reason != "" {
			io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d did not complete: %s — continuing without it\n", round, reason))
			s.recordFailedRound(round, reason, result)
			break
		}
		findings := s.classify(result)
		s.recordRound(round, findings, result)
		s.finalTreeReviewed = true
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d completed: %d finding(s), %d agent turn(s) reported (cap: %s model responses)\n", round, len(findings), result.Turns, s.lastTurnCap))
		if round == 1 {
			// Before anything moves the tree or the loop's state, so the shadow
			// review sees exactly what round 1 saw.
			if err := s.shadowReview(result); err != nil {
				return s.parameters, err
			}
		}
		// Nothing to route back — including every advisory review, whose
		// findings are annotations by definition (see classify).
		sentBack := sentBackOnly(findings)
		if len(sentBack) == 0 {
			s.rememberOpenSentBack(nil)
			break
		}
		s.rememberOpenSentBack(sentBack)
		if mustFixRounds >= maxMustFixRounds {
			io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: %d sent-back finding(s) still open after %d fix round(s) — handing the evidence to a human on the pull request\n", len(sentBack), mustFixRounds))
			break
		}
		if !s.canAfford(mustFixRunTimeout + reviewRunTimeout) {
			io.WriteString(s.logsWriter, "Review stage: not enough of the stage budget left for a fix round and the review that would follow it — handing the evidence to a human on the pull request\n")
			break
		}
		s.reportStage(tasks.StageImplement)
		ran, err := s.attemptFix(round, sentBack)
		if err != nil {
			// A user stop, or a restore that did not work. Nothing else here
			// fails the Step.
			return s.parameters, err
		}
		if !ran {
			// The fix did not run, or ran and was undone. The findings this
			// round opened are still open and go to a human.
			break
		}
		// The fix's work is kept: the tree is no longer the one the last
		// completed round reviewed, until another round completes.
		s.finalTreeReviewed = false
		mustFixRounds++
		round++
	}
	return s.finish()
}

// accumulateReviewRun folds a review round's usage and cost into the Step's
// totals, and keeps the round's BLOCKED HOSTS out of them.
//
// A fix run's blocked hosts are the Step's: the implementer tried to reach
// something the Task's allowlist does not cover, and the pull request's
// "Network: blocked hosts during this Step" section exists to tell the user to
// add it. A REVIEW run's are not. The reviewer is a different agent, spawned by
// this stage for its own purposes, and its startup calls are its own — a Codex
// reviewer's github.com, api.github.com and chatgpt.com turned up on a pull
// request as advice to widen the implementer's allowlist for hosts the
// implementer never asked for.
//
// So they go to the job log instead, where whoever is debugging the reviewer can
// find them, and nowhere near the Step's record.
func (s *reviewStage) accumulateReviewRun(round int, result agentResult) {
	if len(result.DeniedHosts) > 0 {
		io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d: the reviewer's requests to these hosts were blocked: %s\n",
			round, strings.Join(result.DeniedHosts, ", ")))
	}
	result.DeniedHosts = nil
	_ = accumulateReviewRunUsage(s.parameters, s.reviewerView(), result)
}

// attemptFix takes the undo copy, runs the fix, and decides what its outcome
// means for the loop. It is the whole of "a bounded cleanup that did not finish
// never costs the Step its implementation".
//
//	(true, nil)  the fix ran, changed something, and the loop carries on to the
//	             next review round.
//	(false, nil) nothing more is worth reviewing: the fix could not be undone so
//	             it was never started, or it failed and the tree has been put
//	             back, or it finished and changed no file. In all three the
//	             round's must-fix findings are still open and the existing
//	             handoff renders them on a draft pull request.
//	(_, err)     the Step fails. A user stop, which is not a failed fix at all
//	             and belongs to the outer loop's stop path; or a restore that
//	             did not work, which leaves a tree nobody can describe.
func (s *reviewStage) attemptFix(round int, mustFix []reviewFindingOutput) (bool, error) {
	// Only a kept fix leaves diffs for the next round; nothing from an
	// earlier one may survive into it.
	s.pendingFixDiffs = nil
	// The copy can take a while on a large checkout and cannot be interrupted,
	// so a stop is honoured on both sides of it rather than after a fix run
	// that was never going to be wanted.
	if s.stopRequested() {
		return false, types.ErrJobStoppedByUser
	}
	snapshot, err := s.takeFixRoundSnapshot(round)
	if err != nil {
		// NEVER run a fix that cannot be undone. A fix run edits a tree that
		// already passed its verify; without the copy, a fix that then failed
		// would leave it half-edited with nothing to go back to. The detail
		// (host paths included) goes to the job log only; the pull request
		// gets the plain statement.
		s.fixNotAttempted = true
		s.fixError = "the change could not be copied first, so a failed fix could not have been undone"
		io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: no fix was attempted — the change could not be copied first: %s — handing the evidence to a human on the pull request\n", err))
		return false, nil
	}
	if s.stopRequested() {
		snapshot.remove()
		return false, types.ErrJobStoppedByUser
	}
	// The fix run's own diff, for the round after it: trees before and after,
	// held in a temporary directory that goes on every path out of here.
	trees := s.takeFixTrees()
	defer trees.remove()
	fixErr := s.fixRound(mustFix)
	if fixErr == nil {
		// THE UNDO COPY IS ALSO THE ANSWER TO "DID THIS RUN CHANGE ANYTHING",
		// so the comparison happens before the copy is dropped.
		changed, err := snapshot.treeChanged()
		snapshot.remove()
		if err != nil {
			// Fail-open, like the rest of the stage: a comparison nobody could
			// make must not end the loop on a guess. Reviewed again as before.
			io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not tell whether fix round %d changed anything (%s) — reviewing it as usual\n", round, err))
			s.pendingFixDiffs = trees.fixDiffs()
			return true, nil
		}
		if !changed {
			// A fix run may decline a finding it judges wrong or contrary to the
			// Step, and say why in its summary. When it changes NO file, another
			// review round can only report the same findings again — the reviewer
			// never sees that explanation, by design — at the cost of up to two
			// more rounds and another fix run. So the loop stops here and the
			// findings go to a human with the explanation the fix run wrote,
			// which recordFixRunResult has already merged into the description.
			s.stoppedNoChange = true
			io.WriteString(s.logsWriter, fmt.Sprintf("Fix round %d changed no file — the findings it was sent stay open; handing them to a human on the pull request\n", round))
			return false, nil
		}
		s.pendingFixDiffs = trees.fixDiffs()
		return true, nil
	}
	if errors.Is(fixErr, types.ErrJobStoppedByUser) {
		// Not a failed fix: the user stopped the Job and the container was
		// SIGTERMed. Returned exactly as before, with the tree left as it is —
		// the deferred sweep collects the copy.
		return false, fixErr
	}
	io.WriteString(s.logsWriter, fmt.Sprintf("Fix round %d did not complete: %s — putting the change back to what it was before it ran\n", round, fixErr))
	if err := snapshot.restore(); err != nil {
		// THE ONE FIX-RELATED PATH THAT FAILS THE STEP. A tree that is neither
		// the implementation nor the fix cannot be committed, and cannot be
		// described on a pull request either.
		io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: the change could NOT be put back after the failed fix run: %s\n", err))
		return false, fmt.Errorf("error restoring the change after fix round %d failed (%s): %s", round, fixErr, err)
	}
	s.fixError = fixErr.Error()
	io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: the change is back to exactly what it was before fix round %d — %d sent-back finding(s) go to a human on the pull request\n", round, len(mustFix)))
	return false, nil
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
		Participation:     participationName(s.participation),
		Rounds:            s.rounds,
		FixedInLoop:       s.fixedInLoop,
		MustFixOpen:       s.mustFixOpen(),
		FixError:          s.fixError,
		FixNotAttempted:   s.fixNotAttempted,
		StoppedNoChange:   s.stoppedNoChange,
		FinalTreeReviewed: s.finalTreeReviewed,
	}
	s.logFullReview(out)
	if err := mergeReviewIntoJobOutput(s.parameters, out); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not record the review on the job output: %s\n", err))
	}
	s.reportReviewResult(out)
	return s.parameters, nil
}

// classify turns one round's raw findings into the Step's record: each finding
// marked sent back or not and must-fix or not, and the previous round's open
// sent-back findings settled against the reviewer's own status for each of
// them.
//
// TWO THRESHOLDS, TWO QUESTIONS. SentBack — at or above the parameter's FIX
// threshold — routes the finding back to the implementer for a fix round.
// MustFix — at or above the HOLD threshold (isMustFix) — makes it hold the
// pull request if it is still open when the loop ends. A must-fix finding is
// always sent back too.
//
// WHAT DECIDES "FIXED" IS THE REVIEWER'S ANSWER, NOT A MISSING KEY. A round
// that was sent the previous round's open must-fix findings (see
// openSentBackEnvValue) answers with review_result.previous: one entry per key
// it was given, resolved or still_present. A finding is fixed in loop only when
// its entry says resolved AND this round did not report that key again.
// Everything else is HELD — still sent back, carried into the next round, and
// rendered as still present, in the class the round that opened it gave it.
//
// Key matching alone used to make that decision, and the key is the reviewer's
// own free-text slug: a reworded key made an unfixed problem look fixed and its
// re-report look new. A live run listed a Critical unauthenticated secret dump
// under "Fixed during review" on a NON-DRAFT pull request while the code still
// had it. So there is deliberately NO FALLBACK to key matching when previous is
// absent — agentbox omits it both when the reviewer left the list out and when
// every entry it returned was unusable, and a fallback would reproduce exactly
// that bug. The cost of an image that never fills it in is that open findings
// are held and the pull request opens as a draft, which is the safe direction.
//
// Rounds that were sent nothing — round 1, and any round following one that
// left nothing open — have nothing to settle either way.
//
// Pairing is by findingKey: the producer's own key when it supplied one,
// otherwise a synthesised one, capped either way at agentbox's key length so
// the key the runner sends is the key it is answered about.
func (s *reviewStage) classify(result agentResult) []reviewFindingOutput {
	if result.ReviewResult == nil {
		// Unreachable on the live path — reviewRoundFailure ends the loop for
		// a round with no review_result — but classify must not be the thing
		// that turns a missing report into a crashed Step.
		return nil
	}
	findings := make([]reviewFindingOutput, 0, len(result.ReviewResult.Findings))
	// indexByKey is where each key the round ITSELF reported sits in findings,
	// so a held finding can mark that entry instead of being appended beside it.
	indexByKey := map[string]int{}
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
		// Must-fix implies sent back: nothing may hold the pull request that
		// was never given a fix round.
		out.SentBack = out.MustFix || (s.participation == participationOn && s.isSentBack(f))
		key := findingKey(out)
		// Nothing is "new" in the first round — everything is. The flag only
		// means something once there is a round to be new since.
		out.New = !firstRound && !s.seen[key]
		findings = append(findings, out)
		if _, reported := indexByKey[key]; !reported {
			indexByKey[key] = len(findings) - 1
		}
	}
	for key := range indexByKey {
		s.seen[key] = true
	}
	// An empty openSentBack is exactly "this round was sent no open findings":
	// round 1 starts with the map empty, and a round that left nothing open
	// empties it again. Nothing to resolve, nothing to hold.
	if len(s.openSentBack) == 0 {
		return findings
	}
	return s.settleOpenSentBack(findings, indexByKey, result.ReviewResult.Previous)
}

// reviewStatusResolved is the one status that clears an open must-fix finding.
// Every other value agentbox can return — still_present, an unrecognised
// string, an absent entry — leaves it held.
const reviewStatusResolved = "resolved"

// reviewStatusStillPresent is the reviewer's explicit "not fixed". Only its
// note is recorded: a note that came with "resolved" describes the fix.
const reviewStatusStillPresent = "still_present"

// settleOpenSentBack records each open sent-back finding as fixed in loop or
// held, from the statuses the round returned. Held findings go into the round's
// own finding list, which is what routes them back to the implementer and, at
// the loop's bounds, onto a draft pull request.
func (s *reviewStage) settleOpenSentBack(findings []reviewFindingOutput, indexByKey map[string]int, previous []reviewPreviousFinding) []reviewFindingOutput {
	statuses := make(map[string]reviewPreviousFinding, len(previous))
	for _, p := range previous {
		if key := strings.TrimSpace(p.Key); key != "" {
			statuses[key] = p
		}
	}
	for _, key := range sortedFindingKeys(s.openSentBack) {
		open := s.openSentBack[key]
		status := statuses[key]
		i, reReported := indexByKey[key]
		// Re-reported under the same key is the reviewer contradicting its own
		// status line, and the finding is the stronger evidence of the two.
		if !reReported && strings.EqualFold(strings.TrimSpace(status.Status), reviewStatusResolved) {
			// Held is cleared on the way in. A finding an EARLIER round held was
			// stored back into openSentBack still marked held, and carrying that
			// flag into FixedInLoop would render it under "Fixed during review"
			// with "still present after a fix round" underneath it — the two
			// statements this change exists to keep apart, in one entry.
			open.Held = false
			open.StillPresentNote = ""
			s.fixedInLoop = append(s.fixedInLoop, open)
			continue
		}
		if reReported {
			// Marked in place rather than copied: one problem the reader sees
			// once and the implementer is sent once. It keeps the CLASS the
			// round that opened it assigned, whatever severity it was
			// re-reported at: a reviewer downgrading a must-fix Medium to Low
			// does not release it, and a sent-back Low re-reported higher never
			// starts holding the pull request.
			//
			// A re-report under a DIFFERENT key is not this case and is left
			// alone: it stands as its own finding beside the held one, because
			// merging two reports that may or may not be one problem is a
			// guess, and the loop already has one bug from guessing.
			findings[i].SentBack = true
			findings[i].MustFix = open.MustFix
			findings[i].Held = true
			findings[i].StillPresentNote = stillPresentNote(status)
			continue
		}
		// Not mentioned at all: carried as the round that opened it reported
		// it, and NOT new — it has been on the record since that round.
		held := open
		held.SentBack = true
		held.Held = true
		held.New = false
		held.StillPresentNote = stillPresentNote(status)
		findings = append(findings, held)
	}
	return findings
}

// stillPresentNote is this round's note for a held finding: the reviewer's
// own sentence when it said "still_present", and nothing otherwise. It
// REPLACES the previous round's note rather than adding to it; a finding the
// reviewer did not mention this round has no current note, and an older one
// would describe code the last fix round may have changed.
func stillPresentNote(status reviewPreviousFinding) string {
	if !strings.EqualFold(strings.TrimSpace(status.Status), reviewStatusStillPresent) {
		return ""
	}
	return strings.TrimSpace(status.Note)
}

// sortedFindingKeys orders the open sent-back findings' keys, so a held finding
// lands in the same place in the record on every run.
func sortedFindingKeys(open map[string]reviewFindingOutput) []string {
	keys := make([]string, 0, len(open))
	for key := range open {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
	return meetsThreshold(f, s.thresholds)
}

// isSentBack is the FIX gate: the same comparison as isMustFix, against the
// fix thresholds.
func (s *reviewStage) isSentBack(f reviewFinding) bool {
	return meetsThreshold(f, s.fixThresholdsOrHold())
}

// fixThresholdsOrHold is the fix thresholds, or the hold thresholds when the
// Job carries none — which makes a Job from an older control plane send back
// exactly what it holds, as before the split.
func (s *reviewStage) fixThresholdsOrHold() map[uint]uint {
	if s.fixThresholds == nil {
		return s.thresholds
	}
	return s.fixThresholds
}

// meetsThreshold maps a finding's parameter and severity through
// deployment-runner-kit's wire mirror and compares them with one set of
// thresholds. Unparseable findings and absent or zero thresholds never meet it.
func meetsThreshold(f reviewFinding, thresholds map[uint]uint) bool {
	parameter, ok := tasks.ReviewParameterValue(f.Parameter)
	if !ok {
		return false
	}
	severity, ok := tasks.ReviewSeverityValue(f.Severity)
	if !ok {
		return false
	}
	threshold, ok := thresholds[parameter]
	if !ok || threshold == 0 {
		return false
	}
	return severity >= threshold
}

func (s *reviewStage) rememberOpenSentBack(findings []reviewFindingOutput) {
	s.openSentBack = map[string]reviewFindingOutput{}
	for _, f := range findings {
		s.openSentBack[findingKey(f)] = f
	}
}

// mustFixOpen is MustFixOpen: whether any finding still open at the end is
// at or above the HOLD threshold. A sent-back finding below it that survived
// the loop is noted on the pull request and holds nothing.
func (s *reviewStage) mustFixOpen() bool {
	for _, f := range s.openSentBack {
		if f.MustFix {
			return true
		}
	}
	return false
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
	// round never opens a sent-back finding of its own.
	s.rememberOpenSentBack(nil)
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
			b.WriteString(fmt.Sprintf("  [%s] %s/%s at %s — %s\n", findingLogMarker(f, round.Round), f.Parameter, f.Severity, f.Location, f.What))
			if f.Why != "" {
				b.WriteString(fmt.Sprintf("      why: %s\n", f.Why))
			}
			if f.StillPresentNote != "" {
				b.WriteString(fmt.Sprintf("      still present: %s\n", f.StillPresentNote))
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
	if out.FixError != "" {
		b.WriteString(fmt.Sprintf("A fix attempt did not complete: %s — the change is exactly as it was before that attempt\n", out.FixError))
	}
	b.WriteString(fmt.Sprintf("Must-fix findings still open: %t\n", out.MustFixOpen))
	io.WriteString(s.logsWriter, b.String())
}

// findingLogMarker is how one finding reads in the job log.
//
// A HELD finding is marked still present and nothing else. That is the
// reviewer's own verdict on a finding it was asked about, which is a different
// and stronger statement than "still open" — that only ever meant the key
// turned up again — and it must never read as new.
//
// A finding sent back for a fix round that does not hold the pull request is
// marked SENT BACK rather than MUST FIX, so the log says which findings can
// make the pull request a draft.
func findingLogMarker(f reviewFindingOutput, round int) string {
	marker := "annotated"
	switch {
	case f.MustFix:
		marker = "MUST FIX"
	case f.SentBack:
		marker = "SENT BACK"
	}
	if f.Held {
		if !f.SentBack {
			// A record from before the split: held always meant must-fix.
			marker = "MUST FIX"
		}
		return marker + ", still present"
	}
	switch {
	case f.New:
		return marker + ", new since the last round"
	case round > 1:
		return marker + ", still open"
	}
	return marker
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
	// A REVIEW THAT EXAMINED NOTHING IS A FAILED ROUND, NOT A CLEAN ONE. An
	// agent that ran and reported no checked parameter found no findings
	// because it looked at nothing — and recorded as a completed round with an
	// empty finding list, that is indistinguishable on the pull request from a
	// change a reviewer read and passed. (It happened: a reviewer whose sandbox
	// could not start failed every command it ran, skipped every pass, and the
	// pull request said it had been reviewed.)
	//
	// Turns > 0 is what separates "ran and saw nothing" from "never ran": a
	// round agentbox's cost gate declined to spend an agent on — a
	// documentation-only change, say — reports zero turns and no checked
	// coverage, and is exactly as clean as it was before.
	//
	// Only when it also reported NO findings. The coverage state is free text
	// from the agent; a round that found something but labelled its coverage
	// differently did examine the change, and failing it would throw its
	// findings away — must-fix ones included.
	if result.Turns > 0 && len(result.ReviewResult.Findings) == 0 && !anyCoverageChecked(result.ReviewResult.Coverage) {
		return "the reviewer ran but could not examine the change: " + firstSkippedCoverageReason(result.ReviewResult.Coverage)
	}
	return ""
}

// anyCoverageChecked reports whether the round examined ANY parameter.
func anyCoverageChecked(coverage []reviewCoverage) bool {
	for _, c := range coverage {
		if coverageChecked(c.State) {
			return true
		}
	}
	return false
}

// coverageChecked is the one reading of a coverage state, applied wherever the
// question "was this parameter actually examined" is asked. Case-insensitive:
// the state is a free string from an agent, and a round reported as "Checked"
// examined the change exactly as much as one reported as "checked".
func coverageChecked(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "checked")
}

// firstSkippedCoverageReason is the reviewer's own account of why it examined
// nothing — the reason on the first parameter it did not check, which is what
// names the cause (a sandbox that could not start, a diff it could not read)
// instead of restating that nothing was checked.
func firstSkippedCoverageReason(coverage []reviewCoverage) string {
	for _, c := range coverage {
		if coverageChecked(c.State) {
			continue
		}
		if reason := strings.TrimSpace(c.Reason); reason != "" {
			return reason
		}
	}
	return "it reported no coverage and gave no reason"
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

// sentBackOnly is the findings routed back to the implementer: every finding
// at or above its fix threshold, must-fix or not.
func sentBackOnly(findings []reviewFindingOutput) []reviewFindingOutput {
	var out []reviewFindingOutput
	for _, f := range findings {
		if f.SentBack || f.MustFix {
			out = append(out, f)
		}
	}
	return out
}

// reviewFindingKeyMaxRunes is agentbox's cap on a finding key — the one it
// applies to the keys it is given and to the keys it echoes back in
// review_result.previous (agentbox's docs/CONTRACT.md).
//
// findingKey never exceeds it, so THE KEY THE RUNNER SENDS IS THE KEY IT IS
// ANSWERED ABOUT. A longer key would come back cut, match nothing in
// openSentBack, and the finding under it would be held for the rest of the loop
// however thoroughly it was fixed.
const reviewFindingKeyMaxRunes = 120

// findingKey is how the same finding is recognised across rounds, and the key
// the reviewer is asked about. The producer's own key when it supplied one;
// otherwise parameter, location and the first 80 runes of what — specific
// enough to distinguish two problems in one file, loose enough that a reworded
// report of the same problem still pairs.
func findingKey(f reviewFindingOutput) string {
	if key := strings.TrimSpace(f.Key); key != "" {
		return capKeyRunes(key)
	}
	what := []rune(strings.TrimSpace(f.What))
	if len(what) > 80 {
		what = what[:80]
	}
	return capKeyRunes(strings.ToLower(strings.TrimSpace(f.Parameter) + "|" + strings.TrimSpace(f.Location) + "|" + string(what)))
}

// capKeyRunes truncates a key to the cap. Plain truncation with no ellipsis,
// because the result has to be byte-identical to agentbox's own cut of the same
// string — a marker character would make the two disagree.
func capKeyRunes(key string) string {
	runes := []rune(key)
	if len(runes) <= reviewFindingKeyMaxRunes {
		return key
	}
	return string(runes[:reviewFindingKeyMaxRunes])
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

// decodeFixThresholds reads the resolved FIX policy, in the same shape as
// decodeMustFixThresholds, e.g. {"1":2,"2":2}.
//
// NIL means "no fix thresholds on this Job": the parameter is absent (a Job
// created by an older control plane) or unreadable. The stage then sends back
// exactly what it holds (see fixThresholdsOrHold), which is today's behaviour
// — never more than the org asked to hold on, never less. A present, readable
// empty object is NOT nil: it sends nothing back beyond what must be fixed.
func decodeFixThresholds(parameters map[string]interface{}, logsWriter io.Writer) map[uint]uint {
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.ReviewFixThresholds)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	decoded := map[string]uint{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not read the review fix thresholds (%s) — findings are sent back at the must-fix thresholds\n", err))
		return nil
	}
	out := map[uint]uint{}
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

// stopRequested reports, without blocking, whether the user has stopped the
// Job. The fix run's own spawn honours the stop signal while it waits; this
// covers the host-side work around it (the undo copy).
func (s *reviewStage) stopRequested() bool {
	if s.stopSignal == nil {
		return false
	}
	select {
	case <-s.stopSignal:
		return true
	default:
		return false
	}
}

// readRepositoryDirsFromJobOutput returns every checked-out repository
// directory recorded in the Job output, whether or not a start commit was
// recorded for it. readBaseCommitsFromJobOutput keeps only the ones with a
// commit — the review needs a baseline to diff against — but the fix run's
// undo has to cover every directory a fix run could have edited.
func readRepositoryDirsFromJobOutput(parameters map[string]interface{}) []string {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var dirs []string
	for _, bc := range data.BaseCommits {
		if bc.Dir == "" || seen[bc.Dir] {
			continue
		}
		seen[bc.Dir] = true
		dirs = append(dirs, bc.Dir)
	}
	sort.Strings(dirs)
	return dirs
}
