package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- the fix run's own diff, with real git -----------------------------------

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(withoutGitEnv(os.Environ()),
		"GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=agent@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s\n%s", args, err, out)
	}
	return string(out)
}

// initFixTreesRepository is a repository with one commit, a node_modules/
// ignore rule, an ignored file, an untracked file and an uncommitted edit —
// the shapes a working tree is in when a fix run starts.
func initFixTreesRepository(t *testing.T, repoDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	writeFile(t, filepath.Join(repoDir, ".gitignore"), "node_modules/\n")
	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api\n\nfunc Handle() {}\n")
	writeFile(t, filepath.Join(repoDir, "doomed.go"), "package api\n\n// deleted by the fix\n")
	runGit(t, repoDir, "init", "-q")
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-q", "-m", "the implement run")
	writeFile(t, filepath.Join(repoDir, "node_modules", "dep", "index.js"), "module.exports = 1\n")
	writeFile(t, filepath.Join(repoDir, "notes.txt"), "untracked before the fix\n")
	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api\n\nfunc Handle() { /* uncommitted */ }\n")
}

// repositoryState is every path under a repository, .git included, with its
// mode and the hash of its contents (or link target).
func repositoryState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		entry := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			entry += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry += " " + hex.EncodeToString(sum[:])
		}
		state[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertSameState(t *testing.T, when string, want, got map[string]string) {
	t.Helper()
	for path, entry := range want {
		if got[path] != entry {
			t.Errorf("%s: %s changed: %q -> %q", when, path, entry, got[path])
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("%s: %s appeared in the repository", when, path)
		}
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the temporary directory was left behind: %v", entries)
	}
}

func newTestFixTrees(t *testing.T, workDir string, logs io.Writer, dirs ...string) (*fixTrees, string) {
	t.Helper()
	tmpRoot := t.TempDir()
	trees := &fixTrees{workDirHost: workDir, logsWriter: logs, dirs: dirs}
	trees.takeBefore(tmpRoot)
	return trees, tmpRoot
}

// The snapshots and the diff must not write anything into the repository —
// not its working tree, not its index, not its objects — and must leave no
// temporary directory behind.
func TestFixTreesWriteNothingIntoTheRepository(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)

	start := repositoryState(t, repoDir)
	trees, tmpRoot := newTestFixTrees(t, workDir, io.Discard, "0-acme/api")
	assertSameState(t, "after the before snapshot", start, repositoryState(t, repoDir))

	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api\n\nfunc Handle() { /* fixed */ }\n")
	writeFile(t, filepath.Join(repoDir, "session.go"), "package api\n")
	fixed := repositoryState(t, repoDir)
	diffs := trees.fixDiffs()
	assertSameState(t, "after the after snapshot and the diff", fixed, repositoryState(t, repoDir))
	if diffs["0-acme/api"] == "" {
		t.Error("the fix produced no diff")
	}

	trees.remove()
	assertEmptyDir(t, tmpRoot)
}

func TestTheFixDiffCarriesAnEditAnAdditionAndADeletion(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)
	trees, _ := newTestFixTrees(t, workDir, io.Discard, "0-acme/api")
	defer trees.remove()

	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api\n\nfunc Handle() { checkSession() }\n")
	writeFile(t, filepath.Join(repoDir, "session.go"), "package api\n\nfunc checkSession() {}\n")
	if err := os.Remove(filepath.Join(repoDir, "doomed.go")); err != nil {
		t.Fatal(err)
	}

	diff := trees.fixDiffs()["0-acme/api"]
	for _, want := range []string{
		"diff --git a/handler.go b/handler.go",
		"-func Handle() { /* uncommitted */ }",
		"+func Handle() { checkSession() }",
		"diff --git a/session.go b/session.go",
		"new file mode",
		"+func checkSession() {}",
		"diff --git a/doomed.go b/doomed.go",
		"deleted file mode",
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("the fix diff does not carry %q:\n%s", want, diff)
		}
	}
	// Only what the FIX changed: the untracked file and the uncommitted edit
	// that predate it are not in the fix diff as additions of their own.
	if strings.Contains(diff, "notes.txt") {
		t.Errorf("the fix diff carries a file the fix did not touch:\n%s", diff)
	}
}

func TestAFixThatOnlyRewritesIgnoredFilesHasNoFixDiff(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)
	var logs strings.Builder
	trees, _ := newTestFixTrees(t, workDir, &logs, "0-acme/api")
	defer trees.remove()

	writeFile(t, filepath.Join(repoDir, "node_modules", "dep", "index.js"), "module.exports = 2\n")
	writeFile(t, filepath.Join(repoDir, "node_modules", ".vite", "results.json"), "{}\n")

	if diffs := trees.fixDiffs(); len(diffs) != 0 {
		t.Errorf("fix diffs = %v, want none for a change git ignores", diffs)
	}
	if logs.Len() != 0 {
		t.Errorf("an unchanged repository logged:\n%s", logs.String())
	}
}

func TestARepositoryWithNoCommitsGetsAFixDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "1-acme/new")
	writeFile(t, filepath.Join(repoDir, "README.md"), "# new\n")
	runGit(t, repoDir, "init", "-q")
	if _, err := os.Stat(filepath.Join(repoDir, ".git", "index")); !os.IsNotExist(err) {
		t.Fatalf("a fresh repository already has an index: %v", err)
	}
	start := repositoryState(t, repoDir)
	trees, tmpRoot := newTestFixTrees(t, workDir, io.Discard, "1-acme/new")
	writeFile(t, filepath.Join(repoDir, "main.go"), "package main\n")
	fixed := repositoryState(t, repoDir)

	diff := trees.fixDiffs()["1-acme/new"]
	if !strings.Contains(diff, "diff --git a/main.go b/main.go") || strings.Contains(diff, "README.md") {
		t.Errorf("fix diff = %q, want exactly the new main.go", diff)
	}
	if _, ok := fixed[".git/index"]; ok {
		t.Fatal("the test wrote an index itself")
	}
	assertSameState(t, "after the snapshots", fixed, repositoryState(t, repoDir))
	if _, ok := start[".git/index"]; ok {
		t.Error("the snapshot wrote the repository's index")
	}
	trees.remove()
	assertEmptyDir(t, tmpRoot)
}

// The repository is the agent's to write, config included. Nothing it names
// there — a clean filter, an fsmonitor hook, an external diff — may run on the
// runner's host.
func TestTheRepositorysOwnGitConfigRunsNothing(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := "touch " + marker + "; cat"
	runGit(t, repoDir, "config", "filter.evil.clean", hook)
	runGit(t, repoDir, "config", "core.fsmonitor", "touch "+marker)
	runGit(t, repoDir, "config", "diff.external", "touch "+marker)
	writeFile(t, filepath.Join(repoDir, ".git", "info", "attributes"), "* filter=evil\n")
	writeFile(t, filepath.Join(repoDir, ".gitattributes"), "* filter=evil diff=evil\n")
	runGit(t, repoDir, "config", "diff.evil.textconv", "touch "+marker+"; cat")
	_ = os.Remove(marker)

	trees, _ := newTestFixTrees(t, workDir, io.Discard, "0-acme/api")
	defer trees.remove()
	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api // fixed\n")
	if diffs := trees.fixDiffs(); diffs["0-acme/api"] == "" {
		t.Error("no fix diff")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("a command from the repository's git config ran on the host")
	}
}

func TestAFailingGitLeavesNoFixDiff(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)
	var logs strings.Builder
	trees := &fixTrees{workDirHost: workDir, logsWriter: &logs, dirs: []string{"0-acme/api"}, git: filepath.Join(t.TempDir(), "no-such-git")}
	tmpRoot := t.TempDir()
	trees.takeBefore(tmpRoot)
	writeFile(t, filepath.Join(repoDir, "handler.go"), "package api // fixed\n")

	if diffs := trees.fixDiffs(); len(diffs) != 0 {
		t.Errorf("fix diffs = %v, want none when git cannot run", diffs)
	}
	if !strings.Contains(logs.String(), "Review stage: no fix diff for 0-acme/api (") ||
		!strings.Contains(logs.String(), ") — the next round reviews the whole change") {
		t.Errorf("the job log does not say why there is no fix diff:\n%s", logs.String())
	}
	trees.remove()
	assertEmptyDir(t, tmpRoot)
}

func TestAnOversizedFixDiffIsDropped(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)
	var logs strings.Builder
	trees, _ := newTestFixTrees(t, workDir, &logs, "0-acme/api")
	defer trees.remove()

	writeFile(t, filepath.Join(repoDir, "big.txt"), strings.Repeat("a line of generated output\n", (maxFixDiffBytes/27)+100))

	if diffs := trees.fixDiffs(); len(diffs) != 0 {
		t.Errorf("an over-1-MiB fix diff was kept (%d bytes)", len(diffs["0-acme/api"]))
	}
	if !strings.Contains(logs.String(), "Review stage: no fix diff for 0-acme/api (the diff is larger than 1048576 bytes) — the next round reviews the whole change") {
		t.Errorf("the job log does not say why there is no fix diff:\n%s", logs.String())
	}
}

// --- the loop ------------------------------------------------------------------

// fixDiffLoopStage is a stage over one real repository: every round sends the
// same finding back, every fix run edits handler.go, and every round's
// environment is captured as the container would have received it.
func fixDiffLoopStage(t *testing.T, logs io.Writer) (*reviewStage, string, map[int]map[string]string) {
	t.Helper()
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	initFixTreesRepository(t, repoDir)
	stage := fixLoopStage(workDir, logs)
	stage.parameters = implementerJobParameters(t)
	stage.fixTreesTmpRoot = t.TempDir()
	envs := map[int]map[string]string{}
	loopReview := stage.runReview
	stage.runReview = func(round int) (agentResult, error) {
		env, err := stage.reviewSpawnEnv(round)
		if err != nil {
			t.Fatalf("reviewSpawnEnv: %s", err)
		}
		envs[round] = envMap(env)
		// What the container would have read, while the round runs.
		var paths map[string]string
		if raw, ok := envs[round]["REVIEW_FIX_DIFFS"]; ok {
			if err := json.Unmarshal([]byte(raw), &paths); err != nil {
				t.Fatalf("REVIEW_FIX_DIFFS = %q: %s", raw, err)
			}
			for dir, path := range paths {
				host := filepath.Join(workDir, strings.TrimPrefix(path, agentboxWorkDirInContainer+"/"))
				envs[round]["file:"+dir] = readFile(t, host)
			}
		}
		return loopReview(round)
	}
	fixes := 0
	stage.runFix = func([]reviewFindingOutput) error {
		fixes++
		writeFile(t, filepath.Join(repoDir, "handler.go"), "package api\n\nfunc Handle() { fix() }\n"+strings.Repeat("// again\n", fixes))
		return nil
	}
	return stage, workDir, envs
}

func TestTheRoundAfterAKeptFixGetsTheFixDiff(t *testing.T) {
	var logs strings.Builder
	stage, workDir, envs := fixDiffLoopStage(t, &logs)
	stage.shadowEffort = "high"
	var shadowEnv map[string]string
	stage.runShadow = func(env []string) (agentResult, error) {
		shadowEnv = envMap(env)
		return shadowReport(), nil
	}

	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}

	if _, ok := envs[1]["REVIEW_FIX_DIFFS"]; ok {
		t.Error("round 1 was sent REVIEW_FIX_DIFFS")
	}
	if _, ok := shadowEnv["REVIEW_FIX_DIFFS"]; ok || shadowEnv == nil {
		t.Errorf("the shadow review's env = %v, want it run without REVIEW_FIX_DIFFS", shadowEnv)
	}
	for _, round := range []int{2, 3} {
		raw := envs[round]["REVIEW_FIX_DIFFS"]
		if raw != `{"0-acme/api":"/work/.review-fix/0-acme__api.diff"}` {
			t.Errorf("round %d REVIEW_FIX_DIFFS = %q", round, raw)
		}
	}
	if d := envs[2]["file:0-acme/api"]; !strings.Contains(d, "+func Handle() { fix() }") {
		t.Errorf("round 2's fix diff file does not carry the first fix:\n%s", d)
	}
	// Round 3's diff is only the second fix's: one more "// again" line.
	if d := envs[3]["file:0-acme/api"]; !strings.Contains(d, "+// again") || strings.Contains(d, "+func Handle()") {
		t.Errorf("round 3's fix diff is not exactly the second fix:\n%s", d)
	}
	if !strings.Contains(logs.String(), "Review round 2: fix diff for 1 repository (") {
		t.Errorf("the job log does not report round 2's fix diff:\n%s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, reviewFixDiffDirName)); !os.IsNotExist(err) {
		t.Errorf("the .review-fix directory outlived its round: %v", err)
	}
	if stage.pendingFixDiffs != nil || stage.roundFixDiffs != "" {
		t.Error("fix diffs were left pending after the loop")
	}
	assertEmptyDir(t, stage.fixTreesTmpRoot)
}

func TestTheFixDiffDirectoryIsRemovedWhenTheRoundFails(t *testing.T) {
	stage, workDir, _ := fixDiffLoopStage(t, nil)
	sawDir := false
	captured := stage.runReview
	stage.runReview = func(round int) (agentResult, error) {
		if round == 1 {
			return captured(round)
		}
		_, err := os.Stat(filepath.Join(workDir, reviewFixDiffDirName, "0-acme__api.diff"))
		sawDir = err == nil
		return agentResult{}, errors.New("the container crashed")
	}

	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
	if !sawDir {
		t.Error("round 2 ran without its fix diff on disk")
	}
	if _, err := os.Stat(filepath.Join(workDir, reviewFixDiffDirName)); !os.IsNotExist(err) {
		t.Errorf("the .review-fix directory outlived a failed round: %v", err)
	}
}

// A fix that is not kept leaves nothing pending — and its temporary directory
// is gone either way.
func TestAFixThatIsNotKeptLeavesNoFixDiff(t *testing.T) {
	for _, tc := range []struct {
		name string
		fix  func(repoDir string) error
	}{
		{"it failed and was rolled back", func(repoDir string) error {
			if err := os.WriteFile(filepath.Join(repoDir, "handler.go"), []byte("half a fix\n"), 0o644); err != nil {
				return err
			}
			return errors.New("max turns reached")
		}},
		{"it changed nothing", func(string) error { return nil }},
		{"it changed only ignored files", func(repoDir string) error {
			return os.WriteFile(filepath.Join(repoDir, "node_modules", "dep", "index.js"), []byte("x\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, workDir, _ := fixDiffLoopStage(t, nil)
			stage.runFix = func([]reviewFindingOutput) error {
				return tc.fix(filepath.Join(workDir, "0-acme/api"))
			}
			ran, err := stage.attemptFix(1, nil)
			if err != nil {
				t.Fatalf("attemptFix: %s", err)
			}
			if ran {
				t.Fatal("the fix was kept")
			}
			if stage.pendingFixDiffs != nil {
				t.Errorf("pending fix diffs = %v after a fix that was not kept", stage.pendingFixDiffs)
			}
			assertEmptyDir(t, stage.fixTreesTmpRoot)
			// And nothing reaches a later round.
			stage.stageFixDiffs(2)
			if stage.roundFixDiffs != "" {
				t.Errorf("a later round got REVIEW_FIX_DIFFS = %q", stage.roundFixDiffs)
			}
		})
	}
}

// Pending diffs reach only the round directly after the fix that made them.
func TestPendingFixDiffsAreNeverReused(t *testing.T) {
	stage := fixLoopStage(t.TempDir(), nil)
	stage.pendingFixDiffs = map[string]string{"0-acme/api": "diff --git a/x b/x\n"}
	stage.stageFixDiffs(2)
	if stage.roundFixDiffs == "" {
		t.Fatal("round 2 got no fix diffs")
	}
	stage.clearFixDiffs()
	stage.stageFixDiffs(3)
	if stage.roundFixDiffs != "" {
		t.Errorf("round 3 reused round 2's fix diffs: %q", stage.roundFixDiffs)
	}

	stage.pendingFixDiffs = map[string]string{"0-acme/api": "diff --git a/x b/x\n"}
	stage.stageFixDiffs(1)
	if stage.roundFixDiffs != "" || stage.pendingFixDiffs != nil {
		t.Error("round 1 was given fix diffs")
	}
}

func TestAnInheritedReviewFixDiffsIsStripped(t *testing.T) {
	inherited := []string{"ANTHROPIC_API_KEY=sk-ant-test", `REVIEW_FIX_DIFFS={"0-acme/api":"/work/elsewhere.diff"}`}
	env := envMap(applyReviewEnv(inherited, reviewEnvInputs{passes: reviewPasses, baseCommits: `{"0-acme/api":"abc"}`, round: 2}))
	if _, ok := env["REVIEW_FIX_DIFFS"]; ok {
		t.Errorf("an inherited REVIEW_FIX_DIFFS survived: %q", env["REVIEW_FIX_DIFFS"])
	}
	env = envMap(applyReviewEnv(inherited, reviewEnvInputs{passes: reviewPasses, baseCommits: `{"0-acme/api":"abc"}`, round: 2, fixDiffs: `{"0-acme/api":"/work/.review-fix/0-acme__api.diff"}`}))
	if env["REVIEW_FIX_DIFFS"] != `{"0-acme/api":"/work/.review-fix/0-acme__api.diff"}` {
		t.Errorf("REVIEW_FIX_DIFFS = %q, want the round's own", env["REVIEW_FIX_DIFFS"])
	}
}
