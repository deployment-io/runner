package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/deployment-io/deployment-runner-kit/types"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// --- the undo itself ---------------------------------------------------------

// testCopyTree is the copy these tests inject: the real one without the chown.
//
// The real copy chowns to UID 1000, which an unprivileged test process cannot
// do — calling it here would make every test of the stage's decisions a
// root-only test. The COPY is otherwise the production one, so what is
// exercised below is the real walk, the real modes and the real symlinks;
// ownership is covered separately by a euid-gated case.
func testCopyTree(src, dst string) error {
	return copyTreePreserving(src, dst, nil)
}

// A fix run can do anything an implement run can: delete a tracked file, leave
// an untracked one, rewrite a file, and commit — which moves HEAD and rewrites
// .git. The undo has to put ALL of that back, which is why it is a byte copy of
// the whole directory rather than anything git-shaped.
func TestTheSnapshotRestoresTheRepositoryByteForByte(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	head := initSnapshotTestRepository(t, repoDir)
	before := treeFingerprint(t, repoDir)

	stage := &reviewStage{
		workDirHost: workDir,
		logsWriter:  io.Discard,
		baseCommits: map[string]string{"0-acme/api": head},
		copyTree:    testCopyTree,
	}
	snapshot, err := stage.takeFixRoundSnapshot(1)
	if err != nil {
		t.Fatalf("takeFixRoundSnapshot: %s", err)
	}
	if got := filepath.Dir(snapshot.root); got == workDir {
		t.Errorf("the snapshot %q is inside the work dir — the fix run can see, edit and commit its own undo", snapshot.root)
	}

	// The fix run: deletes, adds, rewrites and commits.
	if err := os.Remove(filepath.Join(repoDir, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoDir, "untracked.tmp"), "half a fix")
	writeFile(t, filepath.Join(repoDir, "main.go"), "package main // half rewritten\n")
	moved := commitEverything(t, repoDir, "the fix run's commit")
	if moved == head {
		t.Fatal("the fix run's commit did not move HEAD, so this test is not exercising the case it names")
	}

	if err := snapshot.restore(); err != nil {
		t.Fatalf("restore: %s", err)
	}

	assertSameTree(t, before, treeFingerprint(t, repoDir))
	if got := headCommit(t, repoDir); got != head {
		t.Errorf("HEAD = %s, want the commit the implement run left at %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "untracked.tmp")); !os.IsNotExist(err) {
		t.Errorf("the fix run's untracked file survived the restore: %v", err)
	}
	if _, err := os.Lstat(snapshot.root); !os.IsNotExist(err) {
		t.Errorf("the snapshot survived a completed restore: %v", err)
	}
}

// The restored tree is what the NEXT container writes through the bind mount.
// A repository owned by root is one the UID-1000 agent cannot touch, so a copy
// that lost the ownership would undo the fix run and break everything after it.
func TestTheSnapshotIsOwnedByTheAgentboxUser(t *testing.T) {
	if os.Geteuid() != commandUtils.AgentboxUID && os.Geteuid() != 0 {
		t.Skipf("chown to %d is not permitted as uid %d", commandUtils.AgentboxUID, os.Geteuid())
	}
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "pkg", "main.go"), "package main\n")
	dst := filepath.Join(t.TempDir(), "copy")

	if err := copyTreeToAgentbox(src, dst); err != nil {
		t.Fatalf("copyTreeToAgentbox: %s", err)
	}
	for _, path := range []string{dst, filepath.Join(dst, "pkg"), filepath.Join(dst, "pkg", "main.go")} {
		assertOwnedByAgentbox(t, path)
	}
}

// A snapshot that covers some of the repositories is not an undo. One copy that
// fails takes the whole snapshot with it, and the caller runs no fix.
func TestAPartialSnapshotIsAbandonedRatherThanKept(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "main.go"), "package main\n")
	stage := &reviewStage{
		workDirHost: workDir,
		logsWriter:  io.Discard,
		// The second repository was never checked out, so copying it fails.
		baseCommits: map[string]string{"0-acme/api": "abc123", "1-acme/web": "def456"},
		copyTree:    testCopyTree,
	}

	if _, err := stage.takeFixRoundSnapshot(1); err == nil {
		t.Fatal("a snapshot missing a repository was reported as usable")
	}
	if _, err := os.Stat(fixRoundSnapshotPath(workDir, 1)); !os.IsNotExist(err) {
		t.Errorf("the half-taken snapshot was left on the host: %v", err)
	}
}

// --- what the loop does with a fix run that did not finish -------------------

// A fix run's failure is not the Step's. Each shape below leaves the tree
// exactly as the implement run left it, records why, and hands the findings to
// a human on the pull request — without failing the Step.
func TestAFailedFixRunRestoresTheChangeAndHandsOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result agentResult
		want   string
	}{
		{
			name:   "the fix run did not succeed",
			result: agentResult{Status: "failure", ExitCode: 1, Error: "claude exited: max turns reached"},
			want:   "max turns reached",
		},
		{
			name: "the fix run failed its own verify gate",
			result: agentResult{Status: "success", VerifyResult: &verifyResult{
				Ran: true, Command: "go build ./... && go test ./...",
				StderrTail: "handler.go:41: undefined: session",
				Steps:      []verifyStep{{Repo: "0-acme/api", Passed: false}},
			}},
			want: "undefined: session",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			repoDir := filepath.Join(workDir, "0-acme/api")
			writeFile(t, filepath.Join(repoDir, "handler.go"), "the implementation\n")
			var logs strings.Builder
			stage := fixLoopStage(workDir, &logs)
			stage.runFix = func([]reviewFindingOutput) error {
				// The fix run gets half way and leaves the tree behind.
				writeFile(t, filepath.Join(repoDir, "handler.go"), "half a fix\n")
				writeFile(t, filepath.Join(repoDir, "scratch.tmp"), "")
				return fixRunOutcome(tc.result, io.Discard)
			}

			out, err := stage.run()
			if err != nil {
				t.Fatalf("a failed fix run failed the Step: %s", err)
			}

			if got := readFile(t, filepath.Join(repoDir, "handler.go")); got != "the implementation\n" {
				t.Errorf("handler.go = %q, want the implement run's own version back", got)
			}
			if _, err := os.Stat(filepath.Join(repoDir, "scratch.tmp")); !os.IsNotExist(err) {
				t.Errorf("the fix run's leftovers survived: %v", err)
			}
			review := readReviewFromJobOutput(out)
			if review == nil {
				t.Fatal("no review was recorded")
			}
			if !review.MustFixOpen {
				t.Error("the must-fix finding the fix run was sent to fix was not left open")
			}
			if !strings.Contains(review.FixError, tc.want) {
				t.Errorf("fix_error = %q, want it to name %q", review.FixError, tc.want)
			}
			if _, err := os.Stat(fixRoundSnapshotPath(workDir, 1)); !os.IsNotExist(err) {
				t.Errorf("the snapshot was left on the host after a completed restore: %v", err)
			}
			if !strings.Contains(logs.String(), "back to exactly what it was") {
				t.Errorf("the job log does not say the change was put back:\n%s", logs.String())
			}
		})
	}
}

// A user stop is not a failed fix. It still reaches the outer loop's stop path
// unchanged — nothing is restored, nothing is recorded, and the sentinel is
// returned exactly as it was before the undo existed.
func TestAUserStopDuringTheFixRunStillReturnsTheSentinel(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, nil)
	stage.runFix = func([]reviewFindingOutput) error {
		return fmt.Errorf("error waiting for the fix run: %w", types.ErrJobStoppedByUser)
	}

	out, err := stage.run()
	if !errors.Is(err, types.ErrJobStoppedByUser) {
		t.Fatalf("run returned %v, want the user-stop sentinel", err)
	}
	if readReviewFromJobOutput(out) != nil {
		t.Error("a stopped Job recorded a review block; the stop path returns before the stage finishes")
	}
	// The copy is left where it is — the deferred sweep collects it, and a
	// stopped Job is not a Job whose tree anybody is about to commit.
	if _, err := os.Stat(fixRoundSnapshotPath(workDir, 1)); err != nil {
		t.Errorf("the stop path went out of its way to remove the snapshot: %v", err)
	}
	cleanupReviewStageSiblings(workDir)
	if _, err := os.Stat(fixRoundSnapshotPath(workDir, 1)); !os.IsNotExist(err) {
		t.Errorf("the sweep left a whole checkout's worth of copy on the host: %v", err)
	}
}

// NEVER RUN A FIX THAT CANNOT BE UNDONE. With no snapshot there is no way back
// from a fix run that fails, so the fix does not start at all.
func TestASnapshotFailureSkipsTheFixRunAndHandsOff(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	var logs strings.Builder
	stage := fixLoopStage(workDir, &logs)
	stage.copyTree = func(string, string) error { return errors.New("no space left on device") }
	fixRan := false
	stage.runFix = func([]reviewFindingOutput) error {
		fixRan = true
		return nil
	}

	out, err := stage.run()
	if err != nil {
		t.Fatalf("a snapshot that could not be taken failed the Step: %s", err)
	}
	if fixRan {
		t.Error("a fix run started with no way to undo it")
	}
	review := readReviewFromJobOutput(out)
	if review == nil || !review.MustFixOpen {
		t.Fatalf("review = %+v, want the findings handed over still open", review)
	}
	if !review.FixNotAttempted {
		t.Error("fix_not_attempted = false, want the pull request told no fix was attempted")
	}
	// The detail (which can carry host paths) is for the job log, not the
	// pull request.
	if strings.Contains(review.FixError, "no space left on device") {
		t.Errorf("fix_error = %q carries the internal error into the pull request", review.FixError)
	}
	if !strings.Contains(logs.String(), "no space left on device") {
		t.Errorf("the job log does not say why the fix was skipped:\n%s", logs.String())
	}
}

// If the restore itself fails the tree is neither the implementation nor the
// fix. That cannot be committed and cannot be described on a pull request, so
// it is the ONE fix-related path that fails the Step.
func TestARestoreFailureFailsTheStep(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	var logs strings.Builder
	stage := fixLoopStage(workDir, &logs)
	stage.restoreDir = func(string, string) error { return errors.New("cross-device link") }
	stage.runFix = func([]reviewFindingOutput) error {
		return fixRunOutcome(agentResult{Status: "failure", Error: "the fix run crashed"}, io.Discard)
	}

	if _, err := stage.run(); err == nil {
		t.Fatal("a tree that could not be put back was committed anyway")
	} else if !strings.Contains(err.Error(), "cross-device link") {
		t.Errorf("error = %q, want it to name why the restore failed", err)
	}
	if !strings.Contains(logs.String(), "could NOT be put back") {
		t.Errorf("the job log does not report the failed restore:\n%s", logs.String())
	}
}

// A fix run that DID finish leaves nothing behind: the undo is dropped as soon
// as it stops being one, and the loop carries on to the next review round.
func TestASuccessfulFixRunRemovesItsSnapshotAndContinues(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, nil)
	fixRuns := 0
	stage.runFix = func([]reviewFindingOutput) error {
		fixRuns++
		return fixRunOutcome(agentResult{Status: "success"}, io.Discard)
	}

	if _, err := stage.run(); err != nil {
		t.Fatalf("run: %s", err)
	}
	// The stand-in reviewer reports the same finding every round, so the loop
	// runs its two fix rounds and then hands over — unchanged behaviour.
	if fixRuns != maxMustFixRounds {
		t.Errorf("the loop ran %d fix round(s), want %d", fixRuns, maxMustFixRounds)
	}
	for round := 1; round <= maxMustFixRounds; round++ {
		if _, err := os.Stat(fixRoundSnapshotPath(workDir, round)); !os.IsNotExist(err) {
			t.Errorf("fix round %d's snapshot was kept after the fix succeeded: %v", round, err)
		}
	}
}

// --- the pull-request body ---------------------------------------------------

// The diff under a pull request whose fix run failed is the IMPLEMENTER's, not
// a half-applied cleanup. A reader comparing the findings against the change
// has to be told, or they go looking for fix work that was rolled back.
func TestTheReviewSectionNamesAFailedFixAttemptOnlyWhenThereWasOne(t *testing.T) {
	review := completedReview(true, reviewFindingOutput{
		Parameter: "security", Severity: "high", Location: "0-acme/api/handler.go:41",
		What: "no session check", MustFix: true,
	})

	if section := (&taskOpenPR{review: review}).reviewSection(); strings.Contains(section, "A fix attempt did not complete") {
		t.Errorf("a review with no failed fix attempt claimed one:\n%s", section)
	}

	review.FixError = `agent step did not succeed: status=failure exit_code=1 error="max turns reached"`
	section := (&taskOpenPR{review: review}).reviewSection()
	want := "A fix attempt did not complete (" + review.FixError + "); the change is shown as it was before that attempt."
	if !strings.Contains(section, want) {
		t.Errorf("the review section does not carry the failed-fix sentence:\n%s", section)
	}
	// Under the must-fix findings, which is what the sentence is about.
	if strings.Index(section, "no session check") > strings.Index(section, want) {
		t.Errorf("the failed-fix sentence is above the findings it qualifies:\n%s", section)
	}
}

// --- helpers -----------------------------------------------------------------

// fixLoopStage is a Review stage whose review round is a stand-in: one
// unfixed must-fix finding, every round. Everything the loop does with the fix
// run's outcome is real.
func fixLoopStage(workDir string, logs io.Writer) *reviewStage {
	if logs == nil {
		logs = io.Discard
	}
	return &reviewStage{
		parameters:    map[string]interface{}{},
		workDirHost:   workDir,
		logsWriter:    logs,
		participation: participationOn,
		thresholds:    map[uint]uint{1: 4},
		baseCommits:   map[string]string{"0-acme/api": "abc123"},
		deadline:      time.Now().Add(reviewStageBudget),
		copyTree:      testCopyTree,
		runReview: func(int) (agentResult, error) {
			return agentResult{Status: "success", Turns: 4, ReviewResult: &reviewResult{
				Findings: []reviewFinding{{
					Key: "sec-1", Parameter: "security", Severity: "high",
					Location: "0-acme/api/handler.go:41", What: "the handler does not check the caller's session",
				}},
				Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}},
			}}, nil
		},
	}
}

// initSnapshotTestRepository builds a checkout that exercises every shape the
// copy has to carry: a tracked file, an executable one, a symlink, and a real
// .git with a commit in it. Returns the commit HEAD points at.
func initSnapshotTestRepository(t *testing.T, repoDir string) string {
	t.Helper()
	writeFile(t, filepath.Join(repoDir, "main.go"), "package main\n")
	writeFile(t, filepath.Join(repoDir, "doomed.txt"), "deleted by the fix run\n")
	writeFile(t, filepath.Join(repoDir, "scripts", "build.sh"), "#!/bin/sh\ngo build ./...\n")
	if err := os.Chmod(filepath.Join(repoDir, "scripts", "build.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("main.go", filepath.Join(repoDir, "entry.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatal(err)
	}
	return commitEverything(t, repoDir, "the implement run's commit")
}

func commitEverything(t *testing.T, repoDir, message string) string {
	t.Helper()
	repository, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "agent", Email: "agent@example.com", When: time.Unix(1700000000, 0).UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func headCommit(t *testing.T, repoDir string) string {
	t.Helper()
	repository, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash().String()
}

// treeFingerprint is every path under root with its mode and its contents —
// the link target for a symlink, so a symlink that was copied as its target
// shows up as a difference rather than as a match.
func treeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %o", info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "symlink -> " + target
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file %o %s", info.Mode().Perm(), data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprinting %s: %s", root, err)
	}
	return out
}

func assertSameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	paths := map[string]bool{}
	for path := range want {
		paths[path] = true
	}
	for path := range got {
		paths[path] = true
	}
	var names []string
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	for _, path := range names {
		if want[path] != got[path] {
			t.Errorf("%s: after the restore it is %q, was %q", path, got[path], want[path])
		}
	}
}

// A failed fix run's work is undone on disk, so its RESULT must not reach the
// pull request or the commit: its title, summary, files and failing verify
// would describe a fix that is not there. Its usage still counts.
func TestAFailedFixRunsResultIsNotMergedButItsUsageIs(t *testing.T) {
	parameters := map[string]interface{}{}
	implement := agentResult{Status: "success", Turns: 10, PRTitle: "Add session handler",
		ChangesSummary: "Add session handler", TokenUsage: tokenUsage{InputTokens: 100}}
	if err := mergeAgentResultIntoJobOutput(parameters, implement); err != nil {
		t.Fatal(err)
	}
	failed := agentResult{Status: "failure", Turns: 5, PRTitle: "fix: check caller session",
		ChangesSummary: "Add session check to handler", TokenUsage: tokenUsage{InputTokens: 50}}
	if err := recordFixRunResult(parameters, failed, nil, io.Discard); err == nil {
		t.Fatal("a failed fix run was reported as kept")
	}
	data := jobOutputFor(t, parameters)
	if data.Agent.PRTitle != "Add session handler" || strings.Contains(data.Agent.ChangesSummary, "session check") {
		t.Errorf("the rolled-back fix reached the record: title %q, summary %q", data.Agent.PRTitle, data.Agent.ChangesSummary)
	}
	if data.Agent.Turns != 15 || data.Agent.TokenUsage.InputTokens != 150 {
		t.Errorf("turns %d, input tokens %d; want the failed fix run's usage counted (15, 150)", data.Agent.Turns, data.Agent.TokenUsage.InputTokens)
	}

	kept := agentResult{Status: "success", Turns: 3, ChangesSummary: "Add session check to handler"}
	if err := recordFixRunResult(parameters, kept, nil, io.Discard); err != nil {
		t.Fatalf("a successful fix run was reported as failed: %s", err)
	}
	if data := jobOutputFor(t, parameters); !strings.Contains(data.Agent.ChangesSummary, "session check") {
		t.Errorf("a kept fix run's summary was not merged: %q", data.Agent.ChangesSummary)
	}
}

func jobOutputFor(t *testing.T, parameters map[string]interface{}) jobOutputData {
	t.Helper()
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil {
		t.Fatal(err)
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	if data.Agent == nil {
		t.Fatal("job output has no agent block")
	}
	return data
}

// The work dir is agent-writable and the copy and restore run as root, so a
// repository path swapped for a symlink — at any component — is refused
// rather than followed.
func TestSymlinkedRepositoryPathsAreRefused(t *testing.T) {
	workDir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "api", "victim.go"), "not the Task's\n")
	if err := os.Symlink(outside, filepath.Join(workDir, "0-acme")); err != nil {
		t.Fatal(err)
	}
	if err := ensureRealRepositoryDir(workDir, "0-acme/api"); err == nil {
		t.Error("a repository path through a symlinked component was accepted")
	}
	for _, bad := range []string{"../escape", "/abs", ".", ""} {
		if err := ensureRealRepositoryDir(workDir, bad); err == nil {
			t.Errorf("repository path %q was accepted", bad)
		}
	}
	stage := fixLoopStage(workDir, nil)
	if _, err := stage.takeFixRoundSnapshot(1); err == nil {
		t.Error("a snapshot was taken through a symlinked repository path")
	}
	if _, err := os.Stat(filepath.Join(outside, "api", "victim.go")); err != nil {
		t.Errorf("the directory outside the Task was touched: %s", err)
	}
}

// The restore moves the fix run's tree aside before moving the copy in, so a
// failure never leaves a repository missing.
func TestRestoreMovesTheFixRunsTreeAsideFirst(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "work", "0-acme", "api")
	snap := filepath.Join(root, "snap", "0-acme", "api")
	writeFile(t, filepath.Join(live, "handler.go"), "fix run's version\n")
	writeFile(t, filepath.Join(snap, "handler.go"), "implementation\n")
	if err := restoreRepositoryDir(snap, live); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(live, "handler.go")); string(got) != "implementation\n" {
		t.Errorf("restored content = %q", got)
	}
	if _, err := os.Stat(snap + ".fix-run-tree"); !os.IsNotExist(err) {
		t.Error("the set-aside fix-run tree was left behind")
	}

	// A copy that cannot be moved in leaves the fix run's tree in place, whole.
	writeFile(t, filepath.Join(live, "handler.go"), "fix run's version\n")
	if err := restoreRepositoryDir(filepath.Join(root, "missing"), live); err == nil {
		t.Fatal("a restore from a missing copy succeeded")
	}
	if got, _ := os.ReadFile(filepath.Join(live, "handler.go")); string(got) != "fix run's version\n" {
		t.Errorf("after a failed restore the repository holds %q, want the fix run's tree back in place", got)
	}
}

// Every checked-out repository is copied, including one with no recorded
// start commit (a new, empty repository).
func TestRepositoryDirsIncludeOnesWithNoStartCommit(t *testing.T) {
	parameters := map[string]interface{}{}
	raw, _ := json.Marshal(jobOutputData{BaseCommits: []baseCommitOutput{
		{Index: 0, Dir: "0-acme/api", CommitSHA: "abc"},
		{Index: 1, Dir: "1-acme/new"},
	}})
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(raw))
	got := readRepositoryDirsFromJobOutput(parameters)
	if len(got) != 2 || got[1] != "1-acme/new" {
		t.Errorf("repository dirs = %v, want both, including the one with no start commit", got)
	}
	if bc := readBaseCommitsFromJobOutput(parameters); len(bc) != 1 {
		t.Errorf("base commits = %v; the review baseline must still skip a repository with no commit", bc)
	}
}

// The pull request states what actually happened: no "fix budget ran out"
// when the loop ended on a failed or skipped fix, and a plain sentence when no
// fix was attempted.
func TestTheReviewSectionWordsTheFixOutcomeHonestly(t *testing.T) {
	base := func() *reviewOutput {
		return completedReview(true, reviewFindingOutput{Parameter: "security", Severity: "high",
			Location: "handler.go:41", What: "no session check", MustFix: true})
	}
	failed := base()
	failed.FixError = "agent step did not succeed"
	body := reviewTestOpener(failed).reviewSection()
	if strings.Contains(body, "fix budget ran out") || !strings.Contains(body, "A fix attempt did not complete") {
		t.Errorf("a failed fix is worded wrongly:\n%s", body)
	}
	skipped := base()
	skipped.FixError = "the change could not be copied first, so a failed fix could not have been undone"
	skipped.FixNotAttempted = true
	body = reviewTestOpener(skipped).reviewSection()
	if !strings.Contains(body, "No fix was attempted") || strings.Contains(body, "A fix attempt did not complete") {
		t.Errorf("a skipped fix is worded wrongly:\n%s", body)
	}
	exhausted := base()
	if body := reviewTestOpener(exhausted).reviewSection(); !strings.Contains(body, "fix budget ran out") {
		t.Errorf("an exhausted loop lost its wording:\n%s", body)
	}
}
