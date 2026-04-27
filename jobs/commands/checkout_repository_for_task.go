package commands

import (
	"fmt"
	"io"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// runForTask is the Tasks-mode entrypoint for CheckoutRepository. It iterates
// the Job's Repositories list and clones each into <baseDir>/<idx>-<name>.
// First Step (StepIndex == 0): checkout each repo's BaseBranch and create the
// shared Task branch locally so subsequent commits land on it.
// Subsequent Steps: fetch + checkout the shared Task branch from origin
// (where the prior Step pushed). Tokens are minted fresh per-repo via
// RefreshGitTokenForInstallation; lazy-refresh on 401 follows the existing
// CheckoutRepository pattern.
func (cr *CheckoutRepository) runForTask(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	ctx, err := commandUtils.ParseTaskCheckoutContext(parameters)
	if err != nil {
		return parameters, err
	}
	for idx, entry := range ctx.Entries {
		repoDir := commandUtils.GetTaskRepositoryDir(ctx.OrganizationID, ctx.TaskID, idx, entry.Name)
		io.WriteString(logsWriter, fmt.Sprintf("Checking out repo %s into %s\n", entry.Name, repoDir))
		if err := checkoutOneTaskRepo(repoDir, ctx, entry, logsWriter); err != nil {
			return parameters, fmt.Errorf("error checking out repo %s: %s", entry.Name, err)
		}
	}
	return parameters, nil
}

// checkoutOneTaskRepo clones one repo and positions it on the right branch
// for the current Step. Encapsulates the per-repo retry-on-401 dance.
func checkoutOneTaskRepo(repoDir string, ctx commandUtils.TaskCheckoutContext, entry tasks.RepositoryEntry, logsWriter io.Writer) error {
	token, err := commandUtils.RefreshGitTokenForInstallation(entry.InstallationID, ctx.OrganizationID)
	if err != nil {
		return fmt.Errorf("error minting installation token: %s", err)
	}
	repository, token, err := cloneTaskRepoWithRetry(repoDir, entry, token, ctx.OrganizationID, logsWriter)
	if err != nil {
		return err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("error getting worktree: %s", err)
	}
	if ctx.StepIndex == 0 {
		return checkoutBaseBranchAndCreateTaskBranch(worktree, entry.BaseBranch, ctx.BranchName)
	}
	return fetchAndCheckoutTaskBranch(repository, worktree, entry, token, ctx, logsWriter)
}

// cloneTaskRepoWithRetry runs the clone, refreshes token + retries once
// on go-git's "authentication required" error. Returns the repository and
// the (possibly-refreshed) token used for the successful clone.
func cloneTaskRepoWithRetry(repoDir string, entry tasks.RepositoryEntry, token, orgID string, logsWriter io.Writer) (*git.Repository, string, error) {
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

// checkoutBaseBranchAndCreateTaskBranch is the StepIndex==0 path: position
// the worktree on the repo's base branch, then create the shared Task branch
// locally so subsequent commits land on it.
func checkoutBaseBranchAndCreateTaskBranch(worktree *git.Worktree, baseBranch, taskBranchName string) error {
	baseRef := plumbing.NewRemoteReferenceName("origin", baseBranch)
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: baseRef}); err != nil {
		return fmt.Errorf("error checking out base branch %s: %s", baseBranch, err)
	}
	taskBranchRef := plumbing.NewBranchReferenceName(taskBranchName)
	if err := worktree.Checkout(&git.CheckoutOptions{Create: true, Branch: taskBranchRef}); err != nil {
		return fmt.Errorf("error creating task branch %s: %s", taskBranchName, err)
	}
	return nil
}

// fetchAndCheckoutTaskBranch is the StepIndex>0 path: pull the shared Task
// branch (where prior Steps pushed) and check it out so this Step's commits
// stack on top.
func fetchAndCheckoutTaskBranch(repository *git.Repository, worktree *git.Worktree,
	entry tasks.RepositoryEntry, token string, ctx commandUtils.TaskCheckoutContext, logsWriter io.Writer) error {
	if err := fetchWithRetry(repository, entry, token, ctx.OrganizationID, logsWriter); err != nil {
		return err
	}
	taskBranchRef := plumbing.NewRemoteReferenceName("origin", ctx.BranchName)
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: taskBranchRef}); err != nil {
		return fmt.Errorf("error checking out task branch %s: %s", ctx.BranchName, err)
	}
	return nil
}

// fetchWithRetry runs FetchRepository, refreshes token + retries once on
// "authentication required". Mirrors the clone retry shape.
func fetchWithRetry(repository *git.Repository, entry tasks.RepositoryEntry, token, orgID string, logsWriter io.Writer) error {
	if err := commandUtils.FetchRepository(repository, token, entry.Provider, logsWriter); err == nil {
		return nil
	} else if !commandUtils.IsErrorAuthenticationRequired(err) {
		return err
	}
	token, err := commandUtils.RefreshGitTokenForInstallation(entry.InstallationID, orgID)
	if err != nil {
		return err
	}
	return commandUtils.FetchRepository(repository, token, entry.Provider, logsWriter)
}
