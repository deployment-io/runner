package commands

import "testing"

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
