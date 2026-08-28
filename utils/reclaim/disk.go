package reclaim

import (
	"io"
	"os"
	"strconv"
)

const (
	// diskPath is the filesystem every reclaim measures. Both things that
	// fill up — the Docker data root and the per-job scratch dirs under
	// /tmp — live on the root volume in every runner layout we ship.
	diskPath = "/"

	// lowWaterBytesEnvVar overrides the free-space level below which the
	// runner starts warning. Per-runner because instance sizes differ.
	lowWaterBytesEnvVar = "RUNNER_DISK_LOW_WATER_BYTES"

	// defaultLowWaterBytes is generous on purpose: a single application
	// image plus its build cache can be several GB, so a runner with less
	// than this free is one deploy away from ENOSPC.
	defaultLowWaterBytes = 10 << 30 // 10 GiB
)

// LogFreeDisk reports free space on the runner's root volume, and warns when
// it has fallen below the low-water mark. Callers log it at the points where
// a human reading the job output would want to know — before a build, around
// each reclaim.
//
// Logging only: the runner has no internal-alert path (alerts are typed in
// kit and emitted by deployment-server), so a warning line in the job log and
// the runner log is the whole mechanism.
func LogFreeDisk(stage string, logsWriter io.Writer) {
	free, ok := freeDiskBytes(diskPath)
	if !ok {
		return
	}
	logf(logsWriter, "Disk %s: %s free on %s", stage, humanBytes(free), diskPath)
	warnIfLow(free, logsWriter)
}

// warnIfLow emits the low-water-mark warning. Split out so every reclaim
// path warns identically.
func warnIfLow(free uint64, logsWriter io.Writer) {
	mark := lowWaterBytes()
	if free >= mark {
		return
	}
	logf(logsWriter, "WARNING: only %s free on %s — below the %s low-water mark. "+
		"Builds and Task steps may fail with ENOSPC.", humanBytes(free), diskPath, humanBytes(mark))
}

// reclaiming brackets a cleanup with a free-space reading on either side, so
// the job log says how much room the runner had and how much the cleanup
// recovered. A runner should say how much room it has before a job dies.
func reclaiming(logsWriter io.Writer, what string, fn func()) {
	before, hadBefore := freeDiskBytes(diskPath)
	if hadBefore {
		logf(logsWriter, "Reclaiming %s — %s free on %s", what, humanBytes(before), diskPath)
	} else {
		logf(logsWriter, "Reclaiming %s", what)
	}
	fn()
	after, hadAfter := freeDiskBytes(diskPath)
	if !hadAfter {
		return
	}
	if hadBefore && after > before {
		logf(logsWriter, "Reclaimed %s — %s free on %s (%s recovered)",
			what, humanBytes(after), diskPath, humanBytes(after-before))
	} else {
		logf(logsWriter, "Reclaimed %s — %s free on %s", what, humanBytes(after), diskPath)
	}
	warnIfLow(after, logsWriter)
}

// lowWaterBytes resolves the warning threshold, falling back to the default
// on anything unparseable rather than failing a job over a bad env var.
func lowWaterBytes() uint64 {
	if v := os.Getenv(lowWaterBytesEnvVar); v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultLowWaterBytes
}

// humanBytes formats a byte count for a log line.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	value := float64(n)
	suffixes := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for value >= unit && i < len(suffixes)-1 {
		value /= unit
		i++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + suffixes[i]
}
