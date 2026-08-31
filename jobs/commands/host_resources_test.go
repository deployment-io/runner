package commands

import (
	"context"
	"testing"
)

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
