package commands

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

func qaCriteria(states ...string) []qaCriterion {
	out := make([]qaCriterion, 0, len(states))
	for i, s := range states {
		out = append(out, qaCriterion{Criterion: fmt.Sprintf("criterion %d", i+1), State: s, Reason: fmt.Sprintf("reason %d", i+1)})
	}
	return out
}

func completedQA(round qaRoundOutput) *qaOutput {
	round.Round, round.Status = 1, tasks.QAStatusCompleted
	return &qaOutput{Status: tasks.QAStatusCompleted, NoCriteria: round.NoCriteria, Rounds: []qaRoundOutput{round}}
}

// --- the status line ---------------------------------------------------------

func TestQAStatusLine(t *testing.T) {
	const p, f, n = tasks.QACriterionPassed, tasks.QACriterionFailed, tasks.QACriterionNotChecked
	fixed := completedQA(qaRoundOutput{Criteria: qaCriteria(p, p, f)})
	fixed.FixedByQA = []string{"criterion 1"}
	for name, tc := range map[string]struct {
		qa   *qaOutput
		want string
	}{
		"no QA":           {nil, ""},
		"all passed":      {completedQA(qaRoundOutput{Criteria: qaCriteria(p, p)}), "**QA on a preview:** 2 of 2 acceptance criteria passed"},
		"none passed":     {completedQA(qaRoundOutput{Criteria: qaCriteria(f, n)}), "**QA on a preview:** 0 of 2 acceptance criteria passed · 1 failed · 1 not checked"},
		"failed only":     {completedQA(qaRoundOutput{Criteria: qaCriteria(p, f)}), "**QA on a preview:** 1 of 2 acceptance criteria passed · 1 failed"},
		"not checked":     {completedQA(qaRoundOutput{Criteria: qaCriteria(p, n)}), "**QA on a preview:** 1 of 2 acceptance criteria passed · 1 not checked"},
		"fixed suffix":    {fixed, "**QA on a preview:** 2 of 3 acceptance criteria passed · 1 failed · 1 fixed before this PR"},
		"smoke, clean":    {completedQA(qaRoundOutput{NoCriteria: true}), "**QA on a preview:** no acceptance criteria — checked the preview loads"},
		"smoke, 1 found":  {completedQA(qaRoundOutput{NoCriteria: true, Findings: []reviewFindingOutput{{What: "a"}}}), "**QA on a preview:** no acceptance criteria — checked the preview loads · 1 problem found"},
		"smoke, 2 found":  {completedQA(qaRoundOutput{NoCriteria: true, Findings: []reviewFindingOutput{{What: "a"}, {What: "b"}}}), "**QA on a preview:** no acceptance criteria — checked the preview loads · 2 problems found"},
		"skipped":         {&qaOutput{Status: tasks.QAStatusSkipped, StatusReason: "no previewable repository"}, "**QA on a preview:** skipped — no previewable repository"},
		"couldn't run":    {&qaOutput{Status: tasks.QAStatusCouldNotRun, StatusReason: "the preview did not deploy"}, "**QA on a preview:** couldn't run — the preview did not deploy"},
		"reason is plain": {&qaOutput{Status: tasks.QAStatusSkipped, StatusReason: "ask @alice\nsee [x](y)"}, "**QA on a preview:** skipped — ask @\u200balice see \\[x\\](y)"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := qaStatusLine(tc.qa); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The line sits directly under the review tally, or opens the body without one.
func TestQAStatusLineGoesWhereTheTallyIs(t *testing.T) {
	qa := &qaOutput{Status: tasks.QAStatusSkipped, StatusReason: "no previewable repository"}
	line := "**QA on a preview:** skipped — no previewable repository"

	opr := reviewTestOpener(completedReview(false))
	opr.qa = qa
	_, body := opr.buildPRTitleAndBody()
	if !strings.HasPrefix(body, "**Review:** no findings.\n\n"+line+"\n\nAdded the login endpoint.") {
		t.Errorf("the QA line is not directly under the tally:\n%s", body)
	}

	opr = reviewTestOpener(nil)
	opr.qa = qa
	_, body = opr.buildPRTitleAndBody()
	if !strings.HasPrefix(body, line+"\n\nAdded the login endpoint.") {
		t.Errorf("without a review the QA line should open the body:\n%s", body)
	}
	// Skipped: the line says it all, so there is no section.
	if strings.Contains(body, "**QA on a preview**") {
		t.Errorf("a skipped QA got a section:\n%s", body)
	}
}

// --- the section -------------------------------------------------------------

func TestQASectionOrderAndPlacement(t *testing.T) {
	const p, f, n = tasks.QACriterionPassed, tasks.QACriterionFailed, tasks.QACriterionNotChecked
	qa := completedQA(qaRoundOutput{
		Criteria:     qaCriteria(f, n, p),
		Findings:     []reviewFindingOutput{{Location: "/login", What: "the button does nothing", Why: "nobody can log in"}},
		Observations: []string{"the page is slow"},
	})
	qa.FixedByQA = []string{"criterion 3"}
	qa.StoppedNoChange, qa.DeclinedExplanation = true, "the failure is in a third-party widget"
	opr := reviewTestOpener(completedReview(false, layoutFinding("off by one", false, false)))
	opr.qa = qa
	opr.deniedHosts = []string{"pypi.example.com"}

	_, body := opr.buildPRTitleAndBody()

	requireInOrder(t, body,
		"**QA on a preview:** 1 of 3",
		"---\n**Review**",
		"---\n**QA on a preview**",
		"does not hold this pull request",
		"_Failed_", "- criterion 1\n  reason 1\n",
		"`/login` the button does nothing", "_Why it matters:_ nobody can log in",
		"the failure is in a third-party widget",
		"<details><summary>Not checked (1)</summary>", "- criterion 2 — reason 2",
		"<details><summary>Fixed by QA (1)</summary>", "- criterion 3",
		"<details><summary>Observations (1)</summary>", "- the page is slow",
		"<details><summary>Hosts the agent",
	)
	if strings.Contains(body[strings.Index(body, "**QA on a preview**"):], "criterion 3\n  reason 3") {
		t.Errorf("a passed criterion is listed in the section:\n%s", body)
	}
}

func TestQASectionIsLeftOutWithNothingToShow(t *testing.T) {
	for name, qa := range map[string]*qaOutput{
		"all passed":                      completedQA(qaRoundOutput{Criteria: qaCriteria(tasks.QACriterionPassed)}),
		"smoke, clean":                    completedQA(qaRoundOutput{NoCriteria: true}),
		"couldn't run":                    {Status: tasks.QAStatusCouldNotRun, StatusReason: "no preview", Rounds: []qaRoundOutput{{Round: 1, Status: tasks.QAStatusCouldNotRun}}},
		"couldn't run, placeholders only": {Status: tasks.QAStatusCouldNotRun, StatusReason: "QA didn't report a result", Rounds: []qaRoundOutput{{Round: 1, Status: tasks.QAStatusCouldNotRun, Criteria: qaCriteria(tasks.QACriterionNotChecked, tasks.QACriterionNotChecked)}}},
		"gate only":                       {Status: tasks.QAStatusSkipped, StatusReason: "no static site in this change"},
	} {
		opr := reviewTestOpener(nil)
		opr.qa = qa
		if got := opr.qaSection(); got != "" {
			t.Errorf("%s: want no section, got:\n%s", name, got)
		}
	}
}

func TestQASectionIsBounded(t *testing.T) {
	var criteria []qaCriterion
	var findings []reviewFindingOutput
	var observations []string
	for i := 0; i < 30; i++ {
		criteria = append(criteria, qaCriterion{Criterion: strings.Repeat("c", 500), State: tasks.QACriterionFailed, Reason: strings.Repeat("r", 500)})
		findings = append(findings, reviewFindingOutput{Location: strings.Repeat("l", 400), What: strings.Repeat("w", 900), Why: strings.Repeat("y", 900)})
		observations = append(observations, "observation")
	}
	opr := reviewTestOpener(nil)
	opr.qa = completedQA(qaRoundOutput{Criteria: criteria, Findings: findings, Observations: observations})

	section := opr.qaSection()

	if n := utf8.RuneCountInString(section); n > qaSectionMaxRunes {
		t.Errorf("section is %d runes, want at most %d", n, qaSectionMaxRunes)
	}
	if n := strings.Count(section, "\n- "); n > qaSectionMaxItems {
		t.Errorf("section lists %d items, want at most %d", n, qaSectionMaxItems)
	}
	if !strings.Contains(section, "further QA item(s) are not shown here") {
		t.Errorf("the section does not say what it left out:\n%s", section)
	}
	if strings.Count(section, "<details>") != strings.Count(section, "</details>") {
		t.Errorf("a collapsed block is left open:\n%s", section)
	}

	// Many small items: the item cap is what binds.
	criteria = criteria[:0]
	for i := 0; i < 30; i++ {
		criteria = append(criteria, qaCriterion{Criterion: "c", State: tasks.QACriterionFailed})
	}
	opr.qa = completedQA(qaRoundOutput{Criteria: criteria, Observations: observations})
	section = opr.qaSection()
	if n := strings.Count(section, "\n- "); n != qaSectionMaxItems {
		t.Errorf("section lists %d items, want %d", n, qaSectionMaxItems)
	}
	if !strings.Contains(section, "_40 further QA item(s) are not shown here._") {
		t.Errorf("the overflow count is wrong:\n%s", section)
	}
}

// --- QA never holds the pull request -------------------------------------------

type capturePullRequestRPC struct {
	stubPullRequestRPC
	opened []oauth.OpenPullRequestArgsV1
}

func (c *capturePullRequestRPC) OpenPullRequest(org string, args oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error) {
	c.opened = append(c.opened, args)
	return c.stubPullRequestRPC.OpenPullRequest(org, args)
}

func TestQAFailuresNeverHoldThePullRequest(t *testing.T) {
	rpc := &capturePullRequestRPC{}
	qa := completedQA(qaRoundOutput{
		Criteria: qaCriteria(tasks.QACriterionFailed, tasks.QACriterionFailed),
		Findings: []reviewFindingOutput{{What: "broken", MustFix: true}},
	})
	qa.FixError = "the fix run timed out"
	opr := &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", BranchName: "tasks/x",
			Entries: []tasks.RepositoryEntry{{Name: "acme/api", BaseBranch: "main", InstallationID: "inst-1"}},
		},
		logsWriter:   io.Discard,
		agentPRTitle: "Add OAuth login",
		review:       completedReview(false),
		qa:           qa,
		pullRequests: rpc,
	}

	if opr.needsFixes() {
		t.Fatalf("a QA failure made the pull request need fixes")
	}
	if _, err := opr.openAll(map[int]bool{0: true}); err != nil {
		t.Fatalf("openAll: %s", err)
	}
	if len(rpc.opened) != 1 {
		t.Fatalf("opened %d pull requests, want 1", len(rpc.opened))
	}
	got := rpc.opened[0]
	if got.Draft || got.Title != "Add OAuth login" || strings.Contains(got.Title, needsFixesTitlePrefix) {
		t.Errorf("QA changed the pull request: draft=%v title=%q", got.Draft, got.Title)
	}
	for _, want := range []string{"0 of 2 acceptance criteria passed · 2 failed", "**QA on a preview**", "the fix run timed out"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("the body is missing %q:\n%s", want, got.Body)
		}
	}
}

// --- the body budget -----------------------------------------------------------

// A Task title of any length lands in the trailer. When the parts around the
// summary cannot fit, the advisory QA section goes before the Review section
// is reached by the last cut.
func TestQASectionIsDroppedBeforeTheReviewSection(t *testing.T) {
	opr := reviewTestOpener(completedReview(true, reviewFindingOutput{
		Parameter: "security", Severity: "high", Location: "handler.go:41", What: "no session check", MustFix: true,
	}))
	opr.qa = completedQA(qaRoundOutput{Criteria: qaCriteria(tasks.QACriterionFailed), Observations: []string{"slow page"}})

	// Fits: both sections are there.
	_, body := opr.buildPRTitleAndBody()
	requireInOrder(t, body, "**QA on a preview:**", "no session check", "**QA on a preview**", "slow page")

	// The trailer leaves room for the Review section but not for both.
	// Over the cap by half the QA section, so dropping it is enough.
	over := utf8.RuneCountInString(opr.qaSection()) / 2
	opr.ctx.TaskTitle = strings.Repeat("t", prBodyMaxRunes-utf8.RuneCountInString(body)+over)
	_, body = opr.buildPRTitleAndBody()

	if n := utf8.RuneCountInString(body); n > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", n, prBodyMaxRunes)
	}
	if strings.Contains(body, "**QA on a preview**") {
		t.Errorf("the QA section survived a body over budget")
	}
	for _, want := range []string{"**QA on a preview:** 0 of 1", "**Review**", "no session check", "Generated-By: deployment.io Tasks"} {
		if !strings.Contains(body, want) {
			t.Errorf("the body dropped %q", want)
		}
	}
}

// A QA run that stopped part-way still shows what it reported, under a lead-in
// that says it did not finish.
func TestQASectionShowsAPartialRun(t *testing.T) {
	const p, f, n = tasks.QACriterionPassed, tasks.QACriterionFailed, tasks.QACriterionNotChecked
	qa := &qaOutput{
		Status:       tasks.QAStatusCouldNotRun,
		StatusReason: "the QA run ended as timeout before it finished",
		Rounds: []qaRoundOutput{{
			Round: 1, Status: tasks.QAStatusCouldNotRun,
			Criteria:     qaCriteria(p, f, n),
			Findings:     []reviewFindingOutput{{Location: "/pricing", What: "no Sign up button", Why: "visitors cannot start"}},
			Observations: []string{"a console error"},
		}},
	}
	opr := reviewTestOpener(nil)
	opr.qa = qa
	got := opr.qaSection()
	requireInOrder(t, got,
		"**QA on a preview**",
		"QA did not finish",
		"does not hold this pull request",
		"- criterion 2\n  reason 2\n",
		"`/pricing` no Sign up button",
		"<details><summary>Not checked (1)</summary>",
		"<details><summary>Observations (1)</summary>",
	)
	if strings.Contains(got, "QA checked the change on a preview") {
		t.Errorf("a partial run is described as a completed check:\n%s", got)
	}
}
