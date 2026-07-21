package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// Without the subscription marker (the default — org is on AnthropicDirect),
// the injected API key must survive untouched.
func TestSubscriptionAuth_DisabledByDefault(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-xyz", "AGENT_TYPE": "claude-code"}
	maybeApplyClaudeSubscriptionAuth(env, "", io.Discard)
	if env["ANTHROPIC_API_KEY"] != "sk-ant-api-xyz" {
		t.Errorf("API key was modified while subscription auth disabled: %q", env["ANTHROPIC_API_KEY"])
	}
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Error("OAuth token set while subscription auth disabled")
	}
}

// The marker is a control-plane signal, not part of the agentbox contract — it
// must be consumed here and never reach the container env.
func TestSubscriptionAuth_MarkerNotLeakedToContainer(t *testing.T) {
	env := map[string]string{
		claudeAuthModeEnvVar: claudeAuthModeSubscription,
		"ANTHROPIC_API_KEY":  "sk-ant-api-xyz",
		"AGENT_TYPE":         "codex", // returns before any AWS call
	}
	maybeApplyClaudeSubscriptionAuth(env, "", io.Discard)
	if _, ok := env[claudeAuthModeEnvVar]; ok {
		t.Errorf("%s leaked into the container env", claudeAuthModeEnvVar)
	}
}

// Even in subscription mode, non-Claude-Code agents (codex/opencode) must be
// left on their injected API key — the function returns before any Secrets
// Manager lookup, so this test makes no AWS calls.
func TestSubscriptionAuth_ClaudeCodeOnly(t *testing.T) {
	env := map[string]string{
		claudeAuthModeEnvVar: claudeAuthModeSubscription,
		"OPENAI_API_KEY":     "sk-openai",
		"AGENT_TYPE":         "codex",
	}
	maybeApplyClaudeSubscriptionAuth(env, "", io.Discard)
	if env["OPENAI_API_KEY"] != "sk-openai" {
		t.Errorf("codex key was modified: %q", env["OPENAI_API_KEY"])
	}
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Error("OAuth token set for codex agent (Claude Code only)")
	}
}

func TestResolveContainerLimits_Defaults(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "")
	t.Setenv(cpuCoresEnvVar, "")
	mem, nano := resolveContainerLimits()
	if mem != defaultMemoryBytes {
		t.Errorf("memory = %d, want default %d", mem, defaultMemoryBytes)
	}
	if nano != defaultCPUCores*1_000_000_000 {
		t.Errorf("nanoCPUs = %d, want default %d", nano, defaultCPUCores*1_000_000_000)
	}
}

func TestResolveContainerLimits_EnvOverride(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "4294967296") // 4 GB
	t.Setenv(cpuCoresEnvVar, "4")
	mem, nano := resolveContainerLimits()
	if mem != 4294967296 {
		t.Errorf("memory = %d, want 4294967296 (4 GB)", mem)
	}
	if nano != 4_000_000_000 {
		t.Errorf("nanoCPUs = %d, want 4e9 (4 cores)", nano)
	}
}

// TestResolveContainerLimits_InvalidEnvFallsBack ensures malformed env
// values don't break Step Job execution. A garbage string in the env
// var falls back to the default rather than producing nonsense limits
// (or, worse, NaN which Docker would reject).
func TestResolveContainerLimits_InvalidEnvFallsBack(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "not-a-number")
	t.Setenv(cpuCoresEnvVar, "abc")
	mem, nano := resolveContainerLimits()
	if mem != defaultMemoryBytes {
		t.Errorf("memory with invalid env = %d, want default %d", mem, defaultMemoryBytes)
	}
	if nano != defaultCPUCores*1_000_000_000 {
		t.Errorf("nanoCPUs with invalid env = %d, want default %d", nano, defaultCPUCores*1_000_000_000)
	}
}

// TestResolveContainerLimits_NegativeEnvFallsBack covers the explicit-zero
// and negative-value cases — Docker treats Memory=0 as "no limit" but we
// want the fallback to apply (someone setting AGENTBOX_MEMORY_BYTES=0
// almost certainly didn't mean "unlimited").
func TestResolveContainerLimits_NegativeEnvFallsBack(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "-1")
	t.Setenv(cpuCoresEnvVar, "0")
	mem, nano := resolveContainerLimits()
	if mem != defaultMemoryBytes {
		t.Errorf("memory with negative env = %d, want default %d", mem, defaultMemoryBytes)
	}
	if nano != defaultCPUCores*1_000_000_000 {
		t.Errorf("nanoCPUs with zero env = %d, want default %d", nano, defaultCPUCores*1_000_000_000)
	}
}

// TestReadAgentResult_TokenUsageObjectShape regression-tests the runner's
// ability to parse agentbox's /result.json. Agentbox has emitted
// token_usage as an object (not an int) since v1.1.0; the runner's
// earlier int64 typing failed at first-ever Tasks Step result-read with
// "cannot unmarshal object into Go struct field agentResult.token_usage
// of type int64". This test pins the object-shape contract so the shape
// can't silently drift back.
func TestReadAgentResult_TokenUsageObjectShape(t *testing.T) {
	workDir := t.TempDir()
	resultDir := filepath.Join(workDir, agentboxResultDirRel)
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("mkdir result dir: %v", err)
	}
	// Matches the on-the-wire shape from agentbox v1.1.0+
	// (internal/result/result.go::Outcome + TokenUsage).
	payload := `{
		"schema_version": 1,
		"status": "success",
		"exit_code": 0,
		"agent_type": "claude-code",
		"agent_version": "2.1.141",
		"changes_summary": "edited 1 file",
		"turns": 3,
		"token_usage": {
			"input_tokens": 1234,
			"output_tokens": 567,
			"cache_read_tokens": 89
		},
		"denied_hosts": ["evil.example.com"]
	}`
	resultPath := filepath.Join(resultDir, agentboxResultFile)
	if err := os.WriteFile(resultPath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write result.json: %v", err)
	}
	got, err := readAgentResult(workDir)
	if err != nil {
		t.Fatalf("readAgentResult returned error: %v", err)
	}
	if got.Status != "success" {
		t.Errorf("status = %q, want %q", got.Status, "success")
	}
	if got.Turns != 3 {
		t.Errorf("Turns = %d, want 3", got.Turns)
	}
	if got.TokenUsage.InputTokens != 1234 {
		t.Errorf("InputTokens = %d, want 1234", got.TokenUsage.InputTokens)
	}
	if got.TokenUsage.OutputTokens != 567 {
		t.Errorf("OutputTokens = %d, want 567", got.TokenUsage.OutputTokens)
	}
	if got.TokenUsage.CacheReadTokens != 89 {
		t.Errorf("CacheReadTokens = %d, want 89", got.TokenUsage.CacheReadTokens)
	}
	if len(got.DeniedHosts) != 1 || got.DeniedHosts[0] != "evil.example.com" {
		t.Errorf("DeniedHosts = %v, want [evil.example.com]", got.DeniedHosts)
	}
}

// TestFormatAgentFailure_IncludesError pins the requirement that the
// runner surfaces agentbox's result.Error message in the failure
// returned upstream. Pre-v0.1.24 the runner only printed status +
// exit_code, which made debugging Claude API auth failures (and
// every other agentbox-classified failure) impossible without SSHing
// into the runner and reading result.json by hand.
func TestFormatAgentFailure_IncludesError(t *testing.T) {
	got := formatAgentFailure(agentResult{
		Status: "failure",
		Error:  "claude exited with error: API key invalid",
	}).Error()
	if !strings.Contains(got, `status=failure`) {
		t.Errorf("error %q missing status field", got)
	}
	if !strings.Contains(got, `error="claude exited with error: API key invalid"`) {
		t.Errorf("error %q missing agentbox-side error detail", got)
	}
}

// TestFormatAgentFailure_EmptyErrorStillReadable covers the edge case
// where agentbox writes an empty Error (shouldn't happen on the failure
// path, but the format must not panic or produce a confusing message).
func TestFormatAgentFailure_EmptyErrorStillReadable(t *testing.T) {
	got := formatAgentFailure(agentResult{Status: "failure"}).Error()
	if !strings.Contains(got, `status=failure`) {
		t.Errorf("error %q missing status field", got)
	}
	if !strings.Contains(got, `error=""`) {
		t.Errorf("error %q should include empty-error sentinel for diagnosability, got: %s", got, got)
	}
}

// TestReadAgentResult_ZeroValueTokenUsage ensures the zero-value
// agentbox emits on early-exit paths (where no tokens were consumed)
// also parses cleanly — was the path the v1.1.0 → runner-int64 bug
// originally tripped on.
func TestReadAgentResult_ZeroValueTokenUsage(t *testing.T) {
	workDir := t.TempDir()
	resultDir := filepath.Join(workDir, agentboxResultDirRel)
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("mkdir result dir: %v", err)
	}
	payload := `{
		"schema_version": 1,
		"status": "failure",
		"exit_code": 243,
		"error": "agent install failed",
		"token_usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0}
	}`
	resultPath := filepath.Join(resultDir, agentboxResultFile)
	if err := os.WriteFile(resultPath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write result.json: %v", err)
	}
	got, err := readAgentResult(workDir)
	if err != nil {
		t.Fatalf("readAgentResult returned error: %v", err)
	}
	if got.ExitCode != 243 {
		t.Errorf("ExitCode = %d, want 243", got.ExitCode)
	}
	if got.TokenUsage != (tokenUsage{}) {
		t.Errorf("TokenUsage = %+v, want zero value", got.TokenUsage)
	}
}

// TestMergeAgentResultIntoJobOutput_TurnsCarryThrough pins that the
// turn count read from agentbox's result.json lands on the JobOutput
// envelope's agent block. Earlier the field existed on agentResult
// but was dropped on the merge — the dashboard then rendered "Turn 0"
// for completed runs that finished too fast for the LiveProgress
// heartbeat to land. Mirrors the carry-through assertion the
// app-server side relies on in extractAgentTurns.
func TestMergeAgentResultIntoJobOutput_TurnsCarryThrough(t *testing.T) {
	params := map[string]interface{}{}
	err := mergeAgentResultIntoJobOutput(params, agentResult{
		Status:         "success",
		ChangesSummary: "edited 1 file",
		Turns:          7,
		TokenUsage:     tokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 25},
	})
	if err != nil {
		t.Fatalf("mergeAgentResultIntoJobOutput: %v", err)
	}
	raw, err := jobs.GetParameterValue[string](params, parameters_enums.JobOutput)
	if err != nil {
		t.Fatalf("GetParameterValue: %v", err)
	}
	var got jobOutputData
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if got.Agent == nil {
		t.Fatal("agent block missing")
	}
	if got.Agent.Turns != 7 {
		t.Errorf("agent.turns = %d, want 7", got.Agent.Turns)
	}
	if got.Agent.TokenUsage.InputTokens != 100 {
		t.Errorf("agent.token_usage.input_tokens = %d, want 100", got.Agent.TokenUsage.InputTokens)
	}
}

// isAccessDenied must fire only on authorization failures. A missing secret is
// the customer not having created it yet — self-granting wouldn't help, and
// treating it as a denial would write IAM on every task of a misconfigured org.
func TestIsAccessDenied(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"access denied", &smithy.GenericAPIError{Code: "AccessDeniedException"}, true},
		{"access denied short form", &smithy.GenericAPIError{Code: "AccessDenied"}, true},
		{"unauthorized operation", &smithy.GenericAPIError{Code: "UnauthorizedOperation"}, true},
		{"secret not found", &smithy.GenericAPIError{Code: "ResourceNotFoundException"}, false},
		{"throttled", &smithy.GenericAPIError{Code: "ThrottlingException"}, false},
		{"wrapped access denied", fmt.Errorf("reading secret: %w",
			&smithy.GenericAPIError{Code: "AccessDeniedException"}), true},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAccessDenied(tt.err); got != tt.want {
				t.Errorf("isAccessDenied(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
