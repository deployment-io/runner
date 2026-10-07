package commands

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// --- FinalTreeReviewed: did the latest completed round see the committed tree?

func mustFixFinding(key string) reviewFinding {
	return reviewFinding{Key: key, Parameter: "security", Severity: "critical", Location: "0-acme/api/debug.go:12", What: "the debug endpoint has no auth"}
}

func TestFinalTreeReviewedAfterACompletedFinalRound(t *testing.T) {
	stage := heldLoopStage(t)
	review := runScripted(t, stage, []agentResult{
		reviewRoundResult(nil, mustFixFinding("A")),
		// The fix is kept, and the round after it completes.
		reviewRoundResult([]reviewPreviousFinding{{Key: "A", Status: "resolved"}}),
	})
	if !review.FinalTreeReviewed {
		t.Error("final_tree_reviewed = false after the last round reviewed the kept fix")
	}
}

func TestFinalTreeReviewedStaysTrueAfterARolledBackFix(t *testing.T) {
	stage := heldLoopStage(t)
	stage.runFix = func([]reviewFindingOutput) error {
		writeFile(t, filepath.Join(stage.workDirHost, "0-acme/api/debug.go"), "half a fix\n")
		return errors.New("claude exited: max turns reached")
	}
	review := runScripted(t, stage, []agentResult{reviewRoundResult(nil, mustFixFinding("A"))})
	if review.FixError == "" {
		t.Fatal("the fix was not recorded as rolled back, so this case is not exercising what it names")
	}
	if !review.FinalTreeReviewed {
		t.Error("final_tree_reviewed = false after a rolled-back fix: the tree is the one round 1 reviewed")
	}
}

func TestFinalTreeReviewedStaysTrueAfterANoChangeStop(t *testing.T) {
	stage := heldLoopStage(t)
	stage.runFix = func([]reviewFindingOutput) error { return nil }
	review := runScripted(t, stage, []agentResult{reviewRoundResult(nil, mustFixFinding("A"))})
	if !review.StoppedNoChange {
		t.Fatal("the loop did not stop on a no-change fix, so this case is not exercising what it names")
	}
	if !review.FinalTreeReviewed {
		t.Error("final_tree_reviewed = false after a fix that changed nothing")
	}
}

func TestFinalTreeReviewedIsFalseAfterAKeptFixAndAFailedRound(t *testing.T) {
	stage := heldLoopStage(t)
	review := runScripted(t, stage, []agentResult{
		reviewRoundResult(nil, mustFixFinding("A")),
		{Status: "failure", Error: "the reviewer crashed"},
	})
	if review.Rounds[len(review.Rounds)-1].Completed {
		t.Fatal("the last round completed, so this case is not exercising what it names")
	}
	if review.FinalTreeReviewed {
		t.Error("final_tree_reviewed = true, but no completed round saw the kept fix")
	}
}

func TestFinalTreeReviewedIsFalseAfterAKeptFixAndTheBudgetStop(t *testing.T) {
	stage := heldLoopStage(t)
	fix := stage.runFix
	stage.runFix = func(f []reviewFindingOutput) error {
		err := fix(f)
		// The fix used up the stage budget: the top-of-loop check stops it.
		stage.deadline = time.Now()
		return err
	}
	review := runScripted(t, stage, []agentResult{reviewRoundResult(nil, mustFixFinding("A"))})
	if len(review.Rounds) != 1 {
		t.Fatalf("the loop ran %d round(s), want the budget to stop it after the fix", len(review.Rounds))
	}
	if review.FinalTreeReviewed {
		t.Error("final_tree_reviewed = true, but nothing reviewed the kept fix")
	}
}

func TestFinalTreeReviewedIsFalseWhenNoRoundRan(t *testing.T) {
	stage := heldLoopStage(t)
	stage.baseCommits = nil
	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}
	if review := readReviewFromJobOutput(out); review == nil || review.FinalTreeReviewed {
		t.Errorf("review = %+v, want final_tree_reviewed false", review)
	}
}

// --- which findings get a comment, and how it reads ---------------------------

type stubPullRequestRPC struct {
	reviews   []oauth.PostPullRequestReviewArgsV1
	reviewDto oauth.PostPullRequestReviewDtoV1
	reviewErr error
}

func (s *stubPullRequestRPC) OpenPullRequest(string, oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error) {
	return oauth.OpenPullRequestDtoV1{URL: "https://github.com/acme/api/pull/7", Number: 7}, nil
}

func (s *stubPullRequestRPC) PostPullRequestReview(_ string, args oauth.PostPullRequestReviewArgsV1) (oauth.PostPullRequestReviewDtoV1, error) {
	s.reviews = append(s.reviews, args)
	return s.reviewDto, s.reviewErr
}

func commentTestPR(review *reviewOutput, logs io.Writer, repos ...string) (*taskOpenPR, *stubPullRequestRPC) {
	if len(repos) == 0 {
		repos = []string{"acme/api"}
	}
	var entries []tasks.RepositoryEntry
	for _, r := range repos {
		entries = append(entries, tasks.RepositoryEntry{Name: r, BaseBranch: "main", InstallationID: "inst-1"})
	}
	if logs == nil {
		logs = io.Discard
	}
	rpc := &stubPullRequestRPC{}
	return &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", BranchName: "tasks/x", Entries: entries,
		},
		logsWriter:   logs,
		review:       review,
		pullRequests: rpc,
	}, rpc
}

func reviewWith(findings []reviewFindingOutput, fixed ...reviewFindingOutput) *reviewOutput {
	return &reviewOutput{
		Participation:     "on",
		FinalTreeReviewed: true,
		Rounds:            []reviewRoundOutput{{Round: 2, Completed: true, Findings: findings}},
		FixedInLoop:       fixed,
	}
}

func commentsFor(t *testing.T, pr *taskOpenPR, idx int) reviewCommentPlan {
	t.Helper()
	plan, ok := pr.reviewComments(idx, pr.ctx.Entries[idx])
	if !ok {
		t.Fatal("reviewComments refused to build comments")
	}
	return plan
}

func TestReviewCommentsCarryOpenFindingsWithTheirClass(t *testing.T) {
	review := reviewWith([]reviewFindingOutput{
		{Key: "held", Parameter: "security", Severity: "critical", Location: "0-acme/api/handler.go:41", What: "no session check",
			Why: "anyone can call it", MustFix: true, SentBack: true, Held: true, StillPresentNote: "still no check"},
		{Key: "fresh", Parameter: "correctness", Severity: "high", Location: "0-acme/api/store.go:10-12", What: "retry loops forever", MustFix: true, SentBack: true, New: true},
		{Key: "sent", Parameter: "security", Severity: "low", Location: "0-acme/api/log.go:5", What: "echoes the path", SentBack: true, Held: true},
		{Key: "note", Parameter: "maintainability", Severity: "info", Location: "0-acme/api/util.go:3", What: "rename this"},
	}, reviewFindingOutput{Key: "fixed", Parameter: "security", Severity: "critical", Location: "0-acme/api/auth.go:9", What: "was fixed"})
	pr, _ := commentTestPR(review, nil)
	plan := commentsFor(t, pr, 0)

	if len(plan.comments) != 4 {
		t.Fatalf("got %d comments, want 4: %+v", len(plan.comments), plan.comments)
	}
	byPath := map[string]oauth.PullRequestReviewCommentV1{}
	for _, c := range plan.comments {
		byPath[c.Path] = c
	}
	if _, ok := byPath["auth.go"]; ok {
		t.Error("a finding fixed during review got a comment")
	}
	held := byPath["handler.go"]
	if held.Line != 41 || held.StartLine != 0 {
		t.Errorf("held comment at %d-%d, want line 41", held.StartLine, held.Line)
	}
	for _, want := range []string{"**security · critical** — must fix before merge, still present after a fix round", "no session check", "_Why it matters:_ anyone can call it", "_Reviewer's note:_ still no check"} {
		if !strings.Contains(held.Body, want) {
			t.Errorf("held comment lacks %q:\n%s", want, held.Body)
		}
	}
	fresh := byPath["store.go"]
	if fresh.StartLine != 10 || fresh.Line != 12 {
		t.Errorf("range comment at %d-%d, want 10-12", fresh.StartLine, fresh.Line)
	}
	if !strings.Contains(fresh.Body, "— must fix before merge\n") || strings.Contains(fresh.Body, "fix round") {
		t.Errorf("a must-fix finding first reported in the final round says it had a fix round:\n%s", fresh.Body)
	}
	if !strings.Contains(byPath["log.go"].Body, "— still open, does not hold this pull request, still present after a fix round") {
		t.Errorf("sent-back comment = %q", byPath["log.go"].Body)
	}
	if !strings.Contains(byPath["util.go"].Body, "— noted\n") {
		t.Errorf("noted comment = %q", byPath["util.go"].Body)
	}
	// Highest severity first.
	if plan.comments[0].Path != "handler.go" || plan.comments[3].Path != "util.go" {
		t.Errorf("order = %v, want critical first and info last", plan.comments)
	}
}

func TestReviewCommentsPlacement(t *testing.T) {
	findings := []reviewFindingOutput{
		{Key: "a", Severity: "high", Location: "0-acme/api/handler.go:41:9"},
		{Key: "b", Severity: "high", Location: "1-acme/web/app.ts:3"},
		{Key: "c", Severity: "high", Location: "0-acme/api/handler.go"},
		{Key: "d", Severity: "high", Location: "somewhere in the handler"},
		{Key: "e", Severity: "high", Location: ""},
		{Key: "f", Severity: "high", Location: "internal/config/config.go:352"},
	}
	t.Run("multi-repository Task", func(t *testing.T) {
		pr, _ := commentTestPR(reviewWith(findings), nil, "acme/api", "acme/web")
		plan := commentsFor(t, pr, 0)
		if len(plan.comments) != 1 || plan.comments[0].Path != "handler.go" || plan.comments[0].Line != 41 {
			t.Errorf("comments = %+v, want only handler.go:41 (column ignored, prefix stripped)", plan.comments)
		}
		// No line (c, d, e) and unprefixed on a multi-repository Task (f).
		if plan.unplaced != 4 {
			t.Errorf("unplaced = %d, want 4", plan.unplaced)
		}
		web := commentsFor(t, pr, 1)
		if len(web.comments) != 1 || web.comments[0].Path != "app.ts" {
			t.Errorf("web comments = %+v, want app.ts only", web.comments)
		}
	})
	t.Run("one-repository Task", func(t *testing.T) {
		pr, _ := commentTestPR(reviewWith(findings), nil)
		plan := commentsFor(t, pr, 0)
		paths := []string{}
		for _, c := range plan.comments {
			paths = append(paths, fmt.Sprintf("%s:%d", c.Path, c.Line))
		}
		if strings.Join(paths, ",") != "handler.go:41,internal/config/config.go:352" {
			t.Errorf("comments = %v, want handler.go:41 and the unprefixed config.go:352", paths)
		}
	})
}

func TestReviewCommentsNeutraliseModelText(t *testing.T) {
	pr, _ := commentTestPR(reviewWith([]reviewFindingOutput{{
		Key: "x", Parameter: "security", Severity: "high", Location: "0-acme/api/a.go:1",
		What: "ping @octocat about #12 and see [docs](https://evil.example) ![img](https://evil.example/x.png) or https://bare.example and WWW.bare.example",
	}}), nil)
	body := commentsFor(t, pr, 0).comments[0].Body
	for _, bad := range []string{"@octocat", "#12", "[docs](", "![img](", "://", "WWW.", "www."} {
		if strings.Contains(body, bad) {
			t.Errorf("comment still carries %q:\n%s", bad, body)
		}
	}
	for _, want := range []string{"@\u200boctocat", "#\u200b12", `\[docs\]`, "https:\u200b//bare.example", "WWW\u200b.bare.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
}

func TestReviewCommentsAreCappedHighestSeverityFirst(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 1; i <= 40; i++ {
		severity := "low"
		if i > 35 {
			severity = "critical"
		}
		findings = append(findings, reviewFindingOutput{Key: fmt.Sprint(i), Parameter: "security", Severity: severity, Location: fmt.Sprintf("0-acme/api/a.go:%d", i)})
	}
	pr, _ := commentTestPR(reviewWith(findings), nil)
	plan := commentsFor(t, pr, 0)
	if len(plan.comments) != reviewCommentsMax || plan.capped != 10 {
		t.Fatalf("got %d comments, %d capped; want %d and 10", len(plan.comments), plan.capped, reviewCommentsMax)
	}
	for i := 0; i < 5; i++ {
		if !strings.Contains(plan.comments[i].Body, "critical") {
			t.Errorf("comment %d is not one of the critical findings: %s", i, plan.comments[i].Body)
		}
	}
}

func TestNoReviewCommentsWhenTheyMayNotBePosted(t *testing.T) {
	finding := []reviewFindingOutput{{Key: "x", Severity: "high", Location: "0-acme/api/a.go:1", MustFix: true}}
	stale := reviewWith(finding)
	stale.FinalTreeReviewed = false
	off := reviewWith(finding)
	off.Participation = "off"
	for name, review := range map[string]*reviewOutput{"final tree not reviewed": stale, "participation off": off, "no review": nil} {
		t.Run(name, func(t *testing.T) {
			pr, rpc := commentTestPR(review, nil)
			if _, err := pr.openOne(0, pr.ctx.Entries[0]); err != nil {
				t.Fatalf("openOne: %s", err)
			}
			if len(rpc.reviews) != 0 {
				t.Errorf("a review was posted: %+v", rpc.reviews)
			}
		})
	}
}

func TestAdvisoryPostsItsFindings(t *testing.T) {
	review := reviewWith([]reviewFindingOutput{{Key: "x", Parameter: "security", Severity: "high", Location: "0-acme/api/a.go:1"}})
	review.Participation = "advisory"
	pr, rpc := commentTestPR(review, nil)
	rpc.reviewDto = oauth.PostPullRequestReviewDtoV1{Posted: 1}
	if _, err := pr.openOne(0, pr.ctx.Entries[0]); err != nil {
		t.Fatalf("openOne: %s", err)
	}
	if len(rpc.reviews) != 1 || !strings.Contains(rpc.reviews[0].Comments[0].Body, "— noted") {
		t.Errorf("reviews = %+v, want the advisory finding posted as noted", rpc.reviews)
	}
}

func TestOpenOnePostsOneReviewAndLogsTheOutcome(t *testing.T) {
	review := reviewWith([]reviewFindingOutput{
		{Key: "x", Parameter: "security", Severity: "high", Location: "0-acme/api/a.go:1", MustFix: true},
		{Key: "y", Parameter: "security", Severity: "high", Location: "0-acme/api/b.go:9"},
		{Key: "z", Parameter: "security", Severity: "high", Location: "the whole module"},
	})
	var logs strings.Builder
	pr, rpc := commentTestPR(review, &logs)
	rpc.reviewDto = oauth.PostPullRequestReviewDtoV1{Posted: 1, Dropped: 1}
	out, err := pr.openOne(0, pr.ctx.Entries[0])
	if err != nil || out.PRNumber != 7 {
		t.Fatalf("openOne = %+v, %v", out, err)
	}
	if len(rpc.reviews) != 1 {
		t.Fatalf("posted %d reviews, want 1", len(rpc.reviews))
	}
	got := rpc.reviews[0]
	if got.PRNumber != 7 || got.RepoName != "acme/api" || got.InstallationID != "inst-1" || len(got.Comments) != 2 {
		t.Errorf("review args = %+v", got)
	}
	if !strings.HasPrefix(got.Body, "deployment.io review: "+oauth.PullRequestReviewPostedPlaceholder+" finding(s)") {
		t.Errorf("review body = %q", got.Body)
	}
	if !strings.Contains(logs.String(), "Posted 1 inline review comment(s); 1 were not on the diff; 1 had no line to attach to") {
		t.Errorf("log lacks the outcome line:\n%s", logs.String())
	}
}

func TestAReviewThatCannotBePostedNeverFailsTheStep(t *testing.T) {
	review := reviewWith([]reviewFindingOutput{{Key: "x", Severity: "high", Location: "0-acme/api/a.go:1", MustFix: true}})
	for name, set := range map[string]func(*stubPullRequestRPC){
		"rpc error": func(s *stubPullRequestRPC) {
			s.reviewErr = errors.New("rpc: can't find method Oauth.PostPullRequestReviewV1")
		},
		"unsupported provider": func(s *stubPullRequestRPC) { s.reviewDto = oauth.PostPullRequestReviewDtoV1{Unsupported: true} },
	} {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			pr, rpc := commentTestPR(review, &logs)
			set(rpc)
			out, err := pr.openOne(0, pr.ctx.Entries[0])
			if err != nil || out.PRURL == "" {
				t.Fatalf("openOne = %+v, %v; want the pull request regardless", out, err)
			}
			if !strings.Contains(logs.String(), "Inline review comments not posted: ") {
				t.Errorf("log lacks the not-posted line:\n%s", logs.String())
			}
		})
	}
}

func TestParseFindingLocation(t *testing.T) {
	for _, tc := range []struct {
		in         string
		path       string
		start, end int
		ok         bool
	}{
		{"a/b.go:41", "a/b.go", 41, 41, true},
		{"a/b.go:41-45", "a/b.go", 41, 45, true},
		{"a/b.go:41:9", "a/b.go", 41, 41, true},
		{"`a/b.go:7`", "a/b.go", 7, 7, true},
		{"a/b.go", "", 0, 0, false},
		{"a/b.go:0", "", 0, 0, false},
		{"a/b.go:x", "", 0, 0, false},
		{"a/b.go:41-41", "a/b.go", 41, 41, true},
		{"a/b.go:41-0", "", 0, 0, false},
		{"a/b.go:41-20", "", 0, 0, false},
		{"a/b.go:41-99999999999999999999", "", 0, 0, false},
		{"a/b.go:99999999999999999999", "", 0, 0, false},
	} {
		path, start, end, ok := parseFindingLocation(tc.in)
		if path != tc.path || start != tc.start || end != tc.end || ok != tc.ok {
			t.Errorf("parseFindingLocation(%q) = %q %d %d %t", tc.in, path, start, end, ok)
		}
	}
}
