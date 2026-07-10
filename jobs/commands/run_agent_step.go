package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// RunAgentStep is a Tasks-only runner command. Spawns an agentbox container
// with the Task's working directory bind-mounted at /work, lets the agent
// edit files there, parses the agentbox /result.json on exit, and merges
// the result into the Step Job's accumulated JobOutput.
//
// Sits between CheckoutRepo (which populated /work/) and CommitAndPush
// (which picks up the diff). All three share the bind-mounted dir within
// the same runner invocation.
//
// Phase 5.2 scope: pull image, spawn container with bind mount + env
// vars, stream logs, wait for exit, parse /result.json. Hardening
// (read-only rootfs, UID 1000 enforcement, memory/CPU limits,
// proxy-based hostname allowlist, cloud-metadata pin, image-pull
// timeout) shipped across Phase 5.4. Heartbeat-driven mid-run stop
// wiring (Phase 5.5) plumbs the runner's stop signal in via the
// StoppableCommand interface and honors it inside spawnAgentbox-
// AndWait by SIGTERM-ing the container with grace; agentbox catches
// the SIGTERM and writes a partial /result.json (status="cancelled")
// before SIGKILL.
type RunAgentStep struct {
	// stopSignal is set by the runner outer loop via SetStopSignal
	// before Run is invoked. Closes when the server reports the Job
	// has been moved to the Stopping state — at which point we
	// SIGTERM the agentbox container with grace. Nil when the outer
	// loop hasn't called SetStopSignal (defensive — a nil channel
	// just blocks forever in select, so the stop branch never fires
	// and behavior matches pre-Phase-5.5).
	stopSignal <-chan struct{}

	// progressSink is set by the runner outer loop via SetProgressSink
	// before Run is invoked. RunAgentStep polls progress.json from the
	// bind-mounted agentbox output dir and calls the sink on each
	// fresh snapshot; the outer loop stores into a per-Job atomic
	// that the heartbeat poller forwards to the server. Nil when
	// the outer loop hasn't called SetProgressSink — the polling
	// goroutine doesn't start in that case (no point reading the
	// file if no consumer cares about the result).
	progressSink func(jobs.LiveProgressV1)
}

// SetStopSignal satisfies jobs.StoppableCommand. The runner's outer
// loop calls this exactly once per Step Job before Run, sharing the
// channel its heartbeat poller's deferred close fires when the server
// reports Stopping=true.
func (rs *RunAgentStep) SetStopSignal(stop <-chan struct{}) {
	rs.stopSignal = stop
}

// SetProgressSink satisfies jobs.ProgressEmittingCommand. The runner's
// outer loop calls this exactly once per Step Job before Run with a
// callback that stores into a per-Job atomic the heartbeat poller
// reads. RunAgentStep invokes the sink each time the polling goroutine
// reads a fresh progress.json snapshot from the bind-mounted dir.
func (rs *RunAgentStep) SetProgressSink(sink func(jobs.LiveProgressV1)) {
	rs.progressSink = sink
}

const (
	agentboxWorkDirInContainer = "/work"
	// Where agentbox writes /result.json. We override the agentbox default
	// (/tmp/result.json) to /work/.agentbox-output/result.json so the file
	// lands in the bind-mounted dir and the runner can read it post-exit.
	// The .agentbox-output prefix keeps it out of CommitAndPush's per-repo
	// iteration (which scans /work/<idx>-<name>/ subdirs).
	agentboxResultDirRel    = ".agentbox-output"
	agentboxResultFile      = "result.json"
	agentboxResultPathInCtr = agentboxWorkDirInContainer + "/" + agentboxResultDirRel + "/" + agentboxResultFile

	// agentboxMCPSocketInContainer is where the per-task MCP tool socket is
	// bind-mounted inside the agent container; agentbox reads its path from
	// MCP_TOOL_RPC_SOCKET and bridges the agent's stdio MCP client to it. Tools
	// execute on the runner side (agent_mcp), so credentials never enter the
	// container. C0: a ping tool only.
	agentboxMCPSocketInContainer = "/run/agentbox/tool-rpc.sock"
	agentMCPSocketEnvVar         = "MCP_TOOL_RPC_SOCKET"
	agentMCPServerVersion        = "0.1.0"

	// agentboxCacheDirInContainer is the shared module-cache + toolchain
	// shelf — a Docker volume mounted into both the vendor phase (which
	// populates it with the git token) and the agent phase (which builds /
	// verifies offline against it). Passed to agentbox as AGENTBOX_CACHE_DIR;
	// agentbox maps it per language (GOMODCACHE, etc.) so the runner stays
	// language-agnostic. See PLAN_tasks_verification.md.
	agentboxCacheDirInContainer = "/cache"
	// agentboxProgressFile is the basename of the live snapshot agentbox
	// writes (Phase 5.5b) next to result.json. Periodic, atomic via
	// temp+rename, schema in agentbox/internal/progress.Snapshot.
	agentboxProgressFile = "progress.json"
	// progressPollInterval is how often the runner re-reads progress.json.
	// Faster than the heartbeat cadence (5s) so each heartbeat sees a
	// reasonably fresh snapshot. Slower would risk dropping intermediate
	// updates, but agentbox's writer is also throttled (~3s) so polling
	// faster than that wastes file reads with no new data.
	progressPollInterval = 3 * time.Second

	// defaultWallClockTimeout is the runner-side cap on how long agentbox
	// can run. Defense in depth — agentbox's own NO_ACTIVITY_TIMEOUT
	// (10m default) catches stdout-silent hangs; this catches the
	// hypothetical case where agentbox itself hangs (orchestrator bug)
	// or where the agent loops with periodic stdout but never finishes.
	// Per PLAN_tasks Open Question 6: 4h proposed; tune after early
	// usage. Phase 6 wires per-Task / per-org overrides via Task model
	// field + Advanced UI.
	defaultWallClockTimeout = 4 * time.Hour
	// defaultVendorTimeout caps the dependency pre-fetch phase. `go mod
	// download` of a large graph is minutes; 30m is generous headroom
	// without letting a stuck private-registry fetch hang the Step.
	defaultVendorTimeout = 30 * time.Minute
	// containerStopGraceSec mirrors agentbox's own SIGTERM grace window
	// (per PLAN_agentbox.md). After this many seconds, Docker promotes
	// SIGTERM to SIGKILL.
	containerStopGraceSec = 10
	// defaultImagePullTimeout bounds how long pullAgentboxImage will
	// wait on Docker Hub / GHCR before failing the Step. cli.ImagePull
	// returns a streaming response that we drain with io.Copy — the
	// reader respects context cancellation (regular HTTP, not hijacked),
	// so wrapping the pull in a context.WithTimeout actually fires.
	// Without this, a slow / rate-limited / network-blipped registry
	// can hang the runner indefinitely (TCP-level retries can take
	// many minutes per stuck pull, compounded by imagePullLock
	// serializing concurrent Step Jobs onto the same upstream wait).
	// 10m is generous: a fresh agentbox pull over a fast link is
	// ~30s, ~2-3min on constrained networks.
	defaultImagePullTimeout = 10 * time.Minute

	// Hardened HostConfig defaults. All four are env-var-overridable
	// (see resolveContainerLimits) so different runner instance sizes
	// can dial up/down without a runner redeploy. Phase 6 wires per-org
	// overrides via Settings UI.
	//
	// Memory + CPU sized for typical Tasks workloads. The real ceiling is
	// the production BUILD, not the agent's analysis working set (~1GB) or
	// npm/pip install (~500MB): a Vite/webpack build of a real app
	// (observed: the dashboard) gets OOM-killed at 2GB during chunk
	// rendering — exit 137 (cgroup SIGKILL) / 134 (Node heap abort). 4GB
	// covers typical builds; unusually heavy ones can raise it further via
	// AGENTBOX_MEMORY_BYTES without a runner redeploy. CPU at 2 cores keeps
	// multiple concurrent Step Jobs feasible on a 4-core runner.
	defaultMemoryBytes = 4 * 1024 * 1024 * 1024 // 4 GB
	defaultCPUCores    = int64(2)               // 2 cores

	// Tmpfs sizes. /tmp covers general scratch (build artifacts, npm
	// caches, etc.); /home/agent covers the agentbox runtime install
	// (npm install -g claude-code lands at $NPM_CONFIG_PREFIX which
	// is /home/agent/.npm-global — see agentbox Dockerfile).
	//
	// uid/gid/mode are mandatory: Docker mounts tmpfs as root-owned by
	// default, which makes runtime `npm install -g` fail with EACCES
	// when agentbox's Driver.Ensure detects a Claude Code version
	// mismatch and tries to install into /home/agent/.npm-global.
	// Pinning to UID 1000 matches the agent user inside the agentbox
	// image (Dockerfile USER agent, UID 1000).
	//
	// `exec` is also mandatory: Docker's default tmpfs flags are
	// `rw,nosuid,nodev,noexec,relatime` and those defaults are
	// merged with whatever we pass — so `noexec` survives unless we
	// explicitly override it. Without `exec`, the kernel refuses to
	// execute any binary that lives in the tmpfs (claude binary
	// installed at /home/agent/.npm-global/lib/.../claude-code-*-x64/
	// claude), producing "Permission denied" on the agent subprocess
	// spawn even though the file's permission bits and ownership are
	// correct. We deliberately keep nosuid + nodev — they're
	// security-relevant and we don't need either for the agent.
	tmpfsTmpOpts  = "rw,exec,size=512m,uid=1000,gid=1000,mode=755"
	tmpfsHomeOpts = "rw,exec,size=1g,uid=1000,gid=1000,mode=755"

	// Env vars on the runner host that override the defaults above.
	memoryBytesEnvVar = "AGENTBOX_MEMORY_BYTES"
	cpuCoresEnvVar    = "AGENTBOX_CPU_CORES"
)

func (rs *RunAgentStep) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkStepDone(parameters, err)
		}
	}()
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	imageRef, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return parameters, fmt.Errorf("agentbox image missing: %s", err)
	}
	if err := pullAgentboxImage(imageRef); err != nil {
		return parameters, fmt.Errorf("error pulling agentbox image: %s", err)
	}
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(ctx.OrganizationID, ctx.TaskID)
	if err := prepareAgentboxResultDir(workDirHost); err != nil {
		return parameters, fmt.Errorf("error preparing agent result dir: %s", err)
	}
	// Two-phase model: a vendor container pre-fetches dependencies into a
	// shared cache volume using the git token, then the credential-less
	// agent container builds / verifies offline against it. The volume is
	// per-Step and ephemeral — removed on the way out. See
	// PLAN_tasks_verification.md.
	cacheVolume := cacheVolumeName(ctx)
	if err := createCacheVolume(cacheVolume); err != nil {
		return parameters, fmt.Errorf("error creating cache volume: %s", err)
	}
	defer removeCacheVolume(cacheVolume)
	vendorSpec, err := buildVendorSpec(imageRef, workDirHost, cacheVolume, ctx)
	if err != nil {
		return parameters, err
	}
	if err := rs.spawnVendorAndWait(vendorSpec, logsWriter); err != nil {
		return parameters, fmt.Errorf("error vendoring dependencies: %s", err)
	}
	envVars, err := buildAgentSpawnEnvVars(parameters)
	if err != nil {
		return parameters, err
	}
	// Per-task MCP tool socket: the runner serves runner-executed tools on a
	// host socket (sibling of the work dir, so it never lands in /work or a
	// commit diff) bind-mounted into the agent container. Tools: ping (C0) +
	// deploy_preview (C2).
	envVars = append(envVars, agentMCPSocketEnvVar+"="+agentboxMCPSocketInContainer)
	previewDeps := buildPreviewDeps(ctx, parameters, workDirHost, logsWriter)
	result, err := rs.spawnAgentboxAndWait(agentboxSpawnSpec{
		imageRef:      imageRef,
		workDirHost:   workDirHost,
		cacheVolume:   cacheVolume,
		env:           envVars,
		mcpSocketHost: agentMCPSocketHostPath(workDirHost),
		previewDeps:   previewDeps,
	}, logsWriter)
	// User-stop path: agentbox SIGTERM-handled and wrote a partial
	// /result.json (status="cancelled" with whatever progress it had).
	// Merge that partial into JobOutput so token usage / denied hosts /
	// changes_summary aren't lost — then surface ErrJobStoppedByUser
	// so the outer loop's stop UX path fires (Step marked cancelled,
	// PR not opened, working dir cleaned).
	if errors.Is(err, types.ErrJobStoppedByUser) {
		_ = mergeAgentResultIntoJobOutput(parameters, result) // best-effort
		return parameters, err
	}
	if err != nil {
		return parameters, fmt.Errorf("error running agentbox: %s", err)
	}
	if err := mergeAgentResultIntoJobOutput(parameters, result); err != nil {
		return parameters, fmt.Errorf("error merging agent result: %s", err)
	}
	if result.Status != "success" {
		return parameters, formatAgentFailure(result)
	}
	// Gate the commit on the agent's self-verification. Failing here stops
	// the command chain before CommitAndPush, so code that failed build/test
	// never reaches a commit or PR. ran==false is deliberately NOT gated — a
	// docs-only or no-build change legitimately skips verify, and CI on PR
	// open remains the backstop (see PLAN_tasks_verification.md Open Q6).
	if vr := result.VerifyResult; vr != nil && vr.Ran && !vr.Passed {
		return parameters, formatVerifyFailure(vr)
	}
	return parameters, nil
}

// agentboxImagePullLock serializes image pulls across concurrent Step Jobs
// on the same runner. Mirrors the existing imagePullLock in
// build_static_site.go — Docker's image-pull is idempotent but doing it
// concurrently for the same image causes wasted bandwidth and occasional
// layer-extraction conflicts.
var agentboxImagePullLock sync.Mutex

func pullAgentboxImage(imageRef string) error {
	agentboxImagePullLock.Lock()
	defer agentboxImagePullLock.Unlock()
	dockerCtx, cancel := context.WithTimeout(context.Background(), defaultImagePullTimeout)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	reader, err := cli.ImagePull(dockerCtx, imageRef, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		if dockerCtx.Err() != nil {
			return fmt.Errorf("image pull exceeded %s timeout for %s", defaultImagePullTimeout, imageRef)
		}
		return err
	}
	return nil
}

// prepareAgentboxResultDir creates the on-host directory that agentbox
// writes /result.json into (via the bind mount). Pre-creating ensures the
// directory exists and is writable before the container starts.
//
// Chowns both the work base and the result dir to the agentbox `agent`
// user so the spawned container (UID 1000) can write through the bind
// mount. CheckoutRepository chowns the cloned repo subtrees; this
// function covers the result dir and the base it sits in.
func prepareAgentboxResultDir(workDirHost string) error {
	resultDir := filepath.Join(workDirHost, agentboxResultDirRel)
	if err := os.MkdirAll(resultDir, 0755); err != nil {
		return err
	}
	if err := os.Chown(workDirHost, commandUtils.AgentboxUID, commandUtils.AgentboxGID); err != nil {
		return err
	}
	return os.Chown(resultDir, commandUtils.AgentboxUID, commandUtils.AgentboxGID)
}

// buildAgentSpawnEnvVars assembles the env vars passed to the agentbox
// container. Combines the runtime-injected credentials (AgentEnvVars,
// populated by deployment-server at Job pickup) with the per-Job spawn
// parameters (StepPrompt, MaxTurns, etc.) and the fixed agentbox contract
// vars (WORK_DIR, RESULT_PATH).
func buildAgentSpawnEnvVars(parameters map[string]interface{}) ([]string, error) {
	env := map[string]string{
		"WORK_DIR":    agentboxWorkDirInContainer,
		"RESULT_PATH": agentboxResultPathInCtr,
		// The shared cache mount. agentbox maps it per language (GOMODCACHE,
		// etc.) — the runner stays language-agnostic.
		"AGENTBOX_CACHE_DIR": agentboxCacheDirInContainer,
	}
	if creds, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.AgentEnvVars); err == nil {
		for k, v := range creds {
			env[k] = v
		}
	} else {
		return nil, fmt.Errorf("agent env vars missing — deployment-server should have injected at pickup: %s", err)
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentType); err == nil && v != "" {
		env["AGENT_TYPE"] = v
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.StepPrompt); err == nil && v != "" {
		env["STEP_PROMPT"] = v
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.PreviousStepsSummary); err == nil && v != "" {
		env["PREVIOUS_STEPS_SUMMARY"] = v
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.Model); err == nil && v != "" {
		env["MODEL"] = v
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.ClaudeCodeVersion); err == nil && v != "" {
		env["CLAUDE_CODE_VERSION"] = v
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.CodexVersion); err == nil && v != "" {
		env["CODEX_VERSION"] = v
	}
	if v, err := jobs.GetParameterValue[int64](parameters, parameters_enums.MaxTurns); err == nil && v > 0 {
		env["MAX_TURNS"] = strconv.FormatInt(v, 10)
	}
	if v, err := jobs.GetParameterValue[int64](parameters, parameters_enums.TokenBudget); err == nil && v > 0 {
		env["TOKEN_BUDGET"] = strconv.FormatInt(v, 10)
	}
	// agentbox proxy allowlist additions. Runner can also layer in its own
	// host-level baseline via the AGENTBOX_ADDITIONAL_ALLOWED_HOSTS env
	// var on the runner process — useful for ops escape hatch (e.g., an
	// internal artifact registry every runner needs reachable). Final
	// value sent to agentbox is the union; agentbox then unions with
	// the Driver's built-in allowlist inside its CONNECT proxy.
	allowed := mergeAdditionalAllowedHosts(parameters)
	if allowed != "" {
		env["ADDITIONAL_ALLOWED_HOSTS"] = allowed
	}
	return mapToEnvSlice(env), nil
}

// mergeAdditionalAllowedHosts unions:
//   - Org-level additions (from Job parameters, populated by deployment-server
//     at pickup from Organization.AdditionalAllowedHosts)
//   - Runner-host baseline (AGENTBOX_ADDITIONAL_ALLOWED_HOSTS env var on
//     the runner process — optional ops escape hatch)
//
// Returns comma-separated string; empty when neither source has hosts.
// Deduplicates while preserving first-seen order. Empty when the
// runner env is unset and the org has no additions — matches the user
// fallback intent: agentbox proxy uses just the Driver's built-in
// allowlist, which already covers the common case for Claude Code.
func mergeAdditionalAllowedHosts(parameters map[string]interface{}) string {
	seen := make(map[string]struct{})
	var ordered []string
	add := func(raw string) {
		for _, h := range strings.Split(raw, ",") {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			if _, ok := seen[h]; ok {
				continue
			}
			seen[h] = struct{}{}
			ordered = append(ordered, h)
		}
	}
	if v, err := jobs.GetParameterValue[string](parameters, parameters_enums.AdditionalAllowedHosts); err == nil {
		add(v)
	}
	add(os.Getenv("AGENTBOX_ADDITIONAL_ALLOWED_HOSTS"))
	return strings.Join(ordered, ",")
}

// mapToEnvSlice converts a string→string env map to Docker's KEY=VALUE
// slice form. Sorted for deterministic spawn (eases log inspection /
// reproducibility).
func mapToEnvSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// spawnAgentboxAndWait creates + starts the container, streams its logs
// to the runner's job log writer, blocks until the container exits (or
// the wall-clock cap fires, or the stop signal arrives), then reads
// /result.json from the bind-mounted host dir.
//
// The wall-clock cap scopes to the container-wait phase only — image
// pull and container creation happen on context.Background() so a slow
// network pull doesn't eat into the agent's run budget.
//
// stopSignal (set by RunAgentStep.SetStopSignal from the runner's
// outer loop) is honored mid-wait: when it fires we SIGTERM the
// container with grace, the agent has time to flush a partial
// /result.json, and waitForContainerExit returns ErrJobStoppedByUser.
// The partial result is still read + returned so token usage /
// changes_summary / denied_hosts aren't lost.
//
// progressSink (set by RunAgentStep.SetProgressSink from the outer
// loop) drives a parallel polling goroutine that reads agentbox's
// progress.json from the bind-mounted dir on its own cadence and
// forwards each fresh snapshot. Nil sink skips the poller entirely.
func (rs *RunAgentStep) spawnAgentboxAndWait(spec agentboxSpawnSpec, logsWriter io.Writer) (agentResult, error) {
	dockerCtx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return agentResult{}, err
	}
	defer cli.Close()
	// Bring the per-task MCP tool socket up BEFORE the container is created —
	// Docker turns a missing bind source into a directory, so the socket file
	// must already exist. Served on the runner side (agent_mcp) so credentials
	// never enter the container; torn down on the way out (LIFO defer order runs
	// this after the container is removed).
	if spec.mcpSocketHost != "" {
		mcpSrv := agentmcp.New("deployment-io-runner", agentMCPServerVersion)
		agentmcp.RegisterPing(mcpSrv)
		if spec.previewDeps != nil {
			agenttools.RegisterDeployPreview(mcpSrv, *spec.previewDeps)
		}
		ln, lerr := mcpSrv.Listen(spec.mcpSocketHost)
		if lerr != nil {
			return agentResult{}, fmt.Errorf("error opening agent tool socket: %w", lerr)
		}
		mcpCtx, cancelMCP := context.WithCancel(dockerCtx)
		defer func() {
			cancelMCP()
			_ = ln.Close()
			_ = os.Remove(spec.mcpSocketHost)
		}()
		go func() { _ = mcpSrv.ServeListener(mcpCtx, ln) }()
	}
	containerID, err := createAgentboxContainer(dockerCtx, cli, spec)
	if err != nil {
		return agentResult{}, err
	}
	// Defers fire LIFO, so the order at return is:
	//   1. logsWg.Wait — wait for the log-streaming goroutine to drain
	//   2. removeContainer — force-remove (registered earlier, runs last)
	// Wait FIRST is wrong: the goroutine only exits when the log stream
	// EOFs, which only happens once the container is gone. So we want:
	//   1. removeContainer (registered LAST below → runs FIRST)
	//   2. logsWg.Wait     (registered FIRST below → runs LAST,
	//                       after the container is gone and the stream EOFs)
	//   3. cli.Close       (registered above → runs after Wait,
	//                       ensuring the goroutine has already returned
	//                       its borrow of cli before we close it)
	var logsWg sync.WaitGroup
	defer logsWg.Wait()
	defer func() { _ = removeContainer(dockerCtx, cli, containerID) }()
	if err := cli.ContainerStart(dockerCtx, containerID, container.StartOptions{}); err != nil {
		return agentResult{}, fmt.Errorf("error starting container: %s", err)
	}
	logsWg.Add(1)
	go func() {
		defer logsWg.Done()
		streamContainerLogs(dockerCtx, cli, containerID, logsWriter)
	}()
	// Phase 5.5b: parallel poller forwards live progress snapshots from
	// agentbox's progress.json (in the bind-mounted output dir) to the
	// outer loop's heartbeat path. Stops when stopProgressPoll closes,
	// which happens at function exit via defer.
	stopProgressPoll := make(chan struct{})
	defer close(stopProgressPoll)
	if rs.progressSink != nil {
		go pollProgressFile(spec.workDirHost, rs.progressSink, stopProgressPoll)
	}
	waitCtx, cancelWait := context.WithTimeout(dockerCtx, defaultWallClockTimeout)
	defer cancelWait()
	_, waitErr := waitForContainerExit(waitCtx, cli, containerID, rs.stopSignal)
	// On user-stop, return the partial result (caller merges into
	// JobOutput) plus the stop sentinel error so the caller can route
	// to the stop UX path. On other errors, propagate as-is.
	if errors.Is(waitErr, types.ErrJobStoppedByUser) {
		result, _ := readAgentResult(spec.workDirHost) // best-effort; may be empty if SIGTERM grace expired
		return result, waitErr
	}
	if waitErr != nil {
		return agentResult{}, waitErr
	}
	return readAgentResult(spec.workDirHost)
}

// createAgentboxContainer wires the container config and host config.
// Hardening applied:
//   - User=1000:1000 (non-root, matches agentbox Dockerfile's `agent` user)
//   - CapDrop=ALL (no Linux capabilities)
//   - ReadonlyRootfs=true (image filesystem can't be modified)
//   - Tmpfs at /tmp + /home/agent (writable for agentbox's runtime
//     npm install + general scratch)
//   - Memory + NanoCPUs limits (env-var-overridable)
//   - ExtraHosts pin cloud-metadata endpoints to 127.0.0.1 (Phase 5.4b
//     defense-in-depth alongside agentbox's CONNECT proxy). The proxy
//     already blocks any host not on the Driver/org allowlist, but
//     pinning the metadata IPs in /etc/hosts neutralizes any direct-IP
//     bypass (e.g., a tool that reads `/proc/net/route` to find a
//     gateway and synthesizes a `169.254.169.254` request without
//     resolving a hostname). Costs nothing; the agent has no
//     legitimate reason to talk to either endpoint.
//
// Network-level enforcement (NetworkMode=bridge with iptables rules)
// is intentionally deferred — the in-container proxy + ExtraHosts
// covers the reachable threat model and avoids host-firewall blast
// radius; revisit if cost-runaway or sandbox-escape incidents
// materialize per PLAN_tasks.md Phase 5.4b notes.
func createAgentboxContainer(ctx context.Context, cli *client.Client, spec agentboxSpawnSpec) (string, error) {
	cfg := &container.Config{
		Image: spec.imageRef,
		Env:   spec.env,
		User:  "1000:1000",
		Tty:   false,
	}
	// Empty Cmd → the image ENTRYPOINT runs agent mode; ["vendor"] selects
	// the dependency pre-fetch subcommand.
	if len(spec.cmd) > 0 {
		cfg.Cmd = spec.cmd
	}
	memoryBytes, nanoCPUs := resolveContainerLimits()
	mounts := []mount.Mount{{
		Type:   mount.TypeBind,
		Source: spec.workDirHost,
		Target: agentboxWorkDirInContainer,
	}}
	if spec.cacheVolume != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: spec.cacheVolume,
			Target: agentboxCacheDirInContainer,
		})
	}
	if spec.mcpSocketHost != "" {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: spec.mcpSocketHost,
			Target: agentboxMCPSocketInContainer,
		})
	}
	hostCfg := &container.HostConfig{
		Mounts:         mounts,
		CapDrop:        []string{"ALL"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":        tmpfsTmpOpts,
			"/home/agent": tmpfsHomeOpts,
		},
		Resources: container.Resources{
			Memory:   memoryBytes,
			NanoCPUs: nanoCPUs,
		},
		ExtraHosts: []string{
			// Hostnames known to resolve to cloud-metadata endpoints. Any
			// gethostbyname-style lookup inside the container returns
			// 127.0.0.1 instead of the real metadata IP.
			"metadata.google.internal:127.0.0.1", // GCP metadata
			"metadata.goog:127.0.0.1",            // GCP metadata (alias)
			// AWS/Azure/OpenStack IMDS is reached by IP literal
			// (169.254.169.254). /etc/hosts is mostly ignored for IP
			// literals — direct-IP defense is the agentbox CONNECT
			// proxy refusing any CONNECT for hosts not on the allowlist.
			// We still pin the IP entry for the rare client that
			// consults nss for IP-literal "hostnames".
			"169.254.169.254:127.0.0.1",
		},
	}
	resp, err := cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("error creating container: %s", err)
	}
	return resp.ID, nil
}

// resolveContainerLimits returns the memory (bytes) and CPU (NanoCPUs)
// caps for the agentbox container. Reads per-runner env-var overrides
// before falling back to the defaults — different EC2 instance sizes
// need different limits without redeploying the runner. Invalid env
// values fall back to defaults (silently — logging from a const-style
// helper would obscure the actual runner logs).
//
// 1 CPU core = 1e9 NanoCPUs in Docker's accounting.
func resolveContainerLimits() (memoryBytes int64, nanoCPUs int64) {
	memoryBytes = defaultMemoryBytes
	if v := os.Getenv(memoryBytesEnvVar); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			memoryBytes = parsed
		}
	}
	cores := defaultCPUCores
	if v := os.Getenv(cpuCoresEnvVar); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			cores = parsed
		}
	}
	nanoCPUs = cores * 1_000_000_000
	return memoryBytes, nanoCPUs
}

// pollProgressFile reads agentbox's progress.json from the bind-mounted
// output dir on a fixed interval and forwards each fresh snapshot via
// sink. Stops when stopCh closes. Best-effort throughout — any read /
// unmarshal error is silently dropped because:
//
//   - The file is written atomically by agentbox (temp + rename), so
//     true partial reads aren't possible. A "no such file" error is
//     normal during the first ~3s before agentbox's first write.
//
//   - A transient stat / read error self-heals on the next tick.
//
//   - Forwarding a stale or malformed snapshot would be worse than
//     forwarding none — the dashboard prefers "no live counter" over
//     "wrong live counter".
//
// Dedup: only forwards when UpdatedAtUnix advances. Prevents the
// heartbeat from spamming identical snapshots when the agent is
// pausing between turns.
func pollProgressFile(workDirHost string, sink func(jobs.LiveProgressV1), stopCh <-chan struct{}) {
	path := filepath.Join(workDirHost, agentboxResultDirRel, agentboxProgressFile)
	ticker := time.NewTicker(progressPollInterval)
	defer ticker.Stop()
	var lastUpdatedAt int64
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var snap struct {
				Turns           int   `json:"turns"`
				InputTokens     int   `json:"input_tokens"`
				OutputTokens    int   `json:"output_tokens"`
				CacheReadTokens int   `json:"cache_read_tokens"`
				UpdatedAtUnix   int64 `json:"updated_at_unix"`
			}
			if err := json.Unmarshal(data, &snap); err != nil {
				continue
			}
			if snap.UpdatedAtUnix == lastUpdatedAt {
				continue // no new write since last poll
			}
			lastUpdatedAt = snap.UpdatedAtUnix
			sink(jobs.LiveProgressV1{
				Turns:           snap.Turns,
				InputTokens:     snap.InputTokens,
				OutputTokens:    snap.OutputTokens,
				CacheReadTokens: snap.CacheReadTokens,
				UpdatedAtUnix:   snap.UpdatedAtUnix,
			})
		}
	}
}

func streamContainerLogs(ctx context.Context, cli *client.Client, containerID string, logsWriter io.Writer) {
	logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("error attaching to container logs: %s\n", err))
		return
	}
	defer logs.Close()
	// Container is created with Tty=false, so Docker prepends an 8-byte
	// header per chunk to multiplex stdout/stderr. Demux via StdCopy so
	// the dashboard sees readable log text instead of headers leaking
	// through as control characters. Both streams flow into the same
	// underlying logsWriter — we don't surface the stdout/stderr split
	// to users today, but the binary headers MUST be stripped first.
	if _, err := stdcopy.StdCopy(logsWriter, logsWriter, logs); err != nil && err != io.EOF {
		io.WriteString(logsWriter, fmt.Sprintf("error streaming container logs: %s\n", err))
	}
}

// waitForContainerExit blocks until one of three things happens:
//   - The container exits naturally (returns its exit code; for the agent
//     phase the code is advisory since status flows via /result.json)
//   - The wall-clock cap fires (returns a wrapped deadline error)
//   - The user-stop signal fires (SIGTERM the container with grace,
//     wait for it to actually exit so /result.json gets written, then
//     return ErrJobStoppedByUser)
//
// stopSignal can be nil — a nil channel never fires in select, so the
// stop branch is silently skipped. Pre-Phase-5.5 behavior matches.
func waitForContainerExit(ctx context.Context, cli *client.Client, containerID string, stopSignal <-chan struct{}) (int, error) {
	statusCh, errCh := cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if errors.Is(err, context.DeadlineExceeded) {
			// Wall-clock cap fired. SIGTERM the container with the standard
			// 10s grace; agentbox catches and writes a partial result.json
			// (status=cancelled) before SIGKILL. We surface the cap as the
			// error so the Step is marked Failed with a clear cause.
			stopCtx := context.Background() // ContainerStop on a fresh context — the wait ctx is already done
			grace := containerStopGraceSec
			_ = cli.ContainerStop(stopCtx, containerID, container.StopOptions{Timeout: &grace})
			return 0, fmt.Errorf("agentbox exceeded wall-clock cap of %s — SIGTERM sent", defaultWallClockTimeout)
		}
		if err != nil {
			return 0, fmt.Errorf("error waiting for container exit: %s", err)
		}
	case status := <-statusCh:
		// Container exited. Agent phase: the exit code is advisory (status
		// flows via /result.json). Vendor phase: the caller treats a
		// non-zero code as a dependency-fetch failure.
		return int(status.StatusCode), nil
	case <-stopSignal:
		// User stopped the Job mid-run. SIGTERM the container with grace
		// so agentbox flushes a partial /result.json (status="cancelled"),
		// then wait for the actual exit before returning. Without the
		// follow-up wait, the deferred removeContainer in spawnAgentbox-
		// AndWait would race a still-flushing agentbox and we'd lose the
		// partial result.
		stopCtx := context.Background()
		grace := containerStopGraceSec
		_ = cli.ContainerStop(stopCtx, containerID, container.StopOptions{Timeout: &grace})
		select {
		case <-statusCh:
		case <-errCh:
		}
		return 0, types.ErrJobStoppedByUser
	}
	return 0, nil
}

func removeContainer(ctx context.Context, cli *client.Client, containerID string) error {
	return cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// agentResult mirrors the shape of agentbox's /result.json. Only the
// fields the runner consumes are pulled out; agentbox can emit
// additional fields without breaking unmarshal.
//
// DeniedHosts lists hostnames the agentbox proxy refused due to the
// allowlist not covering them. Promoted into JobOutput so the
// dashboard can surface "add these to your allowlist" suggestions.
// Empty when no allowlist denies happened during the run.
type agentResult struct {
	Status         string     `json:"status"`
	ExitCode       int        `json:"exit_code"`
	AgentVersion   string     `json:"agent_version,omitempty"`
	ChangesSummary string     `json:"changes_summary,omitempty"`
	TokenUsage     tokenUsage `json:"token_usage"`
	// Turns is agentbox's per-run turn count (internal/result.Outcome.Turns,
	// emitted as "turns"). The runner previously declared this as TurnCount
	// with json:"turn_count" — neither name matched agentbox's wire shape,
	// so the field always parsed as zero and the dashboard rendered
	// "Turn 0" on completed runs even when liveProgress had been replaced
	// by the final result.json. Carried through to agentOutput by
	// mergeAgentResultIntoJobOutput so app-server's projection picks it up.
	Turns int `json:"turns,omitempty"`
	// CostUSD is the agent's self-reported total run cost in USD (agentbox
	// Outcome.CostUSD, emitted as "cost_usd"). Present for Claude Code; nil
	// for Codex (token usage only). Carried through to agentOutput by
	// mergeAgentResultIntoJobOutput so app-server's projection can show it.
	CostUSD     *float64 `json:"cost_usd,omitempty"`
	Error       string   `json:"error,omitempty"`
	DeniedHosts []string `json:"denied_hosts,omitempty"`
	// PRTitle is the agent-produced short title for the resulting
	// pull request. Distinct from ChangesSummary (longer, what + why).
	PRTitle string `json:"pr_title,omitempty"`
	// VerifyResult is the agent's self-reported build/test outcome. The
	// runner gates the Step's commit on it (ran && !passed → fail before
	// CommitAndPush). Nil when the agent reported none.
	VerifyResult *verifyResult `json:"verify_result,omitempty"`
}

// verifyResult mirrors the fields of agentbox's result.json verify_result
// that the runner consumes for the commit gate. agentbox also emits
// duration/stdout/stderr tails; the runner doesn't need them here.
type verifyResult struct {
	Ran           bool   `json:"ran"`
	Passed        bool   `json:"passed"`
	Command       string `json:"command,omitempty"`
	SkippedReason string `json:"skipped_reason,omitempty"`
}

// tokenUsage mirrors agentbox's /result.json token_usage object. Agentbox
// has emitted this as an object since v1.1.0 (never an int); the runner's
// earlier int64 typing was a latent mismatch that surfaced the first time
// a Tasks Step produced a result.json (success OR failure path — agentbox
// always writes the zero-value object even on early-exit). Fields mirror
// agentbox's internal/result/result.go::TokenUsage exactly.
type tokenUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

// formatAgentFailure produces the error returned when agentbox reports
// status != "success". result.Error carries agentbox's classified
// failure message (e.g., "claude exited with error: ...", "no agent
// output for 10m; subprocess killed", "cancelled by signal", auth/
// rate-limit context). Without including it the runner reports only
// status + exit_code, which is rarely enough to debug.
func formatAgentFailure(result agentResult) error {
	return fmt.Errorf(
		"agent step did not succeed: status=%s exit_code=%d error=%q",
		result.Status, result.ExitCode, result.Error,
	)
}

// formatVerifyFailure is returned when the agent's self-verification ran and
// failed — failing the Step before CommitAndPush so broken code never lands.
// The agent's changes_summary (already merged into JobOutput) carries the
// narrative; this names the command for the Re-run-with-feedback signal.
func formatVerifyFailure(vr *verifyResult) error {
	cmd := vr.Command
	if cmd == "" {
		cmd = "(unspecified command)"
	}
	return fmt.Errorf("agent self-verification failed: %s", cmd)
}

func readAgentResult(workDirHost string) (agentResult, error) {
	resultPath := filepath.Join(workDirHost, agentboxResultDirRel, agentboxResultFile)
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return agentResult{}, fmt.Errorf("error reading %s: %s", resultPath, err)
	}
	var result agentResult
	if err := json.Unmarshal(data, &result); err != nil {
		return agentResult{}, fmt.Errorf("error unmarshalling agent result: %s", err)
	}
	if strings.TrimSpace(result.Status) == "" {
		return result, fmt.Errorf("agent result missing status field")
	}
	return result, nil
}

// mergeAgentResultIntoJobOutput writes the agent block of the JobOutput
// envelope. CommitAndPush + OpenPullRequest later extend the same
// envelope's repositories block; the merge-then-write pattern preserves
// each command's contribution.
func mergeAgentResultIntoJobOutput(parameters map[string]interface{}, result agentResult) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.Agent = &agentOutput{
		ChangesSummary: result.ChangesSummary,
		TokenUsage:     result.TokenUsage,
		Turns:          result.Turns,
		CostUSD:        result.CostUSD,
		ExitCode:       result.ExitCode,
		DeniedHosts:    result.DeniedHosts,
		PRTitle:        result.PRTitle,
	}
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// agentboxSpawnSpec is the per-phase container configuration. An empty Cmd
// runs the image's default agent mode; cacheVolume (when set) mounts the
// shared module cache at /cache. Grouped into a struct to keep
// createAgentboxContainer within the parameter limit.
type agentboxSpawnSpec struct {
	imageRef    string
	workDirHost string
	cacheVolume string
	cmd         []string
	env         []string
	// mcpSocketHost, set for the agent phase only, is the host path of the
	// per-task MCP tool socket. The runner serves tools on it and bind-mounts
	// it into the container at agentboxMCPSocketInContainer. Empty for the
	// vendor phase (no agent, no tools).
	mcpSocketHost string
	// previewDeps, set for the agent phase only, is the task-scoped context the
	// deploy_preview MCP tool closes over. Nil for the vendor phase.
	previewDeps *agenttools.DeployPreviewDeps
}

// agentMCPSocketHostPath returns the host path for a task's MCP tool socket: a
// sibling of the work dir (NOT inside it) so the socket never appears in /work
// — keeping it out of the agent's file view and CommitAndPush's diff.
func agentMCPSocketHostPath(workDirHost string) string {
	return strings.TrimRight(workDirHost, "/") + "-agent-mcp.sock"
}

// buildPreviewDeps assembles the task-scoped context the deploy_preview MCP tool
// closes over. The preview deploys to the runner's OWN cloud via its IAM role in
// the runner's region — so the only wiring needed is setting the Region job-param
// the cloud_api_clients builders read (a Tasks job doesn't carry one today) and
// handing the tool a lazy client factory + the task scope. See PLAN_agent_driven_
// preview_verify.md (C2, thin: runner-only, no control-plane record yet).
func buildPreviewDeps(ctx commandUtils.TaskJobContext, parameters map[string]interface{}, workDirHost string, logsWriter io.Writer) *agenttools.DeployPreviewDeps {
	runnerRegion := utils.RunnerData.Get().RunnerRegion
	if rt, err := region_enums.GetType(runnerRegion); err == nil {
		jobs.SetParameterValue[int64](parameters, parameters_enums.Region, int64(rt))
	}
	return &agenttools.DeployPreviewDeps{
		OrgID:       ctx.OrganizationID,
		TaskID:      ctx.TaskID,
		Region:      runnerRegion,
		WorkDirHost: workDirHost,
		LogsWriter:  logsWriter,
		BuildClients: func() (*s3.Client, *cloudfront.Client, error) {
			s3Client, err := cloud_api_clients.GetS3Client(parameters)
			if err != nil {
				return nil, nil, err
			}
			cfClient, err := cloud_api_clients.GetCloudfrontClient(parameters, cloudfrontRegion)
			if err != nil {
				return nil, nil, err
			}
			return s3Client, cfClient, nil
		},
	}
}

// spawnVendorAndWait runs the vendor container to completion, streaming its
// logs. Honors the user-stop signal and a vendor-phase wall-clock cap.
// Returns an error when the container exits non-zero — a dependency-fetch
// failure that fails the Step before the agent runs (distinct from an
// agent / verify failure). See PLAN_tasks_verification.md.
func (rs *RunAgentStep) spawnVendorAndWait(spec agentboxSpawnSpec, logsWriter io.Writer) error {
	io.WriteString(logsWriter, "Vendoring dependencies into shared cache\n")
	dockerCtx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	containerID, err := createAgentboxContainer(dockerCtx, cli, spec)
	if err != nil {
		return err
	}
	var logsWg sync.WaitGroup
	defer logsWg.Wait()
	defer func() { _ = removeContainer(dockerCtx, cli, containerID) }()
	if err := cli.ContainerStart(dockerCtx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("error starting vendor container: %s", err)
	}
	logsWg.Add(1)
	go func() {
		defer logsWg.Done()
		streamContainerLogs(dockerCtx, cli, containerID, logsWriter)
	}()
	waitCtx, cancelWait := context.WithTimeout(dockerCtx, defaultVendorTimeout)
	defer cancelWait()
	code, waitErr := waitForContainerExit(waitCtx, cli, containerID, rs.stopSignal)
	if waitErr != nil {
		return waitErr
	}
	if code != 0 {
		return fmt.Errorf("vendor phase exited with code %d", code)
	}
	return nil
}

// buildVendorSpec assembles the vendor-phase container spec: the `vendor`
// subcommand, the shared cache mount (AGENTBOX_CACHE_DIR), and the git token
// `agentbox vendor` uses to authenticate private fetches. Language-specific
// env (GOMODCACHE/GOPRIVATE/etc.) is set inside agentbox, not here.
func buildVendorSpec(imageRef, workDirHost, cacheVolume string, ctx commandUtils.TaskJobContext) (agentboxSpawnSpec, error) {
	token, err := vendorGitToken(ctx)
	if err != nil {
		return agentboxSpawnSpec{}, fmt.Errorf("error getting installation token: %s", err)
	}
	env := map[string]string{
		"WORK_DIR":           agentboxWorkDirInContainer,
		"AGENTBOX_CACHE_DIR": agentboxCacheDirInContainer,
	}
	if token != "" {
		env["GIT_TOKEN"] = token
	}
	return agentboxSpawnSpec{
		imageRef:    imageRef,
		workDirHost: workDirHost,
		cacheVolume: cacheVolume,
		cmd:         []string{"vendor"},
		env:         mapToEnvSlice(env),
	}, nil
}

// vendorGitToken mints an installation token for the Step's repos. v1
// assumes a single GitHub App installation (the deployment.io dogfood
// shape); repos spanning multiple installations would each need their own
// token, which the agentbox vendor phase's single github.com rewrite does
// not yet support.
func vendorGitToken(ctx commandUtils.TaskJobContext) (string, error) {
	if len(ctx.Entries) == 0 {
		return "", nil
	}
	return commandUtils.RefreshGitTokenForInstallation(ctx.Entries[0].InstallationID, ctx.OrganizationID)
}

// cacheVolumeName is the per-Step-Job Docker volume holding the shared
// module cache. Scoped to (taskID, stepIndex) so concurrent Steps don't
// collide and cleanup is unambiguous.
func cacheVolumeName(ctx commandUtils.TaskJobContext) string {
	return fmt.Sprintf("agentbox-cache-%s-%d", ctx.TaskID, ctx.StepIndex)
}

func createCacheVolume(name string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.VolumeCreate(context.Background(), volume.CreateOptions{Name: name})
	return err
}

// removeCacheVolume best-effort deletes the per-Step cache volume. Called
// via defer; a leaked volume is reclaimable out of band, so failures here
// are swallowed rather than masking the Step's real outcome.
func removeCacheVolume(name string) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return
	}
	defer cli.Close()
	_ = cli.VolumeRemove(context.Background(), name, true)
}

// (Language-specific helpers — GOPRIVATE derivation, go.work generation,
// per-language verify hosts — now live in agentbox's detector registry, so
// the runner stays language-agnostic. See PLAN_tasks_verification.md.)
