package commands

import (
	"encoding/json"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

func costParams(model string, p llm_provider_enums.Provider) map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.Model, model)
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentProvider, p.Key())
	return parameters
}

// A cost the agent reported wins outright, and is marked as measured. This is
// the case that must never be overwritten by our arithmetic: the vendor that
// billed the run is a better source than a rate table we maintain by hand.
func TestResolveRunCost_AgentReportedWins(t *testing.T) {
	reported := 1.2345
	got := resolveRunCost(
		costParams("claude-opus-5", llm_provider_enums.AnthropicDirect),
		agentResult{CostUSD: &reported, TokenUsage: tokenUsage{InputTokens: 999999, OutputTokens: 999999}},
	)
	if got == nil || got.USD != reported || got.Source != costSourceAgent {
		t.Fatalf("resolveRunCost = %+v, want the reported %.4f marked %q", got, reported, costSourceAgent)
	}
	// Recorded so a later question about a surprising figure does not require
	// guessing which route was taken.
	if got.Provider != llm_provider_enums.AnthropicDirect.String() || got.Model != "claude-opus-5" {
		t.Errorf("cost = %+v, want the model and provider recorded", got)
	}
}

// codex reports tokens only, so the run is priced here — once — and marked as
// an estimate rather than presented as measured.
func TestResolveRunCost_EstimatesWhenTheAgentReportsNone(t *testing.T) {
	got := resolveRunCost(
		costParams("gpt-5.5", llm_provider_enums.OpenAIDirect),
		agentResult{TokenUsage: tokenUsage{InputTokens: 1_000_000, CacheReadTokens: 600_000, OutputTokens: 100_000}},
	)
	if got == nil {
		t.Fatal("resolveRunCost = nil; a priced pair must produce an estimate")
	}
	// 400k fresh @ $5 + 600k cached @ $0.50 + 100k output @ $30.
	if want := 0.4*5.00 + 0.6*0.50 + 0.1*30.00; got.USD < want-1e-9 || got.USD > want+1e-9 {
		t.Errorf("USD = %.6f, want %.6f", got.USD, want)
	}
	if got.Source != costSourceEstimated {
		t.Errorf("Source = %q, want %q — an estimate must not present as measured", got.Source, costSourceEstimated)
	}
}

// nil, never zero. A run we cannot price has an UNKNOWN cost; recording 0.0
// would tell a customer it was free.
func TestResolveRunCost_UnpriceableIsNilNotZero(t *testing.T) {
	cases := []struct {
		name  string
		model string
		p     llm_provider_enums.Provider
	}{
		// No per-token price exists for a subscription at all.
		{"subscription", "claude-opus-5", llm_provider_enums.AnthropicSubscription},
		// Priced pair exists for OpenAI, but not through this route.
		{"unpriced route", "claude-opus-5", llm_provider_enums.AWSBedrock},
		{"model outside the catalogue", "some-model-we-retired", llm_provider_enums.OpenAIDirect},
		{"no model at all", "", llm_provider_enums.OpenAIDirect},
	}
	for _, c := range cases {
		if got := resolveRunCost(costParams(c.model, c.p), agentResult{
			TokenUsage: tokenUsage{InputTokens: 500_000, OutputTokens: 50_000},
		}); got != nil {
			t.Errorf("%s: resolveRunCost = %+v, want nil so the cost renders as unknown", c.name, got)
		}
	}
}

// Cache-write tokens must survive the trip from agentbox to Job.Output.
//
// They were silently dropped here for a while: agentbox emits
// cache_creation_tokens and app-server reads it, but this package's tokenUsage
// had no such field, so the middle of the chain zeroed it. A struct field
// missing from a passthrough type is invisible in review and in tests that only
// assert on the fields that exist — hence a test that decodes the real wire
// shape rather than constructing the struct.
func TestTokenUsage_CarriesCacheCreationTokens(t *testing.T) {
	const resultJSON = `{
		"status": "success",
		"token_usage": {
			"input_tokens": 1000,
			"output_tokens": 200,
			"cache_read_tokens": 300,
			"cache_creation_tokens": 400
		}
	}`
	var got agentResult
	if err := json.Unmarshal([]byte(resultJSON), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TokenUsage.CacheCreationTokens != 400 {
		t.Errorf("CacheCreationTokens = %d, want 400 — agentbox reports this and app-server reads it; dropping it here understates every cache-heavy run",
			got.TokenUsage.CacheCreationTokens)
	}

	// And it must survive re-encoding into the JobOutput envelope under the key
	// app-server looks for.
	encoded, err := json.Marshal(agentOutput{TokenUsage: got.TokenUsage})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	usage, _ := envelope["token_usage"].(map[string]interface{})
	if v, _ := usage["cache_creation_tokens"].(float64); v != 400 {
		t.Errorf("token_usage.cache_creation_tokens = %v, want 400 — this is the key app-server extracts", usage["cache_creation_tokens"])
	}
}
