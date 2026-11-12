package commands

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// taskSpecJSON is kit's TaskSpec as it reaches the ReviewSpec parameter:
// marshalled with no json tags, so the keys are the Go field names.
func taskSpecJSON(t *testing.T, acceptance []string) string {
	t.Helper()
	spec := struct {
		Goal        string
		Acceptance  []string
		Constraints []string
	}{Goal: "add login", Acceptance: acceptance, Constraints: []string{"keep the API stable"}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	return string(data)
}

func layoutFinding(what string, mustFix, sentBack bool) reviewFindingOutput {
	return reviewFindingOutput{Parameter: "correctness", Severity: "medium", Location: "x.go:1", What: what, MustFix: mustFix, SentBack: sentBack}
}

// requireInOrder fails unless every needle is in body, each after the last.
func requireInOrder(t *testing.T, body string, needles ...string) {
	t.Helper()
	at := 0
	for _, n := range needles {
		idx := strings.Index(body[at:], n)
		if idx < 0 {
			t.Fatalf("%q is missing or out of order:\n%s", n, body)
		}
		at += idx + len(n)
	}
}

// --- the body's layout -------------------------------------------------------

// fullOpener is an opener with every part of the body to show.
func fullOpener() *taskOpenPR {
	review := completedReview(true,
		layoutFinding("must one", true, true),
		layoutFinding("noted one", false, false),
	)
	review.Rounds[0].Model = "gpt-5.5"
	review.FixedInLoop = []reviewFindingOutput{layoutFinding("fixed one", true, true)}
	review.DeployRequirements = []resolvedDeployRequirement{{Variable: "STRIPE_KEY", Service: "api", Environment: "prod", EnvironmentID: "e", Managed: true, Found: true}}
	opr := reviewTestOpener(review)
	opr.ctx.DashboardURL = "https://app.example.com/"
	opr.verifyResult = &verifyResult{Ran: true, Passed: true, Command: "go test ./..."}
	opr.qa = completedQA(qaRoundOutput{
		Criteria:     qaCriteria(tasks.QACriterionFailed, tasks.QACriterionNotChecked),
		Findings:     []reviewFindingOutput{{Location: "/login", What: "the button does nothing"}},
		Observations: []string{"the page is slow"},
		MergeResults: []qaMergeResultOutput{{Repository: "acme/web", BaseBranch: "main", BaseCommit: "0123456789", Merged: true}},
	})
	opr.taskPreviews = stubPreviewLister{previews: []task_previews.TaskPreviewV1{
		{ServiceName: "acme-web-dist", ServiceType: task_previews.ServiceTypeStaticSite, URL: "https://d1abc.cloudfront.net"},
	}}
	return opr
}

func TestPRBodyPartsAreInTheFixedOrder(t *testing.T) {
	_, body := fullOpener().buildPRTitleAndBody()

	const opening = "**Review:** 3 found · 1 fixed before this PR · 1 must be fixed before merge · 1 noted\n\n" +
		"**QA on a preview:** 0 of 2 acceptance criteria passed · 1 failed · 1 not checked\n\n" +
		"**Details:** review, QA results, previews and logs on the [Task page](https://app.example.com/tasks/task-1)\n\n" +
		"**Before deploying**\n\n"
	if !strings.HasPrefix(body, opening) {
		t.Errorf("the body does not open with the tally, the QA line, the details line and Before deploying:\n%s", body)
	}
	requireInOrder(t, body,
		"**Before deploying**",
		"`STRIPE_KEY`",
		"\n\nAdded the login endpoint.\n\n**Needs you**\n\n_Still open and must be fixed_\n\n",
		"must one",
		"They need a human.",
		"\n\n**How it was checked**\n\n",
		"- `go test ./...` passed",
		"\n\n**Previews**\n\n",
		"- acme-web-dist: https://d1abc.cloudfront.net",
		"Previews are temporary",
	)
	const wantEnd = "\n\n---\nGenerated-By: deployment.io Tasks\nTask: My Task\nTask-URL: https://app.example.com/tasks/task-1\n"
	if !strings.HasSuffix(body, wantEnd) {
		t.Errorf("the trailer is not the last thing in the body:\n%s", body)
	}
}

// Each part is left out when it has nothing to show, and leaves no gap.
func TestPRBodyLeavesOutEachPartWithNothingToShow(t *testing.T) {
	for name, tc := range map[string]struct {
		strip  func(*taskOpenPR)
		absent string
	}{
		"no review":        {func(o *taskOpenPR) { o.review = nil }, "**Review:**"},
		"no QA":            {func(o *taskOpenPR) { o.qa = nil }, "**QA on a preview:**"},
		"no dashboard URL": {func(o *taskOpenPR) { o.ctx.DashboardURL = "" }, "**Details:**"},
		"no requirements":  {func(o *taskOpenPR) { o.review.DeployRequirements = nil }, "**Before deploying**"},
		"no summary":       {func(o *taskOpenPR) { o.agentSummary = "" }, "Added the login endpoint."},
		"nothing open":     {func(o *taskOpenPR) { o.review = completedReview(false) }, "**Needs you**"},
		"no verify result": {func(o *taskOpenPR) { o.verifyResult = nil }, "**How it was checked**"},
		"no previews":      {func(o *taskOpenPR) { o.taskPreviews = stubPreviewLister{} }, "**Previews**"},
	} {
		t.Run(name, func(t *testing.T) {
			opr := fullOpener()
			tc.strip(opr)
			_, body := opr.buildPRTitleAndBody()
			if strings.Contains(body, tc.absent) {
				t.Errorf("the body carries %q:\n%s", tc.absent, body)
			}
			if strings.Contains(body, "\n\n\n") {
				t.Errorf("a left-out part left a gap:\n%s", body)
			}
			if !strings.Contains(body, "Generated-By: deployment.io Tasks") {
				t.Errorf("the trailer is missing:\n%s", body)
			}
		})
	}
}

// What the Task page shows is not repeated in the body: the Task's acceptance
// criteria, the rest of the review, the QA results and the blocked hosts.
func TestPRBodyLeavesTheTaskPagesPartsOut(t *testing.T) {
	failedReview := fullOpener()
	failedReview.review.Rounds = append(failedReview.review.Rounds, reviewRoundOutput{Round: 2, Error: "timeout",
		Coverage: notCheckedCoverage("timeout")})
	noReview := fullOpener()
	noReview.review = nil
	for name, opr := range map[string]*taskOpenPR{
		"every part":    fullOpener(),
		"review failed": failedReview,
		"no review":     noReview,
		"ordinary":      reviewTestOpener(completedReview(false, layoutFinding("noted one", false, false))),
	} {
		t.Run(name, func(t *testing.T) {
			_, body := opr.buildPRTitleAndBody()
			for _, absent := range []string{
				"What the Task asks for", "What was asked",
				"Reviewed by", "Passes run", "_Noted_", "noted one", "Fixed during review", "fixed one", "Coverage",
				"**QA on a preview**\n", "the button does nothing", "the page is slow", "QA checked acme/web",
				"Hosts the agent was blocked", "<details>",
			} {
				if strings.Contains(body, absent) {
					t.Errorf("the body carries %q:\n%s", absent, body)
				}
			}
		})
	}
}

// --- the details line --------------------------------------------------------

func TestDetailsLinePointsToTheTaskPage(t *testing.T) {
	opr := reviewTestOpener(nil)
	opr.ctx.DashboardURL = "https://app.example.com/"
	const url = "https://app.example.com/tasks/task-1"
	if got, want := opr.detailsLine(), "**Details:** review, QA results, previews and logs on the [Task page]("+url+")"; got != want {
		t.Errorf("detailsLine = %q, want %q", got, want)
	}
	_, body := opr.buildPRTitleAndBody()
	if !strings.HasPrefix(body, opr.detailsLine()+"\n\n") || !strings.Contains(body, "\nTask-URL: "+url+"\n") {
		t.Errorf("the details line and Task-URL do not share the Task page's URL:\n%s", body)
	}

	opr.ctx.DashboardURL = ""
	if got := opr.detailsLine(); got != "" {
		t.Errorf("detailsLine without a dashboard URL = %q, want none", got)
	}
	_, body = opr.buildPRTitleAndBody()
	if strings.Contains(body, "**Details:**") || strings.Contains(body, "Task-URL:") {
		t.Errorf("a Task without a dashboard URL got a link to its page:\n%s", body)
	}
}

// The cap holds with every part as large as it gets, and the summary is what
// yields.
func TestPRBodyStaysUnderTheCapWithEveryPartAtItsLimit(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 0; i < 40; i++ {
		findings = append(findings, reviewFindingOutput{
			Parameter: "correctness", Severity: "low", SentBack: true,
			Location: strings.Repeat("l", 400), What: strings.Repeat("w", 900), Why: strings.Repeat("y", 900),
		})
	}
	opr := reviewTestOpener(completedReview(false, findings...))
	opr.ctx.DashboardURL = "https://app.example.com"
	opr.agentSummary = strings.Repeat("This line is part of a very long summary.\n", 2440) // ~100,000 runes
	if n := utf8.RuneCountInString(opr.agentSummary); n < 100000 {
		opr.agentSummary += strings.Repeat("x", 100000-n)
	}
	var previews []task_previews.TaskPreviewV1
	for i := 0; i < 10; i++ {
		previews = append(previews, task_previews.TaskPreviewV1{ServiceName: strings.Repeat("n", 300), URL: "https://d.example.com"})
	}
	opr.taskPreviews = stubPreviewLister{previews: previews}

	_, body := opr.buildPRTitleAndBody()

	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	for _, want := range []string{"**Review:**", "**Details:**", prSummaryTruncatedNote, "**Needs you**",
		"further finding(s) are not shown here", "**Previews**", "- and 5 more", "Generated-By: deployment.io Tasks"} {
		if !strings.Contains(body, want) {
			t.Errorf("the capped body dropped %q", want)
		}
	}
}

// --- the review tally --------------------------------------------------------

func TestReviewTally(t *testing.T) {
	failedAfterFix := completedReview(true, layoutFinding("no session check", true, true))
	failedAfterFix.Rounds = append(failedAfterFix.Rounds, reviewRoundOutput{Round: 2, Error: "timeout"})

	nothingExamined := completedReview(false)
	nothingExamined.Rounds[0].Coverage = notCheckedCoverage("the sandbox did not start")

	notRechecked := completedReview(false, layoutFinding("off by one", false, false))
	notRechecked.FinalTreeReviewed = false

	fixedAndNoted := completedReview(false, layoutFinding("off by one", false, false))
	fixedAndNoted.FixedInLoop = []reviewFindingOutput{
		layoutFinding("a", true, true), layoutFinding("b", true, true), layoutFinding("c", false, true),
	}

	cases := []struct {
		name   string
		review *reviewOutput
		want   string
	}{
		{"nil review", nil, ""},
		{"no rounds", &reviewOutput{Participation: "on"}, ""},
		{"failed last round", failedAfterFix, "**Review:** did not complete — details on the Task page."},
		{"nothing examined", nothingExamined, "**Review:** nothing was examined — details on the Task page."},
		{"last fix not re-checked", notRechecked, "**Review:** the last fix was not re-checked — details on the Task page."},
		{"no findings", completedReview(false), "**Review:** no findings."},
		{"fixed and noted", fixedAndNoted, "**Review:** 4 found · 3 fixed before this PR · 1 noted"},
		{"one must-fix", completedReview(true, layoutFinding("no session check", true, false)), "**Review:** 1 found · 1 must be fixed before merge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewTally(tc.review); got != tc.want {
				t.Errorf("tally = %q, want %q", got, tc.want)
			}
			_, body := reviewTestOpener(tc.review).buildPRTitleAndBody()
			if tc.want == "" {
				if strings.Contains(body, "**Review:**") {
					t.Errorf("a body with no tally carries one:\n%s", body)
				}
			} else if !strings.HasPrefix(body, tc.want+"\n\n") {
				t.Errorf("the body does not open with the tally:\n%s", body)
			}
		})
	}
}

// --- the Task's acceptance criteria -----------------------------------------

func TestAcceptanceCriteriaAreReadOneEach(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewSpec,
		taskSpecJSON(t, []string{"login works", "logout\nworks", "  sessions expire  "}))
	got := readAcceptanceCriteria(parameters)
	if want := []string{"login works", "logout works", "sessions expire"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("criteria = %q, want %q", got, want)
	}
}

// A re-run with feedback prefixes the ReviewSpec with its instructions; the
// criteria are still read from the spec after "[Spec]".
func TestAcceptanceCriteriaAreReadPastRerunInstructions(t *testing.T) {
	spec := "[Re-run instructions — these override the spec below where they conflict; later ones override earlier ones]\n" +
		"1. use tabs\n2. quote this:\n[Spec]\nnot the spec\n" +
		"\n[Spec]\n" + taskSpecJSON(t, []string{"login works"})
	if got := parseAcceptanceCriteria(spec); len(got) != 1 || got[0] != "login works" {
		t.Errorf("criteria = %q, want [login works]", got)
	}
	prose := "[Re-run instructions — these override the spec below where they conflict; later ones override earlier ones]\n" +
		"1. use tabs\n\n[Spec]\nAdd a login endpoint."
	if got := parseAcceptanceCriteria(prose); got != nil {
		t.Errorf("criteria = %q, want none", got)
	}
}

func TestAcceptanceCriteriaAreNoneWithoutAStructuredSpec(t *testing.T) {
	cases := map[string]string{
		"prose description":     "Add a login endpoint that checks the caller's session.",
		"object with no key":    `{"Goal":"add login"}`,
		"blank criteria":        `{"Acceptance":["", "  ", "\n"]}`,
		"not an object":         `["login works"]`,
		"malformed object":      `{"Acceptance":["login works"`,
		"acceptance not a list": `{"Acceptance":"login works"}`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseAcceptanceCriteria(spec); got != nil {
				t.Errorf("criteria = %q, want none", got)
			}
		})
	}
	t.Run("missing parameter", func(t *testing.T) {
		if got := readAcceptanceCriteria(map[string]interface{}{}); got != nil {
			t.Errorf("criteria = %q, want none", got)
		}
	})
}

func TestAcceptanceCriteriaAreCapped(t *testing.T) {
	long := parseAcceptanceCriteria(taskSpecJSON(t, []string{strings.Repeat("é", 500)}))
	if len(long) != 1 || utf8.RuneCountInString(long[0]) != prBodyAcceptanceMaxRunes || !strings.HasSuffix(long[0], "…") {
		t.Errorf("a 500-rune criterion was not capped at %d runes: %d", prBodyAcceptanceMaxRunes, utf8.RuneCountInString(long[0]))
	}
}

// --- how it was checked ------------------------------------------------------

func TestHowItWasChecked(t *testing.T) {
	const head = "**How it was checked**\n\n"
	cases := []struct {
		name string
		vr   *verifyResult
		want string
	}{
		{"no verify result", nil, ""},
		{"not run", &verifyResult{Ran: false}, head + "deployment.io did not run a build or test check.\n"},
		{"not run, with a reason", &verifyResult{Ran: false, SkippedReason: "docs-only change"},
			head + "deployment.io did not run a build or test check: docs-only change.\n"},
		{"no steps, passed", &verifyResult{Ran: true, Passed: true, Command: "go test ./..."}, head + "- `go test ./...` passed\n"},
		{"no steps, failed", &verifyResult{Ran: true, Passed: false}, head + "- `(unspecified command)` failed\n"},
		{"a passed step", &verifyResult{Ran: true, Passed: true, Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./...", Passed: true},
		}}, head + "- `go test ./...` passed in `0-acme/api`\n"},
		{"a pre-existing failure", &verifyResult{Ran: true, Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./...", StderrTail: "user_test.go:31: want 200, got 500", BaselineRan: true},
		}}, head + "- `go test ./...` failed in `0-acme/api` — it fails on the base commit too, so this change did not cause it\n" +
			"\n<details><summary>Output</summary>\n\n```\nuser_test.go:31: want 200, got 500\n```\n\n</details>\n\n"},
		{"a new failure", &verifyResult{Ran: true, Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./...", StderrTail: "boom", BaselineRan: true, BaselinePassed: true},
		}}, head + "- `go test ./...` failed in `0-acme/api`\n" +
			"\n<details><summary>Output</summary>\n\n```\nboom\n```\n\n</details>\n\n"},
		{"a failure with no output", &verifyResult{Ran: true, Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./..."},
		}}, head + "- `go test ./...` failed in `0-acme/api`\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opr := reviewTestOpener(nil)
			opr.verifyResult = tc.vr
			if got := opr.howCheckedSection(prBodyMaxRunes); got != tc.want {
				t.Errorf("section =\n%q\nwant\n%q", got, tc.want)
			}
			_, body := opr.buildPRTitleAndBody()
			if strings.Contains(body, "**How it was checked**") != (tc.want != "") {
				t.Errorf("the body's section does not match:\n%s", body)
			}
		})
	}
}

// Passing steps' bullets survive a budget the failing steps' output exhausts.
func TestHowItWasCheckedKeepsPassingStepsWhenOutputIsDropped(t *testing.T) {
	var steps []verifyStep
	for i := 0; i < 5; i++ {
		steps = append(steps, verifyStep{Repo: fmt.Sprintf("%d-acme/fail", i), Command: "go test ./...", StderrTail: strings.Repeat("x", 1500)})
	}
	steps = append(steps, verifyStep{Repo: "9-acme/ok", Command: "npm test", Passed: true})
	opr := reviewTestOpener(nil)
	opr.verifyResult = &verifyResult{Ran: true, Steps: steps}

	section := opr.howCheckedSection(1000)
	if !strings.Contains(section, "`0-acme/fail`") || !strings.Contains(section, "- `npm test` passed in `9-acme/ok`\n") {
		t.Errorf("the first failing step or a passing step was dropped:\n%s", section)
	}
	if !strings.HasSuffix(section, "- and 4 more failing step(s) — the full output is in the Step's job log\n") {
		t.Errorf("the section does not count the failing steps it dropped:\n%s", section)
	}
}

// --- "Needs you" ----------------------------------------------------------------

func TestNeedsYouOrder(t *testing.T) {
	findings := []reviewFindingOutput{
		layoutFinding("must one", true, true),
		layoutFinding("below one", false, true),
	}
	for i := 0; i < 16; i++ {
		findings = append(findings, layoutFinding(fmt.Sprintf("noted %d", i), false, false))
	}
	review := completedReview(true, findings...)
	review.Rounds = append(review.Rounds, reviewRoundOutput{Round: 2, Error: "the review run timed out",
		Coverage: notCheckedCoverage("the review run timed out")})
	review.FixError = "max turns reached"
	review.FixedInLoop = []reviewFindingOutput{layoutFinding("fixed 0", true, true)}

	section := reviewTestOpener(review).needsYouSection()

	requireInOrder(t, section,
		"**Needs you**\n\nThe review did not complete: the review run timed out. The change was committed and this pull request opened anyway; nothing here has been gated on a review verdict.\n",
		"\n_Reported by the last completed review, fix not verified_\n\n",
		"must one",
		"\nA fix attempt did not complete (max turns reached); the change is shown as it was before that attempt.\n",
		"\n_Reported by the last completed review, fix not verified (does not hold this pull request)_\n\n",
		"below one",
		"\nThese findings were open at the last completed review. They need a human.\n",
	)
	for _, absent := range []string{"noted", "fixed 0", "further finding(s)", "Coverage"} {
		if strings.Contains(section, absent) {
			t.Errorf("the part carries %q:\n%s", absent, section)
		}
	}
}

// A still-open finding below the hold threshold is listed on its own, with the
// fix outcome line under it and no "They need a human." — nothing holds the
// pull request.
func TestNeedsYouWithOnlyABelowHoldFinding(t *testing.T) {
	review := completedReview(false, layoutFinding("below one", false, true))
	review.StoppedNoChange = true
	section := reviewTestOpener(review).needsYouSection()
	want := "**Needs you**\n" +
		"\n_" + stillOpenBelowHoldHeading + "_\n\n" + renderFinding(layoutFinding("below one", false, true)) +
		"\n_The last fix round changed nothing._\n"
	if section != want {
		t.Errorf("section =\n%q\nwant\n%q", section, want)
	}
}

func TestNeedsYouSaysWhenFindingsAreUnverified(t *testing.T) {
	base := func() *reviewOutput {
		return completedReview(true, layoutFinding("must one", true, true), layoutFinding("below one", false, true))
	}
	notRechecked := base()
	notRechecked.FinalTreeReviewed = false
	failedAfterFix := base()
	failedAfterFix.FixedInLoop = []reviewFindingOutput{layoutFinding("fixed one", true, true)}
	failedAfterFix.Rounds = append(failedAfterFix.Rounds, reviewRoundOutput{Round: 2, Error: "timeout"})

	unverified := []string{
		"_Reported by the last completed review, fix not verified_",
		"_Reported by the last completed review, fix not verified (does not hold this pull request)_",
		"These findings were open at the last completed review. They need a human.",
	}
	verified := []string{
		"_Still open and must be fixed_",
		"_" + stillOpenBelowHoldHeading + "_",
		"These findings are still open. They need a human.",
	}
	for name, tc := range map[string]struct {
		review      *reviewOutput
		want, avoid []string
	}{
		"final tree not reviewed":     {notRechecked, unverified, verified},
		"failed last round after fix": {failedAfterFix, unverified, verified},
		"final tree reviewed":         {base(), verified, unverified},
	} {
		t.Run(name, func(t *testing.T) {
			section := reviewTestOpener(tc.review).needsYouSection()
			for _, w := range tc.want {
				if !strings.Contains(section, w) {
					t.Errorf("missing %q:\n%s", w, section)
				}
			}
			for _, a := range tc.avoid {
				if strings.Contains(section, a) {
					t.Errorf("unexpected %q:\n%s", a, section)
				}
			}
		})
	}
}

// Whatever the per-finding caps allow, "Needs you" stays within its rune cap
// and says it was cut; the trailer stays last.
func TestNeedsYouIsCutWithANote(t *testing.T) {
	review := completedReview(false, layoutFinding("below one", false, true))
	review.StoppedNoChange = true
	// A declining fix run's explanation is capped on its own, so a long failure
	// reason is what pushes the part past its cap.
	review.Rounds = append(review.Rounds, reviewRoundOutput{Round: 2, Error: strings.Repeat("e", reviewSectionMaxRunes)})
	opr := reviewTestOpener(review)

	section := opr.needsYouSection()
	const note = "\n\n_This list was truncated — the full review is on the Task page._\n"
	if !strings.HasSuffix(section, note) {
		t.Fatalf("the part was not cut with its note:\n%s", section[len(section)-200:])
	}
	if n := utf8.RuneCountInString(section); n > reviewSectionMaxRunes+utf8.RuneCountInString(note) {
		t.Errorf("the part is %d runes, over its cap", n)
	}
	_, body := opr.buildPRTitleAndBody()
	if !strings.HasSuffix(strings.TrimSpace(body), "Task: My Task") {
		t.Errorf("the trailer is no longer last:\n%s", body)
	}
}

// The whole-body cut closes what it cuts into, and stays within budget.
func TestCutToRuneBudgetNeverLeavesACollapsedBlockOpen(t *testing.T) {
	text := "intro\n\n<details><summary>Output</summary>\n\n" + strings.Repeat("line\n", 100) + "\n</details>\n\ntail\n"
	cut := cutToRuneBudget(text, 120, "\n\n_cut_")
	if strings.Count(cut, "<details>") != strings.Count(cut, "</details>") {
		t.Errorf("cutToRuneBudget left a block open:\n%s", cut)
	}
	if n := utf8.RuneCountInString(cut); n > 120 {
		t.Errorf("cutToRuneBudget returned %d runes, over its budget of 120", n)
	}
}
