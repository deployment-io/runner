package commands

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// Memory admission control for container-spawning jobs.
//
// WHY THIS EXISTS
//
// Sizing each container from the host (host_resources.go) bounds any
// SINGLE container, but says nothing about what happens when several run
// at once — and the runner is built to run several at once. The job
// dispatcher is a 3-lane concurrent pipeline where each lane runs a
// 5-worker pool (entrypoints/common/common.go), so up to 15 jobs can be
// in flight, and nothing in that path is aware of memory. Fifteen jobs
// that each create a container sized for the whole host is fifteen times
// the host.
//
// The naive fix — make the worker count a function of RAM — is wrong,
// because job footprints differ by three orders of magnitude. Of the 32
// command types the runner implements, most are AWS API calls that need
// essentially no memory (CreateAwsVpc, VerifyAcmCertificate,
// GetDeploymentLogsAws, ...). Throttling those to protect against an
// agentbox container would tank deploy latency for no reason at all.
//
// So concurrency is gated by WEIGHT rather than by count: only the jobs
// that actually create a container acquire memory, and they acquire the
// amount they were sized for. Everything else runs unimpeded, and on a
// larger instance the same code admits proportionally more with no
// retuning.
//
// What that means concretely on the shipped m6a.large (2 vCPU, 8 GB):
// two static-site builds fit together, but an agent container or an
// image build is sized to the whole budget and so runs strictly alone.
// See consequence 1 below — this was originally described here as "one
// Task at a time while several builds still run in parallel", which was
// wrong: the arithmetic leaves zero units beside an agent job.
//
// This is also what makes the generous per-container sizing safe. Giving
// agentbox the whole budget would be reckless if two could overlap;
// admission control is the guarantee that they can't.
//
// TWO CONSEQUENCES WORTH KNOWING, both deliberate:
//
//  1. On the smallest instance we ship (m6a.large, ~7.7 GB) an agent
//     container is sized to the ENTIRE budget, so its weight is 100% of
//     capacity and nothing runs beside it. That is not an accident of the
//     arithmetic — it is what "the agent may use all the memory the host
//     can spare" means, and on a host that size the alternative is to
//     shrink the agent back below the 4 GB that was already OOM-killing
//     Tasks. Concurrency on a small runner is the thing being traded
//     away; a larger instance buys it back with no code change, because
//     both the caps and the capacity scale with the host.
//
//  2. golang.org/x/sync/semaphore is strict FIFO and deliberately leaves
//     every waiter blocked behind one that does not fit (see the comment
//     in its notifyWaiters). So a queued full-budget agent request stalls
//     a small build that would otherwise fit. This is the RIGHT tradeoff
//     and is not worked around: allowing small jobs to barge ahead would
//     starve the large one indefinitely on a busy runner, turning a
//     bounded delay into a job that never runs at all. It does mean the
//     admission wait must be long enough to outlast the job in front —
//     see admissionWaitTimeout.

const (
	// admissionWaitTimeout bounds how long a job waits for memory before
	// giving up. Waiting is the CORRECT behaviour here: a deployment that
	// starts late is a far better outcome than one that fails, or than two
	// jobs that start at once and are both OOM-killed.
	//
	// Sized to outlast a realistic agentbox Task rather than a "typical"
	// one. On the smallest instance we ship, an agent container is sized
	// to the whole budget (see host_resources.go), so a build genuinely
	// cannot start until the Task finishes — and at the previous 30m this
	// turned every deploy dispatched during a longer Task into a HARD
	// FAILURE rather than a delay.
	//
	// Note this does NOT sit inside some larger per-job envelope: there is
	// no runner-wide job wall-clock. The 4h defaultWallClockTimeout in
	// run_agent_step.go bounds the agent CONTAINER once it starts, and
	// defaultBuildTimeout bounds a build the same way, both AFTER
	// admission. So this timeout adds to a job's total time rather than
	// fitting within it, which is the reason it is 2h and not longer: it
	// has to outlast the job in front without letting a job that will
	// never be admitted wedge a worker indefinitely.
	admissionWaitTimeout = 2 * time.Hour
)

var (
	admissionOnce sync.Once
	admissionSem  *semaphore.Weighted
	// admissionCapacity is the semaphore's size, kept so acquire can clamp
	// oversized requests. semaphore.Weighted.Acquire has an explicit
	// `n > s.size` branch that parks on the context and returns its error
	// — so an unclamped oversized weight would not deadlock, but it WOULD
	// be doomed from the start: the job would sit for the whole admission
	// timeout and then fail, having never been satisfiable. Clamping turns
	// that guaranteed slow failure into a job that simply runs alone.
	admissionCapacity int64
)

// admissionUnitBytes is the granularity of the semaphore. Weights are
// expressed in units rather than raw bytes purely to keep the numbers
// small and readable in logs; the ratio is what matters.
const admissionUnitBytes = 64 * 1024 * 1024 // 64 MB

func admissionSemaphore() (*semaphore.Weighted, int64) {
	admissionOnce.Do(func() {
		admissionCapacity = memoryBudget() / admissionUnitBytes
		if admissionCapacity < 1 {
			admissionCapacity = 1
		}
		admissionSem = semaphore.NewWeighted(admissionCapacity)
	})
	return admissionSem, admissionCapacity
}

// admissionWeight converts a byte figure into semaphore units, rounding
// UP so a container is never admitted against less memory than it may
// actually use, and clamping to the total capacity.
//
// The clamp is what keeps an operator-supplied override from producing a
// doomed job: someone who sets AGENTBOX_MEMORY_BYTES above what the host
// can back gets a job that runs alone (weight == whole capacity) rather
// than one that parks for the full admission timeout and then fails,
// having never been satisfiable (Acquire's `n > s.size` branch waits on
// the context rather than returning immediately).
func admissionWeight(memoryBytes int64) int64 {
	_, capacity := admissionSemaphore()
	units := (memoryBytes + admissionUnitBytes - 1) / admissionUnitBytes
	if units < 1 {
		units = 1
	}
	if units > capacity {
		units = capacity
	}
	return units
}

// acquireMemory reserves memoryBytes worth of host memory for a
// container that is about to be created, blocking until the host has
// room. Returns a release function the caller MUST call — via defer at
// the call site, so it runs on every exit path including panics.
//
// It takes the job's STOP CHANNEL rather than a context, deliberately.
// An earlier version took the caller's context, which was wrong in both
// directions:
//
//   - Callers passed context.Background(), so a user pressing Stop on a
//     queued job could not interrupt the wait. The job sat holding a
//     worker until the timeout and then failed with a resource error
//     instead of reporting as cancelled.
//
//   - The one caller that did pass a real context (imageBuild) passed one
//     already carrying the BUILD's 30-minute deadline, so time spent
//     queueing was silently deducted from the build's own wall-clock. A
//     25-minute wait left a 5-minute build, which then died mid-layer
//     with a bare "context deadline exceeded" that named neither memory
//     nor the build timeout.
//
// Taking the stop channel gets cancellation without importing anyone
// else's deadline: the admission timeout is applied here and here only.
// A nil channel is fine and means "not stoppable" — a nil channel never
// fires in select.
//
// logsWriter gets a line only when the job actually has to wait. On an
// idle runner this is silent; when it does print, "waiting for memory"
// is exactly the explanation a user staring at a stalled job needs, and
// the alternative (staying quiet) is what makes queueing feel like a
// hang.
func acquireMemory(stop <-chan struct{}, memoryBytes int64, label string, logsWriter io.Writer) (release func(), err error) {
	sem, _ := admissionSemaphore()
	weight := admissionWeight(memoryBytes)

	// Fast path: if there's room right now, take it without announcing
	// anything. TryAcquire never blocks.
	if sem.TryAcquire(weight) {
		return func() { sem.Release(weight) }, nil
	}

	if logsWriter != nil {
		_, _ = io.WriteString(logsWriter,
			fmt.Sprintf("Waiting for memory on the runner before starting %s (needs %d MB of a %d MB budget). "+
				"Another job is using it; this job will start when that one finishes.\n",
				label, memoryBytes/(1024*1024), memoryBudget()/(1024*1024)))
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), admissionWaitTimeout)
	defer cancel()
	// Translate the stop channel into cancellation of the wait. The
	// goroutine exits via the deferred cancel on every return path, so it
	// cannot outlive this call.
	go func() {
		select {
		case <-stop:
			cancel()
		case <-waitCtx.Done():
		}
	}()

	if err := sem.Acquire(waitCtx, weight); err != nil {
		// Distinguish "the user stopped this job" from "we gave up
		// waiting" — they need different responses from whoever reads the
		// log, and only the latter is a resource problem.
		select {
		case <-stop:
			return nil, fmt.Errorf("stopped while waiting for memory on the runner to start %s", label)
		default:
		}
		return nil, fmt.Errorf("timed out after %s waiting for memory on the runner to start %s "+
			"(needs %d MB of a %d MB budget). Another job held it for longer than that; "+
			"a larger runner instance would let them run at the same time",
			admissionWaitTimeout, label, memoryBytes/(1024*1024), memoryBudget()/(1024*1024))
	}
	if logsWriter != nil {
		_, _ = io.WriteString(logsWriter, fmt.Sprintf("Memory available, starting %s.\n", label))
	}
	return func() { sem.Release(weight) }, nil
}
