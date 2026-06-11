package commands

import (
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
		if err := cloneSessionRepoReadOnly(repoDir, entry, orgID, tokenCache, logsWriter); err != nil {
			return parameters, fmt.Errorf("error checking out repo %s: %s", entry.Name, err)
		}
	}
	return parameters, nil
}

// cloneSessionRepoReadOnly clones one repo at its base branch into repoDir for an
// interactive session, retrying once with a refreshed token on auth failure
// (mirrors the Task clone). Chowns the tree to the agentbox `agent` user so the
// UID-1000 container can read it through the bind mount. The caller wipes the
// base dir once, so this doesn't remove repoDir itself.
func cloneSessionRepoReadOnly(repoDir string, entry tasks.RepositoryEntry, orgID string, tokenCache map[string]string, logsWriter io.Writer) error {
	token, err := sessionToken(tokenCache, entry.InstallationID, orgID)
	if err != nil {
		return fmt.Errorf("error getting installation token: %s", err)
	}
	repository, _, err := cloneSessionWithRetry(repoDir, entry, token, orgID, tokenCache, logsWriter)
	if err != nil {
		return err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("error getting worktree: %s", err)
	}
	baseRef := plumbing.NewRemoteReferenceName("origin", entry.BaseBranch)
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: baseRef}); err != nil {
		return fmt.Errorf("error checking out base branch %s: %s", entry.BaseBranch, err)
	}
	if err := scrubRemoteToken(repository, entry.CloneURL); err != nil {
		return err
	}
	return chownTreeToAgentbox(repoDir)
}

// scrubRemoteToken resets origin's URL to the tokenless clone URL, removing the
// installation token go-git persists into .git/config from a tokenized clone.
// The in-container agent can read .git/config, but a session is read-only and
// never fetches or pushes again (and the network proxy blocks GitHub anyway),
// so the credential is pure liability — strip it. Best-effort by design: a
// missing origin is not an error.
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

// cloneSessionWithRetry runs the clone, refreshing the token + retrying once on
// go-git's "authentication required" error. A refreshed token is written back to
// the cache so later repos sharing the installation use it. Returns the
// (possibly-refreshed) token used for the successful clone.
func cloneSessionWithRetry(repoDir string, entry tasks.RepositoryEntry, token, orgID string, tokenCache map[string]string, logsWriter io.Writer) (*git.Repository, string, error) {
	cloneURL, err := commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	repository, err := commandUtils.CloneRepository(repoDir, cloneURL, token, entry.Provider, logsWriter)
	if err == nil {
		return repository, token, nil
	}
	if !commandUtils.IsErrorAuthenticationRequired(err) {
		return nil, token, err
	}
	token, err = commandUtils.RefreshGitTokenForInstallation(entry.InstallationID, orgID)
	if err != nil {
		return nil, token, err
	}
	tokenCache[entry.InstallationID] = token
	cloneURL, err = commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	repository, err = commandUtils.CloneRepository(repoDir, cloneURL, token, entry.Provider, logsWriter)
	return repository, token, err
}
