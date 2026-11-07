package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// The builders of a Step's structured progress events (tasks.TaskEventV1).
//
// Each builds one event from what the stage already has — a round's output, a
// QA result, a preview deploy, a pull request — and every word of text in it is
// text the runner already writes to the job log or the pull request. Only the
// fields listed on the event types: never a password, a token, a cookie, a
// role's email, a secret's value, a migrate or seed command's text or output,
// or a request's or response's headers or body. Text is passed as built;
// deployment-server's Bound() caps it.
//
// They are pure so they can be tested without a stage; emitTaskEvent hands the
// result to the Job's sender (commandUtils.TaskEvents).

// taskEventOutcomeMaxRunes caps the error line a "Failed: …" or "No pull
// request: …" outcome carries.
const taskEventOutcomeMaxRunes = 200

// emitTaskEvent queues e on the Job's sender. Never blocks, never fails.
func emitTaskEvent(parameters map[string]interface{}, ctx commandUtils.TaskJobContext, logsWriter io.Writer, e tasks.TaskEventV1) {
	jobID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	emitTaskEventForJob(ctx, jobID, logsWriter, e)
}

func emitTaskEventForJob(ctx commandUtils.TaskJobContext, jobID string, logsWriter io.Writer, e tasks.TaskEventV1) {
	commandUtils.TaskEvents(ctx, jobID).LogTo(logsWriter).Emit(e)
}

func stageStartedEvent(stage string, round int, now string) tasks.TaskEventV1 {
	return tasks.TaskEventV1{
		Type:        tasks.TaskEventStageStarted,
		Stage:       stage,
		Round:       round,
		Now:         now,
		StageDetail: &tasks.TaskEventStageV1{},
	}
}

func stageFinishedEvent(stage, outcome string) tasks.TaskEventV1 {
	return tasks.TaskEventV1{
		Type:        tasks.TaskEventStageFinished,
		Stage:       stage,
		StageDetail: &tasks.TaskEventStageV1{Outcome: outcome},
	}
}

// eventErrorLine is err's first line, at most taskEventOutcomeMaxRunes runes.
func eventErrorLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > taskEventOutcomeMaxRunes {
		line = string([]rune(line)[:taskEventOutcomeMaxRunes])
	}
	return line
}

// --- Checkout ---

func checkoutDoneEvent(entries []tasks.RepositoryEntry) tasks.TaskEventV1 {
	repos := make([]string, 0, len(entries))
	for _, e := range entries {
		repos = append(repos, e.Name)
	}
	return tasks.TaskEventV1{
		Type:     tasks.TaskEventCheckoutDone,
		Stage:    tasks.TaskEventStageSetup,
		Now:      fmt.Sprintf("Setting up · checked out %d repositories", len(repos)),
		Checkout: &tasks.TaskEventCheckoutV1{Repos: repos},
	}
}

// --- Implement ---

const implementStartedNow = "Implement · the agent is working"

// selfCheckPayload is a run's verify_result as the event carries it: the
// command it ran (never its output), whether it passed, and how long it took
// or why it was skipped. Nil when the run reported none.
func selfCheckPayload(vr *verifyResult) *tasks.TaskEventSelfCheckV1 {
	if vr == nil {
		return nil
	}
	p := &tasks.TaskEventSelfCheckV1{Passed: vr.Ran && vr.Passed}
	if c := strings.TrimSpace(vr.Command); c != "" {
		p.Commands = []string{c}
	}
	if !vr.Ran {
		p.Summary = strings.TrimSpace(vr.SkippedReason)
		if p.Summary == "" {
			p.Summary = "skipped"
		}
		return p
	}
	verdict := "failed"
	if vr.Passed {
		verdict = "passed"
	}
	if vr.DurationMs > 0 {
		p.Summary = fmt.Sprintf("%s in %s", verdict, (time.Duration(vr.DurationMs) * time.Millisecond).Round(time.Second))
	} else {
		p.Summary = verdict
	}
	return p
}

// selfCheckEvent is the self_check event of a run, ok false when the run
// reported no verify_result.
func selfCheckEvent(stage string, round int, vr *verifyResult) (tasks.TaskEventV1, bool) {
	p := selfCheckPayload(vr)
	if p == nil {
		return tasks.TaskEventV1{}, false
	}
	return tasks.TaskEventV1{Type: tasks.TaskEventSelfCheck, Stage: stage, Round: round, SelfCheck: p}, true
}

// implementFinishedEvent is the implement run's usage and summary. Diffstat
// stays empty: the per-repository change size comes later.
func implementFinishedEvent(result agentResult) tasks.TaskEventV1 {
	return tasks.TaskEventV1{
		Type:  tasks.TaskEventImplementFinished,
		Stage: tasks.TaskEventStageImplement,
		Now:   fmt.Sprintf("Implement · done in %d turns", result.Turns),
		Implement: &tasks.TaskEventImplementV1{
			Turns:          result.Turns,
			InputTokens:    result.TokenUsage.InputTokens,
			OutputTokens:   result.TokenUsage.OutputTokens,
			ChangesSummary: result.ChangesSummary,
		},
	}
}

// implementOutcome is the Implement stage's stage_finished outcome.
func implementOutcome(err error) string {
	if err == nil {
		return "Done"
	}
	return "Failed: " + eventErrorLine(err)
}

// --- Review ---

func reviewRoundStartedEvent(round int) tasks.TaskEventV1 {
	return stageStartedEvent(tasks.TaskEventStageReview, round, fmt.Sprintf("Review · round %d", round))
}

// reviewRoundFinishedEvent is round n's result: the findings no earlier round
// reported (earlierKeys), the reviewer's status for each earlier finding it was
// sent, and the round's coverage. The "now" line counts the round's findings as
// its log line does.
func reviewRoundFinishedEvent(round int, findings []reviewFindingOutput, earlierKeys map[string]bool, result agentResult) tasks.TaskEventV1 {
	payload := &tasks.TaskEventReviewRoundV1{
		Findings: []tasks.TaskEventFindingV1{},
		Previous: []tasks.TaskEventFindingStatusV1{},
		Coverage: []tasks.TaskEventCoverageV1{},
	}
	mustFix := 0
	for _, f := range findings {
		if f.MustFix {
			mustFix++
		}
		if earlierKeys[findingKey(f)] {
			continue
		}
		payload.Findings = append(payload.Findings, tasks.TaskEventFindingV1{
			Key:       findingKey(f),
			Parameter: f.Parameter,
			Severity:  f.Severity,
			Location:  f.Location,
			What:      f.What,
			Why:       f.Why,
			MustFix:   f.MustFix,
		})
	}
	if rr := result.ReviewResult; rr != nil {
		for _, p := range rr.Previous {
			key := strings.TrimSpace(p.Key)
			if key == "" {
				continue
			}
			// The stage's own reading: only "resolved" clears a finding,
			// anything else holds it.
			status := tasks.TaskEventFindingStillPresent
			if strings.EqualFold(strings.TrimSpace(p.Status), reviewStatusResolved) {
				status = tasks.TaskEventFindingResolved
			}
			payload.Previous = append(payload.Previous, tasks.TaskEventFindingStatusV1{Key: key, Status: status})
		}
		for _, c := range rr.Coverage {
			payload.Coverage = append(payload.Coverage, tasks.TaskEventCoverageV1{Parameter: c.Parameter, State: c.State, Reason: c.Reason})
		}
	}
	return tasks.TaskEventV1{
		Type:        tasks.TaskEventReviewRoundFinished,
		Stage:       tasks.TaskEventStageReview,
		Round:       round,
		Now:         fmt.Sprintf("Review · round %d: %d findings (%d must-fix)", round, len(findings), mustFix),
		ReviewRound: payload,
	}
}

// reviewEarlierKeys are the keys of every finding the given rounds reported.
func reviewEarlierKeys(rounds []reviewRoundOutput) map[string]bool {
	keys := map[string]bool{}
	for _, r := range rounds {
		for _, f := range r.Findings {
			keys[findingKey(f)] = true
		}
	}
	return keys
}

func reviewFixStartedEvent(round int) tasks.TaskEventV1 {
	return stageStartedEvent(tasks.TaskEventStageImplement, round, fmt.Sprintf("Fixing round %d's must-fix findings", round))
}

func qaFixStartedEvent(round int) tasks.TaskEventV1 {
	return stageStartedEvent(tasks.TaskEventStageImplement, round, fmt.Sprintf("Fixing QA round %d's failed acceptance criteria", round))
}

// fixRoundFinishedEvent is a fix round's end, in the stage that sent it (review
// or qa): the finding keys routed to it, and the fix run's summary and
// self-check when it ran.
func fixRoundFinishedEvent(stage string, round int, addressed []reviewFindingOutput, fixRun *agentResult) tasks.TaskEventV1 {
	payload := &tasks.TaskEventFixRoundV1{Addressed: []string{}}
	for _, f := range addressed {
		payload.Addressed = append(payload.Addressed, findingKey(f))
	}
	if fixRun != nil {
		payload.ChangesSummary = fixRun.ChangesSummary
		payload.SelfCheck = selfCheckPayload(fixRun.VerifyResult)
	}
	return tasks.TaskEventV1{Type: tasks.TaskEventFixRoundFinished, Stage: stage, Round: round, FixRound: payload}
}

// reviewStageOutcome is the Review stage's stage_finished outcome, from its
// record: "Skipped: <reason>" when no round completed, else "Must-fix open",
// "N fixed" or "No findings".
func reviewStageOutcome(out *reviewOutput, skipped string) string {
	if out == nil || len(out.Rounds) == 0 {
		if skipped == "" {
			skipped = "no review round ran"
		}
		return "Skipped: " + skipped
	}
	if latestCompletedRound(out) == nil {
		return "Skipped: " + strings.TrimRight(out.Rounds[len(out.Rounds)-1].Error, ". ")
	}
	if out.MustFixOpen {
		return "Must-fix open"
	}
	if n := len(out.FixedInLoop); n > 0 {
		return fmt.Sprintf("%d fixed", n)
	}
	return "No findings"
}

// --- QA ---

func qaRoundStartedEvent(round int) tasks.TaskEventV1 {
	return stageStartedEvent(tasks.TaskEventStageQA, round, fmt.Sprintf("QA · round %d", round))
}

// previewDeployDetails is what a backend preview's deploy did with its
// database, migrate and seed commands (previewDeployFacts), and the roles it
// seeded.
type previewDeployDetails struct {
	databaseEngine string
	migrate        *tasks.TaskEventCommandV1
	seed           *tasks.TaskEventCommandV1
	roles          []string
}

// previewDeployResult is how one preview's deploy ended: the repository's
// full name, the kind of preview, its URL or its error, and — for a backend —
// what its deploy did.
type previewDeployResult struct {
	repo    string
	kind    string
	url     string
	err     error
	details *previewDeployDetails
}

// previewDeployFinishedEvent is one preview's deploy in a QA round: its URL on
// success, the first line of its failure otherwise.
func previewDeployFinishedEvent(round int, r previewDeployResult) tasks.TaskEventV1 {
	payload := &tasks.TaskEventPreviewDeployV1{Kind: r.kind, OK: r.err == nil, Roles: []string{}}
	now := fmt.Sprintf("QA · preview of %s deployed", r.repo)
	if r.err == nil {
		payload.URL = r.url
	} else {
		payload.Error = qaFirstErrorLine(r.err, qaBackendErrorMax)
		now = fmt.Sprintf("QA · preview of %s failed", r.repo)
	}
	if d := r.details; d != nil {
		payload.DatabaseEngine = d.databaseEngine
		payload.Migrate = d.migrate
		payload.Seed = d.seed
		payload.Roles = append(payload.Roles, d.roles...)
	}
	return tasks.TaskEventV1{
		Type:          tasks.TaskEventPreviewDeployFinished,
		Stage:         tasks.TaskEventStageQA,
		Round:         round,
		Repo:          r.repo,
		Now:           now,
		PreviewDeploy: payload,
	}
}

// What a backend preview's deploy did is recorded by the runner's own deploy
// code as it happens — never read back from the job log, which the migrate and
// seed commands print their output to, so a command could print a line that
// looks like the runner's. recordPreviewDeploy wraps the deploy: it reads the
// preview database from the deploy's input, and hands the deploy a log writer
// (previewCommandLog) its one-off tasks record each command's end on
// (recordPreviewCommand). Every text is one the deploy also writes to the job
// log; the commands' text and output are never recorded.

// previewMigrationsNotSet is agenttools' "Preview database migrations: …"
// reason when the service has no preview migrate command.
const previewMigrationsNotSet = "not run: no preview migrate command is set for this service (set one in Resources → Services)"

// previewSeedNoCommand is how agenttools' test-users summary (the "Preview
// test users: …" line) begins when roles are set but no seed command is.
const previewSeedNoCommand = "roles are set but there's no preview seed command"

// previewDeployFacts is what one backend preview's deploy did, as it did it.
type previewDeployFacts struct {
	mu        sync.Mutex
	details   previewDeployDetails
	seedRoles []string
}

// recordPreviewDeploy is deps with its Deploy recording into the returned facts.
func recordPreviewDeploy(deps agenttools.DeployWebServicePreviewDeps) (agenttools.DeployWebServicePreviewDeps, *previewDeployFacts) {
	facts := &previewDeployFacts{}
	deploy := deps.Deploy
	if deploy == nil {
		return deps, facts
	}
	deps.Deploy = func(ctx context.Context, in agenttools.WebServicePreviewDeployInput) (agenttools.WebServicePreviewDeployOutput, error) {
		facts.start(in)
		if in.Logs != nil {
			in.Logs = previewCommandLog{Writer: in.Logs, facts: facts}
		}
		return deploy(ctx, in)
	}
	return deps, facts
}

// start forgets an earlier deploy's facts and records the preview database
// and the roles the seed is for.
func (f *previewDeployFacts) start(in agenttools.WebServicePreviewDeployInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details, f.seedRoles = previewDeployDetails{}, nil
	if db := in.Database; db != nil {
		// As agenttools' "Preview database: <engine> <version> (…)" line.
		f.details.databaseEngine = strings.TrimSpace(db.Engine + " " + db.Version)
		if strings.TrimSpace(db.MigrateCommand) == "" {
			f.details.migrate = &tasks.TaskEventCommandV1{Summary: previewMigrationsNotSet}
		}
	}
	if in.Seed != nil {
		for _, u := range in.Seed.Users {
			f.seedRoles = append(f.seedRoles, u.Role)
		}
	}
}

func (f *previewDeployFacts) recordCommand(name string, c tasks.TaskEventCommandV1) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case preDeployCommandName(true):
		f.details.migrate = &c
	case previewSeedCommandName:
		f.details.seed = &c
	}
}

// result is what the deploy did: the roles it seeded once its seed succeeded
// (even when a later step failed), and why no seed ran when roles are set
// without a seed command (d.TestUsers, written once the deploy succeeded).
func (f *previewDeployFacts) result(d agenttools.WebServicePreviewDeployed) *previewDeployDetails {
	f.mu.Lock()
	defer f.mu.Unlock()
	details := f.details
	if s := details.seed; s != nil && s.Ran && s.ExitCode == 0 {
		details.roles = append([]string{}, f.seedRoles...)
	}
	if details.seed == nil && strings.HasPrefix(d.TestUsers, previewSeedNoCommand) {
		details.seed = &tasks.TaskEventCommandV1{Summary: "not run: " + d.TestUsers}
	}
	return &details
}

// previewCommandLog is a backend preview deploy's log writer, unchanged, with
// the facts its one-off tasks record on.
type previewCommandLog struct {
	io.Writer
	facts *previewDeployFacts
}

// previewCommandRecorder is a log writer that records how a deploy's commands
// ended.
type previewCommandRecorder interface {
	recordPreviewCommand(name string, c tasks.TaskEventCommandV1)
}

func (l previewCommandLog) recordPreviewCommand(name string, c tasks.TaskEventCommandV1) {
	l.facts.recordCommand(name, c)
}

// recordPreviewCommand records how the command named name ended on
// logsWriter, when it is a backend preview deploy's.
func recordPreviewCommand(logsWriter io.Writer, name string, c tasks.TaskEventCommandV1) {
	if r, ok := logsWriter.(previewCommandRecorder); ok {
		r.recordPreviewCommand(name, c)
	}
}

// recordPreviewCommandNotRun records that the command named name didn't run,
// for failure — the line the deploy writes, "The <name> couldn't start: …." —
// as "not run: couldn't start: …".
func recordPreviewCommandNotRun(logsWriter io.Writer, name, failure string) {
	reason := strings.TrimSuffix(strings.TrimPrefix(failure, "The "+name+" "), ".")
	reason = strings.TrimPrefix(reason, "didn't run: ")
	recordPreviewCommand(logsWriter, name, tasks.TaskEventCommandV1{Summary: "not run: " + reason})
}

// qaCriterionEvents are round n's verdict on each criterion, in its order.
func qaCriterionEvents(out qaRoundOutput) []tasks.TaskEventV1 {
	events := make([]tasks.TaskEventV1, 0, len(out.Criteria))
	for i, c := range out.Criteria {
		events = append(events, tasks.TaskEventV1{
			Type:  tasks.TaskEventQACriterionFinished,
			Stage: tasks.TaskEventStageQA,
			Round: out.Round,
			QACriterion: &tasks.TaskEventQACriterionV1{
				Criterion: i,
				Text:      c.Criterion,
				State:     c.State,
				Reason:    c.Reason,
			},
		})
	}
	return events
}

// qaRoundFinishedEvent is round n's tally, with its log line as the summary.
func qaRoundFinishedEvent(out qaRoundOutput, final bool) tasks.TaskEventV1 {
	passed, failed, notChecked := countCriteria(out.Criteria)
	summary := strings.TrimSpace(qaRoundLogLine(out))
	var now string
	if len(out.Criteria) > 0 {
		now = fmt.Sprintf("QA · round %d: %d of %d passed", out.Round, passed, len(out.Criteria))
	} else {
		now = fmt.Sprintf("QA · round %d: %s", out.Round, strings.TrimPrefix(summary, fmt.Sprintf("QA round %d: ", out.Round)))
	}
	return tasks.TaskEventV1{
		Type:  tasks.TaskEventQARoundFinished,
		Stage: tasks.TaskEventStageQA,
		Round: out.Round,
		Now:   now,
		QARound: &tasks.TaskEventQARoundV1{
			Final:      final,
			Passed:     passed,
			Failed:     failed,
			NotChecked: notChecked,
			Summary:    summary,
		},
	}
}

// qaStageOutcome is the QA stage's stage_finished outcome: its coverage line
// ("QA: 6 of 6 passed", "QA: no acceptance criteria; checked the preview
// loads"), or why it did not check anything.
func qaStageOutcome(out *qaOutput) string {
	if out == nil {
		return "Skipped: no repository changed"
	}
	switch out.Status {
	case tasks.QAStatusSkipped:
		return "Skipped: " + out.StatusReason
	case tasks.QAStatusCouldNotRun:
		return "Couldn't run: " + out.StatusReason
	}
	final := out.finalRound()
	if out.NoCriteria || final == nil || len(final.Criteria) == 0 {
		return "QA: no acceptance criteria; checked the preview loads"
	}
	passed, _, _ := countCriteria(final.Criteria)
	return fmt.Sprintf("QA: %d of %d passed", passed, len(final.Criteria))
}

// --- Pull request ---

const prStartedNow = "Opening pull requests"

// prOpened is one repository's pull request: the server's answer, the title
// sent, whether a draft was requested, and the inline comments posted on it.
type prOpened struct {
	repo           string
	reply          oauth.OpenPullRequestDtoV1
	title          string
	draftRequested bool
	inlineComments int
}

// prOpenedEvent is one repository's pull request as the server answered: a
// draft when one was requested and honoured, updated when it already existed.
func prOpenedEvent(pr prOpened) tasks.TaskEventV1 {
	now := fmt.Sprintf("Pull request #%d opened", pr.reply.Number)
	if pr.reply.AlreadyExisted {
		now = fmt.Sprintf("Pull request #%d updated", pr.reply.Number)
	}
	return tasks.TaskEventV1{
		Type:  tasks.TaskEventPROpened,
		Stage: tasks.TaskEventStagePR,
		Repo:  pr.repo,
		Now:   now,
		PullRequest: &tasks.TaskEventPullRequestV1{
			Number:         pr.reply.Number,
			URL:            pr.reply.URL,
			Title:          pr.title,
			Draft:          pr.draftRequested && !pr.reply.DraftUnsupported,
			Updated:        pr.reply.AlreadyExisted,
			InlineComments: pr.inlineComments,
		},
	}
}

// prStageOutcome is the pull-request stage's stage_finished outcome.
func prStageOutcome(opened, updated int, err error) string {
	switch {
	case err != nil:
		return "No pull request: " + eventErrorLine(err)
	case opened > 0 && updated > 0:
		return fmt.Sprintf("%d opened, %d updated", opened, updated)
	case opened > 0:
		return fmt.Sprintf("%d opened", opened)
	case updated > 0:
		return fmt.Sprintf("%d updated", updated)
	}
	return "No pull request: no repository had changes"
}
