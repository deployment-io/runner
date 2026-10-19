package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// THE FIX RUN'S OWN DIFF.
//
// A review round after a kept fix re-reads the whole change, but what it most
// needs to check is narrower: did the fix resolve what it was sent, and did it
// break anything. So each repository is snapshotted as a git TREE before the
// fix run and again after it, and the diff between the two trees — exactly
// what the fix changed, nothing else — is handed to the next round as the
// place to start (REVIEW_FIX_DIFFS).
//
// NOTHING IS WRITTEN INTO THE REPOSITORY. Not its index, not its working tree,
// not its .git directory. Every git command runs with a temporary index (a copy
// of the repository's) and a temporary object directory, with the repository's
// own objects as an ALTERNATE so existing blobs are still found. Objects
// written into the repository would be owned by the runner's user, and a later
// fix run — which runs as the agentbox user — could then fail to write beside
// them.
//
// NOR IS THE REPOSITORY'S OWN GIT CONFIG READ. The repository is writable by
// the agent, and git honours commands named in a repository's config: a clean
// filter runs on `git add`, core.fsmonitor on any index refresh, an external
// diff or textconv on `git diff`. Each would run on the runner's host. So git
// is pointed at a throwaway GIT_DIR with an empty config, and the repository
// directory is only its WORK TREE. The repository's ignore rules (.gitignore
// files, and .git/info/exclude, copied across) still apply, which is what keeps
// node_modules, build output and test caches out of the trees.
//
// FAIL-OPEN, LIKE THE REST OF THE STAGE. Any failure leaves that repository
// without a fix diff and the next round reviews the whole change, exactly as
// it did before this existed. Nothing here can fail the Step or change what
// the loop does.

const (
	// fixTreesGitTimeout bounds one git command.
	fixTreesGitTimeout = 2 * time.Minute
	// fixTreesGitWaitDelay bounds the wait for a killed git command's output.
	fixTreesGitWaitDelay = 5 * time.Second
	// maxFixDiffBytes is the largest fix diff handed to a round. A fix that
	// rewrote this much is no narrower than the whole change.
	maxFixDiffBytes = 1 << 20
	// reviewFixDiffDirName is the directory beside the repository checkouts
	// where the round after a kept fix finds the fix's own diffs.
	reviewFixDiffDirName = ".review-fix"
)

// fixTrees is one fix run's before/after tree snapshots, for every repository
// directory the stage knows.
type fixTrees struct {
	workDirHost string
	logsWriter  io.Writer
	// git is the git binary; "git" from PATH unless a test substitutes one.
	git string
	// root holds one temporary directory per repository, outside every
	// repository; removed by remove.
	root string
	// before is the tree id per repository taken before the fix run.
	before map[string]string
	// failed is why a repository has no usable "before" snapshot.
	failed map[string]string
	// dirs is every repository directory, in order.
	dirs []string
	// ctx, when set, bounds every git command (the QA stage's merge result
	// runs under one overall timeout); nil for the fix trees.
	ctx context.Context
}

// takeFixTrees snapshots every repository directory the stage knows
// ("before"), ahead of a fix run.
func (s *reviewStage) takeFixTrees() *fixTrees {
	t := &fixTrees{
		workDirHost: s.workDirHost,
		logsWriter:  s.logsWriter,
		git:         s.fixTreesGit,
		dirs:        s.allRepositoryDirs(),
	}
	t.takeBefore(s.fixTreesTmpRoot)
	return t
}

// takeBefore creates the temporary directory under tmpRoot ("" for the system
// default) and snapshots every repository.
//
// It never fails: a repository that could not be snapshotted is remembered
// with its reason, and fixDiffs reports it as having no fix diff.
func (t *fixTrees) takeBefore(tmpRoot string) {
	if t.git == "" {
		t.git = "git"
	}
	t.before = map[string]string{}
	t.failed = map[string]string{}
	root, err := os.MkdirTemp(tmpRoot, "review-fix-trees-")
	if err != nil {
		for _, dir := range t.dirs {
			t.failed[dir] = fmt.Sprintf("could not create a temporary directory: %s", err)
		}
		return
	}
	t.root = root
	for i, dir := range t.dirs {
		tree, err := t.prepare(dir, filepath.Join(root, fmt.Sprintf("repo-%d", i)))
		if err != nil {
			t.failed[dir] = err.Error()
			continue
		}
		t.before[dir] = tree
	}
}

// remove deletes the temporary directory. Safe to call more than once, and on
// a nil receiver.
func (t *fixTrees) remove() {
	if t == nil || t.root == "" {
		return
	}
	_ = os.RemoveAll(t.root)
	t.root = ""
}

// fixDiffs snapshots every repository again ("after") and returns the diff
// between its two trees, keyed by repository directory. A repository whose
// trees are equal is left out; one that failed anywhere is left out with a
// log line.
func (t *fixTrees) fixDiffs() map[string]string {
	out := map[string]string{}
	for i, dir := range t.dirs {
		before, ok := t.before[dir]
		if !ok {
			logNoFixDiff(t.logsWriter, dir, t.failed[dir])
			continue
		}
		tmp := filepath.Join(t.root, fmt.Sprintf("repo-%d", i))
		after, err := t.snapshot(dir, tmp)
		if err != nil {
			logNoFixDiff(t.logsWriter, dir, err.Error())
			continue
		}
		if after == before {
			continue
		}
		diff, err := t.diff(dir, tmp, before, after)
		if err != nil {
			logNoFixDiff(t.logsWriter, dir, err.Error())
			continue
		}
		out[dir] = diff
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// logNoFixDiff is the one line every fail-open path of the fix diff writes.
func logNoFixDiff(w io.Writer, dir, reason string) {
	if reason == "" {
		reason = "no snapshot was taken"
	}
	io.WriteString(w, fmt.Sprintf("Review stage: no fix diff for %s (%s) — the next round reviews the whole change\n", dir, reason))
}

// prepare lays out the repository's temporary git directory and takes its
// "before" snapshot.
func (t *fixTrees) prepare(dir, tmp string) (string, error) {
	_, gitDir, err := t.repositoryPaths(dir)
	if err != nil {
		return "", err
	}
	gd := filepath.Join(tmp, "gitdir")
	for _, d := range []string{filepath.Join(gd, "objects"), filepath.Join(gd, "refs"), filepath.Join(gd, "info")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(gd, "HEAD"), []byte("ref: refs/heads/fix-trees\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(gd, "config"), []byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n"), 0o600); err != nil {
		return "", err
	}
	// The repository's own exclude file is part of its ignore rules.
	if err := copyRegularFileIfPresent(filepath.Join(gitDir, "info", "exclude"), filepath.Join(gd, "info", "exclude")); err != nil {
		return "", fmt.Errorf("could not copy .git/info/exclude: %s", err)
	}
	// A new repository may have no index yet; git starts an empty one.
	if err := copyRegularFileIfPresent(filepath.Join(gitDir, "index"), filepath.Join(tmp, "index")); err != nil {
		return "", fmt.Errorf("could not copy .git/index: %s", err)
	}
	return t.snapshot(dir, tmp)
}

// repositoryPaths resolves a repository directory and its .git directory,
// refusing anything that is not a real directory: the work dir is writable by
// the agent, and a symlink would point this at host paths outside the Task.
func (t *fixTrees) repositoryPaths(dir string) (string, string, error) {
	if err := ensureRealRepositoryDir(t.workDirHost, dir); err != nil {
		return "", "", err
	}
	repo := filepath.Join(t.workDirHost, dir)
	gitDir := filepath.Join(repo, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return "", "", errors.New("no .git directory")
	}
	if !info.IsDir() {
		return "", "", errors.New(".git is not a plain directory")
	}
	objects := filepath.Join(gitDir, "objects")
	if info, err := os.Lstat(objects); err != nil || !info.IsDir() {
		return "", "", errors.New(".git/objects is not a plain directory")
	}
	return repo, gitDir, nil
}

// snapshot records the working tree as a tree object in the temporary object
// directory and returns its id.
func (t *fixTrees) snapshot(dir, tmp string) (string, error) {
	if _, err := t.run(dir, tmp, nil, "add", "-A"); err != nil {
		return "", err
	}
	out, err := t.run(dir, tmp, nil, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(out)
	if tree == "" {
		return "", errors.New("git write-tree printed no tree id")
	}
	return tree, nil
}

// diff is the patch from the before tree to the after tree, or an error when
// it is larger than maxFixDiffBytes.
func (t *fixTrees) diff(dir, tmp, before, after string) (string, error) {
	limit := &limitedBuffer{limit: maxFixDiffBytes}
	if _, err := t.run(dir, tmp, limit, "diff", "--no-color", "--no-ext-diff", "--no-textconv", before, after); err != nil {
		return "", err
	}
	if limit.overflow {
		return "", fmt.Errorf("the diff is larger than %d bytes", maxFixDiffBytes)
	}
	return limit.buf.String(), nil
}

// run runs one git command for a repository with the temporary environment.
// stdout goes to out when given, and is returned otherwise.
func (t *fixTrees) run(dir, tmp string, out io.Writer, args ...string) (string, error) {
	return t.runIsolated(dir, tmp, isolatedGit{stdout: out}, args...)
}

// isolatedGit is what one git command of runIsolated may point elsewhere; ""
// fields keep the fix trees' defaults.
type isolatedGit struct {
	// workTree is the command's work tree and directory; the repository
	// directory by default.
	workTree string
	// index is the command's index file; <tmp>/index by default.
	index string
	// env is added to the command's environment.
	env []string
	// secret is removed from the error's git output.
	secret string
	// stdout receives the command's output, which is returned when it is nil.
	stdout io.Writer
}

// runIsolated is run with the work tree, index, environment and output of o,
// bounded by t.ctx (when set) beside the per-command timeout. The repository's
// objects stay an ALTERNATE and its config is never read, whatever o says.
func (t *fixTrees) runIsolated(dir, tmp string, o isolatedGit, args ...string) (string, error) {
	repo, gitDir, err := t.repositoryPaths(dir)
	if err != nil {
		return "", err
	}
	workTree, index := repo, filepath.Join(tmp, "index")
	if o.workTree != "" {
		workTree = o.workTree
	}
	if o.index != "" {
		index = o.index
	}
	parent := t.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, fixTreesGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.git, append([]string{"-c", "safe.directory=*", "-C", workTree}, args...)...)
	// git runs helpers of its own (git-remote-https for a fetch): on timeout
	// the whole process group is killed, and the wait for its output is
	// bounded, so a stalled helper can't hold the command past its deadline.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = fixTreesGitWaitDelay
	cmd.Env = append(withoutGitEnv(os.Environ()),
		"GIT_DIR="+filepath.Join(tmp, "gitdir"),
		"GIT_WORK_TREE="+workTree,
		"GIT_INDEX_FILE="+index,
		"GIT_OBJECT_DIRECTORY="+filepath.Join(tmp, "gitdir", "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(gitDir, "objects"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
	cmd.Env = append(cmd.Env, o.env...)
	var stdout bytes.Buffer
	if o.stdout != nil {
		cmd.Stdout = o.stdout
	} else {
		cmd.Stdout = &stdout
	}
	stderr := &limitedBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", isolatedGitError(parent, ctx, args[0], err, scrubSecret(stderr.buf.String(), o.secret, stderr.overflow))
	}
	return stdout.String(), nil
}

// isolatedGitError says why the git command name failed: cancelled or timed
// out with parent, timed out on its own (ctx), or git's first line of stderr.
func isolatedGitError(parent, ctx context.Context, name string, err error, stderr string) error {
	switch {
	case errors.Is(parent.Err(), context.Canceled):
		return fmt.Errorf("git %s was cancelled", name)
	case parent.Err() != nil:
		return fmt.Errorf("git %s timed out", name)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("git %s timed out after %s", name, fixTreesGitTimeout)
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("git %s: %s: %s", name, err, firstLine(msg))
	}
	return fmt.Errorf("git %s: %s", name, err)
}

// scrubSecret removes every occurrence of secret from s, and its URL-escaped
// forms. When s was cut short (truncated), its last len(secret) bytes are
// dropped first: they could hold the start of an occurrence the cut split.
func scrubSecret(s, secret string, truncated bool) string {
	if secret == "" {
		return s
	}
	if truncated {
		if len(s) > len(secret) {
			s = s[:len(s)-len(secret)]
		} else {
			s = ""
		}
	}
	s = strings.ReplaceAll(s, secret, "***")
	if escaped := url.QueryEscape(secret); escaped != secret {
		s = strings.ReplaceAll(s, escaped, "***")
	}
	if escaped := url.PathEscape(secret); escaped != secret {
		s = strings.ReplaceAll(s, escaped, "***")
	}
	return s
}

// withoutGitEnv drops every inherited GIT_* variable, so nothing in the
// runner's own environment redirects these commands.
func withoutGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// copyRegularFileIfPresent copies src to dst when src is a regular file, and
// does nothing when it does not exist. Anything else at src is an error.
func copyRegularFileIfPresent(src, dst string) error {
	info, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(src))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// limitedBuffer keeps the first limit bytes written to it and notes whether
// anything past them was dropped. It never fails a write, so the command it
// collects from runs to the end rather than dying on a broken pipe.
type limitedBuffer struct {
	limit    int
	buf      bytes.Buffer
	overflow bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.limit - l.buf.Len(); room > 0 {
		if len(p) <= room {
			l.buf.Write(p)
		} else {
			l.buf.Write(p[:room])
			l.overflow = true
		}
	} else if len(p) > 0 {
		l.overflow = true
	}
	return len(p), nil
}

// stageFixDiffs writes the pending fix diffs for the round about to run under
// <work dir>/.review-fix and sets the round's REVIEW_FIX_DIFFS payload. The
// pending diffs are taken either way, so they can only ever reach the round
// that directly follows the fix that produced them. Round 1 never has any.
func (s *reviewStage) stageFixDiffs(round int) {
	pending := s.pendingFixDiffs
	s.pendingFixDiffs = nil
	s.roundFixDiffs = ""
	if round <= 1 || len(pending) == 0 {
		return
	}
	hostDir := filepath.Join(s.workDirHost, reviewFixDiffDirName)
	_ = os.RemoveAll(hostDir)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		for _, dir := range sortedRepositoryDirs(pending) {
			logNoFixDiff(s.logsWriter, dir, err.Error())
		}
		return
	}
	paths := map[string]string{}
	total := 0
	for _, dir := range sortedRepositoryDirs(pending) {
		name := strings.ReplaceAll(dir, "/", "__") + ".diff"
		if err := os.WriteFile(filepath.Join(hostDir, name), []byte(pending[dir]), 0o644); err != nil {
			logNoFixDiff(s.logsWriter, dir, err.Error())
			continue
		}
		paths[dir] = agentboxWorkDirInContainer + "/" + reviewFixDiffDirName + "/" + name
		total += len(pending[dir])
	}
	if len(paths) == 0 {
		return
	}
	encoded, err := json.Marshal(paths)
	if err != nil {
		io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: no fix diffs for round %d (%s) — it reviews the whole change\n", round, err))
		return
	}
	s.roundFixDiffs = string(encoded)
	noun := "repositories"
	if len(paths) == 1 {
		noun = "repository"
	}
	io.WriteString(s.logsWriter, fmt.Sprintf("Review round %d: fix diff for %d %s (%d bytes)\n", round, len(paths), noun, total))
}

// clearFixDiffs removes the round's fix diffs and its payload once the round
// has ended, on every path.
func (s *reviewStage) clearFixDiffs() {
	s.roundFixDiffs = ""
	_ = os.RemoveAll(filepath.Join(s.workDirHost, reviewFixDiffDirName))
}
