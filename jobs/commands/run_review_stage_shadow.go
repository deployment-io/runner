package commands

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
)

// THE SHADOW REVIEW IS A TEMPORARY MEASUREMENT KNOB. It answers one question —
// does reviewing at a higher reasoning effort find more real problems, and what
// does it cost — before a per-Task "review depth" setting is built. Remove it,
// the ReviewShadowEffort parameter and everything in this file when per-Task
// review depth replaces it.
//
// When the Job carries a ReviewShadowEffort parameter set to one of
// reviewEffortLevels, the stage runs ONE extra review right after round 1
// completes: the same tree, spec, passes, reviewer and review environment as
// round 1, plus REVIEW_EFFORT. It is LOG-ONLY. Its findings are never
// classified, sent back, counted in MustFixOpen, rendered on the pull request
// or the dashboard, and it is never a round in reviewOutput.Rounds; the only
// trace outside the job log is its spend, which is real and goes on the Job's
// cost. The level comes from the Job: the control plane stamps it at Job
// creation, for the orgs being measured only. The runner's own environment
// plays no part — it is wiped when a runner auto-upgrades.

// reviewEffortLevels are the REVIEW_EFFORT values agentbox accepts.
var reviewEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// readShadowEffort reads the Job's ReviewShadowEffort parameter, trimmed and
// lowercased. An absent or empty parameter is "": no shadow review. Anything
// that is not one of reviewEffortLevels is logged and treated as absent.
func readShadowEffort(parameters map[string]interface{}, logsWriter io.Writer) string {
	raw, _ := jobs.GetParameterValue[string](parameters, parameters_enums.ReviewShadowEffort)
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return ""
	}
	for _, level := range reviewEffortLevels {
		if v == level {
			return v
		}
	}
	io.WriteString(logsWriter, fmt.Sprintf("Review stage: ignoring ReviewShadowEffort=%s\n", raw))
	return ""
}

// shadowReview runs the shadow review after round 1 completed, and logs it
// beside round 1. Returns an error only for a user stop; every other failure is
// logged and the stage carries on exactly as if it had not run.
func (s *reviewStage) shadowReview(round1 agentResult) error {
	if s.shadowEffort == "" {
		return nil
	}
	// Nothing to measure when round 1 already ran at the shadow's effort.
	if s.shadowEffort == s.loopEffort() {
		io.WriteString(s.logsWriter, fmt.Sprintf("Shadow review: skipped — round 1 already ran at effort %s\n", s.shadowEffort))
		return nil
	}
	// The shadow review plus the round that may follow a fix: never spend the
	// budget the loop itself needs.
	if !s.canAfford(2 * reviewRunTimeout) {
		io.WriteString(s.logsWriter, "Review stage: not enough of the stage budget left for the shadow review — skipping it\n")
		return nil
	}
	io.WriteString(s.logsWriter, fmt.Sprintf("Shadow review: reviewing round 1's tree again at effort %s (log only)\n", s.shadowEffort))
	result, err := s.shadowRun()
	s.accumulateShadowRun(result)
	if errors.Is(err, types.ErrJobStoppedByUser) {
		return err
	}
	if reason := reviewRoundFailure(result, err); reason != "" {
		io.WriteString(s.logsWriter, fmt.Sprintf("Shadow review (effort %s) did not complete: %s\n", s.shadowEffort, reason))
		return nil
	}
	s.logEffortComparison(round1, result)
	return nil
}

// shadowRun spawns the shadow review: round 1's environment plus REVIEW_EFFORT,
// with its output parked under its own name so it cannot land on round 1's.
func (s *reviewStage) shadowRun() (agentResult, error) {
	env, err := s.reviewSpawnEnvWithEffort(1, s.shadowEffort)
	if err != nil {
		return agentResult{}, err
	}
	if s.runShadow != nil {
		return s.runShadow(env)
	}
	return s.spawnReviewRun(reviewRunSpec{
		label:     "Shadow review",
		round:     1,
		outputDir: shadowReviewOutputPath(s.workDirHost),
	}, env)
}

// shadowReviewOutputPath is the shadow review's own output directory, a
// sibling of the work dir. It matches cleanupReviewStageSiblings' round
// pattern, so the stage's sweep collects it too.
func shadowReviewOutputPath(workDirHost string) string {
	return strings.TrimRight(workDirHost, "/") + "-review-round-1-shadow-output"
}

// accumulateShadowRun puts the shadow review's spend on the Job's cost — but
// never as a round, which is why it is not accumulateReviewRun. Its blocked
// hosts go to the job log only, as a round's do.
func (s *reviewStage) accumulateShadowRun(result agentResult) {
	if len(result.DeniedHosts) > 0 {
		io.WriteString(s.logsWriter, fmt.Sprintf("Shadow review: the reviewer's requests to these hosts were blocked: %s\n",
			strings.Join(result.DeniedHosts, ", ")))
	}
	result.DeniedHosts = nil
	if err := accumulateReviewRunUsage(s.parameters, s.reviewerView(), result); err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("warning: could not record the shadow review's usage: %s\n", err))
	}
}

// logEffortComparison writes round 1 and the shadow review side by side, then
// the shadow's findings and coverage in logFullReview's layout.
func (s *reviewStage) logEffortComparison(round1, shadow agentResult) {
	var b strings.Builder
	b.WriteString("--- Review effort comparison (round 1's tree) ---\n")
	b.WriteString(effortComparisonLine("Round 1 (effort "+effortName(s.loopEffort())+")", round1))
	b.WriteString(effortComparisonLine("Shadow (effort "+s.shadowEffort+")", shadow))
	for _, f := range shadow.ReviewResult.Findings {
		b.WriteString(fmt.Sprintf("  [shadow] %s/%s at %s — %s\n", f.Parameter, f.Severity, f.Location, f.What))
		if f.Why != "" {
			b.WriteString(fmt.Sprintf("      why: %s\n", f.Why))
		}
	}
	for _, c := range coverageOutputs(shadow.ReviewResult.Coverage) {
		line := fmt.Sprintf("  coverage: %s — %s", c.Parameter, c.State)
		if c.Reason != "" {
			line += " (" + c.Reason + ")"
		}
		b.WriteString(line + "\n")
	}
	io.WriteString(s.logsWriter, b.String())
}

// effortComparisonLine is one run's line of the comparison: its findings, its
// wall clock from result.json, its tokens, and its cost when it reported one.
func effortComparisonLine(name string, result agentResult) string {
	findings := 0
	if result.ReviewResult != nil {
		findings = len(result.ReviewResult.Findings)
	}
	duration := "duration unknown"
	if result.StartedAt != 0 && result.EndedAt != 0 {
		duration = fmt.Sprintf("%d s", result.EndedAt-result.StartedAt)
	}
	line := fmt.Sprintf("%s: %d finding(s), %s, in %d / out %d tokens",
		name, findings, duration, result.TokenUsage.InputTokens, result.TokenUsage.OutputTokens)
	if result.CostUSD != nil {
		line += fmt.Sprintf(", $%.4f", *result.CostUSD)
	}
	return line + "\n"
}
