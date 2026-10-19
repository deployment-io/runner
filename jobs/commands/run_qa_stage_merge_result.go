package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// THE QA STAGE'S MERGE RESULT.
//
// What ships when the pull request merges is the base branch plus the change,
// and the base branch may have moved since the Task started. So during the QA
// stage a runner-built preview is built from the MERGE RESULT: the agent's
// working tree (uncommitted changes included) merged three-way with the base
// branch's latest commit, fetched at QA time. Implement and fix runs build the
// branch as built, exactly as before.
//
// NOTHING IS WRITTEN INTO THE REPOSITORY, and nothing of the repository's own
// git config runs: every git command here is the fix trees' (see
// run_review_stage_fix_trees.go) — a throwaway GIT_DIR with an empty config,
// a temporary index and object directory with the repository's objects as an
// ALTERNATE. The working tree is snapshotted as a commit whose parent is HEAD,
// the base branch is fetched into the temporary directory only, and the two
// are merged with git 2.20's plumbing (read-tree -m -u, then merge-index with
// git-merge-one-file, which runs no merge driver or filter) in a scratch work
// tree outside every repository. Each build then gets its own checkout of the
// merged tree in a new build directory beside the work dir. Nothing is pushed,
// and the pull request gets no merge commit.
//
// FAIL-OPEN. A conflict, a failed fetch, a git error or the timeout leaves
// that repository's previews built from the branch as built, and the reason is
// recorded on the round (merge_results) and shown on the pull request. QA is
// never failed or skipped because of it, and the QA agent is told nothing.

const (
	// qaMergeResultTimeout bounds one build's merge-result work for a
	// repository: snapshot, token lookup, fetch, merge and the build's
	// checkout together (a later build, which reuses the merge, has it for
	// its checkout alone).
	qaMergeResultTimeout = 2 * time.Minute
	// qaMergeTaskRef and qaMergeBaseRef are the temporary git directory's
	// refs: the snapshot commit (so the fetch negotiates from HEAD's history)
	// and the fetched base commit.
	qaMergeTaskRef = "refs/qa/task"
	qaMergeBaseRef = "refs/qa/base"
	// qaMergeErrorMaxRunes caps the git error a reason carries.
	qaMergeErrorMaxRunes = 200
)

// qaMergeResultOutput is what a QA round's previews of one repository were
// built from: the merge result, or the branch as built and why.
type qaMergeResultOutput struct {
	Repository string `json:"repository"`
	BaseBranch string `json:"base_branch"`
	// BaseCommit is the full fetched SHA, empty when the fetch failed.
	BaseCommit string `json:"base_commit"`
	Merged     bool   `json:"merged"`
	Reason     string `json:"reason,omitempty"`
}

// qaMergeResults is one QA round's merge results, made per repository on the
// first preview build that needs one and reused by the round's later builds.
// It is the round's agenttools.MergeResultProvider.
type qaMergeResults struct {
	workDirHost  string
	repositories []tasks.RepositoryEntry
	logsWriter   io.Writer
	// git is the git binary and tmpRoot the temporary directory's parent; ""
	// means "git" from PATH and the system default.
	git     string
	tmpRoot string
	timeout time.Duration
	// fetchSource is the URL the base branch is fetched from and the secret
	// it carries, which is scrubbed from everything git prints. It should
	// give up when ctx is done; fetchBase stops waiting for it then anyway.
	fetchSource func(ctx context.Context, entry tasks.RepositoryEntry) (string, string, error)

	// mu is held for reading by every checkout and for writing by remove, so
	// the temporary directory is never removed under a build.
	mu     sync.RWMutex
	closed bool
	// reposMu guards root and repos.
	reposMu sync.Mutex
	root    string
	repos   map[string]*qaMergeRepository
}

// qaMergeRepository is one repository's merge result.
type qaMergeRepository struct {
	index int
	dir   string // "<idx>-<name>"
	entry tasks.RepositoryEntry
	tmp   string // its temporary git directory's parent, under root
	once  sync.Once
	tree  string // the merged tree, "" when the merge result couldn't be made
	// setupErr is why its temporary directory couldn't be made, if it wasn't.
	setupErr error
	// recordMu guards record, which a failed checkout can change.
	recordMu sync.Mutex
	record   qaMergeResultOutput
}

// qaMergeConflict is a merge that left unresolved paths.
type qaMergeConflict struct{ files int }

func (e qaMergeConflict) Error() string { return fmt.Sprintf("%d conflicting files", e.files) }

// qaFetchFailed is a base branch that couldn't be fetched.
type qaFetchFailed struct{ err error }

func (e qaFetchFailed) Error() string { return e.err.Error() }

// CheckoutMergeResult implements agenttools.MergeResultProvider: it makes the
// repository's merge result once per round, then checks it out into dir.
func (m *qaMergeResults) CheckoutMergeResult(ctx context.Context, repoDir, dir string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return false
	}
	r, err := m.repository(repoDir)
	if err != nil {
		io.WriteString(m.logsWriter, fmt.Sprintf("QA: the preview of %s builds from the branch as built (%s)\n", repoDir, err))
		return false
	}
	// One deadline covers this build's merge-result work: making the merge
	// (on the round's first build of the repository) and checking it out.
	timeout := m.mergeTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r.once.Do(func() { m.make(ctx, r, timeout) })
	if r.tree == "" {
		return false
	}
	if err := m.checkout(ctx, r, dir); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		reason := qaMergeReason(r.entry.BaseBranch, err)
		r.recordMu.Lock()
		r.record.Merged, r.record.Reason = false, reason
		r.recordMu.Unlock()
		io.WriteString(m.logsWriter, fmt.Sprintf("QA: the preview of %s builds from the branch as built: %s\n", repoDir, reason))
		return false
	}
	return true
}

// repository is repoDir's entry, creating the temporary directory on first use.
func (m *qaMergeResults) repository(repoDir string) (*qaMergeRepository, error) {
	m.reposMu.Lock()
	defer m.reposMu.Unlock()
	if r, ok := m.repos[repoDir]; ok {
		return r, nil
	}
	index := -1
	for i, e := range m.repositories {
		if fmt.Sprintf("%d-%s", i, e.Name) == repoDir {
			index = i
		}
	}
	if index < 0 {
		return nil, errors.New("not one of the Task's repositories")
	}
	if m.repos == nil {
		m.repos = map[string]*qaMergeRepository{}
	}
	entry := m.repositories[index]
	r := &qaMergeRepository{
		index:  index,
		dir:    repoDir,
		entry:  entry,
		record: qaMergeResultOutput{Repository: entry.Name, BaseBranch: entry.BaseBranch},
	}
	// The repository is recorded even when its temporary directory can't be
	// made: its previews then build as built, and the record says why.
	m.repos[repoDir] = r
	if m.root == "" {
		root, err := os.MkdirTemp(m.tmpRoot, "qa-merge-result-")
		if err != nil {
			r.setupErr = fmt.Errorf("could not create a temporary directory: %s", err)
			return r, nil
		}
		m.root = root
	}
	r.tmp = filepath.Join(m.root, fmt.Sprintf("repo-%d", index))
	return r, nil
}

// mergeTimeout is the deadline of one build's merge-result work.
func (m *qaMergeResults) mergeTimeout() time.Duration {
	if m.timeout > 0 {
		return m.timeout
	}
	return qaMergeResultTimeout
}

// make makes r's merge result within ctx, whose deadline is timeout, and
// records the outcome.
func (m *qaMergeResults) make(ctx context.Context, r *qaMergeRepository, timeout time.Duration) {
	var result qaMerge
	err := r.setupErr
	if err == nil {
		result, err = m.merge(ctx, r)
	}
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	r.record.BaseCommit = result.baseCommit
	if err == nil {
		r.tree = result.tree
		r.record.Merged = true
		io.WriteString(m.logsWriter, fmt.Sprintf("QA: previews of %s build from it merged with %s at %s\n", r.dir, r.entry.BaseBranch, shortCommit(r.record.BaseCommit)))
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	r.record.Reason = qaMergeReason(r.entry.BaseBranch, err)
	io.WriteString(m.logsWriter, fmt.Sprintf("QA: previews of %s build from the branch as built: %s (%s)\n", r.dir, r.record.Reason, err))
}

// qaMergeReason is the recorded reason for err.
func qaMergeReason(base string, err error) string {
	var conflict qaMergeConflict
	var fetch qaFetchFailed
	switch {
	case errors.As(err, &conflict):
		return fmt.Sprintf("the branch doesn't merge cleanly with %s (%d conflicting files)", base, conflict.files)
	case errors.As(err, &fetch):
		return fmt.Sprintf("%s couldn't be fetched", base)
	}
	return fmt.Sprintf("the merge couldn't be made (%s)", capRunes(err.Error(), qaMergeErrorMaxRunes))
}

// qaMerge is a merge's outcome: the fetched base commit ("" when the fetch
// failed) and the merged tree ("" when the merge failed).
type qaMerge struct {
	baseCommit string
	tree       string
}

// merge snapshots the working tree as a commit on HEAD, fetches the base
// branch and merges the two.
func (m *qaMergeResults) merge(ctx context.Context, r *qaMergeRepository) (qaMerge, error) {
	trees := m.trees(ctx)
	tree, err := trees.prepare(r.dir, r.tmp)
	if err != nil {
		return qaMerge{}, err
	}
	_, gitDir, err := trees.repositoryPaths(r.dir)
	if err != nil {
		return qaMerge{}, err
	}
	head, err := repositoryHead(gitDir)
	if err != nil {
		return qaMerge{}, err
	}
	scratch := filepath.Join(r.tmp, "work")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return qaMerge{}, err
	}
	o := isolatedGit{workTree: scratch, env: []string{
		"GIT_AUTHOR_NAME=" + commitAuthorName, "GIT_AUTHOR_EMAIL=" + commitAuthorEmail,
		"GIT_COMMITTER_NAME=" + commitAuthorName, "GIT_COMMITTER_EMAIL=" + commitAuthorEmail,
	}}
	out, err := trees.runIsolated(r.dir, r.tmp, o, "commit-tree", tree, "-p", head, "-m", "The Task's working tree")
	if err != nil {
		return qaMerge{}, err
	}
	ours := strings.TrimSpace(out)
	if _, err := trees.runIsolated(r.dir, r.tmp, o, "update-ref", qaMergeTaskRef, ours); err != nil {
		return qaMerge{}, err
	}
	theirs, err := m.fetchBase(trees, r, scratch)
	if err != nil {
		return qaMerge{}, err
	}
	result := qaMerge{baseCommit: theirs}
	out, err = trees.runIsolated(r.dir, r.tmp, o, "merge-base", ours, theirs)
	if err != nil {
		return result, errors.New("the branch and the base branch have no common history")
	}
	result.tree, err = mergeTrees(trees, r, strings.TrimSpace(out))
	return result, err
}

// fetchBase fetches the latest commit of r's base branch into the temporary
// git directory only and returns its SHA. Every failure is a qaFetchFailed,
// with the secret scrubbed from it.
func (m *qaMergeResults) fetchBase(trees *fixTrees, r *qaMergeRepository, scratch string) (string, error) {
	base := r.entry.BaseBranch
	if !validBranchName(base) {
		return "", qaFetchFailed{fmt.Errorf("the base branch %q isn't a branch name", base)}
	}
	source, secret, err := m.lookupFetchSource(trees.ctx, r.entry)
	if err != nil {
		return "", qaFetchFailed{errors.New(scrubSecret(err.Error(), secret, false))}
	}
	if source == "" || strings.HasPrefix(source, "-") {
		return "", qaFetchFailed{errors.New("the repository has no URL to fetch from")}
	}
	o := isolatedGit{workTree: scratch, secret: secret}
	refspec := "+refs/heads/" + base + ":" + qaMergeBaseRef
	_, err = trees.runIsolated(r.dir, r.tmp, o, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", source, refspec)
	// FETCH_HEAD notes where the fetch came from; it is no use here.
	_ = os.Remove(filepath.Join(r.tmp, "gitdir", "FETCH_HEAD"))
	if err != nil {
		return "", qaFetchFailed{err}
	}
	out, err := trees.runIsolated(r.dir, r.tmp, o, "rev-parse", "--verify", "--quiet", qaMergeBaseRef+"^{commit}")
	if err != nil {
		return "", qaFetchFailed{err}
	}
	return strings.TrimSpace(out), nil
}

// lookupFetchSource is fetchSource bounded by ctx: a lookup that stalls (a
// token refresh that keeps retrying) is left behind when ctx is done, so the
// merge still falls back within its timeout.
func (m *qaMergeResults) lookupFetchSource(ctx context.Context, entry tasks.RepositoryEntry) (string, string, error) {
	type lookup struct {
		source, secret string
		err            error
	}
	done := make(chan lookup, 1)
	go func() {
		source, secret, err := m.fetchSource(ctx, entry)
		done <- lookup{source, secret, err}
	}()
	select {
	case l := <-done:
		return l.source, l.secret, l.err
	case <-ctx.Done():
		return "", "", errors.New("the installation token lookup didn't finish in time")
	}
}

// mergeTrees merges the snapshot commit (qaMergeTaskRef) and the base commit
// (qaMergeBaseRef) at mergeBase in a scratch work tree with a scratch index,
// both removed afterwards, and returns the merged tree. Any path left
// unresolved is a qaMergeConflict.
func mergeTrees(trees *fixTrees, r *qaMergeRepository, mergeBase string) (string, error) {
	scratch := filepath.Join(r.tmp, "work")
	index := filepath.Join(r.tmp, "merge-index")
	defer os.RemoveAll(scratch)
	defer os.Remove(index)
	o := isolatedGit{workTree: scratch, index: index}
	for _, args := range [][]string{
		{"read-tree", qaMergeTaskRef},
		{"checkout-index", "--all", "--force", "-u"},
		{"read-tree", "-m", "-u", "--aggressive", mergeBase, qaMergeTaskRef, qaMergeBaseRef},
	} {
		if _, err := trees.runIsolated(r.dir, r.tmp, o, args...); err != nil {
			return "", err
		}
	}
	unmerged, err := unmergedPaths(trees, r, o)
	if err != nil {
		return "", err
	}
	if unmerged > 0 {
		if err := hasMergeOneFile(trees, r, o); err != nil {
			return "", err
		}
		// merge-index fails when a file doesn't merge; what is left unmerged
		// afterwards says which.
		_, mergeErr := trees.runIsolated(r.dir, r.tmp, o, "merge-index", "-o", "git-merge-one-file", "-a")
		if unmerged, err = unmergedPaths(trees, r, o); err != nil {
			return "", err
		}
		if unmerged > 0 {
			return "", qaMergeConflict{files: unmerged}
		}
		if mergeErr != nil {
			return "", mergeErr
		}
	}
	if err := hasNoSubmodules(trees, r, o); err != nil {
		return "", err
	}
	out, err := trees.runIsolated(r.dir, r.tmp, o, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// hasNoSubmodules fails when the merged index holds a submodule: a checkout
// of the merged tree leaves a submodule's directory empty, where the branch
// as built has its files, so such a repository builds as built.
func hasNoSubmodules(trees *fixTrees, r *qaMergeRepository, o isolatedGit) error {
	out, err := trees.runIsolated(r.dir, r.tmp, o, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(out, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return errors.New("the repository has submodules")
		}
	}
	return nil
}

// unmergedPaths counts the paths the index holds unmerged.
func unmergedPaths(trees *fixTrees, r *qaMergeRepository, o isolatedGit) (int, error) {
	out, err := trees.runIsolated(r.dir, r.tmp, o, "ls-files", "--unmerged", "-z")
	if err != nil {
		return 0, err
	}
	paths := map[string]bool{}
	for _, entry := range strings.Split(out, "\x00") {
		if _, path, ok := strings.Cut(entry, "\t"); ok {
			paths[path] = true
		}
	}
	return len(paths), nil
}

// hasMergeOneFile checks git's exec path has git-merge-one-file, which
// merge-index runs: without it every file to merge would read as a conflict.
func hasMergeOneFile(trees *fixTrees, r *qaMergeRepository, o isolatedGit) error {
	out, err := trees.runIsolated(r.dir, r.tmp, o, "--exec-path")
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(out), "git-merge-one-file")); err != nil {
		return errors.New("git-merge-one-file isn't installed")
	}
	return nil
}

// checkout writes r's merged tree into dir with an index of its own, so one
// build's checkout can't affect another's.
func (m *qaMergeResults) checkout(ctx context.Context, r *qaMergeRepository, dir string) error {
	f, err := os.CreateTemp(r.tmp, "build-index-")
	if err != nil {
		return err
	}
	index := f.Name()
	f.Close()
	// git writes the index itself: an empty file is not one.
	_ = os.Remove(index)
	defer os.Remove(index)
	trees := m.trees(ctx)
	o := isolatedGit{workTree: dir, index: index}
	if _, err := trees.runIsolated(r.dir, r.tmp, o, "read-tree", r.tree); err != nil {
		return err
	}
	_, err = trees.runIsolated(r.dir, r.tmp, o, "checkout-index", "--all", "--force")
	return err
}

// trees is the fix trees' isolated git, bounded by ctx.
func (m *qaMergeResults) trees(ctx context.Context) *fixTrees {
	git := m.git
	if git == "" {
		git = "git"
	}
	return &fixTrees{workDirHost: m.workDirHost, logsWriter: m.logsWriter, git: git, ctx: ctx}
}

// remove deletes the temporary directory once every checkout has finished; a
// later checkout builds the branch as built. Safe on a nil receiver.
func (m *qaMergeResults) remove() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.reposMu.Lock()
	defer m.reposMu.Unlock()
	if m.root != "" {
		_ = os.RemoveAll(m.root)
	}
}

// results is one entry per repository a preview was built of, in work-dir
// order; nil when none was.
func (m *qaMergeResults) results() []qaMergeResultOutput {
	if m == nil {
		return nil
	}
	m.reposMu.Lock()
	defer m.reposMu.Unlock()
	var out []qaMergeResultOutput
	for i := range m.repositories {
		for _, r := range m.repos {
			if r.index != i {
				continue
			}
			r.recordMu.Lock()
			out = append(out, r.record)
			r.recordMu.Unlock()
		}
	}
	return out
}

var objectIDPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// repositoryHead reads the commit HEAD names from the repository's .git
// directory without running git there: a detached HEAD, or a branch's loose
// or packed ref. Only regular files are read.
func repositoryHead(gitDir string) (string, error) {
	head, err := readRegularFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", errors.New("could not read HEAD")
	}
	ref, symbolic := strings.CutPrefix(head, "ref: ")
	if !symbolic {
		return objectID(head)
	}
	if !strings.HasPrefix(ref, "refs/") || !filepath.IsLocal(ref) || strings.Contains(ref, "..") {
		return "", errors.New("HEAD names no branch")
	}
	if id, err := readRegularFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		return objectID(id)
	}
	packed, err := readRegularFile(filepath.Join(gitDir, "packed-refs"))
	if err == nil {
		for _, line := range strings.Split(packed, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[1] == ref {
				return objectID(fields[0])
			}
		}
	}
	return "", errors.New("HEAD names no commit")
}

func objectID(s string) (string, error) {
	if !objectIDPattern.MatchString(s) {
		return "", errors.New("HEAD names no commit")
	}
	return s, nil
}

// readRegularFile is the trimmed content of path when it is a regular file.
func readRegularFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// validBranchName is a conservative check of a branch name before it goes into
// a refspec: no option, no refspec syntax, no "..".
func validBranchName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range name {
		if c <= ' ' || c == 0x7f || strings.ContainsRune(":~^?*[\\", c) {
			return false
		}
	}
	return true
}

// shortCommit is the first 7 characters of a commit SHA.
func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// newQAMergeResults is a QA round's merge-result provider. The base branch is
// fetched with an installation token obtained the way CommitAndPush obtains
// its own.
func (s *qaStage) newQAMergeResults() *qaMergeResults {
	fetchSource := s.mergeFetchSource
	if fetchSource == nil {
		tcp := &taskCommitPush{ctx: s.ctx, tokenCache: map[string]string{}, logsWriter: s.logsWriter}
		// One lookup at a time shares tcp's token cache; waiting for the turn
		// gives up with ctx, so a stalled lookup can't hold up another
		// repository past its own timeout. The lookup itself stops polling a
		// refresh in process once ctx is done, so it ends with the merge.
		turn := make(chan struct{}, 1)
		fetchSource = func(ctx context.Context, entry tasks.RepositoryEntry) (string, string, error) {
			select {
			case turn <- struct{}{}:
			case <-ctx.Done():
				return "", "", errors.New("the installation token lookup didn't finish in time")
			}
			token, err := tcp.getTokenContext(ctx, entry.InstallationID)
			<-turn
			if err != nil {
				return "", "", err
			}
			source, err := commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
			return source, token, err
		}
	}
	return &qaMergeResults{
		workDirHost:  s.workDirHost,
		repositories: s.ctx.Entries,
		logsWriter:   s.logsWriter,
		git:          s.fixTreesGit,
		tmpRoot:      s.fixTreesTmpRoot,
		fetchSource:  fetchSource,
	}
}
