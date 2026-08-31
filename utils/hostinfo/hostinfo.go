// Package hostinfo reports the physical resources of the machine the
// runner is running on.
//
// It exists as its own package because two callers need it and they sit
// on opposite sides of an existing import edge: jobs/commands sizes
// containers from these numbers, and client reports them to the control
// plane on ping. jobs/commands already imports client, so the detection
// cannot live in either without creating a cycle.
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
	memoryOnce  sync.Once
	memoryCache int64
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
// Cached: the value cannot change while the process lives, and this is
// called on every container create.
func MemoryBytes() int64 {
	memoryOnce.Do(func() {
		memoryCache = readMemTotalBytes(procMeminfoPath)
		if memoryCache <= 0 {
			memoryCache = FallbackMemoryBytes
		}
	})
	return memoryCache
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
