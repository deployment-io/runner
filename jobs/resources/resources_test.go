package resources

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

// TestTryAcquireMemoryClampsOversizedRequests guards a liveness
// property. A request larger than the whole budget must be clamped and
// admitted alone, not refused: refused, it could never be satisfied on
// any poll, so an operator who set AGENTBOX_MEMORY_BYTES above what the
// host can back would get a job requeued forever that never runs, with
// nothing to explain why.
func TestTryAcquireMemoryClampsOversizedRequests(t *testing.T) {
	release, ok := TryAcquireMemory(1 << 62)
	if !ok {
		t.Fatal("an oversized request was refused; it would requeue forever and never run")
	}
	// It took the whole budget, so nothing else fits beside it.
	if _, alsoOK := TryAcquireMemory(1); alsoOK {
		t.Error("something was admitted alongside a request clamped to the whole budget")
	}
	release()
}

// TestTryAcquireMemoryReleasesCapacity checks the reserve/release cycle
// returns memory to the pool. A leaked reservation would progressively
// starve the runner until every job was requeued forever.
func TestTryAcquireMemoryReleasesCapacity(t *testing.T) {
	release, ok := TryAcquireMemory(memoryBudget())
	if !ok {
		t.Fatal("TryAcquireMemory failed on an idle pool")
	}
	if _, alsoOK := TryAcquireMemory(1); alsoOK {
		t.Error("admitted a job while the whole budget was held")
	}
	release()

	release2, ok := TryAcquireMemory(memoryBudget())
	if !ok {
		t.Fatal("capacity was not returned to the pool after release")
	}
	release2()
}

// TestTryAcquireMemoryDoesNotBlock is the regression test for the design
// this replaced. The old acquire waited for capacity, which held a runner
// worker for hours and eventually FAILED the job. It must now refuse
// immediately so the caller can hand the job back to the server instead.
func TestTryAcquireMemoryDoesNotBlock(t *testing.T) {
	release, ok := TryAcquireMemory(memoryBudget())
	if !ok {
		t.Fatal("could not drain the pool for the test")
	}
	defer release()

	done := make(chan bool, 1)
	go func() {
		_, admitted := TryAcquireMemory(memoryBudget())
		done <- admitted
	}()
	select {
	case admitted := <-done:
		if admitted {
			t.Error("TryAcquireMemory succeeded while the pool was fully held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TryAcquireMemory blocked; it must refuse immediately so the job can be requeued")
	}
}

// TestTryAcquireMemoryAdmitsWhatFits pins the counting itself: several
// jobs whose sizes sum inside the budget must all be admitted, and the
// one that would tip it over must not.
func TestTryAcquireMemoryAdmitsWhatFits(t *testing.T) {
	half := memoryBudget() / 2
	first, ok := TryAcquireMemory(half)
	if !ok {
		t.Fatal("first half-budget job was refused on an idle pool")
	}
	defer first()
	second, ok := TryAcquireMemory(half)
	if !ok {
		t.Fatal("second half-budget job was refused; two halves must fit")
	}
	defer second()
	if _, third := TryAcquireMemory(half); third {
		t.Error("a third half-budget job was admitted, exceeding the budget")
	}
}

// TestCapsAgainstBudget pins how the per-container caps relate to the
// budget they are all drawn from. The original suite checked each cap
// against the budget individually, which passes trivially and missed
// that the caps summed to more than the host — a single Task silently
// serializing the whole runner.
//
// It asserts the host-independent shape rather than numbers, so it stays
// meaningful anywhere: nothing exceeds the budget, and static-site builds
// stay under half of it so two concurrent deployments still fit. The
// heavy workloads take the WHOLE budget only while it is under their
// 8 GB ceiling — true on every instance we ship today, but not on a large
// one, where an 8 GB agent is a seventh of a 56 GB budget and plenty runs
// beside it. That split is logged, not asserted.
func TestCapsAgainstBudget(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "")
	t.Setenv(cpuCoresEnvVar, "")
	t.Setenv(buildMemoryBytesEnvVar, "")
	t.Setenv(imageBuildMemoryBytesEnvVar, "")

	budget := memoryBudget()

	agentMem, _ := LimitsForAgentContainer()
	imageMem, _ := LimitsForImageBuild()
	staticMem, _ := LimitsForStaticSiteBuild()

	// No cap may exceed the budget. Such a job is clamped and so runs, but
	// it would silently be reserved for less than its container is allowed
	// to use — the over-commit this whole mechanism exists to prevent.
	for _, tc := range []struct {
		name  string
		bytes int64
	}{
		{"agentbox", agentMem},
		{"image build", imageMem},
		{"static build", staticMem},
	} {
		if tc.bytes > budget {
			t.Errorf("%s cap %d exceeds the budget %d", tc.name, tc.bytes, budget)
		}
		if tc.bytes < 1 {
			t.Errorf("%s cap %d must be positive", tc.name, tc.bytes)
		}
	}

	// Static-site builds are the parallel workload, so two should fit —
	// but only on a host where the DIVISOR decides the cap. Below roughly
	// 6 GB of host, budget/2 falls under buildMemoryFloorBytes and
	// clampMemory raises the cap back to the floor; two floor-sized builds
	// then exceed the budget, correctly, and admission runs one. Asserting
	// unconditionally would fail on an 8 GB laptop or a small CI container
	// for a sizing decision that is right. Same guard as the margin check
	// below and as TestJobMemoryBytes.
	if budget >= 2*buildMemoryFloorBytes && staticMem*2 > budget {
		t.Errorf("static build cap %d is more than half the budget %d — two concurrent "+
			"deployments would no longer fit", staticMem, budget)
	}

	// The cap must clear the one memory figure we have actually MEASURED.
	// run_agent_step.go records our own dashboard — a Vite static site, so
	// it builds in exactly this container — being OOM-killed at 2 GB during
	// chunk rendering. A cap at or below that is known to fail a real
	// build, so sizing must stay above it wherever the host can afford it.
	// A MARGIN above it, not merely above it: a cap 3% over a figure known
	// to kill a real build is not an improvement, it is the same failure
	// with rounding. Require at least 1.5x, and only on a host whose budget
	// can seat two builds that size — smaller hosts legitimately fall back
	// to the floor.
	const (
		observedStaticBuildOOM = 2 * 1024 * 1024 * 1024
		requiredStaticHeadroom = observedStaticBuildOOM * 3 / 2
	)
	if budget >= 2*requiredStaticHeadroom && staticMem < requiredStaticHeadroom {
		t.Errorf("static build cap %d leaves no margin over the %d that OOM-killed a real "+
			"build; want at least %d on a host with budget %d",
			staticMem, observedStaticBuildOOM, requiredStaticHeadroom, budget)
	}

	// Documents the accepted trade-off rather than asserting against it:
	// on a host whose whole budget goes to one agent container, nothing
	// runs beside it. If this ever stops being true the comments in
	// admission.go and the PR description need revisiting.
	t.Logf("budget=%d MB; agentbox=%d image=%d static=%d; room beside an agent job: %d MB",
		budget/(1024*1024), agentMem/(1024*1024), imageMem/(1024*1024),
		staticMem/(1024*1024), (budget-agentMem)/(1024*1024))
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
	agentMem, _ := LimitsForAgentContainer()
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
	sessionMem := MemoryForAssistantSession()
	session := JobMemoryBytes([]commands_enums.Type{
		commands_enums.CheckoutRepo, commands_enums.RunAssistantSession,
	})
	if session != sessionMem {
		t.Errorf("session = %d, want the session cap %d", session, sessionMem)
	}
	// The next two only mean anything on a host big enough for the clamps
	// not to collapse. Below roughly 2.5 GB every cap bottoms out at
	// minContainerMemoryBytes, so a session and a Step are legitimately
	// equal and a session legitimately does take the whole pool — that is
	// the honest consequence of a tiny host, not a regression, and
	// asserting it would just make this test fail on small CI boxes.
	if memoryBudget() <= sessionMemoryCeilingBytes {
		t.Skipf("host budget %d is too small to distinguish the caps", memoryBudget())
	}
	if session >= step {
		t.Errorf("session cap %d is not smaller than the task step cap %d", session, step)
	}
	// It must also leave room for something else to run alongside it.
	if session >= memoryBudget() {
		t.Errorf("a session takes the whole budget (%d of %d); deploys would queue behind it for hours",
			session, memoryBudget())
	}
}

// TestResolveContainerLimits_DerivedFromHost pins the property that
// replaced the old flat 4 GB constant: with no override, the cap comes
// from the host and always lands inside the floor/ceiling band. The
// exact value is machine-dependent (CI, a laptop and an m6a.large all
// differ), so this asserts the invariants rather than a number.
//
// The floor is the important half. It is the pre-existing 4 GB default,
// and deriving from the host must never hand a Task LESS memory than it
// had before this change.
func TestResolveContainerLimits_DerivedFromHost(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "")
	t.Setenv(cpuCoresEnvVar, "")
	mem, nano := LimitsForAgentContainer()

	if want := clampMemory(memoryBudget(), agentboxMemoryFloorBytes, agentboxMemoryCeilingBytes); mem != want {
		t.Errorf("memory = %d, want host-derived %d", mem, want)
	}
	if mem > agentboxMemoryCeilingBytes {
		t.Errorf("memory %d exceeds ceiling %d", mem, agentboxMemoryCeilingBytes)
	}
	// Only assert the floor when the host can actually back it — a small
	// dev machine legitimately lands below it (clampMemory prefers a cap
	// the machine can honour over a floor it cannot).
	if memoryBudget() >= agentboxMemoryFloorBytes && mem < agentboxMemoryFloorBytes {
		t.Errorf("memory %d regressed below the previous default %d on a host with budget %d",
			mem, agentboxMemoryFloorBytes, memoryBudget())
	}
	if nano != hostCPUCores()*1_000_000_000 {
		t.Errorf("nanoCPUs = %d, want host cores %d", nano, hostCPUCores()*1_000_000_000)
	}
}

// TestResolveContainerLimits_NeverExceedsBudget is the regression test
// for the bug this change set out to fix: caps that summed to more than
// the machine had. No single container may be sized above the whole
// budget, whatever the host reports.
func TestResolveContainerLimits_NeverExceedsBudget(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "")
	t.Setenv(cpuCoresEnvVar, "")
	agentMem, _ := LimitsForAgentContainer()
	buildMem, _ := LimitsForStaticSiteBuild()
	imageMem, _ := LimitsForImageBuild()
	budget := memoryBudget()

	for _, tc := range []struct {
		name string
		mem  int64
	}{
		{"agentbox", agentMem},
		{"static build", buildMem},
		{"image build", imageMem},
	} {
		if tc.mem > budget {
			t.Errorf("%s cap %d exceeds the host budget %d", tc.name, tc.mem, budget)
		}
	}
	// The budget itself must leave the host something to run on.
	if budget >= hostMemoryBytes() {
		t.Errorf("budget %d leaves no reserve out of host memory %d", budget, hostMemoryBytes())
	}
}

func TestResolveContainerLimits_EnvOverride(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "4294967296") // 4 GB
	t.Setenv(cpuCoresEnvVar, "4")
	mem, nano := LimitsForAgentContainer()
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
	mem, nano := LimitsForAgentContainer()
	if want := clampMemory(memoryBudget(), agentboxMemoryFloorBytes, agentboxMemoryCeilingBytes); mem != want {
		t.Errorf("memory with invalid env = %d, want host-derived %d", mem, want)
	}
	if nano != hostCPUCores()*1_000_000_000 {
		t.Errorf("nanoCPUs with invalid env = %d, want host cores %d", nano, hostCPUCores()*1_000_000_000)
	}
}

// TestResolveContainerLimits_NegativeEnvFallsBack covers the explicit-zero
// and negative-value cases — Docker treats Memory=0 as "no limit" but we
// want the fallback to apply (someone setting AGENTBOX_MEMORY_BYTES=0
// almost certainly didn't mean "unlimited").
func TestResolveContainerLimits_NegativeEnvFallsBack(t *testing.T) {
	t.Setenv(memoryBytesEnvVar, "-1")
	t.Setenv(cpuCoresEnvVar, "0")
	mem, nano := LimitsForAgentContainer()
	if want := clampMemory(memoryBudget(), agentboxMemoryFloorBytes, agentboxMemoryCeilingBytes); mem != want {
		t.Errorf("memory with negative env = %d, want host-derived %d", mem, want)
	}
	if nano != hostCPUCores()*1_000_000_000 {
		t.Errorf("nanoCPUs with zero env = %d, want host cores %d", nano, hostCPUCores()*1_000_000_000)
	}
}
