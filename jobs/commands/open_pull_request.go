package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
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
		verifyResult: readVerifyResultFromJobOutput(parameters),
		review:       readReviewFromJobOutput(parameters),
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
	// verifyResult is agentbox's verify_result for this Step run, read back
	// off the same envelope. Non-nil with pre-existing steps only when the
	// commit gate let a failing verification through — the case the PR body
	// has to explain. Nil otherwise (including for older agentbox images).
	verifyResult *verifyResult
	// review is the Review stage's record for this Step run. Nil when the
	// stage did not run — a Task with review participation Off, or a Job
	// created before the stage existed — in which case the PR body carries no
	// Review section and looks exactly as it did before.
	review *reviewOutput
	// pullRequests is the deployment-server RPC surface this command uses.
	// Nil means the runner client; tests substitute a stub.
	pullRequests pullRequestRPC
}

// pullRequestRPC is the part of the runner client that opens pull requests and
// posts review comments on them.
type pullRequestRPC interface {
	OpenPullRequest(organizationID string, args oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error)
	PostPullRequestReview(organizationID string, args oauth.PostPullRequestReviewArgsV1) (oauth.PostPullRequestReviewDtoV1, error)
}

func (opr *taskOpenPR) rpc() pullRequestRPC {
	if opr.pullRequests != nil {
		return opr.pullRequests
	}
	return client.Get()
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
	// An unresolved must-fix finding asks for BOTH a draft and a title that
	// says so. The draft is the guard: it cannot be merged by accident. The
	// title is the signal: it is what shows in a pull-request list, a chat
	// notification and an email, none of which show draft state — and it
	// survives someone clicking "Ready for review" before the findings are
	// fixed. Where the provider has no drafts, or the pull request already
	// exists and its draft state can no longer be set, the title is also
	// the only one of the two the provider can honour. Either way a pull
	// request exists and the work is not discarded.
	needsFixes := opr.needsFixes()
	if needsFixes {
		title = prefixNeedsFixes(title)
	}
	dto, err := opr.rpc().OpenPullRequest(opr.ctx.OrganizationID, oauth.OpenPullRequestArgsV1{
		InstallationID: entry.InstallationID,
		RepoName:       entry.Name,
		BaseBranch:     entry.BaseBranch,
		HeadBranch:     opr.ctx.BranchName,
		Title:          title,
		Body:           body,
		Draft:          needsFixes,
	})
	if err != nil {
		return repoOutput{}, err
	}
	io.WriteString(opr.logsWriter, fmt.Sprintf("Opened PR #%d for repo %s: %s\n", dto.Number, entry.Name, dto.URL))
	// Never fails the Step: the pull request exists and its description
	// carries every finding; the inline comments are a view onto it.
	opr.postReviewComments(idx, entry, dto.Number)
	return repoOutput{
		Index:      idx,
		Name:       entry.Name,
		HasChanges: true,
		Branch:     opr.ctx.BranchName,
		PRURL:      dto.URL,
		PRNumber:   dto.Number,
	}, nil
}

// needsFixes reports whether this pull request carries an unresolved must-fix
// finding — the one condition that asks for a draft and, failing that, for a
// title that says so.
//
// Advisory never qualifies: its findings are annotations, and holding a pull
// request open over them is precisely what the Task opted out of.
func (opr *taskOpenPR) needsFixes() bool {
	return opr.review != nil && opr.review.MustFixOpen && opr.review.Participation == "on"
}

// Bounds on the pull-request body as a whole.
//
// GitHub rejects a body over 65,536 characters, and a rejected body is a pull
// request that never opens — the Step's work would be pushed with nothing
// pointing at it. 60,000 leaves margin for a provider that counts differently
// than we do.
const (
	prBodyMaxRunes = 60000
	// prBodyDeniedHostsMaxItems bounds the blocked-host list. A run that was
	// denied hundreds of hosts was denied the same handful over and over in
	// practice; the count in the overflow line is what the reader needs past
	// the first fifty names.
	prBodyDeniedHostsMaxItems = 50
	// prBodyVerifyOverflowReserveRunes is held back from whatever budget the
	// verification section is given for the line saying how many steps were
	// left out, so the note about what was dropped is never itself the thing
	// that gets dropped.
	prBodyVerifyOverflowReserveRunes = 200
	// prSummaryTruncatedNote goes where the summary was cut. The Task-URL
	// line above it is what "the Task page" refers to, so the note needs no
	// link of its own.
	prSummaryTruncatedNote = "\n\n_The summary was truncated. The full text is on the Task page._"
	// prBodyTruncatedNote goes at the end of a body that had to be cut below
	// the summary — the last guard's note, for the case where dropping the
	// summary entirely still left the body over the cap.
	prBodyTruncatedNote = "\n\n_This description was truncated to fit the provider's limit on pull-request bodies. The full text is on the Task page._"
)

// buildPRTitleAndBody assembles the PR subject + body. Subject source
// is decided by subjectAndLeadIn (agent pr_title > changes_summary
// first line > generic fallback; see that function for the policy).
//
// Body composition: lead-in first (when present) so reviewers see the
// agent's narrative before metadata, then the trailer block, then the
// optional denied-hosts, verification and Review sections.
//
// EVERYTHING BELOW THE SUMMARY IS BUILT FIRST and the summary is given what
// is left of prBodyMaxRunes. Each of those sections is bounded — the Review
// section by reviewSectionMaxRunes, the blocked hosts by
// prBodyDeniedHostsMaxItems, a verify step's output by boundVerifyTail — and
// the agent's summary is the one part that is not, so it is the part that
// yields when the body has to fit.
//
// The body as a whole is bounded LAST and unconditionally. Giving the summary
// what is left over is what keeps an ordinary pull request readable; it is not
// what makes the cap hold. Those sections' own bounds are several, and several
// bounded parts still add up — plus the trailer carries a Task title of any
// length. The last cut is the one that guarantees the pull request opens.
func (opr *taskOpenPR) buildPRTitleAndBody() (string, string) {
	subject, leadIn := opr.subjectAndLeadIn()
	below := opr.bodyBelowSummary()
	// Less two runes for the blank line between the summary and what follows.
	leadIn = boundPRSummary(leadIn, prBodyMaxRunes-utf8.RuneCountInString(below)-2)
	var sb strings.Builder
	if len(leadIn) > 0 {
		sb.WriteString(leadIn)
		sb.WriteString("\n\n")
	}
	sb.WriteString(below)
	return subject, cutToRuneBudget(sb.String(), prBodyMaxRunes, prBodyTruncatedNote)
}

// bodyBelowSummary is everything after the agent's narrative: the trailer
// block, then the optional denied-hosts, verification and Review sections.
//
// The verification section is the one part here with no bound of its own — a
// step's output is bounded by boundVerifyTail, but the number of failing steps
// follows the Task's repositories — so it is given what the rest of this block
// leaves of the body. That budget is the whole body's, not a smaller one of its
// own: as long as the body fits, every pre-existing failure is named, which is
// what this section exists to do.
func (opr *taskOpenPR) bodyBelowSummary() string {
	var sb strings.Builder
	sb.WriteString("Generated-By: deployment.io Tasks\n")
	sb.WriteString(fmt.Sprintf("Task: %s\n", opr.ctx.TaskTitle))
	if len(opr.ctx.DashboardURL) > 0 {
		sb.WriteString(fmt.Sprintf("Task-URL: %s/tasks/%s\n", strings.TrimRight(opr.ctx.DashboardURL, "/"), opr.ctx.TaskID))
	}
	sb.WriteString(opr.blockedHostsSection())
	review := opr.reviewSection()
	sb.WriteString(opr.verificationSection(prBodyMaxRunes - utf8.RuneCountInString(sb.String()) - utf8.RuneCountInString(review)))
	sb.WriteString(review)
	return sb.String()
}

// blockedHostsSection lists the hostnames the agentbox proxy denied during
// this Step, so the PR reviewer can see what the agent tried to reach — helps
// diagnose "agent gave up because it couldn't fetch X" without digging
// through container logs. Empty when nothing was denied.
func (opr *taskOpenPR) blockedHostsSection() string {
	if len(opr.deniedHosts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n---\n")
	sb.WriteString("**Network: blocked hosts during this Step**\n\n")
	sb.WriteString("The agent attempted to reach the following hostnames but they weren't on the allowlist. Add them to your org's Tasks → Allowed Hosts settings if expected:\n\n")
	shown := opr.deniedHosts
	if len(shown) > prBodyDeniedHostsMaxItems {
		shown = shown[:prBodyDeniedHostsMaxItems]
	}
	for _, h := range shown {
		sb.WriteString(fmt.Sprintf("- `%s`\n", h))
	}
	if omitted := len(opr.deniedHosts) - len(shown); omitted > 0 {
		sb.WriteString(fmt.Sprintf("- and %d more\n", omitted))
	}
	return sb.String()
}

// boundPRSummary fits the agent's narrative into the runes the rest of the
// body left for it.
func boundPRSummary(summary string, budget int) string {
	return cutToRuneBudget(summary, budget, prSummaryTruncatedNote)
}

// cutToRuneBudget cuts text to a rune budget, note included, and appends the
// note in place of what it removed.
//
// Cut at the LAST LINE BREAK that fits, so what is kept ends on a whole line
// rather than mid-sentence, and say that it was cut — an unmarked truncation
// reads as text that simply stopped having anything to say. Counted in runes
// and sliced on runes, so a multi-byte character is never split.
//
// A budget too small for even the note returns nothing: the cap is what keeps
// the pull request openable, and overrunning it to explain itself would
// defeat the point.
func cutToRuneBudget(text string, budget int, note string) string {
	if utf8.RuneCountInString(text) <= budget {
		return text
	}
	allowance := budget - utf8.RuneCountInString(note)
	if allowance <= 0 {
		return ""
	}
	kept := string([]rune(text)[:allowance])
	// The index is a byte offset into a valid UTF-8 string at a '\n', which is
	// always a character boundary.
	if idx := strings.LastIndexByte(kept, '\n'); idx > 0 {
		kept = kept[:idx]
	}
	return strings.TrimRight(kept, "\n ") + note
}

// verificationSection reports a verification that failed but was allowed
// through because the same failure is present on the base commit.
//
// This PR exists only because of that exemption — without it the Step would
// have been discarded — so the exemption belongs where the reviewer is, not
// buried in a job log they'd have to know to open. Empty string when nothing
// was pre-existing, which is every ordinary PR: a green verify has nothing to
// say and a genuinely new failure never reaches PR-open at all.
// budget is the runes the rest of the body below the summary left for it; a
// Task failing across enough repositories to exhaust the whole body reports
// what it can and says how many steps it left out, rather than pushing the
// Review section below it past the provider's limit.
func (opr *taskOpenPR) verificationSection(budget int) string {
	steps := preExistingVerifySteps(opr.verifyResult)
	if len(steps) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n---\n")
	sb.WriteString("**Verification: failing before this Step**\n\n")
	sb.WriteString("The agent's build/test check failed, but the same command fails on the base commit too — so this Step didn't introduce it, and the work was committed rather than discarded:\n\n")
	// Spend the budget over the steps in the order agentbox reported them, and
	// name what was left out.
	budget -= utf8.RuneCountInString(sb.String()) + prBodyVerifyOverflowReserveRunes
	shown := 0
	for _, s := range steps {
		entry := verifyStepEntry(s)
		cost := utf8.RuneCountInString(entry)
		// The first step is always rendered: its own output is bounded, and a
		// section that reports failures without naming one reports nothing.
		if shown > 0 && cost > budget {
			break
		}
		sb.WriteString(entry)
		budget -= cost
		shown++
	}
	if omitted := len(steps) - shown; omitted > 0 {
		sb.WriteString(fmt.Sprintf("- and %d more failing step(s) — the full output is in the Step's job log\n", omitted))
	}
	return sb.String()
}

// verifyStepEntry renders one pre-existing failure: the command, the repo it
// ran in, and the tail of what it printed.
func verifyStepEntry(s verifyStep) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("- `%s` in `%s` — fails on the base commit as well\n",
		verifyCommandLabel(s.Command), verifyStepRepoLabel(s.Repo)))
	if tail := boundVerifyTail(verifyStepTail(s)); tail != "" {
		sb.WriteString("\n```\n" + tail + "\n```\n")
	}
	return sb.String()
}

// subjectAndLeadIn picks the PR subject + body lead-in from whichever
// source is available, with defensive truncation against agents that
// ignore the 72-char instruction:
//
//  1. Newer agentbox (v1.x+ with pr_title): use agentPRTitle, capped
//     at 72 chars. Full changes_summary becomes the lead-in body.
//  2. Older agentbox (no pr_title): split changes_summary at its
//     first newline. Short first line → use as-is; long first line →
//     cap AND keep the full narrative as the body so reviewers don't
//     lose context.
//  3. No agent output at all: generic "Tasks Step N: <title>".
//
// Bug 2 fix: the pre-fix code emitted the entire first line as the PR
// title regardless of length. A 119-char single-line narrative ended
// up as the literal PR title. capTitle prevents that recurrence even
// if the agent doesn't follow the instruction.
// Every path strips a "[Needs fixes] " prefix before capping. The title is
// regenerated each attempt from the agent's own words, and an agent that read
// the previous attempt's title off the pull request would hand the marker
// back — leaving a resolved Step still announcing that it needs fixes. The
// marker is re-applied, once, only when this attempt still has an open
// must-fix finding (see openOne).
func (opr *taskOpenPR) subjectAndLeadIn() (string, string) {
	if len(opr.agentPRTitle) > 0 {
		return capTitle(stripNeedsFixesPrefix(opr.agentPRTitle), prTitleMaxRunes), strings.TrimSpace(opr.agentSummary)
	}
	if len(opr.agentSummary) > 0 {
		first, rest := splitFirstLine(opr.agentSummary)
		first = stripNeedsFixesPrefix(first)
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

// readVerifyResultFromJobOutput pulls agentbox's verify_result that
// RunAgentStep wrote to the accumulated JobOutput. Nil on a missing or
// malformed payload — the PR is the user-facing artifact and must still land;
// the worst case is a PR body without the Verification section, and the job
// log still carries the warning. Mirrors readDeniedHostsFromJobOutput; both
// read different fields off the same envelope.
func readVerifyResultFromJobOutput(parameters map[string]interface{}) *verifyResult {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil
	}
	if data.Agent == nil {
		return nil
	}
	return data.Agent.VerifyResult
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
