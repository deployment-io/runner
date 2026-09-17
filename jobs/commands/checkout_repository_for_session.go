package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// runForSession is the Assistant-session entrypoint for CheckoutRepository. It
// read-only-clones each of the session's repos at its base branch into
// <baseDir>/<idx>-<name>, and RunAssistantSession bind-mounts the base dir as
// /work (the agent's cwd; each repo is an <idx>-<name> subdirectory). Unlike
// runForTask there is no task branch, no PR lookup, and no commit — a session
// plans against the code, it doesn't modify it. Layout mirrors runForTask so
// single- and multi-repo sessions are identical.
func (cr *CheckoutRepository) runForSession(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	orgID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return parameters, fmt.Errorf("organization id missing: %s", err)
	}
	jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	if err != nil {
		return parameters, fmt.Errorf("job id missing: %s", err)
	}
	repositoriesJSON, err := jobs.GetParameterValue[string](parameters, parameters_enums.Repositories)
	if err != nil {
		return parameters, fmt.Errorf("repositories missing: %s", err)
	}
	var entries []tasks.RepositoryEntry
	if err := json.Unmarshal([]byte(repositoriesJSON), &entries); err != nil {
		return parameters, fmt.Errorf("error unmarshalling repositories: %s", err)
	}
	if len(entries) == 0 {
		return parameters, fmt.Errorf("session has no repositories")
	}
	// Start from a clean base so a re-picked-up session Job (crash recovery)
	// doesn't clone into stale repo dirs. The agentbox IO dirs are created
	// later by RunAssistantSession (after checkout), so wiping here is safe.
	baseDir := commandUtils.GetSessionRepositoriesBaseDir(orgID, jobID)
	_ = os.RemoveAll(baseDir)
	// Cache tokens per installation so multi-repo sessions sharing one GitHub
	// App installation don't trigger redundant refresh RPCs.
	tokenCache := map[string]string{}
	for idx, entry := range entries {
		repoDir := commandUtils.GetSessionRepositoryDir(orgID, jobID, idx, entry.Name)
		io.WriteString(logsWriter, fmt.Sprintf("Cloning %s (%s) read-only into %s\n", entry.Name, entry.BaseBranch, repoDir))
		req := sessionCloneRequest{entry: entry, orgID: orgID, tokenCache: tokenCache, logsWriter: logsWriter}
		if err := cloneSessionRepoReadOnly(sessionStartCloneContext(), repoDir, req); err != nil {
			return parameters, fmt.Errorf("error checking out repo %s: %s", entry.Name, err)
		}
	}
	return parameters, nil
}

// sessionStartCloneContext is the context a SESSION-START clone runs under:
// deliberately unbounded, so a big monorepo checkout keeps exactly the timing it
// has today (nothing is waiting on it — the session hasn't begun). Only the
// mid-session add path bounds its clone, with sessionCloneDeadline, because
// there a stuck clone holds back every later turn of a live conversation.
// A named function so "no deadline here" is an invariant a test can assert.
func sessionStartCloneContext() context.Context {
	return context.Background()
}

// sessionCloneRequest groups the per-repo clone inputs that travel together, so
// threading ctx through doesn't push the clone helpers past four parameters.
type sessionCloneRequest struct {
	entry      tasks.RepositoryEntry
	orgID      string // OrganizationIDNamespace — the org the installation token is minted for
	tokenCache map[string]string
	logsWriter io.Writer
}

// cloneSessionRepoReadOnly clones one repo at its base branch into repoDir for an
// interactive session, retrying once with a refreshed token on auth failure
// (mirrors the Task clone). Chowns the tree to the agentbox `agent` user so the
// UID-1000 container can read it through the bind mount. The caller wipes the
// base dir once, so this doesn't remove repoDir itself — which is also what
// makes it safe to call per-repo mid-session, where nothing is wiped at all.
//
// ctx bounds the clone. Session start passes one with no deadline; the
// mid-session add passes a 5-minute one.
func cloneSessionRepoReadOnly(ctx context.Context, repoDir string, req sessionCloneRequest) error {
	token, err := sessionToken(req.tokenCache, req.entry.InstallationID, req.orgID)
	if err != nil {
		return fmt.Errorf("error getting installation token: %s", err)
	}
	repository, _, err := cloneSessionWithRetry(ctx, repoDir, req, token)
	if err != nil {
		return err
	}
	entry := req.entry
	if err := checkoutSessionBaseBranch(repository, entry.BaseBranch); err != nil {
		return err
	}
	if err := scrubRemoteToken(repository, entry.CloneURL); err != nil {
		return err
	}
	return chownTreeToAgentbox(repoDir)
}

// checkoutSessionBaseBranch positions the worktree on baseBranch as a LOCAL
// branch — creating refs/heads/<base> from origin/<base> when the clone didn't
// already materialize it. Checking out the remote-tracking ref directly
// (refs/remotes/origin/<base>) leaves HEAD detached, which a session agent
// reads as a repo-hygiene problem and tells the user to "fix" (git checkout
// main): a false finding about our own ephemeral clone, not their repo.
func checkoutSessionBaseBranch(repository *git.Repository, baseBranch string) error {
	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("error getting worktree: %s", err)
	}
	localRef := plumbing.NewBranchReferenceName(baseBranch)
	opts := &git.CheckoutOptions{Branch: localRef}
	if _, err := repository.Reference(localRef, false); err != nil {
		// No local branch yet (base != the clone's default branch): create it
		// at the remote ref and switch to it.
		remoteRef, rerr := repository.Reference(plumbing.NewRemoteReferenceName("origin", baseBranch), true)
		if rerr != nil {
			return fmt.Errorf("error resolving base branch %s: %s", baseBranch, rerr)
		}
		opts.Hash = remoteRef.Hash()
		opts.Create = true
	}
	if err := worktree.Checkout(opts); err != nil {
		return fmt.Errorf("error checking out base branch %s: %s", baseBranch, err)
	}
	return nil
}

// scrubRemoteToken resets origin's URL to the tokenless clone URL, removing the
// installation token go-git persists into .git/config from a tokenized clone.
// Call it at the end of a checkout, after the runner's own clone/fetch, so the
// in-container agent never sees a credential it doesn't need. Safe for both
// flows: a session never pushes, and a Task pushes via commit_and_push, which
// authenticates with its own freshly-minted token through PushOptions.Auth —
// independent of the remote URL. Best-effort: a missing origin is not an error.
func scrubRemoteToken(repository *git.Repository, tokenlessURL string) error {
	cfg, err := repository.Config()
	if err != nil {
		return fmt.Errorf("error reading repo config to scrub token: %s", err)
	}
	remote, ok := cfg.Remotes["origin"]
	if !ok {
		return nil
	}
	remote.URLs = []string{tokenlessURL}
	if err := repository.Storer.SetConfig(cfg); err != nil {
		return fmt.Errorf("error setting scrubbed remote config: %s", err)
	}
	return nil
}

// sessionToken returns a token for the installation, minting + caching on a miss
// so repos sharing one installation don't re-hit the refresh RPC.
func sessionToken(cache map[string]string, installationID, orgID string) (string, error) {
	if token, ok := cache[installationID]; ok {
		return token, nil
	}
	token, err := commandUtils.RefreshGitTokenForInstallation(installationID, orgID)
	if err != nil {
		return "", err
	}
	cache[installationID] = token
	return token, nil
}

// cloneSessionWithRetry runs the clone under ctx, refreshing the token +
// retrying once on go-git's "authentication required" error. A refreshed token
// is written back to the cache so later repos sharing the installation use it.
// Returns the (possibly-refreshed) token used for the successful clone.
//
// It goes through commandUtils.CloneRepositoryWithContext rather than
// CloneRepository so the mid-session add can impose a deadline;
// CloneRepository's signature and behaviour are untouched for Tasks and
// deployments, and an unbounded ctx here behaves exactly as it did before.
func cloneSessionWithRetry(ctx context.Context, repoDir string, req sessionCloneRequest, token string) (*git.Repository, string, error) {
	entry := req.entry
	cloneURL, err := commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	opts := commandUtils.CloneOptions{
		CloneURLWithToken: cloneURL, Token: token, Provider: entry.Provider, LogsWriter: req.logsWriter,
	}
	repository, err := commandUtils.CloneRepositoryWithContext(ctx, repoDir, opts)
	if err == nil {
		return repository, token, nil
	}
	if !commandUtils.IsErrorAuthenticationRequired(err) {
		return nil, token, err
	}
	token, err = commandUtils.RefreshGitTokenForInstallation(entry.InstallationID, req.orgID)
	if err != nil {
		return nil, token, err
	}
	req.tokenCache[entry.InstallationID] = token
	cloneURL, err = commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	opts.CloneURLWithToken, opts.Token = cloneURL, token
	repository, err = commandUtils.CloneRepositoryWithContext(ctx, repoDir, opts)
	return repository, token, err
}
