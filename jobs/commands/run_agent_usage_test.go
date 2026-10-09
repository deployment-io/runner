package commands

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

func readJobOutput(t *testing.T, parameters map[string]interface{}) jobOutputData {
	t.Helper()
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil {
		t.Fatalf("no job output: %s", err)
	}
	data := jobOutputData{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("job output: %s", err)
	}
	return data
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func assertStageUsage(t *testing.T, name string, got *stageUsageOutput, tokens tokenUsage, usd *float64, estimated bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: stage usage absent", name)
	}
	if got.TokenUsage != tokens {
		t.Errorf("%s: tokens = %+v, want %+v", name, got.TokenUsage, tokens)
	}
	switch {
	case usd == nil && got.USD != nil:
		t.Errorf("%s: usd = %v, want absent", name, *got.USD)
	case usd != nil && got.USD == nil:
		t.Errorf("%s: usd absent, want %v", name, *usd)
	case usd != nil && !approxEqual(*got.USD, *usd):
		t.Errorf("%s: usd = %v, want %v", name, *got.USD, *usd)
	}
	if got.Estimated != estimated {
		t.Errorf("%s: estimated = %v, want %v", name, got.Estimated, estimated)
	}
}

func f64(v float64) *float64 { return &v }

// An implement run alone: Implement holds its tokens and cost, Review is absent.
func TestUsage_ImplementRunAlone(t *testing.T) {
	parameters := implementerJobParameters(t)
	tokens := tokenUsage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 10, CacheCreationTokens: 5}
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", Turns: 5, TokenUsage: tokens, CostUSD: f64(0.40),
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	data := readJobOutput(t, parameters)
	if data.Usage == nil {
		t.Fatal("usage absent")
	}
	assertStageUsage(t, "implement", data.Usage.Implement, tokens, f64(0.40), false)
	if data.Usage.Review != nil {
		t.Errorf("review = %+v, want absent", data.Usage.Review)
	}
}

// Every run of a reviewed Step lands in its own stage: the implement run and
// every fix run (kept, failed, stopped) in Implement, every review round (the
// stopped one included) and the shadow review in Review — priced against the
// reviewer's view for a review run and the Job's own for a fix run. The totals
// and the rounds are what they were before the split.
func TestUsage_SplitsAReviewedStepByStage(t *testing.T) {
	openAI := llm_provider_enums.OpenAIDirect
	parameters := withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", &openAI,
		map[string]string{"OPENAI_API_KEY": "sk-openai-reviewer"})
	stage := reviewStageFor(t, parameters)

	// Implement run: Claude, agent-reported cost.
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", Turns: 10, PRTitle: "Add export",
		TokenUsage: tokenUsage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 10, CacheCreationTokens: 5},
		CostUSD:    f64(0.40),
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	// Review round 1: codex, estimated at gpt-5.5's rate = 0.05 + 0.06.
	round1 := agentResult{Status: "success", Turns: 3, TokenUsage: tokenUsage{InputTokens: 10_000, OutputTokens: 2_000}}
	stage.accumulateReviewRun(1, round1)
	stage.recordRound(1, nil, round1)
	// Kept fix run.
	if err := recordFixRunResult(parameters, agentResult{
		Status: "success", Turns: 2, TokenUsage: tokenUsage{InputTokens: 300, OutputTokens: 30}, CostUSD: f64(0.20),
	}, nil, io.Discard); err != nil {
		t.Fatalf("kept fix run: %s", err)
	}
	// Review round 2: 0.10 + 0.03.
	round2 := agentResult{Status: "success", Turns: 3, TokenUsage: tokenUsage{InputTokens: 20_000, OutputTokens: 1_000}}
	stage.accumulateReviewRun(2, round2)
	stage.recordRound(2, nil, round2)
	// Failed fix run without a reported cost: priced against the Job's own
	// Claude/Anthropic view, which has no rate — so it adds tokens only. Priced
	// against the reviewer's view it would have added an estimate.
	if err := recordFixRunResult(parameters, agentResult{
		Status: "failed", Turns: 1, TokenUsage: tokenUsage{InputTokens: 200, OutputTokens: 20},
	}, nil, io.Discard); err == nil {
		t.Fatal("failed fix run: want its outcome error")
	}
	// Stopped fix run.
	stopped := errors.New("stopped")
	if err := recordFixRunResult(parameters, agentResult{
		Turns: 1, TokenUsage: tokenUsage{InputTokens: 50, OutputTokens: 5}, CostUSD: f64(0.01),
	}, stopped, io.Discard); !errors.Is(err, stopped) {
		t.Fatalf("stopped fix run: err = %v", err)
	}
	// Shadow review: 0.025 + 0.015, never a round.
	stage.accumulateShadowRun(agentResult{Status: "success", Turns: 2, TokenUsage: tokenUsage{InputTokens: 5_000, OutputTokens: 500}})
	// A review round the user stopped: counted, never recorded as a round.
	// 0.005 + 0.003.
	stage.accumulateReviewRun(3, agentResult{Turns: 1, TokenUsage: tokenUsage{InputTokens: 1_000, OutputTokens: 100}})

	if err := mergeReviewIntoJobOutput(parameters, &reviewOutput{Rounds: stage.rounds}); err != nil {
		t.Fatalf("merge review: %s", err)
	}
	data := readJobOutput(t, parameters)
	if data.Usage == nil {
		t.Fatal("usage absent — lost on the review block's write?")
	}
	assertStageUsage(t, "implement", data.Usage.Implement,
		tokenUsage{InputTokens: 1550, OutputTokens: 155, CacheReadTokens: 10, CacheCreationTokens: 5},
		f64(0.40+0.20+0.01), false)
	assertStageUsage(t, "review", data.Usage.Review,
		tokenUsage{InputTokens: 36_000, OutputTokens: 3_600},
		f64(0.11+0.13+0.04+0.008), true)

	// The totals are as before: every run in the agent block and "cost".
	if data.Agent == nil {
		t.Fatal("agent block absent")
	}
	if want := (tokenUsage{InputTokens: 37_550, OutputTokens: 3_755, CacheReadTokens: 10, CacheCreationTokens: 5}); data.Agent.TokenUsage != want {
		t.Errorf("agent.token_usage = %+v, want %+v", data.Agent.TokenUsage, want)
	}
	if data.Agent.Turns != 23 {
		t.Errorf("agent.turns = %d, want 23", data.Agent.Turns)
	}
	if data.Agent.PRTitle != "Add export" {
		t.Errorf("agent.pr_title = %q", data.Agent.PRTitle)
	}
	if data.Cost == nil || !approxEqual(data.Cost.USD, 0.61+0.288) || data.Cost.Source != costSourceAgent {
		t.Errorf("cost = %+v, want 0.898 with the first run's source", data.Cost)
	}
	if data.Review == nil || len(data.Review.Rounds) != 2 {
		t.Fatalf("review.rounds = %+v, want the two recorded rounds", data.Review)
	}
	if data.Review.Rounds[0].TokenUsage != round1.TokenUsage || data.Review.Rounds[1].TokenUsage != round2.TokenUsage {
		t.Errorf("review.rounds token usage = %+v / %+v", data.Review.Rounds[0].TokenUsage, data.Review.Rounds[1].TokenUsage)
	}
	if data.Review.Rounds[0].CostUSD != nil || data.Review.Rounds[1].CostUSD != nil {
		t.Errorf("a codex round recorded a cost_usd")
	}
}

// Estimated is set when any part of a stage was estimated, and stays set.
func TestUsage_EstimatedWhenAnyPartIs(t *testing.T) {
	openAI := llm_provider_enums.OpenAIDirect
	parameters := costParams("gpt-5.5", openAI)
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", TokenUsage: tokenUsage{InputTokens: 1_000_000}, CostUSD: f64(1),
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	if err := mergeFixResultIntoJobOutput(parameters, agentResult{
		Status: "success", TokenUsage: tokenUsage{OutputTokens: 100_000},
	}); err != nil {
		t.Fatalf("merge fix: %s", err)
	}
	data := readJobOutput(t, parameters)
	assertStageUsage(t, "implement", data.Usage.Implement,
		tokenUsage{InputTokens: 1_000_000, OutputTokens: 100_000}, f64(1+3), true)
	if data.Cost.Source != costSourceAgent {
		t.Errorf("cost.source = %q, want the first run's, as before", data.Cost.Source)
	}
}

// A stage none of whose runs could be priced has no USD — unknown, not zero —
// while a priced stage beside it keeps its own.
func TestUsage_USDAbsentWhenNothingInTheStageIsPriced(t *testing.T) {
	sub := llm_provider_enums.AnthropicSubscription
	parameters := implementerJobParameters(t)
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentProvider, sub.Key())
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", TokenUsage: tokenUsage{InputTokens: 700, OutputTokens: 70},
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	openAI := llm_provider_enums.OpenAIDirect
	if err := accumulateReviewRunUsage(parameters, costParams("gpt-5.5", openAI), usageStageReview, agentResult{
		TokenUsage: tokenUsage{InputTokens: 1_000_000},
	}); err != nil {
		t.Fatalf("accumulate: %s", err)
	}
	data := readJobOutput(t, parameters)
	assertStageUsage(t, "implement", data.Usage.Implement, tokenUsage{InputTokens: 700, OutputTokens: 70}, nil, false)
	assertStageUsage(t, "review", data.Usage.Review, tokenUsage{InputTokens: 1_000_000}, f64(5), true)

	raw, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	var generic map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	implement := generic["usage"].(map[string]interface{})["implement"].(map[string]interface{})
	if _, ok := implement["usd"]; ok {
		t.Errorf("usage.implement.usd = %v, want the key absent", implement["usd"])
	}
}

// Each stage records the provider its runs used: the implement run's and every
// fix run's from the Job's own parameters, every review round's and the shadow
// review's from the reviewer's view. Nothing else about the usage block, the
// agent block or "cost" changes.
func TestUsage_RecordsEachStagesProvider(t *testing.T) {
	sub := llm_provider_enums.AnthropicSubscription
	openAI := llm_provider_enums.OpenAIDirect
	parameters := withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", &openAI,
		map[string]string{"OPENAI_API_KEY": "sk-openai-reviewer"})
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentProvider, sub.Key())
	stage := reviewStageFor(t, parameters)

	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", Turns: 10, PRTitle: "Add export",
		TokenUsage: tokenUsage{InputTokens: 1000, OutputTokens: 100}, CostUSD: f64(0.40),
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	// Review round: 0.05 + 0.06.
	round1 := agentResult{Status: "success", Turns: 3, TokenUsage: tokenUsage{InputTokens: 10_000, OutputTokens: 2_000}}
	stage.accumulateReviewRun(1, round1)
	stage.recordRound(1, nil, round1)
	// Failed fix run, priced against the Job's own (subscription) view.
	if err := recordFixRunResult(parameters, agentResult{
		Status: "failed", Turns: 1, TokenUsage: tokenUsage{InputTokens: 200, OutputTokens: 20},
	}, nil, io.Discard); err == nil {
		t.Fatal("failed fix run: want its outcome error")
	}
	// Shadow review: 0.025 + 0.015.
	stage.accumulateShadowRun(agentResult{Status: "success", Turns: 2, TokenUsage: tokenUsage{InputTokens: 5_000, OutputTokens: 500}})
	if err := mergeReviewIntoJobOutput(parameters, &reviewOutput{Rounds: stage.rounds}); err != nil {
		t.Fatalf("merge review: %s", err)
	}

	data := readJobOutput(t, parameters)
	if data.Usage == nil {
		t.Fatal("usage absent")
	}
	if got := data.Usage.Implement.Provider; got != "anthropic-subscription" {
		t.Errorf("usage.implement.provider = %q, want anthropic-subscription", got)
	}
	if got := data.Usage.Review.Provider; got != "openai-direct" {
		t.Errorf("usage.review.provider = %q, want openai-direct", got)
	}
	assertStageUsage(t, "implement", data.Usage.Implement,
		tokenUsage{InputTokens: 1200, OutputTokens: 120}, f64(0.40), false)
	assertStageUsage(t, "review", data.Usage.Review,
		tokenUsage{InputTokens: 15_000, OutputTokens: 2_500}, f64(0.11+0.04), true)
	if want := (tokenUsage{InputTokens: 16_200, OutputTokens: 2_620}); data.Agent == nil || data.Agent.TokenUsage != want || data.Agent.Turns != 16 {
		t.Errorf("agent = %+v, want tokens %+v over 16 turns", data.Agent, want)
	}
	if data.Cost == nil || !approxEqual(data.Cost.USD, 0.40+0.15) || data.Cost.Source != costSourceAgent ||
		data.Cost.Provider != sub.String() {
		t.Errorf("cost = %+v, want 0.55, the first run's source and its provider's display name", data.Cost)
	}
}

// The first run of a stage whose provider is known sets it; a later run on
// another provider never overwrites it, and an earlier unknown one does not
// block it.
func TestUsage_FirstKnownProviderStands(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.Model, "gpt-5.5")
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", TokenUsage: tokenUsage{InputTokens: 10},
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	if data := readJobOutput(t, parameters); data.Usage.Implement.Provider != "" {
		t.Errorf("provider = %q after an unknown-provider run, want empty", data.Usage.Implement.Provider)
	}
	for _, p := range []llm_provider_enums.Provider{llm_provider_enums.OpenAIDirect, llm_provider_enums.OpenRouter} {
		if err := accumulateReviewRunUsage(parameters, costParams("gpt-5.5", p), usageStageImplement, agentResult{
			TokenUsage: tokenUsage{InputTokens: 10},
		}); err != nil {
			t.Fatalf("accumulate: %s", err)
		}
	}
	if got := readJobOutput(t, parameters).Usage.Implement.Provider; got != llm_provider_enums.OpenAIDirect.Key() {
		t.Errorf("provider = %q, want the first known one %q", got, llm_provider_enums.OpenAIDirect.Key())
	}
}

// A stage none of whose runs had a known provider omits the key.
func TestUsage_UnknownProviderOmitted(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentProvider, "no-such-provider")
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", TokenUsage: tokenUsage{InputTokens: 10},
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	raw, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	var generic map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	implement := generic["usage"].(map[string]interface{})["implement"].(map[string]interface{})
	if _, ok := implement["provider"]; ok {
		t.Errorf("usage.implement.provider = %v, want the key absent", implement["provider"])
	}
}
