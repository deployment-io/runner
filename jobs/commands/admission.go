package commands

import "sync"

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

// admissionUsed is the memory currently reserved by running jobs, in
// bytes, guarded by admissionMu. A plain counter is the whole mechanism:
// reserve on admit, subtract on release, refuse when the total would
// exceed the budget.
//
// This deliberately does NOT use a semaphore. An earlier version did,
// back when admission waited for capacity, but once waiting was replaced
// by requeueing the only calls left were the non-blocking ones — so the
// waiter queue was permanently empty, its FIFO fairness ordered nothing,
// and its blocking acquire was unreachable. Carrying that machinery
// implied to a reader that jobs queue here, which is exactly what they
// no longer do.
//
// Counting BYTES rather than fixed-size units follows from the same
// simplification: the units only existed to keep semaphore weights
// small, and they cost a rounding step that reserved slightly more than
// a job asked for.
var (
	admissionMu   sync.Mutex
	admissionUsed int64
)

// TryAcquireMemory reserves memoryBytes of host memory for a job that is
// about to run, WITHOUT blocking. It returns a release function the
// caller must defer, and false if the runner has no room right now.
//
// Non-blocking is the whole point. An earlier version waited here, which
// was wrong in three ways at once: it held one of the runner's job
// workers for the duration, it eventually FAILED the job (turning a busy
// runner into failed deployments), and its queue let a large waiter block
// smaller jobs that would have fit.
//
// The caller's answer to false is to hand the job back to the server, so
// it returns to the pending queue and is offered again once capacity
// frees. Nothing waits, nothing fails, and no worker is tied up.
//
// A request larger than the whole budget is clamped to it rather than
// refused outright. Without the clamp — an operator setting
// AGENTBOX_MEMORY_BYTES above what the host can back — the job could
// never be admitted on any poll, so it would requeue forever and never
// run, with nothing to explain why. Clamped, it runs alone.
func TryAcquireMemory(memoryBytes int64) (release func(), ok bool) {
	budget := memoryBudget()
	want := memoryBytes
	if want > budget {
		want = budget
	}
	if want < 1 {
		want = 1
	}

	admissionMu.Lock()
	defer admissionMu.Unlock()
	if admissionUsed+want > budget {
		return nil, false
	}
	admissionUsed += want

	// Called exactly once, via defer at the single call site in the
	// dispatcher.
	return func() {
		admissionMu.Lock()
		admissionUsed -= want
		admissionMu.Unlock()
	}, true
}

// MemoryBudgetBytes exposes the host memory budget for the dispatcher's
// operator-facing log line, so "at capacity" reports what the capacity
// actually was instead of leaving someone to guess at the host size.
func MemoryBudgetBytes() int64 { return memoryBudget() }
