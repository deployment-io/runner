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

func TestResolveBedrockModelID_UnknownLogicalIDPassesThrough(t *testing.T) {
	// Not in the family map and not dotted: hand it to Bedrock so its own error
	// message surfaces, rather than guessing a profile.
	const model = "some-future-model"
	if got := resolveBedrockModelID(context.Background(), aws.Config{}, model, "eu-west-1", io.Discard); got != model {
		t.Errorf("resolveBedrockModelID(%q) = %q, want it passed through unchanged", model, got)
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
	// The logical -> family map is no longer duplicated here; it comes from
	// deployment-runner-kit, which the control plane also imports. Pin the seam
	// so a catalogue change that drops a family is visible from this side too.
	m, err := llm_provider_enums.GetModel("claude-opus-4-8")
	if err != nil {
		t.Fatalf("catalogue no longer knows claude-opus-4-8: %v", err)
	}
	if m.BedrockFamily() == "" {
		t.Error("claude-opus-4-8 has no Bedrock family in the catalogue; discovery cannot resolve it")
	}
	// A model the catalogue does not know must pass through, not panic or guess.
	const unknown = "some-future-model"
	if got := resolveBedrockModelID(context.Background(), aws.Config{}, unknown, "eu-west-1", io.Discard); got != unknown {
		t.Errorf("resolveBedrockModelID(%q) = %q, want it passed through unchanged", unknown, got)
	}
	// A known model that Bedrock does not serve must also pass through — gpt-5.5
	// is in the catalogue but has no family token.
	if got := resolveBedrockModelID(context.Background(), aws.Config{}, "gpt-5.5", "eu-west-1", io.Discard); got != "gpt-5.5" {
		t.Errorf("resolveBedrockModelID(gpt-5.5) = %q, want it passed through unchanged", got)
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

// applyBedrockModelEnv is where the org-level Bedrock marker is rendered into
// whatever the SELECTED HARNESS actually reads. Both branches are pure once a
// model is already concrete (the dot rule short-circuits before any AWS call),
// so they are testable without live credentials.

func TestApplyBedrockModelEnv_ClaudeCodeGetsAnthropicModel(t *testing.T) {
	const concrete = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "MODEL": concrete}
	applyBedrockModelEnv(env, aws.Config{}, "eu-west-1", io.Discard)

	if env["ANTHROPIC_MODEL"] != concrete {
		t.Errorf("ANTHROPIC_MODEL = %q, want %q", env["ANTHROPIC_MODEL"], concrete)
	}
	// claude-code reads the marker itself, so it must survive.
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Error("claude-code needs CLAUDE_CODE_USE_BEDROCK in its own env; it must not be consumed")
	}
	if env["MODEL"] != concrete {
		t.Errorf("MODEL = %q, want it left alone for claude-code", env["MODEL"])
	}
}

func TestApplyBedrockModelEnv_EmptyAgentTypeIsClaudeCode(t *testing.T) {
	// Tasks created before AGENT_TYPE existed carry no value, and agentbox
	// defaults empty to claude-code. Diverging here would render the wrong env.
	const concrete = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "MODEL": concrete, "AGENT_TYPE": ""}
	applyBedrockModelEnv(env, aws.Config{}, "eu-west-1", io.Discard)
	if env["ANTHROPIC_MODEL"] != concrete {
		t.Errorf("empty AGENT_TYPE must be treated as claude-code; ANTHROPIC_MODEL = %q", env["ANTHROPIC_MODEL"])
	}
}

func TestApplyBedrockModelEnv_OpencodeKeepsThePrefixAndDropsTheMarker(t *testing.T) {
	const concrete = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	env := map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AGENT_TYPE":              llm_provider_enums.Opencode.String(),
		"MODEL":                   "amazon-bedrock/" + concrete,
	}
	applyBedrockModelEnv(env, aws.Config{}, "eu-west-1", io.Discard)

	// opencode picks its provider from the prefix, so a bare profile id would
	// route the request somewhere else entirely.
	if want := "amazon-bedrock/" + concrete; env["MODEL"] != want {
		t.Errorf("MODEL = %q, want %q — opencode selects its provider from the prefix", env["MODEL"], want)
	}
	// The marker is a runner-facing signal. opencode cannot use it, so leaking
	// it into the container would be inert but misleading.
	if _, present := env["CLAUDE_CODE_USE_BEDROCK"]; present {
		t.Error("CLAUDE_CODE_USE_BEDROCK must be consumed for opencode, not passed through")
	}
	// ANTHROPIC_MODEL means nothing to opencode.
	if _, present := env["ANTHROPIC_MODEL"]; present {
		t.Error("ANTHROPIC_MODEL must not be set for opencode")
	}
}

func TestApplyBedrockModelEnv_NoModelIsANoOp(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}
	applyBedrockModelEnv(env, aws.Config{}, "eu-west-1", io.Discard)
	if _, present := env["ANTHROPIC_MODEL"]; present {
		t.Error("no MODEL means nothing to render")
	}
}
