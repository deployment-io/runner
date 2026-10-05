package commands

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

const opencodeLevelNotice = "Review stage: review level Thorough is not available for opencode reviewers; the rounds run at the model's default\n"

func TestReviewEffortForLevel(t *testing.T) {
	for _, tc := range []struct {
		level, agentType string
		want             string
		wantUnavailable  bool
	}{
		{"thorough", "claude-code", "high", false},
		{"thorough", "codex", "high", false},
		{" Thorough ", "codex", "high", false},
		{"thorough", "", "high", false}, // an empty agent type is claude-code
		{"thorough", "opencode", "", true},
		{"thorough", "something-else", "", false},
		{"standard", "claude-code", "", false},
		{"standard", "opencode", "", false},
		{"", "codex", "", false},
		{"maximum", "claude-code", "", false},
	} {
		got, unavailable := reviewEffortForLevel(tc.level, tc.agentType)
		if got != tc.want || unavailable != tc.wantUnavailable {
			t.Errorf("reviewEffortForLevel(%q, %q) = %q, %t; want %q, %t",
				tc.level, tc.agentType, got, unavailable, tc.want, tc.wantUnavailable)
		}
	}
}

func withReviewLevel(t *testing.T, parameters map[string]interface{}, level string) map[string]interface{} {
	t.Helper()
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewLevel, level)
	return parameters
}

// Every loop round carries the level's effort; the fix run, an implement run,
// does not.
func TestThoroughLoopRoundsCarryTheEffortAndTheFixRunDoesNot(t *testing.T) {
	openAI := llm_provider_enums.OpenAIDirect
	for _, tc := range []struct {
		name       string
		parameters func(t *testing.T) map[string]interface{}
	}{
		{"the Job's own claude-code agent", func(t *testing.T) map[string]interface{} {
			return withReviewLevel(t, implementerJobParameters(t), "thorough")
		}},
		{"a codex reviewer", func(t *testing.T) map[string]interface{} {
			return withReviewLevel(t, withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", &openAI,
				map[string]string{"OPENAI_API_KEY": "sk-openai-reviewer"}), "thorough")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := reviewStageFor(t, tc.parameters(t))
			stage.baseCommits = map[string]string{"0-acme/api": "abc123"}
			for round := 1; round <= 3; round++ {
				env, err := stage.reviewSpawnEnv(round)
				if err != nil {
					t.Fatalf("reviewSpawnEnv(%d): %s", round, err)
				}
				if got := envMap(env)["REVIEW_EFFORT"]; got != "high" {
					t.Errorf("round %d REVIEW_EFFORT = %q, want high", round, got)
				}
			}
			fixEnv, err := buildAgentSpawnEnvVars(stage.parameters, io.Discard)
			if err != nil {
				t.Fatalf("the fix run's env: %s", err)
			}
			if v, present := envMap(applyMustFixEnv(fixEnv, "fix these findings"))["REVIEW_EFFORT"]; present {
				t.Errorf("the fix spawn carries REVIEW_EFFORT=%s; a fix run is an implement run", v)
			}
		})
	}
}

func TestStandardAndUnknownLevelsSendNoEffort(t *testing.T) {
	for _, level := range []string{"standard", "", "maximum"} {
		stage := reviewStageFor(t, withReviewLevel(t, implementerJobParameters(t), level))
		stage.baseCommits = map[string]string{"0-acme/api": "abc123"}
		env, err := stage.reviewSpawnEnv(1)
		if err != nil {
			t.Fatal(err)
		}
		if v, present := envMap(env)["REVIEW_EFFORT"]; present {
			t.Errorf("level %q spawns with REVIEW_EFFORT=%s, want none", level, v)
		}
	}
}

// An opencode reviewer cannot run Thorough: no effort, and one log line per
// stage however many rounds run.
func TestThoroughOnAnOpencodeReviewerLogsOnceAndSendsNoEffort(t *testing.T) {
	anthropic := llm_provider_enums.AnthropicDirect
	parameters := withReviewLevel(t, withReviewer(t, implementerJobParameters(t), "opencode", "claude-sonnet-4-6", &anthropic,
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-reviewer"}), "thorough")
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
			t.Errorf("round %d spawns with REVIEW_EFFORT=%s, want none for opencode", round, v)
		}
	}
	if n := strings.Count(logs.String(), opencodeLevelNotice); n != 1 {
		t.Errorf("the opencode notice is logged %d times, want once:\n%s", n, logs.String())
	}
}

// The shadow review is skipped when round 1 already ran at its effort, and
// runs — comparing against round 1's real effort — otherwise.
func TestTheShadowReviewIsSkippedWhenRoundOneRanAtItsEffort(t *testing.T) {
	var logs strings.Builder
	var events []string
	stage := shadowTestStage(t, &logs, &events)
	withReviewLevel(t, stage.parameters, "thorough")
	stage.shadowEffort = "high"
	stage.runShadow = func([]string) (agentResult, error) {
		events = append(events, "shadow")
		return shadowReport(), nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
	if want := []string{"review 1", "fix", "review 2"}; !slices.Equal(events, want) {
		t.Errorf("runs = %v, want %v", events, want)
	}
	if !strings.Contains(logs.String(), "Shadow review: skipped — round 1 already ran at effort high\n") {
		t.Errorf("the skip is not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "effort comparison") {
		t.Errorf("a skipped shadow review logged a comparison:\n%s", logs.String())
	}
}

func TestTheShadowReviewRunsWhenItsEffortDiffersFromRoundOnes(t *testing.T) {
	var logs strings.Builder
	var events []string
	stage := shadowTestStage(t, &logs, &events)
	withReviewLevel(t, stage.parameters, "thorough")
	stage.shadowEffort = "xhigh"
	stage.runShadow = func([]string) (agentResult, error) {
		events = append(events, "shadow")
		return shadowReport(), nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
	if want := []string{"review 1", "shadow", "fix", "review 2"}; !slices.Equal(events, want) {
		t.Errorf("runs = %v, want %v", events, want)
	}
	for _, want := range []string{
		"Round 1 (effort high): ",
		"Shadow (effort xhigh): ",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the comparison block does not carry %q:\n%s", want, logs.String())
		}
	}
}
