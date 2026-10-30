package commands

// build_docker_image_buildkit.go builds a deployment's image with BuildKit when
// its Job carries build secrets (a Dockerfile's RUN --mount=type=secret needs
// BuildKit; the classic builder refuses it). Every other build stays on the
// classic builder in build_docker_image.go.
//
// The build runs in a rootless buildkitd container started for it — capped
// like the classic build, labelled for the startup sweep, removed when the
// build ends — and buildctl is driven through the Docker API, so the runner
// takes no BuildKit dependency. buildctl writes the image as a tar into a
// per-build directory, which is loaded into the host daemon for
// UploadDockerImageToEcr to tag and push as it does a classic build's image.
//
// Secret values reach buildctl only through its exec's environment, under
// prefixed names; they never appear in its argv, a log line or an error, and
// the build's output is redacted before it reaches the job log.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/containerd/errdefs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/jobs/resources"
	"github.com/deployment-io/deployment-runner/utils/hostinfo"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/deployment-io/deployment-runner/utils/redact"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	// buildkitImage is the rootless BuildKit release builds with secrets use.
	// Pinned; bump it deliberately.
	buildkitImage = "docker.io/moby/buildkit:v0.34.1-rootless"
	// buildkitStateDir is where the image's buildkitd keeps its state and cache.
	buildkitStateDir = "/home/user/.local/share/buildkit"
	// buildkitCacheVolumePrefix, followed by the DeploymentID, names a
	// service's volume holding buildkitd's state across its builds. One build
	// uses one at a time (buildkitBuildLock).
	buildkitCacheVolumePrefix = "buildkit-cache-"
	// buildkitCacheVolumesKept is how many service cache volumes a runner
	// keeps: the one being built and the most recently used other one.
	buildkitCacheVolumesKept = 2
	// buildkitCacheKeepMB is the cache budget `buildctl prune --keep-storage`
	// trims to after every build.
	buildkitCacheKeepMB = 5120
	// buildkitPidsLimit is generous: the classic build has no PID limit.
	buildkitPidsLimit = 4096
	// buildkitSecretEnvPrefix prefixes each secret's variable in buildctl's
	// environment, so a secret named PATH, HOME or BUILDKIT_HOST doesn't
	// replace the variable buildctl itself runs with.
	buildkitSecretEnvPrefix = "DEPLOYMENT_IO_BUILD_SECRET_"
	// buildkitImageTar is the image buildctl writes into the output directory.
	buildkitImageTar = "image.tar"

	// buildkitBuildTimeout bounds the build, as imageBuild's does.
	buildkitBuildTimeout = 30 * time.Minute
	// buildkitReadyTimeout bounds the wait for buildkitd to answer.
	buildkitReadyTimeout  = 60 * time.Second
	buildkitReadyInterval = time.Second
	// buildkitProbeTimeout bounds one readiness probe.
	buildkitProbeTimeout = 10 * time.Second
	// buildkitPruneTimeout bounds the cache prune after a build.
	buildkitPruneTimeout = 5 * time.Minute
	// buildkitLoadTimeout bounds loading the built image into the host daemon.
	buildkitLoadTimeout = 10 * time.Minute
	// buildkitLogLines is how many of buildkitd's last log lines a failed
	// start reports.
	buildkitLogLines = 20
)

// buildkitBuildLock lets one BuildKit build run at a time on a runner: the
// cache volume is used by one build at a time, and a build's start removes
// any other buildkit container as a leftover.
var buildkitBuildLock sync.Mutex

// buildkitCaches is what this runner process knows of the service cache
// volumes it created; read and written under buildkitBuildLock, never stored
// anywhere else.
var buildkitCaches = newBuildkitCacheState()

type buildkitCacheState struct {
	// secretsHash is, per DeploymentID, the hash of the build secrets the
	// service's volume was last used with (buildkitSecretsHash).
	secretsHash map[string][sha256.Size]byte
	// lru lists the DeploymentIDs whose volume this process created, least
	// recently used first.
	lru []string
}

func newBuildkitCacheState() *buildkitCacheState {
	return &buildkitCacheState{secretsHash: map[string][sha256.Size]byte{}}
}

// touch moves deploymentID to the most recently used end of the LRU.
func (c *buildkitCacheState) touch(deploymentID string) {
	c.forget(deploymentID)
	c.lru = append(c.lru, deploymentID)
}

func (c *buildkitCacheState) forget(deploymentID string) {
	for i, id := range c.lru {
		if id == deploymentID {
			c.lru = append(c.lru[:i:i], c.lru[i+1:]...)
			return
		}
	}
}

// buildkitSecretsHash is a SHA-256 of the secrets, names sorted, each name
// and value length-prefixed so that different sets never hash alike.
// BuildKit leaves secret values out of its cache keys, so a cache is reused
// only with the secrets it was built with.
func buildkitSecretsHash(secrets map[string]string) [sha256.Size]byte {
	h := sha256.New()
	var n [8]byte
	write := func(s string) {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	for _, name := range sortedNames(secrets) {
		write(name)
		write(secrets[name])
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func buildkitCacheVolumeName(deploymentID string) string {
	return buildkitCacheVolumePrefix + deploymentID
}

// buildkitRequest is one image build with build secrets.
type buildkitRequest struct {
	deploymentID    string
	repoDir         string
	dockerFile      string
	imageNameAndTag string
	buildArgs       map[string]*string
	secrets         map[string]string
}

// buildkitBuilder runs BuildKit builds. Its Docker calls and host facts are
// fields so tests can replace them.
type buildkitBuilder struct {
	maxUserNamespaces func() (int64, bool)
	limits            func() (memoryBytes int64, cores int64)
	listContainers    func(ctx context.Context) ([]string, error)
	imagePresent      func(ctx context.Context, ref string) (bool, error)
	pullImage         func(ref string) error
	createVolume      func(ctx context.Context, name string) error
	removeVolume      func(ctx context.Context, name string) error
	createContainer   func(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error)
	startContainer    func(ctx context.Context, id string) error
	exec              func(ctx context.Context, id string, cmd, env []string, out io.Writer) (int, error)
	logsTail          func(id string) string
	oomKilled         func(id string) bool
	removeContainer   func(id string)
	loadImage         func(tarPath string, logsWriter io.Writer) error
	chown             func(path string) error
	outputRoot        string
	buildTimeout      time.Duration
	readyTimeout      time.Duration
	readyInterval     time.Duration
}

var runnerBuildkitBuilder = newBuildkitBuilder()

func newBuildkitBuilder() *buildkitBuilder {
	return &buildkitBuilder{
		maxUserNamespaces: hostinfo.MaxUserNamespaces,
		limits:            resources.LimitsForImageBuild,
		listContainers:    listBuildkitContainers,
		imagePresent:      dockerImagePresent,
		// Pulls ref within defaultImagePullTimeout and reads the progress
		// stream for a failure; nothing in it is verify-browser's own.
		pullImage:       pullVerifyBrowserImage,
		createVolume:    createBuildkitCacheVolume,
		removeVolume:    removeBuildkitCacheVolume,
		createContainer: dockerCreateContainer,
		startContainer:  dockerStartContainer,
		exec:            dockerExec,
		logsTail:        buildkitLogsTail,
		oomKilled:       dockerContainerOOMKilled,
		removeContainer: func(id string) { _ = removeBuildContainer(id) },
		loadImage:       dockerLoadImage,
		chown: func(path string) error {
			return os.Chown(path, commandUtils.AgentboxUID, commandUtils.AgentboxGID)
		},
		outputRoot:    reclaim.BuildkitOutputRoot,
		buildTimeout:  buildkitBuildTimeout,
		readyTimeout:  buildkitReadyTimeout,
		readyInterval: buildkitReadyInterval,
	}
}

// buildImageWithBuildkit is Run's build for a Job with build secrets.
func buildImageWithBuildkit(parameters map[string]interface{}, req buildkitRequest, logsWriter io.Writer) error {
	buildArgs, err := commandUtils.GetDockerBuildArgs(parameters)
	if err != nil {
		return err
	}
	req.buildArgs = buildArgs
	io.WriteString(logsWriter, fmt.Sprintf("Building docker image with BuildKit (build secrets: %s)\n", strings.Join(sortedNames(req.secrets), ", ")))
	return runnerBuildkitBuilder.build(req, logsWriter)
}

// build builds req's image and loads it into the host daemon.
func (b *buildkitBuilder) build(req buildkitRequest, logsWriter io.Writer) error {
	if n, ok := b.maxUserNamespaces(); ok && n == 0 {
		return errors.New("This runner host has user namespaces disabled, which build secrets need; update the runner's CloudFormation stack.")
	}
	buildkitBuildLock.Lock()
	defer buildkitBuildLock.Unlock()
	releaseImage := reclaim.MarkImageInUse(buildkitImage)
	defer releaseImage()
	if err := b.prepare(req, logsWriter); err != nil {
		return err
	}
	outputDir, err := os.MkdirTemp(b.outputRoot, reclaim.BuildkitOutputDirPattern)
	if err != nil {
		return fmt.Errorf("creating the BuildKit output directory: %w", err)
	}
	defer os.RemoveAll(outputDir)
	if err := b.chown(outputDir); err != nil {
		return fmt.Errorf("preparing the BuildKit output directory: %w", err)
	}
	memoryBytes, cores := b.limits()
	id, err := b.startBuildkitd(req.repoDir, outputDir, buildkitCacheVolumeName(req.deploymentID), memoryBytes, cores)
	if err != nil {
		return err
	}
	defer b.removeContainer(id)
	if err := b.waitReady(id); err != nil {
		return err
	}
	err = b.runBuild(id, req, outputDir, logsWriter)
	b.prune(id, logsWriter)
	if err != nil {
		return b.explainFailure(id, err, memoryBytes)
	}
	tarPath := filepath.Join(outputDir, buildkitImageTar)
	if err := b.loadImage(tarPath, logsWriter); err != nil {
		return fmt.Errorf("loading the built image: %w", err)
	}
	return os.Remove(tarPath)
}

// prepare removes a leftover buildkit container, makes sure the image is on
// the host, resets or keeps the service's cache volume, removes the older
// services' volumes and creates the service's.
func (b *buildkitBuilder) prepare(req buildkitRequest, logsWriter io.Writer) error {
	ctx := context.Background()
	// A buildkit container here is a runner's that died mid-build: this
	// build holds buildkitBuildLock. The startup sweep leaves a running one.
	leftovers, err := b.listContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing BuildKit containers: %w", err)
	}
	for _, id := range leftovers {
		b.removeContainer(id)
	}
	present, err := b.imagePresent(ctx, buildkitImage)
	if err != nil {
		return fmt.Errorf("checking for the BuildKit image %s: %w", buildkitImage, err)
	}
	if !present {
		io.WriteString(logsWriter, fmt.Sprintf("Pulling %s\n", buildkitImage))
		if err := b.pullImage(buildkitImage); err != nil {
			return fmt.Errorf("pulling the BuildKit image %s: %w", buildkitImage, err)
		}
	}
	// No buildkit container is left to mount a cache volume now.
	if err := b.resetCacheVolume(ctx, req, logsWriter); err != nil {
		return err
	}
	b.evictCacheVolumes(ctx, logsWriter)
	if err := b.createVolume(ctx, buildkitCacheVolumeName(req.deploymentID)); err != nil {
		return fmt.Errorf("creating the BuildKit cache volume: %w", err)
	}
	return nil
}

// resetCacheVolume removes the service's cache volume unless this process
// last built the service with the same build secrets, then records them. A
// volume from before this process is stale.
func (b *buildkitBuilder) resetCacheVolume(ctx context.Context, req buildkitRequest, logsWriter io.Writer) error {
	hash := buildkitSecretsHash(req.secrets)
	previous, built := buildkitCaches.secretsHash[req.deploymentID]
	if !built || previous != hash {
		if built {
			io.WriteString(logsWriter, "Build secrets changed since this service's last build on this runner: building without the BuildKit cache\n")
		}
		if !b.removeCacheVolume(ctx, buildkitCacheVolumeName(req.deploymentID), logsWriter) {
			return errors.New("could not reset the BuildKit cache for this service")
		}
	}
	buildkitCaches.secretsHash[req.deploymentID] = hash
	buildkitCaches.touch(req.deploymentID)
	return nil
}

// evictCacheVolumes removes the cache volumes of every service but the one
// being built (the most recently used) and the most recently used other one,
// least recently used first. Their builds are over: buildkitBuildLock is
// held. A volume that can't be removed stays listed, to be tried again.
func (b *buildkitBuilder) evictCacheVolumes(ctx context.Context, logsWriter io.Writer) {
	evict := len(buildkitCaches.lru) - buildkitCacheVolumesKept
	if evict <= 0 {
		return
	}
	for _, deploymentID := range append([]string(nil), buildkitCaches.lru[:evict]...) {
		if b.removeCacheVolume(ctx, buildkitCacheVolumeName(deploymentID), logsWriter) {
			delete(buildkitCaches.secretsHash, deploymentID)
			buildkitCaches.forget(deploymentID)
		}
	}
}

// removeCacheVolume removes a cache volume, logging a failure; a volume that
// isn't there counts as removed.
func (b *buildkitBuilder) removeCacheVolume(ctx context.Context, name string, logsWriter io.Writer) bool {
	if err := b.removeVolume(ctx, name); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not remove the BuildKit cache volume %s: %s\n", name, err))
		return false
	}
	return true
}

// startBuildkitd creates and starts the buildkitd container, removing it if
// it doesn't start.
func (b *buildkitBuilder) startBuildkitd(repoDir, outputDir, cacheVolume string, memoryBytes, cores int64) (string, error) {
	ctx := context.Background()
	id, err := b.createContainer(ctx, buildkitContainerConfig(), buildkitHostConfig(repoDir, outputDir, cacheVolume, memoryBytes, cores))
	if err != nil {
		return "", fmt.Errorf("creating the BuildKit container: %w", err)
	}
	if err := b.startContainer(ctx, id); err != nil {
		b.removeContainer(id)
		return "", fmt.Errorf("starting the BuildKit container: %w", err)
	}
	return id, nil
}

// waitReady polls `buildctl debug workers` until buildkitd answers, at most
// readyTimeout overall: each probe and each wait between probes ends at that
// deadline.
func (b *buildkitBuilder) waitReady(id string) error {
	overall, cancelOverall := context.WithTimeout(context.Background(), b.readyTimeout)
	defer cancelOverall()
	for {
		ctx, cancel := context.WithTimeout(overall, buildkitProbeTimeout)
		code, err := b.exec(ctx, id, []string{"buildctl", "debug", "workers"}, nil, io.Discard)
		cancel()
		if err == nil && code == 0 && overall.Err() == nil {
			return nil
		}
		select {
		case <-overall.Done():
			return fmt.Errorf("BuildKit didn't start:\n%s", b.logsTail(id))
		case <-time.After(b.readyInterval):
		}
	}
}

// errBuildkitBuildFailed is a build that ran and exited non-zero.
var errBuildkitBuildFailed = errors.New("The image build failed")

// runBuild runs buildctl build, its output redacted into the job log.
func (b *buildkitBuilder) runBuild(id string, req buildkitRequest, outputDir string, logsWriter io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), b.buildTimeout)
	defer cancel()
	out, flush := redact.NewWriter(&buildLogWriter{w: logsWriter}, req.secrets)
	code, err := b.exec(ctx, id, buildctlBuildCmd(req, outputDir), buildctlSecretEnv(req.secrets), out)
	flush()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("The image build timed out after %d minutes", int(buildkitBuildTimeout.Minutes()))
	}
	if err != nil {
		return fmt.Errorf("running the image build: %w", err)
	}
	if code != 0 {
		return errBuildkitBuildFailed
	}
	return nil
}

// buildLogMaxLine is the longest line buildLogWriter passes on: well under
// the 64 KiB token limit of the job log's line scanner, which stops reading
// (and closes its pipe) at a longer line.
const buildLogMaxLine = 16 << 10

// buildLogWriter passes the build's output to the job log, breaking a line
// longer than buildLogMaxLine, and never fails a write: a job log that can't
// take the output (its reader gone) mustn't fail an otherwise good build.
type buildLogWriter struct {
	w       io.Writer
	lineLen int
	failed  bool
}

func (l *buildLogWriter) Write(p []byte) (int, error) {
	if l.failed {
		return len(p), nil
	}
	out := make([]byte, 0, len(p)+len(p)/buildLogMaxLine+1)
	for _, c := range p {
		if c == '\n' || c == '\r' {
			l.lineLen = 0
		} else {
			// Break before a character, never inside one: a UTF-8
			// continuation byte runs the line up to 3 bytes past the cap.
			// Output that isn't UTF-8 (a run of continuation bytes) is
			// still broken there.
			if l.lineLen >= buildLogMaxLine && (utf8.RuneStart(c) || l.lineLen >= buildLogMaxLine+utf8.UTFMax-1) {
				out = append(out, '\n')
				l.lineLen = 0
			}
			l.lineLen++
		}
		out = append(out, c)
	}
	if _, err := l.w.Write(out); err != nil {
		l.failed = true
	}
	return len(p), nil
}

// explainFailure turns a failed build into the out-of-memory error when the
// container was OOM-killed: a RUN step killed for memory leaves buildkitd
// running and buildctl exiting non-zero, so the container's flag is the
// signal, not an exit code.
func (b *buildkitBuilder) explainFailure(id string, err error, memoryBytes int64) error {
	if errors.Is(err, errBuildkitBuildFailed) && b.oomKilled(id) {
		return fmt.Errorf("The image build ran out of memory (limit %.1f GiB)", float64(memoryBytes)/(1<<30))
	}
	return err
}

// prune trims the build cache to its budget. Its failure is logged and
// doesn't change the build's result.
func (b *buildkitBuilder) prune(id string, logsWriter io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), buildkitPruneTimeout)
	defer cancel()
	cmd := []string{"buildctl", "prune", "--keep-storage", fmt.Sprint(buildkitCacheKeepMB)}
	code, err := b.exec(ctx, id, cmd, nil, io.Discard)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit code %d", code)
	}
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Pruning the BuildKit cache failed: %s\n", err))
	}
}

// buildctlBuildCmd is the build's argv: names only, never a secret's value.
func buildctlBuildCmd(req buildkitRequest, outputDir string) []string {
	dockerFile := req.dockerFile
	if dockerFile == "" {
		dockerFile = "Dockerfile"
	}
	cmd := []string{"buildctl", "build", "--progress", "plain", "--frontend", "dockerfile.v0",
		"--local", "context=" + req.repoDir,
		"--local", "dockerfile=" + req.repoDir,
		"--opt", "filename=" + dockerFile,
		"--opt", "platform=linux/" + runtime.GOARCH,
	}
	argNames := make([]string, 0, len(req.buildArgs))
	for name, value := range req.buildArgs {
		if value != nil {
			argNames = append(argNames, name)
		}
	}
	sort.Strings(argNames)
	for _, name := range argNames {
		cmd = append(cmd, "--opt", "build-arg:"+name+"="+*req.buildArgs[name])
	}
	// The labels the classic build sets (reclaim.Labels), so the sweep still
	// recognises the image once it is untagged.
	cmd = append(cmd,
		"--opt", "label:"+reclaim.OwnerLabelKey+"="+reclaim.OwnerLabelValue,
		"--opt", "label:"+reclaim.PurposeLabelKey+"="+reclaim.PurposeDeploymentImage)
	for _, name := range sortedNames(req.secrets) {
		cmd = append(cmd, "--secret", "id="+name+",env="+buildkitSecretEnvPrefix+name)
	}
	return append(cmd, "--output", "type=docker,name="+req.imageNameAndTag+",dest="+filepath.Join(outputDir, buildkitImageTar))
}

// buildctlSecretEnv is the build exec's environment: each secret under its
// prefixed name.
func buildctlSecretEnv(secrets map[string]string) []string {
	env := make([]string, 0, len(secrets))
	for _, name := range sortedNames(secrets) {
		env = append(env, buildkitSecretEnvPrefix+name+"="+secrets[name])
	}
	return env
}

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildkitContainerConfig runs the image's own entrypoint and arguments
// (rootlesskit buildkitd). Never --oci-worker-no-process-sandbox: the process
// sandbox is what stops a RUN step reading buildctl's environment, and with
// it every secret.
func buildkitContainerConfig() *container.Config {
	return &container.Config{
		Image: buildkitImage,
		// Labelled so a later build and the startup sweep recognise it.
		Labels: reclaim.Labels(reclaim.PurposeBuildkit),
	}
}

// buildkitHostConfig is rootless BuildKit's sandbox: seccomp, AppArmor and
// the masked /proc paths are lifted so rootlesskit can set up its user
// namespace, but the container is never privileged, gets no added capability
// and no no-new-privileges (newuidmap/newgidmap are setuid).
func buildkitHostConfig(repoDir, outputDir, cacheVolume string, memoryBytes, cores int64) *container.HostConfig {
	return &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: repoDir, Target: repoDir, ReadOnly: true},
			{Type: mount.TypeBind, Source: outputDir, Target: outputDir},
			{Type: mount.TypeVolume, Source: cacheVolume, Target: buildkitStateDir},
		},
		SecurityOpt: []string{"seccomp=unconfined", "apparmor=unconfined"},
		// systempaths=unconfined: empty, not nil — nil means Docker's defaults.
		MaskedPaths:   []string{},
		ReadonlyPaths: []string{},
		Resources: container.Resources{
			Memory: memoryBytes,
			// No swap, as for the classic build.
			MemorySwap: memoryBytes,
			NanoCPUs:   cores * 1_000_000_000,
			PidsLimit:  pidsLimitPtr(buildkitPidsLimit),
		},
		// Not host networking: the instance metadata service stays out of
		// reach, as for the other job containers.
		NetworkMode: container.NetworkMode("bridge"),
		ExtraHosts:  cloudMetadataExtraHosts(),
	}
}

func listBuildkitContainers(ctx context.Context) ([]string, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	filters := client.Filters{}.
		Add("label", reclaim.OwnerLabelKey+"="+reclaim.OwnerLabelValue).
		Add("label", reclaim.PurposeLabelKey+"="+reclaim.PurposeBuildkit)
	listed, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(listed.Items))
	for _, c := range listed.Items {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

// createBuildkitCacheVolume creates a service's cache volume; Docker returns
// the existing one when it is already there.
func createBuildkitCacheVolume(ctx context.Context, name string) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Labels: reclaim.Labels(reclaim.PurposeBuildkitCache),
	})
	return err
}

// removeBuildkitCacheVolume removes a cache volume; one that isn't there is
// no error. Never forced: the daemon refuses to remove a volume a container
// still mounts.
func removeBuildkitCacheVolume(ctx context.Context, name string) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: false})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func dockerStartContainer(ctx context.Context, id string) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// dockerExec runs cmd in the container with env added to its environment,
// streams its stdout and stderr to out and returns its exit code. When ctx
// ends first it returns ctx's error; the exec is left to the container's
// removal.
func dockerExec(ctx context.Context, id string, cmd, env []string, out io.Writer) (int, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return 0, err
	}
	defer cli.Close()
	created, err := cli.ExecCreate(ctx, id, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: cmd, Env: env})
	if err != nil {
		return 0, err
	}
	resp, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: false})
	if err != nil {
		return 0, err
	}
	defer resp.Close()
	// The attach hijacks the connection, which no longer sees ctx: close it
	// when ctx ends so the copy returns (as execCommand does).
	copyDone := make(chan struct{})
	defer close(copyDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = resp.Conn.Close()
		case <-copyDone:
		}
	}()
	copyErr := copyExecOutput(out, resp.Reader)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if copyErr != nil {
		return 0, copyErr
	}
	inspected, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return 0, err
	}
	return inspected.ExitCode, nil
}

// buildkitLogsTail is buildkitd's last log lines.
func buildkitLogsTail(id string) string {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return ""
	}
	defer cli.Close()
	lines := strings.Split(strings.TrimRight(verifyBrowserContainerOutput(cli, id), "\n"), "\n")
	if len(lines) > buildkitLogLines {
		lines = lines[len(lines)-buildkitLogLines:]
	}
	return strings.Join(lines, "\n")
}

// dockerContainerOOMKilled reports Docker's OOMKilled flag; false when the
// container can't be inspected.
func dockerContainerOOMKilled(id string) bool {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return false
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), containerInspectTimeout)
	defer cancel()
	res, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || res.Container.State == nil {
		return false
	}
	return res.Container.State.OOMKilled
}

// dockerLoadImage loads the image tar into the host daemon, its output in
// the job log as a classic build's is.
func dockerLoadImage(tarPath string, logsWriter io.Writer) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), buildkitLoadTimeout)
	defer cancel()
	res, err := cli.ImageLoad(ctx, f, client.ImageLoadWithQuiet(true))
	if err != nil {
		return err
	}
	defer res.Close()
	return printBodyToLog(res, logsWriter)
}
