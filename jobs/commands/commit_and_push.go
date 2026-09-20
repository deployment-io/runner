package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

const (
	commitAuthorName  = "deployment.io"
	commitAuthorEmail = "noreply@deployment.io"
	commitTrailerTag  = "Generated-By: deployment.io Tasks"
)

// CommitAndPush is a Tasks-only runner command. Per repo in the Job's
// TaskJobContext.Entries: opens the working dir, detects changes via
// go-git status, commits + pushes the shared Task branch when dirty,
// skips when clean. Aggregates per-repo results into the JobOutput
// repositories block so the deployment-server hook can persist
// HasChanges + commit SHA back to Task.Repositories[i].
type CommitAndPush struct{}

// Run is the runner-side entrypoint. Tasks-only; no non-Tasks branch.
// MarkStepDone fires on error to clean up the Task working dir; success
// cleanup happens in the last command of the Step Job (OpenPullRequest).
func (cap *CommitAndPush) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkStepDone(parameters, err)
		}
	}()
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	tcp := &taskCommitPush{
		ctx:          ctx,
		tokenCache:   make(map[string]string),
		logsWriter:   logsWriter,
		agentSummary: readAgentSummaryFromJobOutput(parameters),
	}
	repoOutputs, err := tcp.commitAndPushAll()
	if err != nil {
		return parameters, err
	}
	if err := detectOrphanedChanges(parameters, repoOutputs, logsWriter); err != nil {
		return parameters, err
	}
	if err := tcp.mergeRepositoriesIntoJobOutput(parameters, repoOutputs); err != nil {
		return parameters, fmt.Errorf("error merging repositories into job output: %s", err)
	}
	return parameters, nil
}

// taskCommitPush bundles the per-Step-Job state. Mirrors the taskCheckout
// pattern from checkout_repository_for_task.go so multi-repo Tasks sharing
// one GitHub App installation don't trigger redundant token-refresh RPCs.
//
// agentSummary is the agent's changes_summary from /result.json, snapshot
// at command-Run time (RunAgentStep has already written it into JobOutput
// by then). Cached on the struct so each per-repo commit gets the same
// subject/body without repeatedly parsing JobOutput.
type taskCommitPush struct {
	ctx          commandUtils.TaskJobContext
	tokenCache   map[string]string
	logsWriter   io.Writer
	agentSummary string
}

func (tcp *taskCommitPush) getToken(installationID string) (string, error) {
	if token, ok := tcp.tokenCache[installationID]; ok {
		return token, nil
	}
	token, err := commandUtils.RefreshGitTokenForInstallation(installationID, tcp.ctx.OrganizationID)
	if err != nil {
		return "", err
	}
	tcp.tokenCache[installationID] = token
	return token, nil
}

func (tcp *taskCommitPush) refreshToken(installationID string) (string, error) {
	token, err := commandUtils.RefreshGitTokenForInstallation(installationID, tcp.ctx.OrganizationID)
	if err != nil {
		return "", err
	}
	tcp.tokenCache[installationID] = token
	return token, nil
}

// commitAndPushAll iterates the Job's repositories. Per-repo errors abort
// the loop — partial success would leave the Task in an inconsistent state
// (some repos pushed, others not), and the Step Job is treated as a unit.
func (tcp *taskCommitPush) commitAndPushAll() ([]repoOutput, error) {
	outputs := make([]repoOutput, 0, len(tcp.ctx.Entries))
	for idx, entry := range tcp.ctx.Entries {
		repoDir := commandUtils.GetTaskRepositoryDir(tcp.ctx.OrganizationID, tcp.ctx.TaskID, idx, entry.Name)
		out, err := tcp.commitAndPushOne(repoDir, entry, idx)
		if err != nil {
			return nil, fmt.Errorf("error committing/pushing repo %s: %s", entry.Name, err)
		}
		outputs = append(outputs, out)
	}
	return outputs, nil
}

// commitAndPushOne handles one repo. Returns a clean-but-no-changes output
// when the working dir has nothing to commit; otherwise commits + pushes
// and returns the new commit SHA. The idx is the position in the Task's
// Repositories slice — used as the stable identifier in JobOutput because
// repo Name can collide across orgs (org-a/api + org-b/api).
func (tcp *taskCommitPush) commitAndPushOne(repoDir string, entry tasks.RepositoryEntry, idx int) (repoOutput, error) {
	repository, err := git.PlainOpen(repoDir)
	if err != nil {
		return repoOutput{}, fmt.Errorf("error opening repo at %s: %s", repoDir, err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return repoOutput{}, fmt.Errorf("error getting worktree: %s", err)
	}
	// Hand the worktree a pattern set collected with git's semantics before
	// anything reads status or stages. go-git's own collection re-includes
	// files under an excluded directory when something in there ships a
	// negating .gitignore — see gitignore.go. This must precede Status(),
	// which applies the same matcher and would otherwise report vendored
	// files as changes.
	worktree.Excludes = append(worktree.Excludes, gitSemanticsPatterns(worktree.Filesystem)...)

	status, err := worktree.Status()
	if err != nil {
		return repoOutput{}, fmt.Errorf("error reading status: %s", err)
	}
	if status.IsClean() {
		// A clean worktree is not the same as no work: an agent told to
		// "open a PR" runs git commit itself (push is blocked by the egress
		// allowlist, so the commits sit on the local task branch). Push those
		// as they are rather than reporting no changes — which used to fail
		// the Step as "written outside the repository", the one diagnosis
		// that was certainly wrong.
		ahead, headSHA, err := unpushedCommits(repository, tcp.ctx.BranchName, entry.BaseBranch, tcp.logsWriter)
		if err != nil {
			return repoOutput{}, fmt.Errorf("error comparing local branch to origin: %s", err)
		}
		if !ahead {
			io.WriteString(tcp.logsWriter, fmt.Sprintf("No changes in repo %s — skipping commit/push\n", entry.Name))
			return repoOutput{Index: idx, Name: entry.Name, HasChanges: false}, nil
		}
		io.WriteString(tcp.logsWriter, fmt.Sprintf("Repo %s has no uncommitted changes but the agent committed on %s itself — pushing as-is\n", entry.Name, tcp.ctx.BranchName))
		if err := tcp.pushWithRetry(repository, entry); err != nil {
			return repoOutput{}, fmt.Errorf("error pushing: %s", err)
		}
		io.WriteString(tcp.logsWriter, fmt.Sprintf("Pushed %s (%s) for repo %s\n", headSHA[:7], tcp.ctx.BranchName, entry.Name))
		return repoOutput{
			Index:      idx,
			Name:       entry.Name,
			HasChanges: true,
			CommitSHA:  headSHA,
			Branch:     tcp.ctx.BranchName,
		}, nil
	}
	if err := worktree.AddGlob("."); err != nil {
		return repoOutput{}, fmt.Errorf("error staging changes: %s", err)
	}
	commitSHA, err := tcp.commit(worktree)
	if err != nil {
		return repoOutput{}, err
	}
	// The commit landed on HEAD, which is the task branch unless the agent
	// switched branches; either way the task branch is what gets pushed.
	if tip, err := reconcileTaskBranch(repository, tcp.ctx.BranchName, tcp.logsWriter); err != nil {
		return repoOutput{}, err
	} else {
		commitSHA = tip.String()
	}
	if err := tcp.pushWithRetry(repository, entry); err != nil {
		return repoOutput{}, fmt.Errorf("error pushing: %s", err)
	}
	io.WriteString(tcp.logsWriter, fmt.Sprintf("Committed %s and pushed %s for repo %s\n", commitSHA[:7], tcp.ctx.BranchName, entry.Name))
	return repoOutput{
		Index:      idx,
		Name:       entry.Name,
		HasChanges: true,
		CommitSHA:  commitSHA,
		Branch:     tcp.ctx.BranchName,
	}, nil
}

// commit creates the commit with the deployment.io Tasks author identity.
// Message is the agent's changes_summary when present (Phase 5+) or a
// generic Phase 4 fallback. Trailer block is uniform across both cases.
func (tcp *taskCommitPush) commit(worktree *git.Worktree) (string, error) {
	hash, err := worktree.Commit(tcp.buildCommitMessage(), &git.CommitOptions{
		Author: &object.Signature{
			Name:  commitAuthorName,
			Email: commitAuthorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("error committing: %s", err)
	}
	return hash.String(), nil
}

// buildCommitMessage assembles subject + optional body + trailer. The
// subject prefers the agent's changes_summary first line; without it,
// falls back to "Tasks Step <N>: <title>".
func (tcp *taskCommitPush) buildCommitMessage() string {
	subject, body := tcp.subjectAndBody()
	var sb strings.Builder
	sb.WriteString(subject)
	sb.WriteString("\n")
	if len(body) > 0 {
		sb.WriteString("\n")
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(commitTrailerTag)
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("Task: %s\n", tcp.ctx.TaskTitle))
	if len(tcp.ctx.DashboardURL) > 0 {
		sb.WriteString(fmt.Sprintf("Task-URL: %s/tasks/%s\n", strings.TrimRight(tcp.ctx.DashboardURL, "/"), tcp.ctx.TaskID))
	}
	return sb.String()
}

// subjectAndBody returns (subject, body) for the commit message.
// Uses the agent's changes_summary when present — split at the first
// newline, with the first line as subject and remainder as body.
// Falls back to "Tasks Step <N>: <title>" with no body when the
// summary is absent (Step ran but didn't produce one, or runtime
// older than the Phase-5 RunAgentStep producer).
func (tcp *taskCommitPush) subjectAndBody() (string, string) {
	if len(tcp.agentSummary) > 0 {
		if idx := strings.Index(tcp.agentSummary, "\n"); idx > 0 {
			return strings.TrimSpace(tcp.agentSummary[:idx]), strings.TrimSpace(tcp.agentSummary[idx+1:])
		}
		return strings.TrimSpace(tcp.agentSummary), ""
	}
	return fmt.Sprintf("Tasks Step %d: %s", tcp.ctx.StepIndex+1, tcp.ctx.TaskTitle), ""
}

// detectOrphanedChanges fails the Step when the agent reported changed files but
// none landed in any repository — the "wrote outside the repo directory" failure
// (the agent creates files at the /work root instead of /work/<owner>/<repo>, so
// the per-repo git diff is empty). Without this the Step silently succeeds with
// no commit and no PR, which looks like success but produced nothing. A genuine
// no-op (the agent changed nothing) has empty files_changed and is left alone.
func detectOrphanedChanges(parameters map[string]interface{}, repoOutputs []repoOutput, logsWriter io.Writer) error {
	for _, r := range repoOutputs {
		if r.HasChanges {
			return nil // at least one repo changed — normal path
		}
	}
	filesChanged := readAgentFilesChangedFromJobOutput(parameters)
	if len(filesChanged) == 0 {
		return nil // genuine no-op — the agent reported no changed files
	}
	joined := strings.Join(filesChanged, ", ")
	io.WriteString(logsWriter, fmt.Sprintf(
		"Agent reported %d changed file(s) but none landed in any repository — they were likely written outside the repository directory (e.g. at /work instead of /work/<owner>/<repo>) and cannot be committed. Files: %s\n",
		len(filesChanged), joined))
	return fmt.Errorf("agent changes were written outside the repository — nothing to commit (files: %s)", joined)
}

// readAgentFilesChangedFromJobOutput pulls the agent.files_changed list from the
// JobOutput envelope (written by RunAgentStep). Empty on any parse miss.
func readAgentFilesChangedFromJobOutput(parameters map[string]interface{}) []string {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil || data.Agent == nil {
		return nil
	}
	return data.Agent.FilesChanged
}

// readAgentSummaryFromJobOutput pulls the agent.changes_summary field
// out of the accumulated JobOutput envelope. RunAgentStep populates
// this before CommitAndPush + OpenPullRequest run; both commands call
// this once at Run-time and cache the result for the per-repo loop.
// Best-effort — any unmarshalling failure returns "" so the caller's
// commit/PR-body fallback path fires.
func readAgentSummaryFromJobOutput(parameters map[string]interface{}) string {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return ""
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return ""
	}
	if data.Agent == nil {
		return ""
	}
	return data.Agent.ChangesSummary
}

// pushWithRetry pushes the local Task branch to origin. Refreshes the
// installation token + retries once on go-git's "authentication required"
// error, mirroring the clone/fetch retry pattern.
func (tcp *taskCommitPush) pushWithRetry(repository *git.Repository, entry tasks.RepositoryEntry) error {
	token, err := tcp.getToken(entry.InstallationID)
	if err != nil {
		return err
	}
	refSpec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", tcp.ctx.BranchName, tcp.ctx.BranchName))
	if err := tcp.push(repository, entry, token, refSpec); err == nil {
		return nil
	} else if !commandUtils.IsErrorAuthenticationRequired(err) {
		return err
	}
	token, err = tcp.refreshToken(entry.InstallationID)
	if err != nil {
		return err
	}
	return tcp.push(repository, entry, token, refSpec)
}

func (tcp *taskCommitPush) push(repository *git.Repository, entry tasks.RepositoryEntry, token string, refSpec config.RefSpec) error {
	return repository.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{refSpec},
		Auth: &http.BasicAuth{
			Username: commandUtils.GetUsernameForProvider(entry.Provider),
			Password: token,
		},
		Progress: tcp.logsWriter,
	})
}

// reconcileTaskBranch makes refs/heads/<task branch> — the only ref we ever
// push — point at the agent's work, and returns its tip. An agent told to
// "create a branch feature/x" commits on that branch, so HEAD descends from
// the task branch but the task branch never moved; fast-forward it to HEAD.
// A HEAD that does not descend from the task branch (the agent checked out
// something unrelated) is left alone and logged.
func reconcileTaskBranch(repository *git.Repository, branchName string, logsWriter io.Writer) (plumbing.Hash, error) {
	taskRefName := plumbing.NewBranchReferenceName(branchName)
	taskRef, err := repository.Reference(taskRefName, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("error reading task branch %s: %s", branchName, err)
	}
	head, err := repository.Head()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("error reading HEAD: %s", err)
	}
	tip := taskRef.Hash()
	if head.Hash() == tip {
		return tip, nil
	}
	descends, err := isAncestor(repository, tip, head.Hash())
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if !descends {
		io.WriteString(logsWriter, fmt.Sprintf("HEAD (%s at %s) is not on the task branch and does not descend from it — only %s is pushed\n", head.Name().Short(), head.Hash().String()[:7], branchName))
		return tip, nil
	}
	if err := repository.Storer.SetReference(plumbing.NewHashReference(taskRefName, head.Hash())); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("error fast-forwarding task branch %s: %s", branchName, err)
	}
	io.WriteString(logsWriter, fmt.Sprintf("HEAD (%s) moved off the task branch but descends from it — fast-forwarded %s to %s\n", head.Name().Short(), branchName, head.Hash().String()[:7]))
	return head.Hash(), nil
}

// unpushedCommits reports whether the task branch holds commits origin
// doesn't, after reconcileTaskBranch, and returns the task branch's SHA.
//
// The tip is "already on origin" if it equals ANY remote reference point that
// exists: origin/<task branch> (later Steps and re-runs fetch it) or
// origin/<base branch> (a first Step creates the task branch locally off base).
// Both are checked, not the first found: the clone fetches every remote
// branch, so a stale origin/<task branch> from an earlier, abandoned run can
// sit next to a fresh task branch that still equals base — comparing against
// the stale ref alone would call that "ahead" and push base onto it
// (non-fast-forward, Step fails) where the right answer is "nothing to push".
// With no remote reference point at all the answer is "no" — never a spurious
// push.
func unpushedCommits(repository *git.Repository, branchName, baseBranch string, logsWriter io.Writer) (bool, string, error) {
	tip, err := reconcileTaskBranch(repository, branchName, logsWriter)
	if err != nil {
		return false, "", err
	}
	found := false
	for _, name := range []string{branchName, baseBranch} {
		ref, err := repository.Reference(plumbing.NewRemoteReferenceName("origin", name), true)
		if err != nil {
			continue
		}
		found = true
		if ref.Hash() == tip {
			return false, tip.String(), nil
		}
	}
	return found, tip.String(), nil
}

// isAncestor reports whether commit a is an ancestor of (or equal to) commit b.
func isAncestor(repository *git.Repository, a, b plumbing.Hash) (bool, error) {
	ca, err := repository.CommitObject(a)
	if err != nil {
		return false, fmt.Errorf("error reading commit %s: %s", a.String()[:7], err)
	}
	cb, err := repository.CommitObject(b)
	if err != nil {
		return false, fmt.Errorf("error reading commit %s: %s", b.String()[:7], err)
	}
	return ca.IsAncestor(cb)
}

// repoOutput is one entry in the JobOutput repositories block.
// CommitAndPush populates Index + Name + HasChanges + CommitSHA + Branch.
// OpenPullRequest (chunk #4) extends each entry with PRURL + PRNumber.
//
// Index is the position in Task.Repositories — the stable identifier
// for merging output across commands. Name can collide across orgs
// (org-a/api + org-b/api both have Name == "api"), so it's a display
// field only, never the merge key.
type repoOutput struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	HasChanges bool   `json:"has_changes"`
	CommitSHA  string `json:"commit_sha,omitempty"`
	Branch     string `json:"branch,omitempty"`
	PRURL      string `json:"pr_url,omitempty"`
	PRNumber   int    `json:"pr_number,omitempty"`
}

// jobOutputSchemaVersion is the current version of the JobOutput envelope
// for Tasks Step Jobs. Bump when introducing breaking shape changes (e.g.,
// renaming a field, removing a field, changing the type of a field).
// Additive changes (new optional fields) don't require a bump — readers
// tolerate unknown fields.
const jobOutputSchemaVersion = 1

// jobOutputData is the on-the-wire shape of parameters_enums.JobOutput
// for Tasks Step Jobs. Agent block populated by RunAgentStep (Phase 5);
// repositories block populated by CommitAndPush + OpenPullRequest.
//
// SchemaVersion is set on every write so consumers (deployment-server hook,
// dashboard) can detect newer producers and reject incompatible payloads
// rather than silently mis-parsing. Missing schema_version on read is
// treated as version 1 for backward compat with payloads written before
// this field landed.
type jobOutputData struct {
	SchemaVersion int          `json:"schema_version"`
	Agent         *agentOutput `json:"agent,omitempty"`
	Repositories  []repoOutput `json:"repositories,omitempty"`
	// BaseCommits is each repository's commit at checkout, recorded by
	// CheckoutRepository before any agent could move it. The Review stage
	// diffs against these; see recordBaseCommit for why HEAD at review time
	// is not a substitute.
	BaseCommits []baseCommitOutput `json:"base_commits,omitempty"`
	// Review is the Review stage's record: every round, what was fixed
	// inside the loop, and whether a must-fix finding was still open at the
	// end. Nil when the stage did not run (participation Off, or a runner
	// older than the stage).
	Review *reviewOutput `json:"review,omitempty"`
	// Cost is what the run cost, RESOLVED ONCE HERE and never recomputed.
	//
	// Deliberately outside Agent: that block is agentbox's result.json passed
	// through verbatim, and this is our resolution of it — sometimes the
	// agent's own figure, sometimes our arithmetic. Mixing the two would lose
	// the distinction between what was measured and what was inferred.
	Cost *costOutput `json:"cost,omitempty"`
}

// costOutput is a run's cost with its PROVENANCE.
//
// Source matters as much as the number. An agent-reported cost came from the
// vendor that billed it; an estimate is our arithmetic over token counts and a
// rate table, and is wrong whenever the table is stale. Rendering them
// identically tells a customer we know something we do not.
//
// Recorded at completion rather than derived on read, because a Task's cost is
// a fact about when it ran. app-server previously recomputed codex costs during
// response mapping, so editing a rate silently rewrote every past Task.
type costOutput struct {
	USD float64 `json:"usd"`
	// Source is "agent" (the agent reported it) or "estimated" (we priced it
	// from token counts). Absent Cost means neither was possible — render an
	// unknown cost, never zero.
	Source string `json:"source"`
	// Model and Provider are recorded so a later question about a surprising
	// figure can be answered without guessing which rate applied. They are also
	// the only durable record of WHICH provider served a run, since the same
	// model costs different amounts by route.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

const (
	costSourceAgent     = "agent"
	costSourceEstimated = "estimated"
)

// baseCommitOutput is one repository's start-of-run commit.
//
// Index is the position in Task.Repositories — the stable identifier every
// other block in this envelope merges on. Dir is the directory name relative
// to /work ("0-acme-api"), which is what the review container needs: it sees
// the repository at that path and nowhere else.
type baseCommitOutput struct {
	Index     int    `json:"index"`
	Name      string `json:"name,omitempty"`
	Dir       string `json:"dir,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
}

// reviewFindingOutput mirrors agentbox's review finding, plus the one field
// agentbox cannot supply: whether this finding meets the org's must-fix
// threshold. That is the runner's decision, made from the thresholds stamped
// into the Job, and is never read from the agent.
type reviewFindingOutput struct {
	Key       string `json:"key,omitempty"`
	Parameter string `json:"parameter,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Location  string `json:"location,omitempty"`
	What      string `json:"what,omitempty"`
	Why       string `json:"why,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Pass      string `json:"pass,omitempty"`
	MustFix   bool   `json:"must_fix,omitempty"`
	// New marks a finding no earlier round reported. On a second or third
	// round it is the difference between "the fix did not work" and "the fix
	// introduced something else" — two very different things for a reader to
	// see, and indistinguishable from the finding alone.
	New bool `json:"new,omitempty"`
}

// reviewCoverageOutput mirrors agentbox's coverage entry: what happened to one
// review parameter, and why.
type reviewCoverageOutput struct {
	Parameter string `json:"parameter,omitempty"`
	State     string `json:"state,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// reviewRoundOutput is one review run. Completed=false with an Error is a
// round that did not produce a usable report — never a failed Step, but
// something the PR body has to say out loud rather than pass over.
type reviewRoundOutput struct {
	Round      int                    `json:"round"`
	Findings   []reviewFindingOutput  `json:"findings,omitempty"`
	Coverage   []reviewCoverageOutput `json:"coverage,omitempty"`
	AgentType  string                 `json:"agent_type,omitempty"`
	Model      string                 `json:"model,omitempty"`
	TokenUsage tokenUsage             `json:"token_usage"`
	CostUSD    *float64               `json:"cost_usd,omitempty"`
	Completed  bool                   `json:"completed"`
	Error      string                 `json:"error,omitempty"`
}

// reviewOutput is the Review stage's whole record for this Step run.
//
// FixedInLoop is what the implementer resolved without a human ever seeing it
// — the stage's actual product, and the thing a reader most wants to know.
// MustFixOpen is the decision that follows from the last round: true means the
// work still carries an unresolved must-fix finding and the pull request says
// so.
type reviewOutput struct {
	Participation string                `json:"participation,omitempty"`
	Rounds        []reviewRoundOutput   `json:"rounds,omitempty"`
	FixedInLoop   []reviewFindingOutput `json:"fixed_in_loop,omitempty"`
	MustFixOpen   bool                  `json:"must_fix_open"`
}

type agentOutput struct {
	ChangesSummary string `json:"changes_summary,omitempty"`
	// FilesChanged is the agent's self-reported changed-file list. Used by
	// CommitAndPush to detect the "reported changes but none landed in a repo"
	// failure (files written outside the repo dir → nothing to commit).
	FilesChanged []string   `json:"files_changed,omitempty"`
	TokenUsage   tokenUsage `json:"token_usage"`
	// Turns is the per-run turn count agentbox writes to /result.json.
	// Surfaced on the JobOutput envelope so app-server's projection
	// can populate AgentRunSummary.Turns for completed runs — the
	// dashboard prefers this over LiveProgress when the run is in a
	// terminal state, because a run that finishes faster than agentbox's
	// progress.json writer interval (~5s) leaves LiveProgress at zero.
	Turns int `json:"turns,omitempty"`
	// CostUSD is the agent's self-reported total run cost in USD, surfaced
	// from agentbox's /result.json ("cost_usd"). Present for Claude Code; nil
	// for Codex (which reports token usage only), so app-server estimates
	// Codex cost from TokenUsage and the published per-model rates.
	CostUSD  *float64 `json:"cost_usd,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	// DeniedHosts is the dedup-sorted list of hostnames the agentbox
	// proxy refused due to allowlist mismatches during this Step run.
	// Populated by RunAgentStep from agentbox's /result.json. Surfaced
	// to the dashboard so users can suggest allowlist additions
	// without parsing container logs. Empty when no denies happened.
	DeniedHosts []string `json:"denied_hosts,omitempty"`
	// PRTitle is the agent-produced short title for the resulting PR.
	// Distinct from ChangesSummary so OpenPullRequest can pick a clean
	// title instead of taking the first line of a long single-line
	// narrative. Empty when the agentbox image predates the pr_title
	// field; OpenPullRequest falls back to truncated first line of
	// ChangesSummary in that case.
	PRTitle string `json:"pr_title,omitempty"`
	// VerifyResult is agentbox's verify_result carried onto the envelope so
	// OpenPullRequest — a separate command that sees only JobOutput, never
	// /result.json — can put a pre-existing verification failure in the PR
	// body. Without this field the PR is the one place the failure ISN'T
	// mentioned, which is the place the reviewer is actually looking.
	// Nil for older agentbox images and for runs that reported no verify.
	VerifyResult *verifyResult `json:"verify_result,omitempty"`
}

// mergeRepositoriesIntoJobOutput reads existing JobOutput JSON (any prior
// command's contribution), merges in this command's per-repo entries
// (replacing any matching by name), writes back as JSON. Lets multiple
// commands in a Step Job contribute to one combined output document.
func (tcp *taskCommitPush) mergeRepositoriesIntoJobOutput(parameters map[string]interface{}, outputs []repoOutput) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data) // best-effort: malformed prior output is overwritten
	}
	data.SchemaVersion = jobOutputSchemaVersion // always stamp current; zero on read = pre-versioning payload, fine to overwrite
	data.Repositories = mergeRepoOutputs(data.Repositories, outputs)
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// mergeRepoOutputs replaces existing entries by Index with new ones,
// preserving any existing entries not in the new set. Used so that
// OpenPullRequest's later PR-fields write doesn't clobber CommitAndPush's
// commit-fields write — and vice-versa for retry scenarios.
//
// Per-field preservation policy: when a field on the incoming entry is
// at its zero value AND the existing entry has a non-zero value, the
// existing value is kept. This makes the merge commutative across the
// command sequence: each command writes only the fields it knows about
// (CommitAndPush writes CommitSHA + Branch + HasChanges; OpenPullRequest
// writes PRURL + PRNumber + Branch). Without this preservation, the
// later command's write would silently drop the earlier command's
// fields and the deployment-server hook would persist an incomplete
// repository record.
//
// Keyed on Index (position in Task.Repositories) rather than Name because
// repo names can collide across orgs in multi-org Tasks. Output is sorted
// by Index for deterministic JSON.
func mergeRepoOutputs(existing, incoming []repoOutput) []repoOutput {
	if len(existing) == 0 {
		return incoming
	}
	byIndex := make(map[int]repoOutput, len(existing))
	for _, e := range existing {
		byIndex[e.Index] = e
	}
	for _, in := range incoming {
		if prev, hadPrev := byIndex[in.Index]; hadPrev {
			if len(in.CommitSHA) == 0 && len(prev.CommitSHA) > 0 {
				in.CommitSHA = prev.CommitSHA
			}
			if len(in.Branch) == 0 && len(prev.Branch) > 0 {
				in.Branch = prev.Branch
			}
			if len(in.PRURL) == 0 && len(prev.PRURL) > 0 {
				in.PRURL = prev.PRURL
			}
			if in.PRNumber == 0 && prev.PRNumber != 0 {
				in.PRNumber = prev.PRNumber
			}
		}
		byIndex[in.Index] = in
	}
	out := make([]repoOutput, 0, len(byIndex))
	for _, e := range byIndex {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}
