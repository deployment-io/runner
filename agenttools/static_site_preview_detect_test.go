package agenttools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// detectDeps is planDeps for a repository deployment.io doesn't deploy, under
// workDir, detecting detected (or nothing, for reason) and recording where.
func detectDeps(workDir string, reply task_previews.StaticSiteBuildSettingsReplyV1, detected *DetectedSite, reason string, gotDir *string) DeployStaticSitePreviewDeps {
	deps := planDeps(reply, nil)
	deps.WorkDirHost = workDir
	deps.DetectSite = func(siteDir string) (DetectedSite, string, bool) {
		if gotDir != nil {
			*gotDir = siteDir
		}
		if detected == nil {
			return DetectedSite{}, reason, false
		}
		return *detected, "", true
	}
	return deps
}

var viteSite = DetectedSite{Framework: "Vite", BuildCommand: "pnpm run build", PublishDirectory: "dist", IsSPA: true}

func TestPlanDetectedPreview(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	var gotDir string
	p, ok, err := planRunnerBuiltPreview(detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, &viteSite, "", &gotDir), "0-acme/web", nil)
	if err != nil || !ok {
		t.Fatalf("detectable: ok=%v err=%v; want the runner-built path", ok, err)
	}
	if gotDir != repoDir {
		t.Errorf("detected in %s, want the repository %s", gotDir, repoDir)
	}
	if p.detected == nil || *p.detected != viteSite || p.site.BuildCommand != "pnpm run build" || !p.site.IsSpa ||
		p.publishDir != "dist" || p.rootDirectory != "" || p.serviceName != "acme-web-dist" || p.displayName() != "0-acme/web" {
		t.Errorf("plan %+v", p)
	}

	p, ok, err = planRunnerBuiltPreview(detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, &viteSite, "", &gotDir), "0-acme/web", strPtr("./apps/web/"))
	if err != nil || !ok {
		t.Fatalf("root_directory: ok=%v err=%v", ok, err)
	}
	if gotDir != filepath.Join(repoDir, "apps", "web") || p.rootDirectory != "apps/web" || p.serviceName != "acme-web-apps-web-dist" || p.displayName() != "0-acme/web/apps/web" {
		t.Errorf("root_directory not honoured: detected in %s, plan %+v", gotDir, p)
	}

	// The same refusal as a deployed site's.
	root := DetectedSite{Framework: "Vite", BuildCommand: "npm run build", PublishDirectory: "."}
	if _, _, err := planRunnerBuiltPreview(detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, &root, "", nil), "0-acme/web", nil); err == nil ||
		err.Error() != "0-acme/web's publish directory is its root directory, so its preview can't be built" {
		t.Errorf("publish directory equal to the root: got %v", err)
	}

	if _, _, err := planRunnerBuiltPreview(detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, &viteSite, "", nil), "0-acme/web", strPtr("apps/nope")); err == nil ||
		!strings.Contains(err.Error(), `root_directory "apps/nope" isn't a directory in 0-acme/web`) {
		t.Errorf("missing root_directory: got %v", err)
	}
}

func TestPlanDetectedPreviewRefusesSymlinkOutOfRepository(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repoDir, "escape")); err != nil {
		t.Fatal(err)
	}
	detected := false
	deps := detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, &viteSite, "", nil)
	deps.DetectSite = func(string) (DetectedSite, string, bool) { detected = true; return viteSite, "", true }
	_, _, err := planRunnerBuiltPreview(deps, "0-acme/web", strPtr("escape"))
	if err == nil || !strings.Contains(err.Error(), `root_directory "escape" resolves outside 0-acme/web`) {
		t.Errorf("got %v, want root_directory refused", err)
	}
	if detected {
		t.Error("nothing outside the repository may be read")
	}
}

func TestPlanNotDetectedTakesAgentPath(t *testing.T) {
	workDir, _ := newWorkDir(t)
	deps := detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, nil, `package.json has no "build" script`, nil)
	p, ok, err := planRunnerBuiltPreview(deps, "0-acme/web", nil)
	if err != nil || ok || p.notDetected != `package.json has no "build" script` {
		t.Fatalf("not detectable: ok=%v err=%v reason %q", ok, err, p.notDetected)
	}

	builds := newPreviewBuilds()
	defer builds.stop()
	_, err = handleDeployStaticSitePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"0-acme/web"}`))
	want := `deployment.io doesn't deploy this repository and couldn't detect how to build it (package.json has no "build" script), so build it yourself and pass publish_dir`
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}

	restore := fakeDeploy(t)
	defer restore()
	writeTree(t, filepath.Join(workDir, "0-acme", "web"), map[string]string{"out/index.html": "<html>"})
	out, err := handleDeployStaticSitePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"0-acme/web","publish_dir":"0-acme/web/out"}`))
	if err != nil || !strings.Contains(out, `"built_by":"agent"`) {
		t.Errorf("with publish_dir: %s, %v; want the agent-built deploy", out, err)
	}
}

func TestDeployedRepositoryIsNotDetected(t *testing.T) {
	workDir, _ := newWorkDir(t)
	reply := task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{site("web", "", "npm run build", "build", false)}}
	called := false
	deps := detectDeps(workDir, reply, &viteSite, "", nil)
	deps.DetectSite = func(string) (DetectedSite, string, bool) { called = true; return viteSite, "", true }
	p, ok, err := planRunnerBuiltPreview(deps, "0-acme/web", nil)
	if err != nil || !ok || called || p.detected != nil || p.publishDir != "build" || p.displayName() != "web" {
		t.Errorf("deployed: ok=%v err=%v detect called=%v plan %+v", ok, err, called, p)
	}
}

func TestDetectedSettingsTextAndLogLine(t *testing.T) {
	d := viteSite
	p := runnerBuiltPreview{serviceName: "acme-web-dist", name: "0-acme/web", detected: &d, publishDir: "dist"}
	if got, want := detectedSettingsText(p), `detected from the repository: Vite, build "pnpm run build", publish directory "dist", single-page app: yes`; got != want {
		t.Errorf("settings %q, want %q", got, want)
	}
	if got, want := previewBuildLogLine(p), "Preview build: acme-web-dist built by deployment.io from settings detected in 0-acme/web (Vite) with none — no preview configuration is set\n"; got != want {
		t.Errorf("log line %q, want %q", got, want)
	}
	d.IsSPA = false
	if got := detectedSettingsText(p); !strings.HasSuffix(got, "single-page app: no") {
		t.Errorf("settings %q", got)
	}
	p.configuration = task_previews.StaticSiteBuildSettingsReplyV1{ConfigurationSet: true, Variables: map[string]string{"API_KEY": "s3cret-value"}}
	if got, want := previewBuildLogLine(p), "Preview build: acme-web-dist built by deployment.io from settings detected in 0-acme/web (Vite) with preview configuration (1 variables, 0 secret files)\n"; got != want {
		t.Errorf("log line %q, want %q", got, want)
	}
	if detectedSettingsText(runnerBuiltPreview{site: site("web", "", "npm run build", "dist", false)}) != "" {
		t.Error("a deployed site's build has no detected settings")
	}
}

func TestRunDetectedPreview(t *testing.T) {
	restore := fakeDeploy(t)
	defer restore()
	workDir, _ := newWorkDir(t)
	for _, configured := range []bool{true, false} {
		reply := task_previews.StaticSiteBuildSettingsReplyV1{}
		if configured {
			reply = task_previews.StaticSiteBuildSettingsReplyV1{
				ConfigurationSet: true,
				Variables:        map[string]string{"API_KEY": "s3cret-value"},
				Files:            []task_previews.PreviewFileV1{{Name: ".env", Contents: "TOKEN=f1le-value"}},
			}
		}
		logs := &syncBuffer{}
		deps := detectDeps(workDir, reply, &viteSite, "", nil)
		deps.LogsWriter = logs
		deps.BuildSite = func(ctx context.Context, dir, buildCommand string, env []string, w io.Writer) error {
			if buildCommand != "pnpm run build" {
				t.Errorf("build command %q", buildCommand)
			}
			writeTree(t, dir, map[string]string{"dist/index.html": "<html>"})
			return nil
		}
		p, ok, err := planRunnerBuiltPreview(deps, "0-acme/web", strPtr("apps/web"))
		if err != nil || !ok {
			t.Fatalf("plan: %v %v", ok, err)
		}
		out, err := runRunnerBuiltPreview(context.Background(), deps, p)
		if err != nil {
			t.Fatal(err)
		}
		var res map[string]interface{}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatal(err)
		}
		if res["built_by"] != "deployment.io" || res["settings"] != `detected from the repository: Vite, build "pnpm run build", publish directory "dist", single-page app: yes` {
			t.Errorf("result %s", out)
		}
		wantLine := "Preview build: acme-web-apps-web-dist built by deployment.io from settings detected in 0-acme/web/apps/web (Vite) with " + configurationText(reply) + "\n"
		if !strings.Contains(logs.String(), wantLine) {
			t.Errorf("log %q lacks %q", logs.String(), wantLine)
		}
		for _, value := range []string{"s3cret-value", "f1le-value"} {
			if strings.Contains(out, value) || strings.Contains(logs.String(), value) {
				t.Errorf("a configuration value leaked: result %s, log %q", out, logs.String())
			}
		}
	}

	deps := detectDeps(workDir, task_previews.StaticSiteBuildSettingsReplyV1{}, &viteSite, "", nil)
	deps.BuildSite = func(context.Context, string, string, []string, io.Writer) error { return nil }
	p, _, _ := planRunnerBuiltPreview(deps, "0-acme/web", nil)
	_, err := runRunnerBuiltPreview(context.Background(), deps, p)
	want := "the build of 0-acme/web didn't produce 0-acme/web/dist/index.html. If the project writes its build somewhere else, build it yourself and call deploy_static_site_preview with publish_dir and without repository"
	if err == nil || err.Error() != want {
		t.Errorf("no index.html: got %v, want %q", err, want)
	}
}

func TestCopyRepositoryForBuildLeavesOutEscapingSymlinks(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	links := map[string]string{
		"abs":                 "/etc/passwd",
		"apps/web/up":         "../../../../secret",
		"apps/web/inside":     "../../src/index.js",
		"apps/web/public/dir": "../sub",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(repoDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuffer{}
	dst, err := copyRepositoryForBuild(repoDir, workDir, logs)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dst)
	for _, left := range []string{"abs", "apps/web/up"} {
		if _, err := os.Lstat(filepath.Join(dst, left)); !os.IsNotExist(err) {
			t.Errorf("%s must be left out of the copy (err %v)", left, err)
		}
		if !strings.Contains(logs.String(), "left out the symlink "+left+",") {
			t.Errorf("log %q doesn't name %s", logs.String(), left)
		}
	}
	for _, kept := range []string{"apps/web/inside", "apps/web/public/dir"} {
		got, err := os.Readlink(filepath.Join(dst, kept))
		if err != nil || got != links[kept] {
			t.Errorf("%s: got %q, %v; want the symlink copied unchanged", kept, got, err)
		}
	}
	for _, leaked := range []string{"/etc/passwd", "secret", workDir, dst} {
		if strings.Contains(logs.String(), leaked) {
			t.Errorf("log %q names %q", logs.String(), leaked)
		}
	}
}

func TestCopyRepositoryForBuildLeavesOutChainedEscapingSymlinks(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(repoDir), "outside.yml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Each link stays inside lexically; together .yarnrc.yml resolves outside, and so
	// does .npmrc, whose target doesn't exist beside the source but could beside the copy.
	// missing/../../x leaves past a component the copy may hold differently.
	for name, target := range map[string]string{
		"a": ".", ".yarnrc.yml": "a/../outside.yml", ".npmrc": "a/../absent.yml", "loop": "loop",
		"past-missing": "missing/../../x", "dangling": "missing/file",
	} {
		if err := os.Symlink(target, filepath.Join(repoDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuffer{}
	dst, err := copyRepositoryForBuild(repoDir, workDir, logs)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dst)
	for _, left := range []string{".yarnrc.yml", ".npmrc", "loop", "past-missing"} {
		if _, err := os.Lstat(filepath.Join(dst, left)); !os.IsNotExist(err) {
			t.Errorf("%s must be left out of the copy (err %v)", left, err)
		}
		if !strings.Contains(logs.String(), "left out the symlink "+left+",") {
			t.Errorf("log %q doesn't name %s", logs.String(), left)
		}
	}
	for kept, target := range map[string]string{"a": ".", "dangling": "missing/file"} {
		if got, err := os.Readlink(filepath.Join(dst, kept)); err != nil || got != target {
			t.Errorf("%s: got %q, %v; want the symlink copied unchanged", kept, got, err)
		}
	}
	if strings.Contains(logs.String(), "outside.yml") || strings.Contains(logs.String(), "absent.yml") {
		t.Errorf("log %q names the target", logs.String())
	}
}

// swapForSymlink replaces p with a symlink to target.
func swapForSymlink(t *testing.T, p, target string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// assertNotInCopy fails if any file in dst holds secret.
func assertNotInCopy(t *testing.T, dst, secret string) {
	t.Helper()
	filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), secret) {
				t.Errorf("%s in the copy holds the outside content", p)
			}
		}
		return nil
	})
}

func TestCopyRepositoryForBuildRefusesDirectorySwappedForSymlink(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"environ": "CONTROL_PLANE_TOKEN"})
	logs := &syncBuffer{}
	dst, err := copyRepository(repoDir, workDir, logs, func(rel string) {
		if rel == "src" {
			swapForSymlink(t, filepath.Join(repoDir, "src"), outside)
		}
	})
	defer os.RemoveAll(dst)
	if err == nil || !strings.Contains(err.Error(), "src") {
		t.Fatalf("got %v, want an error naming src", err)
	}
	if strings.Contains(err.Error(), repoDir) {
		t.Errorf("error %q names the host path", err)
	}
	if !strings.Contains(logs.String(), err.Error()) {
		t.Errorf("log %q doesn't hold the error %q", logs.String(), err)
	}
	assertNotInCopy(t, dst, "CONTROL_PLANE_TOKEN")
}

func TestCopyRepositoryForBuildRefusesFileSwappedForSymlink(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	outside := filepath.Join(t.TempDir(), "environ")
	if err := os.WriteFile(outside, []byte("CONTROL_PLANE_TOKEN"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, err := copyRepository(repoDir, workDir, io.Discard, func(rel string) {
		if rel == "package.json" {
			swapForSymlink(t, filepath.Join(repoDir, "package.json"), outside)
		}
	})
	defer os.RemoveAll(dst)
	if err == nil || !strings.Contains(err.Error(), "package.json") {
		t.Fatalf("got %v, want an error naming package.json", err)
	}
	assertNotInCopy(t, dst, "CONTROL_PLANE_TOKEN")
}

func TestCopyRepositoryForBuildChecksSymlinksInTheCopy(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	writeTree(t, repoDir, map[string]string{"d/e/f": "x"})
	for name, target := range map[string]string{"a": "c/..", "c": "d/e"} {
		if err := os.Symlink(target, filepath.Join(repoDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuffer{}
	// a is copied while c -> d/e (a resolves to d); then c changes to "." before it is read.
	dst, err := copyRepository(repoDir, workDir, logs, func(rel string) {
		if rel == "c" {
			swapForSymlink(t, filepath.Join(repoDir, "c"), ".")
		}
	})
	defer os.RemoveAll(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "a")); !os.IsNotExist(err) {
		t.Errorf("a must be removed from the copy (err %v)", err)
	}
	if !strings.Contains(logs.String(), "left out the symlink a,") {
		t.Errorf("log %q doesn't name a", logs.String())
	}
	if got, err := os.Readlink(filepath.Join(dst, "c")); err != nil || got != "." {
		t.Errorf("c: got %q, %v; want the symlink kept", got, err)
	}
}

func TestCopyRepositoryForBuildRechecksLinksThroughRemovedOnes(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	writeTree(t, repoDir, map[string]string{"sub/deep/a/b/f": "x"})
	// sub/z leaves lexically, so it is removed, though followed through sub/x it
	// resolves to sub; a, checked before it in the first pass, resolves through it to
	// the root — and, once sub/z is gone, past a missing component to above the root.
	for name, target := range map[string]string{"sub/x": "deep/a/b", "sub/z": "x/../../..", "a": "sub/z/.."} {
		if err := os.Symlink(target, filepath.Join(repoDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	logs := &syncBuffer{}
	dst, err := copyRepositoryForBuild(repoDir, workDir, logs)
	defer os.RemoveAll(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, left := range []string{"sub/z", "a"} {
		if _, err := os.Lstat(filepath.Join(dst, left)); !os.IsNotExist(err) {
			t.Errorf("%s must be removed from the copy (err %v)", left, err)
		}
		if !strings.Contains(logs.String(), "left out the symlink "+left+",") {
			t.Errorf("log %q doesn't name %s", logs.String(), left)
		}
	}
	if got, err := os.Readlink(filepath.Join(dst, "sub/x")); err != nil || got != "deep/a/b" {
		t.Errorf("sub/x: got %q, %v; want it kept", got, err)
	}
}

func TestCopyRepositoryForBuildRefusesRepositorySwappedForSymlink(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"environ": "CONTROL_PLANE_TOKEN"})
	swapForSymlink(t, repoDir, outside)
	logs := &syncBuffer{}
	dst, err := copyRepositoryForBuild(repoDir, workDir, logs)
	defer os.RemoveAll(dst)
	if err == nil || !strings.Contains(err.Error(), "0-acme/web") || strings.Contains(err.Error(), workDir) {
		t.Fatalf("got %v, want an error naming 0-acme/web and not the host path", err)
	}
	if !strings.Contains(logs.String(), err.Error()) {
		t.Errorf("the log doesn't hold %q: %q", err, logs.String())
	}
	assertNotInCopy(t, dst, "CONTROL_PLANE_TOKEN")
}
