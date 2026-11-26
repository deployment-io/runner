package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deployment-io/deployment-runner/agenttools"
	"github.com/deployment-io/deployment-runner/jobs/resources"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// CROSS-REPO BUILDS (the stack path only).
//
// A backend B that uses a library another changed repository of the Task holds
// would build its preview against the version its manifest pins, not this
// Task's change. When B qualifies (crossRepoLinker.Qualifies), the gate keeps it
// and records its dependencies; on every deploy of B's preview in a QA round,
// B's build copy is pointed at copies of those dependencies' build sources and
// re-vendored offline in a one-off container (prepareCrossRepoBuild), and the
// image is built from it as usual. Nothing leaves the build copy: the Task's
// trees, the pull request and production are untouched.
//
// The language-specific parts sit behind crossRepoLinker; everything here is
// generic.

// crossRepoLinker is one language's cross-repository dependency handling.
type crossRepoLinker interface {
	// Detect is backendDir's dependencies on the changed repositories other
	// than itself, in work-dir order; none when it has none this linker knows.
	Detect(backendDir string, changed []string) []crossRepoDependency
	// Qualifies reports whether QA can build backendDir against the Task's
	// change; when it can't, why is the condition it needs ("… only when <why>").
	Qualifies(backendDir string) (ok bool, why string)
	// CheckService is why the planned service can't be built against the
	// change, or "": buildDir is the build copy and serviceDir the service's
	// directory in it. It runs before anything is prepared.
	CheckService(buildDir, serviceDir string) string
	// Prepare is what runs in buildDir so it builds against deps, the k-th of
	// them at crossRepoReplaceDir(k) in it.
	Prepare(buildDir string, deps []crossRepoDependency) (crossRepoCommands, error)
}

// crossRepoDependency is a backend's dependency on a changed repository.
type crossRepoDependency struct {
	// dir is the dependency's work-dir name; name what the backend uses of it
	// (a Go module path); description how messages name it ("the Go module <name>").
	dir, name, description string
}

// crossRepoCommands is what a linker runs in a build copy: commands in order,
// in a container of image with env. summary names them for the Step log
// ("go mod vendor").
type crossRepoCommands struct {
	image    string
	commands [][]string
	env      []string
	summary  string
}

// linkedBackend is a backend the gate kept that builds against the Task's
// change of its dependencies, in work-dir order.
type linkedBackend struct {
	backendDir   string
	linker       crossRepoLinker
	dependencies []crossRepoDependency
}

// dependencyNames is every dependency's work-dir name, joined with " and ".
func (b linkedBackend) dependencyNames() string {
	names := make([]string, 0, len(b.dependencies))
	for _, d := range b.dependencies {
		names = append(names, d.dir)
	}
	return strings.Join(names, " and ")
}

// crossRepoLinkers are the linkers the gate tries, in order; the first whose
// Detect finds a dependency is the backend's.
func (s *qaStage) crossRepoLinkers() []crossRepoLinker {
	if s.linkers != nil {
		return s.linkers
	}
	return []crossRepoLinker{goCrossRepoLinker{stage: s}}
}

// detectCrossRepo is backendDir's linker and dependencies on changed
// repositories; no dependencies when no linker finds one.
func (s *qaStage) detectCrossRepo(backendDir string, changed []string) (crossRepoLinker, []crossRepoDependency) {
	for _, linker := range s.crossRepoLinkers() {
		if deps := linker.Detect(backendDir, changed); len(deps) > 0 {
			return linker, deps
		}
	}
	return nil, nil
}

// crossRepoReplaceRoot is where, in a build copy, the dependencies' build
// sources go: crossRepoReplaceDir(k) is the k-th's.
const crossRepoReplaceRoot = ".deployment-io/replace"

func crossRepoReplaceDir(k int) string {
	return fmt.Sprintf("%s/%d", crossRepoReplaceRoot, k)
}

// crossRepoBuildTimeout bounds the container's commands; crossRepoOutputLines is
// how many of their last output lines a failure carries.
const (
	crossRepoBuildTimeout = 10 * time.Minute
	crossRepoOutputLines  = 20
	crossRepoOutputMax    = 64 << 10
)

// errCrossRepoTimeout is the container's commands running out of
// crossRepoBuildTimeout.
var errCrossRepoTimeout = errors.New("the commands didn't finish within 10 minutes")

// crossRepoRound is a linked backend's deploy in a QA round labelled label
// whose previews build from mergeResult (nil: the working trees).
type crossRepoRound struct {
	label       string
	backend     linkedBackend
	mergeResult agenttools.MergeResultProvider
}

// crossRepoPrepareBuild is b's PrepareBuild hook in the round labelled label
// whose previews build from mergeResult.
func (s *qaStage) crossRepoPrepareBuild(label string, b linkedBackend, mergeResult agenttools.MergeResultProvider) func(ctx context.Context, repository, buildDir, serviceDir string) error {
	round := crossRepoRound{label: label, backend: b, mergeResult: mergeResult}
	return func(ctx context.Context, _, buildDir, serviceDir string) error {
		return s.prepareCrossRepoBuild(ctx, round, buildDir, serviceDir)
	}
}

// prepareCrossRepoBuild prepares the backend's build copy buildDir: it checks
// the planned service, asks the linker what to run, puts each dependency's
// build source at crossRepoReplaceDir(k) and runs the linker's commands in a
// container. Every failure is "couldn't build <B> against this Task's
// <dependencies>: <reason>".
func (s *qaStage) prepareCrossRepoBuild(ctx context.Context, round crossRepoRound, buildDir, serviceDir string) error {
	b := round.backend
	fail := func(reason string) error {
		return fmt.Errorf("couldn't build %s against this Task's %s: %s", b.backendDir, b.dependencyNames(), reason)
	}
	if why := b.linker.CheckService(buildDir, serviceDir); why != "" {
		return fail(why)
	}
	commands, err := b.linker.Prepare(buildDir, b.dependencies)
	if err != nil {
		return fail(err.Error())
	}
	described := make([]string, 0, len(b.dependencies))
	for _, d := range b.dependencies {
		described = append(described, fmt.Sprintf("%s (%s)", d.dir, d.name))
	}
	io.WriteString(s.logsWriter, fmt.Sprintf("%s: building %s's preview against this Task's %s — %s in the preview's build copy only\n", round.label, b.backendDir, strings.Join(described, " and "), commands.summary))
	exists := s.cacheExists
	if exists == nil {
		exists = cacheVolumeExists
	}
	volume := cacheVolumeName(s.ctx)
	if !exists(volume) {
		return fail("the Step's dependency cache is gone")
	}
	for k, d := range b.dependencies {
		if err := s.placeCrossRepoSource(ctx, round.mergeResult, d.dir, buildDir, k); err != nil {
			return fail(fmt.Sprintf("couldn't make %s's build source: %s", d.dir, err))
		}
	}
	tail := &tailWriter{max: crossRepoOutputMax}
	out := io.MultiWriter(&buildLogWriter{w: s.logsWriter}, tail)
	spec := crossRepoContainerSpec{commands: commands, buildDir: buildDir, cacheVolume: volume}
	docker := s.crossRepoDocker
	if docker == nil {
		docker = defaultCrossRepoDocker()
	}
	if err := docker.run(ctx, spec, out); err != nil {
		reason := err.Error()
		if lines := lastLines(string(tail.buf), crossRepoOutputLines); lines != "" {
			reason += "\n" + lines
		}
		return fail(reason)
	}
	return nil
}

// placeCrossRepoSource makes dependency's build source as for a preview of it
// (agenttools.PreviewBuildSource) and moves it, or copies it when it can't be
// moved, to crossRepoReplaceDir(k) in buildDir. Its own directory is removed
// in every case.
func (s *qaStage) placeCrossRepoSource(ctx context.Context, mergeResult agenttools.MergeResultProvider, dependency, buildDir string, k int) error {
	source := s.crossRepoSource
	if source == nil {
		source = agenttools.PreviewBuildSource
	}
	dir, err := source(ctx, agenttools.DeployStaticSitePreviewDeps{WorkDirHost: s.workDirHost, LogsWriter: s.logsWriter, MergeResult: mergeResult}, dependency)
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if err != nil {
		return err
	}
	parent := buildDir
	for _, name := range strings.Split(crossRepoReplaceRoot, "/") {
		parent = filepath.Join(parent, name)
		if err := ensureRealDir(parent); err != nil {
			return err
		}
	}
	dst := filepath.Join(buildDir, filepath.FromSlash(crossRepoReplaceDir(k)))
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.Rename(dir, dst); err == nil {
		return nil
	}
	return copyTree(dir, dst)
}

// ensureRealDir makes path a directory of its own: whatever else is there — a
// file, or a symlink the build copy brought — is removed first.
func ensureRealDir(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.IsDir() {
		return nil
	}
	if err == nil {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Mkdir(path, 0o755)
}

// copyTree copies src into the new directory dst: directories, regular files
// and symlinks as they are (a build source has none that leaves it).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Mkdir(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil
	})
}

// lastLines is text's last n lines, trailing newlines left out.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// crossRepoContainerSpec is one run of a linker's commands in a build copy.
type crossRepoContainerSpec struct {
	commands    crossRepoCommands
	buildDir    string
	cacheVolume string
}

// crossRepoBuildMount is where the build copy is mounted in the container, and
// the commands' working directory; crossRepoCacheMount the dependency cache's.
const (
	crossRepoBuildMount = "/src"
	crossRepoCacheMount = "/cache"
)

// containerConfig is the container spec runs in: the linker's image and env, as
// root in the build copy (read-write) with the Step's dependency cache
// read-only, no network, and the static-site build container's limits,
// cloud-metadata pins and reclaim labels.
func (spec crossRepoContainerSpec) containerConfig() (*container.Config, *container.HostConfig) {
	memoryBytes, nanoCPUs := resources.LimitsForStaticSiteBuild()
	return &container.Config{
		Image:      spec.commands.image,
		Cmd:        []string{"tail", "-f", "/dev/null"},
		User:       "root",
		WorkingDir: crossRepoBuildMount,
		Env:        append([]string{}, spec.commands.env...),
		Labels:     reclaim.Labels(reclaim.PurposeStaticSiteBuild),
	}, &container.HostConfig{
		NetworkMode: "none",
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: spec.buildDir, Target: crossRepoBuildMount},
			{Type: mount.TypeVolume, Source: spec.cacheVolume, Target: crossRepoCacheMount, ReadOnly: true},
		},
		Resources: container.Resources{
			Memory:    memoryBytes,
			NanoCPUs:  nanoCPUs,
			PidsLimit: pidsLimitPtr(defaultBuildPidsLimit),
		},
		ExtraHosts: cloudMetadataExtraHosts(),
	}
}

// crossRepoDocker runs a crossRepoContainerSpec; its fields are the Docker
// calls, seams for tests.
type crossRepoDocker struct {
	pull    func(ctx context.Context, image string) error
	create  func(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error)
	start   func(ctx context.Context, id string) error
	exec    func(ctx context.Context, id string, cmd, env []string, out io.Writer) (int, error)
	remove  func(id string)
	timeout time.Duration
}

func defaultCrossRepoDocker() *crossRepoDocker {
	return &crossRepoDocker{
		pull:    pullDockerImageForBuilding,
		create:  dockerCreateContainer,
		start:   dockerStartContainer,
		exec:    dockerExec,
		remove:  func(id string) { _ = removeBuildContainer(id) },
		timeout: crossRepoBuildTimeout,
	}
}

// run pulls spec's image, starts its container and runs its commands in order
// within the timeout, their output to out, stopping at the first that fails.
// The container is removed in every case.
func (d *crossRepoDocker) run(ctx context.Context, spec crossRepoContainerSpec, out io.Writer) error {
	cfg, hostCfg := spec.containerConfig()
	return d.runContainer(ctx, cfg, hostCfg, spec.commands.commands, out)
}

// runContainer pulls cfg's image, starts a container of cfg and hostCfg and
// runs commands in it in order within the timeout, their output to out,
// stopping at the first that fails. The container is removed in every case.
func (d *crossRepoDocker) runContainer(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, commands [][]string, out io.Writer) error {
	image := cfg.Image
	// Held until the container is gone, so no cleanup removes the image under it.
	release := reclaim.MarkImageInUse(image)
	defer release()
	if err := d.pull(ctx, image); err != nil {
		return fmt.Errorf("couldn't pull %s: %w", image, err)
	}
	timeout := d.timeout
	if timeout <= 0 {
		timeout = crossRepoBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Not cancelled: a create cut off mid-request can leave a container
	// without returning its ID to remove.
	id, err := d.create(context.Background(), cfg, hostCfg)
	if err != nil {
		return fmt.Errorf("couldn't create the container: %w", err)
	}
	defer d.remove(id)
	timedOut := func(err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errCrossRepoTimeout
		}
		return err
	}
	if err := d.start(ctx, id); err != nil {
		return timedOut(fmt.Errorf("couldn't start the container: %w", err))
	}
	for _, cmd := range commands {
		code, err := d.exec(ctx, id, cmd, nil, out)
		if err != nil {
			return timedOut(fmt.Errorf("%s: %w", strings.Join(cmd, " "), err))
		}
		if code != 0 {
			return fmt.Errorf("%s exited with code %d", strings.Join(cmd, " "), code)
		}
	}
	return nil
}
