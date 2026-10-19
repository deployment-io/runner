package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
)

// --- the QA stage's merge result, with real git ------------------------------

// mergeFixture is a base repository ("origin", branch main) and the Task's
// checkout of it at <workDir>/0-acme/site on a task branch, with a committed
// change, an uncommitted edit and an untracked file. main has moved on since.
type mergeFixture struct {
	workDir, repoDir, origin string
	tmpRoot                  string
	logs                     *lockedBuffer
	fetches                  int
	fetchMu                  sync.Mutex
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newMergeFixture(t *testing.T, base map[string]string) *mergeFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	f := &mergeFixture{
		workDir: filepath.Join(root, "task"),
		origin:  filepath.Join(root, "origin"),
		tmpRoot: filepath.Join(root, "tmp"),
		logs:    &lockedBuffer{},
	}
	f.repoDir = filepath.Join(f.workDir, "0-acme", "site")
	for _, d := range []string{f.workDir, f.tmpRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(f.origin, ".gitignore"), "node_modules/\n")
	writeFile(t, filepath.Join(f.origin, "index.html"), "<h1>home</h1>\n")
	writeFile(t, filepath.Join(f.origin, "about.html"), "line 1\nline 2\nline 3\nline 4\nline 5\n")
	for name, contents := range base {
		writeFile(t, filepath.Join(f.origin, name), contents)
	}
	runGit(t, f.origin, "init", "-q")
	runGit(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, f.origin, "add", "-A")
	runGit(t, f.origin, "commit", "-q", "-m", "the base")
	runGit(t, f.workDir, "clone", "-q", f.origin, "0-acme/site")
	runGit(t, f.repoDir, "checkout", "-q", "-b", "tasks/x")
	// The Task's committed change, its uncommitted edit and an untracked file.
	writeFile(t, filepath.Join(f.repoDir, "about.html"), "line 1 (task)\nline 2\nline 3\nline 4\nline 5\n")
	runGit(t, f.repoDir, "commit", "-q", "-am", "the implement run")
	writeFile(t, filepath.Join(f.repoDir, "index.html"), "<h1>home</h1>\n<p>uncommitted</p>\n")
	writeFile(t, filepath.Join(f.repoDir, "task.html"), "untracked\n")
	writeFile(t, filepath.Join(f.repoDir, "node_modules", "dep", "index.js"), "module.exports = 1\n")
	return f
}

// advanceBase commits files onto origin's main.
func (f *mergeFixture) advanceBase(t *testing.T, files map[string]string) string {
	t.Helper()
	for name, contents := range files {
		writeFile(t, filepath.Join(f.origin, name), contents)
	}
	runGit(t, f.origin, "add", "-A")
	runGit(t, f.origin, "commit", "-q", "-m", "main moves on")
	return strings.TrimSpace(runGit(t, f.origin, "rev-parse", "HEAD"))
}

func (f *mergeFixture) provider(source, secret string) *qaMergeResults {
	return &qaMergeResults{
		workDirHost:  f.workDir,
		repositories: []tasks.RepositoryEntry{{Name: "acme/site", BaseBranch: "main", CloneURL: "https://example.com/acme/site.git"}},
		logsWriter:   f.logs,
		tmpRoot:      f.tmpRoot,
		fetchSource: func(context.Context, tasks.RepositoryEntry) (string, string, error) {
			f.fetchMu.Lock()
			f.fetches++
			f.fetchMu.Unlock()
			return source, secret, nil
		},
	}
}

// buildSource is the directory a QA preview of the repository builds in, as
// runRunnerBuiltPreview gets it.
func (f *mergeFixture) buildSource(t *testing.T, m *qaMergeResults) string {
	t.Helper()
	deps := agenttools.DeployStaticSitePreviewDeps{WorkDirHost: f.workDir, LogsWriter: f.logs}
	if m != nil {
		deps.MergeResult = m
	}
	dir, err := agenttools.PreviewBuildSource(context.Background(), deps, "0-acme/site")
	if err != nil {
		if dir != "" {
			os.RemoveAll(dir)
		}
		t.Fatal(err)
	}
	return dir
}

// assertNoBuildDirsLeft checks nothing is left beside the work dir or in the
// temporary root.
func (f *mergeFixture) assertNoBuildDirsLeft(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.workDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "-preview-build-") {
			t.Errorf("a build directory was left behind: %s", e.Name())
		}
	}
	assertEmptyDir(t, f.tmpRoot)
}

func readTreeFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("%s: %s", name, err)
	}
	return string(b)
}

func TestTheQAPreviewBuildsFromTheMergeResult(t *testing.T) {
	f := newMergeFixture(t, nil)
	baseCommit := f.advanceBase(t, map[string]string{
		"news.html":  "news from main\n",
		"about.html": "line 1\nline 2\nline 3\nline 4\nline 5 (main)\n",
	})
	repoBefore := repositoryState(t, f.repoDir)
	workBefore := repositoryState(t, f.workDir)
	m := f.provider(f.origin, "")

	first := f.buildSource(t, m)
	if filepath.Dir(first) != filepath.Dir(f.workDir) {
		t.Errorf("build directory %s isn't beside the work dir", first)
	}
	if got := readTreeFile(t, first, "news.html"); got != "news from main\n" {
		t.Errorf("news.html = %q, want main's newer commit", got)
	}
	if got := readTreeFile(t, first, "index.html"); got != "<h1>home</h1>\n<p>uncommitted</p>\n" {
		t.Errorf("index.html = %q, want the Task's uncommitted edit", got)
	}
	if got := readTreeFile(t, first, "task.html"); got != "untracked\n" {
		t.Errorf("task.html = %q, want the Task's untracked file", got)
	}
	// Both sides changed about.html: git-merge-one-file merged it.
	if got := readTreeFile(t, first, "about.html"); got != "line 1 (task)\nline 2\nline 3\nline 4\nline 5 (main)\n" {
		t.Errorf("about.html = %q, want both sides' changes", got)
	}
	for _, name := range []string{".git", "node_modules"} {
		if _, err := os.Lstat(filepath.Join(first, name)); !os.IsNotExist(err) {
			t.Errorf("%s is in the build directory", name)
		}
	}

	// A second build gets its own directory, from the same merge: computed
	// once per round.
	writeFile(t, filepath.Join(first, "news.html"), "changed by the first build\n")
	second := f.buildSource(t, m)
	if second == first {
		t.Fatal("two builds shared a build directory")
	}
	if got := readTreeFile(t, second, "news.html"); got != "news from main\n" {
		t.Errorf("the second build saw the first build's change: %q", got)
	}
	if f.fetches != 1 {
		t.Errorf("fetched %d times, want once per round", f.fetches)
	}
	os.RemoveAll(first)
	os.RemoveAll(second)

	want := []qaMergeResultOutput{{Repository: "acme/site", BaseBranch: "main", BaseCommit: baseCommit, Merged: true}}
	m.remove()
	if got := m.results(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("results = %+v, want %+v", got, want)
	}
	assertSameState(t, "after the merge result", repoBefore, repositoryState(t, f.repoDir))
	assertSameState(t, "the work dir after the merge result", workBefore, repositoryState(t, f.workDir))
	f.assertNoBuildDirsLeft(t)

	// After the round, a late build falls back rather than finding nothing.
	late := f.buildSource(t, m)
	defer os.RemoveAll(late)
	if _, err := os.Stat(filepath.Join(late, "news.html")); !os.IsNotExist(err) {
		t.Error("a build after the round ended used the removed merge result")
	}
}

func TestAConflictFallsBackToTheBranchAsBuilt(t *testing.T) {
	f := newMergeFixture(t, map[string]string{"style.css": "body { color: black }\n"})
	f.advanceBase(t, map[string]string{
		"index.html": "<h1>home, says main</h1>\n",
		"style.css":  "body { color: blue }\n",
	})
	writeFile(t, filepath.Join(f.repoDir, "style.css"), "body { color: red }\n")
	repoBefore := repositoryState(t, f.repoDir)
	m := f.provider(f.origin, "")

	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	if got := readTreeFile(t, dir, "index.html"); got != "<h1>home</h1>\n<p>uncommitted</p>\n" {
		t.Errorf("index.html = %q, want the branch as built", got)
	}
	m.remove()
	got := m.results()
	if len(got) != 1 || got[0].Merged || got[0].BaseCommit == "" ||
		got[0].Reason != "the branch doesn't merge cleanly with main (2 conflicting files)" {
		t.Errorf("results = %+v", got)
	}
	assertSameState(t, "after a conflict", repoBefore, repositoryState(t, f.repoDir))
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

func TestAFetchFailureFallsBackWithoutTheToken(t *testing.T) {
	f := newMergeFixture(t, nil)
	const token = "ghs_s3cretInstallationToken"
	source := filepath.Join(t.TempDir(), "oauth2:"+token, "missing.git")
	m := f.provider(source, token)

	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	if _, err := os.Stat(filepath.Join(dir, "task.html")); err != nil {
		t.Errorf("the fallback isn't the branch as built: %s", err)
	}
	// Nothing the fetch left behind carries the token.
	_ = filepath.WalkDir(f.tmpRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
				t.Errorf("%s carries the token", p)
			}
		}
		return nil
	})
	m.remove()
	got := m.results()
	if len(got) != 1 || got[0].Merged || got[0].BaseCommit != "" || got[0].Reason != "main couldn't be fetched" {
		t.Errorf("results = %+v", got)
	}
	if strings.Contains(f.logs.String(), token) || strings.Contains(got[0].Reason, token) {
		t.Errorf("the token leaked into the log:\n%s", f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "QA: previews of 0-acme/site build from the branch as built: main couldn't be fetched (git fetch: ") {
		t.Errorf("the log doesn't say why:\n%s", f.logs.String())
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

func TestScrubSecret(t *testing.T) {
	for _, tc := range []struct {
		in, secret string
		truncated  bool
		want       string
	}{
		{"fatal: https://oauth2:tok@host/x", "tok", false, "fatal: https://oauth2:***@host/x"},
		{"a b%2Fc", "b/c", false, "a ***"},
		{"cut at to", "tok", true, "cut at"},
		{"nothing", "", false, "nothing"},
	} {
		if got := scrubSecret(tc.in, tc.secret, tc.truncated); got != tc.want {
			t.Errorf("scrubSecret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The repository's config and both trees' .gitattributes are the agent's and
// the customer's: no filter, fsmonitor, diff command or merge driver they name
// runs on the host, even through a merge that needs a file merged.
func TestTheMergeResultRunsNothingTheRepositoryNames(t *testing.T) {
	f := newMergeFixture(t, map[string]string{".gitattributes": "* filter=evil merge=evil diff=evil\n"})
	f.advanceBase(t, map[string]string{"about.html": "line 1\nline 2\nline 3\nline 4\nline 5 (main)\n"})
	marker := filepath.Join(t.TempDir(), "ran")
	hook := "touch " + marker + "; cat"
	runGit(t, f.repoDir, "config", "filter.evil.clean", hook)
	runGit(t, f.repoDir, "config", "filter.evil.smudge", hook)
	runGit(t, f.repoDir, "config", "filter.evil.required", "true")
	runGit(t, f.repoDir, "config", "core.fsmonitor", "touch "+marker)
	runGit(t, f.repoDir, "config", "diff.external", "touch "+marker)
	runGit(t, f.repoDir, "config", "diff.evil.textconv", hook)
	runGit(t, f.repoDir, "config", "merge.evil.driver", "touch "+marker+"; true")
	writeFile(t, filepath.Join(f.repoDir, ".git", "info", "attributes"), "* filter=evil merge=evil\n")
	writeFile(t, filepath.Join(f.repoDir, ".gitattributes"), "* filter=evil merge=evil diff=evil\n# the agent's\n")
	_ = os.Remove(marker)
	m := f.provider(f.origin, "")

	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	m.remove()
	if got := m.results(); len(got) != 1 || !got[0].Merged {
		t.Fatalf("results = %+v", got)
	}
	if got := readTreeFile(t, dir, "about.html"); got != "line 1 (task)\nline 2\nline 3\nline 4\nline 5 (main)\n" {
		t.Errorf("about.html = %q, want git's own merge", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("a command the repository named ran on the host")
	}
}

// A symlink in either tree that leaves the build directory is removed before
// the build; one that stays inside is kept.
func TestEscapingSymlinksInTheMergeResultAreRemoved(t *testing.T) {
	f := newMergeFixture(t, nil)
	if err := os.Symlink("../../../../etc", filepath.Join(f.origin, "from-main")); err != nil {
		t.Fatal(err)
	}
	f.advanceBase(t, nil)
	if err := os.Symlink("/etc/passwd", filepath.Join(f.repoDir, "from-task")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.html", filepath.Join(f.repoDir, "inside")); err != nil {
		t.Fatal(err)
	}
	m := f.provider(f.origin, "")
	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	defer m.remove()
	if got := m.results(); len(got) != 1 || !got[0].Merged {
		t.Fatalf("results = %+v", got)
	}
	for _, name := range []string{"from-main", "from-task"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("the escaping symlink %s is in the build directory", name)
		}
	}
	if target, err := os.Readlink(filepath.Join(dir, "inside")); err != nil || target != "index.html" {
		t.Errorf("the symlink inside the tree was not kept: %q, %v", target, err)
	}
}

func TestATimeoutFallsBackWithItsReason(t *testing.T) {
	f := newMergeFixture(t, nil)
	m := f.provider(f.origin, "")
	m.timeout = time.Nanosecond
	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	m.remove()
	if got := m.results(); len(got) != 1 || got[0].Merged || got[0].Reason != "the merge couldn't be made (timed out after 1ns)" {
		t.Errorf("results = %+v", got)
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

func TestAFailingGitFallsBackWithItsReason(t *testing.T) {
	f := newMergeFixture(t, nil)
	m := f.provider(f.origin, "")
	m.git = filepath.Join(t.TempDir(), "no-such-git")
	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	m.remove()
	if got := m.results(); len(got) != 1 || got[0].Merged || !strings.HasPrefix(got[0].Reason, "the merge couldn't be made (git add: ") {
		t.Errorf("results = %+v", got)
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

// With no provider (implement and fix runs) the build source is the branch as
// built, as before.
func TestWithoutAMergeResultTheBuildSourceIsTheRepository(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news\n"})
	dir := f.buildSource(t, nil)
	defer os.RemoveAll(dir)
	if _, err := os.Stat(filepath.Join(dir, "news.html")); !os.IsNotExist(err) {
		t.Error("an implement-run build saw main's newer commit")
	}
	if got := readTreeFile(t, dir, "index.html"); got != "<h1>home</h1>\n<p>uncommitted</p>\n" {
		t.Errorf("index.html = %q", got)
	}
	if f.fetches != 0 {
		t.Error("an implement-run build fetched")
	}
}

func TestRepositoryHead(t *testing.T) {
	gitDir := t.TempDir()
	const id = "0123456789abcdef0123456789abcdef01234567"
	writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/tasks/x\n")
	writeFile(t, filepath.Join(gitDir, "packed-refs"), "# pack-refs with: peeled\n"+id+" refs/heads/tasks/x\n")
	if got, err := repositoryHead(gitDir); err != nil || got != id {
		t.Errorf("packed: %q, %v", got, err)
	}
	writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: ../../etc/passwd\n")
	if _, err := repositoryHead(gitDir); err == nil {
		t.Error("a HEAD naming a path outside refs/ was followed")
	}
	writeFile(t, filepath.Join(gitDir, "HEAD"), id+"\n")
	if got, err := repositoryHead(gitDir); err != nil || got != id {
		t.Errorf("detached: %q, %v", got, err)
	}
}

// --- the round's record ------------------------------------------------------

func TestTheRoundRecordsItsMergeResults(t *testing.T) {
	stage, _ := newQAStageForMergeResults(t)
	results := []qaMergeResultOutput{{Repository: "acme/site", BaseBranch: "main", BaseCommit: strings.Repeat("a", 40), Merged: true}}
	stage.runQA = func(round int) (agentResult, error) {
		stage.roundMergeResults = results
		return agentResult{}, errors.New("no container here")
	}
	round, err := stage.qaRound(1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(round)
	if !strings.Contains(string(encoded), `"merge_results":[{"repository":"acme/site","base_branch":"main","base_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","merged":true}]`) {
		t.Errorf("round = %s", encoded)
	}

	stage.runQA = func(round int) (agentResult, error) { return agentResult{}, errors.New("no container here") }
	round, err = stage.qaRound(2)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(round); strings.Contains(string(encoded), "merge_results") {
		t.Errorf("a round that built no preview carries merge_results: %s", encoded)
	}
}

func newQAStageForMergeResults(t *testing.T) (*qaStage, map[string]interface{}) {
	t.Helper()
	parameters := map[string]interface{}{}
	stage := &qaStage{parameters: parameters, logsWriter: &lockedBuffer{}, out: &qaOutput{}}
	return stage, parameters
}

// A merged tree with a submodule can't carry the submodule's files: such a
// repository builds as built, and says why.
func TestASubmoduleFallsBackToTheBranchAsBuilt(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news from main\n"})
	sub := filepath.Join(f.repoDir, "theme")
	writeFile(t, filepath.Join(sub, "theme.css"), "body {}\n")
	runGit(t, sub, "init", "-q")
	runGit(t, sub, "add", "-A")
	runGit(t, sub, "commit", "-q", "-m", "the theme")
	runGit(t, f.repoDir, "add", "theme")
	runGit(t, f.repoDir, "commit", "-q", "-m", "a submodule")
	m := f.provider(f.origin, "")

	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	if got := readTreeFile(t, dir, "theme/theme.css"); got != "body {}\n" {
		t.Errorf("theme/theme.css = %q, want the branch as built", got)
	}
	m.remove()
	if got := m.results(); len(got) != 1 || got[0].Merged || got[0].Reason != "the merge couldn't be made (the repository has submodules)" {
		t.Errorf("results = %+v", got)
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

// A temporary directory that can't be made is still a recorded fallback.
func TestATemporaryDirectoryFailureIsRecorded(t *testing.T) {
	f := newMergeFixture(t, nil)
	m := f.provider(f.origin, "")
	m.tmpRoot = filepath.Join(f.tmpRoot, "missing", "dir")
	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	m.remove()
	got := m.results()
	if len(got) != 1 || got[0].Merged || !strings.HasPrefix(got[0].Reason, "the merge couldn't be made (could not create a temporary directory: ") {
		t.Errorf("results = %+v", got)
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

// A token lookup that never returns doesn't hold the merge past its timeout.
func TestAStalledTokenLookupFallsBackWithinTheTimeout(t *testing.T) {
	f := newMergeFixture(t, nil)
	m := f.provider(f.origin, "")
	release := make(chan struct{})
	defer close(release)
	m.fetchSource = func(context.Context, tasks.RepositoryEntry) (string, string, error) {
		<-release
		return "", "", errors.New("released")
	}
	m.timeout = 500 * time.Millisecond
	start := time.Now()
	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the build waited %s for the token lookup", elapsed)
	}
	m.remove()
	if got := m.results(); len(got) != 1 || got[0].Merged || got[0].Reason != "the merge couldn't be made (timed out after 500ms)" {
		t.Errorf("results = %+v", got)
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

// A git whose helper outlives it, holding its output open, is killed with the
// helper at the deadline instead of waiting for the helper.
func TestATimedOutGitIsKilledWithItsHelpers(t *testing.T) {
	f := newMergeFixture(t, nil)
	script := filepath.Join(t.TempDir(), "git")
	writeFile(t, script, "#!/bin/sh\nsleep 60 &\nwait\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	trees := &fixTrees{workDirHost: f.workDir, logsWriter: f.logs, git: script, ctx: ctx}
	start := time.Now()
	_, err := trees.runIsolated("0-acme/site", t.TempDir(), isolatedGit{}, "fetch")
	if err == nil || err.Error() != "git fetch timed out" {
		t.Errorf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("runIsolated took %s: it waited for the helper", elapsed)
	}
}

// A build's checkout of the merge result shares the merge's deadline: when the
// snapshot, fetch and merge have used most of it, a stalled checkout falls
// back once it is spent, not after a timeout of its own.
func TestTheCheckoutSharesTheMergesTimeout(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"newer.txt": "from main\n"})
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	marker := filepath.Join(t.TempDir(), "checkout-started")
	script := filepath.Join(t.TempDir(), "git")
	writeFile(t, script, "#!/bin/sh\n"+
		"case \"$GIT_INDEX_FILE\" in *build-index-*)\n"+
		"  for a in \"$@\"; do if [ \"$a\" = checkout-index ]; then : > '"+marker+"'; exec sleep 60; fi; done;;\n"+
		"esac\n"+
		"exec '"+realGit+"' \"$@\"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	m := f.provider(f.origin, "")
	m.git = script
	lookup := m.fetchSource
	m.fetchSource = func(ctx context.Context, entry tasks.RepositoryEntry) (string, string, error) {
		time.Sleep(2 * time.Second)
		return lookup(ctx, entry)
	}
	m.timeout = 3 * time.Second
	start := time.Now()
	dir := f.buildSource(t, m)
	defer os.RemoveAll(dir)
	elapsed := time.Since(start)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the build's checkout never ran: %v", err)
	}
	// With a timeout of its own the checkout would hold the build until 5s.
	if elapsed > 4500*time.Millisecond {
		t.Errorf("the build waited %s for its checkout", elapsed)
	}
	m.remove()
	if got := m.results(); len(got) != 1 || got[0].Merged || got[0].Reason != "the merge couldn't be made (timed out after 3s)" {
		t.Errorf("results = %+v", got)
	}
	os.RemoveAll(dir)
	f.assertNoBuildDirsLeft(t)
}

// Once a checkout of the merge result has failed, the round's later builds of
// the repository build as built too, so the record matches every preview
// built after it.
func TestAFailedCheckoutKeepsLaterBuildsAsBuilt(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news from main\n"})
	m := f.provider(f.origin, "")
	defer m.remove()
	// A regular file where the build directory should be: the checkout fails.
	notADir := filepath.Join(t.TempDir(), "file")
	writeFile(t, notADir, "x")
	if m.CheckoutMergeResult(context.Background(), "0-acme/site", notADir) {
		t.Fatal("a checkout into a file succeeded")
	}
	if m.CheckoutMergeResult(context.Background(), "0-acme/site", t.TempDir()) {
		t.Error("a later build used the merge result after a failed checkout")
	}
	results := m.results()
	if len(results) != 1 || results[0].Merged || results[0].Reason == "" {
		t.Errorf("results = %+v, want one fallback with a reason", results)
	}
}

// --- concurrent builds -------------------------------------------------------

// buildSourceIn is buildSource for a goroutine: the error is returned, not
// fatal, and ctx is the build's own.
func (f *mergeFixture) buildSourceIn(ctx context.Context, m *qaMergeResults, repoDir string) (string, error) {
	deps := agenttools.DeployStaticSitePreviewDeps{WorkDirHost: f.workDir, LogsWriter: f.logs, MergeResult: m}
	return agenttools.PreviewBuildSource(ctx, deps, repoDir)
}

// fetchCount is how many lookups the fetch source has started.
func (f *mergeFixture) fetchCount() int {
	f.fetchMu.Lock()
	defer f.fetchMu.Unlock()
	return f.fetches
}

// gatedFetch is a fetch source that blocks every lookup until released, so
// the test decides when the merges may proceed.
func (f *mergeFixture) gatedFetch(m *qaMergeResults, source string, release <-chan struct{}) {
	m.fetchSource = func(ctx context.Context, _ tasks.RepositoryEntry) (string, string, error) {
		f.fetchMu.Lock()
		f.fetches++
		f.fetchMu.Unlock()
		select {
		case <-release:
			return source, "", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
}

// Builds of one repository started together share one merge: it is made
// once, the others wait for it, and each gets its own directory of it.
func TestConcurrentBuildsOfARepositoryShareOneMerge(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news from main\n"})
	m := f.provider(f.origin, "")
	release := make(chan struct{})
	f.gatedFetch(m, f.origin, release)

	const builds = 4
	dirs := make([]string, builds)
	errs := make([]error, builds)
	var wg sync.WaitGroup
	for i := 0; i < builds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dirs[i], errs[i] = f.buildSourceIn(context.Background(), m, "0-acme/site")
		}(i)
	}
	// Let every build reach the merge before it can finish.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < builds; i++ {
		if errs[i] != nil {
			t.Fatalf("build %d: %s", i, errs[i])
		}
		defer os.RemoveAll(dirs[i])
		if seen[dirs[i]] {
			t.Errorf("two builds shared the directory %s", dirs[i])
		}
		seen[dirs[i]] = true
		if got := readTreeFile(t, dirs[i], "news.html"); got != "news from main\n" {
			t.Errorf("build %d: news.html = %q, want the merge result", i, got)
		}
	}
	if n := f.fetchCount(); n != 1 {
		t.Errorf("fetched %d times, want once for the round", n)
	}
	m.remove()
	if got := m.results(); len(got) != 1 || !got[0].Merged {
		t.Errorf("results = %+v, want one merged entry", got)
	}
}

// Two repositories merge independently: neither waits for the other's merge.
func TestTwoRepositoriesMergeConcurrently(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news from main\n"})
	other := filepath.Join(f.workDir, "1-acme", "other")
	runGit(t, f.workDir, "clone", "-q", f.origin, "1-acme/other")
	runGit(t, other, "checkout", "-q", "-b", "tasks/x")
	writeFile(t, filepath.Join(other, "other.html"), "the other repository\n")
	m := f.provider(f.origin, "")
	m.repositories = append(m.repositories, tasks.RepositoryEntry{Name: "acme/other", BaseBranch: "main", CloneURL: "https://example.com/acme/other.git"})
	// Each lookup waits until both are in flight: merges that ran one at a
	// time would never both arrive, and would fall back at the timeout.
	var arrived sync.WaitGroup
	arrived.Add(2)
	m.fetchSource = func(ctx context.Context, _ tasks.RepositoryEntry) (string, string, error) {
		arrived.Done()
		both := make(chan struct{})
		go func() { arrived.Wait(); close(both) }()
		select {
		case <-both:
			return f.origin, "", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	m.timeout = 20 * time.Second

	repos := []string{"0-acme/site", "1-acme/other"}
	dirs := make([]string, len(repos))
	errs := make([]error, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			dirs[i], errs[i] = f.buildSourceIn(context.Background(), m, repo)
		}(i, repo)
	}
	wg.Wait()
	for i := range repos {
		if errs[i] != nil {
			t.Fatalf("%s: %s", repos[i], errs[i])
		}
		defer os.RemoveAll(dirs[i])
		if got := readTreeFile(t, dirs[i], "news.html"); got != "news from main\n" {
			t.Errorf("%s: news.html = %q, want the merge result", repos[i], got)
		}
	}
	if got := readTreeFile(t, dirs[1], "other.html"); got != "the other repository\n" {
		t.Errorf("other.html = %q, want the other repository's change", got)
	}
	m.remove()
	got := m.results()
	if len(got) != 2 || got[0].Repository != "acme/site" || got[1].Repository != "acme/other" || !got[0].Merged || !got[1].Merged {
		t.Errorf("results = %+v, want both merged, in work-dir order", got)
	}
}

// The round ending while a build is still merging waits for that build, then
// removes everything; a build after it falls back.
func TestTheRoundEndWaitsForABuildInProgress(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news from main\n"})
	m := f.provider(f.origin, "")
	release := make(chan struct{})
	f.gatedFetch(m, f.origin, release)

	type build struct {
		dir string
		err error
	}
	built := make(chan build, 1)
	go func() {
		dir, err := f.buildSourceIn(context.Background(), m, "0-acme/site")
		built <- build{dir, err}
	}()
	// Wait for the build to be inside the merge.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f.fetchCount() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the build never reached the fetch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	removed := make(chan struct{})
	go func() {
		m.remove()
		close(removed)
	}()
	select {
	case <-removed:
		t.Fatal("the round ended under a build in progress")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	b := <-built
	if b.err != nil {
		t.Fatal(b.err)
	}
	defer os.RemoveAll(b.dir)
	if got := readTreeFile(t, b.dir, "news.html"); got != "news from main\n" {
		t.Errorf("news.html = %q, want the merge result", got)
	}
	select {
	case <-removed:
	case <-time.After(10 * time.Second):
		t.Fatal("the round's end never finished")
	}
	if got := m.results(); len(got) != 1 || !got[0].Merged {
		t.Errorf("results = %+v, want one merged entry", got)
	}
	os.RemoveAll(b.dir)
	f.assertNoBuildDirsLeft(t)

	late, err := f.buildSourceIn(context.Background(), m, "0-acme/site")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(late)
	if _, err := os.Stat(filepath.Join(late, "news.html")); !os.IsNotExist(err) {
		t.Error("a build after the round ended used the merge result")
	}
}

// A first build cancelled mid-merge leaves the repository as built for the
// round, recorded with a reason: the merge isn't retried by a later build.
func TestACancelledFirstBuildKeepsTheRoundAsBuilt(t *testing.T) {
	f := newMergeFixture(t, nil)
	f.advanceBase(t, map[string]string{"news.html": "news from main\n"})
	m := f.provider(f.origin, "")
	never := make(chan struct{})
	f.gatedFetch(m, f.origin, never)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	// The cancelled build's own copy step may refuse; only the record matters.
	if dir, _ := f.buildSourceIn(ctx, m, "0-acme/site"); dir != "" {
		os.RemoveAll(dir)
	}
	later, err := f.buildSourceIn(context.Background(), m, "0-acme/site")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(later)
	if _, err := os.Stat(filepath.Join(later, "news.html")); !os.IsNotExist(err) {
		t.Error("a later build used a merge result after the first was cancelled")
	}
	if n := f.fetchCount(); n != 1 {
		t.Errorf("fetched %d times, want once for the round", n)
	}
	m.remove()
	if got := m.results(); len(got) != 1 || got[0].Merged || got[0].Reason == "" {
		t.Errorf("results = %+v, want one fallback with a reason", got)
	}
}
