package agenttools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMergeResult is a MergeResultProvider that writes files into the build
// directory, or reports that the merge result couldn't be made.
type fakeMergeResult struct {
	ok    bool
	files map[string]string
	links map[string]string
	calls []string
	dirs  []string
}

func (f *fakeMergeResult) CheckoutMergeResult(_ context.Context, repoDir, dir string) bool {
	f.calls = append(f.calls, repoDir)
	f.dirs = append(f.dirs, dir)
	if !f.ok {
		return false
	}
	for name, contents := range f.files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(contents), 0o644)
	}
	for name, target := range f.links {
		_ = os.Symlink(target, filepath.Join(dir, name))
	}
	return true
}

func mergeResultPreview() runnerBuiltPreview {
	return runnerBuiltPreview{
		repository:    "0-acme/web",
		site:          site("web-prod", "apps/web", "npm run build", "dist", false),
		rootDirectory: "apps/web",
		publishDir:    "dist",
		serviceName:   "acme-web-apps-web-dist",
	}
}

// buildCapturing is a BuildSite recording the files of the directory it builds
// in (relative to the build directory) and producing an index.html.
func buildCapturing(t *testing.T, builtIn *string, seen map[string]bool) func(context.Context, string, string, []string, io.Writer) error {
	return func(_ context.Context, dir, _ string, _ []string, _ io.Writer) error {
		*builtIn = dir
		top := filepath.Dir(filepath.Dir(dir)) // <build dir>/apps/web
		_ = filepath.Walk(top, func(p string, info os.FileInfo, err error) error {
			if err == nil && p != top {
				rel, _ := filepath.Rel(top, p)
				seen[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		writeTree(t, dir, map[string]string{"dist/index.html": "<html>"})
		return nil
	}
}

// With a provider that made the merge result, the build runs in its build
// directory — beside the work dir, without .git, node_modules or a symlink
// that leaves it — and the directory is removed after the build.
func TestRunnerBuiltPreviewBuildsFromTheMergeResult(t *testing.T) {
	restore := fakeDeploy(t)
	defer restore()
	workDir, _ := newWorkDir(t)
	provider := &fakeMergeResult{ok: true, files: map[string]string{
		"apps/web/package.json":          "{}",
		"apps/web/merged.txt":            "from the merge",
		"apps/web/node_modules/x/a.js":   "x",
		"apps/web/.git/HEAD":             "ref",
		"apps/web/sub/node_modules/b.js": "x",
	}, links: map[string]string{
		"apps/web/escape": "../../../../etc",
		"apps/web/abs":    "/etc/passwd",
		"apps/web/inside": "merged.txt",
	}}
	var builtIn string
	seen := map[string]bool{}
	deps := DeployStaticSitePreviewDeps{WorkDirHost: workDir, LogsWriter: io.Discard, MergeResult: provider, BuildSite: buildCapturing(t, &builtIn, seen)}
	if _, err := runRunnerBuiltPreview(context.Background(), deps, mergeResultPreview()); err != nil {
		t.Fatal(err)
	}
	if len(provider.calls) != 1 || provider.calls[0] != "0-acme/web" {
		t.Fatalf("provider calls = %v", provider.calls)
	}
	buildDir := provider.dirs[0]
	if filepath.Dir(buildDir) != filepath.Dir(workDir) || !strings.HasPrefix(filepath.Base(buildDir), "task-preview-build-") {
		t.Errorf("build directory %s; want a new directory beside the work dir", buildDir)
	}
	if builtIn != filepath.Join(buildDir, "apps", "web") {
		t.Errorf("built in %s; want the merge result's apps/web", builtIn)
	}
	for _, want := range []string{"apps/web/merged.txt", "apps/web/inside", "apps/web/package.json"} {
		if !seen[want] {
			t.Errorf("the build didn't see %s: %v", want, seen)
		}
	}
	for _, unwanted := range []string{"apps/web/node_modules", "apps/web/.git", "apps/web/sub/node_modules", "apps/web/escape", "apps/web/abs", "apps/web/src", "src/index.js"} {
		if seen[unwanted] {
			t.Errorf("the build saw %s", unwanted)
		}
	}
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Error("the merge result's build directory must be removed after the build")
	}
}

// A provider that couldn't make the merge result leaves the build to the copy
// of the repository, as today, and no build directory of its own behind.
func TestRunnerBuiltPreviewFallsBackToTheCopy(t *testing.T) {
	restore := fakeDeploy(t)
	defer restore()
	workDir, _ := newWorkDir(t)
	provider := &fakeMergeResult{ok: false}
	var builtIn string
	seen := map[string]bool{}
	deps := DeployStaticSitePreviewDeps{WorkDirHost: workDir, LogsWriter: io.Discard, MergeResult: provider, BuildSite: buildCapturing(t, &builtIn, seen)}
	if _, err := runRunnerBuiltPreview(context.Background(), deps, mergeResultPreview()); err != nil {
		t.Fatal(err)
	}
	if len(provider.dirs) != 1 {
		t.Fatalf("provider calls = %v", provider.calls)
	}
	if _, err := os.Stat(provider.dirs[0]); !os.IsNotExist(err) {
		t.Error("the unused build directory was left behind")
	}
	if !seen["apps/web/public/robots.txt"] {
		t.Errorf("the build didn't run on the copy of the repository: %v", seen)
	}
	if builtIn == "" || filepath.Dir(filepath.Dir(filepath.Dir(builtIn))) == provider.dirs[0] {
		t.Errorf("built in %s", builtIn)
	}
}

// Without a provider (implement and fix runs) the build source is the copy of
// the repository.
func TestPreviewBuildSourceWithoutAProviderIsTheCopy(t *testing.T) {
	workDir, _ := newWorkDir(t)
	dir, err := PreviewBuildSource(context.Background(), DeployStaticSitePreviewDeps{WorkDirHost: workDir, LogsWriter: io.Discard}, "0-acme/web")
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "apps", "web", "public", "robots.txt")); err != nil || string(b) != "ok" {
		t.Errorf("the build source isn't a copy of the repository: %q, %v", b, err)
	}
}
