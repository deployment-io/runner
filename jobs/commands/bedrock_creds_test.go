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

// The requested session duration is a cross-repo constraint, not a local knob:
// STS rejects the whole AssumeRole call if it exceeds dr-bedrock-role's
// MaxSessionDuration, and the caller then runs the task credential-free. These
// pin the invariant so that changing defaultWallClockTimeout — which reads as a
// plain task-timeout constant — cannot silently break Bedrock.
func TestBedrockSessionSeconds_WithinStsAndRoleBounds(t *testing.T) {
	const stsMinSeconds = 900
	got := bedrockSessionSeconds()
	if got > bedrockMaxSessionSeconds {
		t.Errorf("session %ds exceeds the dr-bedrock-role ceiling of %ds — AssumeRole would fail outright", got, bedrockMaxSessionSeconds)
	}
	if got < stsMinSeconds {
		t.Errorf("session %ds is below the STS minimum of %ds", got, stsMinSeconds)
	}
}

func TestBedrockSessionSeconds_UsesFullTaskCapToday(t *testing.T) {
	// defaultWallClockTimeout is 4h and the role ceiling is also 14400, so the
	// clamp is a no-op right now and creds last exactly as long as the task may.
	// If this fails, the two have drifted apart — check whether the template's
	// MaxSessionDuration still matches bedrockMaxSessionSeconds.
	if got := bedrockSessionSeconds(); got != int32(defaultWallClockTimeout.Seconds()) {
		t.Errorf("bedrockSessionSeconds() = %d, want the full task cap %d", got, int32(defaultWallClockTimeout.Seconds()))
	}
}
