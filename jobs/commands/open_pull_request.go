package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

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
// MarkStepDone fires on error via the deferred cleanup; the success
// cleanup is handled by the outer executeJobs loop after the full
// command sequence completes, so this command stays symmetric with
// the other Tasks commands and the cleanup site doesn't shift if a
// new command lands after OpenPullRequest.
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
		agentPRTitle: readAgentPRTitleFromJobOutput(parameters),
	}
	prOutputs, err := opener.openAll(hasChangesByIndex)
	if err != nil {
		return parameters, err
	}
	if err := opener.mergeIntoJobOutput(parameters, prOutputs); err != nil {
		return parameters, fmt.Errorf("error merging PR results into job output: %s", err)
	}
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
	// agentPRTitle is the agent-produced short PR title from /result.json
	// (newer agentbox images that emit pr_title). Empty when the agentbox
	// image predates the field — subjectAndLeadIn falls through to the
	// truncated-first-line-of-changes_summary path.
	agentPRTitle string
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

// buildPRTitleAndBody assembles the PR subject + body. Subject source
// is decided by subjectAndLeadIn (agent pr_title > changes_summary
// first line > generic fallback; see that function for the policy).
//
// Body composition: lead-in first (when present) so reviewers see the
// agent's narrative before metadata, then the trailer block, then the
// optional denied-hosts section. When the Step had allowlist denies,
// the section lists the blocked hostnames so the PR reviewer can see
// what the agent tried to reach — helps diagnose "agent gave up
// because it couldn't fetch X" without digging through container logs.
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

// subjectAndLeadIn picks the PR subject + body lead-in from whichever
// source is available, with defensive truncation against agents that
// ignore the 72-char instruction:
//
//   1. Newer agentbox (v1.x+ with pr_title): use agentPRTitle, capped
//      at 72 chars. Full changes_summary becomes the lead-in body.
//   2. Older agentbox (no pr_title): split changes_summary at its
//      first newline. Short first line → use as-is; long first line →
//      cap AND keep the full narrative as the body so reviewers don't
//      lose context.
//   3. No agent output at all: generic "Tasks Step N: <title>".
//
// Bug 2 fix: the pre-fix code emitted the entire first line as the PR
// title regardless of length. A 119-char single-line narrative ended
// up as the literal PR title. capTitle prevents that recurrence even
// if the agent doesn't follow the instruction.
func (opr *taskOpenPR) subjectAndLeadIn() (string, string) {
	if len(opr.agentPRTitle) > 0 {
		return capTitle(opr.agentPRTitle, prTitleMaxRunes), strings.TrimSpace(opr.agentSummary)
	}
	if len(opr.agentSummary) > 0 {
		first, rest := splitFirstLine(opr.agentSummary)
		if utf8.RuneCountInString(first) <= prTitleMaxRunes {
			return first, rest
		}
		return capTitle(first, prTitleMaxRunes), strings.TrimSpace(opr.agentSummary)
	}
	return fmt.Sprintf("Tasks Step %d: %s", opr.ctx.StepIndex+1, opr.ctx.TaskTitle), ""
}

// prTitleMaxRunes is the defensive cap on PR title length. Matches the
// instruction agentbox gives the agent in its system prompt (72 chars
// = Conventional Commits + GitHub PR title soft limit). Counted in
// runes, not bytes, so multi-byte titles aren't double-counted.
const prTitleMaxRunes = 72

// capTitle truncates s to at most n runes (not bytes — respects
// multi-byte chars). Appends "…" when truncation occurred so the title
// visibly signals that it was clipped.
func capTitle(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

// splitFirstLine returns (firstLine, rest), both trimmed of surrounding
// whitespace. When s has no newline, returns (trimmed s, "").
func splitFirstLine(s string) (string, string) {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:])
	}
	return strings.TrimSpace(s), ""
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

// readAgentPRTitleFromJobOutput pulls the agent.pr_title field that
// RunAgentStep populated from agentbox's /result.json. Returns "" on
// missing/malformed payloads so the caller's fallback path (truncated
// first line of changes_summary) fires — keeps backward-compat with
// older agentbox images that didn't emit pr_title.
func readAgentPRTitleFromJobOutput(parameters map[string]interface{}) string {
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
	return data.Agent.PRTitle
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

