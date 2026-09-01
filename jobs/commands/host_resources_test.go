package commands

import (
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
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

// TestAdmissionWeightClampedToCapacity guards a liveness property rather
// than a sizing one. Acquire does not deadlock on an oversized request —
// its `n > s.size` branch parks on the context — but it can never be
// satisfied either, so an operator who sets AGENTBOX_MEMORY_BYTES larger
// than the host would get a job that waits out the whole admission
// timeout and then fails. Clamping makes it run alone instead.
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

// TestTryAcquireMemoryReleasesCapacity checks the reserve/release cycle
// returns memory to the pool. A leaked slot would progressively starve
// the runner until every job was requeued forever.
func TestTryAcquireMemoryReleasesCapacity(t *testing.T) {
	sem, capacity := admissionSemaphore()

	release, ok := TryAcquireMemory(memoryBudget())
	if !ok {
		t.Fatal("TryAcquireMemory failed on an idle pool")
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

// TestTryAcquireMemoryDoesNotBlock is the regression test for the design
// this replaced. The old acquire waited for capacity, which held a runner
// worker for hours and eventually FAILED the job. It must now refuse
// immediately so the caller can hand the job back to the server instead.
func TestTryAcquireMemoryDoesNotBlock(t *testing.T) {
	sem, capacity := admissionSemaphore()
	if !sem.TryAcquire(capacity) {
		t.Fatal("could not drain the semaphore for the test")
	}
	defer sem.Release(capacity)

	done := make(chan bool, 1)
	go func() {
		_, ok := TryAcquireMemory(memoryBudget())
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("TryAcquireMemory succeeded while the pool was fully held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TryAcquireMemory blocked; it must refuse immediately so the job can be requeued")
	}
}

// TestAdmissionWeightsAgainstCapacity is the test that was missing when
// the sizing landed. The original suite only checked each CAP against the
// budget, which passes trivially, and so never noticed that the derived
// caps convert into admission WEIGHTS equal to 100% of capacity — meaning
// a single Task silently serialized the entire runner.
//
// It asserts the real, intended shape rather than a number, so it stays
// meaningful on any host. Note the heavy workloads take the WHOLE pool
// only while the budget is under their 8 GB ceiling, which is the case on
// every instance we ship today but not on a large one: at a 56 GB budget
// an 8 GB agent is a seventh of capacity and plenty runs beside it. So
// the assertions here are the host-independent ones — nothing exceeds
// capacity, and static-site builds stay small enough that at least two
// run together, since they are the workload that legitimately fans out
// across concurrent deployments. The actual split is logged, not
// asserted.
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

// TestJobMemoryBytes pins the dispatcher's view of what a job needs.
// Command sequences that spawn no container MUST come back as zero, or a
// busy runner would start requeuing plain AWS API calls — the exact
// over-throttling that gating by worker count would have caused.
func TestJobMemoryBytes(t *testing.T) {
	if got := JobMemoryBytes([]commands_enums.Type{
		commands_enums.CreateAwsVpc, commands_enums.VerifyAcmCertificate, commands_enums.CommitAndPush,
	}); got != 0 {
		t.Errorf("container-free sequence = %d, want 0 so it is never gated", got)
	}
	if got := JobMemoryBytes(nil); got != 0 {
		t.Errorf("empty sequence = %d, want 0", got)
	}

	// A Task Step: the peak across its sequence is the agent container.
	agentMem, _ := resolveContainerLimits()
	step := JobMemoryBytes([]commands_enums.Type{
		commands_enums.CheckoutRepo, commands_enums.MaterializeContext,
		commands_enums.RunAgentStep, commands_enums.CommitAndPush, commands_enums.OpenPullRequest,
	})
	if step != agentMem {
		t.Errorf("task step = %d, want the agent cap %d", step, agentMem)
	}

	// A session is sized for analysis, not a build, so it must ask for
	// materially less than a Step — otherwise one interactive session
	// reserves the whole host for hours and blocks every deploy.
	sessionMem, _ := resolveSessionLimits()
	session := JobMemoryBytes([]commands_enums.Type{
		commands_enums.CheckoutRepo, commands_enums.RunAssistantSession,
	})
	if session != sessionMem {
		t.Errorf("session = %d, want the session cap %d", session, sessionMem)
	}
	if session >= step {
		t.Errorf("session cap %d is not smaller than the task step cap %d", session, step)
	}
	// It must also leave room for something else to run alongside it.
	_, capacity := admissionSemaphore()
	if admissionWeight(session) >= capacity {
		t.Errorf("a session takes the whole pool (%d of %d units); deploys would queue behind it for hours",
			admissionWeight(session), capacity)
	}
}
