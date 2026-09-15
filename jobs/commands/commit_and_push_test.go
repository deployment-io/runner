package commands

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestMergeRepoOutputs_PreservesCommitSHAAcrossCommands pins the
// CommitAndPush → OpenPullRequest hand-off. CommitAndPush writes
// {Index, Name, HasChanges, CommitSHA, Branch}; OpenPullRequest
// writes {Index, Name, HasChanges, Branch, PRURL, PRNumber} without
// CommitSHA. The merge must preserve the prior CommitSHA so the
// deployment-server hook persists a complete repository record.
func TestMergeRepoOutputs_PreservesCommitSHAAcrossCommands(t *testing.T) {
	// CommitAndPush's contribution.
	existing := []repoOutput{
		{
			Index:      0,
			Name:       "owner/api",
			HasChanges: true,
			CommitSHA:  "abc123def4567890",
			Branch:     "tasks/migrate-jwt",
		},
	}
	// OpenPullRequest's contribution — no CommitSHA in incoming.
	incoming := []repoOutput{
		{
			Index:      0,
			Name:       "owner/api",
			HasChanges: true,
			Branch:     "tasks/migrate-jwt",
			PRURL:      "https://github.com/owner/api/pull/42",
			PRNumber:   42,
		},
	}
	got := mergeRepoOutputs(existing, incoming)
	if len(got) != 1 {
		t.Fatalf("expected 1 merged entry, got %d", len(got))
	}
	if got[0].CommitSHA != "abc123def4567890" {
		t.Errorf("CommitSHA = %q, want %q (preserved from prior CommitAndPush output)",
			got[0].CommitSHA, "abc123def4567890")
	}
	if got[0].PRURL != "https://github.com/owner/api/pull/42" {
		t.Errorf("PRURL = %q, want the OpenPullRequest value", got[0].PRURL)
	}
	if got[0].PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got[0].PRNumber)
	}
	if got[0].Branch != "tasks/migrate-jwt" {
		t.Errorf("Branch = %q, want %q", got[0].Branch, "tasks/migrate-jwt")
	}
}

// TestMergeRepoOutputs_PreservesPRFieldsOnRetry pins the reverse
// direction: a retry that re-runs CommitAndPush after a prior
// successful OpenPullRequest should NOT clobber the existing PR
// fields. (Step Job re-run regenerates the working dir from scratch,
// so CommitAndPush runs again with fresh repoOutputs containing
// CommitSHA but no PR fields.)
func TestMergeRepoOutputs_PreservesPRFieldsOnRetry(t *testing.T) {
	existing := []repoOutput{
		{
			Index:      0,
			Name:       "owner/api",
			HasChanges: true,
			CommitSHA:  "oldcommit",
			Branch:     "tasks/migrate-jwt",
			PRURL:      "https://github.com/owner/api/pull/42",
			PRNumber:   42,
		},
	}
	incoming := []repoOutput{
		{
			Index:      0,
			Name:       "owner/api",
			HasChanges: true,
			CommitSHA:  "newcommit",
			Branch:     "tasks/migrate-jwt",
		},
	}
	got := mergeRepoOutputs(existing, incoming)
	if got[0].CommitSHA != "newcommit" {
		t.Errorf("CommitSHA should be overwritten on retry: got %q", got[0].CommitSHA)
	}
	if got[0].PRURL != "https://github.com/owner/api/pull/42" {
		t.Errorf("PRURL must survive retry: got %q", got[0].PRURL)
	}
	if got[0].PRNumber != 42 {
		t.Errorf("PRNumber must survive retry: got %d", got[0].PRNumber)
	}
}

// TestMergeRepoOutputs_EmptyExistingReturnsIncoming pins the
// no-prior-output case: an empty existing slice means the incoming
// values are the merged result. Covers the first-command (CommitAndPush)
// path where there's nothing to merge against.
func TestMergeRepoOutputs_EmptyExistingReturnsIncoming(t *testing.T) {
	incoming := []repoOutput{
		{Index: 0, Name: "a", HasChanges: true, CommitSHA: "sha1"},
		{Index: 1, Name: "b", HasChanges: false},
	}
	got := mergeRepoOutputs(nil, incoming)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].CommitSHA != "sha1" {
		t.Errorf("CommitSHA at index 0 = %q, want sha1", got[0].CommitSHA)
	}
}

// TestMergeRepoOutputs_SortedByIndex pins the determinism guarantee.
// Output is sorted by Index regardless of input order — ensures the
// JSON envelope is stable across runs (eases diffing in Slack alerts
// and audit logs).
func TestMergeRepoOutputs_SortedByIndex(t *testing.T) {
	existing := []repoOutput{
		{Index: 2, Name: "c"},
		{Index: 0, Name: "a"},
	}
	incoming := []repoOutput{
		{Index: 1, Name: "b"},
	}
	got := mergeRepoOutputs(existing, incoming)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	for i := range got {
		if got[i].Index != i {
			t.Errorf("entry[%d].Index = %d, want %d (sorted)", i, got[i].Index, i)
		}
	}
}

// TestMergeRepoOutputs_PreservesUntouchedExisting pins that an
// existing entry with no incoming entry at the same Index survives
// the merge. Covers partial command failure scenarios where one repo
// was processed and another wasn't.
func TestMergeRepoOutputs_PreservesUntouchedExisting(t *testing.T) {
	existing := []repoOutput{
		{Index: 0, Name: "a", CommitSHA: "sha-a"},
		{Index: 1, Name: "b", CommitSHA: "sha-b"},
	}
	incoming := []repoOutput{
		{Index: 0, Name: "a", PRURL: "u1", PRNumber: 1},
	}
	got := mergeRepoOutputs(existing, incoming)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[1].CommitSHA != "sha-b" {
		t.Errorf("untouched entry at Index 1 should survive: got CommitSHA=%q", got[1].CommitSHA)
	}
}

// TestReadAgentSummaryFromJobOutput_PresentAndAbsent pins the
// envelope-walking rules. Mirrors kit's job_hooks.extractAgentChanges-
// Summary defensiveness — any malformed payload, missing block, or
// missing field returns "" so the caller's fallback path fires.
func TestReadAgentSummaryFromJobOutput_PresentAndAbsent(t *testing.T) {
	cases := []struct {
		name     string
		envelope jobOutputData
		want     string
	}{
		{
			name:     "agent block absent",
			envelope: jobOutputData{SchemaVersion: jobOutputSchemaVersion},
			want:     "",
		},
		{
			name: "agent block present but changes_summary empty",
			envelope: jobOutputData{
				SchemaVersion: jobOutputSchemaVersion,
				Agent:         &agentOutput{ExitCode: 0},
			},
			want: "",
		},
		{
			name: "agent block with changes_summary",
			envelope: jobOutputData{
				SchemaVersion: jobOutputSchemaVersion,
				Agent: &agentOutput{
					ChangesSummary: "Migrated JWT signing from RS256 to HS256",
				},
			},
			want: "Migrated JWT signing from RS256 to HS256",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.envelope)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			params := map[string]interface{}{}
			// Set via parameters_enums.JobOutput key — use the same
			// SetParameterValue helper the production code uses so
			// shape matches.
			jobs.SetParameterValue[string](params, parameters_enums.JobOutput, string(raw))
			got := readAgentSummaryFromJobOutput(params)
			if got != tc.want {
				t.Errorf("readAgentSummaryFromJobOutput() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadAgentSummaryFromJobOutput_NoEnvelope pins the
// missing-parameter case: no JobOutput key at all returns "" without
// erroring (the agent simply hasn't produced anything yet, or this
// Step Job's runtime predates the producer).
func TestReadAgentSummaryFromJobOutput_NoEnvelope(t *testing.T) {
	got := readAgentSummaryFromJobOutput(map[string]interface{}{})
	if got != "" {
		t.Errorf("empty params should yield empty summary, got %q", got)
	}
}

// TestReadAgentSummaryFromJobOutput_MalformedJSON pins the defensive
// path: malformed JobOutput JSON returns "" rather than panicking
// or propagating the unmarshal error. Best-effort by design.
func TestReadAgentSummaryFromJobOutput_MalformedJSON(t *testing.T) {
	params := map[string]interface{}{}
	jobs.SetParameterValue[string](params, parameters_enums.JobOutput, "{not-valid-json")
	got := readAgentSummaryFromJobOutput(params)
	if got != "" {
		t.Errorf("malformed JSON should yield empty summary, got %q", got)
	}
}

// TestTaskCommitPush_SubjectAndBody pins the commit-message wiring.
// Agent summary present → first line is subject, rest is body.
// Single-line summary → subject only, empty body. Absent summary →
// generic "Tasks Step N: <title>" fallback.
func TestTaskCommitPush_SubjectAndBody(t *testing.T) {
	baseCtx := mkTaskCtx("My Task", 2) // StepIndex 2 → "Tasks Step 3" in the fallback

	cases := []struct {
		name        string
		summary     string
		wantSubject string
		wantBody    string
	}{
		{
			name:        "multi-line summary splits",
			summary:     "Migrated JWT signing\n\nDetailed body across\nmultiple lines.",
			wantSubject: "Migrated JWT signing",
			wantBody:    "Detailed body across\nmultiple lines.",
		},
		{
			name:        "single-line summary yields empty body",
			summary:     "Migrated JWT signing",
			wantSubject: "Migrated JWT signing",
			wantBody:    "",
		},
		{
			name:        "absent summary falls back",
			summary:     "",
			wantSubject: "Tasks Step 3: My Task",
			wantBody:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tcp := &taskCommitPush{ctx: baseCtx, agentSummary: tc.summary}
			subject, body := tcp.subjectAndBody()
			if subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", subject, tc.wantSubject)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestTaskCommitPush_BuildCommitMessage_ContainsTrailer pins that
// every commit message — agent-summary or fallback — gets the
// trailer block (Generated-By + Task: + optional Task-URL). The
// trailer is what git providers display as commit "metadata" and
// what links the commit back to the Task in the dashboard.
func TestTaskCommitPush_BuildCommitMessage_ContainsTrailer(t *testing.T) {
	ctx := mkTaskCtx("Task A", 0)
	ctx.DashboardURL = "https://app.example.com/"

	tcp := &taskCommitPush{ctx: ctx, agentSummary: "First-line summary"}
	msg := tcp.buildCommitMessage()

	for _, want := range []string{
		"First-line summary",
		commitTrailerTag,
		"Task: Task A",
		"Task-URL: https://app.example.com/tasks/" + ctx.TaskID,
	} {
		if !containsLine(msg, want) {
			t.Errorf("commit message missing %q\n--- full message ---\n%s", want, msg)
		}
	}
}

// mkTaskCtx builds a minimal TaskJobContext for tests. Stable taskID +
// title so assertions can match strings literally.
func mkTaskCtx(title string, stepIdx int64) commandUtils.TaskJobContext {
	return commandUtils.TaskJobContext{
		OrganizationID: "org-1",
		TaskID:         "task-1",
		TaskTitle:      title,
		StepIndex:      stepIdx,
		BranchName:     "tasks/test-branch",
	}
}

// containsLine reports whether msg contains exactly the line `want`
// (delimited by newlines or the message boundaries). Distinguishes
// a literal Task: header line from an incidental "Task:" substring
// in some other context.
func containsLine(msg, want string) bool {
	for _, line := range splitLines(msg) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// initRepoWithCommit creates a repo with one commit on master and returns it
// with that commit's hash.
func initRepoWithCommit(t *testing.T) (*git.Repository, *git.Worktree, plumbing.Hash) {
	t.Helper()
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(name string) plumbing.Hash {
		if err := util.WriteFile(wt.Filesystem, name, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(name, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	return repo, wt, commit("base.txt")
}

func setRemoteRef(t *testing.T, repo *git.Repository, name string, h plumbing.Hash) {
	t.Helper()
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", name), h)); err != nil {
		t.Fatal(err)
	}
}

// The agent may commit on the task branch itself (a task description that says
// "open a PR" reliably produces this). A clean worktree must then still be
// pushed, judged against origin/<task branch> when it exists and origin/<base>
// on a first Step — and never when there is nothing to compare against.
func TestUnpushedCommits(t *testing.T) {
	repo, wt, base := initRepoWithCommit(t)
	// First Step shape: task branch created locally off base; origin only has base.
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("tasks/x")}); err != nil {
		t.Fatal(err)
	}
	setRemoteRef(t, repo, "master", base)

	ahead, sha, err := unpushedCommits(repo, "tasks/x", "master", io.Discard)
	if err != nil || ahead || sha != base.String() {
		t.Fatalf("fresh task branch: ahead=%v sha=%s err=%v, want not ahead at base", ahead, sha, err)
	}

	// The agent commits.
	if err := util.WriteFile(wt.Filesystem, "agent.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("agent.txt"); err != nil {
		t.Fatal(err)
	}
	agentCommit, err := wt.Commit("agent commit", &git.CommitOptions{Author: &object.Signature{Name: "a", Email: "a@a", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	ahead, sha, err = unpushedCommits(repo, "tasks/x", "master", io.Discard)
	if err != nil || !ahead || sha != agentCommit.String() {
		t.Fatalf("after agent commit: ahead=%v sha=%s err=%v, want ahead at the agent's commit", ahead, sha, err)
	}

	// A later Step: origin has the task branch at the agent's commit → nothing to push,
	// even though the branch is ahead of base.
	setRemoteRef(t, repo, "tasks/x", agentCommit)
	ahead, _, err = unpushedCommits(repo, "tasks/x", "master", io.Discard)
	if err != nil || ahead {
		t.Fatalf("origin task branch at head: ahead=%v err=%v, want not ahead", ahead, err)
	}

	// No remote refs at all: nothing to compare against, so never a spurious push.
	bare, bareWt, _ := initRepoWithCommit(t)
	if err := bareWt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("tasks/y")}); err != nil {
		t.Fatal(err)
	}
	ahead, _, err = unpushedCommits(bare, "tasks/y", "master", io.Discard)
	if err != nil || ahead {
		t.Fatalf("no remote refs: ahead=%v err=%v, want not ahead", ahead, err)
	}
}

// "Create a branch feature/x for this" makes the agent commit on a branch of
// its own. The task branch is the only ref we push, so it must be
// fast-forwarded to that work; a HEAD that doesn't descend from the task
// branch must be left alone.
func TestUnpushedCommits_ReconcilesHeadWithTaskBranch(t *testing.T) {
	repo, wt, base := initRepoWithCommit(t)
	setRemoteRef(t, repo, "master", base)
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("tasks/x")}); err != nil {
		t.Fatal(err)
	}
	// Agent branches off the task branch and commits there.
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("feature/x")}); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteFile(wt.Filesystem, "f.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	featureCommit, err := wt.Commit("on feature/x", &git.CommitOptions{Author: &object.Signature{Name: "a", Email: "a@a", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	ahead, sha, err := unpushedCommits(repo, "tasks/x", "master", io.Discard)
	if err != nil || !ahead || sha != featureCommit.String() {
		t.Fatalf("feature branch off task branch: ahead=%v sha=%s err=%v, want ahead at the feature commit", ahead, sha, err)
	}
	taskRef, err := repo.Reference(plumbing.NewBranchReferenceName("tasks/x"), true)
	if err != nil || taskRef.Hash() != featureCommit {
		t.Fatalf("task branch = %v (%v), want fast-forwarded to %s", taskRef, err, featureCommit)
	}

	// HEAD on an unrelated line: a second root commit on an orphan branch.
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("orphan"), Hash: base}); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteFile(wt.Filesystem, "o.txt", []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("o.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("unrelated", &git.CommitOptions{Author: &object.Signature{Name: "a", Email: "a@a", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	// orphan descends from base but not from tasks/x (which is at featureCommit).
	ahead, sha, err = unpushedCommits(repo, "tasks/x", "master", io.Discard)
	if err != nil || !ahead || sha != featureCommit.String() {
		t.Fatalf("unrelated HEAD: ahead=%v sha=%s err=%v, want the task branch untouched at the feature commit", ahead, sha, err)
	}
	taskRef, _ = repo.Reference(plumbing.NewBranchReferenceName("tasks/x"), true)
	if taskRef.Hash() != featureCommit {
		t.Fatalf("task branch moved to %s on an unrelated HEAD", taskRef.Hash())
	}
}

// The clone fetches every remote branch, so a stale origin/<task branch> from
// an abandoned earlier run can sit next to a fresh task branch that equals
// base. That is "nothing to push", not "ahead" — pushing base onto the stale
// ref would fail non-fast-forward where the old code cleanly skipped.
func TestUnpushedCommits_StaleRemoteTaskBranchIsNotAhead(t *testing.T) {
	repo, wt, base := initRepoWithCommit(t)
	setRemoteRef(t, repo, "master", base)
	// Stale remote task branch at some other commit.
	if err := util.WriteFile(wt.Filesystem, "old.txt", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("old.txt"); err != nil {
		t.Fatal(err)
	}
	stale, err := wt.Commit("abandoned run", &git.CommitOptions{Author: &object.Signature{Name: "a", Email: "a@a", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	setRemoteRef(t, repo, "tasks/x", stale)
	// Fresh first Step: task branch created off base.
	if err := wt.Checkout(&git.CheckoutOptions{Hash: base}); err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("tasks/x"), Hash: base}); err != nil {
		t.Fatal(err)
	}
	ahead, sha, err := unpushedCommits(repo, "tasks/x", "master", io.Discard)
	if err != nil || ahead || sha != base.String() {
		t.Fatalf("fresh branch at base beside a stale remote task branch: ahead=%v sha=%s err=%v, want not ahead", ahead, sha, err)
	}
}

// Dirty path: the agent switched to its own branch and left uncommitted work.
// Our commit lands on HEAD; the task branch must follow it before the push.
func TestReconcileTaskBranch_AfterCommitOnAgentBranch(t *testing.T) {
	repo, wt, base := initRepoWithCommit(t)
	setRemoteRef(t, repo, "master", base)
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("tasks/x")}); err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName("feature/x")}); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteFile(wt.Filesystem, "dirty.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	ours, err := wt.Commit("ours", &git.CommitOptions{Author: &object.Signature{Name: "d", Email: "d@d", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	tip, err := reconcileTaskBranch(repo, "tasks/x", io.Discard)
	if err != nil || tip != ours {
		t.Fatalf("tip = %s err=%v, want our commit %s", tip, err, ours)
	}
	taskRef, _ := repo.Reference(plumbing.NewBranchReferenceName("tasks/x"), true)
	if taskRef.Hash() != ours {
		t.Fatalf("task branch = %s, want fast-forwarded to %s", taskRef.Hash(), ours)
	}
}
