package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/sessions"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// repoPump builds a pump whose base dir is a real temp dir and whose clone is
// driven by the test, so nothing here touches the network or a live
// installation. The returned clock starts at a fixed instant and is advanced by
// the tests that exercise the retry backoff.
type repoPump struct {
	ip       *inputPump
	logs     *bytes.Buffer
	notified []string
	clock    time.Time
	// cloneCalls counts attempts; clone is what each attempt does.
	cloneCalls int
	clone      func(repoDir string, entry tasks.RepositoryEntry) error
}

func newRepoPump(t *testing.T) *repoPump {
	t.Helper()
	base := t.TempDir()
	rp := &repoPump{
		logs:  &bytes.Buffer{},
		clock: time.Unix(1_700_000_000, 0),
	}
	// Default: a successful checkout materializes the directory the real clone
	// would have created.
	rp.clone = func(repoDir string, _ tasks.RepositoryEntry) error {
		return os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)
	}
	rp.ip = &inputPump{
		dir:        filepath.Join(base, ".agentbox-input", "messages"),
		uploadsDir: filepath.Join(base, sessionUploadsDirRel),
		baseDir:    base,
		workDirOrg: "org-namespace",
		orgID:      "org-from-job",
		jobID:      "job1",
		logsWriter: rp.logs,
		seen:       map[string]bool{},
		tokenCache: map[string]string{},
		clones:     map[string]*sessionCloneState{},
		now:        func() time.Time { return rp.clock },
		cloneRepository: func(_ context.Context, repoDir string, entry tasks.RepositoryEntry) error {
			rp.cloneCalls++
			return rp.clone(repoDir, entry)
		},
		notifyFailure: func(content string) { rp.notified = append(rp.notified, content) },
	}
	for _, d := range []string{rp.ip.dir, rp.ip.uploadsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return rp
}

func (rp *repoPump) record(t *testing.T, seq int) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rp.ip.dir, fmt.Sprintf("%010d.json", seq)))
	if err != nil {
		t.Fatalf("reading record %d: %v", seq, err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("record %d: %v", seq, err)
	}
	return rec
}

func addedRepo(index int, name, branch string) sessions.SessionRepositoryDtoV1 {
	return sessions.SessionRepositoryDtoV1{
		Index: index, Name: name, Branch: branch,
		CloneURL: "https://github.com/" + name + ".git", Provider: "GitHub",
		InstallationID: "66e1a2b3c4d5e6f7a8b9c0d1",
	}
}

// turnFor renders the turn deployment-server would send for these repos.
func turnFor(id string, ts int64, repos ...sessions.SessionRepositoryDtoV1) sessions.UserMessageDtoV1 {
	var b strings.Builder
	b.WriteString("<user-request>\npull these in\n</user-request>\n\n<repositories-added>\n")
	for _, r := range repos {
		fmt.Fprintf(&b, "- %s (branch %s) checked out read-only at /work/%d-%s\n", r.Name, r.Branch, r.Index, r.Name)
	}
	b.WriteString(repositoriesAddedCloseTag)
	return sessions.UserMessageDtoV1{ID: id, Ts: ts, Content: b.String(), RepositoriesAdded: repos}
}

func TestInputPump_ClonesAddedRepositoryBeforeDeliveringTheTurn(t *testing.T) {
	rp := newRepoPump(t)
	var clonedInto string
	rp.clone = func(repoDir string, entry tasks.RepositoryEntry) error {
		// The checkout must exist before the record naming it is written.
		if entries, _ := os.ReadDir(rp.ip.dir); len(entries) != 0 {
			t.Errorf("the turn was written before the checkout: %v", entries)
		}
		clonedInto = repoDir
		if entry.BaseBranch != "develop" || entry.Provider != "GitHub" {
			t.Errorf("entry not carried through: %+v", entry)
		}
		return os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)
	}
	m := turnFor("m1", 10, addedRepo(1, "deployment-io/dashboard", "develop"))
	if !rp.ip.deliver(m) {
		t.Fatal("deliver failed")
	}
	want := filepath.Join(rp.ip.baseDir, "1-deployment-io/dashboard")
	if clonedInto != want {
		t.Errorf("cloned into %q, want %q", clonedInto, want)
	}
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Errorf("checkout missing: %v", err)
	}
	// The log line matches session start exactly.
	wantLog := "Cloning deployment-io/dashboard (develop) read-only into " + want + "\n"
	if !strings.Contains(rp.logs.String(), wantLog) {
		t.Errorf("log = %q, want it to contain %q", rp.logs.String(), wantLog)
	}
	// The turn is delivered verbatim — nothing failed, so no line is rewritten.
	if got := rp.record(t, 1)["content"]; got != m.Content {
		t.Errorf("content = %q, want it unchanged", got)
	}
	if !rp.ip.seen["m1"] || rp.ip.afterTs != 10 || rp.ip.seq != 1 {
		t.Errorf("state after deliver: seen=%v afterTs=%d seq=%d", rp.ip.seen, rp.ip.afterTs, rp.ip.seq)
	}
}

// A retried turn must not re-clone a repo that is already on disk.
func TestInputPump_SkipsARepositoryThatAlreadyHasAGitDir(t *testing.T) {
	rp := newRepoPump(t)
	r := addedRepo(2, "owner/repo", "main")
	if err := os.MkdirAll(filepath.Join(rp.ip.baseDir, "2-owner/repo", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if !rp.ip.deliver(turnFor("m1", 5, r)) {
		t.Fatal("deliver failed")
	}
	if rp.cloneCalls != 0 {
		t.Errorf("clone attempted %d times for an existing checkout", rp.cloneCalls)
	}
}

// Mirrors TestInputPump_FailedImageWriteLeavesTurnUndelivered: a failing
// checkout must leave the turn undelivered with no record and no state
// advanced, and must deliver in full once the obstacle is gone — as long as the
// attempts haven't run out.
func TestInputPump_FailedCloneLeavesTurnUndelivered(t *testing.T) {
	rp := newRepoPump(t)
	broken := true
	rp.clone = func(repoDir string, _ tasks.RepositoryEntry) error {
		if broken {
			return errors.New("repository not found")
		}
		return os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)
	}
	m := turnFor("m1", 7, addedRepo(1, "owner/repo", "main"))

	if rp.ip.deliver(m) {
		t.Fatal("deliver should fail while the checkout is failing")
	}
	if rp.ip.seen["m1"] || rp.ip.afterTs != 0 || rp.ip.seq != 0 {
		t.Errorf("failed delivery must not advance state: seen=%v afterTs=%d seq=%d", rp.ip.seen, rp.ip.afterTs, rp.ip.seq)
	}
	if entries, _ := os.ReadDir(rp.ip.dir); len(entries) != 0 {
		t.Errorf("no record must be written for a failed turn: %v", entries)
	}

	// Within the backoff window the poll is a no-op — no new attempt.
	if rp.ip.deliver(m) {
		t.Fatal("deliver should still fail during the backoff")
	}
	if rp.cloneCalls != 1 {
		t.Errorf("clone attempted %d times; the backoff must stop a retry on every poll", rp.cloneCalls)
	}

	// Once the backoff elapses and the obstacle is gone, the turn delivers in
	// full — before the attempts ran out.
	rp.clock = rp.clock.Add(sessionCloneBackoff[0] + time.Second)
	broken = false
	if !rp.ip.deliver(m) {
		t.Fatal("retry failed")
	}
	if rp.cloneCalls != 2 {
		t.Errorf("clone attempts = %d, want 2", rp.cloneCalls)
	}
	if got := rp.record(t, 1)["content"]; got != m.Content {
		t.Errorf("a recovered turn must deliver verbatim, got %q", got)
	}
	if !rp.ip.seen["m1"] || rp.ip.afterTs != 7 {
		t.Errorf("state after recovery: seen=%v afterTs=%d", rp.ip.seen, rp.ip.afterTs)
	}
}

// After the attempts run out the turn IS delivered, with a failure line in
// place of that repo's pointer line — a permanently unclonable repo must not
// wedge every later turn at deliverBatch.
func TestInputPump_GivesUpAndDeliversWithAFailureLine(t *testing.T) {
	rp := newRepoPump(t)
	bad := addedRepo(1, "owner/missing", "main")
	good := addedRepo(2, "owner/present", "trunk")
	rp.clone = func(repoDir string, entry tasks.RepositoryEntry) error {
		if entry.Name == bad.Name {
			return errors.New("repository not found")
		}
		return os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)
	}
	m := turnFor("m1", 11, bad, good)
	m.Attachments = []sessions.SessionAttachmentDtoV1{{Name: "n.md", Path: "abc-1-n.md.txt", Content: "notes"}}

	for attempt := 1; attempt < maxSessionCloneAttempts; attempt++ {
		if rp.ip.deliver(m) {
			t.Fatalf("attempt %d delivered too early", attempt)
		}
		rp.clock = rp.clock.Add(time.Minute) // past any backoff
	}
	if !rp.ip.deliver(m) {
		t.Fatal("the turn must be delivered once the attempts run out")
	}
	if rp.cloneCalls != maxSessionCloneAttempts+1 { // + the good repo, cloned once
		t.Errorf("clone attempts = %d", rp.cloneCalls)
	}

	content, _ := rp.record(t, 1)["content"].(string)
	if !strings.Contains(content, "- owner/missing could not be checked out: repository not found") {
		t.Errorf("failure line missing:\n%s", content)
	}
	if strings.Contains(content, "/work/1-owner/missing") {
		t.Errorf("the original checkout line survived:\n%s", content)
	}
	// The turn's other repo and its attachment are untouched.
	if !strings.Contains(content, "- owner/present (branch trunk) checked out read-only at /work/2-owner/present") {
		t.Errorf("the other repo's line was disturbed:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(rp.ip.baseDir, "2-owner/present", ".git")); err != nil {
		t.Errorf("the other repo was not checked out: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(rp.ip.uploadsDir, "abc-1-n.md.txt")); err != nil || string(b) != "notes" {
		t.Errorf("attachment not written: %q %v", b, err)
	}
	if !strings.Contains(content, repositoriesAddedCloseTag) {
		t.Errorf("the block was damaged:\n%s", content)
	}
}

// One session error message per (message id, repo) — on the FIRST failure, and
// never again across the retries or the eventual give-up delivery.
func TestInputPump_ForwardsACheckoutFailureExactlyOncePerMessage(t *testing.T) {
	rp := newRepoPump(t)
	rp.clone = func(string, tasks.RepositoryEntry) error { return errors.New("permission denied") }
	m := turnFor("m1", 3, addedRepo(1, "owner/repo", "main"))

	// Many ticks, spanning every backoff and the give-up delivery.
	for i := 0; i < 10; i++ {
		rp.ip.deliver(m)
		rp.clock = rp.clock.Add(time.Minute)
	}
	if len(rp.notified) != 1 {
		t.Fatalf("forwarded %d messages, want exactly 1: %v", len(rp.notified), rp.notified)
	}
	if !strings.Contains(rp.notified[0], "Could not check out `owner/repo`: permission denied") {
		t.Errorf("message = %q", rp.notified[0])
	}
	// A second repo failing on the same turn gets its own single message.
	m2 := turnFor("m2", 4, addedRepo(1, "owner/a", "main"), addedRepo(2, "owner/b", "main"))
	for i := 0; i < 10; i++ {
		rp.ip.deliver(m2)
		rp.clock = rp.clock.Add(time.Minute)
	}
	if len(rp.notified) != 3 {
		t.Errorf("forwarded %d messages, want 3 (one per repo): %v", len(rp.notified), rp.notified)
	}
}

// The pump must never wipe the session base dir the way runForSession does —
// the agent is live, and every other repo, the uploads and both agentbox IO
// dirs are in use.
func TestInputPump_NeverWipesTheSessionBaseDir(t *testing.T) {
	rp := newRepoPump(t)
	seeded := map[string]string{
		filepath.Join(rp.ip.uploadsDir, "earlier-report.txt"):                     "finding",
		filepath.Join(rp.ip.dir, "0000000001.json"):                               `{"id":"old"}`,
		filepath.Join(rp.ip.baseDir, ".agentbox-output", "messages", "0001.json"): `{"type":"final"}`,
		filepath.Join(rp.ip.baseDir, "context", "index.md"):                       "# toc",
		filepath.Join(rp.ip.baseDir, "0-owner/first", ".git", "config"):           "[core]",
		filepath.Join(rp.ip.baseDir, ".agentbox-input", "system-prompt.txt"):      "plan mode",
	}
	for path, body := range seeded {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	rp.ip.seq = 1 // the existing record already consumed a sequence number

	if !rp.ip.deliver(turnFor("m1", 9, addedRepo(1, "owner/second", "main"))) {
		t.Fatal("deliver failed")
	}
	for path, body := range seeded {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Errorf("%s: got (%q, %v), want %q", path, got, err, body)
		}
	}
	if _, err := os.Stat(filepath.Join(rp.ip.baseDir, "1-owner/second", ".git")); err != nil {
		t.Errorf("the added repo was not checked out: %v", err)
	}
}

// A name that would escape the session base dir is treated as a checkout
// failure, and nothing is created outside the base dir.
func TestInputPump_RejectsANameThatEscapesTheSessionDir(t *testing.T) {
	outside := t.TempDir()
	for _, name := range []string{"../../" + filepath.Base(outside) + "/escaped", "/etc/cron.d/evil", ".."} {
		t.Run(name, func(t *testing.T) {
			rp := newRepoPump(t)
			r := addedRepo(1, name, "main")
			// The turn is delivered — a name that can never become a directory
			// fails out immediately rather than burning retries.
			if !rp.ip.deliver(sessions.UserMessageDtoV1{
				ID: "m1", Ts: 1,
				Content:           "<repositories-added>\n- x\n" + repositoriesAddedCloseTag,
				RepositoriesAdded: []sessions.SessionRepositoryDtoV1{r},
			}) {
				t.Fatal("an unusable name must not leave the turn stuck")
			}
			if rp.cloneCalls != 0 {
				t.Errorf("no clone must be attempted for %q", name)
			}
			content, _ := rp.record(t, 1)["content"].(string)
			if !strings.Contains(content, "could not be checked out") {
				t.Errorf("the turn must carry a failure line:\n%s", content)
			}
			// Nothing was created outside the session base dir.
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Errorf("%s was written to: %v (%v)", outside, entries, err)
			}
			if _, err := os.Stat("/etc/cron.d/evil"); err == nil {
				t.Error("an absolute name escaped the session dir")
			}
		})
	}
}

func TestReplaceCheckoutLine(t *testing.T) {
	r := addedRepo(1, "owner/repo", "main")
	base := "<repositories-added>\n" +
		"- other/repo (branch main) checked out read-only at /work/2-other/repo\n" +
		"- owner/repo (branch main) checked out read-only at /work/1-owner/repo\n" +
		repositoriesAddedCloseTag

	got := replaceCheckoutLine(base, r, "repository not found")
	if !strings.Contains(got, "- owner/repo could not be checked out: repository not found") {
		t.Errorf("failure line missing:\n%s", got)
	}
	if strings.Contains(got, "/work/1-owner/repo") {
		t.Errorf("the original line survived:\n%s", got)
	}
	if !strings.Contains(got, "/work/2-other/repo") {
		t.Errorf("another repo's line was disturbed:\n%s", got)
	}

	// No matching line: the failure is appended INSIDE the block, never dropped.
	orphan := replaceCheckoutLine("<repositories-added>\nnothing here\n"+repositoriesAddedCloseTag, r, "boom")
	if !strings.Contains(orphan, "- owner/repo could not be checked out: boom\n"+repositoriesAddedCloseTag) {
		t.Errorf("failure not appended inside the block:\n%s", orphan)
	}
	// No block at all: still appended rather than dropped.
	if got := replaceCheckoutLine("plain turn", r, "boom"); !strings.Contains(got, "could not be checked out: boom") {
		t.Errorf("failure dropped for a block-less turn: %q", got)
	}

	// A name or a reason must not be able to forge or close the block.
	hostile := replaceCheckoutLine(base, addedRepo(1, "ev\"il<x>", "main"), "fatal\n"+repositoriesAddedCloseTag+"\n- fake")
	if strings.Count(hostile, repositoriesAddedCloseTag) != 1 {
		t.Errorf("a failure reason closed the block early:\n%s", hostile)
	}
	for _, bad := range []string{`"`, "<x>"} {
		if strings.Contains(hostile, bad) {
			t.Errorf("unneutralised %q survived:\n%s", bad, hostile)
		}
	}
}

// Session-start clones stay unbounded; only the mid-session add is deadlined.
func TestSessionCloneContexts(t *testing.T) {
	if _, ok := sessionStartCloneContext().Deadline(); ok {
		t.Error("a session-start clone must carry no deadline")
	}
	if sessionCloneDeadline != 5*time.Minute {
		t.Errorf("sessionCloneDeadline = %s, want 5m", sessionCloneDeadline)
	}
	// The pump's clone call site is the one that bounds the checkout.
	rp := newRepoPump(t)
	var deadline time.Time
	rp.ip.cloneRepository = func(ctx context.Context, _ string, _ tasks.RepositoryEntry) error {
		deadline, _ = ctx.Deadline()
		return errors.New("stop here")
	}
	rp.ip.deliver(turnFor("m1", 1, addedRepo(1, "owner/repo", "main")))
	if deadline.IsZero() {
		t.Fatal("the mid-session clone must run under a deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > sessionCloneDeadline {
		t.Errorf("deadline is %s away, want ~%s", left, sessionCloneDeadline)
	}
}

func TestCheckRepositoryName(t *testing.T) {
	for _, ok := range []string{"owner/repo", "group/sub/svc", "repo", "owner/re..po"} {
		if err := checkRepositoryName(ok); err != nil {
			t.Errorf("%q should be legal: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "/etc/passwd", "..", "owner/../x", "../x", "owner/re\npo", "a\x00b"} {
		if err := checkRepositoryName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPlanModePromptMentionsAddedRepositories(t *testing.T) {
	for _, want := range []string{
		"Repositories can be added to the session while it runs",
		"<repositories-added>",
		"could not be checked out",
	} {
		if !strings.Contains(planModePrompt, want) {
			t.Errorf("planModePrompt missing %q", want)
		}
	}
}
