package commands

// verify_browser.go runs verify_preview_rendered's checks: one verify-browser
// container per check (headless Chromium, built and released from the agentbox
// repository — see its docs/VERIFY_BROWSER.md for the contract). The runner writes
// input.json into a fresh per-call directory mounted at /io, starts the container
// with no credentials, no environment, no /work and every privilege dropped, and
// reads /io/result.json when it exits.
//
// The image is pulled in the background on first use — a check that finds it
// missing returns "preparing" at once rather than holding the agent's tool call
// for a multi-minute download — and at most verifyBrowserSlots browsers run at once
// per runner, whatever the number of Steps.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// verifyBrowserImage is the verify-browser release this runner uses. Pinned to an
// exact version; bump it deliberately when a new verify-browser-vX.Y.Z ships.
const verifyBrowserImage = "docker.io/deploymenthq/verify-browser:1.0.0"

// verifyBrowserImageEnv overrides verifyBrowserImage, for developing the image
// only — it is not a customer setting.
const verifyBrowserImageEnv = "VERIFY_BROWSER_IMAGE"

const (
	// verifyBrowserSlots is how many browser containers may run at once on a runner.
	verifyBrowserSlots = 2
	// verifyBrowserSlotWait is how long a check waits for a free slot.
	verifyBrowserSlotWait = 20 * time.Second
	// verifyBrowserWallClockExtra is added to the check's own timeout for the
	// container's start, browser launch and shutdown.
	verifyBrowserWallClockExtra = 15 * time.Second
	verifyBrowserStopGraceSec   = 10

	verifyBrowserIODir     = "/io"
	verifyBrowserHome      = "/home/verify"
	verifyBrowserTmpfsOpts = "rw,nosuid,nodev,size=256m,uid=1000,gid=1000,mode=0700"
	verifyBrowserShmSize   = 256 * 1024 * 1024
	verifyBrowserMemory    = 1024 * 1024 * 1024
	verifyBrowserNanoCPUs  = 1_000_000_000
	verifyBrowserPidsLimit = 512

	// verifyBrowserOutputMax caps the container output kept for the Step log.
	verifyBrowserOutputMax = 64 * 1024
	// verifyBrowserResultMax caps the result.json read back.
	verifyBrowserResultMax = 1024 * 1024
)

func verifyBrowserImageRef() string {
	if ref := os.Getenv(verifyBrowserImageEnv); ref != "" {
		return ref
	}
	return verifyBrowserImage
}

// verifyBrowserExit is how a verify-browser container ended.
type verifyBrowserExit struct {
	exitCode  int
	timedOut  bool
	oomKilled bool
	output    string
}

// verifyBrowser runs checks. Its Docker calls are fields so tests can replace them.
type verifyBrowser struct {
	imageRef        func() string
	imagePresent    func(ctx context.Context, ref string) (bool, error)
	pullImage       func(ref string) error
	pruneSuperseded func(ref string)
	createContainer func(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error)
	runContainer    func(ctx context.Context, id string, wallClock time.Duration) (verifyBrowserExit, error)
	removeContainer func(id string)
	chown           func(path string) error
	ioRoot          string

	slots    chan struct{}
	slotWait time.Duration

	mu      sync.Mutex
	pulling bool
	pullErr error
	// pulled is closed when a background pull finishes (tests wait on it).
	pulled chan struct{}
}

// runnerVerifyBrowser is the runner's one verify-browser service: the image
// download and the container slots are per runner, not per Step.
var runnerVerifyBrowser = newVerifyBrowser()

func newVerifyBrowser() *verifyBrowser {
	return &verifyBrowser{
		imageRef:     verifyBrowserImageRef,
		imagePresent: dockerImagePresent,
		pullImage:    pullVerifyBrowserImage,
		pruneSuperseded: func(ref string) {
			reclaim.PruneSupersededImages(ref, reclaim.Supersession{SameRepository: true}, nil)
		},
		createContainer: dockerCreateContainer,
		runContainer:    dockerRunVerifyBrowser,
		removeContainer: func(id string) { _ = removeBuildContainer(id) },
		chown: func(path string) error {
			return os.Chown(path, commandUtils.AgentboxUID, commandUtils.AgentboxGID)
		},
		ioRoot:   "/tmp",
		slots:    make(chan struct{}, verifyBrowserSlots),
		slotWait: verifyBrowserSlotWait,
	}
}

// run is agenttools.RunVerifyBrowser.
func (v *verifyBrowser) run(ctx context.Context, req agenttools.VerifyBrowserRequest, logsWriter io.Writer) (agenttools.VerifyBrowserOutcome, error) {
	ref := v.imageRef()
	// Held from before the presence check so a concurrent prune can't remove the
	// image between the check and the container's start.
	releaseImage := reclaim.MarkImageInUse(ref)
	defer releaseImage()
	ready, err := v.ensureImage(ctx, ref, logsWriter)
	if err != nil {
		return agenttools.VerifyBrowserOutcome{}, err
	}
	if !ready {
		return agenttools.VerifyBrowserOutcome{Preparing: true}, nil
	}
	release, err := v.acquireSlot(ctx)
	if err != nil {
		return agenttools.VerifyBrowserOutcome{}, err
	}
	defer release()
	ioDir, err := v.prepareIODir(req.Input)
	if err != nil {
		return agenttools.VerifyBrowserOutcome{}, fmt.Errorf("preparing the QA browser's directory: %w", err)
	}
	defer os.RemoveAll(ioDir)
	id, err := v.createContainer(ctx, verifyBrowserContainerConfig(ref), verifyBrowserHostConfig(ioDir))
	if err != nil {
		return agenttools.VerifyBrowserOutcome{}, fmt.Errorf("creating the QA browser container: %w", err)
	}
	defer v.removeContainer(id)
	exit, err := v.runContainer(ctx, id, req.Timeout+verifyBrowserWallClockExtra)
	if err != nil {
		return agenttools.VerifyBrowserOutcome{}, err
	}
	return agenttools.VerifyBrowserOutcome{
		Result:    readVerifyBrowserResult(filepath.Join(ioDir, "result.json")),
		Output:    exit.output,
		ExitCode:  exit.exitCode,
		TimedOut:  exit.timedOut,
		OOMKilled: exit.oomKilled,
	}, nil
}

// ensureImage reports whether the image is on the host. When it isn't, it starts a
// background pull (unless one is running) and returns false — or, when the last pull
// failed, an error naming the image and that failure, after starting a retry.
func (v *verifyBrowser) ensureImage(ctx context.Context, ref string, logsWriter io.Writer) (bool, error) {
	present, err := v.imagePresent(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("%w: checking for the QA browser image %s: %v", agenttools.ErrVerifyBrowserNotRun, ref, err)
	}
	if present {
		return true, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pulling {
		return false, nil
	}
	lastErr := v.pullErr
	v.pullErr = nil
	v.pulling = true
	v.pulled = make(chan struct{})
	if logsWriter != nil {
		io.WriteString(logsWriter, fmt.Sprintf("verify_preview_rendered: downloading the QA browser image %s on this runner\n", ref))
	}
	go v.pullInBackground(ref, v.pulled)
	if lastErr != nil {
		return false, fmt.Errorf("%w: downloading the QA browser image %s failed: %v — the download is being retried; call verify_preview_rendered again in a minute", agenttools.ErrVerifyBrowserNotRun, ref, lastErr)
	}
	return false, nil
}

func (v *verifyBrowser) pullInBackground(ref string, done chan struct{}) {
	defer close(done)
	err := v.pullImage(ref)
	if err == nil {
		// Otherwise every verify-browser release this runner pulled stays on disk.
		v.pruneSuperseded(ref)
	}
	v.mu.Lock()
	v.pulling = false
	v.pullErr = err
	v.mu.Unlock()
}

// pullVerifyBrowserImage pulls ref within the agentbox pull's timeout. Unlike
// pullImageWithTimeout it reads Docker's progress stream: a layer download or
// extraction that fails is reported inside a stream that otherwise ends normally,
// and must not pass for a successful pull.
func pullVerifyBrowserImage(ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultImagePullTimeout)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	reader, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := checkPullStream(reader); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("image pull exceeded %s timeout for %s", defaultImagePullTimeout, ref)
		}
		return err
	}
	return nil
}

// checkPullStream reads an image pull's JSON progress stream to the end and returns
// the first error it reports, or the error reading it.
func checkPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var line ErrorLine
		err := dec.Decode(&line)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the image pull's progress: %w", err)
		}
		if msg := line.ErrorDetail.Message; msg != "" {
			return errors.New(msg)
		}
		if line.Error != "" {
			return errors.New(line.Error)
		}
	}
}

// acquireSlot takes one of the runner's browser slots, waiting up to slotWait.
func (v *verifyBrowser) acquireSlot(ctx context.Context) (func(), error) {
	release := func() { <-v.slots }
	select {
	case v.slots <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(v.slotWait)
	defer timer.Stop()
	select {
	case v.slots <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: the QA browser is busy with other Tasks on this runner — call verify_preview_rendered again in a minute", agenttools.ErrVerifyBrowserNotRun)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// prepareIODir creates the per-call directory mounted at /io, owned by the image's
// uid 1000, holding input.json.
func (v *verifyBrowser) prepareIODir(input []byte) (string, error) {
	dir, err := os.MkdirTemp(v.ioRoot, "verify-browser-")
	if err != nil {
		return "", err
	}
	inputPath := filepath.Join(dir, "input.json")
	err = os.WriteFile(inputPath, input, 0o644)
	if err == nil {
		err = v.chown(inputPath)
	}
	if err == nil {
		err = v.chown(dir)
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// verifyBrowserContainerConfig: the image's own entrypoint, no Env and no Cmd — the
// only input is /io/input.json.
func verifyBrowserContainerConfig(ref string) *container.Config {
	return &container.Config{
		Image: ref,
		User:  "1000:1000",
		// Labelled so the startup sweep can reclaim it if the deferred removal never
		// runs, and tell it apart from a customer's containers.
		Labels: reclaim.Labels(reclaim.PurposeVerifyBrowser),
	}
}

// verifyBrowserHostConfig is the sandbox the browser runs in: Chromium runs without
// its own sandbox, so the container is the boundary.
func verifyBrowserHostConfig(ioDir string) *container.HostConfig {
	return &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: ioDir,
			Target: verifyBrowserIODir,
		}},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":            verifyBrowserTmpfsOpts,
			verifyBrowserHome: verifyBrowserTmpfsOpts,
		},
		ShmSize: verifyBrowserShmSize,
		Resources: container.Resources{
			Memory:    verifyBrowserMemory,
			NanoCPUs:  verifyBrowserNanoCPUs,
			PidsLimit: pidsLimitPtr(verifyBrowserPidsLimit),
		},
		ExtraHosts:  cloudMetadataExtraHosts(),
		NetworkMode: container.NetworkMode("bridge"),
	}
}

// readVerifyBrowserResult returns result.json, or nil when there is none (or it is
// too large to be one).
func readVerifyBrowserResult(path string) []byte {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > verifyBrowserResultMax {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

func dockerImagePresent(ctx context.Context, ref string) (bool, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false, err
	}
	defer cli.Close()
	if _, _, err := cli.ImageInspectWithRaw(ctx, ref); err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func dockerCreateContainer(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", err
	}
	defer cli.Close()
	resp, err := cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// dockerRunVerifyBrowser starts the container and waits for it, at most wallClock:
// then (or when ctx ends) it is stopped with a grace period. Its output and whether
// it was OOM-killed are read before the caller removes it.
func dockerRunVerifyBrowser(ctx context.Context, id string, wallClock time.Duration) (verifyBrowserExit, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return verifyBrowserExit{}, err
	}
	defer cli.Close()
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return verifyBrowserExit{}, fmt.Errorf("starting the QA browser container: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wallClock)
	defer cancel()
	var exit verifyBrowserExit
	statusCh, errCh := cli.ContainerWait(waitCtx, id, container.WaitConditionNotRunning)
	select {
	case status := <-statusCh:
		exit.exitCode = int(status.StatusCode)
	case err := <-errCh:
		if waitCtx.Err() == nil {
			return verifyBrowserExit{}, fmt.Errorf("waiting for the QA browser container: %w", err)
		}
		grace := verifyBrowserStopGraceSec
		_ = cli.ContainerStop(context.Background(), id, container.StopOptions{Timeout: &grace})
		if ctx.Err() != nil {
			return verifyBrowserExit{}, ctx.Err()
		}
		exit.timedOut = true
	}
	exit.output = verifyBrowserContainerOutput(cli, id)
	if info, err := cli.ContainerInspect(context.Background(), id); err == nil && info.State != nil {
		exit.oomKilled = info.State.OOMKilled
	}
	return exit, nil
}

// verifyBrowserContainerOutput returns the tail of the container's stdout and
// stderr, interleaved.
func verifyBrowserContainerOutput(cli *client.Client, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), containerInspectTimeout)
	defer cancel()
	logs, err := cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer logs.Close()
	tail := &tailWriter{max: verifyBrowserOutputMax}
	_, _ = stdcopy.StdCopy(tail, tail, logs)
	return string(tail.buf)
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append([]byte(nil), t.buf[over:]...)
	}
	return len(p), nil
}
