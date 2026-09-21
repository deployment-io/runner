package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// runReviewRound runs one review container and returns what it reported.
//
// One container per round, from the SAME agentbox image, the same /work bind
// mount and the same per-Step cache volume as the implement run, spawned
// through the same agentboxSpawnSpec and spawnAgentboxAndWait. No additional
// mounts: the only thing that differs is the environment and the fact that the
// implementer's output directory is not in the work dir while it runs.
//
// RESULT_PATH is unchanged — still /work/.agentbox-output/result.json. It
// resolves to the round's own fresh directory by virtue of the rename, which
// is what lets the live-progress poller keep reading its existing path and the
// dashboard's counters keep moving through the Review stage.
func (s *reviewStage) runReviewRound(round int) (agentResult, error) {
	imageRef, err := jobs.GetParameterValue[string](s.parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return agentResult{}, fmt.Errorf("agentbox image missing: %s", err)
	}
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(s.ctx.OrganizationID, s.ctx.TaskID)
	env, err := s.reviewSpawnEnv(round)
	if err != nil {
		return agentResult{}, err
	}
	swap, err := swapInReviewOutputDir(workDirHost, round, prepareAgentboxHostDirs)
	if err != nil {
		return agentResult{}, fmt.Errorf("error preparing the review output directory: %s", err)
	}
	// DEFERRED so a failed, timed-out or cancelled round cannot leave the
	// implementer's output displaced. Everything after this point — the spawn,
	// the wait, the result read — happens with the implementer's directory
	// parked outside the work dir, and it comes back on every path out.
	defer swap.restore(s.logsWriter)

	impl := &RunAgentStep{stopSignal: s.stopSignal, progressSink: s.progressSink}
	return impl.spawnAgentboxAndWait(agentboxSpawnSpec{
		imageRef:    imageRef,
		workDirHost: workDirHost,
		cacheVolume: cacheVolumeName(s.ctx),
		env:         env,
		waitTimeout: reviewRunTimeout,
	}, s.logsWriter)
}

// reviewSpawnEnv builds the review container's environment: the implement
// run's credentials and agent selection, plus the four REVIEW_* inputs.
//
// It deliberately carries NEITHER STEP_PROMPT NOR PREVIOUS_STEPS_SUMMARY. The
// review's work item is the diff, and handing it the implementer's instruction
// would invite it to grade the work against what the implementer was told to
// do rather than against what the change actually does — and to keep going
// where the implementer left off.
func (s *reviewStage) reviewSpawnEnv(round int) ([]string, error) {
	env, err := buildAgentSpawnEnvVars(s.parameters, s.logsWriter)
	if err != nil {
		return nil, err
	}
	baseCommits, err := json.Marshal(s.baseCommits)
	if err != nil {
		return nil, fmt.Errorf("error encoding the review base commits: %s", err)
	}
	spec, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.ReviewSpec)
	return applyReviewEnv(env, reviewEnvInputs{
		spec:        spec,
		passes:      reviewPasses,
		baseCommits: string(baseCommits),
		round:       round,
	}), nil
}

// reviewPasses is the pass set this release runs, matching the two parameters
// the platform default policy gates on. A pass with no gate would be a report
// nobody acts on; a gate with no pass would be a threshold on evidence nothing
// generates.
const reviewPasses = "security,correctness"

type reviewEnvInputs struct {
	spec        string
	passes      string
	baseCommits string
	round       int
}

// applyReviewEnv turns an implement-run environment into a review-run one:
// AGENT_MODE=review, the four REVIEW_* inputs, the review's own turn cap, and
// the two implementer keys REMOVED.
//
// Removal rather than absence: the environment is built by the shared spawn
// helper, which populates STEP_PROMPT and PREVIOUS_STEPS_SUMMARY from the
// Job's parameters. Filtering them out here is what makes "the review never
// sees the implementer's prompt" a property of this function rather than of
// remembering not to add them.
func applyReviewEnv(env []string, in reviewEnvInputs) []string {
	out := make([]string, 0, len(env)+6)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "STEP_PROMPT", "PREVIOUS_STEPS_SUMMARY", "AGENT_MODE", "MAX_TURNS":
			continue
		case agentMCPSocketEnvVar:
			// Review needs no runner tools, and the review spawn mounts no
			// socket — so the var would name a path that is not there. Dropped
			// here as well, so the two facts cannot drift apart.
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"AGENT_MODE=review",
		"REVIEW_PASSES="+in.passes,
		"REVIEW_BASE_COMMITS="+in.baseCommits,
		"REVIEW_ROUND="+strconv.Itoa(in.round),
		"MAX_TURNS="+strconv.Itoa(reviewRunMaxTurns),
	)
	if strings.TrimSpace(in.spec) != "" {
		out = append(out, "REVIEW_SPEC="+in.spec)
	}
	return out
}

// reviewOutputSwap is the BLIND-TO BOUNDARY, enforced by a host-side rename.
//
// A review must not read what the implement run wrote — not its result.json,
// not its progress.json, not its message records. The enforcement is a rename
// rather than mount layering so it depends on NO Docker daemon behaviour and
// can be tested without a daemon.
//
// The stash sits OUTSIDE the work dir, a sibling of it, following the
// agentMCPSocketHostPath precedent. NOT a dot-directory inside: everything
// under the work dir is visible to the container at /work, and a dot-prefix
// only hides a path from agentbox's repo discovery and the commit diff — not
// from an agent that looks.
//
// This is safe because RunAgentStep has already read the implementer's
// result.json into memory and merged it into JobOutput before this stage
// began. Nothing downstream re-reads that file.
type reviewOutputSwap struct {
	workDirHost string
	round       int
	// displaced records whether there was an implementer directory to move.
	// There always is in practice; a round that found none must not invent
	// one on the way back.
	displaced bool
	restored  bool
}

// implementerOutputStashPath is where the implementer's .agentbox-output waits
// out a review round — a sibling of the work dir, so it is not visible at
// /work.
func implementerOutputStashPath(workDirHost string) string {
	return strings.TrimRight(workDirHost, "/") + "-implement-output"
}

// reviewRoundOutputPath is where a round's own output is parked while it is
// copied into the job log, also a sibling of the work dir.
//
// It is DELETED once the copy is made. The job log is the durable record —
// leaving the directory behind put one host directory per round per Step on a
// volume nothing ever swept, and a Step with three rounds left four.
func reviewRoundOutputPath(workDirHost string, round int) string {
	return fmt.Sprintf("%s-review-round-%d-output", strings.TrimRight(workDirHost, "/"), round)
}

// reviewRoundResultLogMaxBytes bounds how much of a round's result.json goes
// into the job log. The findings and coverage are written separately and in
// full by logFullReview; this copy is for the fields that never reach the
// review block — the status, the error, the agent's own prose.
const reviewRoundResultLogMaxBytes = 16000

// logRoundOutput copies the round's result.json into the job log before the
// round directory is removed, so nothing that only lived on disk is lost with
// it. Best-effort throughout: a round that wrote no result is the case the
// caller is already handling.
func logRoundOutput(dir string, round int, logsWriter io.Writer) {
	data, err := os.ReadFile(filepath.Join(dir, agentboxResultFile))
	if err != nil || len(data) == 0 {
		return
	}
	if len(data) > reviewRoundResultLogMaxBytes {
		data = append(data[:reviewRoundResultLogMaxBytes], []byte("\n[… truncated]")...)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Review round %d result.json:\n%s\n", round, data))
}

// cleanupReviewStageSiblings removes every host directory the stage parked
// beside the work dir: the implementer's stash and any round directory a
// failed restore left behind.
//
// Deferred at the top of the stage so it runs however the stage ends. restore
// already removes each round directory on the ordinary path; this is the
// backstop for the path where it could not, which is exactly the path that
// would otherwise leak silently.
func cleanupReviewStageSiblings(workDirHost string) {
	_ = os.RemoveAll(implementerOutputStashPath(workDirHost))
	matches, err := filepath.Glob(strings.TrimRight(workDirHost, "/") + "-review-round-*-output")
	if err != nil {
		return
	}
	for _, dir := range matches {
		_ = os.RemoveAll(dir)
	}
}

// prepareOutputDirFunc creates the round's fresh output directory and gives it
// to the agentbox user.
//
// Injected rather than called directly so the rename half — which is the part
// with the interesting failure modes — can be tested without root. The real
// preparer chowns, and a chown to UID 1000 fails for an unprivileged test
// process, which would have made every test of this logic a root-only test.
// Ownership itself is still covered, by one euid-gated case.
type prepareOutputDirFunc func(workDirHost string) error

// swapInReviewOutputDir moves the implementer's output out of the work dir and
// puts a fresh, correctly-owned directory in its place.
func swapInReviewOutputDir(workDirHost string, round int, prepare prepareOutputDirFunc) (*reviewOutputSwap, error) {
	if prepare == nil {
		prepare = prepareAgentboxHostDirs
	}
	swap := &reviewOutputSwap{workDirHost: workDirHost, round: round}
	implementerDir := filepath.Join(workDirHost, agentboxResultDirRel)
	stash := implementerOutputStashPath(workDirHost)
	// A stash left behind by an earlier round (or an earlier, interrupted
	// Job on the same work dir) would make the rename fail; it is also
	// certainly stale, since the live directory is the one in the work dir.
	_ = os.RemoveAll(stash)
	if _, err := os.Stat(implementerDir); err == nil {
		if err := os.Rename(implementerDir, stash); err != nil {
			return nil, err
		}
		swap.displaced = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// Recreates .agentbox-output (empty) and chowns it, along with the tmp
	// and corepack dirs, to the agentbox user — the container runs as UID
	// 1000 and writes its result through the bind mount.
	if err := prepare(workDirHost); err != nil {
		// Put the implementer's output back before giving up: the caller's
		// deferred restore has not been registered yet.
		swap.restore(io.Discard)
		return nil, err
	}
	return swap, nil
}

// restore copies the round's output into the job log, removes it from the
// host, and puts the implementer's directory back.
//
// Idempotent, and best-effort about the round's own output: losing a round's
// log copy is a diagnostic cost, while failing to restore the implementer's
// directory would leave the Step's own record displaced. The round directory
// is REMOVED rather than kept — the job log is the durable record, and a
// directory per round per Step on a volume nothing sweeps is a leak.
func (s *reviewOutputSwap) restore(logsWriter io.Writer) {
	if s == nil || s.restored {
		return
	}
	s.restored = true
	implementerDir := filepath.Join(s.workDirHost, agentboxResultDirRel)
	roundDir := reviewRoundOutputPath(s.workDirHost, s.round)
	_ = os.RemoveAll(roundDir)
	if _, err := os.Stat(implementerDir); err == nil {
		// Park the round's output under its own name first, so the copy into
		// the log cannot race the implementer's directory coming back.
		if err := os.Rename(implementerDir, roundDir); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("warning: could not set aside review round %d's output: %s\n", s.round, err))
			_ = os.RemoveAll(implementerDir)
		} else {
			logRoundOutput(roundDir, s.round, logsWriter)
			_ = os.RemoveAll(roundDir)
		}
	}
	if !s.displaced {
		return
	}
	if err := os.Rename(implementerOutputStashPath(s.workDirHost), implementerDir); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not restore the implement run's output directory: %s\n", err))
	}
}

// runMustFixRound routes the open must-fix findings back to the implementer as
// an ORDINARY batch agentbox run — same image, same mode, same verify gate as
// any other implement run.
//
// Its prompt is the Step's original prompt plus a labelled block listing only
// the must-fix findings. Never the reviewer's transcript, and never a
// below-threshold finding: the implementer is being asked to fix named
// problems, and padding that with everything the reviewer noticed turns a
// bounded fix into a second open-ended Step.
func (s *reviewStage) runMustFixRound(mustFix []reviewFindingOutput) error {
	imageRef, err := jobs.GetParameterValue[string](s.parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return fmt.Errorf("agentbox image missing: %s", err)
	}
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(s.ctx.OrganizationID, s.ctx.TaskID)
	env, err := buildAgentSpawnEnvVars(s.parameters, s.logsWriter)
	if err != nil {
		return err
	}
	stepPrompt, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.StepPrompt)
	env = applyMustFixEnv(env, buildMustFixPrompt(stepPrompt, mustFix))
	// A fix run is an implement run, so it gets the implement run's tool
	// channel. Without it an agent asked to fix a finding in code that
	// deploys a preview loses the tools the original run used to do that work
	// — and fails, or silently does something else, for a reason that has
	// nothing to do with the finding.
	env = append(env, agentMCPSocketEnvVar+"="+agentboxMCPSocketInContainer)
	previewDeps := buildStaticSitePreviewDeps(s.ctx, s.parameters, workDirHost, s.logsWriter)

	// A fix run BUILDS, and it builds offline: the agent container has no
	// credentials and the proxy allows only the agent's own hosts. RunAgentStep
	// vendored the Step's dependencies into a per-Step volume and then removed
	// it on the way out, so the cache this run mounts is empty until it is
	// filled again. Without this the fix would fail its own verify on missing
	// dependencies and take the Step down with it — a Step failed by the
	// machinery rather than by the code.
	//
	// Once per stage: the second fix round reuses what the first vendored.
	if err := s.ensureVendoredCache(imageRef, workDirHost); err != nil {
		return err
	}

	io.WriteString(s.logsWriter, fmt.Sprintf("Routing %d must-fix finding(s) back to the implementer\n", len(mustFix)))
	impl := &RunAgentStep{stopSignal: s.stopSignal, progressSink: s.progressSink}
	result, err := impl.spawnAgentboxAndWait(agentboxSpawnSpec{
		imageRef:      imageRef,
		workDirHost:   workDirHost,
		cacheVolume:   cacheVolumeName(s.ctx),
		env:           env,
		mcpSocketHost: agentMCPSocketHostPath(workDirHost),
		previewDeps:   previewDeps,
		waitTimeout:   mustFixRunTimeout,
	}, s.logsWriter)
	// Attribute the fix run's work to the Step whether or not it succeeded,
	// then decide what its outcome means.
	mergeErr := mergeAgentResultIntoJobOutput(s.parameters, result)
	if err != nil {
		// Includes the user-stop sentinel, which the caller routes to the
		// existing stop path.
		return err
	}
	if mergeErr != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not merge the fix run's result: %s\n", mergeErr))
	}
	if result.Status != "success" {
		return formatAgentFailure(result)
	}
	// The SAME verify gate as the initial run. A fix that breaks the build
	// fails the Step before CommitAndPush, and a failure that predates the
	// run is still exempt.
	switch decideVerifyGate(result.VerifyResult) {
	case verifyGateFail:
		return formatVerifyFailure(result.VerifyResult)
	case verifyGateWarnPreExisting:
		io.WriteString(s.logsWriter, formatPreExistingVerifyWarning(result.VerifyResult))
	}
	return nil
}

// ensureVendoredCache re-populates the per-Step dependency cache before the
// first fix run, using the same vendor phase RunAgentStep runs.
//
// The volume carries the Step's own name, so it is the same shelf the
// implement run built against — and it is created here rather than assumed,
// because RunAgentStep removes it when it returns.
func (s *reviewStage) ensureVendoredCache(imageRef, workDirHost string) error {
	if s.vendored {
		return nil
	}
	cacheVolume := cacheVolumeName(s.ctx)
	if err := createCacheVolume(cacheVolume); err != nil {
		return fmt.Errorf("error creating the cache volume for the fix run: %w", err)
	}
	spec, err := buildVendorSpec(imageRef, workDirHost, cacheVolume, s.ctx)
	if err != nil {
		return err
	}
	impl := &RunAgentStep{stopSignal: s.stopSignal}
	// %w, not %s: the vendor phase honours the stop signal and returns
	// types.ErrJobStoppedByUser. Flattened to a string it stopped matching
	// errors.Is, so a user who stopped the Job during the fix run's vendor
	// phase got a failed Step instead of a cancelled one.
	if err := impl.spawnVendorAndWait(spec, s.logsWriter); err != nil {
		return fmt.Errorf("error vendoring dependencies for the fix run: %w", err)
	}
	s.vendored = true
	return nil
}

// applyMustFixEnv replaces STEP_PROMPT with the fix prompt and bounds the run.
func applyMustFixEnv(env []string, prompt string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "STEP_PROMPT", "MAX_TURNS", "AGENT_MODE":
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"STEP_PROMPT="+prompt,
		"MAX_TURNS="+strconv.Itoa(mustFixRunMaxTurns),
	)
}

// buildMustFixPrompt folds the Step's original prompt together with the
// findings that must be fixed.
//
// The original prompt comes FIRST and in full: the implementer needs to know
// what it was building before it is told what is wrong with it, or it will fix
// the finding in a way the Step's actual goal does not want.
func buildMustFixPrompt(stepPrompt string, mustFix []reviewFindingOutput) string {
	var b strings.Builder
	b.WriteString(stepPrompt)
	b.WriteString("\n\n[Review findings you must fix before this Step can finish]\n")
	b.WriteString("A review of your change found the following. Fix each one, then re-run the build/test check as usual. Change only what these findings require.\n")
	for i, f := range mustFix {
		b.WriteString(fmt.Sprintf("\n%d. %s (%s) at %s\n", i+1, strings.TrimSpace(f.Parameter), strings.TrimSpace(f.Severity), strings.TrimSpace(f.Location)))
		b.WriteString("   What: " + strings.TrimSpace(f.What) + "\n")
		if why := strings.TrimSpace(f.Why); why != "" {
			b.WriteString("   Why it matters: " + why + "\n")
		}
	}
	return b.String()
}
