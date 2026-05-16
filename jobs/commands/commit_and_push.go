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
	status, err := worktree.Status()
	if err != nil {
		return repoOutput{}, fmt.Errorf("error reading status: %s", err)
	}
	if status.IsClean() {
		io.WriteString(tcp.logsWriter, fmt.Sprintf("No changes in repo %s — skipping commit/push\n", entry.Name))
		return repoOutput{Index: idx, Name: entry.Name, HasChanges: false}, nil
	}
	if err := worktree.AddGlob("."); err != nil {
		return repoOutput{}, fmt.Errorf("error staging changes: %s", err)
	}
	commitSHA, err := tcp.commit(worktree)
	if err != nil {
		return repoOutput{}, err
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
}

type agentOutput struct {
	ChangesSummary string     `json:"changes_summary,omitempty"`
	TokenUsage     tokenUsage `json:"token_usage"`
	ExitCode       int        `json:"exit_code,omitempty"`
	// DeniedHosts is the dedup-sorted list of hostnames the agentbox
	// proxy refused due to allowlist mismatches during this Step run.
	// Populated by RunAgentStep from agentbox's /result.json. Surfaced
	// to the dashboard so users can suggest allowlist additions
	// without parsing container logs. Empty when no denies happened.
	DeniedHosts []string `json:"denied_hosts,omitempty"`
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

