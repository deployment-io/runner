package commands

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// runForTask is the Tasks-mode entrypoint for CheckoutRepository. It iterates
// the Job's Repositories list and clones each into <baseDir>/<idx>-<name>.
// First Step (StepIndex == 0): checkout each repo's BaseBranch and create the
// shared Task branch locally so subsequent commits land on it.
// Subsequent Steps: fetch + checkout the shared Task branch from origin
// (where the prior Step pushed). Tokens are minted on demand and cached
// per-installation across the loop, so multi-repo Tasks sharing one GitHub
// App installation don't trigger redundant refresh RPCs.
func (cr *CheckoutRepository) runForTask(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	// Report the Step as started — the runner has begun this Step, so flip the
	// Task -> Running and Step -> StepRunning now (mirrors how builds report
	// build_enums.Running at checkout), replacing the premature "Running at job
	// creation". Best-effort + batched; the control-plane update is gated, so a
	// no-op (already advanced / cancelled) is harmless.
	jobID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	commandUtils.UpdateTasksPipeline.Add(ctx.OrganizationID, tasks.UpdateTaskStepRunningDtoV1{
		TaskID:    ctx.TaskID,
		StepIndex: int(ctx.StepIndex),
		JobID:     jobID,
	})
	tc := &taskCheckout{
		ctx:        ctx,
		tokenCache: make(map[string]string),
		logsWriter: logsWriter,
	}
	for idx, entry := range ctx.Entries {
		repoDir := commandUtils.GetTaskRepositoryDir(ctx.OrganizationID, ctx.TaskID, idx, entry.Name)
		io.WriteString(logsWriter, fmt.Sprintf("Checking out repo %s into %s\n", entry.Name, repoDir))
		if err := tc.checkoutOne(repoDir, entry); err != nil {
			return parameters, fmt.Errorf("error checking out repo %s: %s", entry.Name, err)
		}
	}
	return parameters, nil
}

// taskCheckout bundles the per-Step-Job state shared across the per-repo
// methods. The tokenCache eliminates redundant RefreshGitTokenForInstallation
// RPCs when multiple repos in this Task share one installation.
type taskCheckout struct {
	ctx        commandUtils.TaskJobContext
	tokenCache map[string]string
	logsWriter io.Writer
}

// getToken returns a fresh token for the installation, minting via the
// server's RefreshGitToken RPC on cache miss. Cache hits skip the RPC,
// which is the whole point of this struct.
func (tc *taskCheckout) getToken(installationID string) (string, error) {
	if token, ok := tc.tokenCache[installationID]; ok {
		return token, nil
	}
	token, err := commandUtils.RefreshGitTokenForInstallation(installationID, tc.ctx.OrganizationID)
	if err != nil {
		return "", err
	}
	tc.tokenCache[installationID] = token
	return token, nil
}

// refreshToken forces a refresh and overwrites the cache. Called from the
// retry-on-401 paths so a fresh token reaches subsequent repos in the loop
// that share this installation.
func (tc *taskCheckout) refreshToken(installationID string) (string, error) {
	token, err := commandUtils.RefreshGitTokenForInstallation(installationID, tc.ctx.OrganizationID)
	if err != nil {
		return "", err
	}
	tc.tokenCache[installationID] = token
	return token, nil
}

// shouldUseExistingTaskBranch decides whether the first Step of a Task
// should iterate on an existing PR (Q15) or branch off base from scratch.
// Asks deployment-server for the most recent PR matching this repo's
// task branch:
//   - Open PR exists → true (caller should fetchAndCheckoutTaskBranch,
//     so the agent's commits stack on the PR's existing tip and the
//     push fast-forwards).
//   - Merged PR     → returns an error (re-running a merged Task is
//     semantically nonsense; the work is already in base).
//   - Closed-unmerged or never-existed PR → false (caller should
//     branch off base — true first-run path).
//
// Any RPC failure (deployment-server older than this PR's deploy,
// provider not supporting the optional interface, transient network)
// degrades gracefully to false. Same effective behavior as before Q15,
// just less informative. Logged so the runner-side ops can see when
// the new path falls back.
func (tc *taskCheckout) shouldUseExistingTaskBranch(entry tasks.RepositoryEntry) (bool, error) {
	result, err := client.Get().GetOpenPullRequestForBranch(tc.ctx.OrganizationID, entry.InstallationID, entry.Name, tc.ctx.BranchName)
	if err != nil {
		io.WriteString(tc.logsWriter, fmt.Sprintf("Could not check existing PR for %s (falling back to base): %s\n", entry.Name, err))
		return false, nil
	}
	if result.Found && result.State == "closed" && result.Merged {
		return false, fmt.Errorf("cannot re-run task: PR #%d for %s is merged (%s); create a new task to extend it further",
			result.PRNumber, entry.Name, result.URL)
	}
	return result.Found && result.State == "open", nil
}

// checkoutOne clones one repo and positions it on the right branch for the
// current Step.
func (tc *taskCheckout) checkoutOne(repoDir string, entry tasks.RepositoryEntry) error {
	token, err := tc.getToken(entry.InstallationID)
	if err != nil {
		return fmt.Errorf("error getting installation token: %s", err)
	}
	repository, token, err := tc.cloneWithRetry(repoDir, entry, token)
	if err != nil {
		return err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("error getting worktree: %s", err)
	}
	if tc.ctx.StepIndex == 0 {
		// First-Step path. For a fresh first run, branch off base and
		// create the task branch locally. For a re-run, the dashboard
		// has already pushed a PR for the task branch — the agent
		// should iterate on it, not start over from base (Q15 in
		// PLAN_tasks_verification.md). Branching from base when an
		// open PR exists leads to sibling commits and a
		// non-fast-forward push that silently drops the agent's
		// verified work.
		useExisting, err := tc.shouldUseExistingTaskBranch(entry)
		if err != nil {
			return err
		}
		if useExisting {
			if err := tc.fetchAndCheckoutTaskBranch(repository, worktree, entry, token); err != nil {
				return err
			}
		} else {
			if err := checkoutBaseBranchAndCreateTaskBranch(worktree, entry.BaseBranch, tc.ctx.BranchName); err != nil {
				return err
			}
		}
	} else {
		if err := tc.fetchAndCheckoutTaskBranch(repository, worktree, entry, token); err != nil {
			return err
		}
	}
	// Strip the installation token from .git/config before the agent runs:
	// the runner's own git ops (clone, and the Step>0 fetch above) are done,
	// and commit_and_push later authenticates with its own freshly-minted
	// token via PushOptions.Auth — so the embedded credential is not needed
	// downstream, and the RunAgentStep container shouldn't be able to read it.
	if err := scrubRemoteToken(repository, entry.CloneURL); err != nil {
		return err
	}
	// go-git's PlainClone + Checkout ran as the runner process (root inside
	// its container). Chown the entire repo tree to the agentbox `agent`
	// user so the spawned RunAgentStep container can modify these files
	// through the bind mount.
	return chownTreeToAgentbox(repoDir)
}

// chownTreeToAgentbox lchowns every entry under root (including root itself)
// to the agentbox `agent` user. lchown so symlinks themselves are retargeted
// rather than whatever they point to (symlinks created by git can point
// outside the work tree).
func chownTreeToAgentbox(root string) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, commandUtils.AgentboxUID, commandUtils.AgentboxGID)
	})
}

// cloneWithRetry runs the clone, refreshes the token + retries once on
// go-git's "authentication required" error. Returns the (possibly-refreshed)
// token used for the successful clone so the caller can pass it to the
// subsequent fetch.
func (tc *taskCheckout) cloneWithRetry(repoDir string, entry tasks.RepositoryEntry, token string) (*git.Repository, string, error) {
	cloneURL, err := commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	repository, err := commandUtils.CloneRepository(repoDir, cloneURL, token, entry.Provider, tc.logsWriter)
	if err == nil {
		return repository, token, nil
	}
	if !commandUtils.IsErrorAuthenticationRequired(err) {
		return nil, token, err
	}
	token, err = tc.refreshToken(entry.InstallationID)
	if err != nil {
		return nil, token, err
	}
	cloneURL, err = commandUtils.GetRepoUrlWithToken(entry.Provider, token, entry.CloneURL)
	if err != nil {
		return nil, token, err
	}
	repository, err = commandUtils.CloneRepository(repoDir, cloneURL, token, entry.Provider, tc.logsWriter)
	return repository, token, err
}

// fetchAndCheckoutTaskBranch is the StepIndex>0 / Q15-re-run path: pull the
// shared Task branch (where prior Steps or the dashboard's first PR push
// landed) and check it out so this Step's commits stack on top.
//
// Materializes the fetched remote ref as a local branch ref before
// checking out. Without this step we'd be checking out refs/remotes/origin/<branch>
// directly, which puts HEAD in detached state. The agent's commits
// would advance detached HEAD but not refs/heads/<branch>, and
// commit_and_push.go's push refspec ("refs/heads/<branch>:refs/heads/<branch>")
// would find no new local commits to send — go-git reports "already
// up-to-date" and the verified work is silently dropped on the floor.
func (tc *taskCheckout) fetchAndCheckoutTaskBranch(repository *git.Repository, worktree *git.Worktree, entry tasks.RepositoryEntry, token string) error {
	if err := tc.fetchWithRetry(repository, entry, token); err != nil {
		return err
	}
	remoteRefName := plumbing.NewRemoteReferenceName("origin", tc.ctx.BranchName)
	remoteRef, err := repository.Reference(remoteRefName, true)
	if err != nil {
		return fmt.Errorf("error resolving fetched ref for task branch %s: %s", tc.ctx.BranchName, err)
	}
	localRefName := plumbing.NewBranchReferenceName(tc.ctx.BranchName)
	if err := repository.Storer.SetReference(plumbing.NewHashReference(localRefName, remoteRef.Hash())); err != nil {
		return fmt.Errorf("error creating local task branch %s: %s", tc.ctx.BranchName, err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: localRefName}); err != nil {
		return fmt.Errorf("error checking out task branch %s: %s", tc.ctx.BranchName, err)
	}
	return nil
}

// fetchWithRetry runs FetchRepository, refreshes the token + retries once on
// "authentication required". Mirrors the clone retry shape.
func (tc *taskCheckout) fetchWithRetry(repository *git.Repository, entry tasks.RepositoryEntry, token string) error {
	if err := commandUtils.FetchRepository(repository, token, entry.Provider, tc.logsWriter); err == nil {
		return nil
	} else if !commandUtils.IsErrorAuthenticationRequired(err) {
		return err
	}
	token, err := tc.refreshToken(entry.InstallationID)
	if err != nil {
		return err
	}
	return commandUtils.FetchRepository(repository, token, entry.Provider, tc.logsWriter)
}

// checkoutBaseBranchAndCreateTaskBranch is the StepIndex==0 path: position
// the worktree on the repo's base branch, then create the shared Task branch
// locally so subsequent commits land on it. Stateless — kept as a free
// function since it doesn't need any taskCheckout state.
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
