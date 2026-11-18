package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/jobs/resources"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// --- the gate: a backend built against the Task's change ---------------------

const (
	crossKit = "0-deployment-io/kit"
	crossLib = "1-deployment-io/lib"
	crossApp = "2-deployment-io/app-server"
	crossWeb = "3-deployment-io/web"

	libGoMod = "module github.com/deployment-io/lib\n\ngo 1.27\n"
	// crossAppGoMod requires kit directly and lib // indirect.
	crossAppGoMod = `module github.com/deployment-io/app-server

go 1.27

require github.com/deployment-io/kit v1.4.0

require github.com/deployment-io/lib v0.3.0 // indirect
`
	vendorModules = "# github.com/deployment-io/kit v1.4.0\n## explicit\ngithub.com/deployment-io/kit\n"

	vendorWhy  = "it vendors its dependencies (vendor/modules.txt at the repository root)"
	versionWhy = "its go.mod's Go version is a plain version number"
)

// crossRepo is one repository of a cross-repo gate test: its files and whether
// the Task changed it.
type crossRepo struct {
	dir     string
	files   map[string]string
	changed bool
}

// crossRepoGateStage is a stage over repos (committed with their files, then
// changed when marked), with static the static sites and backend the
// repositories that have a backend preview.
func crossRepoGateStage(t *testing.T, logs io.Writer, image string, repos []crossRepo, static, backend []string) *qaStage {
	t.Helper()
	workDir := t.TempDir()
	var bases []baseCommitOutput
	for i, repo := range repos {
		for name, content := range repo.files {
			writeFile(t, filepath.Join(workDir, repo.dir, name), content)
		}
		bases = append(bases, baseCommitOutput{Index: i, Dir: repo.dir, CommitSHA: qaTestRepo(t, workDir, repo.dir)})
		if repo.changed {
			writeFile(t, filepath.Join(workDir, repo.dir, "changed.txt"), "changed\n")
		}
	}
	stage := qaTestStage(t, workDir, bases, logs)
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.AgentboxImage, image)
	in := func(list []string, dir string) bool {
		for _, d := range list {
			if d == dir {
				return true
			}
		}
		return false
	}
	stage.checkPreview = func(dir string) agenttools.RunnerBuiltPreviewCheck {
		return agenttools.RunnerBuiltPreviewCheck{OK: in(static, dir), Reason: "not a static site"}
	}
	stage.checkBackend = func(dir string) error {
		if in(backend, dir) {
			return nil
		}
		return fmt.Errorf("no web service for %s", dir)
	}
	return stage
}

func vendoredApp(goMod string) map[string]string {
	return map[string]string{"go.mod": goMod, "vendor/modules.txt": vendorModules}
}

// linkedDirs is each linked backend with its dependencies' dirs and names.
func linkedDirs(linked []linkedBackend) []string {
	var out []string
	for _, b := range linked {
		var deps []string
		for _, d := range b.dependencies {
			deps = append(deps, d.dir+"="+d.name+"|"+d.description)
		}
		out = append(out, b.backendDir+": "+strings.Join(deps, ", "))
	}
	return out
}

func keptLine(dependency, module string) string {
	return fmt.Sprintf("QA stage: %s uses the Go module %s from %s, which this Task also changes — its preview builds against this Task's change\n", crossApp, module, dependency)
}

// A vendored backend depending on changed repositories is kept in the stack
// path — changed or not — with every dependency, direct and // indirect,
// recorded in work-dir order.
func TestQAGateKeepsAVendoredBackendUsingChangedRepositories(t *testing.T) {
	for _, appChanged := range []bool{true, false} {
		t.Run(fmt.Sprintf("app-server changed %v", appChanged), func(t *testing.T) {
			var logs strings.Builder
			stage := crossRepoGateStage(t, &logs, stackQAImage, []crossRepo{
				{crossKit, map[string]string{"go.mod": kitGoModCross}, true},
				{crossLib, map[string]string{"go.mod": libGoMod}, true},
				{crossApp, vendoredApp(crossAppGoMod), appChanged},
			}, nil, []string{crossApp})
			g := stage.gate()
			if !reflect.DeepEqual(g.backends, []string{crossApp}) || g.status != "" || len(g.dropped) != 0 {
				t.Fatalf("gate = %+v", g)
			}
			want := []string{crossApp + ": " + crossKit + "=github.com/deployment-io/kit|the Go module github.com/deployment-io/kit, " +
				crossLib + "=github.com/deployment-io/lib|the Go module github.com/deployment-io/lib"}
			if got := linkedDirs(g.linked); !reflect.DeepEqual(got, want) {
				t.Errorf("linked = %v\nwant %v", got, want)
			}
			kit, lib := keptLine(crossKit, "github.com/deployment-io/kit"), keptLine(crossLib, "github.com/deployment-io/lib")
			if i, j := strings.Index(logs.String(), kit), strings.Index(logs.String(), lib); i < 0 || j < i {
				t.Errorf("log:\n%s", logs.String())
			}
		})
	}
}

const kitGoModCross = "module github.com/deployment-io/kit\n\ngo 1.27\n"

// A backend that doesn't qualify is left out, with the reason worded for why:
// in the log, in QA_PREVIEWS and, with nothing else left, as the skipped reason.
func TestQAGateLeavesOutABackendThatDoesNotQualify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		link  bool // vendor/modules.txt a symlink to a real file
		why   string
	}{
		{name: "not vendored", files: map[string]string{"go.mod": crossAppGoMod}, why: vendorWhy},
		{name: "modules.txt a symlink", files: map[string]string{"go.mod": crossAppGoMod, "elsewhere.txt": vendorModules}, link: true, why: vendorWhy},
		{name: "a go version that isn't plain", files: vendoredApp(strings.Replace(crossAppGoMod, "go 1.27", "go 1.27rc1", 1)), why: versionWhy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, withSite := range []bool{false, true} {
				var logs strings.Builder
				repos := []crossRepo{{crossKit, map[string]string{"go.mod": kitGoModCross}, true}, {crossApp, tc.files, true}}
				var static []string
				if withSite {
					repos = append(repos, crossRepo{crossWeb, nil, true})
					static = []string{crossWeb}
				}
				stage := crossRepoGateStage(t, &logs, stackQAImage, repos, static, []string{crossApp})
				if tc.link {
					if err := os.MkdirAll(filepath.Join(stage.workDirHost, crossApp, "vendor"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(stage.workDirHost, crossApp, "elsewhere.txt"), filepath.Join(stage.workDirHost, crossApp, "vendor", "modules.txt")); err != nil {
						t.Fatal(err)
					}
				}
				g := stage.gate()
				wantLog := fmt.Sprintf("QA stage: leaving %s's backend preview out — it uses the Go module github.com/deployment-io/kit from %s, which this Task also changes, and QA builds it against this Task's change only when %s\n", crossApp, crossKit, tc.why)
				if !strings.Contains(logs.String(), wantLog) {
					t.Errorf("log:\n%s\nwant %s", logs.String(), wantLog)
				}
				if g.backends != nil || len(g.linked) != 0 {
					t.Fatalf("gate = %+v", g)
				}
				if withSite {
					wantError := fmt.Sprintf("left out: it uses the Go module github.com/deployment-io/kit from %s, which this Task also changes, and QA builds it against this Task's change only when %s", crossKit, tc.why)
					if len(g.dropped) != 1 || g.dropped[0].previewError() != wantError {
						t.Errorf("dropped = %+v\nwant the error %q", g.dropped, wantError)
					}
					if g.status != "" || !reflect.DeepEqual(g.static, []string{crossWeb}) {
						t.Errorf("gate = %q %q, static %v", g.status, g.reason, g.static)
					}
					continue
				}
				wantReason := fmt.Sprintf("%s depends on %s, which this Task also changes, and QA builds it against this Task's change only when %s, so QA can't check this change yet", crossApp, crossKit, tc.why)
				if g.status != tasks.QAStatusSkipped || g.reason != wantReason {
					t.Errorf("gate = %q %q\nwant reason %q", g.status, g.reason, wantReason)
				}
			}
		})
	}
}

// A dependency the Task didn't change is what the build uses anyway: nothing
// changes. Outside the stack path a vendored backend is still left out as
// before.
func TestQAGateCrossRepoUnchangedDependencyAndOlderPath(t *testing.T) {
	t.Run("unchanged dependency", func(t *testing.T) {
		var logs strings.Builder
		stage := crossRepoGateStage(t, &logs, stackQAImage, []crossRepo{
			{crossKit, map[string]string{"go.mod": kitGoModCross}, false},
			{crossApp, vendoredApp(crossAppGoMod), true},
		}, nil, []string{crossApp})
		g := stage.gate()
		if !reflect.DeepEqual(g.backends, []string{crossApp}) || len(g.linked) != 0 || len(g.dropped) != 0 || strings.Contains(logs.String(), "which this Task also changes") {
			t.Errorf("gate = %+v\nlog:\n%s", g, logs.String())
		}
	})
	t.Run("older path", func(t *testing.T) {
		var logs strings.Builder
		stage := crossRepoGateStage(t, &logs, backendQAImage, []crossRepo{
			{crossKit, map[string]string{"go.mod": kitGoModCross}, true},
			{crossApp, vendoredApp(crossAppGoMod), true},
		}, nil, []string{crossApp})
		g := stage.gate()
		wantReason := crossApp + " depends on " + crossKit + ", which this Task also changes; previews build against the released " + crossKit + ", so QA can't check this change yet"
		if g.backends != nil || g.linked != nil || g.dropped != nil || g.status != tasks.QAStatusSkipped || g.reason != wantReason {
			t.Errorf("gate = %+v", g)
		}
		wantLog := "QA stage: leaving " + crossApp + "'s backend preview out — it uses the Go module github.com/deployment-io/kit from " + crossKit + ", which this Task also changes, and a preview builds against the version in its go.mod; QA can check it once that change is released\n"
		if !strings.Contains(logs.String(), wantLog) {
			t.Errorf("log:\n%s", logs.String())
		}
	})
}

// --- the Go version ------------------------------------------------------------

func TestGoBuildVersion(t *testing.T) {
	for _, tc := range []struct {
		goMod string
		want  string
		ok    bool
	}{
		{"module a\n\ngo 1.27\n", "1.27", true},
		{"module a\n\ngo 1.27.1\n", "1.27.1", true},
		{"module a\n\ngo 1.26\ntoolchain go1.27.3\n", "1.27.3", true},
		{"module a\n\ngo 1.26 // the floor\ntoolchain go1.27\n", "1.27", true},
		{"module a\n\ngo 1.26\ntoolchain default\n", "1.26", true},
		{"module a\n\ngo 1.26\ntoolchain go1.27rc1\n", "1.26", true},
		{"module a\n\ngo 1.27rc1\n", "", false},
		{"module a\n\ngo 1.27-beta\n", "", false},
		{"module a\n\ngo 1\n", "", false},
		{"module a\n\ngo 1.27.1.2\n", "", false},
		{"module a\n\ngo v1.27\n", "", false},
		{"module a\n\ngo 1.27; rm -rf /\n", "", false},
		{"module a\n", "", false},
		{"module a\nreplace (\n\tgo 1.27\n)\n", "", false},
	} {
		got, ok := goBuildVersion([]byte(tc.goMod))
		if got != tc.want || ok != tc.ok {
			t.Errorf("goBuildVersion(%q) = %q %v, want %q %v", tc.goMod, got, ok, tc.want, tc.ok)
		}
	}
}

// --- the container -------------------------------------------------------------

func TestGoCrossRepoPrepareAndContainerSpec(t *testing.T) {
	buildDir := t.TempDir()
	writeFile(t, filepath.Join(buildDir, "go.mod"), "module a\n\ngo 1.27\n")
	commands, err := goCrossRepoLinker{}.Prepare(buildDir, []crossRepoDependency{
		{dir: crossKit, name: "github.com/deployment-io/kit"}, {dir: crossLib, name: "github.com/deployment-io/lib"},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := crossRepoContainerSpec{commands: commands, buildDir: buildDir, cacheVolume: "agentbox-cache-task-1-0"}
	cfg, host := spec.containerConfig()
	if cfg.Image != "golang:1.27" || cfg.User != "root" || cfg.WorkingDir != "/src" {
		t.Errorf("config = %+v", cfg)
	}
	wantCommands := [][]string{
		{"go", "mod", "edit", "-replace=github.com/deployment-io/kit=./.deployment-io/replace/0"},
		{"go", "mod", "edit", "-replace=github.com/deployment-io/lib=./.deployment-io/replace/1"},
		{"go", "mod", "vendor"},
	}
	if !reflect.DeepEqual(commands.commands, wantCommands) {
		t.Errorf("commands = %q", commands.commands)
	}
	wantEnv := []string{"GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOMODCACHE=/cache/gomod", "GOCACHE=/tmp/go-build", "GOTMPDIR=/tmp"}
	if !reflect.DeepEqual(cfg.Env, wantEnv) {
		t.Errorf("env = %q", cfg.Env)
	}
	if !reflect.DeepEqual(cfg.Labels, reclaim.Labels(reclaim.PurposeStaticSiteBuild)) {
		t.Errorf("labels = %v", cfg.Labels)
	}
	if host.NetworkMode != "none" {
		t.Errorf("network = %q", host.NetworkMode)
	}
	wantMounts := []mount.Mount{
		{Type: mount.TypeBind, Source: buildDir, Target: "/src"},
		{Type: mount.TypeVolume, Source: "agentbox-cache-task-1-0", Target: "/cache", ReadOnly: true},
	}
	if !reflect.DeepEqual(host.Mounts, wantMounts) {
		t.Errorf("mounts = %+v", host.Mounts)
	}
	memory, cpus := resources.LimitsForStaticSiteBuild()
	if host.Resources.Memory != memory || host.Resources.NanoCPUs != cpus || host.Resources.PidsLimit == nil || *host.Resources.PidsLimit != defaultBuildPidsLimit {
		t.Errorf("resources = %+v", host.Resources)
	}
	if !reflect.DeepEqual(host.ExtraHosts, cloudMetadataExtraHosts()) {
		t.Errorf("extra hosts = %v", host.ExtraHosts)
	}
}

// --- the hook ------------------------------------------------------------------

// fakeCrossRepoDocker records a container run and answers each command with
// its exit code (0 when absent); hang makes a command wait for the timeout.
type fakeCrossRepoDocker struct {
	pullErr  error
	codes    map[string]int
	output   map[string]string
	hang     string
	created  []*container.Config
	hosts    []*container.HostConfig
	commands []string
	removed  []string
	// atExec checks the build copy while the commands run.
	atExec func()
}

func (f *fakeCrossRepoDocker) docker(timeout time.Duration) *crossRepoDocker {
	return &crossRepoDocker{
		pull: func(context.Context, string) error { return f.pullErr },
		create: func(_ context.Context, cfg *container.Config, host *container.HostConfig) (string, error) {
			f.created = append(f.created, cfg)
			f.hosts = append(f.hosts, host)
			return "c1", nil
		},
		start: func(context.Context, string) error { return nil },
		exec: func(ctx context.Context, _ string, cmd, _ []string, out io.Writer) (int, error) {
			line := strings.Join(cmd, " ")
			f.commands = append(f.commands, line)
			if f.atExec != nil {
				f.atExec()
				f.atExec = nil
			}
			io.WriteString(out, f.output[line])
			if line == f.hang {
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return f.codes[line], nil
		},
		remove:  func(id string) { f.removed = append(f.removed, id) },
		timeout: timeout,
	}
}

// crossRepoHookStage is a stage whose dependency sources are temporary
// directories holding a file named after the repository; sources lists them.
func crossRepoHookStage(t *testing.T, logs io.Writer, sourceErr map[string]error) (*qaStage, *[]string) {
	t.Helper()
	workDir := filepath.Join(t.TempDir(), "task")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stage := qaTestStage(t, workDir, nil, logs)
	var sources []string
	stage.crossRepoSource = func(_ context.Context, deps agenttools.DeployStaticSitePreviewDeps, repository string) (string, error) {
		if deps.WorkDirHost != workDir {
			t.Errorf("source deps work dir = %s", deps.WorkDirHost)
		}
		dir, err := os.MkdirTemp(filepath.Dir(workDir), "dep-")
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, dir)
		writeFile(t, filepath.Join(dir, "repository.txt"), repository)
		return dir, sourceErr[repository]
	}
	return stage, &sources
}

func goLinkedApp(stage *qaStage) linkedBackend {
	return linkedBackend{backendDir: crossApp, linker: goCrossRepoLinker{stage: stage}, dependencies: []crossRepoDependency{
		{dir: crossKit, name: "github.com/deployment-io/kit", description: "the Go module github.com/deployment-io/kit"},
		{dir: crossLib, name: "github.com/deployment-io/lib", description: "the Go module github.com/deployment-io/lib"},
	}}
}

func appBuildDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), crossAppGoMod)
	return dir
}

const crossPreparingLine = "QA round 1: building " + crossApp + "'s preview against this Task's " + crossKit + " (github.com/deployment-io/kit) and " +
	crossLib + " (github.com/deployment-io/lib) — go mod vendor in the preview's build copy only\n"

// Each dependency's build source lands at .deployment-io/replace/<k> of the
// build copy before the commands run, in a container over the Step's cache.
func TestCrossRepoPrepareBuildPlacesTheDependencies(t *testing.T) {
	var logs strings.Builder
	stage, sources := crossRepoHookStage(t, &logs, nil)
	stage.ctx = commandUtils.TaskJobContext{OrganizationID: "org-1", TaskID: "task-1", StepIndex: 2}
	var checkedVolume string
	stage.cacheExists = func(name string) bool { checkedVolume = name; return true }
	buildDir := appBuildDir(t)
	// The copy brought a .deployment-io symlink: it's replaced, never followed.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(buildDir, ".deployment-io")); err != nil {
		t.Fatal(err)
	}
	f := &fakeCrossRepoDocker{output: map[string]string{"go mod vendor": "go: finding module for package x\n"}}
	f.atExec = func() {
		for k, repo := range []string{crossKit, crossLib} {
			if got := readFile(t, filepath.Join(buildDir, ".deployment-io", "replace", fmt.Sprint(k), "repository.txt")); got != repo {
				t.Errorf("replace/%d holds %s, want %s", k, got, repo)
			}
		}
	}
	stage.crossRepoDocker = f.docker(time.Minute)
	if err := stage.crossRepoPrepareBuild("QA round 1", goLinkedApp(stage), nil)(context.Background(), crossApp, buildDir, buildDir); err != nil {
		t.Fatal(err)
	}
	if checkedVolume != "agentbox-cache-task-1-2" || len(f.hosts) != 1 || f.hosts[0].Mounts[1].Source != "agentbox-cache-task-1-2" || f.hosts[0].Mounts[0].Source != buildDir {
		t.Errorf("volume %q, hosts %+v", checkedVolume, f.hosts)
	}
	if want := []string{
		"go mod edit -replace=github.com/deployment-io/kit=./.deployment-io/replace/0",
		"go mod edit -replace=github.com/deployment-io/lib=./.deployment-io/replace/1",
		"go mod vendor",
	}; !reflect.DeepEqual(f.commands, want) {
		t.Errorf("commands = %q", f.commands)
	}
	if !reflect.DeepEqual(f.removed, []string{"c1"}) {
		t.Errorf("removed = %v", f.removed)
	}
	for _, dir := range *sources {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the source %s survived: %v", dir, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the symlinked .deployment-io was written through: %v", entries)
	}
	if !strings.Contains(logs.String(), crossPreparingLine+"go: finding module for package x\n") {
		t.Errorf("log:\n%s", logs.String())
	}
}

// Every failure fails the deploy with the exact error; the root directory is
// checked before anything is prepared.
func TestCrossRepoPrepareBuildFailures(t *testing.T) {
	const prefix = "couldn't build " + crossApp + " against this Task's " + crossKit + " and " + crossLib + ": "
	var vendorOutput strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&vendorOutput, "line %d\n", i)
	}
	var last20 []string
	for i := 6; i <= 25; i++ {
		last20 = append(last20, fmt.Sprintf("line %d", i))
	}
	for _, tc := range []struct {
		name        string
		subdir      string
		noCache     bool
		sourceErr   map[string]error
		docker      fakeCrossRepoDocker
		timeout     time.Duration
		want        string
		wantSources int
		wantCreated bool
	}{
		{name: "root directory", subdir: "services/api", want: prefix + "its service's root directory isn't the repository root"},
		{name: "no cache", noCache: true, want: prefix + "the Step's dependency cache is gone"},
		{name: "source", sourceErr: map[string]error{crossLib: errors.New("the merge result couldn't be checked out")}, wantSources: 2,
			want: prefix + "couldn't make " + crossLib + "'s build source: the merge result couldn't be checked out"},
		{name: "pull", docker: fakeCrossRepoDocker{pullErr: errors.New("no route to host")}, wantSources: 2,
			want: prefix + "couldn't pull golang:1.27: no route to host"},
		{name: "exit", docker: fakeCrossRepoDocker{codes: map[string]int{"go mod vendor": 1}, output: map[string]string{"go mod vendor": vendorOutput.String()}}, wantSources: 2, wantCreated: true,
			want: prefix + "go mod vendor exited with code 1\n" + strings.Join(last20, "\n")},
		{name: "timeout", docker: fakeCrossRepoDocker{hang: "go mod vendor"}, timeout: 20 * time.Millisecond, wantSources: 2, wantCreated: true,
			want: prefix + "the commands didn't finish within 10 minutes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			stage, sources := crossRepoHookStage(t, &logs, tc.sourceErr)
			stage.cacheExists = func(string) bool { return !tc.noCache }
			f := tc.docker
			timeout := tc.timeout
			if timeout == 0 {
				timeout = time.Minute
			}
			stage.crossRepoDocker = f.docker(timeout)
			buildDir := appBuildDir(t)
			serviceDir := filepath.Join(buildDir, tc.subdir)
			err := stage.crossRepoPrepareBuild("QA round 1", goLinkedApp(stage), nil)(context.Background(), crossApp, buildDir, serviceDir)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v\nwant %s", err, tc.want)
			}
			if len(*sources) != tc.wantSources {
				t.Errorf("sources made = %d, want %d", len(*sources), tc.wantSources)
			}
			for _, dir := range *sources {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("the source %s survived: %v", dir, err)
				}
			}
			if created := len(f.created) > 0; created != tc.wantCreated || !reflect.DeepEqual(f.removed, f.removedWant()) {
				t.Errorf("created %v (want %v), removed %v", created, tc.wantCreated, f.removed)
			}
			if tc.subdir != "" {
				if strings.Contains(logs.String(), "building") {
					t.Errorf("prepared before the root-directory check:\n%s", logs.String())
				}
				if _, err := os.Stat(filepath.Join(buildDir, ".deployment-io")); !os.IsNotExist(err) {
					t.Errorf("the copy was changed: %v", err)
				}
			}
		})
	}
}

func (f *fakeCrossRepoDocker) removedWant() []string {
	if len(f.created) == 0 {
		return nil
	}
	return []string{"c1"}
}

// A dependency's source that can't be moved is copied instead.
func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a", "b.go"), "package a\n")
	if err := os.Symlink("b.go", filepath.Join(src, "a", "c.go")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "0")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, "a", "b.go")); got != "package a\n" {
		t.Errorf("b.go = %q", got)
	}
	if link, err := os.Readlink(filepath.Join(dst, "a", "c.go")); err != nil || link != "b.go" {
		t.Errorf("c.go = %q %v", link, err)
	}
}

// --- the stack round -----------------------------------------------------------

// fakeLinker is a crossRepoLinker that isn't Go: every backend in deps depends
// on its list, qualifies unless in notOK, and is prepared with its image and
// commands.
type fakeLinker struct {
	deps  map[string][]crossRepoDependency
	notOK map[string]string
}

func (l fakeLinker) Detect(backendDir string, changed []string) []crossRepoDependency {
	var out []crossRepoDependency
	for _, d := range l.deps[backendDir] {
		for _, c := range changed {
			if c == d.dir && c != backendDir {
				out = append(out, d)
			}
		}
	}
	return out
}

func (l fakeLinker) Qualifies(backendDir string) (bool, string) {
	if why, ok := l.notOK[backendDir]; ok {
		return false, why
	}
	return true, ""
}

func (fakeLinker) CheckService(buildDir, serviceDir string) string {
	if _, err := os.Stat(filepath.Join(serviceDir, "package.json")); err != nil {
		return "it has no package.json"
	}
	return ""
}

func (fakeLinker) Prepare(string, []crossRepoDependency) (crossRepoCommands, error) {
	return crossRepoCommands{image: "node:22-bookworm", commands: [][]string{{"npm", "install", "--offline"}}, env: []string{"NPM_CONFIG_CACHE=/cache/npm"}, summary: "npm install"}, nil
}

// The gate and the hook driven by a linker that isn't Go: kept and left out
// with its description and why, prepared with its image and commands.
func TestCrossRepoWithAnotherLinker(t *testing.T) {
	const ui, app, worker = "0-acme/ui", "1-acme/app", "2-acme/worker"
	var logs strings.Builder
	stage := crossRepoGateStage(t, &logs, stackQAImage, []crossRepo{{ui, nil, true}, {app, nil, true}, {worker, nil, true}}, nil, []string{app, worker})
	dep := crossRepoDependency{dir: ui, name: "@acme/ui", description: "the npm package @acme/ui"}
	stage.linkers = []crossRepoLinker{fakeLinker{
		deps:  map[string][]crossRepoDependency{app: {dep}, worker: {dep}},
		notOK: map[string]string{worker: "it has a lockfile"},
	}}
	g := stage.gate()
	if !reflect.DeepEqual(g.backends, []string{app}) || len(g.linked) != 1 || g.linked[0].backendDir != app {
		t.Fatalf("gate = %+v", g)
	}
	for _, want := range []string{
		"QA stage: 1-acme/app uses the npm package @acme/ui from 0-acme/ui, which this Task also changes — its preview builds against this Task's change\n",
		"QA stage: leaving 2-acme/worker's backend preview out — it uses the npm package @acme/ui from 0-acme/ui, which this Task also changes, and QA builds it against this Task's change only when it has a lockfile\n",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log:\n%s\nwant %s", logs.String(), want)
		}
	}
	if got, want := g.dropped[0].previewError(), "left out: it uses the npm package @acme/ui from 0-acme/ui, which this Task also changes, and QA builds it against this Task's change only when it has a lockfile"; got != want {
		t.Errorf("error = %q", got)
	}

	stage.crossRepoSource = func(context.Context, agenttools.DeployStaticSitePreviewDeps, string) (string, error) {
		return t.TempDir(), nil
	}
	f := &fakeCrossRepoDocker{}
	stage.crossRepoDocker = f.docker(time.Minute)
	buildDir := t.TempDir()
	writeFile(t, filepath.Join(buildDir, "package.json"), "{}")
	if err := stage.crossRepoPrepareBuild("QA round 2", g.linked[0], nil)(context.Background(), app, buildDir, buildDir); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0].Image != "node:22-bookworm" || !reflect.DeepEqual(f.created[0].Env, []string{"NPM_CONFIG_CACHE=/cache/npm"}) ||
		!reflect.DeepEqual(f.commands, []string{"npm install --offline"}) {
		t.Errorf("created %+v, commands %v", f.created, f.commands)
	}
	if want := "QA round 2: building 1-acme/app's preview against this Task's 0-acme/ui (@acme/ui) — npm install in the preview's build copy only\n"; !strings.Contains(logs.String(), want) {
		t.Errorf("log:\n%s", logs.String())
	}
	err := stage.crossRepoPrepareBuild("QA round 2", g.linked[0], nil)(context.Background(), app, t.TempDir(), t.TempDir())
	if err == nil || err.Error() != "couldn't build 1-acme/app against this Task's 0-acme/ui: it has no package.json" {
		t.Errorf("error = %v", err)
	}
}

// In a stack round only a linked backend's deploy gets the hook; one whose
// preparation fails is reported as failed to deploy while the round goes on.
func TestQAStackRoundPreparesLinkedBackends(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api", "2-worker": "worker"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), nil)}}
	stage, logs, specs := stackStage(t, []string{"web", "api", "worker", "kit"}, []string{"0-web"}, []string{"1-api", "2-worker"}, f)
	stage.linkedBackends = []linkedBackend{{backendDir: "1-api", linker: fakeLinker{}, dependencies: []crossRepoDependency{{dir: "3-kit", name: "@acme/kit", description: "the npm package @acme/kit"}}}}
	stage.crossRepoSource = func(context.Context, agenttools.DeployStaticSitePreviewDeps, string) (string, error) {
		return t.TempDir(), nil
	}
	docker := &fakeCrossRepoDocker{codes: map[string]int{"npm install --offline": 1}, output: map[string]string{"npm install --offline": "npm ERR! offline\n"}}
	stage.crossRepoDocker = docker.docker(time.Minute)
	hooked := map[string]bool{}
	ops := *stage.stackOps
	inner := ops.deployBackend
	ops.deployBackend = func(ctx context.Context, deps agenttools.DeployWebServicePreviewDeps, p agenttools.WebServicePreviewPlan, vars map[string]string) (agenttools.WebServicePreviewDeployed, error) {
		hooked[p.Repository] = deps.PrepareBuild != nil
		if deps.PrepareBuild != nil {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "package.json"), "{}")
			if err := deps.PrepareBuild(ctx, p.Repository, dir, dir); err != nil {
				return agenttools.WebServicePreviewDeployed{}, err
			}
		}
		return inner(ctx, deps, p, vars)
	}
	stage.stackOps = &ops
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hooked, map[string]bool{"1-api": true, "2-worker": false}) {
		t.Errorf("hooked = %v", hooked)
	}
	previews := qaPreviews(t, (*specs)[0])
	byRepo := map[string]qaPreview{}
	for _, p := range previews {
		byRepo[p.Kind+":"+p.Repository] = p
	}
	const wantErr = "couldn't build 1-api against this Task's 3-kit: npm install --offline exited with code 1"
	if api := byRepo["backend:1-api"]; api.Error != wantErr || api.URL != "" {
		t.Errorf("1-api = %+v", api)
	}
	if byRepo["backend:2-worker"].URL == "" || byRepo["site:0-web"].URL == "" {
		t.Errorf("previews = %+v", previews)
	}
	if want := "QA round 1: the backend preview of 1-api failed to deploy: " + wantErr + "\nnpm ERR! offline\n"; !strings.Contains(logs.String(), want) {
		t.Errorf("log:\n%s", logs.String())
	}
}

// The older path's backend deploys, the implementer's tool and production
// never get the hook.
func TestCrossRepoHookOnlyInTheStackPath(t *testing.T) {
	var logs strings.Builder
	stage := backendGateStage(t, &logs, []string{"0-web", "1-api"}, []string{"0-web"}, []string{"1-api"})
	stage.backends = []string{"1-api"}
	stage.linkedBackends = []linkedBackend{{backendDir: "1-api", linker: fakeLinker{}}}
	stage.webPreviewDeps = func(record *agenttools.PreviewRecord) *agenttools.DeployWebServicePreviewDeps {
		return &agenttools.DeployWebServicePreviewDeps{Record: record, LogsWriter: io.Discard}
	}
	var hooked []bool
	stage.deployBackend = func(_ context.Context, deps agenttools.DeployWebServicePreviewDeps, _ string) (agenttools.WebServicePreviewDeployed, error) {
		hooked = append(hooked, deps.PrepareBuild != nil)
		return agenttools.WebServicePreviewDeployed{URL: "https://d1.cloudfront.net"}, nil
	}
	previewDeps := buildStaticSitePreviewDeps(stage.ctx, stage.parameters, stage.workDirHost, io.Discard)
	if _, err := stage.deployBackends("QA round 1", previewDeps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hooked, []bool{false}) {
		t.Errorf("older path hooked = %v", hooked)
	}
	tools := buildImplementPreviewTools(commandUtils.TaskJobContext{OrganizationID: "o", TaskID: "t"}, webPreviewJobParameters(t), t.TempDir(), io.Discard)
	if tools.web.PrepareBuild != nil {
		t.Error("the implementer's deploy_web_service_preview has a PrepareBuild hook")
	}
}
