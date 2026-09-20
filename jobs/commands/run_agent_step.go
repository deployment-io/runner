package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner/jobs/resources"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
	runnerclient "github.com/deployment-io/deployment-runner/client"
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

	// agentboxTmpDirRel backs TMPDIR/GOTMPDIR inside the container, moving
	// scratch off /tmp — a 512 MB RAM-backed tmpfs (tmpfsTmpOpts) — and onto
	// the bind-mounted work dir, which sits on the host's 30 GB root volume.
	//
	// 512 MB is not enough for real installs and builds. Measured: a `yarn
	// install` of one ordinary Next.js repo PEAKS at 504.8 MB of TMPDIR while
	// Yarn extracts and re-zips a patched dependency — a 7 MB margin before
	// anything else in the container touches /tmp. It ENOSPC'd in production
	// on 2026-08-28. `go build` without -p 1 fails the same way, from the
	// linker, and that is the long-standing GOTMPDIR failure too.
	//
	// Not /tmp-made-bigger: tmpfs is RAM, and it would be competing with the
	// build that needs it. Not /cache either, though it is disk-backed and
	// roomier — it is a fresh Docker volume per Step with no host path the
	// runner can pre-create a directory in, whereas the work dir is already
	// ours and already prepared here.
	//
	// Dot-prefixed for the same reason as .agentbox-output: agentbox's repo
	// discovery skips dot-dirs at both levels (BuildPlan in
	// internal/vendoring/plan.go), and CommitAndPush iterates
	// /work/<idx>-<name>/ subdirs, so scratch can never be mistaken for a
	// repo or staged into a Task's commit.
	agentboxTmpDirRel   = ".agentbox-tmp"
	agentboxTmpDirInCtr = agentboxWorkDirInContainer + "/" + agentboxTmpDirRel

	// agentboxCorepackHomeInCtr overrides the image's COREPACK_HOME
	// (/opt/corepack), which the container cannot write to: we spawn with
	// ReadonlyRootfs, so every path outside the mounts is read-only.
	//
	// corepack creates a scratch directory under COREPACK_HOME whenever it
	// installs a package-manager version it hasn't cached, so any repo whose
	// packageManager names something other than the image's pre-warmed Yarn
	// Classic dies on the first `yarn` call:
	//
	//   EROFS: read-only file system, mkdir '/opt/corepack/v1/corepack-...'
	//
	// EROFS, not EACCES — the Dockerfile chowns /opt/corepack to the agent
	// user correctly; the filesystem itself is the problem, so no permission
	// change in the image can fix it.
	//
	// Cost of moving it: the image pre-warms Yarn Classic into /opt/corepack
	// so unpinned repos never fetch a yarn binary, and corepack reads only
	// COREPACK_HOME, so redirecting gives that up — such a repo now downloads
	// Classic from registry.npmjs.org once per Step. That is ~1 MB against a
	// vendor step that already pulls hundreds, and it buys correctness for
	// every pinned repo. Preserving both would mean mounting a named volume
	// over /opt/corepack (Docker seeds an empty one from the image layer),
	// which is a second per-Step volume to create and reap for the sake of a
	// megabyte.
	agentboxCorepackHomeRel   = agentboxTmpDirRel + "/corepack"
	agentboxCorepackHomeInCtr = agentboxWorkDirInContainer + "/" + agentboxCorepackHomeRel

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

	// Hardened HostConfig defaults. Memory and CPU are no longer
	// constants — they are derived from the host the runner is on (see
	// resources.LimitsForAgentContainer) and remain
	// env-var-overridable. Phase 6 wires per-org overrides via Settings UI.
	//
	// The real memory ceiling for a Task is the production BUILD, not the
	// agent's analysis working set (~1GB) or npm/pip install (~500MB): a
	// Vite/webpack build of a real app (observed: the dashboard) gets
	// OOM-killed at 2GB during chunk rendering — exit 137 (cgroup SIGKILL)
	// / 134 (Node heap abort). The previous flat 4GB covered typical
	// builds but was itself hit by a real Go build on the shipped
	// m6a.large, which is why sizing now follows the host: see
	// agentboxMemoryFloorBytes / agentboxMemoryCeilingBytes.

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
	// The Step is now implementing. Reported here rather than left implicit
	// so a Step that later moves to Review has a stage to move FROM — a card
	// that only ever said "Review" would read as though the agent never ran.
	reportTaskStepStage(parameters, ctx, tasks.StageImplement, logsWriter)
	imageRef, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return parameters, fmt.Errorf("agentbox image missing: %s", err)
	}
	if err := pullAgentboxImage(imageRef); err != nil {
		return parameters, fmt.Errorf("error pulling agentbox image: %s", err)
	}
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(ctx.OrganizationID, ctx.TaskID)
	if err := prepareAgentboxHostDirs(workDirHost); err != nil {
		return parameters, fmt.Errorf("error preparing agentbox host dirs: %s", err)
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
	envVars, err := buildAgentSpawnEnvVars(parameters, logsWriter)
	if err != nil {
		return parameters, err
	}
	// Per-task MCP tool socket: the runner serves runner-executed tools on a
	// host socket (sibling of the work dir, so it never lands in /work or a
	// commit diff) bind-mounted into the agent container. Tools: ping (C0) +
	// deploy_static_site_preview (C2).
	envVars = append(envVars, agentMCPSocketEnvVar+"="+agentboxMCPSocketInContainer)
	previewDeps := buildStaticSitePreviewDeps(ctx, parameters, workDirHost, logsWriter)
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
	//
	// pre_existing is the third exemption. agentbox replayed every failed
	// step on the commit its repo was checked out at when the run began and
	// found the same failure there, so the Step didn't cause it. Discarding
	// the work over a build that was already red punishes the wrong run; the
	// failure is instead carried into the job log and the PR body, where the
	// reviewer can act on it. agentbox is failure-closed about this — an
	// unknown baseline never sets the flag — so trusting it here does not
	// widen the gate.
	switch decideVerifyGate(result.VerifyResult) {
	case verifyGateFail:
		return parameters, formatVerifyFailure(result.VerifyResult)
	case verifyGateWarnPreExisting:
		io.WriteString(logsWriter, formatPreExistingVerifyWarning(result.VerifyResult))
	}
	return parameters, nil
}

// verifyGateDecision is what the commit gate does with an agent's
// self-verification.
type verifyGateDecision int

const (
	// verifyGateProceed: nothing to gate on — verify passed, was skipped, or
	// wasn't reported at all.
	verifyGateProceed verifyGateDecision = iota
	// verifyGateFail: the run introduced a failure. Stop before
	// CommitAndPush; the work is discarded.
	verifyGateFail
	// verifyGateWarnPreExisting: the failure predates the run. Continue to
	// CommitAndPush and say so in the job log and PR body.
	verifyGateWarnPreExisting
)

// decideVerifyGate is the gate's whole decision, extracted so it can be
// exercised without a Docker daemon.
//
// ran==false is deliberately NOT gated: a docs-only or no-build change
// legitimately skips verify, and CI on PR open remains the backstop.
// PreExisting is only ever set by agentbox after a successful baseline
// replay of EVERY failed step, and never when a baseline couldn't be
// established — a legacy payload with no steps therefore decodes to false
// and fails exactly as it did before this field existed.
func decideVerifyGate(vr *verifyResult) verifyGateDecision {
	if vr == nil || !vr.Ran || vr.Passed {
		return verifyGateProceed
	}
	if vr.PreExisting {
		return verifyGateWarnPreExisting
	}
	return verifyGateFail
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

// prepareAgentboxHostDirs creates the on-host directories agentbox writes
// into through the bind mount: the result dir for /result.json, and the tmp
// dir backing TMPDIR/GOTMPDIR (see agentboxTmpDirRel). Both must exist and be
// writable before the container starts — TMPDIR pointing at a missing
// directory fails every write with ENOENT rather than falling back to /tmp.
//
// Chowns the work base and both dirs to the agentbox `agent` user so the
// spawned container (UID 1000) can write through the bind mount.
// CheckoutRepository chowns the cloned repo subtrees; this function covers
// these two and the base they sit in.
func prepareAgentboxHostDirs(workDirHost string) error {
	if err := os.Chown(workDirHost, commandUtils.AgentboxUID, commandUtils.AgentboxGID); err != nil {
		return err
	}
	for _, rel := range []string{agentboxResultDirRel, agentboxTmpDirRel, agentboxCorepackHomeRel} {
		dir := filepath.Join(workDirHost, rel)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if err := os.Chown(dir, commandUtils.AgentboxUID, commandUtils.AgentboxGID); err != nil {
			return err
		}
	}
	return nil
}

// buildAgentSpawnEnvVars assembles the env vars passed to the agentbox
// container. Combines the runtime-injected credentials (AgentEnvVars,
// populated by deployment-server at Job pickup) with the per-Job spawn
// parameters (StepPrompt, MaxTurns, etc.) and the fixed agentbox contract
// vars (WORK_DIR, RESULT_PATH).
func buildAgentSpawnEnvVars(parameters map[string]interface{}, logsWriter io.Writer) ([]string, error) {
	env := map[string]string{
		"WORK_DIR":    agentboxWorkDirInContainer,
		"RESULT_PATH": agentboxResultPathInCtr,
		// The shared cache mount. agentbox maps it per language (GOMODCACHE,
		// etc.) — the runner stays language-agnostic.
		"AGENTBOX_CACHE_DIR": agentboxCacheDirInContainer,
		// Scratch off the 512 MB tmpfs — see agentboxTmpDirRel. GOTMPDIR is
		// set alongside TMPDIR because the Go toolchain reads it in
		// preference, and the linker is the loudest victim of a small /tmp.
		"TMPDIR":   agentboxTmpDirInCtr,
		"GOTMPDIR": agentboxTmpDirInCtr,
		// corepack cannot write to the image's /opt/corepack under
		// ReadonlyRootfs — see agentboxCorepackHomeInCtr.
		"COREPACK_HOME": agentboxCorepackHomeInCtr,
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
	// Resolved ONCE, up front. Every step below is told which provider serves
	// this Job instead of re-reading the env for a signal — which is what let
	// the old ordering matter (model rendering had to run before the
	// subscription step, because that step consumed the marker the model step
	// still needed).
	provider := resolveJobProvider(parameters, logsWriter)
	agentType, _ := llm_provider_enums.ResolveAgentType(env["AGENT_TYPE"])
	// env["MODEL"] is still set above from the same parameter: agentbox reads it
	// as the generic model var, and applyAgentModelEnv overwrites it with the
	// rendered id for every agent that reads it there.
	spawn := agentSpawn{provider: provider, agentType: agentType, model: env["MODEL"]}
	// claude-code's own Bedrock switch, written for the one agent that reads it.
	// Nothing puts it in the credential bundle, so opencode and codex cannot
	// inherit one.
	llm_provider_enums.ApplyClaudeCodeUseBedrock(env, provider, agentType)

	// Whatever this provider needs at spawn — credentials, discovery — is its
	// own business; the call site does not know which provider it is.
	resolve := prepareProvider(env, provider, logsWriter)
	// Returns an error only when the model CANNOT be resolved and the run is
	// therefore already lost — see applyAgentModelEnv. Propagated rather than
	// logged so the Task's error names the cause, instead of the agent failing
	// later on something that never mentions model access.
	if err := applyAgentModelEnv(env, spawn, resolve, logsWriter); err != nil {
		return nil, err
	}
	// Optionally swap the injected ANTHROPIC_API_KEY for a Claude Code
	// subscription OAuth token read from this runner's own AWS Secrets Manager.
	// OrganizationIDNamespace is best-effort here: it only enables the
	// self-grant retry below, and an unreadable secret still degrades to the
	// API key without it.
	organizationID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	maybeApplyClaudeSubscriptionAuth(env, provider, organizationID, logsWriter)
	return mapToEnvSlice(env), nil
}

const (

	// claudeOAuthSecretName is the Secrets Manager entry holding the customer's
	// Claude Code subscription OAuth token, in their own account.
	//
	// DELIBERATELY A CONSTANT — do NOT make this control-plane-supplied.
	// The runner's IAM role carries secretsmanager:* on Resource "*" (granted on
	// the deploy path via iam_policies AwsSecretsManager), so a remote-supplied
	// name would hand the control plane an arbitrary-secret-read primitive on
	// the customer's account: it could name any secret and we'd read it and
	// inject it into an agent container's env. Keeping the name in code means
	// there is no path to read anything else, regardless of what IAM allows.
	// If this ever needs to vary per customer, scope the IAM to a single secret
	// ARN FIRST. See plans/PLAN_tasks_subscription_auth.md §4.3.
	claudeOAuthSecretName = "deployment-io/claude-code-oauth-token"
)

// maybeApplyClaudeSubscriptionAuth swaps the injected ANTHROPIC_API_KEY for a
// Claude Code subscription OAuth token when the org is in subscription mode.
// The token is read from the customer's own AWS Secrets Manager on this runner
// and never transits the control plane (unlike the API key, which
// deployment-server injects at Job pickup).
//
// Guardrails (see plans/PLAN_tasks_subscription_auth.md):
//   - Off unless deployment-server marked the org subscription-mode. The
//     customer must also have created the secret and granted this runner read
//     access, so the feature is inert without their action on their own cloud.
//   - Genuine Claude Code only — never codex/opencode (their subscription auth
//     is prohibited/blocked; genuine `claude` is what passes Anthropic's
//     client-identity check).
//   - Replace, not co-set: ANTHROPIC_API_KEY is removed so Claude Code doesn't
//     ambiguously prefer it over the OAuth token.
//   - Any failure (missing/unreadable/malformed secret) falls back to the
//     injected API key rather than hard-failing the task. An org configured
//     strict subscription-only has no key to fall back to, so agentbox fails
//     fast instead of quietly reverting to metered billing.
func maybeApplyClaudeSubscriptionAuth(env map[string]string, provider llm_provider_enums.Provider, organizationID string, logsWriter io.Writer) {
	if provider.AuthMode() != llm_provider_enums.AuthSubscription {
		return // org is not in subscription mode (the default)
	}
	// AGENT_TYPE unset defaults to claude-code in agentbox, so "" is allowed.
	if at := env["AGENT_TYPE"]; at != "" && at != "claude-code" {
		return // genuine Claude Code only
	}
	token, err := readClaudeOAuthToken(claudeOAuthSecretName)
	if err != nil && isAccessDenied(err) && organizationID != "" {
		// The customer created the secret but this runner's role can't read it
		// (its stack never deployed anything, so it never self-granted Secrets
		// Manager). Grant it the same way the deploy path does and re-read, so
		// subscription auth needs no manual IAM step during setup.
		io.WriteString(logsWriter, "Subscription auth: no read access to the OAuth secret — granting this runner Secrets Manager access.\n")
		if grantErr := grantSecretsManagerAccess(organizationID); grantErr != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Subscription auth: could not grant Secrets Manager access (%s).\n", grantErr))
		} else {
			token, err = readClaudeOAuthToken(claudeOAuthSecretName)
		}
	}
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Subscription auth: could not read OAuth secret %q, falling back to API key (%s).\n", claudeOAuthSecretName, err))
		return
	}
	if !strings.HasPrefix(token, "sk-ant-oat") {
		io.WriteString(logsWriter, "Subscription auth: secret is not a Claude Code OAuth token (expected sk-ant-oat...), falling back to API key.\n")
		return
	}
	delete(env, "ANTHROPIC_API_KEY")
	env["CLAUDE_CODE_OAUTH_TOKEN"] = token
	io.WriteString(logsWriter, "Subscription auth: using Claude Code subscription OAuth token from Secrets Manager.\n")
}

// readClaudeOAuthToken fetches the OAuth token string from this runner's AWS
// Secrets Manager (runner's own region / instance-role credentials).
func readClaudeOAuthToken(secretName string) (string, error) {
	runnerData := utils.RunnerData.Get()
	client, err := cloud_api_clients.GetSecretsManagerClientFromRegion(runnerData.RunnerRegion)
	if err != nil {
		return "", err
	}
	out, err := client.GetSecretValue(context.TODO(), &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		return "", err
	}
	if out.SecretString == nil {
		return "", fmt.Errorf("secret %q has no string value", secretName)
	}
	return strings.TrimSpace(*out.SecretString), nil
}

// isAccessDenied reports whether an AWS error is an authorization failure, as
// opposed to the secret simply not existing. Only a denial is worth self-granting
// for — retrying a genuinely missing secret would write IAM on every task.
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "AccessDeniedException", "AccessDenied", "UnauthorizedOperation":
		return true
	}
	return false
}

// grantSecretsManagerAccess self-grants this runner Secrets Manager access, the
// same way the deploy path does before creating a registry-credential secret
// (see CreateSecretAwsSecretManager). Reusing the shared helper means the grant,
// the already-present short-circuit and the IAM propagation wait all behave
// identically to every other policy the runner self-provisions — including
// returning immediately once the actions are in place, so a runner that can
// never read the secret (permissions boundary, SCP, CMK-encrypted secret) pays
// the propagation wait once rather than on every spawn.
func grantSecretsManagerAccess(organizationID string) error {
	runnerData := utils.RunnerData.Get()
	return iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsSecretsManager,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID,
		runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
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
			agenttools.RegisterDeployStaticSitePreview(mcpSrv, *spec.previewDeps)
			agenttools.RegisterVerifyPreviewReachable(mcpSrv, spec.previewDeps.LogsWriter)
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
	waitTimeout := defaultWallClockTimeout
	if spec.waitTimeout > 0 && spec.waitTimeout < waitTimeout {
		waitTimeout = spec.waitTimeout
	}
	waitCtx, cancelWait := context.WithTimeout(dockerCtx, waitTimeout)
	defer cancelWait()
	_, waitErr := waitForContainerExit(waitCtx, cli, containerID, rs.stopSignal, logsWriter)
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
	memoryBytes, nanoCPUs := resources.LimitsForAgentContainer()
	if spec.memoryBytes > 0 {
		memoryBytes = spec.memoryBytes
	}
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
func waitForContainerExit(ctx context.Context, cli *client.Client, containerID string, stopSignal <-chan struct{}, logsWriter io.Writer) (int, error) {
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
		//
		// Ask Docker HOW it died before the deferred removal destroys the
		// answer. Silent on an ordinary exit; speaks only for an OOM or a
		// signal, which are the deaths a bare exit code fails to explain.
		reportContainerExit(cli, containerID, logsWriter)
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
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	AgentVersion   string `json:"agent_version,omitempty"`
	ChangesSummary string `json:"changes_summary,omitempty"`
	// FilesChanged is the agent's self-reported list of changed files. Carried
	// through to agentOutput so CommitAndPush can detect the "agent reported
	// changes but nothing landed in a repo" failure (writes outside the repo dir).
	FilesChanged []string   `json:"files_changed,omitempty"`
	TokenUsage   tokenUsage `json:"token_usage"`
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
	// ReviewResult is what a review-mode run found. Nil for an implement
	// run, and for an agentbox image that predates review mode — which is
	// exactly how the older-image case is detected rather than crashed on.
	//
	// Note what it does NOT contain: must_fix_open. That decision is the
	// runner's, computed from these findings and the thresholds stamped into
	// the Job, because an agent that could assert it could wave its own
	// findings through.
	ReviewResult *reviewResult `json:"review_result,omitempty"`
}

// reviewResult mirrors agentbox's result.json review_result object — a
// deliberate hand-mirror, like tokenUsage above, because the runner shares no
// module with agentbox.
//
// Every field is a STRING, including parameter and severity. agentbox imports
// no enum of ours; the runner maps the names through deployment-runner-kit's
// wire mirror and annotates whatever it cannot read.
type reviewResult struct {
	Findings []reviewFinding  `json:"findings,omitempty"`
	Coverage []reviewCoverage `json:"coverage,omitempty"`
}

type reviewFinding struct {
	Key       string `json:"key,omitempty"`
	Parameter string `json:"parameter,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Location  string `json:"location,omitempty"`
	What      string `json:"what,omitempty"`
	Why       string `json:"why,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Pass      string `json:"pass,omitempty"`
}

type reviewCoverage struct {
	Parameter string `json:"parameter,omitempty"`
	State     string `json:"state,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// verifyResult mirrors the fields of agentbox's result.json verify_result
// that the runner consumes for the commit gate.
//
// The tails are here because their absence made a failing Step
// undiagnosable: this struct used to declare only the first four fields,
// so anything else in the JSON was dropped silently at decode, and the
// failure message could name nothing but the command. The comment that
// used to sit here said the runner "doesn't need them", which was the
// assumption that cost a Task 23 minutes of finished work on 2026-08-29.
//
// Steps and PreExisting arrived with agentbox's baseline replay. A
// multi-repo Task verifies once per repository, so the rollup above can only
// ever describe one of them; Steps carries the rest. PreExisting is
// agentbox's own verdict — not the agent's — on whether every FAILED step
// also fails on the commit each repo was checked out at when agentbox
// started. Older images emit neither, which decodes to nil/false and gates
// exactly as before.
type verifyResult struct {
	Ran           bool   `json:"ran"`
	Passed        bool   `json:"passed"`
	Command       string `json:"command,omitempty"`
	SkippedReason string `json:"skipped_reason,omitempty"`
	StdoutTail    string `json:"stdout_tail,omitempty"`
	StderrTail    string `json:"stderr_tail,omitempty"`

	Steps       []verifyStep `json:"steps,omitempty"`
	PreExisting bool         `json:"pre_existing,omitempty"`
}

// verifyStep is one repository's verification within a Step run, with
// agentbox's baseline comparison attached.
//
// BaselineRan false means no baseline could be established — no recorded
// start commit, an unresolvable repo path, an unreachable commit, a replay
// that errored or timed out. agentbox never sets PreExisting in that case,
// so the runner needs no separate handling: an unknown baseline keeps the
// gate closed.
type verifyStep struct {
	Repo       string `json:"repo,omitempty"`
	Command    string `json:"command,omitempty"`
	Passed     bool   `json:"passed"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`

	BaselineRan        bool   `json:"baseline_ran,omitempty"`
	BaselinePassed     bool   `json:"baseline_passed,omitempty"`
	BaselineStderrTail string `json:"baseline_stderr_tail,omitempty"`
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
	// CacheCreationTokens is cache-WRITE input, which Anthropic bills at a
	// premium over fresh input and separately from cache reads.
	//
	// This field was missing, so the value was dropped HERE — in the middle of
	// a chain where both ends already handled it. agentbox emits
	// cache_creation_tokens and its Claude and opencode parsers populate it;
	// app-server reads the same key out of Job.Output. Only this struct never
	// carried it, so every cache write silently became zero and the dashboard
	// has been under-reporting cache-heavy runs since the field was added
	// upstream.
	CacheCreationTokens int `json:"cache_creation_tokens"`
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

// verifyTailMaxBytes caps the failure output carried into the Step error.
// The error is surfaced in the dashboard and fed to Re-run-with-feedback,
// so it has to stay readable; agentbox is asked for a few lines, and this
// only guards against an agent that ignores that.
const verifyTailMaxBytes = 2000

// formatVerifyFailure is returned when the agent's self-verification ran and
// failed — failing the Step before CommitAndPush so broken code never lands.
//
// Carries the failure OUTPUT, not just the command. Naming only the command
// describes the ritual rather than the cause, and leaves the reader to
// reproduce the failure themselves to find out what it was: on 2026-08-29 a
// Step reported `agent self-verification failed: GOWORK=off go build ./...
// && go vet ./... && go test ./...` and the actual reason was a single
// pre-existing vet warning in a package the agent never touched.
//
// stderr is preferred because that is where build and test failures land;
// stdout is the fallback for tools that report failures there instead.
// Empty when the agent reported no tail at all, which is every agentbox
// older than the release that started asking for one — hence the graceful
// degradation to today's message rather than an empty separator.
func formatVerifyFailure(vr *verifyResult) error {
	cmd := verifyCommandLabel(vr.Command)
	if tail := verifyFailureTail(vr); tail != "" {
		return fmt.Errorf("agent self-verification failed: %s — %s", cmd, tail)
	}
	return fmt.Errorf("agent self-verification failed: %s", cmd)
}

// preExistingVerifySteps returns the failed steps agentbox confirmed also
// fail on the base commit. These are the ones the Step is being allowed to
// push over, so they are exactly what the job log and PR body must name.
func preExistingVerifySteps(vr *verifyResult) []verifyStep {
	if vr == nil {
		return nil
	}
	var out []verifyStep
	for _, s := range vr.Steps {
		if !s.Passed && s.BaselineRan && !s.BaselinePassed {
			out = append(out, s)
		}
	}
	return out
}

// formatPreExistingVerifyWarning is written to the job log when the Step is
// allowed through a failing verify. It has to name the repo, the command and
// the failure output: the whole point of continuing is that a human decides
// what to do about the failure, and they can only do that if the log says
// what the failure WAS. A bare "verification failed but looks pre-existing"
// would trade a lost Step for an unexplained green one.
func formatPreExistingVerifyWarning(vr *verifyResult) string {
	steps := preExistingVerifySteps(vr)
	var sb strings.Builder
	sb.WriteString("warning: agent self-verification failed, but the same failure is present on the base commit — committing and opening the PR anyway.\n")
	if len(steps) == 0 {
		// Defensive: agentbox sets pre_existing only from per-step verdicts,
		// so this is unreachable with a well-formed payload. Say what we know
		// rather than printing a header with nothing under it.
		sb.WriteString(fmt.Sprintf("  %s: %s\n", verifyStepRepoLabel(""), verifyCommandLabel(vr.Command)))
		if tail := boundVerifyTail(verifyFailureTail(vr)); tail != "" {
			sb.WriteString("    " + tail + "\n")
		}
		return sb.String()
	}
	for _, s := range steps {
		sb.WriteString(fmt.Sprintf("  %s: %s — fails on the base commit as well\n",
			verifyStepRepoLabel(s.Repo), verifyCommandLabel(s.Command)))
		if tail := boundVerifyTail(verifyStepTail(s)); tail != "" {
			sb.WriteString("    " + tail + "\n")
		}
	}
	return sb.String()
}

// verifyStepTail picks the most useful output for one step, preferring what
// the agent saw over the baseline replay's copy of the same failure.
func verifyStepTail(s verifyStep) string {
	for _, candidate := range []string{s.StderrTail, s.StdoutTail, s.BaselineStderrTail} {
		if tail := strings.TrimSpace(candidate); tail != "" {
			return tail
		}
	}
	return ""
}

func verifyStepRepoLabel(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return "(unnamed repo)"
	}
	return repo
}

func verifyCommandLabel(command string) string {
	if strings.TrimSpace(command) == "" {
		return "(unspecified command)"
	}
	return command
}

// verifyFailureTail picks the most useful output the agent reported and
// bounds it.
func verifyFailureTail(vr *verifyResult) string {
	tail := strings.TrimSpace(vr.StderrTail)
	if tail == "" {
		tail = strings.TrimSpace(vr.StdoutTail)
	}
	return boundVerifyTail(tail)
}

// boundVerifyTail trims and caps a tail for embedding in a Step error, a job
// log line or a PR body.
func boundVerifyTail(tail string) string {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return ""
	}
	if len(tail) > verifyTailMaxBytes {
		// Keep the END: a compiler or test runner puts the failure last,
		// and the head is usually progress output.
		tail = "…" + tail[len(tail)-verifyTailMaxBytes:]
	}
	return tail
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

// resolveRunCost prices this run ONCE, at completion.
//
// Two sources, and which one applied is recorded rather than inferred:
//
//	the agent said so    — Claude Code and opencode both report a total, and a
//	                       figure from the vendor that billed it beats any
//	                       arithmetic of ours
//	we priced it         — codex reports token counts only, so the catalogue's
//	                       (model, provider) rate turns them into dollars
//
// nil when neither is possible: an unpriced pair, an unknown model, or a
// subscription, which has no per-token price at all. Callers must render that
// as an unknown cost — a confident zero tells a customer their run was free.
//
// The provider comes from the Job's own parameter, so the rate is the one for
// the route actually taken. That is the whole reason rates are keyed by
// (model, provider): the same model costs different amounts through Bedrock, a
// direct API, or a gateway.
func resolveRunCost(parameters map[string]interface{}, result agentResult) *costOutput {
	logical, _ := jobs.GetParameterValue[string](parameters, parameters_enums.Model)
	provider := resolveJobProvider(parameters, io.Discard)

	// The agent's own figure wins outright, and is recorded even when we could
	// also have estimated one — measured beats inferred.
	if result.CostUSD != nil {
		return &costOutput{
			USD:      *result.CostUSD,
			Source:   costSourceAgent,
			Model:    logical,
			Provider: provider.String(),
		}
	}

	model, err := llm_provider_enums.GetModel(logical)
	if err != nil {
		return nil
	}
	rate, ok := model.RateFor(provider)
	if !ok {
		return nil
	}
	// Cache writes bill at the fresh-input rate, which UNDERSTATES a
	// cache-heavy Anthropic run — Anthropic charges a premium for them, and
	// Rate has no separate field yet. Harmless for the only models priced here
	// (codex, where cached tokens are a subset of input rather than a
	// separately-billed bucket, so this count is zero), and the count is now
	// passed through so adding that field is the only remaining step.
	return &costOutput{
		USD: rate.CostUSD(
			result.TokenUsage.InputTokens,
			result.TokenUsage.CacheReadTokens,
			result.TokenUsage.CacheCreationTokens,
			result.TokenUsage.OutputTokens,
		),
		Source:   costSourceEstimated,
		Model:    logical,
		Provider: provider.String(),
	}
}

// mergeAgentResultIntoJobOutput writes the agent block of the JobOutput
// envelope. CommitAndPush + OpenPullRequest later extend the same
// envelope's repositories block; the merge-then-write pattern preserves
// each command's contribution.
//
// IT ACCUMULATES rather than overwrites. A Step used to contain exactly one
// agent run, so replacing the block was the same thing as writing it. The
// Review stage can route a must-fix finding back to the implementer, and that
// second run is the same Step's work: overwriting here would report the Step's
// token usage as the FIX's usage alone, erase the implementer's pr_title the
// moment a fix run emitted none, and under-report the Task's cost by however
// much the first run spent.
//
// What replaces and what accumulates follows from what each field means:
// counters add, lists union, the latest verification wins (it is the one that
// describes the code being committed), and a title or summary is only ever
// replaced by a non-empty one.
func mergeAgentResultIntoJobOutput(parameters map[string]interface{}, result agentResult) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.Cost = accumulateCost(data.Cost, resolveRunCost(parameters, result))
	data.Agent = accumulateAgentOutput(data.Agent, result)
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// accumulateAgentOutput folds one implementer run into the Step's agent block.
func accumulateAgentOutput(prev *agentOutput, result agentResult) *agentOutput {
	next := &agentOutput{
		ChangesSummary: result.ChangesSummary,
		FilesChanged:   result.FilesChanged,
		TokenUsage:     result.TokenUsage,
		Turns:          result.Turns,
		CostUSD:        result.CostUSD,
		ExitCode:       result.ExitCode,
		DeniedHosts:    result.DeniedHosts,
		PRTitle:        result.PRTitle,
		VerifyResult:   result.VerifyResult,
	}
	if prev == nil {
		return next
	}
	next.TokenUsage = addTokenUsage(prev.TokenUsage, result.TokenUsage)
	next.Turns = prev.Turns + result.Turns
	next.FilesChanged = unionStrings(prev.FilesChanged, result.FilesChanged)
	next.DeniedHosts = unionStrings(prev.DeniedHosts, result.DeniedHosts)
	next.CostUSD = addOptionalCost(prev.CostUSD, result.CostUSD)
	// A fix run that produced no title must not erase the implementer's: the
	// PR is still titled after the change as a whole.
	if next.PRTitle == "" {
		next.PRTitle = prev.PRTitle
	}
	// The narrative accumulates too, because the commit message is built from
	// it and the fixes are part of what this Step did. A fix run that said
	// nothing leaves the implementer's account standing.
	next.ChangesSummary = appendChangesSummary(prev.ChangesSummary, result.ChangesSummary)
	// The LATEST verification wins — it is the one that ran against the code
	// actually being committed. A nil one does not erase the earlier verdict,
	// because "this run reported no verify" is not "the build is unknown".
	if next.VerifyResult == nil {
		next.VerifyResult = prev.VerifyResult
	}
	return next
}

// accumulateReviewRunUsage folds a REVIEW run's usage and cost into the Step's
// totals, and nothing else.
//
// A review writes no code, so its changes_summary is a description of someone
// else's work and its files_changed is empty; folding those into the agent
// block would put a reviewer's prose into the commit message. But it is an LLM
// run that costs real money, and a Task whose cost omitted its reviews would
// under-report by however much they spent — including for a round that failed
// partway, which still burned the tokens it burned.
func accumulateReviewRunUsage(parameters map[string]interface{}, result agentResult) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.Cost = accumulateCost(data.Cost, resolveRunCost(parameters, result))
	if data.Agent == nil {
		data.Agent = &agentOutput{}
	}
	data.Agent.TokenUsage = addTokenUsage(data.Agent.TokenUsage, result.TokenUsage)
	data.Agent.Turns += result.Turns
	data.Agent.DeniedHosts = unionStrings(data.Agent.DeniedHosts, result.DeniedHosts)
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

func addTokenUsage(a, b tokenUsage) tokenUsage {
	return tokenUsage{
		InputTokens:         a.InputTokens + b.InputTokens,
		OutputTokens:        a.OutputTokens + b.OutputTokens,
		CacheReadTokens:     a.CacheReadTokens + b.CacheReadTokens,
		CacheCreationTokens: a.CacheCreationTokens + b.CacheCreationTokens,
	}
}

// addOptionalCost sums two agent-reported costs, preserving the distinction
// between "no cost reported" (nil) and a real zero. A run that reported one
// and a run that did not sum to the one that did — the alternative is dropping
// a figure we were given.
func addOptionalCost(a, b *float64) *float64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	sum := *a + *b
	return &sum
}

// accumulateCost sums the resolved cost across the runs of one Step.
//
// The PROVENANCE kept is the first one recorded. A Step whose implement run
// was priced by the agent and whose review was estimated is mostly the former,
// and inventing a third source value would break every reader that switches on
// the two that exist. The model and provider are the same for every run in a
// Step today — one credential bundle, one provider per Job — so there is no
// ambiguity to record yet.
func accumulateCost(prev, next *costOutput) *costOutput {
	if prev == nil {
		return next
	}
	if next == nil {
		return prev
	}
	return &costOutput{
		USD:      prev.USD + next.USD,
		Source:   prev.Source,
		Model:    prev.Model,
		Provider: prev.Provider,
	}
}

// unionStrings appends the entries of b that a does not already have,
// preserving first-seen order.
func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, v := range list {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// appendChangesSummary joins the implementer's narrative with a later fix
// run's, under a label so a reader can tell which is which.
func appendChangesSummary(prev, next string) string {
	prev, next = strings.TrimSpace(prev), strings.TrimSpace(next)
	switch {
	case next == "":
		return prev
	case prev == "":
		return next
	}
	return prev + "\n\n[Review fixes]\n" + next
}

// agentboxSpawnSpec is the per-phase container configuration. An empty Cmd
// runs the image's default agent mode; cacheVolume (when set) mounts the
// shared module cache at /cache. Grouped into a struct to keep
// createAgentboxContainer within the parameter limit.
type agentboxSpawnSpec struct {
	// memoryBytes overrides the agentbox memory cap for this spawn. Zero
	// means "use the Task-Step sizing". Assistant sessions set it because
	// they are a different workload in the same container: read-only
	// planning that never builds, held for hours rather than minutes. See
	// resolveSessionLimits.
	memoryBytes int64
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
	// deploy_static_site_preview MCP tool closes over. Nil for the vendor phase.
	previewDeps *agenttools.DeployStaticSitePreviewDeps
	// waitTimeout overrides how long the container may run. Zero means
	// defaultWallClockTimeout, which is what an implement run gets. The
	// Review stage sets it per round, because a review and a bounded fix run
	// are minutes of work and a four-hour cap on them would mean a stuck
	// round eats the whole Step's budget before anyone notices.
	//
	// It can only ever SHORTEN the wait: nothing sets it above the default,
	// so the Review stage cannot push a Step past the runner's existing
	// per-container cap.
	waitTimeout time.Duration
}

// agentMCPSocketHostPath returns the host path for a task's MCP tool socket: a
// sibling of the work dir (NOT inside it) so the socket never appears in /work
// — keeping it out of the agent's file view and CommitAndPush's diff.
func agentMCPSocketHostPath(workDirHost string) string {
	return strings.TrimRight(workDirHost, "/") + "-agent-mcp.sock"
}

// buildStaticSitePreviewDeps assembles the task-scoped context the deploy_static_site_preview MCP tool
// closes over. The preview deploys to the runner's OWN cloud via its IAM role in
// the runner's region — so the only wiring needed is setting the Region job-param
// the cloud_api_clients builders read (a Tasks job doesn't carry one today) and
// handing the tool a lazy client factory + the task scope. See PLAN_agent_driven_
// preview_verify.md (C4: previews are persisted via the TaskPreviews.EnsureV1 RPC).
// taskPreviewStore implements agenttools.PreviewStore for one task + serviceType. It
// bridges the preview tool to deployment-server (EnsureTaskPreview RPC) and the update
// pipeline, mapping agenttools' neutral PreviewState to/from the deployment DTO — so
// agenttools imports neither the RPC client nor the DTO. A web-service or database
// preview tool constructs the same store with its own serviceType.
type taskPreviewStore struct {
	orgID       string
	taskID      string
	serviceType string
}

func (s taskPreviewStore) EnsurePreview(serviceName string) (string, agenttools.PreviewState, error) {
	previewID, existingDistID, existingDomain, err := runnerclient.Get().EnsureTaskPreview(s.orgID, s.taskID, serviceName, s.serviceType)
	if err != nil {
		return "", agenttools.PreviewState{}, err
	}
	return previewID, agenttools.PreviewState{
		CloudFrontDistributionID: existingDistID,
		CloudFrontDomainName:     existingDomain,
	}, nil
}

func (s taskPreviewStore) SavePreview(previewID string, r agenttools.PreviewState) {
	commandUtils.UpdateDeploymentsPipeline.Add(s.orgID, deployments.UpdateDeploymentDtoV1{
		ID:                               previewID,
		CloudfrontDistributionID:         r.CloudFrontDistributionID,
		CloudfrontDistributionArn:        r.CloudFrontDistributionArn,
		CloudfrontDistributionDomainName: r.CloudFrontDomainName,
		Status:                           build_enums.Success,
	})
}

func buildStaticSitePreviewDeps(ctx commandUtils.TaskJobContext, parameters map[string]interface{}, workDirHost string, logsWriter io.Writer) *agenttools.DeployStaticSitePreviewDeps {
	runnerRegion := utils.RunnerData.Get().RunnerRegion
	if rt, err := region_enums.GetType(runnerRegion); err == nil {
		jobs.SetParameterValue[int64](parameters, parameters_enums.Region, int64(rt))
	}
	orgID := ctx.OrganizationID
	taskID := ctx.TaskID
	return &agenttools.DeployStaticSitePreviewDeps{
		OrgID:       orgID,
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
		// Persisted-preview seam (C4): find-or-create the record and save resources back
		// onto it, via deployment-server (the runner has no control-plane DB). Bound to
		// StaticSite; a web-service/database tool builds a store with its own type.
		Store: taskPreviewStore{
			orgID:       orgID,
			taskID:      taskID,
			serviceType: task_previews.ServiceTypeStaticSite,
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
	code, waitErr := waitForContainerExit(waitCtx, cli, containerID, rs.stopSignal, logsWriter)
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
		// The vendor phase is where a small /tmp actually bites: `yarn
		// install` on one ordinary repo peaks at ~505 MB of scratch against a
		// 512 MB tmpfs. See agentboxTmpDirRel.
		"TMPDIR":   agentboxTmpDirInCtr,
		"GOTMPDIR": agentboxTmpDirInCtr,
		// corepack cannot write to the image's /opt/corepack under
		// ReadonlyRootfs — see agentboxCorepackHomeInCtr.
		"COREPACK_HOME": agentboxCorepackHomeInCtr,
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
