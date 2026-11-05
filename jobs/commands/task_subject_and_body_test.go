package commands

import (
	"strings"
	"testing"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// TestTaskSubjectAndBody pins the one subject/body choice shared by the
// pull request and the commit.
func TestTaskSubjectAndBody(t *testing.T) {
	long := strings.Repeat("a", 80)
	cappedLong := strings.Repeat("a", 71) + "…"
	multiByte := strings.Repeat("é", 72)
	cases := []struct {
		name        string
		prTitle     string
		summary     string
		wantSubject string
		wantBody    string
	}{
		{"pr_title with summary", "Add OAuth login", "  Did things\n\nMore detail.  ", "Add OAuth login", "Did things\n\nMore detail."},
		{"pr_title strips needs-fixes prefix", "[Needs fixes] Add OAuth login", "Body.", "Add OAuth login", "Body."},
		{"pr_title capped", long, "Body.", cappedLong, "Body."},
		{"multi-byte pr_title counted in runes", multiByte, "Body.", multiByte, "Body."},
		{"multi-byte pr_title over cap", multiByte + "é", "Body.", strings.Repeat("é", 71) + "…", "Body."},
		{"whitespace-only pr_title falls back to summary", "   ", "Short subject\n\nRest.", "Short subject", "Rest."},
		{"summary first line and rest", "", "Short subject\n\nRest of\nbody.", "Short subject", "Rest of\nbody."},
		{"summary first line strips needs-fixes prefix", "", "[Needs fixes] Short subject\nRest.", "Short subject", "Rest."},
		{"long summary first line capped, whole summary body", "", long + "\nRest.", cappedLong, long + "\nRest."},
		{"summary starting with blank line", "", "\n\n  First real line\nRest.", "First real line", "Rest."},
		{"neither", "", "", "Tasks Step 3: My Task", ""},
		{"whitespace-only summary, no pr_title", "", " \n\t\n", "Tasks Step 3: My Task", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, body := taskSubjectAndBody(tc.prTitle, tc.summary, 2, "My Task")
			if subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", subject, tc.wantSubject)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestTaskCommitPush_BuildCommitMessage_PRTitle pins that the commit subject
// is the pr_title and the body the whole summary, followed by the trailer.
func TestTaskCommitPush_BuildCommitMessage_PRTitle(t *testing.T) {
	ctx := mkTaskCtx("Task A", 0)
	ctx.DashboardURL = "https://app.example.com"
	tcp := &taskCommitPush{
		ctx:          ctx,
		agentPRTitle: "[Needs fixes] Add POST /items",
		agentSummary: "preview-test-api now accepts POST /items.\n\nAlso GET /items/:id.",
	}
	want := "Add POST /items\n\n" +
		"preview-test-api now accepts POST /items.\n\nAlso GET /items/:id.\n\n" +
		commitTrailerTag + "\n" +
		"Task: Task A\n" +
		"Task-URL: https://app.example.com/tasks/task-1\n"
	if got := tcp.buildCommitMessage(); got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
}

// TestTaskCommitPush_BuildCommitMessage_NoPRTitle pins the summary-only
// subject: a short first line with the rest as body, a long one capped with
// the whole summary as body.
func TestTaskCommitPush_BuildCommitMessage_NoPRTitle(t *testing.T) {
	ctx := mkTaskCtx("Task A", 0)
	trailer := commitTrailerTag + "\nTask: Task A\n"

	tcp := &taskCommitPush{ctx: ctx, agentSummary: "Short subject\n\nBody line."}
	if got, want := tcp.buildCommitMessage(), "Short subject\n\nBody line.\n\n"+trailer; got != want {
		t.Errorf("short: message = %q, want %q", got, want)
	}

	long := strings.Repeat("b", 100)
	tcp = &taskCommitPush{ctx: ctx, agentSummary: long + "\nBody line."}
	want := strings.Repeat("b", 71) + "…\n\n" + long + "\nBody line.\n\n" + trailer
	if got := tcp.buildCommitMessage(); got != want {
		t.Errorf("long: message = %q, want %q", got, want)
	}
}

// TestCommitSubjectEqualsPRTitle pins that, for the same agent output, the
// commit subject is the pull request's title.
func TestCommitSubjectEqualsPRTitle(t *testing.T) {
	ctx := commandUtils.TaskJobContext{TaskID: "task-1", TaskTitle: "My Task", StepIndex: 1}
	for _, in := range []struct{ prTitle, summary string }{
		{"Add OAuth login", "I made the change.\n\nDetails."},
		{"", "Short first line\nRest."},
		{"", strings.Repeat("c", 90)},
		{"", ""},
	} {
		prSubject, prLeadIn := (&taskOpenPR{ctx: ctx, agentPRTitle: in.prTitle, agentSummary: in.summary}).subjectAndLeadIn()
		commitSubject, commitBody := (&taskCommitPush{ctx: ctx, agentPRTitle: in.prTitle, agentSummary: in.summary}).subjectAndBody()
		if commitSubject != prSubject || commitBody != prLeadIn {
			t.Errorf("pr=(%q,%q) commit=(%q,%q)", prSubject, prLeadIn, commitSubject, commitBody)
		}
	}
}
