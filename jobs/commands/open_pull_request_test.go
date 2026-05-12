package commands

import (
	"strings"
	"testing"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// TestTaskOpenPR_SubjectAndLeadIn pins the PR title/body source: when
// the agent produced a changes_summary, the first line is the PR title
// and the rest is the lead-in. Without a summary, falls back to
// "Tasks Step N: <title>" with empty lead-in. Symmetric with
// taskCommitPush.subjectAndBody so commit + PR shapes match.
func TestTaskOpenPR_SubjectAndLeadIn(t *testing.T) {
	ctx := commandUtils.TaskJobContext{
		OrganizationID: "org-1",
		TaskID:         "task-1",
		TaskTitle:      "My Task",
		StepIndex:      0, // → "Tasks Step 1" fallback
	}

	cases := []struct {
		name        string
		summary     string
		wantSubject string
		wantLeadIn  string
	}{
		{
			name:        "multi-line summary splits",
			summary:     "Add OAuth login\n\nDetailed description across lines.",
			wantSubject: "Add OAuth login",
			wantLeadIn:  "Detailed description across lines.",
		},
		{
			name:        "single-line summary yields empty lead-in",
			summary:     "Add OAuth login",
			wantSubject: "Add OAuth login",
			wantLeadIn:  "",
		},
		{
			name:        "absent summary falls back",
			summary:     "",
			wantSubject: "Tasks Step 1: My Task",
			wantLeadIn:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opr := &taskOpenPR{ctx: ctx, agentSummary: tc.summary}
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
