package commands

import (
	"os"
	"path/filepath"
	"testing"
)

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
		"turn_count": 3,
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
