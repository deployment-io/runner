package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
)

// qaRoundDirPath is a QA round's /work: a NEW sibling of the Task dir, never
// inside it, holding nothing but agentbox's output directories (and the
// context mount point). The QA run sees no repository.
func qaRoundDirPath(workDirHost string, round int) string {
	return fmt.Sprintf("%s-qa-round-%d", strings.TrimRight(workDirHost, "/"), round)
}

// runQARound runs one QA container and returns what it reported.
func (s *qaStage) runQARound(round int) (agentResult, error) {
	imageRef, err := jobs.GetParameterValue[string](s.parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return agentResult{}, fmt.Errorf("agentbox image missing: %s", err)
	}
	env, err := s.qaSpawnEnv()
	if err != nil {
		return agentResult{}, err
	}
	qaDir := qaRoundDirPath(s.workDirHost, round)
	_ = os.RemoveAll(qaDir)
	defer os.RemoveAll(qaDir)
	if err := os.MkdirAll(qaDir, 0o755); err != nil {
		return agentResult{}, fmt.Errorf("error preparing the QA directory: %s", err)
	}
	if err := prepareAgentboxHostDirs(qaDir); err != nil {
		return agentResult{}, fmt.Errorf("error preparing the QA directory: %s", err)
	}
	if s.contextCopyDir != "" {
		if err := ensureContextMountPoint(qaDir, s.logsWriter); err != nil {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA round %d: the deployment context cannot be mounted (%s) — running without it\n", round, err))
		}
	}
	// Previews build from the Task's REAL working tree, which the QA run never
	// sees: the deploy tool copies <Task dir>/<repository> host-side.
	previewDeps := buildStaticSitePreviewDeps(s.ctx, s.parameters, s.workDirHost, s.logsWriter)
	// ...and from the merge result of that tree with the base branch's latest
	// commit, where it can be made: what ships when the pull request merges.
	mergeResults := s.newQAMergeResults()
	defer func() {
		mergeResults.remove()
		s.roundMergeResults = mergeResults.results()
	}()
	previewDeps.MergeResult = mergeResults
	label := fmt.Sprintf("QA round %d", round)
	io.WriteString(s.logsWriter, fmt.Sprintf("%s: turn cap %s model responses\n", label, envValue(env, "MAX_TURNS")))
	impl := &RunAgentStep{stopSignal: s.stopSignal, progressSink: s.progressSink}
	result, err := impl.spawnAgentboxAndWait(s.qaSpawnSpec(imageRef, qaDir, env, previewDeps), s.logsWriter)
	logRoundOutput(filepath.Join(qaDir, agentboxResultDirRel), label, s.logsWriter)
	return result, err
}

// qaSpawnSpec is a QA round's container: /work is the QA dir, the context
// copy is bound read-only over /work/context, the tool socket sits beside the
// QA dir, and the preview tools build from the Task dir. No repository mounts
// and no dependency cache: the QA run builds nothing itself.
func (s *qaStage) qaSpawnSpec(imageRef, qaDir string, env []string, previewDeps *agenttools.DeployStaticSitePreviewDeps) agentboxSpawnSpec {
	return agentboxSpawnSpec{
		imageRef:           imageRef,
		workDirHost:        qaDir,
		env:                env,
		mcpSocketHost:      agentMCPSocketHostPath(qaDir),
		previewDeps:        previewDeps,
		waitTimeout:        qaRunTimeout,
		readOnlyContextDir: s.contextCopyDir,
	}
}

// qaSpawnEnv builds the QA container's environment: the QA view's agent and
// credentials, then the QA inputs.
func (s *qaStage) qaSpawnEnv() ([]string, error) {
	env, err := buildAgentSpawnEnvVars(s.qaView(), s.logsWriter)
	if err != nil {
		return nil, err
	}
	spec, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.ReviewSpec)
	return applyQAEnv(env, spec, s.repositories), nil
}

// applyQAEnv turns an implement-run environment into a QA-run one.
//
// THE QA RUN IS BLIND, and this is where that is enforced for the environment:
// the step prompt, the previous steps' summary and every REVIEW_* input are
// REMOVED, so the run cannot learn what the implementer was told or what the
// reviewer saw. QA_SPEC is the Task's spec as-is — what the change is for, which
// is what QA checks against.
func applyQAEnv(env []string, spec string, repositories []string) []string {
	out := make([]string, 0, len(env)+5)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch {
		case key == "STEP_PROMPT", key == "PREVIOUS_STEPS_SUMMARY", key == "AGENT_MODE", key == "MAX_TURNS", key == agentMCPSocketEnvVar:
			continue
		case strings.HasPrefix(key, "REVIEW_"), strings.HasPrefix(key, "QA_"):
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"AGENT_MODE=qa",
		"QA_REPOSITORIES="+strings.Join(repositories, ","),
		agentMCPSocketEnvVar+"="+agentboxMCPSocketInContainer,
		"MAX_TURNS="+strconv.Itoa(qaRunMaxTurns),
	)
	if strings.TrimSpace(spec) != "" {
		out = append(out, "QA_SPEC="+spec)
	}
	return out
}

// roundOutput turns a QA run's outcome into its round record. The status is
// agentbox's qa_result status, except where the runner knows better: a spawn
// that failed, an image that predates QA mode, or no qa_result at all is
// couldnt_run naming why. "skipped" is the runner's alone, set before any run,
// so an agent-reported one reads as couldnt_run, as agentbox itself reads it.
func (s *qaStage) roundOutput(round int, result agentResult, err error) qaRoundOutput {
	out := qaRoundOutput{
		Round:      round,
		AgentType:  s.agentType(),
		Model:      s.model(),
		Turns:      result.Turns,
		TokenUsage: result.TokenUsage,
	}
	if cost := resolveRunCost(s.qaView(), result); cost != nil {
		usd := cost.USD
		out.CostUSD = &usd
	}
	// Whatever qa_result agentbox wrote is kept, even beside a failure (a run
	// stopped at its timeout still leaves one): its per-criterion accounting
	// and evidence stand, only the status says the run did not finish.
	if qa := result.QAResult; qa != nil {
		out.Status, out.StatusReason, out.NoCriteria = qa.Status, strings.TrimSpace(qa.StatusReason), qa.NoCriteria
		out.Criteria = qa.Criteria
		out.Observations = qa.Observations
		for _, f := range qa.Findings {
			out.Findings = append(out.Findings, reviewFindingOutput{
				Key: f.Key, Parameter: f.Parameter, Severity: f.Severity, Location: f.Location,
				What: f.What, Why: f.Why, Stage: f.Stage, Pass: f.Pass,
			})
		}
	}
	if reason := qaRoundFailure(result, err); reason != "" {
		out.Status, out.StatusReason = tasks.QAStatusCouldNotRun, reason
		return out
	}
	qa := result.QAResult
	if out.Status != tasks.QAStatusCompleted && out.Status != tasks.QAStatusCouldNotRun {
		if out.StatusReason == "" {
			out.StatusReason = fmt.Sprintf("the QA run reported status %q", qa.Status)
		}
		out.Status = tasks.QAStatusCouldNotRun
	}
	if out.Status == tasks.QAStatusCouldNotRun && out.StatusReason == "" {
		out.StatusReason = "the QA run gave no reason"
	}
	return out
}

// qaRoundFailure names why a QA run produced no usable qa_result, or "".
func qaRoundFailure(result agentResult, err error) string {
	if isOlderAgentboxRejection(result) {
		return qaOlderAgentboxReason
	}
	if err != nil {
		return err.Error()
	}
	if result.QAResult == nil {
		if strings.TrimSpace(result.Error) != "" {
			return fmt.Sprintf("the QA run produced no qa_result: %s", result.Error)
		}
		return "the QA run produced no qa_result"
	}
	return ""
}

// failedCriteria is the round's criteria QA found not met.
func failedCriteria(round qaRoundOutput) []qaCriterion {
	var out []qaCriterion
	for _, c := range round.Criteria {
		if c.State == tasks.QACriterionFailed {
			out = append(out, c)
		}
	}
	return out
}

// fixedByQA is every criterion that failed in round 1 and passed in the final
// round, by its text.
func fixedByQA(first, final qaRoundOutput) []string {
	passed := map[string]bool{}
	for _, c := range final.Criteria {
		if c.State == tasks.QACriterionPassed {
			passed[criterionKey(c.Criterion)] = true
		}
	}
	var out []string
	for _, c := range failedCriteria(first) {
		if passed[criterionKey(c.Criterion)] {
			out = append(out, c.Criterion)
		}
	}
	return out
}

func criterionKey(criterion string) string {
	return strings.ToLower(strings.Join(strings.Fields(criterion), " "))
}
