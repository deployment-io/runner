package commands

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
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

func TestBedrockModelFamilies_AreFamilyTokensNotConcreteIDs(t *testing.T) {
	// The whole point of the map is that it stops at the family: a version or
	// revision baked in here would need a runner release per Bedrock model
	// launch, and would fail at task time in between.
	for logical, family := range bedrockModelFamilies {
		if strings.Contains(family, ".") || strings.Contains(family, ":") {
			t.Errorf("family %q for %q looks like a concrete profile id; it must be a family token only", family, logical)
		}
		if strings.Contains(logical, ".") {
			t.Errorf("logical id %q contains a dot, which resolveBedrockModelID treats as an already-concrete id", logical)
		}
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
