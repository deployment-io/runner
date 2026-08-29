package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/client"
)

// A container that dies for memory says so only if someone asks. Docker
// records it on the container's State, and the runner never looked — so an
// OOM arrived as a bare non-zero exit or an unexplained `signal: killed`,
// and on 2026-08-28 that cost an evening of guessing at causes.
//
// agentbox reports this from the inside now too (its cgroupmem package), and
// the two are deliberately not the same check. Inside, agentbox reads the
// cgroup's kill counter, which catches a CHILD being killed while agentbox
// itself survives — the case that actually happened. Here, Docker's flag
// describes the container as a whole, which is what we see when agentbox is
// the process that died and there is nothing left to report from within.
// Either one alone leaves a hole.

// containerInspectTimeout bounds the post-mortem. It is a local daemon call
// on an already-exited container, so it should be instant; the timeout only
// exists so a wedged daemon cannot turn a diagnostic into a hang on the path
// that reports a job's result.
const containerInspectTimeout = 15 * time.Second

// reportContainerExit writes a line about how the container died when there
// is something worth saying: an OOM kill, or a signal exit that would
// otherwise be a bare number.
//
// MUST be called before the container is removed — the deferred removal in
// the spawn paths destroys the state this reads. Best-effort throughout: a
// failed inspect is silence, never an error, because this runs on a path
// that is already reporting a result and must not change it.
func reportContainerExit(cli *client.Client, containerID string, logsWriter io.Writer) {
	if cli == nil || containerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), containerInspectTimeout)
	defer cancel()

	info, err := cli.ContainerInspect(ctx, containerID)
	if err != nil || info.State == nil {
		return
	}
	if msg := describeContainerExit(info.State.OOMKilled, info.State.ExitCode); msg != "" {
		io.WriteString(logsWriter, msg+"\n")
	}
}

// describeContainerExit renders the exit for a human reading a job log, or ""
// when the exit speaks for itself. Split from the Docker call so the wording
// can be tested without a daemon.
func describeContainerExit(oomKilled bool, exitCode int) string {
	switch {
	case oomKilled:
		// Docker's own flag, so this is stated as fact. It names the knob
		// because the next question is always "what do I do about it", and
		// AGENTBOX_MEMORY_BYTES is a runner env var — not something anyone
		// would guess from an exit code.
		return fmt.Sprintf("Container was OOM-killed by the kernel (exit %d). "+
			"Raise AGENTBOX_MEMORY_BYTES on the runner, or reduce the work's parallelism.", exitCode)
	case exitCode == 137:
		// 128+9: SIGKILL, without Docker attributing it to memory. That is
		// the ambiguous case — a cgroup can kill a CHILD without the
		// container being flagged — so it points at the limit as a
		// possibility rather than asserting it.
		return "Container exited 137 (SIGKILL) without Docker reporting an OOM. " +
			"A memory limit is still the likeliest cause — the kernel can kill a process " +
			"inside the container without the container itself being flagged."
	case exitCode == 143:
		// 128+15: SIGTERM. Normal for our own stop paths, which already say
		// why, so this only matters when nothing else explained it.
		return "Container exited 143 (SIGTERM) — terminated rather than exiting on its own."
	case exitCode > 128:
		return fmt.Sprintf("Container was killed by signal %d (exit %d).", exitCode-128, exitCode)
	}
	return ""
}
