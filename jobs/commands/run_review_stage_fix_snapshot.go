package commands

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// THE FIX RUN'S UNDO.
//
// A fix run is a full implement run against a working tree that has ALREADY
// passed its own verify gate. When it does not finish — its turn cap, its
// timeout, a crash, a spawn or vendor error, or its own verify gate failing —
// what is left on disk is half of a cleanup sitting on top of a finished
// implementation. Neither committing that nor failing the Step is acceptable:
// the Review stage never fails the Step and never discards work.
//
// So the tree is put back exactly as it was before the fix run started, and the
// findings go to a human on the pull request instead. That is only possible if
// the undo was taken FIRST, which is why a snapshot that cannot be taken means
// no fix run at all.
//
// The undo is a whole-directory copy rather than anything git-shaped. A fix run
// may commit, may leave untracked files, may delete tracked ones, may stage
// things, and may rewrite .git itself; `git reset --hard` plus a clean would
// undo some of that and silently keep the rest. A byte copy of the directory,
// .git included, is the only restore that covers every one of them.

// fixRoundSnapshotPath is where one fix run's undo copy lives: a sibling of the
// work dir, following implementerOutputStashPath and reviewRoundOutputPath.
//
// OUTSIDE the work dir, not a dot-directory inside it — everything under the
// work dir is visible to the container at /work, and a fix run that could see
// (or edit, or commit) a copy of the tree it is editing is not an undo.
func fixRoundSnapshotPath(workDirHost string, round int) string {
	return fmt.Sprintf("%s-fix-round-%d-snapshot", strings.TrimRight(workDirHost, "/"), round)
}

// copyTreeFunc takes the copy; restoreDirFunc puts one repository directory
// back from it.
//
// Injected rather than called directly so the stage's decisions can be tested
// without root: the real copy chowns to UID 1000, which an unprivileged test
// process cannot do. Ownership itself is still covered, by a euid-gated case.
type (
	copyTreeFunc   func(src, dst string) error
	restoreDirFunc func(snapshotDir, repoDir string) error
)

// fixRoundSnapshot is one fix run's undo: a copy of every repository directory
// the stage knows about, parked beside the work dir.
type fixRoundSnapshot struct {
	root        string
	workDirHost string
	// repoDirs are the repository directories that were copied, relative to
	// the work dir. Only the ones that actually landed: a restore must not
	// try to put back a copy that was never taken.
	repoDirs   []string
	restoreDir restoreDirFunc
}

// takeFixRoundSnapshot copies every repository directory named in the stage's
// base commits, so the fix run about to start can be undone.
//
// ALL OR NOTHING. A snapshot missing one of several repositories is not an
// undo, so a copy that fails takes the whole snapshot with it and the caller
// runs no fix.
func (s *reviewStage) takeFixRoundSnapshot(round int) (*fixRoundSnapshot, error) {
	copyTree := s.copyTree
	if copyTree == nil {
		copyTree = copyTreeToAgentbox
	}
	restore := s.restoreDir
	if restore == nil {
		restore = restoreRepositoryDir
	}
	snapshot := &fixRoundSnapshot{
		root:        fixRoundSnapshotPath(s.workDirHost, round),
		workDirHost: s.workDirHost,
		restoreDir:  restore,
	}
	// A snapshot left by an earlier round, or by an earlier Job on the same
	// work dir, is certainly stale — the live tree is the one in the work dir.
	if err := os.RemoveAll(snapshot.root); err != nil {
		return nil, err
	}
	started := time.Now()
	for _, repoDir := range sortedRepositoryDirs(s.baseCommits) {
		dst := filepath.Join(snapshot.root, repoDir)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			snapshot.remove()
			return nil, err
		}
		if err := copyTree(filepath.Join(s.workDirHost, repoDir), dst); err != nil {
			snapshot.remove()
			return nil, fmt.Errorf("error copying %s: %s", repoDir, err)
		}
		snapshot.repoDirs = append(snapshot.repoDirs, repoDir)
	}
	// Logged with both numbers: a copy is the one part of the undo that scales
	// with the checkout, and a stage that spent a minute of its budget moving
	// a gigabyte has to say so rather than look like a stalled fix run.
	io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: copied %d repository directory/directories (%s) in %s so fix round %d can be undone\n",
		len(snapshot.repoDirs), formatTreeSize(treeSize(snapshot.root)), time.Since(started).Round(time.Millisecond), round))
	return snapshot, nil
}

// sortedRepositoryDirs is the base commits' repository directories in a stable
// order, so the log and any failure name them the same way twice.
func sortedRepositoryDirs(baseCommits map[string]string) []string {
	dirs := make([]string, 0, len(baseCommits))
	for dir := range baseCommits {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// remove drops the copy. Called when the fix run succeeded and the undo is no
// longer wanted, and when a half-taken snapshot has to be abandoned.
func (snap *fixRoundSnapshot) remove() {
	if snap == nil {
		return
	}
	_ = os.RemoveAll(snap.root)
}

// restore puts every copied repository directory back exactly as it was.
//
// Remove-then-rename rather than a copy back: the fix run may have created
// files that were not there before, and a copy over the top would leave them
// in place. The rename is also atomic per repository, so a restore that fails
// fails with the remaining directories still untouched rather than merged.
//
// An error here is the ONE fix-related failure that fails the Step: the tree is
// then neither the implementation nor the fix, and nothing downstream can tell
// which parts are which.
func (snap *fixRoundSnapshot) restore() error {
	for _, repoDir := range snap.repoDirs {
		if err := snap.restoreDir(filepath.Join(snap.root, repoDir), filepath.Join(snap.workDirHost, repoDir)); err != nil {
			return fmt.Errorf("%s: %s", repoDir, err)
		}
	}
	snap.remove()
	return nil
}

// restoreRepositoryDir is the real restore for one repository: drop what the
// fix run left and move the copy back under its name.
func restoreRepositoryDir(snapshotDir, repoDir string) error {
	if err := os.RemoveAll(repoDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return err
	}
	return os.Rename(snapshotDir, repoDir)
}

// copyTreeToAgentbox is the real copy: modes and symlinks as they were, and
// every entry owned by the agentbox `agent` user.
//
// Ownership is not decoration. The restored tree is what the NEXT container
// writes through the bind mount, and a repository owned by root is one the
// UID-1000 agent cannot touch — so a copy that lost the ownership would undo
// the fix run and break every run after it.
func copyTreeToAgentbox(src, dst string) error {
	return copyTreePreserving(src, dst, lchownToAgentbox)
}

func lchownToAgentbox(path string) error {
	return os.Lchown(path, commandUtils.AgentboxUID, commandUtils.AgentboxGID)
}

// copyTreePreserving copies src to dst recursively, preserving file modes and
// copying symlinks AS SYMLINKS — a checkout's symlinks can point outside the
// tree, and following them would copy the target into the repository.
//
// chown is applied to every entry created, and may be nil for a caller that
// cannot chown (an unprivileged test process).
func copyTreePreserving(src, dst string, chown func(path string) error) error {
	var dirs []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			// Mode and ownership are applied AFTER the walk: a directory whose
			// own mode forbids writing still has to have its contents copied
			// into it first.
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, rel)
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyRegularFile(path, target, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			// A socket, fifo or device node is not part of a checkout and
			// cannot be reproduced by reading it. Skipped rather than failed:
			// refusing the snapshot over one would mean refusing the fix run.
			return nil
		}
		if chown == nil {
			return nil
		}
		return chown(target)
	})
	if err != nil {
		return err
	}
	// Deepest last written, so deepest first here: a directory's mode is only
	// safe to apply once nothing more goes inside it.
	for i := len(dirs) - 1; i >= 0; i-- {
		info, err := os.Lstat(filepath.Join(src, dirs[i]))
		if err != nil {
			return err
		}
		target := filepath.Join(dst, dirs[i])
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
		if chown != nil {
			if err := chown(target); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyRegularFile copies one file's contents and mode. The mode is set
// explicitly after the write because O_CREATE applies the process umask, which
// would quietly drop the execute bit off a checked-in script.
func copyRegularFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// treeSize totals the bytes under root, for the log line. Best-effort: a size
// nobody could read is worth less than the copy it describes.
func treeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func formatTreeSize(bytes int64) string {
	const mib = 1 << 20
	if bytes < mib {
		return fmt.Sprintf("%d KiB", bytes/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/mib)
}
