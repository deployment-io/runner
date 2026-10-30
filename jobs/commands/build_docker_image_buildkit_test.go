package commands

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const testBuildkitMemory = 6 << 30

// fakeBuildkitDocker stands in for the Docker daemon behind buildkitBuilder's seams.
type fakeBuildkitDocker struct {
	mu          sync.Mutex
	userns      int64
	usernsOK    bool
	leftovers   []string
	present     bool
	pulls       []string
	configs     []*container.Config
	hostConfigs []*container.HostConfig
	execs       [][]string
	execEnvs    [][]string
	removed     []string
	events      []string
	// probesFailing is how many readiness probes fail before one succeeds;
	// negative: they all fail.
	probesFailing int
	// probe, when set, is what each readiness probe does.
	probe func(ctx context.Context) (int, error)
	// build is what the buildctl build exec does; nil writes the tar and exits 0.
	build      func(ctx context.Context, cmd []string, out io.Writer) (int, error)
	pruneErr   error
	oom        bool
	loaded     []string
	loadErr    error
	containers int
}

func (d *fakeBuildkitDocker) event(e string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
}

func newTestBuildkitBuilder(t *testing.T, d *fakeBuildkitDocker) *buildkitBuilder {
	t.Helper()
	b := newBuildkitBuilder()
	b.outputRoot = t.TempDir()
	b.readyTimeout = 200 * time.Millisecond
	b.readyInterval = time.Millisecond
	b.chown = func(string) error { return nil }
	b.maxUserNamespaces = func() (int64, bool) { return d.userns, d.usernsOK }
	b.limits = func() (int64, int64) { return testBuildkitMemory, 4 }
	b.listContainers = func(context.Context) ([]string, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		left := d.leftovers
		d.leftovers = nil
		return left, nil
	}
	b.imagePresent = func(context.Context, string) (bool, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.present, nil
	}
	b.pullImage = func(ref string) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.pulls = append(d.pulls, ref)
		d.present = true
		return nil
	}
	b.createVolume = func(context.Context) error { d.event("volume"); return nil }
	b.createContainer = func(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig) (string, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.configs = append(d.configs, cfg)
		d.hostConfigs = append(d.hostConfigs, hostCfg)
		d.containers++
		id := "buildkit-" + string(rune('a'+d.containers-1))
		d.events = append(d.events, "create "+id)
		return id, nil
	}
	b.startContainer = func(_ context.Context, id string) error { d.event("start " + id); return nil }
	b.exec = func(ctx context.Context, id string, cmd, env []string, out io.Writer) (int, error) {
		d.mu.Lock()
		d.execs = append(d.execs, cmd)
		d.execEnvs = append(d.execEnvs, env)
		d.events = append(d.events, "exec "+cmd[1])
		d.mu.Unlock()
		switch cmd[1] {
		case "debug":
			if d.probe != nil {
				return d.probe(ctx)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.probesFailing != 0 {
				d.probesFailing--
				return 1, nil
			}
			return 0, nil
		case "prune":
			return 0, d.pruneErr
		case "build":
			if d.build != nil {
				return d.build(ctx, cmd, out)
			}
			return writeFakeTar(cmd)
		}
		return 127, nil
	}
	b.logsTail = func(string) string { return "buildkitd: cannot set up user namespace" }
	b.oomKilled = func(string) bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.oom
	}
	b.removeContainer = func(id string) {
		d.event("remove " + id)
		d.mu.Lock()
		d.removed = append(d.removed, id)
		d.mu.Unlock()
	}
	b.loadImage = func(tarPath string, _ io.Writer) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if _, err := os.Stat(tarPath); err != nil {
			return err
		}
		d.loaded = append(d.loaded, tarPath)
		d.events = append(d.events, "load")
		return d.loadErr
	}
	return b
}

// writeFakeTar writes the file buildctl's --output names, as a build does.
func writeFakeTar(cmd []string) (int, error) {
	dest := outputDest(cmd)
	if dest == "" {
		return 1, nil
	}
	return 0, os.WriteFile(dest, []byte("tar"), 0o644)
}

func outputDest(cmd []string) string {
	for i, arg := range cmd {
		if arg == "--output" && i+1 < len(cmd) {
			_, dest, _ := strings.Cut(cmd[i+1], "dest=")
			return dest
		}
	}
	return ""
}

func testBuildkitRequest() buildkitRequest {
	value := "1.2.3"
	return buildkitRequest{
		repoDir:         "/tmp/org/build/repo",
		imageNameAndTag: "org-deploy:abc123",
		buildArgs:       map[string]*string{"VERSION": &value},
		secrets:         map[string]string{"NPM_TOKEN": "npm_s3cr3tvalue", "GH_TOKEN": "ghp_an0thervalue"},
	}
}

// assertNoSecretValue fails when any of the request's values is in s.
func assertNoSecretValue(t *testing.T, what, s string, req buildkitRequest) {
	t.Helper()
	for name, value := range req.secrets {
		if strings.Contains(s, value) {
			t.Errorf("%s contains %s's value: %q", what, name, s)
		}
	}
}

func (d *fakeBuildkitDocker) outputDir(t *testing.T) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.hostConfigs) == 0 {
		t.Fatal("no container created")
	}
	return d.hostConfigs[len(d.hostConfigs)-1].Mounts[1].Source
}

func (d *fakeBuildkitDocker) execNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var names []string
	for _, cmd := range d.execs {
		if cmd[1] != "debug" {
			names = append(names, cmd[1])
		}
	}
	return names
}

func buildDockerImageParameters(secrets map[string]string) map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.RepoDirectoryPath, "/tmp/org/build/repo")
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "deploy")
	jobs.SetParameterValue(parameters, parameters_enums.CommitHash, "abc123")
	if secrets != nil {
		jobs.SetParameterValue(parameters, parameters_enums.BuildSecrets, secrets)
	}
	return parameters
}

// A Job without build secrets (absent or empty) builds with the classic
// builder; one with them with BuildKit.
func TestBuildDockerImageChoosesTheBuilder(t *testing.T) {
	origClassic, origBuildkit := classicImageBuild, buildkitImageBuild
	t.Cleanup(func() { classicImageBuild, buildkitImageBuild = origClassic, origBuildkit })
	var used string
	var got buildkitRequest
	classicImageBuild = func(map[string]interface{}, *client.Client, string, string, string, io.Writer) error {
		used = "classic"
		return nil
	}
	buildkitImageBuild = func(_ map[string]interface{}, req buildkitRequest, _ io.Writer) error {
		used, got = "buildkit", req
		return nil
	}
	for _, test := range []struct {
		name    string
		secrets map[string]string
		want    string
	}{
		{"absent", nil, "classic"},
		{"empty", map[string]string{}, "classic"},
		{"present", map[string]string{"NPM_TOKEN": "v"}, "buildkit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			used = ""
			var logs bytes.Buffer
			if _, err := (&BuildDockerImage{}).Run(buildDockerImageParameters(test.secrets), &logs); err != nil {
				t.Fatal(err)
			}
			if used != test.want {
				t.Errorf("built with %q, want %q", used, test.want)
			}
		})
	}
	want := buildkitRequest{repoDir: "/tmp/org/build/repo", dockerFile: "Dockerfile", imageNameAndTag: "org-deploy:abc123", secrets: map[string]string{"NPM_TOKEN": "v"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildKit request = %+v, want %+v", got, want)
	}
}

// The start line names the secrets, sorted, and never a value; the build args
// are the classic build's.
func TestBuildImageWithBuildkitStartLine(t *testing.T) {
	d := &fakeBuildkitDocker{present: true}
	orig := runnerBuildkitBuilder
	t.Cleanup(func() { runnerBuildkitBuilder = orig })
	runnerBuildkitBuilder = newTestBuildkitBuilder(t, d)
	parameters := buildDockerImageParameters(nil)
	jobs.SetParameterValue(parameters, parameters_enums.DockerBuildArgs, primitive.A{"VERSION=1.2.3", "MODE=prod"})
	req := testBuildkitRequest()
	var logs bytes.Buffer
	if err := buildImageWithBuildkit(parameters, req, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(logs.String(), "Building docker image with BuildKit (build secrets: GH_TOKEN, NPM_TOKEN)\n") {
		t.Errorf("log = %q", logs.String())
	}
	assertNoSecretValue(t, "the log", logs.String(), req)
	cmd := strings.Join(d.execs[len(d.execs)-2], " ")
	for _, want := range []string{"--opt build-arg:MODE=prod", "--opt build-arg:VERSION=1.2.3"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("build argv %q lacks %q", cmd, want)
		}
	}
}

func TestBuildkitContainerConfig(t *testing.T) {
	d := &fakeBuildkitDocker{present: true}
	b := newTestBuildkitBuilder(t, d)
	req := testBuildkitRequest()
	if err := b.build(req, io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, hc := d.configs[0], d.hostConfigs[0]
	if cfg.Image != "docker.io/moby/buildkit:v0.34.1-rootless" {
		t.Errorf("image = %q", cfg.Image)
	}
	if cfg.Entrypoint != nil || cfg.Cmd != nil || cfg.User != "" {
		t.Errorf("the image's own entrypoint and user must run; got %v %v %q", cfg.Entrypoint, cfg.Cmd, cfg.User)
	}
	if !reflect.DeepEqual(cfg.Labels, reclaim.Labels(reclaim.PurposeBuildkit)) {
		t.Errorf("labels = %v", cfg.Labels)
	}
	outputDir := d.outputDir(t)
	checks := map[string]bool{
		"Memory is the cap":             hc.Memory == testBuildkitMemory,
		"no swap":                       hc.MemorySwap == hc.Memory,
		"NanoCPUs = cores x 1e9":        hc.NanoCPUs == 4_000_000_000,
		"PidsLimit 4096":                hc.PidsLimit != nil && *hc.PidsLimit == 4096,
		"seccomp and apparmor lifted":   reflect.DeepEqual(hc.SecurityOpt, []string{"seccomp=unconfined", "apparmor=unconfined"}),
		"MaskedPaths empty, not nil":    hc.MaskedPaths != nil && len(hc.MaskedPaths) == 0,
		"ReadonlyPaths empty, not nil":  hc.ReadonlyPaths != nil && len(hc.ReadonlyPaths) == 0,
		"not privileged":                !hc.Privileged,
		"no added capabilities":         len(hc.CapAdd) == 0,
		"bridge network":                hc.NetworkMode == "bridge",
		"metadata hosts pinned":         reflect.DeepEqual(hc.ExtraHosts, cloudMetadataExtraHosts()),
		"output dir under outputRoot":   filepath.Dir(outputDir) == b.outputRoot && strings.HasPrefix(filepath.Base(outputDir), "buildkit-out-"),
		"no no-new-privileges anywhere": !strings.Contains(strings.Join(hc.SecurityOpt, ","), "no-new-privileges"),
	}
	for what, ok := range checks {
		if !ok {
			t.Errorf("%s: host config %+v", what, hc)
		}
	}
	wantMounts := []mount.Mount{
		{Type: mount.TypeBind, Source: req.repoDir, Target: req.repoDir, ReadOnly: true},
		{Type: mount.TypeBind, Source: outputDir, Target: outputDir},
		{Type: mount.TypeVolume, Source: "buildkit-cache", Target: "/home/user/.local/share/buildkit"},
	}
	if !reflect.DeepEqual(hc.Mounts, wantMounts) {
		t.Errorf("mounts = %+v, want %+v", hc.Mounts, wantMounts)
	}
}

// The image is pulled only when it isn't on the host.
func TestBuildkitPullsAMissingImage(t *testing.T) {
	d := &fakeBuildkitDocker{}
	if err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.pulls, []string{buildkitImage}) {
		t.Errorf("pulls = %v", d.pulls)
	}
	d2 := &fakeBuildkitDocker{present: true}
	if err := newTestBuildkitBuilder(t, d2).build(testBuildkitRequest(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(d2.pulls) != 0 {
		t.Errorf("a present image was pulled: %v", d2.pulls)
	}
}

func TestBuildctlBuildCmd(t *testing.T) {
	req := testBuildkitRequest()
	req.secrets["PATH"] = "/evil/bin"
	cmd := buildctlBuildCmd(req, "/tmp/buildkit-out-1")
	want := []string{"buildctl", "build", "--progress", "plain", "--frontend", "dockerfile.v0",
		"--local", "context=/tmp/org/build/repo",
		"--local", "dockerfile=/tmp/org/build/repo",
		"--opt", "filename=Dockerfile",
		"--opt", "platform=linux/" + runtime.GOARCH,
		"--opt", "build-arg:VERSION=1.2.3",
		"--opt", "label:io.deployment.runner=true",
		"--opt", "label:io.deployment.runner.purpose=deployment-image",
		"--secret", "id=GH_TOKEN,env=DEPLOYMENT_IO_BUILD_SECRET_GH_TOKEN",
		"--secret", "id=NPM_TOKEN,env=DEPLOYMENT_IO_BUILD_SECRET_NPM_TOKEN",
		"--secret", "id=PATH,env=DEPLOYMENT_IO_BUILD_SECRET_PATH",
		"--output", "type=docker,name=org-deploy:abc123,dest=/tmp/buildkit-out-1/image.tar",
	}
	if !reflect.DeepEqual(cmd, want) {
		t.Errorf("argv =\n%q\nwant\n%q", cmd, want)
	}
	assertNoSecretValue(t, "the argv", strings.Join(cmd, " "), req)

	req.dockerFile = "docker/Dockerfile.prod"
	if cmd := strings.Join(buildctlBuildCmd(req, "/o"), " "); !strings.Contains(cmd, "--opt filename=docker/Dockerfile.prod") {
		t.Errorf("custom Dockerfile not passed: %q", cmd)
	}

	env := buildctlSecretEnv(req.secrets)
	wantEnv := []string{
		"DEPLOYMENT_IO_BUILD_SECRET_GH_TOKEN=ghp_an0thervalue",
		"DEPLOYMENT_IO_BUILD_SECRET_NPM_TOKEN=npm_s3cr3tvalue",
		"DEPLOYMENT_IO_BUILD_SECRET_PATH=/evil/bin",
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("env = %q, want %q", env, wantEnv)
	}
}

// The build exec gets the values in its Env only; the other execs get none.
func TestBuildkitSecretsOnlyInTheBuildExecEnv(t *testing.T) {
	d := &fakeBuildkitDocker{present: true}
	req := testBuildkitRequest()
	req.secrets["PATH"] = "/evil/bin"
	if err := newTestBuildkitBuilder(t, d).build(req, io.Discard); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range d.execs {
		assertNoSecretValue(t, "an exec argv", strings.Join(cmd, " "), req)
		env := d.execEnvs[i]
		if cmd[1] != "build" {
			if len(env) != 0 {
				t.Errorf("%s exec env = %q", cmd[1], env)
			}
			continue
		}
		for _, kv := range env {
			if !strings.HasPrefix(kv, buildkitSecretEnvPrefix) {
				t.Errorf("build exec env %q isn't prefixed", kv)
			}
		}
		if len(env) != 3 {
			t.Errorf("build exec env has %d entries, want 3", len(env))
		}
	}
}

// A value the build prints reaches the job log as <NAME>.
func TestBuildkitRedactsBuildOutput(t *testing.T) {
	d := &fakeBuildkitDocker{present: true}
	d.build = func(_ context.Context, cmd []string, out io.Writer) (int, error) {
		io.WriteString(out, "#5 [2/3] RUN echo npm_s3cr3t")
		io.WriteString(out, "value\n#6 done, last line ghp_an0thervalue")
		return writeFakeTar(cmd)
	}
	req := testBuildkitRequest()
	var logs bytes.Buffer
	if err := newTestBuildkitBuilder(t, d).build(req, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "RUN echo <NPM_TOKEN>\n") || !strings.Contains(logs.String(), "last line <GH_TOKEN>") {
		t.Errorf("log = %q", logs.String())
	}
	assertNoSecretValue(t, "the log", logs.String(), req)
}

func TestBuildkitNeedsUserNamespaces(t *testing.T) {
	d := &fakeBuildkitDocker{present: true, userns: 0, usernsOK: true}
	err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), io.Discard)
	if err == nil || err.Error() != "This runner host has user namespaces disabled, which build secrets need; update the runner's CloudFormation stack." {
		t.Fatalf("err = %v", err)
	}
	if len(d.configs) != 0 || len(d.events) != 0 {
		t.Errorf("Docker was touched: %v", d.events)
	}

	// An unreadable value goes ahead.
	d = &fakeBuildkitDocker{present: true, userns: 0, usernsOK: false}
	if err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(d.configs) != 1 {
		t.Errorf("%d containers created, want 1", len(d.configs))
	}
}

// A leftover buildkit container is removed before the new one is created; the
// new one is removed after the build, after the prune; the output directory is
// gone and the tar was loaded first.
func TestBuildkitSuccessLifecycle(t *testing.T) {
	d := &fakeBuildkitDocker{present: true, leftovers: []string{"old-buildkit"}, probesFailing: 2}
	b := newTestBuildkitBuilder(t, d)
	if err := b.build(testBuildkitRequest(), io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"remove old-buildkit", "volume", "create buildkit-a", "start buildkit-a",
		"exec debug", "exec debug", "exec debug", "exec build", "exec prune", "load", "remove buildkit-a"}
	if !reflect.DeepEqual(d.events, want) {
		t.Errorf("events =\n%q\nwant\n%q", d.events, want)
	}
	outputDir := d.outputDir(t)
	if !reflect.DeepEqual(d.loaded, []string{filepath.Join(outputDir, "image.tar")}) {
		t.Errorf("loaded = %v", d.loaded)
	}
	if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
		t.Errorf("output dir still there: %v", err)
	}
}

func TestBuildkitFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		docker  *fakeBuildkitDocker
		short   bool
		wantErr string
	}{
		{
			name:    "a non-zero exit",
			docker:  &fakeBuildkitDocker{build: func(context.Context, []string, io.Writer) (int, error) { return 1, nil }},
			wantErr: "The image build failed",
		},
		{
			name: "out of memory",
			docker: &fakeBuildkitDocker{oom: true, build: func(context.Context, []string, io.Writer) (int, error) {
				return 1, nil
			}},
			wantErr: "The image build ran out of memory (limit 6.0 GiB)",
		},
		{
			name: "a timeout",
			docker: &fakeBuildkitDocker{oom: true, build: func(ctx context.Context, _ []string, _ io.Writer) (int, error) {
				<-ctx.Done()
				return 0, ctx.Err()
			}},
			short:   true,
			wantErr: "The image build timed out after 30 minutes",
		},
		{
			name: "an exec error",
			docker: &fakeBuildkitDocker{build: func(context.Context, []string, io.Writer) (int, error) {
				return 0, errors.New("connection reset")
			}},
			wantErr: "running the image build: connection reset",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := test.docker
			d.present = true
			b := newTestBuildkitBuilder(t, d)
			if test.short {
				b.buildTimeout = 20 * time.Millisecond
			}
			req := testBuildkitRequest()
			err := b.build(req, io.Discard)
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("err = %v, want %q", err, test.wantErr)
			}
			if strings.Contains(err.Error(), "_BYTES") || strings.Contains(err.Error(), "BUILD_IMAGE") {
				t.Errorf("error names a runner environment variable: %v", err)
			}
			assertNoSecretValue(t, "the error", err.Error(), req)
			if got := d.execNames(); !reflect.DeepEqual(got, []string{"build", "prune"}) {
				t.Errorf("execs = %v, want build then prune", got)
			}
			if !reflect.DeepEqual(d.removed, []string{"buildkit-a"}) {
				t.Errorf("removed = %v", d.removed)
			}
			if len(d.loaded) != 0 {
				t.Errorf("a failed build was loaded")
			}
			if _, err := os.Stat(d.outputDir(t)); !os.IsNotExist(err) {
				t.Errorf("output dir still there: %v", err)
			}
		})
	}
}

// A prune error is logged and doesn't fail the build.
func TestBuildkitPruneErrorDoesNotFailTheBuild(t *testing.T) {
	d := &fakeBuildkitDocker{present: true, pruneErr: errors.New("prune broke")}
	var logs bytes.Buffer
	if err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "Pruning the BuildKit cache failed: prune broke") {
		t.Errorf("log = %q", logs.String())
	}
	if cmd := d.execs[len(d.execs)-1]; !reflect.DeepEqual(cmd, []string{"buildctl", "prune", "--keep-storage", "5120"}) {
		t.Errorf("prune argv = %q", cmd)
	}
}

func TestBuildkitDidNotStart(t *testing.T) {
	d := &fakeBuildkitDocker{present: true, probesFailing: -1}
	err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "BuildKit didn't start") || !strings.Contains(err.Error(), "cannot set up user namespace") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(d.removed, []string{"buildkit-a"}) {
		t.Errorf("removed = %v", d.removed)
	}
	if got := d.execNames(); len(got) != 0 {
		t.Errorf("ran %v without buildkitd", got)
	}
}

// A probe that hangs, or answers only after the 60 s (here 200 ms) wait,
// doesn't stretch it: the probe ends at the overall deadline and a late
// answer isn't taken as ready.
func TestBuildkitReadyWaitHasOneDeadline(t *testing.T) {
	for name, probe := range map[string]func(ctx context.Context) (int, error){
		"hangs": func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		},
		"answers late": func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeBuildkitDocker{present: true, probe: probe}
			started := time.Now()
			err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), io.Discard)
			if err == nil || !strings.HasPrefix(err.Error(), "BuildKit didn't start") {
				t.Fatalf("err = %v", err)
			}
			if took := time.Since(started); took > 5*time.Second {
				t.Errorf("the wait took %s, past its deadline", took)
			}
			for _, cmd := range d.execNames() {
				if cmd == "build" {
					t.Error("built without buildkitd answering in time")
				}
			}
		})
	}
}

// A line longer than the job log scanner's 64 KiB token limit reaches it in
// pieces, and a job log that stops taking output doesn't fail the build.
func TestBuildkitBuildLogLines(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	d := &fakeBuildkitDocker{present: true}
	d.build = func(_ context.Context, cmd []string, out io.Writer) (int, error) {
		io.WriteString(out, "#5 "+long+"\n#6 done\n")
		return writeFakeTar(cmd)
	}
	reader, writer := io.Pipe()
	var lines []string
	var scanErr error
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		defer reader.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Split(utils.ScanCRLF)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		scanErr = scanner.Err()
	}()
	if err := newTestBuildkitBuilder(t, d).build(testBuildkitRequest(), writer); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	<-scanned
	if scanErr != nil {
		t.Fatalf("scanner: %v", scanErr)
	}
	if got := strings.Join(lines, ""); !strings.Contains(got, "#5 "+long) || !strings.Contains(got, "#6 done") {
		t.Errorf("the log lost output (%d lines)", len(lines))
	}

	closed, w := io.Pipe()
	closed.Close()
	if err := newTestBuildkitBuilder(t, &fakeBuildkitDocker{present: true}).build(testBuildkitRequest(), w); err != nil {
		t.Errorf("a closed job log failed the build: %v", err)
	}
}

// Two builds never run at once.
func TestBuildkitBuildsRunOneAtATime(t *testing.T) {
	d := &fakeBuildkitDocker{present: true}
	var mu sync.Mutex
	running, maxRunning := 0, 0
	d.build = func(_ context.Context, cmd []string, _ io.Writer) (int, error) {
		mu.Lock()
		running++
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return writeFakeTar(cmd)
	}
	b := newTestBuildkitBuilder(t, d)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = b.build(testBuildkitRequest(), io.Discard)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxRunning != 1 {
		t.Errorf("%d builds ran at once", maxRunning)
	}
}
