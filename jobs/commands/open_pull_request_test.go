package commands

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// TestTaskOpenPR_SubjectAndLeadIn pins the Bug 2 fix's subject/body
// policy. Five branches:
//
//  1. Newer agentbox (pr_title set, short): pr_title is the subject,
//     full changes_summary is the lead-in. Prefer the agent-produced
//     title even when changes_summary has a usable first line.
//  2. Newer agentbox (pr_title set, long): defensive 72-rune cap with
//     ellipsis. Guards against an agent that ignores the system-prompt
//     instruction.
//  3. Older agentbox (no pr_title, short first line of summary):
//     first line is the subject, rest is the lead-in. Matches pre-fix
//     behavior so a mixed-version production fleet stays sensible.
//  4. Older agentbox (no pr_title, long first line — the original Bug
//     2 case): cap the first line, preserve the FULL narrative as the
//     lead-in. Previously the entire 119-char line landed as the PR
//     title; now reviewers see a tidy title + the full context in the
//     body.
//  5. No agent output at all: generic "Tasks Step N: <title>" subject,
//     empty lead-in.
func TestTaskOpenPR_SubjectAndLeadIn(t *testing.T) {
	ctx := commandUtils.TaskJobContext{
		OrganizationID: "org-1",
		TaskID:         "task-1",
		TaskTitle:      "My Task",
		StepIndex:      0, // → "Tasks Step 1" fallback
	}

	// 119-char single-line narrative — the exact shape that produced
	// the production bug. Asserting it gets capped here pins the fix.
	longFirstLine := "Updated `scripts.build` in `/work/0-deployment-io/dashboard/package.json:8` — single-line change, no other fields touched."

	cases := []struct {
		name        string
		prTitle     string
		summary     string
		wantSubject string
		wantLeadIn  string
	}{
		{
			name:        "newer agentbox: short pr_title is preferred over summary first line",
			prTitle:     "Add OAuth login to auth-service",
			summary:     "Add OAuth login\n\nDetailed description across lines.",
			wantSubject: "Add OAuth login to auth-service",
			wantLeadIn:  "Add OAuth login\n\nDetailed description across lines.",
		},
		{
			name:        "newer agentbox: long pr_title is defensively capped",
			prTitle:     "This agent ignored the 72-char instruction and produced a wildly overlong title that goes on and on and on",
			summary:     "Body text.",
			wantSubject: "This agent ignored the 72-char instruction and produced a wildly overlo…",
			wantLeadIn:  "Body text.",
		},
		{
			name:        "older agentbox: short multi-line summary splits at newline",
			prTitle:     "",
			summary:     "Add OAuth login\n\nDetailed description across lines.",
			wantSubject: "Add OAuth login",
			wantLeadIn:  "Detailed description across lines.",
		},
		{
			name:        "older agentbox: single-line short summary yields empty lead-in",
			prTitle:     "",
			summary:     "Add OAuth login",
			wantSubject: "Add OAuth login",
			wantLeadIn:  "",
		},
		{
			name:        "older agentbox: long first line is capped AND preserved as lead-in (Bug 2 production case)",
			prTitle:     "",
			summary:     longFirstLine,
			wantSubject: "Updated `scripts.build` in `/work/0-deployment-io/dashboard/package.jso…",
			wantLeadIn:  longFirstLine,
		},
		{
			name:        "no agent output at all: generic fallback",
			prTitle:     "",
			summary:     "",
			wantSubject: "Tasks Step 1: My Task",
			wantLeadIn:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opr := &taskOpenPR{ctx: ctx, agentPRTitle: tc.prTitle, agentSummary: tc.summary}
			subject, leadIn := opr.subjectAndLeadIn()
			if subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", subject, tc.wantSubject)
			}
			if leadIn != tc.wantLeadIn {
				t.Errorf("leadIn = %q, want %q", leadIn, tc.wantLeadIn)
			}
		})
	}
}

// TestCapTitle pins the rune-aware truncation behavior. Multi-byte
// titles can't be byte-counted without breaking glyphs; capTitle uses
// utf8.RuneCountInString and slices runes, not bytes.
func TestCapTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under limit returns as-is", "hello", 10, "hello"},
		{"equal to limit returns as-is", "hello", 5, "hello"},
		{"over limit truncates with ellipsis", "hello world", 7, "hello …"},
		{"trims surrounding whitespace before measuring", "  hello  ", 10, "hello"},
		{"multi-byte runes are not byte-counted", "héllo wörld", 7, "héllo …"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capTitle(tc.in, tc.n); got != tc.want {
				t.Errorf("capTitle(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// TestSplitFirstLine pins the (firstLine, rest) split semantics. Both
// sides are trimmed; no-newline input returns ("input", "").
func TestSplitFirstLine(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantFirst string
		wantRest  string
	}{
		{"no newline", "single line only", "single line only", ""},
		{"newline with body", "first\nrest of body", "first", "rest of body"},
		{"blank line between subject and body", "first\n\nbody", "first", "body"},
		{"trims surrounding whitespace on both halves", "  first  \n  rest  ", "first", "rest"},
		{"empty input", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, rest := splitFirstLine(tc.in)
			if first != tc.wantFirst {
				t.Errorf("first = %q, want %q", first, tc.wantFirst)
			}
			if rest != tc.wantRest {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestTaskOpenPR_BuildPRTitleAndBody_TrailerAndLeadIn pins the
// PR-body composition order: lead-in first (when present), then the
// trailer block, then the optional denied-hosts section. The lead-in
// goes BEFORE the trailer so reviewers see what the agent did before
// the metadata.
func TestTaskOpenPR_BuildPRTitleAndBody_TrailerAndLeadIn(t *testing.T) {
	ctx := commandUtils.TaskJobContext{
		OrganizationID: "org-1",
		TaskID:         "task-1",
		TaskTitle:      "My Task",
		DashboardURL:   "https://app.example.com",
		StepIndex:      0,
	}
	opr := &taskOpenPR{
		ctx:          ctx,
		agentSummary: "Add OAuth\n\nDescription body line.",
	}
	subject, body := opr.buildPRTitleAndBody()

	if subject != "Add OAuth" {
		t.Errorf("subject = %q, want %q", subject, "Add OAuth")
	}
	if !strings.Contains(body, "Description body line.") {
		t.Errorf("body missing the lead-in description:\n%s", body)
	}
	if !strings.Contains(body, "Generated-By: deployment.io Tasks") {
		t.Errorf("body missing Generated-By trailer:\n%s", body)
	}
	if !strings.Contains(body, "Task: My Task") {
		t.Errorf("body missing Task: header:\n%s", body)
	}
	if !strings.Contains(body, "Task-URL: https://app.example.com/tasks/task-1") {
		t.Errorf("body missing Task-URL line:\n%s", body)
	}
	// Lead-in must appear before the trailer so the agent's description
	// reads first.
	leadInIdx := strings.Index(body, "Description body line.")
	trailerIdx := strings.Index(body, "Generated-By:")
	if leadInIdx == -1 || trailerIdx == -1 || leadInIdx >= trailerIdx {
		t.Errorf("lead-in must precede trailer in body:\n%s", body)
	}
}

// TestTaskOpenPR_BuildPRTitleAndBody_WithDeniedHosts pins the
// denied-hosts section appears AFTER the trailer (separated by a "---"
// horizontal rule) and lists each host as a code-formatted bullet.
// Surfaced to reviewers so they can suggest allowlist additions.
func TestTaskOpenPR_BuildPRTitleAndBody_WithDeniedHosts(t *testing.T) {
	opr := &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1",
			TaskID:         "task-1",
			TaskTitle:      "My Task",
			StepIndex:      0,
		},
		deniedHosts: []string{"pypi.example.com", "registry.internal"},
	}
	_, body := opr.buildPRTitleAndBody()

	if !strings.Contains(body, "**Network: blocked hosts during this Step**") {
		t.Errorf("body missing denied-hosts header:\n%s", body)
	}
	if !strings.Contains(body, "`pypi.example.com`") {
		t.Errorf("body missing pypi.example.com bullet:\n%s", body)
	}
	if !strings.Contains(body, "`registry.internal`") {
		t.Errorf("body missing registry.internal bullet:\n%s", body)
	}
	// Denied-hosts must appear after the trailer — they're optional
	// detail, not primary metadata.
	trailerIdx := strings.Index(body, "Generated-By:")
	deniedIdx := strings.Index(body, "**Network: blocked hosts")
	if trailerIdx == -1 || deniedIdx == -1 || deniedIdx <= trailerIdx {
		t.Errorf("denied-hosts must follow trailer:\n%s", body)
	}
}

// An ordinary body — a Step with no fix round and a summary that fits — is
// exactly the body it was before the cap existed, byte for byte. The cap is a
// guard on the one case that would fail the PR open, not a reformatting of
// every pull request.
func TestTaskOpenPR_OrdinaryBodyIsUnchanged(t *testing.T) {
	opr := &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task",
			DashboardURL: "https://app.example.com", StepIndex: 0,
		},
		agentPRTitle: "Add OAuth login to auth-service",
		agentSummary: "Added the login endpoint.\n\nIt checks the caller's session.",
	}
	subject, body := opr.buildPRTitleAndBody()

	const wantSubject = "Add OAuth login to auth-service"
	const wantBody = "Added the login endpoint.\n\nIt checks the caller's session.\n\n" +
		"Generated-By: deployment.io Tasks\n" +
		"Task: My Task\n" +
		"Task-URL: https://app.example.com/tasks/task-1\n"
	if subject != wantSubject {
		t.Errorf("subject = %q, want %q", subject, wantSubject)
	}
	if body != wantBody {
		t.Errorf("body =\n%q\nwant\n%q", body, wantBody)
	}
	if strings.Contains(body, "truncated") {
		t.Errorf("a summary that fits was marked as truncated:\n%s", body)
	}
}

// GitHub rejects a body over 65,536 characters, and a rejected body is a pull
// request that never opens. The summary is the part with no bound of its own,
// so it is the part that yields — and everything the reader cannot get
// elsewhere (the metadata, the Review section) survives the cut.
func TestTaskOpenPR_BodyIsCappedAtSixtyThousandRunes(t *testing.T) {
	opr := reviewTestOpener(completedReview(true, reviewFindingOutput{
		Parameter: "security", Severity: "high", Location: "handler.go:41",
		What: "no session check", Why: "any caller can read another org's data", MustFix: true,
	}))
	opr.ctx.DashboardURL = "https://app.example.com"
	// ~102,500 runes, every line identical so the cut point is checkable.
	const line = "This line is part of a very long summary.\n"
	opr.agentSummary = strings.Repeat(line, 2500)

	_, body := opr.buildPRTitleAndBody()

	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	for _, want := range []string{
		"Generated-By: deployment.io Tasks",
		"Task-URL: https://app.example.com/tasks/task-1",
		"**Review**",
		"no session check",
		prSummaryTruncatedNote,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the capped body dropped %q:\n%s", want, body[:2000])
		}
	}
	// Cut at a line break: what is kept ends on a whole line, not mid-sentence.
	kept := body[:strings.Index(body, prSummaryTruncatedNote)]
	if !strings.HasSuffix(kept, "This line is part of a very long summary.") {
		t.Errorf("the summary was not cut at a line break; it ends %q", kept[len(kept)-60:])
	}
}

// Runes, not bytes: a body cut in the middle of a multi-byte character is
// mojibake in the pull request and, for some providers, an invalid request.
func TestTaskOpenPR_BodyCapNeverSplitsAMultiByteCharacter(t *testing.T) {
	base := &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", StepIndex: 0,
		},
		agentPRTitle: "ログイン処理を追加",
	}
	// One long line and one many-line summary: the first is cut on a rune
	// boundary with no line break to fall back on, the second at a line break.
	for name, summary := range map[string]string{
		"one long line": strings.Repeat("日本語のテキストです。", 9000),
		"many lines":    strings.Repeat("日本語のテキストです。\n", 9000),
	} {
		t.Run(name, func(t *testing.T) {
			opr := *base
			opr.agentSummary = summary
			_, body := opr.buildPRTitleAndBody()
			if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
				t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
			}
			if !utf8.ValidString(body) || strings.ContainsRune(body, utf8.RuneError) {
				t.Error("the body was cut in the middle of a multi-byte character")
			}
			if !strings.Contains(body, prSummaryTruncatedNote) {
				t.Error("the body was cut without saying so")
			}
		})
	}
}

// A run denied hundreds of hosts is denied the same handful over and over.
// Fifty names and a count beat a list that crowds out the rest of the body.
func TestTaskOpenPR_BlockedHostsAreCappedWithACount(t *testing.T) {
	hosts := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		hosts = append(hosts, fmt.Sprintf("host-%02d.example.com", i))
	}
	opr := &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", StepIndex: 0,
		},
		deniedHosts: hosts,
	}
	_, body := opr.buildPRTitleAndBody()

	if got := strings.Count(body, "`host-"); got != 50 {
		t.Errorf("%d hosts rendered, want %d", got, prBodyDeniedHostsMaxItems)
	}
	if !strings.Contains(body, "- and 30 more\n") {
		t.Errorf("the body does not say how many hosts were left out:\n%s", body)
	}
	if strings.Contains(body, "`host-50.example.com`") {
		t.Errorf("a host past the cap was rendered:\n%s", body)
	}
}

// A handful of pre-existing failures across repos is an ordinary body, well
// under the cap. Every one of them is named: the failures are what this pull
// request exists to explain, and the body has room, so nothing is dropped and
// nothing says anything was.
func TestTaskOpenPR_FailingVerifyStepsAreAllKeptWhenTheBodyFits(t *testing.T) {
	steps := make([]verifyStep, 0, 8)
	for i := 0; i < 8; i++ {
		steps = append(steps, verifyStep{
			Repo: fmt.Sprintf("%d-acme/service", i), Command: "go test ./...",
			Passed: false, BaselineRan: true, BaselinePassed: false,
			// boundVerifyTail keeps the END of a tail, where a test runner puts
			// the failure — so that is where the per-step marker goes.
			StderrTail: strings.Repeat("x", 2000) + fmt.Sprintf("\nservice_%d_test.go:31: want 200, got 500", i),
		})
	}
	opr := reviewTestOpener(completedReview(false))
	opr.verifyResult = &verifyResult{Ran: true, Passed: false, Command: "go test ./...", Steps: steps}

	_, body := opr.buildPRTitleAndBody()

	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	for i := 0; i < 8; i++ {
		if want := fmt.Sprintf("service_%d_test.go:31", i); !strings.Contains(body, want) {
			t.Errorf("a failure was dropped from a body that had room for it: %q missing", want)
		}
	}
	if strings.Contains(body, "more failing step(s)") || strings.Contains(body, "truncated") {
		t.Errorf("a body that fits was reported as cut:\n%s", body)
	}
}

// Dropping the summary is not enough on its own. A Task across many repos can
// report a pre-existing failure per repo, each with its own bounded output, and
// bounded parts still add up: past the whole body's cap the section reports what
// it can and names the rest as a count, so the pull request is not rejected by
// the provider. The Review section, which comes after it, has to survive too.
func TestTaskOpenPR_ManyFailingVerifyStepsStillFitTheBody(t *testing.T) {
	steps := make([]verifyStep, 0, 60)
	for i := 0; i < 60; i++ {
		steps = append(steps, verifyStep{
			Repo: fmt.Sprintf("%d-acme/service", i), Command: "go test ./...",
			Passed: false, BaselineRan: true, BaselinePassed: false,
			StderrTail: strings.Repeat("x", 3000),
		})
	}
	opr := reviewTestOpener(completedReview(true, reviewFindingOutput{
		Parameter: "security", Severity: "high", Location: "handler.go:41",
		What: "no session check", Why: "any caller can read another org's data", MustFix: true,
	}))
	opr.verifyResult = &verifyResult{Ran: true, Passed: false, Command: "go test ./...", Steps: steps}

	_, body := opr.buildPRTitleAndBody()

	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	if !strings.Contains(body, "more failing step(s)") {
		t.Errorf("the body does not say how many failing steps were left out:\n%s", body)
	}
	for _, want := range []string{"Generated-By: deployment.io Tasks", "**Review**", "no session check"} {
		if !strings.Contains(body, want) {
			t.Errorf("the verification section crowded out %q:\n%s", want, body)
		}
	}
}

// The last guard, on its own: whatever the sections below the summary add up
// to, the body handed to the provider is within the cap and says it was cut.
func TestCutToRuneBudgetBoundsTheWholeBody(t *testing.T) {
	body := cutToRuneBudget(strings.Repeat("日本語のテキストです。\n", 9000), prBodyMaxRunes, prBodyTruncatedNote)
	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	if !strings.HasSuffix(body, prBodyTruncatedNote) {
		t.Errorf("the body was cut without saying so, it ends %q", body[len(body)-60:])
	}
	if strings.ContainsRune(body, utf8.RuneError) {
		t.Error("the body was cut in the middle of a multi-byte character")
	}
	// A budget with no room for the note keeps the cap rather than the note.
	if got := cutToRuneBudget("some text", 4, prBodyTruncatedNote); got != "" {
		t.Errorf("cutToRuneBudget with no room for the note = %q, want \"\"", got)
	}
}
