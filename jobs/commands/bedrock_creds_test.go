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
