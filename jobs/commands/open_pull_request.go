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
	"github.com/deployment-io/deployment-runner-kit/task_previews"
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
		acceptance:   readAcceptanceCriteria(parameters),
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
	// verifyResult is agentbox's verify_result for this Step run (after review
	// fixes, the last kept fix run's), read back off the same envelope. It is
	// what "How it was checked" reports. Nil for older agentbox images.
	verifyResult *verifyResult
	// review is the Review stage's record for this Step run. Nil when the
	// stage did not run — a Task with review participation Off, or a Job
	// created before the stage existed — in which case the PR body carries no
	// Review section and looks exactly as it did before.
	review *reviewOutput
	// acceptance is the Task's acceptance criteria, read off the ReviewSpec
	// parameter (see readAcceptanceCriteria). Nil for a Task without a
	// structured spec, in which case the body has no "What the Task asks for".
	acceptance []string
	// pullRequests is the deployment-server RPC surface this command uses.
	// Nil means the runner client; tests substitute a stub.
	pullRequests pullRequestRPC
	// taskPreviews lists the Task's previews for the body's Preview part. Nil
	// means the runner client; tests substitute a stub.
	taskPreviews taskPreviewRPC
}

// taskPreviewRPC is the part of the runner client that lists a Task's previews.
type taskPreviewRPC interface {
	ListTaskPreviews(organizationID, taskID string) ([]task_previews.TaskPreviewV1, error)
}

func (opr *taskOpenPR) previewRPC() taskPreviewRPC {
	if opr.taskPreviews != nil {
		return opr.taskPreviews
	}
	return client.Get()
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
	// "How it was checked" section is given for the line saying how many steps were
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
// The body has a fixed layout, every part left out when it has nothing to
// show: the one-line review tally, the Task's previews, "Before deploying" (the configuration the
// change needs that its environment lacks), the agent's summary, what the Task
// asks for, how the change was checked, the Review section, the blocked hosts, and
// the trailer last. What a reviewer reads first is what the review did and
// what the change is; the reference material sits below it, collapsed.
//
// EVERYTHING EXCEPT THE SUMMARY IS BUILT FIRST and the summary is given what
// is left of prBodyMaxRunes. Each of those parts is bounded — the Review
// section by reviewSectionMaxRunes, the Preview part by its line and name caps, "Before deploying" by its entry cap and
// its capped names, the criteria and blocked hosts by their
// item caps, a verify step's output by boundVerifyTail — and the agent's
// summary is the one part that is not, so it is the part that yields when the
// body has to fit.
//
// The body as a whole is bounded LAST and unconditionally. Giving the summary
// what is left over is what keeps an ordinary pull request readable; it is not
// what makes the cap hold. Those parts' own bounds are several, and several
// bounded parts still add up — plus the trailer carries a Task title of any
// length. The last cut is the one that guarantees the pull request opens.
func (opr *taskOpenPR) buildPRTitleAndBody() (string, string) {
	subject, leadIn := opr.subjectAndLeadIn()
	tally := reviewTally(opr.review)
	preview := opr.previewSection()
	deploy := beforeDeployingSection(opr.review, opr.ctx.DashboardURL)
	asks := acceptanceSection(opr.acceptance)
	review := strings.Trim(opr.reviewSection(), "\n")
	hosts := opr.blockedHostsSection()
	trailer := opr.trailer()
	// "How it was checked" is the one part here with no bound of its own — a
	// step's output is bounded by boundVerifyTail, but the number of failing
	// steps follows the Task's repositories — so it is given what the rest of
	// the body leaves, less the separator in front of it.
	others := joinBodyParts(tally, preview, deploy, asks, review, hosts, trailer) + "\n"
	checked := opr.howCheckedSection(prBodyMaxRunes - utf8.RuneCountInString(others) - len(bodyPartSeparator))
	rest := joinBodyParts(tally, preview, deploy, asks, checked, review, hosts, trailer) + "\n"
	leadIn = boundPRSummary(leadIn, prBodyMaxRunes-utf8.RuneCountInString(rest)-len(bodyPartSeparator))
	body := joinBodyParts(tally, preview, deploy, leadIn, asks, checked, review, hosts, trailer) + "\n"
	return subject, cutToRuneBudget(body, prBodyMaxRunes, prBodyTruncatedNote)
}

// bodyPartSeparator is the blank line between two parts of the body.
const bodyPartSeparator = "\n\n"

// joinBodyParts joins the non-empty parts with a blank line between each, so
// a part with nothing to show leaves no gap behind.
func joinBodyParts(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.Trim(p, "\n"); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, bodyPartSeparator)
}

// trailer is the metadata block that closes every body. Never collapsed: the
// lines are what tooling and a reader looking for the Task page key on.
func (opr *taskOpenPR) trailer() string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("Generated-By: deployment.io Tasks\n")
	sb.WriteString(fmt.Sprintf("Task: %s\n", opr.ctx.TaskTitle))
	if len(opr.ctx.DashboardURL) > 0 {
		sb.WriteString(fmt.Sprintf("Task-URL: %s/tasks/%s\n", strings.TrimRight(opr.ctx.DashboardURL, "/"), opr.ctx.TaskID))
	}
	return sb.String()
}

// Bounds on the Preview part.
const (
	prBodyPreviewMaxItems     = 5
	prBodyPreviewNameMaxRunes = 100
)

// previewSection lists the Task's previews so the approver can open them. The
// previews are the Task's (not this repository's), so a multi-repo Task shows
// the same list on every pull request. Empty when the Task has no previews or
// they can't be listed — the pull request never fails over it.
func (opr *taskOpenPR) previewSection() string {
	previews, err := opr.previewRPC().ListTaskPreviews(opr.ctx.OrganizationID, opr.ctx.TaskID)
	if err != nil {
		if opr.logsWriter != nil {
			io.WriteString(opr.logsWriter, fmt.Sprintf("Could not list the Task's previews for the pull request description: %s\n", err))
		}
		return ""
	}
	return formatPreviewSection(previews)
}

// formatPreviewSection renders the Preview part: at most prBodyPreviewMaxItems
// lines, each name cut to prBodyPreviewNameMaxRunes. Empty for no previews.
func formatPreviewSection(previews []task_previews.TaskPreviewV1) string {
	if len(previews) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("**Preview**\n\n")
	sb.WriteString("The agent deployed this Task's change to a preview while working on it:\n\n")
	shown := previews
	if len(shown) > prBodyPreviewMaxItems {
		shown = shown[:prBodyPreviewMaxItems]
	}
	for _, p := range shown {
		name := p.ServiceName
		if r := []rune(name); len(r) > prBodyPreviewNameMaxRunes {
			name = string(r[:prBodyPreviewNameMaxRunes])
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n", name, p.URL))
	}
	if omitted := len(previews) - len(shown); omitted > 0 {
		sb.WriteString(fmt.Sprintf("- and %d more on the Task page\n", omitted))
	}
	sb.WriteString("\nA preview shows the code as of the agent's last deploy to it, which can be earlier than this pull request's final commit. ")
	sb.WriteString("Previews are removed about 72 hours after the Task's last preview deploy, or when the Task is deleted.\n")
	return sb.String()
}

// blockedHostsSection lists the hostnames the agentbox proxy denied during
// this Step, so the PR reviewer can see what the agent tried to reach — helps
// diagnose "agent gave up because it couldn't fetch X" without digging
// through container logs. Collapsed, with the count in its summary: it is
// reference material, not what a reviewer reads first. Empty when nothing was
// denied.
func (opr *taskOpenPR) blockedHostsSection() string {
	if len(opr.deniedHosts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<details><summary>Hosts the agent was blocked from reaching (%d)</summary>\n\n", len(opr.deniedHosts)))
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
	sb.WriteString("\n</details>\n")
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
	// Room for closing a collapsed block the cut lands in. The body's blocks
	// are never nested, so a cut can leave at most one open.
	if strings.Contains(text, detailsOpenTag) {
		allowance -= utf8.RuneCountInString(detailsCloseTag)
	}
	if allowance <= 0 {
		return ""
	}
	kept := string([]rune(text)[:allowance])
	// The index is a byte offset into a valid UTF-8 string at a '\n', which is
	// always a character boundary.
	if idx := strings.LastIndexByte(kept, '\n'); idx > 0 {
		kept = kept[:idx]
	}
	return closeOpenDetails(strings.TrimRight(kept, "\n ")) + note
}

const (
	detailsOpenTag = "<details>"
	// detailsCloseTag closes a collapsed block a cut left open, after a blank
	// line so the markdown inside it still renders.
	detailsCloseTag = "\n\n</details>"
)

// closeOpenDetails closes every <details> block the text opens and does not
// close. On GitHub an unclosed block swallows everything after it, so a cut
// that lands inside one would hide the rest of the body.
func closeOpenDetails(text string) string {
	if open := strings.Count(text, detailsOpenTag) - strings.Count(text, "</details>"); open > 0 {
		return text + strings.Repeat(detailsCloseTag, open)
	}
	return text
}

// Bounds on "What the Task asks for".
const (
	prBodyAcceptanceMaxItems = 20
	prBodyAcceptanceMaxRunes = 300
)

// readAcceptanceCriteria reads the Task's acceptance criteria off the Job's
// ReviewSpec parameter, which kit stamps on every Step Job.
//
// For a Task with a structured spec the parameter is kit's TaskSpec
// marshalled with no json tags, so the criteria sit under "Acceptance"; for a
// Task without one it is the prose Description, which carries no criteria.
// Only a value that is a JSON object is decoded, and only that one key is
// read: nothing else from the spec belongs in the body. Nil when there are no
// non-blank criteria — the section is then left out.
//
// The criteria are the Task's, not the Step's, so a multi-Step Task shows all
// of them on every Step's pull request.
func readAcceptanceCriteria(parameters map[string]interface{}) []string {
	spec, err := jobs.GetParameterValue[string](parameters, parameters_enums.ReviewSpec)
	if err != nil {
		return nil
	}
	return parseAcceptanceCriteria(spec)
}

func parseAcceptanceCriteria(spec string) []string {
	trimmed := strings.TrimSpace(spec)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return nil
	}
	raw, ok := fields["Acceptance"]
	if !ok {
		return nil
	}
	var criteria []string
	if err := json.Unmarshal(raw, &criteria); err != nil {
		return nil
	}
	lineBreaks := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")
	var out []string
	for _, c := range criteria {
		// A criterion must stay one bullet.
		c = strings.TrimSpace(lineBreaks.Replace(c))
		if c == "" {
			continue
		}
		out = append(out, capRunes(c, prBodyAcceptanceMaxRunes))
	}
	return out
}

// acceptanceSection renders "What the Task asks for": the Task's acceptance
// criteria, one bullet each, so a reviewer reads the change against what was
// asked. Empty when there are none.
func acceptanceSection(criteria []string) string {
	if len(criteria) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("**What the Task asks for**\n\n")
	shown := criteria
	if len(shown) > prBodyAcceptanceMaxItems {
		shown = shown[:prBodyAcceptanceMaxItems]
	}
	for _, c := range shown {
		sb.WriteString("- " + c + "\n")
	}
	if omitted := len(criteria) - len(shown); omitted > 0 {
		sb.WriteString(fmt.Sprintf("- and %d more on the Task page\n", omitted))
	}
	return sb.String()
}

// howCheckedSection renders "How it was checked": deployment.io's own verify
// result for the code in this pull request. After review fixes it is the last
// kept fix run's — a fix run that failed its verify is rolled back and never
// merged — so it always describes the code under review. It reports only the
// platform's check: the agent may have run builds and tests of its own.
//
// Empty for an agentbox older than verify_result.
//
// budget is the runes the rest of the body left for it. Passing steps' one-line
// bullets are always shown, as is the first failing step with its output; past
// the budget, further failing steps are left out and counted, rather than
// pushing the parts below past the provider's limit.
func (opr *taskOpenPR) howCheckedSection(budget int) string {
	vr := opr.verifyResult
	if vr == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("**How it was checked**\n\n")
	if !vr.Ran {
		if reason := strings.TrimSpace(vr.SkippedReason); reason != "" {
			sb.WriteString(fmt.Sprintf("deployment.io did not run a build or test check: %s.\n", strings.TrimRight(reason, ".")))
		} else {
			sb.WriteString("deployment.io did not run a build or test check.\n")
		}
		return sb.String()
	}
	if len(vr.Steps) == 0 {
		sb.WriteString(fmt.Sprintf("- `%s` %s\n", verifyCommandLabel(vr.Command), passedOrFailed(vr.Passed)))
		return sb.String()
	}
	// Passing steps are always shown, so their cost comes off the top; the
	// failing steps share what is left, in the order agentbox reported them.
	budget -= utf8.RuneCountInString(sb.String()) + prBodyVerifyOverflowReserveRunes
	for _, s := range vr.Steps {
		if s.Passed {
			budget -= utf8.RuneCountInString(verifyStepEntry(s))
		}
	}
	shownFailing, omitted := 0, 0
	for _, s := range vr.Steps {
		entry := verifyStepEntry(s)
		if !s.Passed {
			cost := utf8.RuneCountInString(entry)
			// The first failing step is always rendered: its own output is
			// bounded, and a section that reports failures without naming one
			// reports nothing.
			if shownFailing > 0 && cost > budget {
				omitted++
				continue
			}
			budget -= cost
			shownFailing++
		}
		sb.WriteString(entry)
	}
	if omitted > 0 {
		sb.WriteString(fmt.Sprintf("- and %d more failing step(s) — the full output is in the Step's job log\n", omitted))
	}
	return sb.String()
}

func passedOrFailed(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

// verifyStepEntry renders one step: the command, the repo it ran in, and for a
// failure, whether the base commit fails the same way and the tail of what it
// printed, collapsed.
func verifyStepEntry(s verifyStep) string {
	command, repo := verifyCommandLabel(s.Command), verifyStepRepoLabel(s.Repo)
	if s.Passed {
		return fmt.Sprintf("- `%s` passed in `%s`\n", command, repo)
	}
	var sb strings.Builder
	if s.BaselineRan && !s.BaselinePassed {
		sb.WriteString(fmt.Sprintf("- `%s` failed in `%s` — it fails on the base commit too, so this change did not cause it\n", command, repo))
	} else {
		sb.WriteString(fmt.Sprintf("- `%s` failed in `%s`\n", command, repo))
	}
	if tail := boundVerifyTail(verifyStepTail(s)); tail != "" {
		sb.WriteString("\n<details><summary>Output</summary>\n\n```\n" + tail + "\n```\n\n</details>\n\n")
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
// the worst case is a PR body without the "How it was checked" section, and the job
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
