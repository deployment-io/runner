package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestReadMemTotalBytes covers the parse that everything else is built
// on. A misread here would silently mis-size every container on the
// runner by orders of magnitude, so malformed input must return 0 (which
// makes the caller apply its 8 GB fallback) rather than a wrong number.
func TestReadMemTotalBytes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int64
	}{
		{
			name:    "typical meminfo",
			content: "MemTotal:       16116496 kB\nMemFree:         1234 kB\n",
			want:    16116496 * 1024,
		},
		{
			name:    "MemTotal not first line",
			content: "SwapTotal:      0 kB\nMemTotal:       8046736 kB\n",
			want:    8046736 * 1024,
		},
		{
			name: "missing MemTotal",
			// A procfs without the line at all must not be guessed at.
			content: "MemFree:         1234 kB\n",
			want:    0,
		},
		{
			name: "unexpected unit",
			// Refusing an unknown unit is the whole point: silently
			// treating an mB value as kB would be a 1000x error.
			content: "MemTotal:       16116496 mB\n",
			want:    0,
		},
		{
			name:    "no unit suffix",
			content: "MemTotal:       16116496\n",
			want:    0,
		},
		{
			name:    "non-numeric value",
			content: "MemTotal:       lots kB\n",
			want:    0,
		},
		{
			name:    "zero value",
			content: "MemTotal:       0 kB\n",
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write meminfo: %v", err)
			}
			if got := readMemTotalBytes(path); got != tc.want {
				t.Errorf("readMemTotalBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadMemTotalBytes_MissingFile(t *testing.T) {
	if got := readMemTotalBytes(filepath.Join(t.TempDir(), "does-not-exist")); got != 0 {
		t.Errorf("readMemTotalBytes(missing) = %d, want 0 so the caller falls back", got)
	}
}

// TestMemoryBudgetLeavesReserve is the invariant that separates a clean
// per-job cgroup OOM kill from a host OOM kill that can take out dockerd
// or the runner itself: the budget must never be the whole machine.
func TestMemoryBudgetLeavesReserve(t *testing.T) {
	host := hostMemoryBytes()
	budget := memoryBudget()
	if budget >= host {
		t.Fatalf("budget %d does not leave a reserve out of host %d", budget, host)
	}
	if reserve := host - budget; reserve < hostMemoryReserveBytes {
		t.Errorf("reserve %d is below the %d floor", reserve, hostMemoryReserveBytes)
	}
}

func TestClampMemory(t *testing.T) {
	budget := memoryBudget()

	// Within the band, the requested value passes through.
	if got := clampMemory(budget, minContainerMemoryBytes, budget); got != budget {
		t.Errorf("clampMemory(budget) = %d, want %d", got, budget)
	}
	// Below the floor is raised to it.
	if got := clampMemory(1, minContainerMemoryBytes, budget); got != minContainerMemoryBytes {
		t.Errorf("clampMemory(tiny) = %d, want floor %d", got, minContainerMemoryBytes)
	}
	// A ceiling above the budget is itself clamped to the budget, so an
	// absurd request can never be sized past what the host has.
	if got := clampMemory(1<<62, minContainerMemoryBytes, 1<<62); got != budget {
		t.Errorf("clampMemory(huge) = %d, want budget %d", got, budget)
	}
}

// TestAdmissionWeightClampedToCapacity guards against a deadlock rather
// than a sizing error. semaphore.Weighted.Acquire blocks forever when
// asked for more than the total capacity, so an operator who sets
// AGENTBOX_MEMORY_BYTES larger than the host must get a job that runs
// alone, not a job that waits for memory that will never exist.
func TestAdmissionWeightClampedToCapacity(t *testing.T) {
	_, capacity := admissionSemaphore()
	if got := admissionWeight(1 << 62); got != capacity {
		t.Errorf("admissionWeight(absurd) = %d, want capacity %d", got, capacity)
	}
	if got := admissionWeight(1); got < 1 {
		t.Errorf("admissionWeight(1 byte) = %d, want at least 1 unit", got)
	}
	// Rounds UP, so a container is never admitted against less memory
	// than it may actually use.
	if got := admissionWeight(admissionUnitBytes + 1); got != 2 {
		t.Errorf("admissionWeight(one unit + 1 byte) = %d, want 2", got)
	}
}

// TestAcquireMemoryReleasesCapacity checks the acquire/release cycle
// actually returns memory to the pool. A leaked slot would progressively
// starve the runner until it could admit nothing at all.
func TestAcquireMemoryReleasesCapacity(t *testing.T) {
	sem, capacity := admissionSemaphore()

	release, err := acquireMemory(context.Background(), memoryBudget(), "test container", nil)
	if err != nil {
		t.Fatalf("acquireMemory: %v", err)
	}
	// Whole budget held: nothing else of that size fits.
	if sem.TryAcquire(capacity) {
		t.Error("acquired the full capacity while it was already held")
	}
	release()
	if !sem.TryAcquire(capacity) {
		t.Error("capacity was not returned to the pool after release")
	}
	sem.Release(capacity)
}

// TestAcquireMemoryHonoursCancelledContext ensures a stopped or
// cancelled job stops queueing immediately instead of holding a worker
// for the full admission timeout.
func TestAcquireMemoryHonoursCancelledContext(t *testing.T) {
	sem, capacity := admissionSemaphore()
	if !sem.TryAcquire(capacity) {
		t.Fatal("could not drain the semaphore for the test")
	}
	defer sem.Release(capacity)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := acquireMemory(ctx, memoryBudget(), "test container", nil); err == nil {
		t.Error("acquireMemory succeeded on a cancelled context")
	}
}
