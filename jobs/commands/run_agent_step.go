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

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
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
// (read-only rootfs, UID 1000 enforcement, memory/CPU limits, restricted
// bridge network with iptables rules, ADDITIONAL_ALLOWED_HOSTS) lands in
// Phase 5.4. Heartbeat-driven mid-run stop wiring lands when the
// integration tests in Phase 5.5 demonstrate the gap.
type RunAgentStep struct{}

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

	// defaultWallClockTimeout is the runner-side cap on how long agentbox
	// can run. Defense in depth — agentbox's own NO_ACTIVITY_TIMEOUT
	// (10m default) catches stdout-silent hangs; this catches the
	// hypothetical case where agentbox itself hangs (orchestrator bug)
	// or where the agent loops with periodic stdout but never finishes.
	// Per PLAN_tasks Open Question 6: 4h proposed; tune after early
	// usage. Phase 6 wires per-Task / per-org overrides via Task model
	// field + Advanced UI.
	defaultWallClockTimeout = 4 * time.Hour
	// containerStopGraceSec mirrors agentbox's own SIGTERM grace window
	// (per PLAN_agentbox.md). After this many seconds, Docker promotes
	// SIGTERM to SIGKILL.
	containerStopGraceSec = 10

	// Hardened HostConfig defaults. All four are env-var-overridable
	// (see resolveContainerLimits) so different runner instance sizes
	// can dial up/down without a runner redeploy. Phase 6 wires per-org
	// overrides via Settings UI.
	//
	// Memory + CPU sized for typical Tasks workloads. Memory accounts
	// for Claude Code's working set during large-repo analysis (~1GB)
	// + npm/pip install during agentbox startup (~500MB) + headroom.
	// CPU at 2 cores keeps multiple concurrent Step Jobs feasible on
	// a 4-core runner without saturating the host.
	defaultMemoryBytes = 2 * 1024 * 1024 * 1024 // 2 GB
	defaultCPUCores    = int64(2)               // 2 cores

	// Tmpfs sizes. /tmp covers general scratch (build artifacts, npm
	// caches, etc.); /home/agent covers the agentbox runtime install
	// (npm install -g claude-code lands at $NPM_CONFIG_PREFIX which
	// is /home/agent/.npm-global — see agentbox Dockerfile).
	tmpfsTmpOpts  = "rw,size=512m"
	tmpfsHomeOpts = "rw,size=1g"

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
	if err := pullAgentboxImage(imageRef, logsWriter); err != nil {
		return parameters, fmt.Errorf("error pulling agentbox image: %s", err)
	}
	workDirHost := commandUtils.GetTaskRepositoriesBaseDir(ctx.OrganizationID, ctx.TaskID)
	if err := prepareAgentboxResultDir(workDirHost); err != nil {
		return parameters, fmt.Errorf("error preparing agent result dir: %s", err)
	}
	envVars, err := buildAgentSpawnEnvVars(parameters)
	if err != nil {
		return parameters, err
	}
	result, err := spawnAgentboxAndWait(imageRef, workDirHost, envVars, logsWriter)
	if err != nil {
		return parameters, fmt.Errorf("error running agentbox: %s", err)
	}
	if err := mergeAgentResultIntoJobOutput(parameters, result); err != nil {
		return parameters, fmt.Errorf("error merging agent result: %s", err)
	}
	if result.Status != "success" {
		return parameters, fmt.Errorf("agent step did not succeed: status=%s exit_code=%d", result.Status, result.ExitCode)
	}
	return parameters, nil
}

// agentboxImagePullLock serializes image pulls across concurrent Step Jobs
// on the same runner. Mirrors the existing imagePullLock in
// build_static_site.go — Docker's image-pull is idempotent but doing it
// concurrently for the same image causes wasted bandwidth and occasional
// layer-extraction conflicts.
var agentboxImagePullLock sync.Mutex

func pullAgentboxImage(imageRef string, logsWriter io.Writer) error {
	agentboxImagePullLock.Lock()
	defer agentboxImagePullLock.Unlock()
	dockerCtx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	io.WriteString(logsWriter, fmt.Sprintf("Pulling agentbox image: %s\n", imageRef))
	reader, err := cli.ImagePull(dockerCtx, imageRef, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return err
	}
	return nil
}

// prepareAgentboxResultDir creates the on-host directory that agentbox
// writes /result.json into (via the bind mount). Pre-creating ensures the
// directory exists and is writable before the container starts.
func prepareAgentboxResultDir(workDirHost string) error {
	resultDir := filepath.Join(workDirHost, agentboxResultDirRel)
	return os.MkdirAll(resultDir, 0755)
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
// the wall-clock cap fires), then reads /result.json from the
// bind-mounted host dir.
//
// The wall-clock cap scopes to the container-wait phase only — image
// pull and container creation happen on context.Background() so a slow
// network pull doesn't eat into the agent's run budget.
func spawnAgentboxAndWait(imageRef, workDirHost string, envVars []string, logsWriter io.Writer) (agentResult, error) {
	dockerCtx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return agentResult{}, err
	}
	defer cli.Close()
	containerID, err := createAgentboxContainer(dockerCtx, cli, imageRef, workDirHost, envVars)
	if err != nil {
		return agentResult{}, err
	}
	defer func() { _ = removeContainer(dockerCtx, cli, containerID) }()
	if err := cli.ContainerStart(dockerCtx, containerID, container.StartOptions{}); err != nil {
		return agentResult{}, fmt.Errorf("error starting container: %s", err)
	}
	go streamContainerLogs(dockerCtx, cli, containerID, logsWriter)
	waitCtx, cancelWait := context.WithTimeout(dockerCtx, defaultWallClockTimeout)
	defer cancelWait()
	if err := waitForContainerExit(waitCtx, cli, containerID); err != nil {
		return agentResult{}, err
	}
	return readAgentResult(workDirHost)
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
func createAgentboxContainer(ctx context.Context, cli *client.Client, imageRef, workDirHost string, envVars []string) (string, error) {
	cfg := &container.Config{
		Image: imageRef,
		Env:   envVars,
		User:  "1000:1000",
		Tty:   false,
	}
	memoryBytes, nanoCPUs := resolveContainerLimits()
	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: workDirHost,
			Target: agentboxWorkDirInContainer,
		}},
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
	// Docker multiplexes stdout/stderr with an 8-byte header per chunk;
	// for v1 we don't bother demuxing since both streams are useful in
	// the unified log view. Just copy through.
	if _, err := io.Copy(logsWriter, logs); err != nil && err != io.EOF {
		io.WriteString(logsWriter, fmt.Sprintf("error streaming container logs: %s\n", err))
	}
}

func waitForContainerExit(ctx context.Context, cli *client.Client, containerID string) error {
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
			return fmt.Errorf("agentbox exceeded wall-clock cap of %s — SIGTERM sent", defaultWallClockTimeout)
		}
		if err != nil {
			return fmt.Errorf("error waiting for container exit: %s", err)
		}
	case <-statusCh:
		// Container exited; non-zero exit code is signaled via /result.json
		// status, not as a wait error. Surface that decision to the caller.
	}
	return nil
}

func removeContainer(ctx context.Context, cli *client.Client, containerID string) error {
	return cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// agentResult mirrors the shape of agentbox's /result.json. Only the
// fields the runner consumes are pulled out; agentbox can emit
// additional fields without breaking unmarshal.
type agentResult struct {
	Status         string `json:"status"`
	ExitCode       int    `json:"exit_code"`
	AgentVersion   string `json:"agent_version,omitempty"`
	ChangesSummary string `json:"changes_summary,omitempty"`
	TokenUsage     int64  `json:"token_usage,omitempty"`
	TurnCount      int    `json:"turn_count,omitempty"`
	Error          string `json:"error,omitempty"`
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
		ExitCode:       result.ExitCode,
	}
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}
