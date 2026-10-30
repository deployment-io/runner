package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
)

// --- the gate: a backend using a Go module the Task also changes -------------

const (
	kitGoMod = "module github.com/deployment-io/kit\n\ngo 1.22\n"
	// appServerGoMod requires kit in a require block.
	appServerGoMod = `module github.com/deployment-io/app-server

go 1.22

require (
	github.com/aws/aws-sdk-go-v2 v1.30.0
	github.com/deployment-io/kit v1.4.0 // pinned
)

require golang.org/x/sys v0.20.0 // indirect
`
	appServerDroppedLog    = "QA stage: leaving 2-deployment-io/app-server's backend preview out — it uses the Go module github.com/deployment-io/kit from 1-deployment-io/kit, which this Task also changes, and a preview builds against the version in its go.mod; QA can check it once that change is released"
	appServerDroppedReason = "2-deployment-io/app-server depends on 1-deployment-io/kit, which this Task also changes; previews build against the released 1-deployment-io/kit, so QA can't check this change yet"
)

// goModRepo is one repository of a go.mod gate test: its go.mod ("" for none)
// and whether the Task changed it.
type goModRepo struct {
	dir, goMod string
	changed    bool
}

// goModGateStage is a stage over repos, with static the static sites and
// backend the repositories that have a backend preview.
func goModGateStage(t *testing.T, logs *strings.Builder, image string, repos []goModRepo, static, backend []string) *qaStage {
	t.Helper()
	workDir := t.TempDir()
	var bases []baseCommitOutput
	for i, repo := range repos {
		if repo.goMod != "" {
			writeFile(t, filepath.Join(workDir, repo.dir, "go.mod"), repo.goMod)
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

func TestQAGateLeavesOutABackendUsingAChangedGoModule(t *testing.T) {
	const kit, app, web = "1-deployment-io/kit", "2-deployment-io/app-server", "0-deployment-io/web"
	for _, tc := range []struct {
		name                     string
		image                    string
		repos                    []goModRepo
		static, backend          []string
		wantStatic, wantBackends []string
		wantStatus, wantReason   string
		wantDropped              bool
	}{
		{
			name: "require block, kit changed", image: backendQAImage,
			repos:      []goModRepo{{kit, kitGoMod, true}, {app, appServerGoMod, true}},
			backend:    []string{app},
			wantStatus: tasks.QAStatusSkipped, wantReason: appServerDroppedReason, wantDropped: true,
		},
		{
			name: "single-line require", image: backendQAImage,
			repos:      []goModRepo{{kit, kitGoMod, true}, {app, "module github.com/deployment-io/app-server\n\nrequire github.com/deployment-io/kit v1.4.0\n", true}},
			backend:    []string{app},
			wantStatus: tasks.QAStatusSkipped, wantReason: appServerDroppedReason, wantDropped: true,
		},
		{
			name: "indirect, quoted", image: backendQAImage,
			repos:      []goModRepo{{kit, "module \"github.com/deployment-io/kit\"\n", true}, {app, "module github.com/deployment-io/app-server\nrequire (\n\t\"github.com/deployment-io/kit\" v1.4.0 // indirect\n)\n", true}},
			backend:    []string{app},
			wantStatus: tasks.QAStatusSkipped, wantReason: appServerDroppedReason, wantDropped: true,
		},
		{
			name: "an empty one-line block, then a single-line require", image: backendQAImage,
			repos:      []goModRepo{{kit, kitGoMod, true}, {app, "module github.com/deployment-io/app-server\nrequire ( )\nrequire github.com/deployment-io/kit v1.4.0\n", true}},
			backend:    []string{app},
			wantStatus: tasks.QAStatusSkipped, wantReason: appServerDroppedReason, wantDropped: true,
		},
		{
			name: "require( without a space", image: backendQAImage,
			repos:      []goModRepo{{kit, kitGoMod, true}, {app, "module github.com/deployment-io/app-server\nrequire(\n\tgithub.com/deployment-io/kit v1.4.0\n)\n", true}},
			backend:    []string{app},
			wantStatus: tasks.QAStatusSkipped, wantReason: appServerDroppedReason, wantDropped: true,
		},
		{
			name: "kit unchanged", image: backendQAImage,
			repos:        []goModRepo{{kit, kitGoMod, false}, {app, appServerGoMod, true}},
			backend:      []string{app},
			wantBackends: []string{app},
		},
		{
			name: "stack path: app-server unchanged, kit changed", image: stackQAImage,
			repos:      []goModRepo{{kit, kitGoMod, true}, {app, appServerGoMod, false}},
			backend:    []string{app},
			wantStatus: tasks.QAStatusSkipped, wantReason: appServerDroppedReason, wantDropped: true,
		},
		{
			name: "backend without go.mod", image: backendQAImage,
			repos:        []goModRepo{{kit, kitGoMod, true}, {app, "", true}},
			backend:      []string{app},
			wantBackends: []string{app},
		},
		{
			name: "changed repository without go.mod", image: backendQAImage,
			repos:        []goModRepo{{kit, "", true}, {app, appServerGoMod, true}},
			backend:      []string{app},
			wantBackends: []string{app},
		},
		{
			name: "changed repository without a module line", image: backendQAImage,
			repos:        []goModRepo{{kit, "go 1.22\n// module github.com/deployment-io/kit\n", true}, {app, appServerGoMod, true}},
			backend:      []string{app},
			wantBackends: []string{app},
		},
		{
			name: "a module path that's only a prefix", image: backendQAImage,
			repos:        []goModRepo{{kit, kitGoMod, true}, {app, "module github.com/deployment-io/app-server\nrequire github.com/deployment-io/kit-extra v1.0.0\n", true}},
			backend:      []string{app},
			wantBackends: []string{app},
		},
		{
			name: "a static site is left", image: backendQAImage,
			repos:  []goModRepo{{web, "", true}, {kit, kitGoMod, true}, {app, appServerGoMod, true}},
			static: []string{web}, backend: []string{app},
			wantStatic: []string{web}, wantDropped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			stage := goModGateStage(t, &logs, tc.image, tc.repos, tc.static, tc.backend)
			g := stage.gate()
			if !reflect.DeepEqual(g.static, tc.wantStatic) || !reflect.DeepEqual(g.backends, tc.wantBackends) {
				t.Errorf("static %v, backends %v; want %v, %v", g.static, g.backends, tc.wantStatic, tc.wantBackends)
			}
			if g.status != tc.wantStatus || g.reason != tc.wantReason {
				t.Errorf("gate = %q %q, want %q %q", g.status, g.reason, tc.wantStatus, tc.wantReason)
			}
			if got := strings.Contains(logs.String(), appServerDroppedLog); got != tc.wantDropped {
				t.Errorf("the drop logged = %v, want %v:\n%s", got, tc.wantDropped, logs.String())
			}
		})
	}
}

// A go.mod that can't be read (other than not being there) keeps the backend,
// logged.
func TestQAGateKeepsABackendWhoseGoModCannotBeRead(t *testing.T) {
	const kit, app = "1-deployment-io/kit", "2-deployment-io/app-server"
	for _, unreadable := range []string{app, kit} {
		t.Run(unreadable, func(t *testing.T) {
			var logs strings.Builder
			stage := goModGateStage(t, &logs, backendQAImage, []goModRepo{{kit, kitGoMod, true}, {app, appServerGoMod, true}}, nil, []string{app})
			stage.readFile = func(path string) ([]byte, error) {
				if path == filepath.Join(stage.workDirHost, unreadable, "go.mod") {
					return nil, errors.New("permission denied")
				}
				return os.ReadFile(path)
			}
			g := stage.gate()
			if !reflect.DeepEqual(g.backends, []string{app}) || g.status != "" {
				t.Errorf("gate = %+v, want the backend kept", g)
			}
			want := fmt.Sprintf("QA stage: could not read %s's go.mod (permission denied)", unreadable)
			if !strings.Contains(logs.String(), want) || strings.Contains(logs.String(), appServerDroppedLog) {
				t.Errorf("log:\n%s", logs.String())
			}
		})
	}
}

// A go.mod that is a symlink — here to /dev/zero, which would never stop being
// read — is not read: the backend is kept, logged.
func TestQAGateDoesNotFollowAGoModSymlink(t *testing.T) {
	const kit, app = "1-deployment-io/kit", "2-deployment-io/app-server"
	var logs strings.Builder
	stage := goModGateStage(t, &logs, backendQAImage, []goModRepo{{kit, kitGoMod, true}, {app, "", true}}, nil, []string{app})
	if err := os.Symlink("/dev/zero", filepath.Join(stage.workDirHost, app, "go.mod")); err != nil {
		t.Fatal(err)
	}
	g := stage.gate()
	if !reflect.DeepEqual(g.backends, []string{app}) || g.status != "" {
		t.Errorf("gate = %+v, want the backend kept", g)
	}
	if want := fmt.Sprintf("QA stage: could not read %s's go.mod (not a regular file", app); !strings.Contains(logs.String(), want) {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestReadGoModFile(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "go.mod")
	writeFile(t, regular, kitGoMod)
	if data, err := readGoModFile(regular); err != nil || string(data) != kitGoMod {
		t.Errorf("regular file = %q, %v", data, err)
	}
	if _, err := readGoModFile(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v, want fs.ErrNotExist", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	writeFile(t, large, strings.Repeat("x", goModMaxBytes+1))
	for _, path := range []string{link, fifo, large, "/dev/zero", dir} {
		if _, err := readGoModFile(path); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v, want an error other than not-found", path, err)
		}
	}
}

// The module path may also be declared in a block.
func TestParseGoModModuleBlock(t *testing.T) {
	for _, data := range []string{
		"module (\n\tgithub.com/deployment-io/kit\n)\n",
		"module (\n\t\"github.com/deployment-io/kit\" // the kit\n)\n",
		"module ( github.com/deployment-io/kit )\n",
		"module(github.com/deployment-io/kit)\n",
		"module ( github.com/deployment-io/kit\n)\n",
	} {
		if module, _ := parseGoMod([]byte(data)); module != "github.com/deployment-io/kit" {
			t.Errorf("parseGoMod(%q) module = %q", data, module)
		}
	}
	module, required := parseGoMod([]byte("module (\n\tgithub.com/deployment-io/app-server\n)\nrequire github.com/deployment-io/kit v1.4.0\n"))
	if module != "github.com/deployment-io/app-server" || !reflect.DeepEqual(required, map[string]bool{"github.com/deployment-io/kit": true}) {
		t.Errorf("module %q, required %v", module, required)
	}
}

// A changed library declaring its module in a block still drops the backend.
func TestQAGateLeavesOutABackendUsingAModuleBlock(t *testing.T) {
	const kit, app = "1-deployment-io/kit", "2-deployment-io/app-server"
	var logs strings.Builder
	stage := goModGateStage(t, &logs, backendQAImage, []goModRepo{{kit, "module (\n\tgithub.com/deployment-io/kit\n)\n", true}, {app, appServerGoMod, true}}, nil, []string{app})
	g := stage.gate()
	if g.backends != nil || g.status != tasks.QAStatusSkipped || g.reason != appServerDroppedReason {
		t.Errorf("gate = %v %q %q", g.backends, g.status, g.reason)
	}
}

// In the stack path the gate hands the dropped backend on, so the round can
// report it; outside it, it doesn't.
func TestQAGateHandsTheDroppedBackendToTheStack(t *testing.T) {
	const web, kit, app = "0-deployment-io/web", "1-deployment-io/kit", "2-deployment-io/app-server"
	repos := []goModRepo{{web, "", true}, {kit, kitGoMod, true}, {app, appServerGoMod, true}}
	want := []droppedBackend{{backendDir: app, dependencyDir: kit, modulePath: "github.com/deployment-io/kit"}}
	for _, tc := range []struct {
		image string
		want  []droppedBackend
	}{{stackQAImage, want}, {backendQAImage, nil}} {
		var logs strings.Builder
		g := goModGateStage(t, &logs, tc.image, repos, []string{web}, []string{app}).gate()
		if !reflect.DeepEqual(g.static, []string{web}) || g.backends != nil || g.status != "" {
			t.Errorf("%s: gate = %v %v %q", tc.image, g.static, g.backends, g.status)
		}
		if !reflect.DeepEqual(g.dropped, tc.want) {
			t.Errorf("%s: dropped = %+v, want %+v", tc.image, g.dropped, tc.want)
		}
	}
}

// A stack round reports a dropped backend as a backend preview that couldn't
// be planned: nothing is deployed for it, and the site lists it unconnected.
func TestQAStackReportsADroppedBackend(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
		links: []task_previews.StackLinkV1{webLink(urlVar("VITE_API_URL", ""), originVar("CORS_ORIGINS", ","))}}
	stage, _, specs := stackStage(t, []string{"web", "api", "kit"}, []string{"0-web"}, nil, f)
	stage.droppedBackends = []droppedBackend{{backendDir: "1-api", dependencyDir: "2-kit", modulePath: "github.com/acme/kit"}}
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	if want := []string{"plan site 0-web", "wiring", "deploy site 0-web", "spawn"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v\nwant %v", f.events, want)
	}
	if len(f.siteVars["0-web"]) != 0 {
		t.Errorf("the site was wired to a dropped backend: %v", f.siteVars["0-web"])
	}
	got := envMap((*specs)[0].env)["QA_PREVIEWS"]
	const want = `[{"repository":"0-web","kind":"site","url":"https://s-0-web.cloudfront.net","unconnected_backends":["1-api"]},` +
		`{"repository":"1-api","kind":"backend","error":"left out: it uses the Go module github.com/acme/kit from 2-kit, which this Task also changes, and a preview builds against the version in its go.mod"}]`
	if got != want {
		t.Errorf("QA_PREVIEWS = %s\nwant %s", got, want)
	}
	if want := []qaWiringOutput{{Frontend: "web", API: "api", Reason: qaWiringBackendLeftOut}}; !reflect.DeepEqual(stage.roundWiring, want) {
		t.Errorf("wiring = %+v", stage.roundWiring)
	}
	// The pull request says it was left out, not that it failed to deploy.
	if line := qaWiringLine(stage.roundWiring[0], stage.roundWiring); !strings.Contains(line, "api's preview was left out") || strings.Contains(line, "failed") {
		t.Errorf("wiring line = %q", line)
	}
}

// A line inside another directive's block is never read as module or require.
func TestParseGoModSkipsOtherBlocks(t *testing.T) {
	data := "module github.com/deployment-io/kit\n\n" +
		"replace (\n\tmodule => ./module\n\trequire => ./require\n)\n" +
		"exclude ( example.com/old v1.0.0 )\n" +
		"require example.com/dep v1.0.0\n" +
		"retract (\n\tv1.0.0\n)\n" +
		"require (\n\tgithub.com/acme/kit v1.2.0 // indirect\n)\n"
	module, required := parseGoMod([]byte(data))
	if module != "github.com/deployment-io/kit" {
		t.Errorf("module = %q", module)
	}
	if want := map[string]bool{"example.com/dep": true, "github.com/acme/kit": true}; !reflect.DeepEqual(required, want) {
		t.Errorf("required = %v, want %v", required, want)
	}
}
