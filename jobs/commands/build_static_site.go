package commands

import (
	"context"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/moby/moby/client"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults applied when the runner spawns a static-site build
// container. All four are env-var-overridable so different runner
// instance sizes can dial up/down without a runner redeploy.
//
// Memory + CPU sized for typical npm/webpack workloads on m6a.large
// (2 vCPU, 8 GB host) running alongside other Tasks/build containers.
// Lower than the headroom of the ECS task to leave room for the runner
// itself + concurrent jobs.
//
// PidsLimit defends against fork-bomb-style npm scripts (malicious or
// accidental) — webpack with parallel workers usually peaks around
// ~100 processes, so 1024 leaves ample room.
//
// Wall-clock timeout bounds a runaway/infinite build. Most builds
// complete in <30 min; 1h is generous for large monorepos. A build
// that takes longer than this is genuinely broken — surfacing the
// timeout as an error is preferable to indefinitely tying up a
// runner slot.
const (
	defaultBuildMemoryBytes = 2 * 1024 * 1024 * 1024 // 2 GB
	defaultBuildCPUCores    = int64(2)
	defaultBuildPidsLimit   = int64(1024)
	defaultBuildTimeout     = 1 * time.Hour
	// defaultBuildImagePullTimeout bounds how long pullDockerImageFor-
	// Building waits on Docker Hub before failing the build. Same
	// rationale as defaultImagePullTimeout in run_agent_step.go: the
	// ImagePull stream respects context cancellation, but the prior
	// code passed context.Background() so a stuck pull could hang
	// indefinitely (compounded by imagePullLock serializing concurrent
	// builds onto the same upstream wait).
	defaultBuildImagePullTimeout = 10 * time.Minute

	buildMemoryBytesEnvVar = "BUILD_MEMORY_BYTES"
	buildCPUCoresEnvVar    = "BUILD_CPU_CORES"
)

type BuildStaticSite struct {
}

// decodes envVariables map to key=value slice
func decodeEnvironmentVariablesToSlice(envVariables string) ([]string, error) {
	var envVariablesSlice []string
	variableEntries := strings.Split(envVariables, "\n")
	for _, entry := range variableEntries {
		if len(entry) == 0 {
			continue
		}
		keyValue := strings.Split(entry, "=")
		if len(keyValue) < 2 {
			return nil, fmt.Errorf("env variables not in correct format")
		}
		value := ""
		if len(keyValue) == 2 {
			value = keyValue[1]
		} else {
			for i, s := range keyValue {
				if i > 0 {
					value += s
					if i != (len(keyValue) - 1) {
						value += "="
					}
				}
			}
		}

		envVariablesSlice = append(envVariablesSlice, fmt.Sprintf("%s=%s", keyValue[0], value))
	}
	return envVariablesSlice, nil
}

// execCommand runs command inside containerID and streams output to
// logsWriter. The caller supplies the context so the wall-clock cap
// (defaultBuildTimeout) lives at the call site and a fired deadline
// surfaces here as ctx.Err() — the deferred container removal in Run
// then SIGKILLs the still-running exec.
//
// Pre-existing bug fixed in this revision: the `defer resp.Close()`
// used to run before the err check from ContainerExecAttach, so an
// attach failure nil-panicked. The error from attach was also
// silently swallowed.
func execCommand(ctx context.Context, containerID, repoDir string, command []string, env []string, logsWriter io.Writer) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	config := container.ExecOptions{
		AttachStderr: true,
		AttachStdout: true,
		WorkingDir:   repoDir,
		Cmd:          command,
		Env:          env,
	}

	idResponse, err := cli.ContainerExecCreate(ctx, containerID, config)
	if err != nil {
		return err
	}
	execID := idResponse.ID

	resp, err := cli.ContainerExecAttach(ctx, execID,
		container.ExecAttachOptions{
			Detach: false,
			Tty:    false,
		},
	)
	if err != nil {
		return fmt.Errorf("attach to exec failed: %w", err)
	}
	defer resp.Close()

	// Watchdog: close the hijacked conn when ctx cancels so io.Copy
	// unblocks. ContainerExecAttach hijacks the underlying TCP conn
	// (see moby/client/hijack.go's setupHijackConn) — the conn is
	// raw net.Conn after that, with no awareness of the original ctx.
	// Without this watchdog a wall-clock-timeout firing would not
	// unblock io.Copy below; we'd hang waiting for the misbehaving
	// build to produce more output, defeating the timeout entirely.
	copyDone := make(chan struct{})
	defer close(copyDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = resp.Conn.Close()
		case <-copyDone:
		}
	}()

	if _, err := io.Copy(logsWriter, resp.Reader); err != nil && ctx.Err() != nil {
		// Copy returned because the watchdog closed the conn after
		// ctx fired. Surface the deadline error so the caller sees
		// the wall-clock cap rather than a generic conn-closed error.
		return fmt.Errorf("build exceeded wall-clock cap of %s", defaultBuildTimeout)
	}

	// Use a fresh background context for Inspect — ctx may have fired
	// while io.Copy was running but the exec finished cleanly anyway,
	// in which case we still want the real exit code instead of
	// ctx.Err().
	res, err := cli.ContainerExecInspect(context.Background(), execID)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("error running command: %v", command)
	}
	return nil
}

var imagePullLock = sync.Mutex{}

func pullDockerImageForBuilding(imageID string) error {
	imagePullLock.Lock()
	defer imagePullLock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), defaultBuildImagePullTimeout)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	reader, err := cli.ImagePull(ctx, fmt.Sprintf("docker.io/library/%s", imageID), image.PullOptions{})
	if err != nil {
		return err
	}

	defer reader.Close()

	if _, err := io.ReadAll(reader); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("image pull exceeded %s timeout for %s", defaultBuildImagePullTimeout, imageID)
		}
		return err
	}

	return nil
}

// startBuildContainer creates and starts the build container with
// hardened HostConfig:
//   - Memory + NanoCPUs caps (env-var-overridable via BUILD_MEMORY_BYTES
//     / BUILD_CPU_CORES) bound a runaway build's blast radius on the
//     shared runner host.
//   - PidsLimit bounds fork-bomb-style npm scripts.
//   - ExtraHosts pins cloud-metadata endpoints to 127.0.0.1 so a
//     compromised npm postinstall script can't exfiltrate the runner's
//     IAM credentials via 169.254.169.254.
//
// The build container intentionally still runs as root with default
// caps and full network access — that's necessary to keep "any npm
// build just works" (many install scripts chown/chmod, fetch from
// arbitrary CDNs, etc.). Tightening those further requires per-deploy
// allowlist work tracked separately.
func startBuildContainer(imageId, repoDir string) (string, error) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", err
	}
	defer cli.Close()

	memoryBytes, nanoCPUs := resolveBuildLimits()
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: imageId,
		Cmd:   []string{"tail", "-f", "/dev/null"},
		Tty:   false,
	}, &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: repoDir,
			Target: repoDir,
		}},
		Resources: container.Resources{
			Memory:    memoryBytes,
			NanoCPUs:  nanoCPUs,
			PidsLimit: pidsLimitPtr(defaultBuildPidsLimit),
		},
		ExtraHosts: cloudMetadataExtraHosts(),
	}, nil, nil, "")
	if err != nil {
		return "", err
	}

	if err = cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", err
	}

	return resp.ID, nil
}

// resolveBuildLimits returns the memory (bytes) and CPU (NanoCPUs)
// caps for the build container. Reads per-runner env-var overrides
// before falling back to the defaults — different EC2 instance sizes
// need different limits without redeploying the runner. Invalid env
// values fall back silently (logging from a const-style helper would
// obscure the actual runner logs).
//
// 1 CPU core = 1e9 NanoCPUs in Docker's accounting.
//
// Mirrors resolveContainerLimits in run_agent_step.go but reads
// BUILD_* env vars instead — keeps the build and Tasks knobs
// independent so ops can tune them separately when concurrent
// build/agentbox jobs need different resource shapes.
func resolveBuildLimits() (memoryBytes int64, nanoCPUs int64) {
	memoryBytes = defaultBuildMemoryBytes
	if v := os.Getenv(buildMemoryBytesEnvVar); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			memoryBytes = parsed
		}
	}
	cores := defaultBuildCPUCores
	if v := os.Getenv(buildCPUCoresEnvVar); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			cores = parsed
		}
	}
	nanoCPUs = cores * 1_000_000_000
	return memoryBytes, nanoCPUs
}

// cloudMetadataExtraHosts returns the /etc/hosts pins applied to every
// build container. Same set used by run_agent_step.createAgentbox-
// Container — direct-IP defense for clients that bypass HTTP_PROXY,
// hostname defense for clients that go through nss.
//
// The 169.254.169.254 entry is best-effort — most clients hitting an
// IP literal don't consult /etc/hosts, but the cost is zero and the
// few clients that do consult nss are caught.
func cloudMetadataExtraHosts() []string {
	return []string{
		"metadata.google.internal:127.0.0.1", // GCP metadata
		"metadata.goog:127.0.0.1",            // GCP metadata (alias)
		"169.254.169.254:127.0.0.1",          // AWS / Azure / OpenStack IMDS
	}
}

// pidsLimitPtr is a one-liner for the *int64 PidsLimit field. Avoids
// littering call sites with intermediate variables just to take an
// address of a constant.
func pidsLimitPtr(n int64) *int64 { return &n }

// removeBuildContainer force-removes the container, killing it first
// if still running. Replaces the prior stop-only path that leaked
// stopped containers indefinitely (consuming disk + the per-instance
// container slot count). Mirrors agentbox's removeContainer in
// run_agent_step.go.
func removeBuildContainer(containerID string) error {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

func (b *BuildStaticSite) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()
	repoDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Building static site\n"))

	//checking if package.json file exists
	if _, err = os.Stat(repoDirectoryPath + "/package.json"); err != nil {
		if os.IsNotExist(err) {
			io.WriteString(logsWriter, fmt.Sprintf("package.json file doesn't exists in root directory\n"))
			return parameters, err
		} else {
			return parameters, err
		}
	}

	buildCommand, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildCommand)
	if err != nil {
		return parameters, err
	}

	nodeVersion, err := jobs.GetParameterValue[string](parameters, parameters_enums.NodeVersion)
	if err != nil {
		return parameters, err
	}

	//if node version is missing install and use latest lts
	//get node docker image id according to node version
	imageId := "node:lts-buster"
	if len(nodeVersion) == 0 {
		nodeVersion = "--lts"
	}

	envVariables, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentVariables)
	var envVariablesSlice []string
	if err == nil {
		envVariablesSlice, err = decodeEnvironmentVariablesToSlice(envVariables)
		if err != nil {
			return parameters, err
		}
	}

	err = pullDockerImageForBuilding(imageId)
	if err != nil {
		return parameters, err
	}

	containerID, err := startBuildContainer(imageId, repoDirectoryPath)
	if err != nil {
		return parameters, err
	}

	// Force-remove the container on exit (success or failure). Replaces
	// the previous stop-only path that leaked stopped containers and
	// guarantees cleanup if the wall-clock timeout below SIGKILLs the
	// exec mid-run.
	defer func() { _ = removeBuildContainer(containerID) }()

	// Wall-clock cap on the npm install + build phase. A build that
	// exceeds this is genuinely broken — surface the deadline as an
	// error rather than tying up a runner slot indefinitely.
	execCtx, cancelExec := context.WithTimeout(context.Background(), defaultBuildTimeout)
	defer cancelExec()
	err = execCommand(execCtx, containerID, repoDirectoryPath, []string{"bash", "-c", "npm install;" + buildCommand}, envVariablesSlice, logsWriter)
	if err != nil {
		return parameters, err
	}

	return parameters, nil
}
