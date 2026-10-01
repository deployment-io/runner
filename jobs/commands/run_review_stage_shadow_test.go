package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
)

// --- the shadow review: one extra, log-only review at a chosen effort --------

func floatPtr(v float64) *float64 { return &v }

// shadowRoundOne is round 1's report: one must-fix finding, so the loop goes on
// to a fix run — which the shadow review must come before.
func shadowRoundOne() agentResult {
	return agentResult{
		Status: "success", Turns: 4,
		TokenUsage: tokenUsage{InputTokens: 1000, OutputTokens: 200},
		CostUSD:    floatPtr(0.5),
		StartedAt:  1000, EndedAt: 1120,
		ReviewResult: &reviewResult{
			Findings: []reviewFinding{{
				Key: "sec-1", Parameter: "security", Severity: "high",
				Location: "0-acme/api/handler.go:41", What: "the handler does not check the caller's session",
			}},
			Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}},
		},
	}
}

// shadowReport is what the shadow review found: more, and different.
func shadowReport() agentResult {
	return agentResult{
		Status: "success", Turns: 7,
		TokenUsage: tokenUsage{InputTokens: 3000, OutputTokens: 900},
		CostUSD:    floatPtr(1.25),
		StartedAt:  2000, EndedAt: 2300,
		DeniedHosts: []string{"chatgpt.com"},
		ReviewResult: &reviewResult{
			Findings: []reviewFinding{
				{Key: "sec-shadow", Parameter: "security", Severity: "critical", Location: "0-acme/api/auth.go:7",
					What: "the session token is compared with ==", Why: "a timing attack recovers it"},
				{Key: "cor-shadow", Parameter: "correctness", Severity: "high", Location: "0-acme/api/handler.go:12",
					What: "the error from Save is dropped"},
			},
			Coverage: []reviewCoverage{
				{Parameter: "security", State: "checked"},
				{Parameter: "correctness", State: "not checked", Reason: "ran out of turns"},
			},
		},
	}
}

// shadowTestStage is a Review stage whose round 1 sends one finding back, whose
// fix run edits the checkout, and whose round 2 says it is resolved. events
// records every container run in order.
func shadowTestStage(t *testing.T, logs io.Writer, events *[]string) *reviewStage {
	t.Helper()
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api\n")
	return &reviewStage{
		parameters:    implementerJobParameters(t),
		workDirHost:   workDir,
		logsWriter:    logs,
		participation: participationOn,
		thresholds:    map[uint]uint{1: 4},
		baseCommits:   map[string]string{"0-acme/api": "abc123"},
		deadline:      time.Now().Add(reviewStageBudget),
		copyTree:      testCopyTree,
		runReview: func(round int) (agentResult, error) {
			*events = append(*events, fmt.Sprintf("review %d", round))
			if round == 1 {
				return shadowRoundOne(), nil
			}
			return reviewRoundResult([]reviewPreviousFinding{{Key: "sec-1", Status: reviewStatusResolved}}), nil
		},
		runFix: func([]reviewFindingOutput) error {
			*events = append(*events, "fix")
			writeFile(t, filepath.Join(repoDir, "handler.go"), "package api // fixed\n")
			return nil
		},
	}
}

func reviewJSON(t *testing.T, parameters map[string]interface{}) string {
	t.Helper()
	review := readReviewFromJobOutput(parameters)
	if review == nil {
		t.Fatal("no review was recorded")
	}
	b, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// With no ReviewShadowEffort on the Job there is no shadow run, and the loop is the
// loop it always was.
func TestNoShadowReviewWhenTheEffortIsUnset(t *testing.T) {
	var logs strings.Builder
	var events []string
	stage := shadowTestStage(t, &logs, &events)
	stage.runShadow = func([]string) (agentResult, error) {
		t.Fatal("a shadow review ran with no ReviewShadowEffort on the Job")
		return agentResult{}, nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
	if want := []string{"review 1", "fix", "review 2"}; !slices.Equal(events, want) {
		t.Errorf("runs = %v, want %v", events, want)
	}
	for _, unwanted := range []string{"Shadow", "shadow", "effort comparison"} {
		if strings.Contains(logs.String(), unwanted) {
			t.Errorf("the job log mentions %q with no shadow review set:\n%s", unwanted, logs.String())
		}
	}
}

// Every review round logs its effort next to its turn cap; the loop never sets
// REVIEW_EFFORT, so it is always the model's default — even when the reviewer's
// own environment carries one.
func TestEveryReviewRoundLogsItsEffort(t *testing.T) {
	parameters := implementerJobParameters(t)
	jobs.SetParameterValue[map[string]string](parameters, parameters_enums.AgentEnvVars,
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-implementer", "REVIEW_EFFORT": "max"})
	var logs strings.Builder
	stage := reviewStageFor(t, parameters)
	stage.logsWriter = &logs
	stage.baseCommits = map[string]string{"0-acme/api": "abc123"}
	for round := 1; round <= 3; round++ {
		env, err := stage.reviewSpawnEnv(round)
		if err != nil {
			t.Fatalf("reviewSpawnEnv(%d): %s", round, err)
		}
		if v, present := envMap(env)["REVIEW_EFFORT"]; present {
			t.Errorf("round %d spawns with REVIEW_EFFORT=%s; the loop never sets it", round, v)
		}
		stage.logReviewRunLimits(fmt.Sprintf("Review round %d", round), env)
		want := fmt.Sprintf("Review round %d: turn cap %d model responses\nReview round %d: effort model default\n", round, reviewRunMaxTurns, round)
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the job log does not carry %q:\n%s", want, logs.String())
		}
	}
	env, err := stage.reviewSpawnEnvWithEffort(1, "high")
	if err != nil {
		t.Fatal(err)
	}
	stage.logReviewRunLimits("Shadow review", env)
	if !strings.Contains(logs.String(), "Shadow review: effort high\n") {
		t.Errorf("the shadow review's effort is not logged:\n%s", logs.String())
	}
}

// The shadow review: exactly one extra run, right after round 1 and before
// the fix run, on round 1's environment plus REVIEW_EFFORT. Its findings are
// logged and go nowhere else; its spend reaches the Job's cost.
func TestTheShadowReviewRunsOnceAfterRoundOneAndIsLogOnly(t *testing.T) {
	var baseLogs strings.Builder
	var baseEvents []string
	baseline := shadowTestStage(t, &baseLogs, &baseEvents)
	baseOut, err := baseline.run()
	if err != nil {
		t.Fatalf("baseline run: %s", err)
	}

	var logs strings.Builder
	var events []string
	stage := shadowTestStage(t, &logs, &events)
	stage.shadowEffort = "high"
	var shadowEnv []string
	stage.runShadow = func(env []string) (agentResult, error) {
		events = append(events, "shadow")
		shadowEnv = env
		return shadowReport(), nil
	}
	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}

	if want := []string{"review 1", "shadow", "fix", "review 2"}; !slices.Equal(events, want) {
		t.Fatalf("runs = %v, want %v", events, want)
	}
	env := envMap(shadowEnv)
	if env["REVIEW_EFFORT"] != "high" || env["REVIEW_ROUND"] != "1" || env["AGENT_MODE"] != "review" {
		t.Errorf("the shadow review's env = %v, want REVIEW_EFFORT=high, REVIEW_ROUND=1, AGENT_MODE=review", env)
	}
	if _, present := env["REVIEW_OPEN_FINDINGS"]; present {
		t.Error("the shadow review was sent open findings; round 1 was sent none")
	}
	// Round 1's environment, and REVIEW_EFFORT is the only difference.
	roundOne, err := stage.reviewSpawnEnv(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := withoutEnv(shadowEnv, "REVIEW_EFFORT"); !slices.Equal(got, roundOne) {
		t.Errorf("the shadow review's env differs from round 1's beyond REVIEW_EFFORT:\n got %q\nwant %q", got, roundOne)
	}

	// The comparison block, as one piece.
	wantBlock := "--- Review effort comparison (round 1's tree) ---\n" +
		"Round 1 (effort model default): 1 finding(s), 120 s, in 1000 / out 200 tokens, $0.5000\n" +
		"Shadow (effort high): 2 finding(s), 300 s, in 3000 / out 900 tokens, $1.2500\n" +
		"  [shadow] security/critical at 0-acme/api/auth.go:7 — the session token is compared with ==\n" +
		"      why: a timing attack recovers it\n" +
		"  [shadow] correctness/high at 0-acme/api/handler.go:12 — the error from Save is dropped\n" +
		"  coverage: security — checked\n" +
		"  coverage: correctness — not checked (ran out of turns)\n"
	if !strings.Contains(logs.String(), wantBlock) {
		t.Errorf("the job log does not carry the comparison block:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "Shadow review: the reviewer's requests to these hosts were blocked: chatgpt.com") {
		t.Errorf("the shadow review's blocked hosts are not in the job log:\n%s", logs.String())
	}

	// Nowhere else: the review block — rounds, findings, must-fix — is
	// byte-for-byte the one a stage without the shadow review records.
	if got, want := reviewJSON(t, out), reviewJSON(t, baseOut); got != want {
		t.Errorf("the shadow review changed the review output:\n got %s\nwant %s", got, want)
	}
	review := readReviewFromJobOutput(out)
	if len(review.Rounds) != 2 || review.MustFixOpen {
		t.Errorf("rounds = %d, must_fix_open = %t; want 2 rounds and nothing open", len(review.Rounds), review.MustFixOpen)
	}
	section := (&taskOpenPR{review: review}).reviewSection()
	for _, what := range []string{"compared with ==", "error from Save"} {
		if strings.Contains(section, what) || strings.Contains(reviewJSON(t, out), what) {
			t.Errorf("the shadow review's finding %q reached the review output or the pull request", what)
		}
	}

	// Its spend is real, and on the Job's cost.
	base, got := decodeJobOutput(t, baseOut), decodeJobOutput(t, out)
	if base.Cost == nil || got.Cost == nil {
		t.Fatalf("no cost recorded: baseline %+v, shadow %+v", base.Cost, got.Cost)
	}
	if diff := got.Cost.USD - base.Cost.USD; diff < 1.2499 || diff > 1.2501 {
		t.Errorf("the shadow review added $%f to the Job's cost, want $1.25", diff)
	}
	if diff := got.Agent.TokenUsage.InputTokens - base.Agent.TokenUsage.InputTokens; diff != 3000 {
		t.Errorf("the shadow review added %d input tokens, want 3000", diff)
	}
	if diff := got.Agent.Turns - base.Agent.Turns; diff != 7 {
		t.Errorf("the shadow review added %d turns, want 7", diff)
	}
	if len(got.Agent.DeniedHosts) != 0 {
		t.Errorf("the shadow review's blocked hosts reached the Step's record: %v", got.Agent.DeniedHosts)
	}
}

// A shadow review that does not complete leaves the rest of the stage exactly
// as it would have been.
func TestAFailedShadowReviewLeavesTheStageUnchanged(t *testing.T) {
	var baseEvents []string
	baseOut, err := shadowTestStage(t, io.Discard, &baseEvents).run()
	if err != nil {
		t.Fatalf("baseline run: %s", err)
	}
	for name, run := range map[string]func([]string) (agentResult, error){
		"spawn error": func([]string) (agentResult, error) { return agentResult{}, fmt.Errorf("docker went away") },
		"status": func([]string) (agentResult, error) {
			return agentResult{Status: "failure", Error: "unsupported effort"}, nil
		},
		"no result": func([]string) (agentResult, error) { return agentResult{Status: "success", Turns: 3}, nil },
	} {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			var events []string
			stage := shadowTestStage(t, &logs, &events)
			stage.shadowEffort = "xhigh"
			stage.runShadow = run
			out, err := stage.run()
			if err != nil {
				t.Fatalf("run: %s", err)
			}
			if want := []string{"review 1", "fix", "review 2"}; !slices.Equal(events, want) {
				t.Errorf("runs = %v, want %v", events, want)
			}
			if !strings.Contains(logs.String(), "Shadow review (effort xhigh) did not complete: ") {
				t.Errorf("the job log does not say the shadow review failed:\n%s", logs.String())
			}
			if strings.Contains(logs.String(), "effort comparison") {
				t.Errorf("a failed shadow review was compared:\n%s", logs.String())
			}
			if got, want := reviewJSON(t, out), reviewJSON(t, baseOut); got != want {
				t.Errorf("a failed shadow review changed the review output:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// A user stop during the shadow review is a user stop.
func TestAUserStopDuringTheShadowReviewStopsTheStage(t *testing.T) {
	var events []string
	stage := shadowTestStage(t, io.Discard, &events)
	stage.shadowEffort = "high"
	stage.runShadow = func([]string) (agentResult, error) {
		return agentResult{}, types.ErrJobStoppedByUser
	}
	if _, err := stage.run(); err != types.ErrJobStoppedByUser {
		t.Fatalf("run returned %v, want the stop sentinel", err)
	}
	if slices.Contains(events, "fix") {
		t.Errorf("a fix ran after the user stopped the Job: %v", events)
	}
}

// The shadow review needs room for itself AND the round a fix would lead to.
func TestTheShadowReviewIsSkippedWhenTheBudgetCannotAffordIt(t *testing.T) {
	var logs strings.Builder
	var events []string
	stage := shadowTestStage(t, &logs, &events)
	stage.shadowEffort = "high"
	// Room for round 1, not for round 1 plus two more review runs.
	stage.deadline = time.Now().Add(reviewRunTimeout + 10*time.Minute)
	stage.runShadow = func([]string) (agentResult, error) {
		t.Fatal("the shadow review ran without the budget for it")
		return agentResult{}, nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
	if !strings.Contains(logs.String(), "Review stage: not enough of the stage budget left for the shadow review — skipping it\n") {
		t.Errorf("the job log does not say the shadow review was skipped:\n%s", logs.String())
	}
}

// No shadow review after a round 1 that did not complete.
func TestNoShadowReviewAfterAFailedRoundOne(t *testing.T) {
	var events []string
	stage := shadowTestStage(t, io.Discard, &events)
	stage.shadowEffort = "high"
	stage.runReview = func(int) (agentResult, error) { return agentResult{Status: "failure"}, nil }
	stage.runShadow = func([]string) (agentResult, error) {
		t.Fatal("a shadow review ran after round 1 failed")
		return agentResult{}, nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
}

func TestReadShadowEffort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
		raw     string
		want    string
		log     string
	}{
		{"absent", false, "", "", ""},
		{"empty", true, "", "", ""},
		{"padded mixed case", true, " High ", "high", ""},
		{"xhigh", true, "  XHigh\n", "xhigh", ""},
		{"low", true, "Low", "low", ""},
		{"medium", true, "medium", "medium", ""},
		{"max", true, "MAX", "max", ""},
		{"bogus", true, "bogus", "", "Review stage: ignoring ReviewShadowEffort=bogus\n"},
		{"ultra", true, "ultra", "", "Review stage: ignoring ReviewShadowEffort=ultra\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := map[string]interface{}{}
			if tc.present {
				jobs.SetParameterValue[string](parameters, parameters_enums.ReviewShadowEffort, tc.raw)
			}
			var logs strings.Builder
			if got := readShadowEffort(parameters, &logs); got != tc.want {
				t.Errorf("readShadowEffort(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if logs.String() != tc.log {
				t.Errorf("logged %q, want %q", logs.String(), tc.log)
			}
		})
	}
}

// The runner's own environment plays no part: only the Job decides.
func TestReadShadowEffortIgnoresTheRunnerEnvironment(t *testing.T) {
	t.Setenv("REVIEW_SHADOW_EFFORT", "high")
	var logs strings.Builder
	if got := readShadowEffort(map[string]interface{}{}, &logs); got != "" {
		t.Errorf("readShadowEffort with no Job parameter = %q, want \"\"", got)
	}
	if logs.String() != "" {
		t.Errorf("logged %q, want nothing", logs.String())
	}
}

func TestEffortComparisonLineWithNoDurationOrCost(t *testing.T) {
	got := effortComparisonLine("Round 1 (effort model default)", agentResult{
		StartedAt: 1000, TokenUsage: tokenUsage{InputTokens: 5, OutputTokens: 6},
		ReviewResult: &reviewResult{},
	})
	if want := "Round 1 (effort model default): 0 finding(s), duration unknown, in 5 / out 6 tokens\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The shadow review's output directory is its own, and the stage's sweep
// collects it.
func TestTheShadowReviewOutputDirIsItsOwn(t *testing.T) {
	workDir := t.TempDir()
	if shadowReviewOutputPath(workDir) == reviewRoundOutputPath(workDir, 1) {
		t.Fatal("the shadow review would overwrite round 1's output")
	}
	writeFile(t, filepath.Join(shadowReviewOutputPath(workDir), "result.json"), "{}")
	cleanupReviewStageSiblings(workDir)
	if matches, _ := filepath.Glob(shadowReviewOutputPath(workDir)); len(matches) != 0 {
		t.Error("the stage's sweep left the shadow review's output behind")
	}
}
