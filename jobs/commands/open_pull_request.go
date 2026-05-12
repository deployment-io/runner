package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// OpenPullRequest is a Tasks-only runner command. Final command in the
// Step Job sequence: opens PRs across all repos with HasChanges=true
// (set by CommitAndPush in this same Job's accumulated JobOutput),
// merges per-repo PR URL + number back into the JobOutput repositories
// block, and triggers MarkStepDone on success.
//
// Provider REST calls happen server-side via the OpenPullRequestV1 RPC
// (see deployment-server/cmd/methods/oauth.go). This command is thin:
// per-repo loop over RepositoryEntries, RPC call when HasChanges, merge,
// done.
type OpenPullRequest struct{}

// Run is the runner-side entrypoint. Tasks-only; no non-Tasks branch.
// MarkStepDone fires both on error (defer) and on success (after merge)
// since this is the last command in the Step Job's command sequence.
func (opr *OpenPullRequest) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkStepDone(parameters, err)
		}
	}()
	ctx, err := commandUtils.ParseTaskJobContext(parameters)
	if err != nil {
		return parameters, err
	}
	hasChangesByIndex, err := readHasChangesFromJobOutput(parameters)
	if err != nil {
		return parameters, fmt.Errorf("error reading commit-and-push output: %s", err)
	}
	deniedHosts, err := readDeniedHostsFromJobOutput(parameters)
	if err != nil {
		// Defensive: a malformed JobOutput here shouldn't fail the PR open
		// (the PR is the user-facing artifact and we want it to land).
		// Log and continue with no deny-section in the PR body.
		io.WriteString(logsWriter, fmt.Sprintf("warning: could not read denied_hosts from job output: %s\n", err))
		deniedHosts = nil
	}
	opener := &taskOpenPR{
		ctx:          ctx,
		logsWriter:   logsWriter,
		deniedHosts:  deniedHosts,
		agentSummary: readAgentSummaryFromJobOutput(parameters),
	}
	prOutputs, err := opener.openAll(hasChangesByIndex)
	if err != nil {
		return parameters, err
	}
	if err := opener.mergeIntoJobOutput(parameters, prOutputs); err != nil {
		return parameters, fmt.Errorf("error merging PR results into job output: %s", err)
	}
	<-MarkStepDone(parameters, nil)
	return parameters, nil
}

// taskOpenPR bundles the per-Step-Job state for the per-repo loop.
// Mirrors the taskCheckout / taskCommitPush pattern. No token cache
// here — token lookup happens server-side in the RPC.
//
// deniedHosts carries the agentbox proxy's allowlist-deny set for this
// Step run (sourced from the agent block of JobOutput, which RunAgentStep
// populated from /result.json). Surfaced in the PR body so reviewers see
// what hosts the agent tried to reach but couldn't — closes the
// feedback loop on org-level allowlist tuning. Empty when no allowlist
// denies happened during the Step.
type taskOpenPR struct {
	ctx          commandUtils.TaskJobContext
	logsWriter   io.Writer
	deniedHosts  []string
	agentSummary string // agent.changes_summary from /result.json; "" when missing
}

// openAll iterates the Job's repositories. Skips repos where
// CommitAndPush set HasChanges=false (clean working dir, no commit
// pushed, no PR to open). Per-repo errors abort — partial PR opening
// across a multi-repo Task leaves an inconsistent state and the Step
// Job is treated as a unit.
func (opr *taskOpenPR) openAll(hasChangesByIndex map[int]bool) ([]repoOutput, error) {
	outputs := make([]repoOutput, 0, len(opr.ctx.Entries))
	for idx, entry := range opr.ctx.Entries {
		if !hasChangesByIndex[idx] {
			io.WriteString(opr.logsWriter, fmt.Sprintf("No changes in repo %s — skipping PR open\n", entry.Name))
			outputs = append(outputs, repoOutput{Index: idx, Name: entry.Name, HasChanges: false})
			continue
		}
		out, err := opr.openOne(idx, entry)
		if err != nil {
			return nil, fmt.Errorf("error opening PR for repo %s: %s", entry.Name, err)
		}
		outputs = append(outputs, out)
	}
	return outputs, nil
}

// openOne calls the deployment-server RPC to open one PR/MR. The repo's
// base branch is its own (per-repo BaseBranch); head is the shared Task
// branch name (same across all repos in the Task).
func (opr *taskOpenPR) openOne(idx int, entry tasks.RepositoryEntry) (repoOutput, error) {
	if len(entry.BaseBranch) == 0 {
		// Defense in depth: connection-time probe and Task-creation gate
		// should have caught this earlier (see PLAN_tasks.md "Net-New
		// Infrastructure" item 8 and Phase 6 dashboard gate). If we got
		// here with an empty BaseBranch, the repo's default branch is
		// missing on the provider — error with a useful message rather
		// than letting the provider 422 with a less helpful one.
		return repoOutput{}, fmt.Errorf("repo %s has no base branch configured; set the default branch on the provider or use the per-Task override", entry.Name)
	}
	title, body := opr.buildPRTitleAndBody()
	prURL, prNumber, err := client.Get().OpenPullRequest(opr.ctx.OrganizationID, entry.InstallationID,
		entry.Name, entry.BaseBranch, opr.ctx.BranchName, title, body)
	if err != nil {
		return repoOutput{}, err
	}
	io.WriteString(opr.logsWriter, fmt.Sprintf("Opened PR #%d for repo %s: %s\n", prNumber, entry.Name, prURL))
	return repoOutput{
		Index:      idx,
		Name:       entry.Name,
		HasChanges: true,
		Branch:     opr.ctx.BranchName,
		PRURL:      prURL,
		PRNumber:   prNumber,
	}, nil
}

// buildPRTitleAndBody mirrors CommitAndPush's commit-message shape so
// the PR description matches the commit:
//
//   - When the agent produced a changes_summary, the first line becomes
//     the PR title and the remainder becomes the PR body lead-in
//     (matches the commit subject/body split).
//   - Without a summary, falls back to "Tasks Step <N>: <title>" with no
//     lead-in (Step ran but didn't produce one, or runtime pre-dates
//     the RunAgentStep producer).
//
// Trailer block + denied-hosts section are uniform across both cases.
// When the Step had allowlist denies, an additional section lists the
// blocked hostnames so the PR reviewer can see what the agent tried
// to reach. Helps diagnose "agent gave up because it couldn't fetch X"
// scenarios without digging through the runner's container logs.
func (opr *taskOpenPR) buildPRTitleAndBody() (string, string) {
	subject, leadIn := opr.subjectAndLeadIn()
	var sb strings.Builder
	if len(leadIn) > 0 {
		sb.WriteString(leadIn)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Generated-By: deployment.io Tasks\n")
	sb.WriteString(fmt.Sprintf("Task: %s\n", opr.ctx.TaskTitle))
	if len(opr.ctx.DashboardURL) > 0 {
		sb.WriteString(fmt.Sprintf("Task-URL: %s/tasks/%s\n", strings.TrimRight(opr.ctx.DashboardURL, "/"), opr.ctx.TaskID))
	}
	if len(opr.deniedHosts) > 0 {
		sb.WriteString("\n---\n")
		sb.WriteString("**Network: blocked hosts during this Step**\n\n")
		sb.WriteString("The agent attempted to reach the following hostnames but they weren't on the allowlist. Add them to your org's Tasks → Allowed Hosts settings if expected:\n\n")
		for _, h := range opr.deniedHosts {
			sb.WriteString(fmt.Sprintf("- `%s`\n", h))
		}
	}
	return subject, sb.String()
}

// subjectAndLeadIn mirrors taskCommitPush.subjectAndBody so the PR
// title/lead-in matches the commit subject/body. Returns the agent's
// changes_summary first-line as subject and remainder as lead-in when
// present; falls back to the generic "Tasks Step N: <title>" subject
// with empty lead-in otherwise.
func (opr *taskOpenPR) subjectAndLeadIn() (string, string) {
	if len(opr.agentSummary) > 0 {
		if idx := strings.Index(opr.agentSummary, "\n"); idx > 0 {
			return strings.TrimSpace(opr.agentSummary[:idx]), strings.TrimSpace(opr.agentSummary[idx+1:])
		}
		return strings.TrimSpace(opr.agentSummary), ""
	}
	return fmt.Sprintf("Tasks Step %d: %s", opr.ctx.StepIndex+1, opr.ctx.TaskTitle), ""
}

// mergeIntoJobOutput reads the existing JobOutput (with CommitAndPush's
// per-repo entries already in place), merges in PR URL/number per repo,
// writes the combined envelope back. Index-keyed merge inherited from
// CommitAndPush keeps name-collision-safety.
func (opr *taskOpenPR) mergeIntoJobOutput(parameters map[string]interface{}, outputs []repoOutput) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.Repositories = mergeRepoOutputs(data.Repositories, outputs)
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// readDeniedHostsFromJobOutput pulls the agentbox proxy's allowlist
// deny set that RunAgentStep wrote to the accumulated JobOutput. Empty
// slice on missing/malformed payloads — caller treats absence as "no
// denies" rather than failing the PR open. Mirrors the
// readHasChangesFromJobOutput shape; both read different fields off
// the same envelope.
func readDeniedHostsFromJobOutput(parameters map[string]interface{}) ([]string, error) {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil, nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil, err
	}
	if data.Agent == nil {
		return nil, nil
	}
	return data.Agent.DeniedHosts, nil
}

// readHasChangesFromJobOutput pulls the per-repo HasChanges flags that
// CommitAndPush wrote to the accumulated JobOutput. Returns a map keyed
// on the repo index. Missing entries default to false (defensive: skip
// repos with no commit-and-push record rather than try to open a PR
// that may have nothing to point to).
func readHasChangesFromJobOutput(parameters map[string]interface{}) (map[int]bool, error) {
	out := make(map[int]bool)
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return out, nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil, err
	}
	for _, repo := range data.Repositories {
		out[repo.Index] = repo.HasChanges
	}
	return out, nil
}

