package commands

import (
	"os"
	"strconv"

	"github.com/deployment-io/deployment-runner/utils/hostinfo"
)

// Host resource detection and the container memory budget derived from it.
//
// WHY THIS EXISTS
//
// Every container the runner spawns (agentbox for Tasks and Assistant
// sessions, the static-site build container, docker/nixpacks image
// builds) used to carry a hardcoded memory cap that had no relationship
// to the machine underneath. That was wrong in both directions:
//
//   - Too small on a big host. A customer who upgrades their runner EC2
//     instance got exactly zero extra room, because the agentbox cap
//     stayed at a constant 4 GB no matter how much RAM the box had.
//     Real Task builds hit that ceiling: agentbox reported "memory ran
//     close to the limit — peak 4.0 GiB of a 4.0 GiB limit", and an
//     earlier run died with an unexplained "signal: killed" (an OOM
//     kill inside the cgroup).
//
//   - Too large in aggregate. The caps were never checked against the
//     host. On the shipped m6a.large (2 vCPU, 8 GB) the runner's own ECS
//     task limit is 6 GB and a 4 GB agentbox container spawns ALONGSIDE
//     it as a sibling through the mounted Docker socket — outside ECS
//     accounting entirely. 6 + 4 = 10 GB of caps on a ~7.7 GB machine.
//     It only survived because the runner process itself uses a few
//     hundred MB and heavy jobs rarely overlapped.
//
// Sizing from the host fixes both: caps grow when the instance grows,
// and the total is bounded by what actually exists. See memoryBudget for
// the aggregate side and admission.go for the enforcement that keeps
// concurrent containers inside it.
const (
	// hostMemoryReserveBytes is the floor held back for everything that
	// is NOT a job container: the kernel, dockerd (which is where an
	// image build actually runs — it is not in the runner's cgroup), the
	// runner process, and the ECS agent. Undersizing this is what turns a
	// cgroup OOM kill (one job dies, cleanly attributed) into a HOST OOM
	// kill (the kernel picks a victim, which can be dockerd or the runner
	// itself, and every in-flight job dies with no useful error).
	hostMemoryReserveBytes = 1536 * 1024 * 1024 // 1.5 GB

	// hostMemoryReserveFraction scales the reserve on larger hosts, where
	// dockerd's own footprint during a big build grows with the image.
	// The effective reserve is the larger of the two — a flat 1.5 GB is
	// right on an 8 GB box and too thin on a 64 GB one.
	hostMemoryReserveFraction = 0.12

	// minContainerMemoryBytes is the absolute floor for any container cap
	// and for the budget itself. Below roughly this, nothing we run is
	// viable, so a host this small should fail loudly at the job rather
	// than quietly hand out a cap of zero (Docker reads 0 as "unlimited",
	// which is the opposite of what a tiny host wants).
	minContainerMemoryBytes = 1 * 1024 * 1024 * 1024 // 1 GB

	// agentboxMemoryFloorBytes is the pre-host-sizing default. Deriving
	// from the host must never hand a Task LESS than it got before, so
	// this is a floor rather than a starting point.
	agentboxMemoryFloorBytes = 4 * 1024 * 1024 * 1024 // 4 GB

	// agentboxMemoryCeilingBytes stops a very large host from sizing a
	// single agent container so big that admission control can't fit
	// anything beside it. Past ~8 GB an agent's own working set stops
	// growing (it's the production BUILD inside the container that
	// consumes memory, not the agent), so the extra would be reserved and
	// unused while blocking concurrent jobs.
	agentboxMemoryCeilingBytes = 8 * 1024 * 1024 * 1024 // 8 GB

	// buildMemoryFloorBytes is the pre-host-sizing static-site build
	// default, kept as a floor for the same no-regression reason.
	buildMemoryFloorBytes = 2 * 1024 * 1024 * 1024 // 2 GB

	// buildMemoryCeilingBytes bounds a single build container.
	buildMemoryCeilingBytes = 8 * 1024 * 1024 * 1024 // 8 GB

	// buildBudgetDivisor keeps static-site builds small enough to still run
	// several at once. They are the parallel workload (a push can fan out
	// across several deployments) and are comparatively light —
	// npm/webpack peaked well inside the previous 2 GB cap. Handing them
	// the full budget would silently serialize deployments that run
	// concurrently today.
	buildBudgetDivisor = 3

	// Image builds (docker / nixpacks) get the WHOLE budget rather than a
	// share, unlike static-site builds. Two reasons:
	//
	//  1. This path was completely UNBOUNDED until now — a `docker build`
	//     could use the entire host. Any cap is a new constraint, so it is
	//     set generously to avoid turning builds that succeed today into
	//     failures. A share (~2 GB on the shipped m6a.large) would be well
	//     inside what a real Node or Java image build uses.
	//  2. An image build is normally the single heavy thing happening
	//     during a deploy, so serializing two of them on a small host is
	//     the correct outcome rather than a regression.
	imageBuildMemoryFloorBytes   = 4 * 1024 * 1024 * 1024 // 4 GB
	imageBuildMemoryCeilingBytes = 8 * 1024 * 1024 * 1024 // 8 GB

	// cpuPeriodMicroseconds is Docker's default CFS scheduling period.
	// The image-build API takes a quota/period pair in microseconds
	// instead of the NanoCPUs used by ContainerCreate.
	cpuPeriodMicroseconds = int64(100000) // 100ms

	imageBuildMemoryBytesEnvVar = "BUILD_IMAGE_MEMORY_BYTES"
	imageBuildCPUCoresEnvVar    = "BUILD_IMAGE_CPU_CORES"
)

// resolveImageBuildLimits returns the memory cap (bytes) and CPU core
// count for a docker/nixpacks image build. Returns CORES, not NanoCPUs,
// because ImageBuildOptions wants a CFS quota/period pair — the caller
// multiplies by cpuPeriodMicroseconds.
//
// BUILD_IMAGE_MEMORY_BYTES / BUILD_IMAGE_CPU_CORES override the derived
// values. They exist specifically so a customer whose build legitimately
// needs more than the derived cap can be unblocked by an operator
// without waiting for a runner release — this path was unbounded for a
// long time and some build somewhere will be sized accordingly.
func resolveImageBuildLimits() (memoryBytes int64, cores int64) {
	memoryBytes = clampMemory(memoryBudget(), imageBuildMemoryFloorBytes, imageBuildMemoryCeilingBytes)
	if override := envMemoryOverride(imageBuildMemoryBytesEnvVar); override > 0 {
		memoryBytes = override
	}
	cores = hostCPUCores()
	if override := envCoresOverride(imageBuildCPUCoresEnvVar); override > 0 {
		cores = override
	}
	return memoryBytes, cores
}

// hostMemoryBytes and hostCPUCores delegate to the hostinfo package.
// The detection lives there rather than here because the ping client
// also needs it to report host specs to the control plane, and
// jobs/commands already imports client — so keeping it in this package
// would make that a cycle.
func hostMemoryBytes() int64 { return hostinfo.MemoryBytes() }

func hostCPUCores() int64 { return hostinfo.CPUCores() }

// memoryBudget returns the total bytes that may be handed out across ALL
// concurrently running job containers. This is the number admission
// control hands out slices of, and the ceiling any single container cap
// is clamped to.
//
// Host memory minus the reserve. Everything the runner spawns has to fit
// in here together.
func memoryBudget() int64 {
	host := hostMemoryBytes()
	reserve := int64(float64(host) * hostMemoryReserveFraction)
	if reserve < hostMemoryReserveBytes {
		reserve = hostMemoryReserveBytes
	}
	budget := host - reserve
	// Degenerate hosts (a tiny dev box, or a fallback that somehow went
	// negative) still need a workable number rather than a zero or
	// negative budget that would deadlock admission control.
	if budget < minContainerMemoryBytes {
		return minContainerMemoryBytes
	}
	return budget
}

// clampMemory bounds a computed cap between a floor and a ceiling, and
// additionally to the whole budget — no single container may be sized
// larger than the total the host can support.
func clampMemory(want, floor, ceiling int64) int64 {
	if budget := memoryBudget(); ceiling > budget {
		ceiling = budget
	}
	if ceiling < floor {
		// A host too small to honor the floor gets the ceiling. Better a
		// cap the machine can actually back than a floor it can't.
		return ceiling
	}
	if want < floor {
		return floor
	}
	if want > ceiling {
		return ceiling
	}
	return want
}

// envMemoryOverride reads an explicit operator override in bytes.
// Returns 0 when unset or invalid, meaning "use the derived value".
//
// These overrides are kept from the pre-host-sizing implementation on
// purpose: they are the escape hatch for a workload whose real needs
// don't match what we derive, and they can be set without a code change.
// A caller applying one is opting out of the host-derived sizing
// deliberately, so it is NOT clamped to the budget — but it IS still
// counted at its full value by admission control, so an operator who
// oversizes it serializes their own jobs rather than OOMing the host.
func envMemoryOverride(key string) int64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// envCoresOverride mirrors envMemoryOverride for CPU core counts.
func envCoresOverride(key string) int64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}
