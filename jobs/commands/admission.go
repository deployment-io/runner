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
// amount they were sized for. Everything else runs unimpeded. The
// practical effect on the shipped m6a.large (2 vCPU, 8 GB) is that one
// agentbox Task or one image build runs at a time, while several
// static-site builds still run in parallel — and on a larger instance
// the same code admits proportionally more, with no retuning.
//
// This is also what makes the generous per-container sizing safe. Giving
// agentbox the whole budget would be reckless if two could overlap;
// admission control is the guarantee that they can't.

const (
	// admissionWaitTimeout bounds how long a job waits for memory before
	// giving up. Generous, because waiting is the CORRECT behaviour here:
	// a queued job that starts three minutes late is a far better outcome
	// than two jobs that start immediately and are both OOM-killed. It
	// exists only so a leaked slot can't wedge a worker forever.
	//
	// Comfortably longer than a typical agentbox Step but well short of
	// the runner's 4h job wall-clock, so a job that can't be admitted
	// fails with a clear message rather than consuming its whole budget
	// sitting in a queue.
	admissionWaitTimeout = 30 * time.Minute
)

var (
	admissionOnce sync.Once
	admissionSem  *semaphore.Weighted
	// admissionCapacity is the semaphore's size, kept so acquire can clamp
	// oversized requests. semaphore.Weighted.Acquire blocks FOREVER when
	// asked for more than the total capacity rather than returning an
	// error, so an unclamped oversized weight would deadlock the job.
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
// The clamp is what keeps an operator-supplied override from wedging the
// runner: someone who sets AGENTBOX_MEMORY_BYTES above what the host can
// back gets a job that runs alone (weight == whole capacity) rather than
// a job that blocks forever waiting for memory that will never exist.
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
// ctx is the job's context, so a cancelled or stopped job stops waiting
// immediately instead of holding a worker.
//
// logsWriter gets a line only when the job actually has to wait. On an
// idle runner this is silent; when it does print, "waiting for memory"
// is exactly the explanation a user staring at a stalled job needs, and
// the alternative (staying quiet) is what makes queueing feel like a
// hang.
func acquireMemory(ctx context.Context, memoryBytes int64, label string, logsWriter io.Writer) (release func(), err error) {
	sem, _ := admissionSemaphore()
	weight := admissionWeight(memoryBytes)

	// Fast path: if there's room right now, take it without announcing
	// anything. TryAcquire never blocks.
	if sem.TryAcquire(weight) {
		return func() { sem.Release(weight) }, nil
	}

	if logsWriter != nil {
		_, _ = io.WriteString(logsWriter,
			fmt.Sprintf("Waiting for memory on the runner before starting %s (needs %d MB)...\n",
				label, memoryBytes/(1024*1024)))
	}

	waitCtx, cancel := context.WithTimeout(ctx, admissionWaitTimeout)
	defer cancel()
	if err := sem.Acquire(waitCtx, weight); err != nil {
		// Distinguish "the job was cancelled" from "we gave up waiting" —
		// they need different responses from whoever reads the log.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("timed out after %s waiting for memory on the runner to start %s "+
			"(needs %d MB of a %d MB budget); other jobs on this runner are holding it",
			admissionWaitTimeout, label, memoryBytes/(1024*1024), memoryBudget()/(1024*1024))
	}
	if logsWriter != nil {
		_, _ = io.WriteString(logsWriter, fmt.Sprintf("Memory available, starting %s.\n", label))
	}
	return func() { sem.Release(weight) }, nil
}
