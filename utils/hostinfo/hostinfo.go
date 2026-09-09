// Package hostinfo reports the physical resources of the machine the
// runner is running on.
//
// It is separate from its two callers — jobs/resources sizes containers
// from these numbers, and client reports them to the control plane on
// ping — because facts about the machine and policy derived from them
// are different concerns, and only the facts are wanted by both.
//
// Note the split is a LAYERING choice today, not a compiler constraint.
// It began as one: the detection originally lived in jobs/commands,
// which imports client, so the ping client could not reach it without a
// cycle. That pressure is gone now that sizing lives in jobs/resources,
// which imports neither client nor anything reaching it — merging the
// two would compile. It should still not be merged: hostinfo answers
// "what does this machine have", jobs/resources answers "who may use how
// much of it", and keeping the second out of the client's import graph
// is what stops sizing policy leaking into the RPC layer.
package hostinfo

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// FallbackMemoryBytes is used when /proc/meminfo can't be read (a
// non-Linux dev machine, an unexpectedly restricted procfs). It is
// deliberately the size of the smallest instance we ship, so anything
// derived from it lands on known-safe values rather than on something
// optimistic that a small host can't honor.
const FallbackMemoryBytes = 8 * 1024 * 1024 * 1024 // 8 GB

const procMeminfoPath = "/proc/meminfo"

var (
	memoryOnce     sync.Once
	measuredMemory int64 // 0 when detection failed
)

// MemoryBytes returns the TOTAL physical memory of the machine the
// runner container is running on — deliberately the host's memory, not
// the runner's own cgroup limit.
//
// The distinction matters and is easy to get backwards. The runner runs
// as an ECS task with a 6 GB task-level limit, so reading its own cgroup
// would report 6 GB on an 8 GB box. But the containers being sized from
// this are NOT children of the runner: they are siblings created through
// the mounted Docker socket, so they draw from host memory and are
// capped independently. The number we need to divide up is the host's.
//
// /proc/meminfo is not namespaced by Docker, so MemTotal read from
// inside the container is the host's MemTotal. That is exactly what we
// want, and is why this reads procfs directly rather than using a
// cgroup-aware memory library.
//
// Falls back to FallbackMemoryBytes when detection fails, so sizing
// always has a number to work with. That fallback makes this the WRONG
// function for reporting the host's specs — see MeasuredMemoryBytes.
//
// Cached: the value cannot change while the process lives, and this is
// called on every container create.
func MemoryBytes() int64 {
	if measured := MeasuredMemoryBytes(); measured > 0 {
		return measured
	}
	return FallbackMemoryBytes
}

// MeasuredMemoryBytes returns the host's total memory, or ZERO when it
// could not be determined.
//
// Use this, never MemoryBytes, when the value is REPORTED rather than
// used for sizing. The two differ only when detection fails, and that is
// exactly the case where the difference matters: MemoryBytes substitutes
// a conservative default so caps land somewhere safe, which is right for
// a limit and wrong for a fact. Reporting the fallback tells the control
// plane a laptop has 8 GB and paints that number in the dashboard beside
// a vCPU count that IS measured, with nothing to say which is which.
//
// Callers that report should let zero mean "unknown" and pass it through
// — the Runner model fields are omitempty and the dashboard renders a
// dash, so an absent value stays visibly absent.
func MeasuredMemoryBytes() int64 {
	memoryOnce.Do(func() {
		measuredMemory = readMemTotalBytes(procMeminfoPath)
	})
	return measuredMemory
}

// readMemTotalBytes parses the MemTotal line out of a meminfo-formatted
// file and returns it in bytes. Returns 0 on any failure so the caller
// applies its fallback — this runs on the container-create path and a
// missing procfs must never fail a job.
//
// The line looks like "MemTotal:       16116496 kB" — the unit is always
// kB in practice, but the suffix is checked rather than assumed so a
// unitless or differently-suffixed value degrades to the fallback
// instead of being silently misread by three orders of magnitude.
func readMemTotalBytes(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		// Expect exactly: ["MemTotal:", "<number>", "kB"]
		if len(fields) != 3 || !strings.EqualFold(fields[2], "kB") {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// CPUCores returns the number of logical CPUs available to the runner
// process.
//
// runtime.NumCPU respects CPU affinity but not cgroup CPU quota, and the
// runner's task definition sets no task-level or container-level Cpu, so
// on ECS this is the host's vCPU count — which is what we want for the
// same sibling-container reason as MemoryBytes.
func CPUCores() int64 {
	if n := runtime.NumCPU(); n > 0 {
		return int64(n)
	}
	return 1
}
