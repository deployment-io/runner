package commands

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
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

func TestPRBodyPartsAreInTheFixedOrder(t *testing.T) {
	review := completedReview(false, layoutFinding("off by one", false, false))
	review.Rounds[0].Model = "gpt-5.5"
	opr := reviewTestOpener(review)
	opr.ctx.DashboardURL = "https://app.example.com"
	opr.acceptance = []string{"login works", "logout works"}
	opr.verifyResult = &verifyResult{Ran: true, Passed: true, Command: "go test ./..."}
	opr.deniedHosts = []string{"pypi.example.com"}

	_, body := opr.buildPRTitleAndBody()

	if !strings.HasPrefix(body, "**Review:** 1 found · 1 noted\n\nAdded the login endpoint.\n\n") {
		t.Errorf("the body does not open with the tally, a blank line and the summary:\n%s", body)
	}
	requireInOrder(t, body,
		"**Review:** 1 found",
		"Added the login endpoint.",
		"**What the Task asks for**",
		"- login works\n- logout works\n",
		"**How it was checked**",
		"- `go test ./...` passed",
		"---\n**Review**",
		"<details><summary>Hosts the agent was blocked from reaching (1)</summary>",
	)
	const wantEnd = "\n\n---\nGenerated-By: deployment.io Tasks\nTask: My Task\nTask-URL: https://app.example.com/tasks/task-1\n"
	if !strings.HasSuffix(body, wantEnd) {
		t.Errorf("the trailer is not the last thing in the body:\n%s", body)
	}
}

// Review participation Off: no tally and no Review section, the rest intact.
func TestPRBodyWithoutAReviewKeepsTheOtherParts(t *testing.T) {
	opr := reviewTestOpener(nil)
	opr.acceptance = []string{"login works"}
	opr.verifyResult = &verifyResult{Ran: false}
	opr.deniedHosts = []string{"pypi.example.com"}

	_, body := opr.buildPRTitleAndBody()

	for _, absent := range []string{"**Review:**", "**Review**"} {
		if strings.Contains(body, absent) {
			t.Errorf("a Task with no Review stage got %q:\n%s", absent, body)
		}
	}
	if !strings.HasPrefix(body, "Added the login endpoint.\n\n") {
		t.Errorf("without a tally the summary should open the body:\n%s", body)
	}
	requireInOrder(t, body, "Added the login endpoint.", "**What the Task asks for**", "**How it was checked**",
		"<details><summary>Hosts the agent", "Generated-By: deployment.io Tasks", "Task: My Task")
}

// The cap holds with every part as large as it gets, and the summary is what
// yields.
func TestPRBodyStaysUnderTheCapWithEveryPartAtItsLimit(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 0; i < 40; i++ {
		findings = append(findings, reviewFindingOutput{
			Parameter: "correctness", Severity: "low",
			Location: strings.Repeat("l", 400), What: strings.Repeat("w", 900), Why: strings.Repeat("y", 900),
		})
	}
	opr := reviewTestOpener(completedReview(false, findings...))
	opr.agentSummary = strings.Repeat("This line is part of a very long summary.\n", 2440) // ~100,000 runes
	if n := utf8.RuneCountInString(opr.agentSummary); n < 100000 {
		opr.agentSummary += strings.Repeat("x", 100000-n)
	}
	var criteria []string
	for i := 0; i < 25; i++ {
		criteria = append(criteria, strings.Repeat("c", 500))
	}
	opr.acceptance = parseAcceptanceCriteria(taskSpecJSON(t, criteria))

	_, body := opr.buildPRTitleAndBody()

	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	for _, want := range []string{"**Review:**", prSummaryTruncatedNote, "**What the Task asks for**",
		"- and 5 more on the Task page", "**Review**", "Generated-By: deployment.io Tasks"} {
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
		{"failed last round", failedAfterFix, "**Review:** did not complete — details below."},
		{"nothing examined", nothingExamined, "**Review:** nothing was examined — details below."},
		{"last fix not re-checked", notRechecked, "**Review:** the last fix was not re-checked — details below."},
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

// --- what the Task asks for --------------------------------------------------

func TestAcceptanceCriteriaRenderOneBulletEach(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewSpec,
		taskSpecJSON(t, []string{"login works", "logout\nworks", "  sessions expire  "}))
	got := acceptanceSection(readAcceptanceCriteria(parameters))
	want := "**What the Task asks for**\n\n- login works\n- logout works\n- sessions expire\n"
	if got != want {
		t.Errorf("section =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "keep the API stable") {
		t.Error("a part of the spec other than the criteria reached the body")
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

func TestAcceptanceSectionIsLeftOutWithoutCriteria(t *testing.T) {
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
		if got := acceptanceSection(nil); got != "" {
			t.Errorf("section = %q, want none", got)
		}
	})
}

func TestAcceptanceCriteriaAreCapped(t *testing.T) {
	var criteria []string
	for i := 0; i < 25; i++ {
		criteria = append(criteria, fmt.Sprintf("criterion %d", i))
	}
	section := acceptanceSection(parseAcceptanceCriteria(taskSpecJSON(t, criteria)))
	if got := strings.Count(section, "\n- criterion "); got != 20 {
		t.Errorf("%d criteria rendered, want 20:\n%s", got, section)
	}
	if !strings.HasSuffix(section, "- criterion 19\n- and 5 more on the Task page\n") {
		t.Errorf("the section does not end with the overflow line:\n%s", section)
	}

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

// --- the Review section --------------------------------------------------------

func TestReviewSectionOrderAndCollapsedBlocks(t *testing.T) {
	findings := []reviewFindingOutput{
		layoutFinding("must one", true, true),
		layoutFinding("below one", false, true),
	}
	for i := 0; i < 16; i++ {
		findings = append(findings, layoutFinding(fmt.Sprintf("noted %d", i), false, false))
	}
	review := completedReview(true, findings...)
	review.Rounds[0].Model = "gpt-5.5"
	review.Rounds = append(review.Rounds, reviewRoundOutput{Round: 2, Error: "the review run timed out",
		Coverage: notCheckedCoverage("the review run timed out")})
	review.FixError = "max turns reached"
	for i := 0; i < 5; i++ {
		review.FixedInLoop = append(review.FixedInLoop, layoutFinding(fmt.Sprintf("fixed %d", i), true, true))
	}

	section := reviewTestOpener(review).reviewSection()

	requireInOrder(t, section,
		"---\n**Review**\n\n",
		"Reviewed by gpt-5.5.",
		"The review did not complete: the review run timed out.",
		"Passes run: security, correctness.",
		"_Reported by the last completed review, fix not verified_",
		"must one",
		"A fix attempt did not complete (max turns reached)",
		"_Reported by the last completed review, fix not verified (does not hold this pull request)_",
		"below one",
		"_Noted_",
		"noted 15",
		// 18 visible + 2 fixed rendered = the 20-item cap; 3 fixed left out.
		"_3 further finding(s) are not shown here",
		"These findings were open at the last completed review. They need a human.",
		"<details><summary>Fixed during review (5)</summary>\n\n- **correctness / medium**",
		"fixed 1",
		"\n\n</details>\n",
		"<details><summary>Coverage</summary>\n\nCoverage — security: checked;",
		"\n\n</details>\n",
	)
	if strings.Contains(section, "fixed 2") {
		t.Errorf("a fixed finding past the item cap was rendered:\n%s", section)
	}
}

func TestReviewSectionSaysWhenFindingsAreUnverified(t *testing.T) {
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
			section := reviewTestOpener(tc.review).reviewSection()
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

// With no completed round the coverage line is collapsed too.
func TestReviewSectionCollapsesCoverageWithNoCompletedRound(t *testing.T) {
	review := &reviewOutput{Participation: "on", Rounds: []reviewRoundOutput{{
		Round: 1, Error: "no review_result", Coverage: notCheckedCoverage("no review_result"),
	}}}
	section := reviewTestOpener(review).reviewSection()
	requireInOrder(t, section, "The review did not complete", "<details><summary>Coverage</summary>\n\nCoverage — ", "\n\n</details>\n")
}

// --- blocked hosts -------------------------------------------------------------

func TestBlockedHostsAreCollapsedWithTheirCount(t *testing.T) {
	hosts := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		hosts = append(hosts, fmt.Sprintf("host-%02d.example.com", i))
	}
	section := (&taskOpenPR{deniedHosts: hosts}).blockedHostsSection()
	if !strings.HasPrefix(section, "<details><summary>Hosts the agent was blocked from reaching (80)</summary>\n\nThe agent attempted") {
		t.Errorf("the hosts are not in a <details> block carrying the count:\n%s", section)
	}
	if !strings.HasSuffix(section, "- and 30 more\n\n</details>\n") {
		t.Errorf("the block does not close after the list:\n%s", section)
	}
	if got := (&taskOpenPR{}).blockedHostsSection(); got != "" {
		t.Errorf("a Step with no denied hosts got %q", got)
	}
}

// A cut never leaves a collapsed block open. The Review section is cut near
// its end, which is where its collapsed blocks are, and on GitHub an unclosed
// <details> swallows everything after it — the blocked hosts and the trailer.
func TestACutNeverLeavesACollapsedBlockOpen(t *testing.T) {
	long := strings.Repeat("x", 290)
	var findings, fixed []reviewFindingOutput
	for i := 0; i < 8; i++ {
		findings = append(findings, reviewFindingOutput{Key: fmt.Sprint("o", i), Parameter: "correctness", Severity: "low",
			Location: strings.Repeat("p", 190), What: long, Why: long})
	}
	for i := 0; i < 6; i++ {
		fixed = append(fixed, reviewFindingOutput{Key: fmt.Sprint("f", i), Parameter: "security", Severity: "medium",
			Location: strings.Repeat("q", 190), What: long, Why: long})
	}
	var coverage []reviewCoverageOutput
	for _, p := range []string{"security", "correctness", "spec conformance", "testing", "deploy readiness", "performance", "maintainability", "reliability"} {
		coverage = append(coverage, reviewCoverageOutput{Parameter: p, State: "not checked", Reason: "no pass for this parameter in this release"})
	}
	coverage[0].State, coverage[1].State = "checked", "checked"
	opr := &taskOpenPR{
		ctx:         commandUtils.TaskJobContext{TaskTitle: "T"},
		deniedHosts: []string{"a.example"},
		review: &reviewOutput{Participation: "on", FinalTreeReviewed: true, FixedInLoop: fixed,
			Rounds: []reviewRoundOutput{{Round: 1, Completed: true, Findings: findings, Coverage: coverage}}},
	}
	section := opr.reviewSection()
	if !strings.Contains(section, "The Review section was truncated") {
		t.Fatalf("the fixture no longer forces a cut; make the findings longer:\n%s", section)
	}
	_, body := opr.buildPRTitleAndBody()
	if open, closed := strings.Count(body, "<details>"), strings.Count(body, "</details>"); open != closed {
		t.Errorf("%d <details> but %d </details>; the rest of the body would be swallowed:\n%s", open, closed, body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "Task: T") {
		t.Errorf("the trailer is no longer last:\n%s", body)
	}

	// The whole-body cut closes what it cuts into, and stays within budget.
	text := "intro\n\n<details><summary>Output</summary>\n\n" + strings.Repeat("line\n", 100) + "\n</details>\n\ntail\n"
	cut := cutToRuneBudget(text, 120, "\n\n_cut_")
	if strings.Count(cut, "<details>") != strings.Count(cut, "</details>") {
		t.Errorf("cutToRuneBudget left a block open:\n%s", cut)
	}
	if n := utf8.RuneCountInString(cut); n > 120 {
		t.Errorf("cutToRuneBudget returned %d runes, over its budget of 120", n)
	}
}
