package loggers

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/logs"
)

// writerHarness is a JobLogsWriter with a captured sink, a settable clock and
// a timer the test fires by hand.
type writerHarness struct {
	mu     sync.Mutex
	lines  []JobLog
	now    time.Time
	timers []*fakeTimer
	w      *JobLogsWriter
}

type fakeTimer struct {
	f       func()
	stopped bool
}

func newHarness(tagged bool) *writerHarness {
	h := &writerHarness{now: time.Unix(1000, 0)}
	h.w = newJobLogsWriter(tagged, func(l JobLog) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.lines = append(h.lines, l)
	}, func() time.Time { return h.now }, func(_ time.Duration, f func()) func() bool {
		t := &fakeTimer{f: f}
		h.timers = append(h.timers, t)
		return func() bool { t.stopped = true; return true }
	})
	return h
}

func (h *writerHarness) write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

// fireTimer fires the latest group timer, as time.AfterFunc would.
func (h *writerHarness) fireTimer(t *testing.T) {
	t.Helper()
	if len(h.timers) == 0 {
		t.Fatal("no timer started")
	}
	h.timers[len(h.timers)-1].f()
}

func (h *writerHarness) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, l := range h.lines {
		out = append(out, l.Message)
	}
	return out
}

func (h *writerHarness) snapshot() []JobLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]JobLog(nil), h.lines...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUntaggedWriterSplitsLikeBefore(t *testing.T) {
	h := newHarness(false)
	h.write(t, h.w, "first li")
	h.write(t, h.w, "ne\n\n  second  \r\nthird\r")
	h.write(t, h.w, "fourth\n   \n")
	h.write(t, h.w, "trailing partial")
	if err := h.w.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"first line", "second", "third", "fourth", "trailing partial"}
	if got := h.messages(); !equalStrings(got, want) {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	for _, l := range h.snapshot() {
		if l.Stage != "" || l.Kind != "" || l.Group != nil {
			t.Fatalf("untagged line %+v carries a tag", l)
		}
	}
}

func TestUntaggedWriterNoSectionsNoFolding(t *testing.T) {
	h := newHarness(false)
	h.w.SetStage(logs.LogStageImplement)
	for i := 0; i < 954; i++ {
		h.write(t, h.w, fmt.Sprintf("Successfully uploaded file: assets/f%d.js\n", i))
	}
	h.write(t, h.w, `{"status":"Already exists","progressDetail":{},"id":"dd051e1b8c92"}`+"\n")
	h.w.Close()
	if got := len(h.snapshot()); got != 955 {
		t.Fatalf("got %d lines, want 955", got)
	}
	if len(h.timers) != 0 {
		t.Fatal("an untagged writer started a group timer")
	}
}

func TestLongLineIsCutOnRuneBoundary(t *testing.T) {
	h := newHarness(false)
	// 70 KiB of 3-byte runes: 64 KiB is not a multiple of 3, so a byte cut
	// would split a rune.
	long := strings.Repeat("€", 70*1024/3)
	h.write(t, h.w, long[:40000])
	h.write(t, h.w, long[40000:]+"\nafter\n")
	h.w.Close()
	got := h.messages()
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}
	if len(got[0]) > maxLineBytes || len(got[0]) < maxLineBytes-3 {
		t.Fatalf("first piece is %d bytes", len(got[0]))
	}
	for _, s := range got[:2] {
		if !utf8.ValidString(s) {
			t.Fatal("piece is not valid UTF-8")
		}
	}
	if got[0]+got[1] != long {
		t.Fatal("pieces don't add up to the line")
	}
	if got[2] != "after" {
		t.Fatalf("later line = %q", got[2])
	}
}

func TestLineOfExactlyMaxBytesIsEmitted(t *testing.T) {
	h := newHarness(false)
	exact := strings.Repeat("x", maxLineBytes)
	h.write(t, h.w, exact)
	if got := h.messages(); len(got) != 1 || got[0] != exact {
		t.Fatalf("a %d-byte pending line was not emitted: %d lines", maxLineBytes, len(got))
	}
	h.write(t, h.w, "\nafter\n")
	h.w.Close()
	if got := h.messages(); len(got) != 2 || got[1] != "after" {
		t.Fatalf("got %d lines after the exact line", len(got))
	}
}

func TestLineOfMaxBytesEndingInPartialRune(t *testing.T) {
	h := newHarness(false)
	// 64 KiB whose last two bytes are the start of a 3-byte rune.
	head := strings.Repeat("x", maxLineBytes-2)
	h.write(t, h.w, head+"€"[:2])
	if got := h.messages(); len(got) != 1 || got[0] != head {
		t.Fatalf("want the line cut before the partial rune, got %d lines", len(got))
	}
	h.write(t, h.w, "€"[2:]+"\n")
	h.w.Close()
	if got := h.messages(); len(got) != 2 || got[1] != "€" {
		t.Fatalf("rune not completed: %q", got)
	}
}

func TestUntaggedStreamsShareOneBuffer(t *testing.T) {
	// An untagged job's container streams write to the job writer itself, so
	// interleaved stdout and stderr chunks join as they always have.
	h := newHarness(false)
	stdout, stderr := NewSource(h.w), NewSource(h.w)
	h.write(t, stdout, "a")
	h.write(t, stderr, "b\n")
	h.write(t, stdout, "c\n")
	EndSource(stdout)
	EndSource(stderr)
	h.w.Close()
	if got := h.messages(); !equalStrings(got, []string{"ab", "c"}) {
		t.Fatalf("got %q, want [ab c]", got)
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	h := newHarness(true)
	src := NewSource(h.w)
	h.w.Close()
	if _, err := h.w.Write([]byte("x\n")); err != io.ErrClosedPipe {
		t.Fatalf("Write after Close = %v, want io.ErrClosedPipe", err)
	}
	if _, err := src.Write([]byte("x\n")); err != io.ErrClosedPipe {
		t.Fatalf("source Write after Close = %v, want io.ErrClosedPipe", err)
	}
}

func TestSourcesNeverSplice(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		h := newHarness(tagged)
		var wg sync.WaitGroup
		for _, name := range []string{"a", "b"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				src := io.Writer(h.w.newSource(logs.LogKindText, false, ""))
				for i := 0; i < 200; i++ {
					io.WriteString(src, name+"-")
					io.WriteString(src, fmt.Sprintf("%d-", i))
					io.WriteString(src, name+"\n")
				}
				EndSource(src)
			}(name)
		}
		wg.Wait()
		h.w.Close()
		got := h.messages()
		if len(got) != 400 {
			t.Fatalf("tagged=%v: got %d lines, want 400", tagged, len(got))
		}
		for _, s := range got {
			if !(strings.HasPrefix(s, "a-") && strings.HasSuffix(s, "-a")) && !(strings.HasPrefix(s, "b-") && strings.HasSuffix(s, "-b")) {
				t.Fatalf("tagged=%v: spliced line %q", tagged, s)
			}
		}
		if len(h.w.sources) != 0 {
			t.Fatalf("ended sources are still held: %d", len(h.w.sources))
		}
	}
}

func TestEndedSourceIsDroppedAndPartialEmitted(t *testing.T) {
	h := newHarness(true)
	src := AgentOutput(h.w, "claude-code")
	io.WriteString(src, "[text] half")
	if len(h.w.sources) != 1 {
		t.Fatalf("held sources = %d, want 1", len(h.w.sources))
	}
	EndSource(src)
	if len(h.w.sources) != 0 {
		t.Fatalf("held sources after End = %d, want 0", len(h.w.sources))
	}
	if got := h.messages(); !equalStrings(got, []string{"[text] half"}) {
		t.Fatalf("messages = %q", got)
	}
}

func TestCloseEmitsEverySourcesPartialLine(t *testing.T) {
	h := newHarness(true)
	io.WriteString(h.w, "base partial")
	io.WriteString(AgentOutput(h.w, ""), "[tool] Bash: ls")
	h.w.Close()
	got := h.snapshot()
	if len(got) != 2 || got[0].Message != "base partial" || got[1].Kind != logs.LogKindTool {
		t.Fatalf("lines = %+v", got)
	}
}

func TestWithKindAndAgentOutputFallBack(t *testing.T) {
	var buf bytes.Buffer
	untagged := newHarness(false).w
	for _, w := range []io.Writer{&buf, untagged, io.MultiWriter(&buf)} {
		if got := WithKind(w, logs.LogKindJSON); got != w {
			t.Errorf("WithKind(%T) wrapped a writer it should return as is", w)
		}
		if got := AgentOutput(w, "codex"); got != w {
			t.Errorf("AgentOutput(%T) wrapped a writer it should return as is", w)
		}
		if got := NewSource(w); got != w {
			t.Errorf("NewSource(%T) wrapped a writer it should return as is", w)
		}
	}
	tagged := newHarness(true).w
	if _, ok := WithKind(tagged, logs.LogKindJSON).(*SourceWriter); !ok {
		t.Error("WithKind on a tagged job writer did not return a source writer")
	}
	if _, ok := AgentOutput(tagged, "codex").(*SourceWriter); !ok {
		t.Error("AgentOutput on a tagged job writer did not return a source writer")
	}
}

func TestStageStartsAtSetupAndWritesSections(t *testing.T) {
	h := newHarness(true)
	if h.w.Stage() != logs.LogStageSetup {
		t.Fatalf("initial stage = %q", h.w.Stage())
	}
	steps := []struct {
		stage string
		line  string
	}{
		{logs.LogStageSetup, "Checking out repo"},
		{logs.LogStageSetup, "Materialized 9 context file(s) into /work/context"},
		{logs.LogStageImplement, "Disk before agent step"},
		{logs.LogStageReview, "Review round 1: effort high"},
		{logs.LogStageQA, "QA round 1"},
		{logs.LogStagePR, "Committed a8ec2a0"},
		{logs.LogStagePR, "Opened PR #259"},
	}
	for _, s := range steps {
		h.w.SetStage(s.stage)
		h.write(t, h.w, s.line+"\n")
	}
	WithKind(h.w, logs.LogKindDone).Write([]byte("Successfully executed - x\n"))
	h.w.Close()
	want := []JobLog{
		{Message: "Checking out repo", Stage: "setup", Kind: "text"},
		{Message: "Materialized 9 context file(s) into /work/context", Stage: "setup", Kind: "text"},
		{Message: "=== Implement ===", Stage: "implement", Kind: "section"},
		{Message: "Disk before agent step", Stage: "implement", Kind: "text"},
		{Message: "=== Review ===", Stage: "review", Kind: "section"},
		{Message: "Review round 1: effort high", Stage: "review", Kind: "text"},
		{Message: "=== QA ===", Stage: "qa", Kind: "section"},
		{Message: "QA round 1", Stage: "qa", Kind: "text"},
		{Message: "=== Pull request ===", Stage: "pr", Kind: "section"},
		{Message: "Committed a8ec2a0", Stage: "pr", Kind: "text"},
		{Message: "Opened PR #259", Stage: "pr", Kind: "text"},
		{Message: "Successfully executed - x", Stage: "pr", Kind: "done"},
	}
	assertLines(t, h.snapshot(), want)
}

func assertLines(t *testing.T, got, want []JobLog) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), dump(got))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Message != w.Message || g.Stage != w.Stage || g.Kind != w.Kind || !sameGroup(g.Group, w.Group) {
			t.Errorf("line %d = %s, want %s", i, dumpLine(g), dumpLine(w))
		}
	}
}

func sameGroup(a, b *logs.JobLogGroupV1) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func dumpLine(l JobLog) string {
	g := ""
	if l.Group != nil {
		g = fmt.Sprintf(" group=%s/%d", l.Group.Name, l.Group.Count)
	}
	return fmt.Sprintf("{%q %s/%s%s}", l.Message, l.Stage, l.Kind, g)
}

func dump(ls []JobLog) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(dumpLine(l) + "\n")
	}
	return b.String()
}

func TestRunFoldsToOneLine(t *testing.T) {
	h := newHarness(true)
	h.w.SetStage(logs.LogStageQA)
	h.write(t, h.w, "QA round 1: building the preview\n")
	for i := 0; i < 3; i++ {
		h.now = time.Unix(int64(2000+i), 0)
		h.write(t, h.w, fmt.Sprintf("Successfully uploaded file: assets/f%d.js\n", i))
	}
	h.now = time.Unix(2010, 0)
	h.write(t, h.w, "verify_preview_reachable: attempt 1\n")
	got := h.snapshot()
	assertLines(t, got, []JobLog{
		{Message: "=== QA ===", Stage: "qa", Kind: "section"},
		{Message: "QA round 1: building the preview", Stage: "qa", Kind: "text"},
		{Message: "Successfully uploaded file: assets/f2.js", Stage: "qa", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupFileUploads, Count: 3}},
		{Message: "verify_preview_reachable: attempt 1", Stage: "qa", Kind: "text"},
	})
	if got[2].Ts != 2000 {
		t.Fatalf("group Ts = %d, want the first line's 2000", got[2].Ts)
	}
}

func TestRunOfOneStaysPlain(t *testing.T) {
	h := newHarness(true)
	h.write(t, h.w, "Successfully uploaded file: a.js\nnext\n")
	assertLines(t, h.snapshot(), []JobLog{
		{Message: "Successfully uploaded file: a.js", Stage: "setup", Kind: "text"},
		{Message: "next", Stage: "setup", Kind: "text"},
	})
}

func TestGroupEmittedOnOtherGroupFromAnotherSource(t *testing.T) {
	h := newHarness(true)
	h.write(t, h.w, "Successfully uploaded file: a.js\nSuccessfully uploaded file: b.js\n")
	agent := AgentOutput(h.w, "claude-code")
	h.write(t, agent, "[tool] Bash: ls\n")
	assertLines(t, h.snapshot(), []JobLog{
		{Message: "Successfully uploaded file: b.js", Stage: "setup", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupFileUploads, Count: 2}},
		{Message: "[tool] Bash: ls", Stage: "setup", Kind: "tool"},
	})
}

func TestGroupEmittedOnStageChangeBeforeSection(t *testing.T) {
	h := newHarness(true)
	h.write(t, h.w, "Successfully uploaded file: a.js\nSuccessfully uploaded file: b.js\n")
	h.w.SetStage(logs.LogStageImplement)
	assertLines(t, h.snapshot(), []JobLog{
		{Message: "Successfully uploaded file: b.js", Stage: "setup", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupFileUploads, Count: 2}},
		{Message: "=== Implement ===", Stage: "implement", Kind: "section"},
	})
}

func TestGroupEmittedByTimerAndNotAfterClose(t *testing.T) {
	h := newHarness(true)
	h.write(t, h.w, "Successfully uploaded file: a.js\nSuccessfully uploaded file: b.js\n")
	if len(h.messages()) != 0 {
		t.Fatal("a group went out before its window closed")
	}
	h.fireTimer(t)
	assertLines(t, h.snapshot(), []JobLog{
		{Message: "Successfully uploaded file: b.js", Stage: "setup", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupFileUploads, Count: 2}},
	})
	// A stale timer from a group already emitted does nothing.
	h.write(t, h.w, "Successfully uploaded file: c.js\nSuccessfully uploaded file: d.js\n")
	h.timers[0].f()
	if len(h.snapshot()) != 1 {
		t.Fatal("a stale timer emitted the new group")
	}
	h.w.Close()
	if !h.timers[1].stopped {
		t.Fatal("Close did not stop the group timer")
	}
	h.fireTimer(t)
	assertLines(t, h.snapshot(), []JobLog{
		{Message: "Successfully uploaded file: b.js", Stage: "setup", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupFileUploads, Count: 2}},
		{Message: "Successfully uploaded file: d.js", Stage: "setup", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupFileUploads, Count: 2}},
	})
}

func TestTwelveSecondRunGivesThreeGroupLines(t *testing.T) {
	h := newHarness(true)
	h.w.SetStage(logs.LogStageQA)
	// One line a second for 12 s; the timer fires 5 s after each group opens.
	opened := 0
	for sec := 0; sec < 12; sec++ {
		if sec-opened == 5 {
			h.fireTimer(t)
			opened = sec
		}
		h.now = time.Unix(int64(3000+sec), 0)
		h.write(t, h.w, fmt.Sprintf(`{"status":"Pushing","progressDetail":{"current":%d},"id":"abc"}`+"\n", sec))
	}
	h.w.Close()
	got := h.snapshot()[1:] // after the section line
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(got), dump(got))
	}
	counts := []int{5, 5, 2}
	for i, l := range got {
		if l.Group == nil || l.Group.Name != GroupDockerProgress || l.Group.Count != counts[i] || l.Stage != "qa" {
			t.Fatalf("line %d = %s", i, dumpLine(l))
		}
		if l.Ts != int64(3000+5*i) {
			t.Fatalf("line %d Ts = %d", i, l.Ts)
		}
	}
}

func TestAgentEventsFoldOnlyInClaudeOutput(t *testing.T) {
	h := newHarness(true)
	h.w.SetStage(logs.LogStageImplement)
	h.write(t, h.w, "[system] task_started\n[rate_limit_event]\n")
	claude := AgentOutput(h.w, "claude-code")
	h.write(t, claude, "[system] thinking_tokens\n[system] task_started\n[rate_limit_event]\n[tool_progress]\n")
	h.write(t, claude, "[tool] Bash: ls\n[tool] Bash: pwd\n[result] (success, 4 bytes) ok\n[result] (success, 4 bytes) ok\n")
	codex := AgentOutput(h.w, "codex")
	h.write(t, codex, "[rate_limit_event]\n")
	h.w.Close()
	assertLines(t, h.snapshot(), []JobLog{
		{Message: "=== Implement ===", Stage: "implement", Kind: "section"},
		{Message: "[system] task_started", Stage: "implement", Kind: "text"},
		{Message: "[rate_limit_event]", Stage: "implement", Kind: "text"},
		{Message: "[tool_progress]", Stage: "implement", Kind: "text", Group: &logs.JobLogGroupV1{Name: GroupAgentEvents, Count: 4}},
		{Message: "[tool] Bash: ls", Stage: "implement", Kind: "tool"},
		{Message: "[tool] Bash: pwd", Stage: "implement", Kind: "tool"},
		{Message: "[result] (success, 4 bytes) ok", Stage: "implement", Kind: "result"},
		{Message: "[result] (success, 4 bytes) ok", Stage: "implement", Kind: "result"},
		{Message: "[rate_limit_event]", Stage: "implement", Kind: "text"},
	})
}

func TestToJobLogDtosCarriesTags(t *testing.T) {
	group := &logs.JobLogGroupV1{Name: GroupDockerProgress, Count: 7}
	messages, dtos := toJobLogDtos("job", []JobLog{
		{Message: "m", Ts: 5, Stage: logs.LogStageQA, Kind: logs.LogKindText, Group: group},
		{Message: "plain", Ts: 6},
		{ErrorMessage: "boom", Ts: 7},
	})
	if !equalStrings(messages, []string{"m", "plain", "boom"}) {
		t.Fatalf("messages = %q", messages)
	}
	d := dtos[0]
	if d.ID != "job" || d.Message != "m" || d.Ts != 5 || d.Stage != "qa" || d.Kind != "text" || d.Group != group {
		t.Fatalf("dto = %+v", d)
	}
	if dtos[1].Stage != "" || dtos[1].Kind != "" || dtos[1].Group != nil {
		t.Fatalf("untagged dto = %+v", dtos[1])
	}
}

// The section lines must not read like the end-of-stage summary headers the
// review and QA stages print (run_review_stage.go, run_qa_stage_report.go).
func TestSectionLinesDifferFromSummaryHeaders(t *testing.T) {
	for stage, line := range sectionLines {
		if strings.HasPrefix(line, "---") {
			t.Errorf("section line for %s = %q; it reads like a stage summary header", stage, line)
		}
	}
}

// TestConcurrentStress runs every concurrent path of a tagged writer at once,
// for the race detector: the job's own stream (whole lines: it is one shared
// stream, as the pipe was), two agent streams and a fixed-kind stream writing
// partial lines, SetStage in a loop, a real group timer firing on its own
// goroutine, and Close while a late writer is still writing.
// Nothing is lost, spliced or reordered; every line has a stage and a kind;
// folded counts add up to the noise written.
func TestConcurrentStress(t *testing.T) {
	var mu sync.Mutex
	var got []JobLog
	w := newJobLogsWriter(true, func(l JobLog) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, l)
	}, time.Now, func(_ time.Duration, f func()) func() bool {
		// A real timer with a short window, so it fires during the writes.
		return time.AfterFunc(time.Millisecond, f).Stop
	})

	const n = 300
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { // the job's own stream, shared with the late writer: whole lines only
		for i := 0; i < n; i++ {
			io.WriteString(w, fmt.Sprintf("base-%d\n", i))
			io.WriteString(w, fmt.Sprintf("Successfully uploaded file: assets/f%d.js\n", i))
		}
	})
	for _, name := range []string{"a", "b"} {
		name := name
		run(func() { // an agent stream: tool calls in chunks and agent events
			src := AgentOutput(w, "claude-code")
			for i := 0; i < n; i++ {
				io.WriteString(src, "[tool] Bash: ")
				io.WriteString(src, fmt.Sprintf("%s-%d", name, i))
				io.WriteString(src, "\n[system] thinking_tokens\n")
			}
			EndSource(src)
		})
	}
	run(func() { // a fixed-kind stream
		src := WithKind(w, logs.LogKindJSON)
		for i := 0; i < n; i++ {
			io.WriteString(src, fmt.Sprintf(`{"json":%d}`, i))
			io.WriteString(src, "\n")
		}
		EndSource(src)
	})
	stagesDone := make(chan struct{})
	run(func() { // stage changes throughout
		stages := []string{logs.LogStageImplement, logs.LogStageReview, logs.LogStageQA, logs.LogStagePR}
		for i := 0; ; i++ {
			select {
			case <-stagesDone:
				return
			default:
			}
			w.SetStage(stages[i%len(stages)])
			time.Sleep(50 * time.Microsecond)
		}
	})

	time.Sleep(20 * time.Millisecond)
	close(stagesDone)
	wg.Wait()

	// Noise only: groups stay open long enough for the real timer to fire
	// while two streams keep joining them.
	const m = 60
	for _, name := range []string{"x", "y"} {
		name := name
		run(func() {
			src := NewSource(w)
			for i := 0; i < m; i++ {
				io.WriteString(src, "Successfully uploaded file: assets/")
				io.WriteString(src, fmt.Sprintf("%s%d.js\n", name, i))
				time.Sleep(200 * time.Microsecond)
			}
			EndSource(src)
		})
	}
	wg.Wait()

	// A late writer that is still writing when Close runs.
	lateOK := 0
	lateDone := make(chan struct{})
	go func() {
		defer close(lateDone)
		for i := 0; ; i++ {
			if _, err := io.WriteString(w, fmt.Sprintf("late-%d\n", i)); err != nil {
				if err != io.ErrClosedPipe {
					t.Errorf("late write: %v", err)
				}
				return
			}
			lateOK++
		}
	}()

	time.Sleep(2 * time.Millisecond)
	w.Close()
	<-lateDone
	time.Sleep(5 * time.Millisecond) // a timer that fires after Close must emit nothing

	mu.Lock()
	defer mu.Unlock()
	validStage := map[string]bool{logs.LogStageSetup: true, logs.LogStageImplement: true,
		logs.LogStageReview: true, logs.LogStageQA: true, logs.LogStagePR: true}
	next := map[string]int{} // producer → next index expected, so order is kept
	folded := map[string]int{}
	for _, l := range got {
		if !validStage[l.Stage] || l.Kind == "" {
			t.Fatalf("line without a valid stage or kind: %+v", l)
		}
		count := 1
		if l.Group != nil {
			count = l.Group.Count
		}
		var producer string
		var idx int
		switch {
		case l.Kind == logs.LogKindSection:
			continue
		case strings.HasPrefix(l.Message, "Successfully uploaded file: assets/"):
			folded[GroupFileUploads] += count
			continue
		case l.Message == "[system] thinking_tokens":
			folded[GroupAgentEvents] += count
			continue
		case l.Group != nil:
			t.Fatalf("unexpected group line: %+v", l)
		}
		if _, err := fmt.Sscanf(l.Message, "base-%d", &idx); err == nil && l.Message == fmt.Sprintf("base-%d", idx) {
			producer = "base"
		} else if _, err := fmt.Sscanf(l.Message, "late-%d", &idx); err == nil && l.Message == fmt.Sprintf("late-%d", idx) {
			producer = "late"
		} else if _, err := fmt.Sscanf(l.Message, `{"json":%d}`, &idx); err == nil && l.Message == fmt.Sprintf(`{"json":%d}`, idx) {
			producer = "json"
			if l.Kind != logs.LogKindJSON {
				t.Fatalf("json stream line has kind %q", l.Kind)
			}
		} else if strings.HasPrefix(l.Message, "[tool] Bash: ") {
			var name string
			rest := strings.TrimPrefix(l.Message, "[tool] Bash: ")
			if len(rest) < 3 || rest[1] != '-' {
				t.Fatalf("spliced line %q", l.Message)
			}
			name = rest[:1]
			if _, err := fmt.Sscanf(rest[2:], "%d", &idx); err != nil || rest != fmt.Sprintf("%s-%d", name, idx) {
				t.Fatalf("spliced line %q", l.Message)
			}
			producer = "agent-" + name
			if l.Kind != logs.LogKindTool {
				t.Fatalf("agent tool line has kind %q", l.Kind)
			}
		} else {
			t.Fatalf("spliced or unknown line %q", l.Message)
		}
		if idx != next[producer] {
			t.Fatalf("%s: got line %d, want %d (lost or reordered)", producer, idx, next[producer])
		}
		next[producer]++
	}
	for _, p := range []string{"base", "json", "agent-a", "agent-b"} {
		if next[p] != n {
			t.Errorf("%s: %d lines, want %d", p, next[p], n)
		}
	}
	if next["late"] != lateOK {
		t.Errorf("late: %d lines, want the %d writes that succeeded", next["late"], lateOK)
	}
	if folded[GroupFileUploads] != n+2*m {
		t.Errorf("upload lines folded: %d, want %d", folded[GroupFileUploads], n+2*m)
	}
	if folded[GroupAgentEvents] != 2*n {
		t.Errorf("agent event lines folded: %d, want %d", folded[GroupAgentEvents], 2*n)
	}
	if len(w.sources) != 0 {
		t.Errorf("sources still held after Close: %d", len(w.sources))
	}
}
