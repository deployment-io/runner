package commands

import (
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
)

// Both guards return before any AWS/RunnerData access, so they're safe to unit
// test. The AssumeRole+inject path needs live AWS creds and is covered by the
// end-to-end dogfood on a Bedrock-enabled runner.

func TestApplyBedrockCredsIfNeeded_NoOpWhenNotBedrock(t *testing.T) {
	env := map[string]string{"MODEL": "us.anthropic.claude-sonnet-4-6"}
	applyBedrockCredsIfNeeded(env, io.Discard)
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("must not inject creds when CLAUDE_CODE_USE_BEDROCK is absent")
	}
	if _, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Error("must not touch env at all when not in Bedrock mode")
	}
}

func TestApplyBedrockCredsIfNeeded_NoOpWhenRoleArnMissing(t *testing.T) {
	t.Setenv("BedrockRoleArn", "") // stack predates Bedrock support
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}
	applyBedrockCredsIfNeeded(env, io.Discard)
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("must not inject creds when BedrockRoleArn is unset")
	}
}

// Model resolution: the dot rule and the region prefix are pure logic and
// testable; the ListInferenceProfiles branch needs live AWS and is covered by
// the smoke test. The dot rule is what keeps a hand-pinned profile id working,
// so it is worth pinning precisely.
func TestResolveBedrockModelID_PassesThroughConcreteProfileIDs(t *testing.T) {
	// Both real shapes seen in the wild — a clean id and a dated/revisioned one.
	for _, id := range []string{
		"eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"anthropic.claude-sonnet-5",
		"us.anthropic.claude-opus-4-1-20250805-v1:0",
	} {
		// A concrete id must short-circuit BEFORE any AWS call — passing a zero
		// aws.Config proves it never reaches the SDK.
		if got := resolveBedrockModelID(context.Background(), aws.Config{}, id, "eu-west-1", io.Discard); got != id {
			t.Errorf("resolveBedrockModelID(%q) = %q, want it passed through unchanged", id, got)
		}
	}
}

func TestBedrockRegionPrefix(t *testing.T) {
	cases := map[string]string{
		"eu-west-1":      "eu.",
		"eu-central-1":   "eu.",
		"ap-southeast-2": "apac.", // NOT "ap." — Bedrock groups APAC under one prefix
		"us-east-1":      "us.",
		"ca-central-1":   "us.", // Americas fall back to the us. geography
	}
	for region, want := range cases {
		if got := bedrockRegionPrefix(region); got != want {
			t.Errorf("bedrockRegionPrefix(%q) = %q, want %q", region, got, want)
		}
	}
}

func TestResolveBedrockModelID_UsesTheSharedCatalogue(t *testing.T) {
	// The logical -> prefix map is not duplicated here; it comes from
	// deployment-runner-kit, which the control plane also imports. Pin the seam
	// so a catalogue change that drops a prefix is visible from this side too.
	m, err := llm_provider_enums.GetModel("claude-opus-4-8")
	if err != nil {
		t.Fatalf("catalogue no longer knows claude-opus-4-8: %v", err)
	}
	if m.BedrockProfilePrefix() == "" {
		t.Error("claude-opus-4-8 has no Bedrock profile prefix in the catalogue; discovery cannot resolve it")
	}
	// This function takes a PROFILE PREFIX, not a logical model — deciding
	// whether a model is a catalogue model, and whether it is served by
	// Bedrock at all, belongs to applyAgentModelEnv one layer up. An empty
	// prefix must short-circuit rather than reach the SDK.
	if got := resolveBedrockModelID(context.Background(), aws.Config{}, "", "eu-west-1", io.Discard); got != "" {
		t.Errorf("empty prefix returned %q, want \"\" without touching AWS", got)
	}
}

// The requested session duration is governed by AWS role chaining, not by
// dr-bedrock-role's MaxSessionDuration: the runner is already an assumed role,
// so assuming another role is chained and hard-capped at 1 hour. Exceeding it
// fails the whole AssumeRole call and the task then runs credential-free.
// These pin the limit so it cannot drift back up.
func TestBedrockSessionSeconds_WithinStsAndChainingBounds(t *testing.T) {
	const stsMinSeconds = 900
	got := bedrockSessionSeconds()
	if got > bedrockMaxSessionSeconds {
		t.Errorf("session %ds exceeds the role-chaining limit of %ds — AssumeRole would fail outright", got, bedrockMaxSessionSeconds)
	}
	if got < stsMinSeconds {
		t.Errorf("session %ds is below the STS minimum of %ds", got, stsMinSeconds)
	}
}

func TestBedrockSessionSeconds_ClampedToOneHourNotTheTaskCap(t *testing.T) {
	// Regression guard for the first live Bedrock run, which requested the full
	// 4h task cap and was rejected with "exceeds the 1 hour session limit for
	// roles assumed by role chaining". The clamp must bite here — asserting the
	// concrete 3600 rather than just "<= the constant" so that raising the
	// constant fails this test instead of silently reintroducing the bug.
	const roleChainingLimit = 3600
	if got := bedrockSessionSeconds(); got != roleChainingLimit {
		t.Errorf("bedrockSessionSeconds() = %d, want %d (AWS role-chaining cap)", got, roleChainingLimit)
	}
	// And it must genuinely be shorter than the task cap — i.e. the clamp is
	// doing work, not coincidentally agreeing. Bedrock creds therefore expire
	// mid-run on long tasks; refresh is the deferred fix, not a longer session.
	if int32(defaultWallClockTimeout.Seconds()) <= roleChainingLimit {
		t.Skip("task cap no longer exceeds the chaining limit; mid-run expiry note is stale")
	}
}

// applyAgentModelEnv renders the Task's LOGICAL model into whatever the agent
// reads. These exercise the non-Bedrock paths, which need no AWS at all — and
// which are the ones that previously had no rendering step, so an opencode task
// on Anthropic would have received a bare id with no provider prefix.

func TestProviderFromEnv_ReadsTheInjectedCredentials(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want llm_provider_enums.Provider
	}{
		{"bedrock marker", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, llm_provider_enums.AWSBedrock},
		{"anthropic key", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"}, llm_provider_enums.AnthropicDirect},
		{"openai key", map[string]string{"OPENAI_API_KEY": "sk-x"}, llm_provider_enums.OpenAIDirect},
		{"subscription", map[string]string{claudeAuthModeEnvVar: claudeAuthModeSubscription}, llm_provider_enums.AnthropicSubscription},
		// A subscription org may ALSO carry an API key as its documented
		// fallback, so the marker has to win or such orgs look like
		// AnthropicDirect and get the wrong model id.
		{"subscription with fallback key", map[string]string{
			claudeAuthModeEnvVar: claudeAuthModeSubscription,
			"ANTHROPIC_API_KEY":  "sk-ant-fallback",
		}, llm_provider_enums.AnthropicSubscription},
		{"nothing recognisable", map[string]string{}, 0},
	}
	for _, c := range cases {
		if got := providerFromEnv(c.env); got != c.want {
			t.Errorf("%s: providerFromEnv = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestApplyAgentModelEnv_OpencodeGetsAProviderPrefixWithoutBedrock(t *testing.T) {
	// The gap this function closes: before it, only the Bedrock path rendered a
	// model, so opencode on Anthropic received a bare "claude-sonnet-4-6" and
	// had no way to know where to route it.
	env := map[string]string{
		"AGENT_TYPE":        llm_provider_enums.Opencode.String(),
		"MODEL":             "claude-sonnet-4-6",
		"ANTHROPIC_API_KEY": "sk-ant-x",
	}
	applyAgentModelEnv(env, nil, io.Discard)
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
	applyAgentModelEnv(env, nil, io.Discard)
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
	applyAgentModelEnv(env, nil, io.Discard)
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
	applyAgentModelEnv(env, nil, io.Discard)
	if env["MODEL"] != "claude-opus-4-8" {
		t.Errorf("MODEL = %q, want it untouched when no provider can be inferred", env["MODEL"])
	}
}

func TestApplyAgentModelEnv_UnknownModelPassesThrough(t *testing.T) {
	env := map[string]string{"MODEL": "some-future-model", "ANTHROPIC_API_KEY": "sk-ant-x"}
	applyAgentModelEnv(env, nil, io.Discard)
	if env["MODEL"] != "some-future-model" {
		t.Errorf("MODEL = %q, want an unknown id passed through unchanged", env["MODEL"])
	}
}

func TestApplyAgentModelEnv_NoResolverAvailableDoesNotPanic(t *testing.T) {
	// applyBedrockCredsIfNeeded leaves the marker in env even when it fails, so
	// providerFromEnv still reports AWSBedrock. With no resolver registered the
	// discovery step must be skipped entirely — an earlier revision called into
	// the SDK with a zero aws.Config, which PANICS rather than erroring and
	// took the whole runner down instead of failing one Step.
	env := map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"MODEL":                   "nova-pro-v1",
		"AGENT_TYPE":              llm_provider_enums.Opencode.String(),
	}
	applyAgentModelEnv(env, nil, io.Discard)
	// Degrades to the logical id, still provider-prefixed so opencode reports a
	// credential problem rather than an unroutable model.
	if want := "amazon-bedrock/nova-pro-v1"; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q", env["MODEL"], want)
	}
}

func TestBuildModelResolvers_AbsenceIsTheAvailabilitySignal(t *testing.T) {
	// No boolean is threaded through the call chain: a provider that cannot
	// discover right now simply has no entry.
	if got := buildModelResolvers(aws.Config{}, "eu-west-1", false, io.Discard); len(got) != 0 {
		t.Errorf("failed Bedrock setup registered %d resolver(s), want none", len(got))
	}
	ready := buildModelResolvers(aws.Config{}, "eu-west-1", true, io.Discard)
	if _, ok := ready[llm_provider_enums.AWSBedrock]; !ok {
		t.Error("a successful Bedrock setup must register a resolver for AWSBedrock")
	}
	// Providers that declare their ids must never get one — a resolver for them
	// would mean discovery where none is needed.
	for _, p := range []llm_provider_enums.Provider{
		llm_provider_enums.AnthropicDirect,
		llm_provider_enums.OpenAIDirect,
		llm_provider_enums.AnthropicSubscription,
	} {
		if _, ok := ready[p]; ok {
			t.Errorf("%s declares its model ids; it must not have a resolver", p)
		}
	}
}

func TestApplyAgentModelEnv_UsesTheRegisteredResolver(t *testing.T) {
	// The call site must not name a provider: it looks the resolver up and uses
	// whatever it returns, which is what lets Vertex slot in as a map entry.
	called := ""
	resolvers := map[llm_provider_enums.Provider]modelResolver{
		llm_provider_enums.AWSBedrock: func(_ context.Context, m llm_provider_enums.Model) string {
			called = m.String()
			return "eu.amazon.nova-pro-v1:0"
		},
	}
	env := map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AGENT_TYPE":              llm_provider_enums.Opencode.String(),
		"MODEL":                   "nova-pro-v1",
	}
	applyAgentModelEnv(env, resolvers, io.Discard)

	if called != "nova-pro-v1" {
		t.Errorf("resolver received %q, want the logical model", called)
	}
	if want := "amazon-bedrock/eu.amazon.nova-pro-v1:0"; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q", env["MODEL"], want)
	}
}
