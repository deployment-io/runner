package commands

import (
	"sync"

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
// that actually create a container reserve memory, and they reserve the
// amount they were sized for. Everything else runs unimpeded, and on a
// larger instance the same code admits proportionally more with no
// retuning.
//
// What that means concretely on the shipped m6a.large (2 vCPU, 8 GB):
// two static-site builds fit together, but an agent container or an
// image build is sized to the whole budget and so runs strictly alone.
//
// This is also what makes the generous per-container sizing safe. Giving
// agentbox the whole budget would be reckless if two could overlap;
// admission control is the guarantee that they can't.
//
// A job that does not fit is NOT queued here — it is handed back to the
// server and returns to the pending queue (see the dispatcher in
// entrypoints/common). That is what keeps this simple: there is no wait,
// no timeout, and no ordering policy to get wrong, because the server's
// existing ScheduledTs-ascending sort already decides who goes next.
//
// TWO CONSEQUENCES ON A SMALL RUNNER, both deliberate and neither
// fixable by retuning the numbers:
//
//  1. On the smallest instance we ship (m6a.large, ~7.7 GB) an agent
//     container is sized to the ENTIRE budget, so its weight is 100% of
//     capacity and nothing runs beside it. That is not an accident of the
//     arithmetic — it is what "the agent may use all the memory the host
//     can spare" means, and the alternative on a host that size is to
//     shrink the agent back below the 4 GB that was already OOM-killing
//     Tasks.
//
//  2. Following from 1: while an Assistant SESSION is alive it holds
//     ~34% of capacity, so an agent job needing 100% can never be
//     admitted until the session ends — up to the server's 4h idle /
//     wall-clock cap. The Task is requeued, not failed, but it can stall
//     for hours. The session→Task path is safe (converting a session
//     MarkStopping's its Job first, freeing the memory), so this bites
//     only a Task or image build dispatched while an unrelated session
//     is left open.
//
//     There is no tuning that fixes this on a 7.7 GB host: an agent plus
//     a session wants 8.23 GB of a 6.17 GB budget, and lowering the agent
//     ceiling far enough to fit a session (6.17 - 3 = 3.17 GB) puts it
//     below the 4 GB that was already OOM-killing Tasks. Concurrency on a
//     small runner is the thing being traded away; a larger instance buys
//     it back with no code change, since both the caps and the capacity
//     scale with the host.

// admissionUnitBytes is the granularity of the semaphore. Weights are
// expressed in units rather than raw bytes purely to keep the numbers
// small and readable in logs; the ratio is what matters.
const admissionUnitBytes = 64 * 1024 * 1024 // 64 MB

var (
	admissionOnce sync.Once
	admissionSem  *semaphore.Weighted
	// admissionCapacity is the semaphore's size, kept so acquire can clamp
	// oversized requests. Without the clamp an operator-supplied override
	// larger than the host would produce a weight that can never be
	// satisfied, so the job could never start no matter how idle the
	// runner became.
	admissionCapacity int64
)

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
// actually use, and clamping to the total capacity so an oversized
// override yields a job that runs alone rather than one that can never
// run at all.
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

// TryAcquireMemory reserves memoryBytes of host memory for a job that is
// about to run, WITHOUT blocking. It returns a release function the
// caller must defer, and false if the runner has no room right now.
//
// Non-blocking is the whole point. An earlier version waited here, which
// was wrong in three ways at once: it held one of the runner's job
// workers for the duration, it eventually FAILED the job (turning a busy
// runner into failed deployments), and because the semaphore is strict
// FIFO a large waiter blocked smaller jobs that would have fit.
//
// The caller's answer to false is to hand the job back to the server, so
// it returns to the pending queue and is offered again once capacity
// frees. Nothing waits, nothing fails, and no worker is tied up.
func TryAcquireMemory(memoryBytes int64) (release func(), ok bool) {
	sem, _ := admissionSemaphore()
	weight := admissionWeight(memoryBytes)
	if !sem.TryAcquire(weight) {
		return nil, false
	}
	return func() { sem.Release(weight) }, true
}

// MemoryBudgetBytes exposes the host memory budget for the dispatcher's
// operator-facing log line, so "at capacity" reports what the capacity
// actually was instead of leaving someone to guess at the host size.
func MemoryBudgetBytes() int64 { return memoryBudget() }
