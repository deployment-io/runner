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
	opener := &taskOpenPR{
		ctx:        ctx,
		logsWriter: logsWriter,
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
type taskOpenPR struct {
	ctx        commandUtils.TaskJobContext
	logsWriter io.Writer
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
// the PR description matches the commit. Phase 5 wires in the agent's
// changes_summary; Phase 4 falls back to the generic format. Trailer
// block is uniform across both.
func (opr *taskOpenPR) buildPRTitleAndBody() (string, string) {
	subject := fmt.Sprintf("Tasks Step %d: %s", opr.ctx.StepIndex+1, opr.ctx.TaskTitle)
	var sb strings.Builder
	sb.WriteString("Generated-By: deployment.io Tasks\n")
	sb.WriteString(fmt.Sprintf("Task: %s\n", opr.ctx.TaskTitle))
	if len(opr.ctx.DashboardURL) > 0 {
		sb.WriteString(fmt.Sprintf("Task-URL: %s/tasks/%s\n", strings.TrimRight(opr.ctx.DashboardURL, "/"), opr.ctx.TaskID))
	}
	return subject, sb.String()
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

