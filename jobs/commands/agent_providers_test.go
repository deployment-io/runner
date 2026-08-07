package commands

import (
	"context"
	"io"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// Both guards return before any AWS/RunnerData access, so they're safe to unit
// test. The AssumeRole+inject path needs live AWS creds and is covered by the
// end-to-end dogfood on a Bedrock-enabled runner.

// Dispatch is the registry's job: a provider with no setup registered gets
// nothing injected and nothing resolved.
func TestPrepareProvider_NoOpForAProviderWithNoSetup(t *testing.T) {
	env := map[string]string{"MODEL": "us.anthropic.claude-sonnet-4-6"}
	if prepareProvider(env, llm_provider_enums.AnthropicDirect, io.Discard) != nil {
		t.Error("a provider that declares its ids must get no resolver")
	}
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("must not inject another provider's credentials")
	}
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Error("must not touch env at all for a provider with no setup")
	}
}

// A hand-pinned profile id must still reach the agent untouched. That is now
// settled a layer UP: such an id is not in the catalogue, so applyAgentModelEnv
// passes it through and never calls a resolver at all. Asserting it here — the
// only place it could be observed before — would test a branch that can no
// longer be reached, so it is asserted where the behaviour actually lives.
func TestApplyAgentModelEnv_HandPinnedBedrockIDsSurviveUntouched(t *testing.T) {
	// Both real shapes seen in the wild — a clean id and a dated/revisioned one.
	for _, id := range []string{
		"eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"anthropic.claude-sonnet-5",
		"us.anthropic.claude-opus-4-1-20250805-v1:0",
	} {
		env := map[string]string{"MODEL": id}
		// A resolver that would corrupt the id if it were ever consulted.
		resolve := modelResolver(func(context.Context, llm_provider_enums.Model) string { return "WRONG" })
		applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.AWSBedrock, agentType: llm_provider_enums.ClaudeCode, model: id}, resolve, io.Discard)
		if env["ANTHROPIC_MODEL"] != id {
			t.Errorf("hand-pinned %q became %q", id, env["ANTHROPIC_MODEL"])
		}
	}
}

// applyAgentModelEnv renders the Task's LOGICAL model into whatever the agent
// reads. These exercise the non-Bedrock paths, which need no AWS at all — and
// which are the ones that previously had no rendering step, so an opencode task
// on Anthropic would have received a bare id with no provider prefix.

func TestApplyAgentModelEnv_OpencodeGetsAProviderPrefixWithoutBedrock(t *testing.T) {
	// The gap this function closes: before it, only the Bedrock path rendered a
	// model, so opencode on Anthropic received a bare "claude-sonnet-4-6" and
	// had no way to know where to route it.
	env := map[string]string{
		"AGENT_TYPE":        llm_provider_enums.Opencode.String(),
		"MODEL":             "claude-sonnet-4-6",
		"ANTHROPIC_API_KEY": "sk-ant-x",
	}
	applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.AnthropicDirect, agentType: llm_provider_enums.Opencode, model: "claude-sonnet-4-6"}, nil, io.Discard)
	if want := "anthropic/claude-sonnet-4-6"; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q", env["MODEL"], want)
	}
	if _, present := env["ANTHROPIC_MODEL"]; present {
		t.Error("ANTHROPIC_MODEL means nothing to opencode")
	}
}

func TestApplyAgentModelEnv_OpencodeOnOpenAI(t *testing.T) {
	env := map[string]string{
		"AGENT_TYPE":     llm_provider_enums.Opencode.String(),
		"MODEL":          "gpt-5.5",
		"OPENAI_API_KEY": "sk-x",
	}
	applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.OpenAIDirect, agentType: llm_provider_enums.Opencode, model: "gpt-5.5"}, nil, io.Discard)
	if want := "openai/gpt-5.5"; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q", env["MODEL"], want)
	}
}

func TestApplyAgentModelEnv_ClaudeCodeKeepsTheLogicalIDOffBedrock(t *testing.T) {
	// claude-code takes --model from MODEL when it is NOT in Bedrock mode, and
	// the logical id is what it expects.
	env := map[string]string{
		"MODEL":             "claude-opus-4-8",
		"ANTHROPIC_API_KEY": "sk-ant-x",
	}
	applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.AnthropicDirect, agentType: llm_provider_enums.ClaudeCode, model: "claude-opus-4-8"}, nil, io.Discard)
	if env["MODEL"] != "claude-opus-4-8" {
		t.Errorf("MODEL = %q, want the logical id unchanged", env["MODEL"])
	}
	if _, present := env["ANTHROPIC_MODEL"]; present {
		t.Error("ANTHROPIC_MODEL is the Bedrock-mode input; it must not be set otherwise")
	}
}

func TestApplyAgentModelEnv_NoCredentialLeavesTheModelAlone(t *testing.T) {
	// Better to fail on the missing credential than on a mangled model id.
	env := map[string]string{"AGENT_TYPE": llm_provider_enums.Opencode.String(), "MODEL": "claude-opus-4-8"}
	applyAgentModelEnv(env, agentSpawn{provider: 0, agentType: llm_provider_enums.Opencode, model: "claude-opus-4-8"}, nil, io.Discard)
	if env["MODEL"] != "claude-opus-4-8" {
		t.Errorf("MODEL = %q, want it untouched when no provider can be inferred", env["MODEL"])
	}
}

func TestApplyAgentModelEnv_UnknownModelPassesThrough(t *testing.T) {
	env := map[string]string{"MODEL": "some-future-model", "ANTHROPIC_API_KEY": "sk-ant-x"}
	applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.AnthropicDirect, agentType: llm_provider_enums.ClaudeCode, model: "some-future-model"}, nil, io.Discard)
	if env["MODEL"] != "some-future-model" {
		t.Errorf("MODEL = %q, want an unknown id passed through unchanged", env["MODEL"])
	}
}

func TestApplyAgentModelEnv_NoResolverAvailableDoesNotPanic(t *testing.T) {
	// The org is on Bedrock but applyBedrockCredsIfNeeded failed, so no resolver
	// was registered. The discovery step must be skipped entirely — an earlier
	// revision called into the SDK with a zero aws.Config, which PANICS rather
	// than erroring and took the whole runner down instead of failing one Step.
	env := map[string]string{
		"MODEL":      "nova-pro-v1",
		"AGENT_TYPE": llm_provider_enums.Opencode.String(),
	}
	applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.AWSBedrock, agentType: llm_provider_enums.Opencode, model: "nova-pro-v1"}, nil, io.Discard)
	// Degrades to the logical id, still provider-prefixed so opencode reports a
	// credential problem rather than an unroutable model.
	if want := "amazon-bedrock/nova-pro-v1"; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q", env["MODEL"], want)
	}
}

// Every registered setup must answer for exactly the provider it claims, and
// only providers whose ids need DISCOVERING may have one at all. A setup
// registered under the wrong key would resolve one provider's model through
// another's discovery.
func TestProviderSetups_RegisterOnlyRuntimeResolvedProviders(t *testing.T) {
	seen := map[llm_provider_enums.Provider]bool{}
	for _, setup := range providerSetups {
		p := setup.provider()
		if seen[p] {
			t.Errorf("%s has two setups; the second would be unreachable", p)
		}
		seen[p] = true
		if !p.ResolvesModelIDAtRuntime() {
			t.Errorf("%s declares its model ids; a setup for it means discovery where none is needed", p)
		}
	}
	// Every provider that DOES need discovery must have one, or its models
	// silently pass through unresolved.
	for _, p := range llm_provider_enums.AllProviders() {
		if p.ResolvesModelIDAtRuntime() && p.IsConfigurable() && !seen[p] {
			t.Errorf("%s resolves ids at runtime but has no setup registered", p)
		}
	}
}

func TestApplyAgentModelEnv_UsesTheRegisteredResolver(t *testing.T) {
	// The call site must not name a provider: it looks the resolver up and uses
	// whatever it returns, which is what lets Vertex slot in as a map entry.
	called := ""
	resolve := modelResolver(func(_ context.Context, m llm_provider_enums.Model) string {
		called = m.String()
		return "eu.amazon.nova-pro-v1:0"
	})
	env := map[string]string{
		"AGENT_TYPE": llm_provider_enums.Opencode.String(),
		"MODEL":      "nova-pro-v1",
	}
	applyAgentModelEnv(env, agentSpawn{provider: llm_provider_enums.AWSBedrock, agentType: llm_provider_enums.Opencode, model: "nova-pro-v1"}, resolve, io.Discard)

	if called != "nova-pro-v1" {
		t.Errorf("resolver received %q, want the logical model", called)
	}
	if want := "amazon-bedrock/eu.amazon.nova-pro-v1:0"; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q", env["MODEL"], want)
	}
}

// The parameter is the ONLY source. An absent or unknown one yields 0 rather
// than a guess: inferring from whichever secrets happen to be present is what
// made a subscription org — which also carries an API key as its documented
// fallback — look like AnthropicDirect.
func TestResolveJobProvider_ParameterIsTheOnlySource(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentProvider, llm_provider_enums.AnthropicSubscription.Key())
	if got := resolveJobProvider(parameters, io.Discard); got != llm_provider_enums.AnthropicSubscription {
		t.Errorf("resolveJobProvider = %v, want AnthropicSubscription", got)
	}
}

func TestResolveJobProvider_NoGuessingWhenAbsentOrUnknown(t *testing.T) {
	// A bundle full of recognisable credentials must NOT be read as a provider.
	if got := resolveJobProvider(map[string]interface{}{}, io.Discard); got.IsValid() {
		t.Errorf("resolveJobProvider = %v, want an invalid provider when the parameter is missing", got)
	}
	// A slug from a newer control plane. 0 leaves the model unrendered and runs
	// no provider setup, so the agent fails on the credential — better than
	// silently serving the Job through whatever this build guesses instead.
	unknown := map[string]interface{}{}
	jobs.SetParameterValue[string](unknown, parameters_enums.AgentProvider, "some-future-provider")
	if got := resolveJobProvider(unknown, io.Discard); got.IsValid() {
		t.Errorf("resolveJobProvider = %v, want an invalid provider for an unknown slug", got)
	}
}

// ANTHROPIC_MODEL is claude-code's Bedrock-mode input. Which agents receive it
// must match exactly which agents get CLAUDE_CODE_USE_BEDROCK — an agent put in
// Bedrock mode but told to read a different variable, or told where to read
// without being in the mode, is broken either way. Keying on the provider alone
// set it for codex too.
func TestApplyAgentModelEnv_OnlyClaudeCodeReadsTheModelFromAnthropicModel(t *testing.T) {
	for _, agentType := range []llm_provider_enums.AgentType{
		llm_provider_enums.Codex,
		llm_provider_enums.Opencode,
	} {
		env := map[string]string{"MODEL": "claude-sonnet-4-6"}
		spawn := agentSpawn{provider: llm_provider_enums.AWSBedrock, agentType: agentType, model: "claude-sonnet-4-6"}
		applyAgentModelEnv(env, spawn, nil, io.Discard)
		if _, present := env["ANTHROPIC_MODEL"]; present {
			t.Errorf("%v on Bedrock got ANTHROPIC_MODEL, which only claude-code reads", agentType)
		}
	}
	// And claude-code still must.
	env := map[string]string{"MODEL": "claude-sonnet-4-6"}
	spawn := agentSpawn{provider: llm_provider_enums.AWSBedrock, agentType: llm_provider_enums.ClaudeCode, model: "claude-sonnet-4-6"}
	applyAgentModelEnv(env, spawn, nil, io.Discard)
	if env["ANTHROPIC_MODEL"] == "" {
		t.Error("claude-code on Bedrock takes its model from ANTHROPIC_MODEL")
	}
}

// The bug this fixes: agentbox reads MODEL and passes it as --model for every
// agent, so writing only ANTHROPIC_MODEL left claude-code being told to run the
// unresolved LOGICAL id against Bedrock — a flag overriding the inference
// profile discovery had just resolved. Opencode was unaffected (it writes
// MODEL), which is why a Nova smoke test would have gone green over it.
func TestApplyAgentModelEnv_BedrockClaudeCodeGetsTheResolvedIDInBothVars(t *testing.T) {
	const resolved = "eu.anthropic.claude-sonnet-4-6-20260101-v1:0"
	env := map[string]string{}
	resolve := modelResolver(func(context.Context, llm_provider_enums.Model) string { return resolved })
	spawn := agentSpawn{
		provider:  llm_provider_enums.AWSBedrock,
		agentType: llm_provider_enums.ClaudeCode,
		model:     "claude-sonnet-4-6",
	}
	applyAgentModelEnv(env, spawn, resolve, io.Discard)

	if env["ANTHROPIC_MODEL"] != resolved {
		t.Errorf("ANTHROPIC_MODEL = %q, want the resolved profile", env["ANTHROPIC_MODEL"])
	}
	// The one that was wrong: agentbox turns this into --model.
	if env["MODEL"] != resolved {
		t.Errorf("MODEL = %q, want the resolved profile — agentbox passes it as --model, so a logical id here overrides ANTHROPIC_MODEL", env["MODEL"])
	}
}
