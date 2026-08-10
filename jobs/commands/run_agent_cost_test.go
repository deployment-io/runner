package commands

import (
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
