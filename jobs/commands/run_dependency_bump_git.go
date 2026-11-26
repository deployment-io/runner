package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/jobs/commands/dependencies"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// The dependency bump's git work, all in the runner process with go-git: the
// token is in http.BasicAuth, never in a URL or a config.

// getToken is the dependent's installation token, asked for once.
func (b *dependencyBump) getToken() (string, error) {
	if b.token != "" {
		return b.token, nil
	}
	return b.fetchToken("")
}

// fetchToken asks for a token; rejected is the one the provider just refused,
// or "".
func (b *dependencyBump) fetchToken(rejected string) (string, error) {
	lookup := b.cmd.token
	if lookup == nil {
		lookup = commandUtils.RefreshGitTokenForInstallationContext
	}
	token, err := lookup(context.Background(), b.entry.InstallationID, b.in.orgID, rejected)
	if err != nil {
		return "", fmt.Errorf("couldn't get a token for %s: %w", b.entry.Name, err)
	}
	b.token = token
	b.tokens = append(b.tokens, token)
	return token, nil
}

// withToken runs op with the token, and once more with a refreshed one when
// the provider asks for authentication.
func (b *dependencyBump) withToken(op func(auth *http.BasicAuth) error) error {
	token, err := b.getToken()
	if err != nil {
		return err
	}
	username := commandUtils.GetUsernameForProvider(b.entry.Provider)
	err = op(&http.BasicAuth{Username: username, Password: token})
	if !commandUtils.IsErrorAuthenticationRequired(err) {
		return err
	}
	if token, err = b.fetchToken(token); err != nil {
		return err
	}
	return op(&http.BasicAuth{Username: username, Password: token})
}

// checkout is step 2: the dependent's Task branch, from its open pull
// request's branch, or the branch pushed earlier without one, or created from
// the base branch. A merged or closed pull request, or one that couldn't be
// looked up, fails every target: nothing is pushed.
func (b *dependencyBump) checkout() bool {
	name := b.entry.Name
	open, ok := b.checkPR()
	if !ok {
		return false
	}
	b.attemptState.prOpen = open
	if err := b.cloneDependent(); err != nil {
		b.logf("couldn't check out %s: %s", name, err)
		b.failOpen(fmt.Sprintf("couldn't check out %s: %s", name, err))
		return false
	}
	return true
}

// checkPR looks up the dependent's pull request: open is true when it's
// open. A merged or closed pull request, or one that couldn't be looked up,
// fails every target, and ok is false.
func (b *dependencyBump) checkPR() (open, ok bool) {
	name := b.entry.Name
	pr, err := b.lookupPR()(b.in.orgID, b.entry.InstallationID, name, b.in.branch)
	switch {
	case err != nil:
		b.logf("couldn't look up %s's pull request: %s", name, err)
		b.failOpen(fmt.Sprintf("couldn't look up %s's pull request", name))
		return false, false
	case pr.Found && pr.State == "open":
		b.logf("%s's pull request #%d is open on %s", name, pr.PRNumber, b.in.branch)
		return true, true
	case pr.Found && pr.State == "closed" && pr.Merged:
		b.failOpen(fmt.Sprintf("%s's pull request has merged; bump it in a new Task", name))
		return false, false
	case pr.Found && pr.State == "closed":
		b.failOpen(fmt.Sprintf("%s's pull request was closed", name))
		return false, false
	case pr.Found:
		b.logf("%s's pull request is %q", name, pr.State)
		b.failOpen(fmt.Sprintf("couldn't look up %s's pull request", name))
		return false, false
	}
	return false, true
}

// recheckPR looks the pull request up again just before the push: one that
// merged or closed while bumping fails every target, since merging needn't
// move the branch and the push would succeed onto a finished pull request.
// One opened meanwhile is used as found open in step 2.
func (b *dependencyBump) recheckPR() bool {
	open, ok := b.checkPR()
	if !ok {
		return false
	}
	b.attemptState.prOpen = b.attemptState.prOpen || open
	return true
}

// cloneDependent clones the dependent into repo/ and checks out the Task
// branch: the remote one when there is a pull request or the branch exists,
// else a new one from the base branch.
func (b *dependencyBump) cloneDependent() error {
	var repository *git.Repository
	err := b.withToken(func(auth *http.BasicAuth) error {
		var err error
		repository, err = commandUtils.CloneRepository(b.repoDir(), b.entry.CloneURL, auth.Password, b.entry.Provider, b.log)
		return err
	})
	if err != nil {
		return err
	}
	if err := commandUtils.ScrubRemoteToken(repository, b.entry.CloneURL); err != nil {
		return err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return err
	}
	b.attemptState.repository = repository
	remoteRef := plumbing.NewRemoteReferenceName("origin", b.in.branch)
	if b.attemptState.prOpen {
		err := b.withToken(func(auth *http.BasicAuth) error {
			return commandUtils.FetchRepository(repository, b.entry.CloneURL, auth.Password, b.entry.Provider, b.log)
		})
		if err != nil {
			return err
		}
	}
	if _, err := repository.Reference(remoteRef, true); err == nil {
		if !b.attemptState.prOpen {
			b.logf("%s has no pull request, but its branch %s exists; bumping on top of it", b.entry.Name, b.in.branch)
		}
		return checkoutRemoteBranch(repository, worktree, b.in.branch)
	} else if b.attemptState.prOpen {
		return fmt.Errorf("its pull request's branch %s isn't on the remote", b.in.branch)
	}
	if b.entry.BaseBranch == "" {
		return errors.New("it has no base branch configured")
	}
	b.logf("%s has no pull request; creating %s from %s", b.entry.Name, b.in.branch, b.entry.BaseBranch)
	return checkoutBaseBranchAndCreateTaskBranch(worktree, b.entry.BaseBranch, b.in.branch)
}

// checkoutRemoteBranch makes origin/<branch> the local branch and checks it
// out, as fetchAndCheckoutTaskBranch does (not a detached HEAD, so a commit
// moves the branch that is pushed).
func checkoutRemoteBranch(repository *git.Repository, worktree *git.Worktree, branch string) error {
	remoteRef, err := repository.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return fmt.Errorf("error resolving %s: %s", branch, err)
	}
	local := plumbing.NewBranchReferenceName(branch)
	if err := repository.Storer.SetReference(plumbing.NewHashReference(local, remoteRef.Hash())); err != nil {
		return fmt.Errorf("error creating the local branch %s: %s", branch, err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: local}); err != nil {
		return fmt.Errorf("error checking out %s: %s", branch, err)
	}
	return nil
}

// bumpMirror is a private repository mirrored for the containers, at
// mirrors/<dir>.
type bumpMirror struct {
	ref     dependencies.RepositoryRef
	dir     string
	url     string
	head    string
	fetched bool
	// aliases are the other spellings of the repository's name that were
	// asked for (another case, or with ".git"): the insteadOf rule maps a
	// URL onto /mirrors/ as it is, and the container's filesystem tells
	// them apart.
	aliases []string
}

func (m *bumpMirror) alias(name string) {
	for _, a := range m.aliases {
		if a == name {
			return
		}
	}
	m.aliases = append(m.aliases, name)
}

func (b *dependencyBump) mirrorDir(name string) string {
	return filepath.Join(b.workDir, "mirrors", name)
}

// addMirror records a repository to mirror, once: names that differ only in
// case or a ".git" suffix are the same repository.
func (b *dependencyBump) addMirror(ref dependencies.RepositoryRef) *bumpMirror {
	name := strings.TrimSuffix(ref.Name, ".git")
	key := strings.ToLower(name)
	if m, ok := b.attemptState.mirrors[key]; ok {
		m.alias(ref.Name)
		return m
	}
	m := &bumpMirror{ref: ref, dir: name, url: "https://" + ref.Host + "/" + ref.Owner + "/" + name}
	// A Task repository is fetched from its clone URL.
	for _, entry := range b.in.repos {
		if r, ok := cloneURLRef(entry.CloneURL); ok && b.scope.contains(r) && strings.EqualFold(r.Name, name) {
			m.ref, m.dir, m.url, m.head = r, r.Name, entry.CloneURL, entry.BaseBranch
			break
		}
	}
	m.alias(ref.Name)
	b.attemptState.mirrors[key] = m
	b.attemptState.mirrorOrd = append(b.attemptState.mirrorOrd, key)
	return m
}

// addInScope records a mirror for every name of m that a repository in the
// org scope provides.
func (b *dependencyBump) addInScope(eco dependencies.Ecosystem, m dependencies.Manifest) {
	names := make([]string, 0, len(m.Requires))
	for name := range m.Requires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ref, ok := eco.Source(name); ok && b.scope.contains(ref) {
			b.addMirror(ref)
		}
	}
}

// tooManyMirrors fails every target when the bump needs more mirrors than it
// may fetch.
func (b *dependencyBump) tooManyMirrors() bool {
	if len(b.attemptState.mirrors) <= dependencyBumpMaxMirrors {
		return false
	}
	b.failOpen(fmt.Sprintf("the bump needs more than %d private repositories", dependencyBumpMaxMirrors))
	return true
}

// fetchMirrors is step 4: each target's library, every private module the
// dependent requires, and every private module each library requires at its
// merged commit, mirrored bare under mirrors/.
func (b *dependencyBump) fetchMirrors() bool {
	sources := map[*bumpTarget]*bumpMirror{}
	for _, t := range b.targets {
		if t.open() {
			ref, _ := cloneURLRef(b.in.repos[t.Source].CloneURL)
			sources[t] = b.addMirror(ref)
		}
	}
	for _, g := range b.groups() {
		b.addInScope(g.ecosystem, b.attemptState.before[g.ecosystem.Name()])
	}
	if b.tooManyMirrors() {
		return false
	}
	for _, t := range b.targets {
		if m := sources[t]; m != nil && !b.fetchMirror(m) {
			return false
		}
	}
	for _, t := range b.targets {
		if m := sources[t]; m != nil && t.open() {
			b.readLibrary(t, m)
		}
	}
	if b.tooManyMirrors() {
		return false
	}
	for _, key := range b.attemptState.mirrorOrd {
		if !b.fetchMirror(b.attemptState.mirrors[key]) {
			return false
		}
	}
	for _, key := range b.attemptState.mirrorOrd {
		m := b.attemptState.mirrors[key]
		if err := b.linkMirror(m); err != nil {
			b.failOpen(fmt.Sprintf("couldn't fetch %s/%s/%s: %s", m.ref.Host, m.ref.Owner, m.ref.Name, err))
			return false
		}
	}
	return true
}

// linkMirror points every other spelling of m's name at mirrors/<dir>: some
// hosts (GitLab) advertise their repositories with ".git", and a module path
// may spell the repository in another case than its clone URL does.
func (b *dependencyBump) linkMirror(m *bumpMirror) error {
	links := []string{m.dir + ".git"}
	for _, a := range m.aliases {
		name := strings.TrimSuffix(a, ".git")
		links = append(links, name, name+".git")
	}
	done := map[string]bool{m.dir: true}
	for _, link := range links {
		if done[link] {
			continue
		}
		done[link] = true
		// On a case-insensitive filesystem the spelling already resolves.
		if _, err := os.Lstat(b.mirrorDir(link)); err == nil {
			continue
		}
		if err := os.Symlink(m.dir, b.mirrorDir(link)); err != nil {
			return err
		}
	}
	return nil
}

// fetchMirror fetches m once: every branch and every tag into a bare
// repository whose config names only the plain URL. A failure fails every
// target.
func (b *dependencyBump) fetchMirror(m *bumpMirror) bool {
	if m.fetched {
		return true
	}
	name := m.ref.Host + "/" + m.ref.Owner + "/" + m.ref.Name
	if err := b.mirror(m); err != nil {
		b.failOpen(fmt.Sprintf("couldn't fetch %s: %s", name, err))
		return false
	}
	m.fetched = true
	b.logf("mirrored %s", name)
	return true
}

func (b *dependencyBump) mirror(m *bumpMirror) error {
	repository, err := git.PlainInit(b.mirrorDir(m.dir), true)
	if err != nil {
		return err
	}
	if _, err := repository.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{m.url}}); err != nil {
		return err
	}
	err = b.withToken(func(auth *http.BasicAuth) error {
		err := repository.Fetch(&git.FetchOptions{
			RemoteName: "origin",
			RemoteURL:  m.url,
			Auth:       auth,
			RefSpecs:   []config.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
			Tags:       git.NoTags,
			Force:      true,
		})
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	// HEAD names the default branch when it's known, so the mirror reads as
	// its repository does.
	for _, head := range []string{m.head, "main", "master"} {
		if head == "" {
			continue
		}
		if _, err := repository.Reference(plumbing.NewBranchReferenceName(head), false); err == nil {
			return repository.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(head)))
		}
	}
	return nil
}

// readLibrary checks that t's merged commit is in its mirror and reads the
// library's manifest at it, through the adapter, from a local clone of the
// mirror in sources/<name> (no network, no token). Every private module it
// requires is mirrored too.
func (b *dependencyBump) readLibrary(t *bumpTarget, m *bumpMirror) {
	short := t.Commit[:dependencyBumpShortCommit]
	library := b.in.repos[t.Source].Name
	mirrorRepo, err := git.PlainOpen(b.mirrorDir(m.dir))
	if err == nil {
		_, err = mirrorRepo.CommitObject(plumbing.NewHash(t.Commit))
	}
	if err != nil {
		b.fail(t, fmt.Sprintf("the merged commit %s isn't in %s", short, library))
		return
	}
	dir := filepath.Join(b.workDir, "sources", m.dir)
	source, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		source, err = git.PlainClone(dir, false, &git.CloneOptions{URL: b.mirrorDir(m.dir), NoCheckout: true, Tags: git.AllTags})
	}
	if err == nil {
		var worktree *git.Worktree
		if worktree, err = source.Worktree(); err == nil {
			err = worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(t.Commit), Force: true})
		}
	}
	if err != nil {
		b.fail(t, fmt.Sprintf("couldn't check out %s at %s: %s", library, short, err))
		return
	}
	manifest, ok, err := t.ecosystem.ReadManifest(dir)
	if err != nil {
		b.fail(t, fmt.Sprintf("couldn't read %s's manifest at %s: %s", library, short, err))
		return
	}
	if ok {
		b.addInScope(t.ecosystem, manifest)
	}
}

// worktree is the dependent's worktree, with the ignore patterns collected
// with git's semantics as commit_and_push uses them (gitignore.go), so status
// and staging agree with git.
func (b *dependencyBump) worktree() (*git.Worktree, error) {
	if b.attemptState.worktree != nil {
		return b.attemptState.worktree, nil
	}
	worktree, err := b.attemptState.repository.Worktree()
	if err != nil {
		return nil, err
	}
	worktree.Excludes = append(worktree.Excludes, gitSemanticsPatterns(worktree.Filesystem)...)
	b.attemptState.worktree = worktree
	return worktree, nil
}

// changedPaths is every path the worktree status shows changed, sorted.
func (b *dependencyBump) changedPaths() ([]string, error) {
	worktree, err := b.worktree()
	if err != nil {
		return nil, err
	}
	status, err := worktree.Status()
	if err != nil {
		return nil, err
	}
	var paths []string
	for path, s := range status {
		if s.Worktree != git.Unmodified || s.Staging != git.Unmodified {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// checkChanges is step 6: a changed path no ecosystem that ran allows fails
// every target, and no change at all means every target is already current.
// Nothing is pushed in either case.
func (b *dependencyBump) checkChanges() bool {
	paths, err := b.changedPaths()
	if err != nil {
		b.failOpen(fmt.Sprintf("couldn't read %s's changes: %s", b.entry.Name, err))
		return false
	}
	var disallowed []string
	for _, path := range paths {
		allowed := false
		for _, eco := range b.attemptState.ran {
			allowed = allowed || eco.Allowed(path)
		}
		if !allowed {
			disallowed = append(disallowed, path)
		}
	}
	if len(disallowed) > 0 {
		if len(disallowed) > 5 {
			disallowed = disallowed[:5]
		}
		names := make([]string, 0, len(b.attemptState.ran))
		for _, eco := range b.attemptState.ran {
			names = append(names, eco.Name())
		}
		b.failOpen(fmt.Sprintf("the bump changed %s, which a %s bump may not change", strings.Join(disallowed, ", "), strings.Join(names, " or ")))
		return false
	}
	if len(paths) == 0 {
		b.logf("the bump changed nothing in %s", b.entry.Name)
		for _, t := range b.targets {
			if t.open() {
				t.result.State = tasks.DependencyBumpStateAlreadyCurrent
				t.result.ToVersion = t.result.FromVersion
			}
		}
		return false
	}
	b.logf("the bump changed %s in %s", strings.Join(paths, ", "), b.entry.Name)
	return true
}

// commitMessage is the bump commit's subject and message.
func (b *dependencyBump) commitMessage() (subject, message string) {
	bumped := b.bumped()
	var body strings.Builder
	if len(bumped) == 1 {
		t := bumped[0]
		subject = fmt.Sprintf("Bump %s to %s", t.ecosystem.Describe(t.Name), t.Commit[:dependencyBumpShortCommit])
	} else {
		subject = fmt.Sprintf("Bump %d dependencies", len(bumped))
		for _, t := range bumped {
			fmt.Fprintf(&body, "Bump %s to %s (%s → %s)\n", t.ecosystem.Describe(t.Name), t.Commit[:dependencyBumpShortCommit], t.result.FromVersion, t.result.ToVersion)
		}
	}
	var sb strings.Builder
	sb.WriteString(subject + "\n")
	if body.Len() > 0 {
		sb.WriteString("\n" + body.String())
	}
	sb.WriteString("\n" + commitTrailerTag + "\n")
	if b.in.taskTitle != "" {
		sb.WriteString(fmt.Sprintf("Task: %s\n", b.in.taskTitle))
	}
	if b.in.dashboardURL != "" {
		sb.WriteString(fmt.Sprintf("Task-URL: %s/tasks/%s\n", strings.TrimRight(b.in.dashboardURL, "/"), b.in.taskID))
	}
	return subject, sb.String()
}

// commitAndPush is step 9: one commit of every change, pushed non-forced onto
// the Task branch. moved is true when the push found the branch moved.
func (b *dependencyBump) commitAndPush() (pushed, moved bool) {
	repository := b.attemptState.repository
	subject, message := b.commitMessage()
	hash, err := b.commit(message)
	if err != nil {
		b.failOpen(fmt.Sprintf("couldn't commit the bump: %s", err))
		return false, false
	}
	refSpec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", b.in.branch, b.in.branch))
	err = b.withToken(func(auth *http.BasicAuth) error {
		// RemoteURL, so nothing in .git/config decides where the push goes.
		return repository.Push(&git.PushOptions{RemoteName: "origin", RemoteURL: b.entry.CloneURL, RefSpecs: []config.RefSpec{refSpec}, Auth: auth})
	})
	if err != nil && (strings.HasPrefix(err.Error(), "non-fast-forward update") || errors.Is(err, git.ErrForceNeeded)) {
		return false, true
	}
	if err != nil {
		b.failOpen(fmt.Sprintf("couldn't push %s: %s", b.entry.Name, b.scrub(err.Error())))
		return false, false
	}
	b.attemptState.commitSHA = hash
	b.attemptState.subject = subject
	b.logf("committed %s and pushed %s for %s", hash[:7], b.in.branch, b.entry.Name)
	return true, false
}

// commit stages every change (deletions included) and commits it as
// commit_and_push does.
func (b *dependencyBump) commit(message string) (string, error) {
	worktree, err := b.worktree()
	if err != nil {
		return "", err
	}
	if err := worktree.AddGlob("."); err != nil {
		return "", err
	}
	hash, err := worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: commitAuthorName, Email: commitAuthorEmail, When: time.Now()},
	})
	if err != nil {
		return "", err
	}
	return hash.String(), nil
}
