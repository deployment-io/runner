package commands

import (
	"fmt"
	"io"
	"strings"
	"testing"

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
	if !strings.Contains(got.Body, "0 of 2 acceptance criteria passed · 2 failed") {
		t.Errorf("the body is missing the QA status line:\n%s", got.Body)
	}
	// The QA results themselves are on the Task page.
	for _, absent := range []string{"**QA on a preview**\n", "broken", "the fix run timed out"} {
		if strings.Contains(got.Body, absent) {
			t.Errorf("the body carries %q:\n%s", absent, got.Body)
		}
	}
}
