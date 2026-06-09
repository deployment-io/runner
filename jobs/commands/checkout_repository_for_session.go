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
// read-only-clones the session's single repo at its base branch into the
// session work dir, which RunAssistantSession then bind-mounts as /work. Unlike
// runForTask there is no task branch, no PR lookup, and no commit — a session
// plans against the code, it doesn't modify it. v1 is single-repo; the agent's
// cwd is the repo root.
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
	repoDir := commandUtils.GetSessionRepositoriesBaseDir(orgID, jobID)
	if err := cloneSessionRepoReadOnly(repoDir, entries[0], orgID, logsWriter); err != nil {
		return parameters, fmt.Errorf("error checking out repo %s: %s", entries[0].Name, err)
	}
	return parameters, nil
}

// cloneSessionRepoReadOnly clones one repo at its base branch into repoDir for an
// interactive session, retrying once with a refreshed token on auth failure
// (mirrors the Task clone). Chowns the tree to the agentbox `agent` user so the
// UID-1000 container can read it through the bind mount.
func cloneSessionRepoReadOnly(repoDir string, entry tasks.RepositoryEntry, orgID string, logsWriter io.Writer) error {
	// Start from a clean dir so a re-picked-up session Job (crash recovery)
	// doesn't clone into a non-empty path.
	_ = os.RemoveAll(repoDir)
	token, err := commandUtils.RefreshGitTokenForInstallation(entry.InstallationID, orgID)
	if err != nil {
		return fmt.Errorf("error getting installation token: %s", err)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Cloning %s (%s) read-only into %s\n", entry.Name, entry.BaseBranch, repoDir))
	repository, token, err := cloneSessionWithRetry(repoDir, entry, token, orgID, logsWriter)
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
	return chownTreeToAgentbox(repoDir)
}

// cloneSessionWithRetry runs the clone, refreshing the token + retrying once on
// go-git's "authentication required" error. Returns the (possibly-refreshed)
// token used for the successful clone.
func cloneSessionWithRetry(repoDir string, entry tasks.RepositoryEntry, token, orgID string, logsWriter io.Writer) (*git.Repository, string, error) {
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
	cloneURL, err = commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	repository, err = commandUtils.CloneRepository(repoDir, cloneURL, token, entry.Provider, logsWriter)
	return repository, token, err
}
