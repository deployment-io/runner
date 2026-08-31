package commands

import (
	"strings"
	"testing"
	"time"
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

	release, err := acquireMemory(nil, memoryBudget(), "test container", nil)
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

// TestAdmissionWeightsAgainstCapacity is the test that was missing when
// the sizing landed. The original suite only checked each CAP against the
// budget, which passes trivially, and so never noticed that the derived
// caps convert into admission WEIGHTS equal to 100% of capacity — meaning
// a single Task silently serialized the entire runner.
//
// It asserts the real, intended shape rather than a number, so it stays
// meaningful on any host: the two heavy workloads are expected to take
// the whole pool (that IS what "may use all the memory the host can
// spare" means), while static-site builds must remain small enough that
// at least two run together — they are the workload that legitimately
// fans out across concurrent deployments.
func TestAdmissionWeightsAgainstCapacity(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "")
	t.Setenv(cpuCoresEnvVar, "")
	t.Setenv(buildMemoryBytesEnvVar, "")
	t.Setenv(imageBuildMemoryBytesEnvVar, "")

	_, capacity := admissionSemaphore()

	agentMem, _ := resolveContainerLimits()
	imageMem, _ := resolveImageBuildLimits()
	staticMem, _ := resolveBuildLimits()

	agentWeight := admissionWeight(agentMem)
	imageWeight := admissionWeight(imageMem)
	staticWeight := admissionWeight(staticMem)

	// No weight may exceed the pool, or Acquire would block forever.
	for _, tc := range []struct {
		name   string
		weight int64
	}{
		{"agentbox", agentWeight},
		{"image build", imageWeight},
		{"static build", staticWeight},
	} {
		if tc.weight > capacity {
			t.Errorf("%s weight %d exceeds capacity %d — Acquire would never be satisfied",
				tc.name, tc.weight, capacity)
		}
		if tc.weight < 1 {
			t.Errorf("%s weight %d must be at least one unit", tc.name, tc.weight)
		}
	}

	// Static-site builds are the parallel workload; if they ever grow to
	// more than half the pool, concurrent deployments silently serialize.
	if staticWeight*2 > capacity {
		t.Errorf("static build weight %d is more than half of capacity %d — two concurrent "+
			"deployments would no longer fit", staticWeight, capacity)
	}

	// Documents the accepted trade-off rather than asserting against it:
	// on a host whose whole budget goes to one agent container, nothing
	// runs beside it. If this ever stops being true the comments in
	// admission.go and the PR description need revisiting.
	t.Logf("capacity=%d units; agentbox=%d image=%d static=%d; room beside an agent job: %d units",
		capacity, agentWeight, imageWeight, staticWeight, capacity-agentWeight)
}

// TestAcquireMemoryHonoursStopSignal is the regression test for the
// admission wait ignoring cancellation. Every call site used to pass
// context.Background(), so a user pressing Stop on a queued job could not
// interrupt it: the job held a worker until the admission timeout and
// then failed with a resource error instead of reporting as stopped.
func TestAcquireMemoryHonoursStopSignal(t *testing.T) {
	sem, capacity := admissionSemaphore()
	if !sem.TryAcquire(capacity) {
		t.Fatal("could not drain the semaphore for the test")
	}
	defer sem.Release(capacity)

	stop := make(chan struct{})
	close(stop)

	done := make(chan error, 1)
	go func() {
		_, err := acquireMemory(stop, memoryBudget(), "test container", nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("acquireMemory succeeded while the pool was fully held")
		}
		if !strings.Contains(err.Error(), "stopped") {
			t.Errorf("error = %q, want it to report the stop rather than a resource timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("acquireMemory ignored the stop signal and kept waiting")
	}
}

// TestAcquireMemoryNilStopIsSafe guards the default for commands that
// never opt into StoppableCommand: a nil channel must simply mean "not
// stoppable", not panic or fire immediately.
func TestAcquireMemoryNilStopIsSafe(t *testing.T) {
	release, err := acquireMemory(nil, minContainerMemoryBytes, "test container", nil)
	if err != nil {
		t.Fatalf("acquireMemory with a nil stop channel: %v", err)
	}
	release()
}
