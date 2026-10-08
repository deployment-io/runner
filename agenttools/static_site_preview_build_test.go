package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

var testRepositories = []tasks.RepositoryEntry{
	{Name: "acme/web", CloneURL: "https://github.com/acme/web.git"},
	{Name: "acme/api", CloneURL: "https://github.com/acme/api.git"},
}

func TestResolveTaskRepository(t *testing.T) {
	for _, in := range []string{"1-acme/api", "/work/1-acme/api", " 1-acme/api/ "} {
		dir, entry, err := resolveTaskRepository(testRepositories, in)
		if err != nil || dir != "1-acme/api" || entry.CloneURL != "https://github.com/acme/api.git" {
			t.Errorf("resolveTaskRepository(%q) = %q, %+v, %v", in, dir, entry, err)
		}
	}
	_, _, err := resolveTaskRepository(testRepositories, "acme/api")
	if err == nil || !strings.Contains(err.Error(), `unknown repository "acme/api"`) || !strings.Contains(err.Error(), "0-acme/web, 1-acme/api") {
		t.Errorf("unknown repository: got %v, want the error listing the Task's repositories", err)
	}
}

func site(name, root, build, publish string, spa bool) task_previews.StaticSiteBuildSettingsV1 {
	return task_previews.StaticSiteBuildSettingsV1{DeploymentName: name, EnvironmentName: "production", RootDirectory: root, BuildCommand: build, PublishDirectory: publish, IsSpa: spa}
}

func strPtr(s string) *string { return &s }

func TestChooseStaticSite(t *testing.T) {
	web := site("web", "apps/web", "npm run build", "dist", true)
	webStaging := site("web-staging", "./apps/web/", "npm run build", "dist", true)
	admin := site("admin", "apps/admin", "npm run build", "build", false)

	if got, err := chooseStaticSite([]task_previews.StaticSiteBuildSettingsV1{web, webStaging}, nil); err != nil || got.DeploymentName != "web" {
		t.Errorf("identical settings: got %+v, %v; want the first", got, err)
	}
	if got, err := chooseStaticSite([]task_previews.StaticSiteBuildSettingsV1{web, admin}, strPtr("apps/admin")); err != nil || got.DeploymentName != "admin" {
		t.Errorf("root_directory: got %+v, %v; want admin", got, err)
	}
	if got, err := chooseStaticSite([]task_previews.StaticSiteBuildSettingsV1{web, admin, webStaging}, strPtr("/apps/web")); err != nil || got.DeploymentName != "web" {
		t.Errorf("root_directory matching identical sites: got %+v, %v; want web", got, err)
	}
	repoRoot := site("root", "", "npm run build", "out", false)
	for _, root := range []string{"", "."} {
		if got, err := chooseStaticSite([]task_previews.StaticSiteBuildSettingsV1{web, admin, repoRoot}, strPtr(root)); err != nil || got.DeploymentName != "root" {
			t.Errorf("root_directory %q: got %+v, %v; want the repository-root site", root, got, err)
		}
	}
	// A selector that leaves the repository is refused — never read as the
	// repository root, which cleanRelativeDir returns alongside its error.
	for _, sites := range [][]task_previews.StaticSiteBuildSettingsV1{{web, admin, repoRoot}, {web, webStaging}} {
		if got, err := chooseStaticSite(sites, strPtr("apps/../../other")); err == nil || !strings.Contains(err.Error(), "leaves the repository") {
			t.Errorf("escaping root_directory: got %+v, %v; want a refusal", got, err)
		}
	}
	for _, root := range []*string{nil, strPtr("apps/other")} {
		_, err := chooseStaticSite([]task_previews.StaticSiteBuildSettingsV1{web, admin, repoRoot}, root)
		if err == nil {
			t.Fatalf("ambiguous with root_directory %v: no error", root)
		}
		for _, want := range []string{"pass root_directory", `web (environment production): root_directory "apps/web"`, `admin (environment production): root_directory "apps/admin"`, `root (environment production): root_directory "" (the repository root)`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ambiguous error %q doesn't contain %q", err, want)
			}
		}
	}
}

func TestCleanRelativeDir(t *testing.T) {
	for in, want := range map[string]string{"": "", ".": "", "/": "", "./apps/web/": "apps/web", "/apps/web": "apps/web", "apps//web": "apps/web", "dist": "dist"} {
		if got, err := cleanRelativeDir(in); err != nil || got != want {
			t.Errorf("cleanRelativeDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := cleanRelativeDir("apps/../../etc"); err == nil {
		t.Error("a directory leaving the repository must be refused")
	}
}

func planDeps(reply task_previews.StaticSiteBuildSettingsReplyV1, gotURL *string) DeployStaticSitePreviewDeps {
	return DeployStaticSitePreviewDeps{
		Repositories: testRepositories,
		BuildSettings: func(cloneURL string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
			if gotURL != nil {
				*gotURL = cloneURL
			}
			return reply, nil
		},
		LogsWriter: io.Discard,
	}
}

func TestPlanRunnerBuiltPreview(t *testing.T) {
	var gotURL string
	_, ok, err := planRunnerBuiltPreview(planDeps(task_previews.StaticSiteBuildSettingsReplyV1{}, &gotURL), "0-acme/web", nil)
	if err != nil || ok {
		t.Errorf("no sites: ok=%v err=%v; want the agent-built path", ok, err)
	}
	if gotURL != "https://github.com/acme/web.git" {
		t.Errorf("asked with clone URL %q, want the repository's", gotURL)
	}

	reply := task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{site("web", "apps/web", "npm run build", "dist", true)}}
	p, ok, err := planRunnerBuiltPreview(planDeps(reply, nil), "0-acme/web", nil)
	if err != nil || !ok {
		t.Fatalf("one site: ok=%v err=%v", ok, err)
	}
	if want, _ := deriveServiceName("0-acme/web/apps/web/dist"); p.serviceName != want || want != "acme-web-apps-web-dist" {
		t.Errorf("service name %q, want %q (the agent-built deploy's)", p.serviceName, want)
	}

	reply.Sites[0].BuildCommand = " "
	_, _, err = planRunnerBuiltPreview(planDeps(reply, nil), "0-acme/web", nil)
	if err == nil || err.Error() != "web has no build command, so its preview can't be built" {
		t.Errorf("empty build command: got %v", err)
	}

	// The deploy refuses a publish directory that is the root directory: it would
	// publish the sources and the secret files written there.
	reply.Sites[0].BuildCommand = "npm run build"
	for _, publish := range []string{"", ".", "./", "/"} {
		reply.Sites[0].PublishDirectory = publish
		_, _, err = planRunnerBuiltPreview(planDeps(reply, nil), "0-acme/web", nil)
		if err == nil || err.Error() != "web's publish directory is its root directory, so its preview can't be built" {
			t.Errorf("publish directory %q: got %v", publish, err)
		}
	}

	if _, _, err = planRunnerBuiltPreview(planDeps(reply, nil), "2-acme/nope", nil); err == nil || !strings.Contains(err.Error(), "unknown repository") {
		t.Errorf("unknown repository: got %v", err)
	}
}

func TestHandlerAgentPathRequiresPublishDir(t *testing.T) {
	deps := planDeps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil)
	builds := newPreviewBuilds()
	defer builds.stop()
	_, err := handleDeployStaticSitePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"0-acme/web"}`))
	if err == nil || err.Error() != errNotDeployedSite {
		t.Errorf("got %v, want %q", err, errNotDeployedSite)
	}
	_, err = handleDeployStaticSitePreview(context.Background(), deps, builds, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "publish_dir is required") {
		t.Errorf("no arguments: got %v", err)
	}
}

// resolvePublishDir is lexical, so a symlink under /work whose target is
// outside it must be refused before anything is uploaded.
func TestHandlerAgentPathRefusesSymlinkOutsideWorkDir(t *testing.T) {
	saved := deployPreviewDir
	defer func() { deployPreviewDir = saved }()
	deployed := false
	deployPreviewDir = func(context.Context, DeployStaticSitePreviewDeps, string, string, bool) (deployStaticSitePreviewResult, error) {
		deployed = true
		return deployStaticSitePreviewResult{}, nil
	}
	deps := planDeps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil)
	workDir, repoDir := newWorkDir(t)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"dist/index.html": "<html>", "dist/secret.env": "TOKEN=1"})
	if err := os.Symlink(outside, filepath.Join(repoDir, "link")); err != nil {
		t.Fatal(err)
	}
	deps.WorkDirHost = workDir
	builds := newPreviewBuilds()
	defer builds.stop()
	_, err := handleDeployStaticSitePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"0-acme/web","publish_dir":"0-acme/web/link/dist"}`))
	if err == nil || !strings.Contains(err.Error(), "resolves outside /work through a symlink") {
		t.Errorf("got %v, want a symlink refusal", err)
	}
	if deployed {
		t.Error("deployed a publish_dir that resolves outside /work")
	}
}

func TestHandlerAgentPathBuiltByAgent(t *testing.T) {
	restore := fakeDeploy(t)
	defer restore()
	deps := planDeps(task_previews.StaticSiteBuildSettingsReplyV1{}, nil)
	workDir, repoDir := newWorkDir(t)
	writeTree(t, repoDir, map[string]string{"dist/index.html": "<html>"})
	deps.WorkDirHost = workDir
	builds := newPreviewBuilds()
	defer builds.stop()
	out, err := handleDeployStaticSitePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"0-acme/web","publish_dir":"0-acme/web/dist"}`))
	if err != nil {
		t.Fatal(err)
	}
	var res deployStaticSitePreviewResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.BuiltBy != "agent" || res.Configuration != "" {
		t.Errorf("got %s, want built_by agent and no configuration", out)
	}
}

// newWorkDir returns <tmp>/<task> holding a repository "0-acme/web".
func newWorkDir(t *testing.T) (workDir, repoDir string) {
	t.Helper()
	workDir = filepath.Join(t.TempDir(), "task")
	repoDir = filepath.Join(workDir, "0-acme", "web")
	writeTree(t, repoDir, map[string]string{
		"package.json":                      "{}",
		"src/index.js":                      "x",
		".git/HEAD":                         "ref",
		"node_modules/react/index.js":       "x",
		"apps/web/package.json":             "{}",
		"apps/web/node_modules/vite/bin.js": "x",
		"apps/web/sub/.git":                 "gitdir: elsewhere",
		"apps/web/public/robots.txt":        "ok",
	})
	return workDir, repoDir
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCopyRepositoryForBuild(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	dst, err := copyRepositoryForBuild(repoDir, workDir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dst)
	if filepath.Dir(dst) != filepath.Dir(workDir) || strings.HasPrefix(dst, workDir+string(filepath.Separator)) {
		t.Errorf("copy %s must be beside the work dir %s, not inside it", dst, workDir)
	}
	for _, kept := range []string{"package.json", "src/index.js", "apps/web/package.json", "apps/web/public/robots.txt"} {
		if _, err := os.Stat(filepath.Join(dst, kept)); err != nil {
			t.Errorf("%s missing from the copy: %v", kept, err)
		}
	}
	for _, left := range []string{".git", "node_modules", "apps/web/node_modules", "apps/web/sub/.git"} {
		if _, err := os.Lstat(filepath.Join(dst, left)); !os.IsNotExist(err) {
			t.Errorf("%s must be left out of the copy (err %v)", left, err)
		}
	}
}

func TestWritePreviewFiles(t *testing.T) {
	siteDir := t.TempDir()
	err := writePreviewFiles(siteDir, []task_previews.PreviewFileV1{{Name: ".env", Contents: "A=1"}, {Name: "config/keys.json", Contents: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(siteDir, ".env")); err != nil || string(b) != "A=1" {
		t.Errorf(".env: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(siteDir, "config", "keys.json")); err != nil {
		t.Errorf("config/keys.json: %v", err)
	}

	for _, name := range []string{"../.env", "a/../../.env", ".."} {
		if err := writePreviewFiles(siteDir, []task_previews.PreviewFileV1{{Name: name, Contents: "x"}}); err == nil || !strings.Contains(err.Error(), "outside the site's root directory") {
			t.Errorf("name %q: got %v, want a refusal", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(siteDir), ".env")); !os.IsNotExist(err) {
		t.Error("an escaping name wrote outside the site's root directory")
	}

	// A symlinked directory of the copied tree can't carry a file out of it.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(siteDir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := writePreviewFiles(siteDir, []task_previews.PreviewFileV1{{Name: "linked/.env", Contents: "x"}}); err == nil {
		t.Error("a name through a symlink out of the root directory must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, ".env")); !os.IsNotExist(err) {
		t.Error("the secret file was written through the symlink")
	}
	// Nor are directories created at the symlink's target.
	if err := writePreviewFiles(siteDir, []task_previews.PreviewFileV1{{Name: "linked/nested/deeper/secret.json", Contents: "x"}}); err == nil {
		t.Error("a nested name through a symlink out of the root directory must be refused")
	}
	if _, err := os.Lstat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Error("directories were created at the symlink's target")
	}
}

func TestConfigurationText(t *testing.T) {
	p := runnerBuiltPreview{serviceName: "acme-web-dist", site: site("web-prod", "", "npm run build", "dist", false)}
	if got := configurationText(p.configuration); got != "none — no preview configuration is set" {
		t.Errorf("no configuration: %q", got)
	}
	if got := previewBuildLogLine(p); got != "Preview build: acme-web-dist built by deployment.io from web-prod with none — no preview configuration is set\n" {
		t.Errorf("log line without a configuration: %q", got)
	}
	p.configuration = task_previews.StaticSiteBuildSettingsReplyV1{
		ConfigurationSet: true,
		Variables:        map[string]string{"API_KEY": "s3cret-value", "API_URL": "https://staging.example"},
		Files:            []task_previews.PreviewFileV1{{Name: ".env", Contents: "TOKEN=f1le-value"}},
	}
	if got := configurationText(p.configuration); got != "preview configuration (2 variables, 1 secret files)" {
		t.Errorf("configuration: %q", got)
	}
	if got := previewBuildLogLine(p); got != "Preview build: acme-web-dist built by deployment.io from web-prod with preview configuration (2 variables, 1 secret files)\n" {
		t.Errorf("log line with a configuration: %q", got)
	}
	if env := previewBuildEnv(p.configuration); strings.Join(env, ",") != "API_KEY=s3cret-value,API_URL=https://staging.example" {
		t.Errorf("env: %v", env)
	}
}

// fakeDeploy replaces the cloud deploy with one that records nothing and returns a URL.
func fakeDeploy(t *testing.T) func() {
	t.Helper()
	saved := deployPreviewDir
	deployPreviewDir = func(_ context.Context, deps DeployStaticSitePreviewDeps, serviceName, distDir string, isSPA bool) (deployStaticSitePreviewResult, error) {
		if _, err := os.Stat(filepath.Join(distDir, "index.html")); err != nil {
			return deployStaticSitePreviewResult{}, err
		}
		return deployStaticSitePreviewResult{URL: "https://d1.cloudfront.net", Status: "deployed", DistributionID: "D1"}, nil
	}
	return func() { deployPreviewDir = saved }
}

// syncBuffer is a goroutine-safe log writer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunRunnerBuiltPreview(t *testing.T) {
	restore := fakeDeploy(t)
	defer restore()
	workDir, _ := newWorkDir(t)
	logs := &syncBuffer{}
	var builtIn string
	var builtEnv []string
	deps := DeployStaticSitePreviewDeps{
		WorkDirHost: workDir,
		LogsWriter:  logs,
		BuildSite: func(ctx context.Context, dir, buildCommand string, env []string, w io.Writer) error {
			builtIn, builtEnv = dir, env
			if b, err := os.ReadFile(filepath.Join(dir, ".env")); err != nil || string(b) != "TOKEN=f1le-value" {
				t.Errorf("secret file not written in the root directory: %q, %v", b, err)
			}
			if buildCommand != "npm run build" {
				t.Errorf("build command %q", buildCommand)
			}
			io.WriteString(w, "vite build\n")
			writeTree(t, dir, map[string]string{"dist/index.html": "<html>"})
			return nil
		},
	}
	p := runnerBuiltPreview{
		repository:    "0-acme/web",
		site:          site("web-prod", "apps/web", "npm run build", "dist", true),
		rootDirectory: "apps/web",
		publishDir:    "dist",
		serviceName:   "acme-web-apps-web-dist",
		configuration: task_previews.StaticSiteBuildSettingsReplyV1{
			ConfigurationSet: true,
			Variables:        map[string]string{"API_KEY": "s3cret-value"},
			Files:            []task_previews.PreviewFileV1{{Name: ".env", Contents: "TOKEN=f1le-value"}},
		},
	}
	out, err := runRunnerBuiltPreview(context.Background(), deps, p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(builtIn, workDir+string(filepath.Separator)) || !strings.HasSuffix(builtIn, filepath.Join("apps", "web")) {
		t.Errorf("built in %s; want <copy>/apps/web outside the work dir", builtIn)
	}
	if strings.Join(builtEnv, ",") != "API_KEY=s3cret-value" {
		t.Errorf("build env %v", builtEnv)
	}
	if _, err := os.Stat(builtIn); !os.IsNotExist(err) {
		t.Error("the copy must be removed after the build")
	}
	var res map[string]interface{}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res["built_by"] != "deployment.io" || res["configuration"] != "preview configuration (1 variables, 1 secret files)" || res["url"] != "https://d1.cloudfront.net" {
		t.Errorf("result %s", out)
	}
	for _, value := range []string{"s3cret-value", "f1le-value"} {
		if strings.Contains(out, value) || strings.Contains(logs.String(), value) {
			t.Errorf("a configuration value leaked: result %s, log %q", out, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "Preview build: acme-web-apps-web-dist built by deployment.io from web-prod with preview configuration (1 variables, 1 secret files)\n") {
		t.Errorf("log %q lacks the preview build line", logs.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, "0-acme", "web", "apps", "web", ".env")); !os.IsNotExist(err) {
		t.Error("the secret file must not land in the agent's working tree")
	}

	// Without a configuration, and with a build that produces no index.html.
	p.configuration = task_previews.StaticSiteBuildSettingsReplyV1{}
	deps.BuildSite = func(ctx context.Context, dir, buildCommand string, env []string, w io.Writer) error {
		builtIn = dir
		if len(env) != 0 {
			t.Errorf("env without a configuration: %v", env)
		}
		return nil
	}
	_, err = runRunnerBuiltPreview(context.Background(), deps, p)
	if err == nil || !strings.Contains(err.Error(), "0-acme/web/apps/web/dist/index.html") {
		t.Errorf("no index.html: got %v, want the directory named", err)
	}
	if _, err := os.Stat(builtIn); !os.IsNotExist(err) {
		t.Error("the copy must be removed after a failed build")
	}
}

func TestPreviewBuildsCallAttachFinish(t *testing.T) {
	saved := previewBuildCallWait
	previewBuildCallWait = 50 * time.Millisecond
	defer func() { previewBuildCallWait = saved }()

	builds := newPreviewBuilds()
	defer builds.stop()
	release := make(chan struct{})
	var mu sync.Mutex
	runs := 0
	run := func(ctx context.Context) (string, error) {
		mu.Lock()
		runs++
		n := runs
		mu.Unlock()
		if n == 1 {
			<-release
			return `{"url":"https://d1.cloudfront.net"}`, nil
		}
		return "", errors.New("second build failed")
	}
	building := `{"status":"building","message":"The preview is still building. Call deploy_static_site_preview again with the same arguments to wait for it."}`
	for i := 0; i < 2; i++ {
		out, err := builds.call(context.Background(), "svc", run)
		if err != nil || out != building {
			t.Fatalf("call %d during the build: %q, %v", i, out, err)
		}
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	out, err := builds.call(context.Background(), "svc", run)
	if err != nil || out != `{"url":"https://d1.cloudfront.net"}` {
		t.Fatalf("call after the build: %q, %v", out, err)
	}
	// The result was returned once: the next call starts a new build.
	if _, err := builds.call(context.Background(), "svc", run); err == nil || err.Error() != "second build failed" {
		t.Fatalf("call after the result: %v, want a new build's error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 {
		t.Errorf("runs = %d, want 2 (one attach, one new build)", runs)
	}
}

func TestPreviewBuildsCancelRemovesCopy(t *testing.T) {
	saved := previewBuildCallWait
	previewBuildCallWait = 50 * time.Millisecond
	defer func() { previewBuildCallWait = saved }()

	workDir, _ := newWorkDir(t)
	started := make(chan string, 1)
	deps := DeployStaticSitePreviewDeps{
		WorkDirHost: workDir,
		LogsWriter:  io.Discard,
		BuildSite: func(ctx context.Context, dir, buildCommand string, env []string, w io.Writer) error {
			started <- dir
			<-ctx.Done()
			return ctx.Err()
		},
	}
	p := runnerBuiltPreview{repository: "0-acme/web", site: site("web", "", "npm run build", "dist", false), serviceName: "acme-web-dist"}
	builds := newPreviewBuilds()
	out, err := builds.call(context.Background(), p.serviceName, func(ctx context.Context) (string, error) {
		return runRunnerBuiltPreview(ctx, deps, p)
	})
	if err != nil || !strings.Contains(out, `"status":"building"`) {
		t.Fatalf("got %q, %v; want building", out, err)
	}
	dir := <-started
	builds.stop()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the copy %s must be removed when the agent run ends", dir)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(workDir), "task-preview-build-*"))
	if len(matches) != 0 {
		t.Errorf("copies left behind: %v", matches)
	}
	if _, err := builds.call(context.Background(), p.serviceName, func(context.Context) (string, error) { return "", nil }); err == nil {
		t.Error("a call after the agent run ended must fail")
	}
}

func TestHandlerRunnerBuiltCallAttachesWithoutLookingUpAgain(t *testing.T) {
	saved := previewBuildCallWait
	previewBuildCallWait = 50 * time.Millisecond
	defer func() { previewBuildCallWait = saved }()

	workDir, _ := newWorkDir(t)
	release := make(chan struct{})
	var lookups int
	var mu sync.Mutex
	deps := DeployStaticSitePreviewDeps{
		WorkDirHost:  workDir,
		LogsWriter:   io.Discard,
		Repositories: testRepositories,
		BuildSettings: func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
			mu.Lock()
			lookups++
			mu.Unlock()
			return task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{site("web", "", "npm run build", "dist", false)}}, nil
		},
		BuildSite: func(ctx context.Context, dir, buildCommand string, env []string, w io.Writer) error {
			<-release
			return errors.New("build failed")
		},
	}
	builds := newPreviewBuilds()
	defer builds.stop()
	args := json.RawMessage(`{"repository":"0-acme/web"}`)
	for i := 0; i < 2; i++ {
		out, err := handleDeployStaticSitePreview(context.Background(), deps, builds, args)
		if err != nil || !strings.Contains(out, `"status":"building"`) {
			t.Fatalf("call %d: %q, %v; want building", i, out, err)
		}
	}
	close(release)
	if _, err := handleDeployStaticSitePreview(context.Background(), deps, builds, args); err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("call after the build: %v, want its error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if lookups != 1 {
		t.Errorf("settings looked up %d times, want once: later calls attach to the build", lookups)
	}
}

func TestLookupBuildSettingsGivesUp(t *testing.T) {
	saved := PreviewBuildSettingsTimeout
	PreviewBuildSettingsTimeout = 20 * time.Millisecond
	defer func() { PreviewBuildSettingsTimeout = saved }()
	block := make(chan struct{})
	defer close(block)
	_, err := lookupBuildSettings(func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
		<-block
		return task_previews.StaticSiteBuildSettingsReplyV1{}, nil
	}, "https://github.com/acme/web.git")
	if err == nil || !strings.Contains(err.Error(), "didn't answer in time") {
		t.Errorf("stalled lookup: got %v", err)
	}
}

func TestRunRunnerBuiltPreviewCancelledBeforeDeploy(t *testing.T) {
	workDir, _ := newWorkDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	deployed := false
	saved := deployPreviewDir
	deployPreviewDir = func(context.Context, DeployStaticSitePreviewDeps, string, string, bool) (deployStaticSitePreviewResult, error) {
		deployed = true
		return deployStaticSitePreviewResult{}, nil
	}
	defer func() { deployPreviewDir = saved }()
	deps := DeployStaticSitePreviewDeps{
		WorkDirHost: workDir,
		LogsWriter:  io.Discard,
		BuildSite: func(_ context.Context, dir, _ string, _ []string, _ io.Writer) error {
			if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
				return err
			}
			cancel() // the agent run ends as the build finishes
			return os.WriteFile(filepath.Join(dir, "dist", "index.html"), []byte("<html></html>"), 0o644)
		},
	}
	p := runnerBuiltPreview{repository: "0-acme/web", site: site("web", "", "npm run build", "dist", false), publishDir: "dist", serviceName: "acme-web-dist"}
	if _, err := runRunnerBuiltPreview(ctx, deps, p); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("got %v, want cancelled", err)
	}
	if deployed {
		t.Error("a cancelled build must not deploy")
	}
}

func TestCtxHTTPClientCancelsInFlightRequests(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	}))
	defer srv.Close()
	defer close(unblock)

	ctx, cancel := context.WithCancel(context.Background())
	c := newCtxHTTPClient(ctx, srv.Client())
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("the request must fail once ctx is cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request wasn't cancelled")
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err == nil {
		t.Error("a request after cancellation must be refused")
	}
}

func TestCtxHTTPClientSendsMultipartAbortAfterCancellation(t *testing.T) {
	var aborted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aborted = r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "u1"
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newCtxHTTPClient(ctx, srv.Client())
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/index.html?uploadId=u1", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("the multipart abort must be sent after cancellation: %v", err)
	}
	resp.Body.Close()
	if !aborted {
		t.Error("the server didn't receive the abort")
	}
	del, _ := http.NewRequest(http.MethodDelete, srv.URL+"/index.html", nil)
	if _, err := c.Do(del); err == nil {
		t.Error("any other request after cancellation must be refused")
	}
}

// blockingStore is a PreviewStore whose EnsurePreview waits until release closes.
type blockingStore struct{ release chan struct{} }

func (s blockingStore) EnsurePreview(string) (string, PreviewState, error) {
	<-s.release
	return "p1", PreviewState{}, nil
}
func (blockingStore) SavePreview(string, PreviewState) {}

func TestEnsurePreviewGivesUpWhenCancelled(t *testing.T) {
	store := blockingStore{release: make(chan struct{})}
	defer close(store.release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := ensurePreview(ctx, store, "acme-web")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ensurePreview kept waiting for the RPC after cancellation")
	}
}

func TestHandlerRefusesEscapingRootDirectory(t *testing.T) {
	workDir, _ := newWorkDir(t)
	var lookups int
	deps := DeployStaticSitePreviewDeps{
		WorkDirHost:  workDir,
		LogsWriter:   io.Discard,
		Repositories: testRepositories,
		BuildSettings: func(string) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
			lookups++
			return task_previews.StaticSiteBuildSettingsReplyV1{Sites: []task_previews.StaticSiteBuildSettingsV1{site("web", "", "npm run build", "dist", false)}}, nil
		},
		BuildSite: func(context.Context, string, string, []string, io.Writer) error {
			t.Error("an escaping root_directory must never start a build")
			return nil
		},
	}
	builds := newPreviewBuilds()
	defer builds.stop()
	args := json.RawMessage(`{"repository":"0-acme/web","root_directory":"apps/../../other"}`)
	if out, err := handleDeployStaticSitePreview(context.Background(), deps, builds, args); err == nil || !strings.Contains(err.Error(), "root_directory") {
		t.Fatalf("escaping root_directory: %q, %v; want a refusal", out, err)
	}
	if lookups != 0 {
		t.Errorf("settings looked up %d times, want none", lookups)
	}
}
