package commands

import (
	"os"
	"strconv"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner/utils/hostinfo"
)

// Host resource detection and the container memory budget derived from it.
//
// # WHY THIS EXISTS
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
	// anything beside it — past this point the extra is reserved but
	// unused while blocking concurrent jobs.
	//
	// 8 GB is a CHOSEN bound, not a measured one: we have an observed
	// lower bound (a real Go build hit the old 4 GB cap) but no data on
	// where a Task's demand actually plateaus. It is set at twice the
	// known-insufficient figure and should be revisited once cgroup peak
	// reporting from agentbox gives real numbers.
	agentboxMemoryCeilingBytes = 8 * 1024 * 1024 * 1024 // 8 GB

	// buildMemoryFloorBytes is the pre-host-sizing static-site build
	// default, kept as a floor for the same no-regression reason.
	buildMemoryFloorBytes = 2 * 1024 * 1024 * 1024 // 2 GB

	// buildMemoryCeilingBytes bounds a single build container.
	buildMemoryCeilingBytes = 8 * 1024 * 1024 * 1024 // 8 GB

	// buildBudgetDivisor keeps static-site builds small enough to still run
	// several at once. They are the workload that legitimately fans out —
	// a push can trigger several deployments — so handing each the full
	// budget would silently serialize deploys that run concurrently today.
	//
	// This is a CONCURRENCY choice, not a claim that builds are light.
	// They are not: run_agent_step.go records a real Vite/webpack build
	// (the dashboard) being OOM-killed at exactly 2 GB. A third of the
	// budget is more than the old flat 2 GB on every host we ship and
	// grows with the instance, but a genuinely heavy single build is
	// still the case BUILD_MEMORY_BYTES exists for.
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

	// Assistant sessions get their OWN, much smaller cap despite sharing
	// createAgentboxContainer with Task Steps. They are a different
	// workload wearing the same container:
	//
	//   - A Step spawns with a cacheVolume and runs a vendor phase before
	//     the agent, because the agent BUILDS and tests. A session spawns
	//     with neither (see run_assistant_session.go) — it is read-only
	//     planning, so it never builds.
	//   - The sizing note in run_agent_step.go says the ceiling is the
	//     production build, not "the agent's analysis working set (~1GB)".
	//     A session is only ever the analysis half.
	//   - A Step lasts minutes. A session stays alive for the whole
	//     conversation, up to the server's 4h cron and an 8h hard cap.
	//
	// Sizing them like a Step meant one interactive session reserved the
	// ENTIRE host budget for hours while a human typed, so every deploy
	// and Task behind it was requeued for the session's lifetime. This is
	// generous for analysis but small enough to leave the host usable.
	sessionMemoryFloorBytes   = 2 * 1024 * 1024 * 1024 // 2 GB
	sessionMemoryCeilingBytes = 3 * 1024 * 1024 * 1024 // 3 GB

	sessionMemoryBytesEnvVar = "SESSION_MEMORY_BYTES"

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

// resolveSessionLimits returns the memory (bytes) and CPU (NanoCPUs) caps
// for an Assistant session container. Deliberately not resolveContainer-
// Limits: a session never builds, and it holds its reservation for hours,
// so it is sized for analysis rather than for a production build. See
// sessionMemoryFloorBytes.
func resolveSessionLimits() (memoryBytes int64, nanoCPUs int64) {
	memoryBytes = clampMemory(memoryBudget()/buildBudgetDivisor, sessionMemoryFloorBytes, sessionMemoryCeilingBytes)
	if override := envMemoryOverride(sessionMemoryBytesEnvVar); override > 0 {
		memoryBytes = override
	}
	cores := hostCPUCores()
	if override := envCoresOverride(cpuCoresEnvVar); override > 0 {
		cores = override
	}
	return memoryBytes, cores * 1_000_000_000
}

// JobMemoryBytes returns the host memory a job needs reserved for its
// whole command sequence, or 0 when it spawns no container.
//
// Keyed off the command enums so the DISPATCHER can reserve before
// running anything. Reserving per-command instead would mean a job that
// cannot get memory has already done its checkout, and would have to redo
// it when requeued. Holding the reservation across the cheap commands
// (checkout, commit, push) costs a little headroom and buys idempotence.
//
// The max, not the sum: a job's containers run one after another (the
// vendor container exits before the agent starts), so the peak is what
// has to fit.
func JobMemoryBytes(commandEnums []commands_enums.Type) int64 {
	var peak int64
	consider := func(bytes int64) {
		if bytes > peak {
			peak = bytes
		}
	}
	for _, commandEnum := range commandEnums {
		switch commandEnum {
		case commands_enums.RunAgentStep:
			bytes, _ := resolveContainerLimits()
			consider(bytes)
		case commands_enums.RunAssistantSession:
			bytes, _ := resolveSessionLimits()
			consider(bytes)
		case commands_enums.BuildStaticSite:
			bytes, _ := resolveBuildLimits()
			consider(bytes)
		case commands_enums.BuildDockerImage, commands_enums.BuildNixPacksImage:
			bytes, _ := resolveImageBuildLimits()
			consider(bytes)
		}
	}
	return peak
}
