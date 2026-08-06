package commands

// Bedrock's setup: the guards that return before any AWS call, and the pure
// logic around the profile search. The AssumeRole + ListInferenceProfiles paths
// need live AWS and are covered by the end-to-end dogfood on a Bedrock-enabled
// runner.

import (
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
)

func TestPrepareProvider_BedrockWithoutARoleArnRegistersNothing(t *testing.T) {
	t.Setenv("BedrockRoleArn", "") // stack predates Bedrock support
	env := map[string]string{}
	// A failed setup must register NO resolver. Registering one anyway would
	// hand the SDK a zero aws.Config, which panics rather than erroring and
	// takes the whole runner down instead of failing one Step.
	if got := prepareProvider(env, llm_provider_enums.AWSBedrock, io.Discard); len(got) != 0 {
		t.Errorf("failed Bedrock setup registered %d resolver(s), want none", len(got))
	}
	if _, ok := env["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("must not inject creds when BedrockRoleArn is unset")
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
	// A model with no Bedrock prefix cannot be discovered, and must say so by
	// returning "" — the caller then keeps model.IDFor(provider). Returning the
	// prefix instead would overwrite a correct id with a family token, which is
	// invisible while the two coincide and wrong as soon as IDFor gains an
	// override. Passing a zero aws.Config proves it short-circuits before the
	// SDK, which would panic.
	unmapped := llm_provider_enums.Gpt55 // real model, no Bedrock profile
	if unmapped.BedrockProfilePrefix() != "" {
		t.Skip("Gpt55 gained a Bedrock prefix; pick another unmapped model")
	}
	if got := resolveBedrockModelID(context.Background(), aws.Config{}, unmapped, "eu-west-1", io.Discard); got != "" {
		t.Errorf("unmapped model returned %q, want \"\" so the caller keeps IDFor", got)
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
