package commands

import (
	"strings"
	"testing"

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
