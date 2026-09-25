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
	repoDirs := s.repoDirs
	if len(repoDirs) == 0 {
		repoDirs = sortedRepositoryDirs(s.baseCommits)
	}
	for _, repoDir := range repoDirs {
		// The work dir is writable by the agent, and everything below runs as
		// root: a repository path the agent swapped for a symlink would make
		// the copy read, and the restore delete and replace, a directory
		// outside the Task. No container is running here, so the check
		// cannot be raced.
		if err := ensureRealRepositoryDir(s.workDirHost, repoDir); err != nil {
			snapshot.remove()
			return nil, err
		}
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
// An error here is the ONE fix-related failure that fails the Step: the tree is
// then neither the implementation nor the fix, and nothing downstream can tell
// which parts are which. Each repository path is re-checked first — the fix
// run had the work dir writable, and a path it replaced with a symlink must
// not be followed by a root-owned delete and rename.
func (snap *fixRoundSnapshot) restore() error {
	for _, repoDir := range snap.repoDirs {
		if err := ensureRealRepositoryDir(snap.workDirHost, repoDir); err != nil {
			return err
		}
		if err := snap.restoreDir(filepath.Join(snap.root, repoDir), filepath.Join(snap.workDirHost, repoDir)); err != nil {
			return fmt.Errorf("%s: %s", repoDir, err)
		}
	}
	snap.remove()
	return nil
}

// restoreRepositoryDir is the real restore for one repository.
//
// MOVE ASIDE, MOVE IN, THEN DELETE. The fix run's tree is renamed out of the
// way first and the copy renamed into its place; only then is the set-aside
// tree deleted, best-effort. Deleting first would leave nothing in place if
// the delete failed partway (a busy or immutable file), and the stage's
// cleanup would then remove the only good copy with the snapshot. Renames on
// one filesystem are atomic, so each repository is always either the fix
// run's tree or the copy, never a mixture.
func restoreRepositoryDir(snapshotDir, repoDir string) error {
	aside := snapshotDir + ".fix-run-tree"
	if err := os.RemoveAll(aside); err != nil {
		return err
	}
	if err := os.Rename(repoDir, aside); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(snapshotDir, repoDir); err != nil {
		// Put the fix run's tree back where it was, so the error describes a
		// tree that is at least whole.
		_ = os.Rename(aside, repoDir)
		return err
	}
	_ = os.RemoveAll(aside)
	return nil
}

// ensureRealRepositoryDir refuses a repository path that is not a plain
// relative path to a real directory under the work dir: absolute, escaping
// with "..", the work dir itself, or passing through a symlink at any
// component.
func ensureRealRepositoryDir(workDirHost, repoDir string) error {
	if !filepath.IsLocal(repoDir) || filepath.Clean(repoDir) == "." {
		return fmt.Errorf("repository directory %q is not a path inside the work dir", repoDir)
	}
	current := workDirHost
	for _, part := range strings.Split(filepath.Clean(repoDir), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("repository directory %q passes through a symlink", repoDir)
		}
		if !info.IsDir() {
			return fmt.Errorf("repository directory %q is not a directory", repoDir)
		}
	}
	return nil
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

// copyTreePreserving copies src to dst recursively, preserving file modes
// (including setuid, setgid and sticky bits) and copying symlinks AS SYMLINKS —
// a checkout's symlinks can point outside the tree, and following them would
// copy the target into the repository.
//
// chown is applied to every entry created, and may be nil for a caller that
// cannot chown (an unprivileged test process). Modes are applied AFTER
// ownership: a chown clears setuid and setgid.
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
		if info.IsDir() {
			// Mode and ownership are applied AFTER the walk: a directory whose
			// own mode forbids writing still has to have its contents copied
			// into it first.
			dirs = append(dirs, rel)
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		return copyTreeEntry(path, filepath.Join(dst, rel), info, chown)
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
		if err := chownThenChmod(filepath.Join(dst, dirs[i]), fullMode(info), chown); err != nil {
			return err
		}
	}
	return nil
}

// copyTreeEntry copies one non-directory entry: a symlink as a symlink, a
// regular file with its contents and mode. A socket, fifo or device node is
// not part of a checkout and cannot be reproduced by reading it, so it is
// skipped rather than failing the whole copy (and with it the fix run).
func copyTreeEntry(src, dst string, info fs.FileInfo, chown func(path string) error) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(link, dst); err != nil {
			return err
		}
		if chown == nil {
			return nil
		}
		return chown(dst)
	case info.Mode().IsRegular():
		if err := copyRegularFile(src, dst, info.Mode().Perm()); err != nil {
			return err
		}
		return chownThenChmod(dst, fullMode(info), chown)
	default:
		return nil
	}
}

// fullMode is the part of a mode chmod can set: permissions plus the setuid,
// setgid and sticky bits.
func fullMode(info fs.FileInfo) fs.FileMode {
	return info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// chownThenChmod applies ownership, then the mode — in that order, because a
// chown clears setuid and setgid.
func chownThenChmod(path string, mode fs.FileMode, chown func(path string) error) error {
	if chown != nil {
		if err := chown(path); err != nil {
			return err
		}
	}
	return os.Chmod(path, mode)
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
