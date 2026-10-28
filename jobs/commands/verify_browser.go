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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// verifyBrowserImage is the verify-browser release this runner uses. Pinned to an
// exact version; bump it deliberately when a new verify-browser-vX.Y.Z ships.
// 1.3.0 is the first with read_only, which production QA requires; 1.4.0 the
// first with the back step.
const verifyBrowserImage = "docker.io/deploymenthq/verify-browser:1.4.0"

// minReadOnlyVerifyBrowserVersion is the first verify-browser release that
// honours read_only. A production QA run refuses an older image rather than
// run it: an older image ignores read_only, so its checks could change data on
// a real site.
const minReadOnlyVerifyBrowserVersion = "1.3.0"

// minProductionQAAgentboxVersion is the first agentbox release with the
// production QA mode (QA_TARGET=production, QA_URLS).
const minProductionQAAgentboxVersion = "1.9.34"

// verifyBrowserImageEnv overrides both the Job's VerifyBrowserImage and
// verifyBrowserImage, for developing the image only — it is not a customer
// setting.
const verifyBrowserImageEnv = "VERIFY_BROWSER_IMAGE"

const (
	// verifyBrowserSlots is how many browser containers may run at once on a runner.
	verifyBrowserSlots = 2
	// verifyBrowserSlotWait is how long a check waits for a free slot, at most —
	// see slotWaitFor.
	verifyBrowserSlotWait = 20 * time.Second
	// verifyBrowserPrefetchCheckTimeout bounds prefetch's presence check.
	verifyBrowserPrefetchCheckTimeout = 10 * time.Second
	// verifyBrowserCallBudget keeps a whole call under the agent's MCP tool-call
	// timeout (~120 s observed for codex), the same bound verify_preview_reachable
	// keeps: slot wait + the check's timeout + verifyBrowserWallClockExtra.
	verifyBrowserCallBudget = 110 * time.Second
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

// verifyBrowserImageRef picks the image for one check: the runner's
// VERIFY_BROWSER_IMAGE (development override), then jobRef — the Job's
// VerifyBrowserImage, pinned by the control plane across a Task's Steps —
// then verifyBrowserImage.
func verifyBrowserImageRef(jobRef string) string {
	if ref := os.Getenv(verifyBrowserImageEnv); ref != "" {
		return ref
	}
	if jobRef != "" {
		return jobRef
	}
	return verifyBrowserImage
}

// imageVersionAtLeast reports whether ref's tag is a version (X.Y.Z, an
// optional leading "v") of at least min. A ref without a tag, or whose tag
// isn't such a version ("latest", "1.3", "1.3.0-rc1"), is false: a production
// check can't tell what an unversioned image supports.
func imageVersionAtLeast(ref, min string) bool {
	got, ok := imageTagVersion(ref)
	if !ok {
		return false
	}
	want, ok := parseImageVersion(min)
	if !ok {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// imageTagVersion parses the version in ref's tag: the text after the last ':'
// of the last path element, with any @digest removed.
func imageTagVersion(ref string) ([3]int, bool) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return [3]int{}, false
	}
	return parseImageVersion(name[i+1:])
}

func parseImageVersion(tag string) ([3]int, bool) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var v [3]int
	for i, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return [3]int{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
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
	imageRef        func(jobRef string) string
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

	// One pull runs at a time across every ref, but what a check is told is
	// per ref: pullingRef is the ref being pulled ("" when none), and
	// pullErrs holds each ref's last failed pull, so a failure pulling one
	// Task's image is never reported to a check that asked for another.
	mu         sync.Mutex
	pullingRef string
	pullErrs   map[string]error
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
		pullErrs: map[string]error{},
	}
}

// run is agenttools.RunVerifyBrowser.
func (v *verifyBrowser) run(ctx context.Context, req agenttools.VerifyBrowserRequest, logsWriter io.Writer) (agenttools.VerifyBrowserOutcome, error) {
	ref := v.imageRef(req.ImageRef)
	if req.MinImageVersion != "" && !imageVersionAtLeast(ref, req.MinImageVersion) {
		return agenttools.VerifyBrowserOutcome{}, fmt.Errorf("%w: the QA browser image %s doesn't support the back step (it needs verify-browser %s or later); check without it, for example with a goto to the earlier page", agenttools.ErrVerifyBrowserNotRun, ref, req.MinImageVersion)
	}
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
	release, err := v.acquireSlot(ctx, v.slotWaitFor(req.Timeout))
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
		ImageRef:  ref,
		Result:    readVerifyBrowserResult(filepath.Join(ioDir, "result.json")),
		Output:    exit.output,
		ExitCode:  exit.exitCode,
		TimedOut:  exit.timedOut,
		OOMKilled: exit.oomKilled,
	}, nil
}

// ensureImage reports whether the image is on the host. When it isn't, it starts a
// background pull (unless one is running, for this ref or another) and returns false
// — or, when this ref's last pull failed, an error naming the image and that failure,
// after starting a retry.
func (v *verifyBrowser) ensureImage(ctx context.Context, ref string, logsWriter io.Writer) (bool, error) {
	present, err := v.imagePresent(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("%w: checking for the QA browser image %s: %v", agenttools.ErrVerifyBrowserNotRun, ref, err)
	}
	if present {
		return true, nil
	}
	started, lastErr := v.startPull(ref)
	if !started {
		// Another ref's pull holds the one download; this ref's starts on a
		// later check once it ends. Either way the check is "preparing".
		return false, nil
	}
	if logsWriter != nil {
		io.WriteString(logsWriter, fmt.Sprintf("verify_preview_rendered: downloading the QA browser image %s on this runner\n", ref))
	}
	if lastErr != nil {
		return false, fmt.Errorf("%w: downloading the QA browser image %s failed: %v — the download is being retried; call verify_preview_rendered again in a minute", agenttools.ErrVerifyBrowserNotRun, ref, lastErr)
	}
	return false, nil
}

// startPull starts a background pull of ref unless a pull (of any ref) is
// running. When it starts one it reports true and returns — and clears — ref's
// last failed pull, which the new pull retries.
func (v *verifyBrowser) startPull(ref string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.pullingRef != "" {
		return false, nil
	}
	lastErr := v.pullErrs[ref]
	delete(v.pullErrs, ref)
	v.pullingRef = ref
	v.pulled = make(chan struct{})
	go v.pullInBackground(ref, v.pulled)
	return true, lastErr
}

// prefetch starts downloading the image a check for jobRef would use, when it
// isn't on the host and no download is running, so it is usually there by the
// QA agent's first check. It is best-effort: it never waits for the download
// and never fails; a later check behaves as it would without it. ctx bounds
// only the presence check.
func (v *verifyBrowser) prefetch(ctx context.Context, jobRef string, logsWriter io.Writer) {
	ref := v.imageRef(jobRef)
	present, err := v.imagePresent(ctx, ref)
	if err != nil || present {
		return
	}
	started, lastErr := v.startPull(ref)
	if !started || logsWriter == nil {
		return
	}
	if lastErr != nil {
		io.WriteString(logsWriter, fmt.Sprintf("QA: the last download of the QA browser image %s failed (%v); downloading it again\n", ref, lastErr))
		return
	}
	io.WriteString(logsWriter, fmt.Sprintf("QA: downloading the QA browser image %s on this runner before the first check\n", ref))
}

func (v *verifyBrowser) pullInBackground(ref string, done chan struct{}) {
	defer close(done)
	err := v.pullImage(ref)
	if err == nil {
		// Otherwise every verify-browser release this runner pulled stays on disk.
		v.pruneSuperseded(ref)
	}
	v.mu.Lock()
	v.pullingRef = ""
	if err != nil {
		v.pullErrs[ref] = err
	} else {
		delete(v.pullErrs, ref)
	}
	v.mu.Unlock()
}

// pullVerifyBrowserImage pulls ref within the agentbox pull's timeout. Unlike
// pullImageWithTimeout it reads Docker's progress stream: a layer download or
// extraction that fails is reported inside a stream that otherwise ends normally,
// and must not pass for a successful pull.
func pullVerifyBrowserImage(ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultImagePullTimeout)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	reader, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
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

// slotWaitFor is how long a check with this timeout may wait for a slot: slotWait,
// shortened so the wait and the container's wall clock together stay within
// verifyBrowserCallBudget. A 90-second check waits 5 seconds at most.
func (v *verifyBrowser) slotWaitFor(checkTimeout time.Duration) time.Duration {
	wait := verifyBrowserCallBudget - checkTimeout - verifyBrowserWallClockExtra
	if wait > v.slotWait {
		wait = v.slotWait
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// acquireSlot takes one of the runner's browser slots, waiting up to wait.
func (v *verifyBrowser) acquireSlot(ctx context.Context, wait time.Duration) (func(), error) {
	release := func() { <-v.slots }
	select {
	case v.slots <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(wait)
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
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return false, err
	}
	defer cli.Close()
	if _, err := cli.ImageInspect(ctx, ref); err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func dockerCreateContainer(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return "", err
	}
	defer cli.Close()
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// dockerRunVerifyBrowser starts the container and waits for it, at most wallClock:
// then (or when ctx ends) it is stopped with a grace period. Its output and whether
// it was OOM-killed are read before the caller removes it.
func dockerRunVerifyBrowser(ctx context.Context, id string, wallClock time.Duration) (verifyBrowserExit, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return verifyBrowserExit{}, err
	}
	defer cli.Close()
	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return verifyBrowserExit{}, fmt.Errorf("starting the QA browser container: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wallClock)
	defer cancel()
	var exit verifyBrowserExit
	wait := cli.ContainerWait(waitCtx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case status := <-wait.Result:
		exit.exitCode = int(status.StatusCode)
	case err := <-wait.Error:
		if waitCtx.Err() == nil {
			return verifyBrowserExit{}, fmt.Errorf("waiting for the QA browser container: %w", err)
		}
		grace := verifyBrowserStopGraceSec
		_, _ = cli.ContainerStop(context.Background(), id, client.ContainerStopOptions{Timeout: &grace})
		if ctx.Err() != nil {
			return verifyBrowserExit{}, ctx.Err()
		}
		exit.timedOut = true
	}
	exit.output = verifyBrowserContainerOutput(cli, id)
	if info, err := cli.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{}); err == nil && info.Container.State != nil {
		exit.oomKilled = info.Container.State.OOMKilled
	}
	return exit, nil
}

// verifyBrowserContainerOutput returns the tail of the container's stdout and
// stderr, interleaved.
func verifyBrowserContainerOutput(cli *client.Client, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), containerInspectTimeout)
	defer cancel()
	logs, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
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
