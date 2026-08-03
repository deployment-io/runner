package commands

import (
	"io"
	"testing"
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
