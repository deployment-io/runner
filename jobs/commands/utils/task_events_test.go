package utils

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// fakeEventsClient records every call; answer, when set, decides each call's
// error, and block holds every call until it is closed.
type fakeEventsClient struct {
	mu       sync.Mutex
	calls    []tasks.AppendTaskEventsArgsV1
	inFlight int
	maxInFl  int
	answer   func(call int) error
	block    chan struct{}
	called   chan struct{}
}

func newFakeEventsClient() *fakeEventsClient {
	return &fakeEventsClient{called: make(chan struct{}, 1000)}
}

func (c *fakeEventsClient) AppendTaskEvents(organizationID string, args tasks.AppendTaskEventsArgsV1) error {
	c.mu.Lock()
	c.calls = append(c.calls, args)
	n := len(c.calls)
	c.inFlight++
	if c.inFlight > c.maxInFl {
		c.maxInFl = c.inFlight
	}
	answer, block := c.answer, c.block
	c.mu.Unlock()
	c.called <- struct{}{}
	if block != nil {
		<-block
	}
	time.Sleep(time.Millisecond)
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	if answer != nil {
		return answer(n)
	}
	return nil
}

func (c *fakeEventsClient) snapshot() []tasks.AppendTaskEventsArgsV1 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tasks.AppendTaskEventsArgsV1(nil), c.calls...)
}

func (c *fakeEventsClient) waitCalls(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for len(c.snapshot()) < n {
		select {
		case <-c.called:
		case <-deadline:
			t.Fatalf("got %d call(s) within %s, want %d", len(c.snapshot()), within, n)
		}
	}
}

// syncLog is a job log tests can read while the sender writes to it.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testSender(t *testing.T, c TaskEventsClient, flushAfter time.Duration) (*TaskEventSender, *syncLog) {
	t.Helper()
	s := buildTaskEventSender("org-1", "task-1", "job-1", c)
	s.flushAfter = flushAfter
	s.retryDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond}
	log := &syncLog{}
	s.LogTo(log)
	go s.run()
	t.Cleanup(func() { s.Close(time.Second) })
	return s, log
}

func intermediate(i int) tasks.TaskEventV1 {
	return tasks.TaskEventV1{Type: tasks.TaskEventSelfCheck, Stage: tasks.TaskEventStageImplement, Now: fmt.Sprint(i)}
}

func verdict(i int) tasks.TaskEventV1 {
	return tasks.TaskEventV1{Type: tasks.TaskEventQARoundFinished, Stage: tasks.TaskEventStageQA, Now: fmt.Sprint(i)}
}

func TestTaskEventsDefaults(t *testing.T) {
	if taskEventsFlushAfter != 2*time.Second {
		t.Errorf("flush after = %s, want 2s", taskEventsFlushAfter)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if fmt.Sprint(taskEventsRetryDelays) != fmt.Sprint(want) {
		t.Errorf("retry delays = %v, want %v", taskEventsRetryDelays, want)
	}
	if taskEventsBatchAt != 50 || taskEventsQueueMax != 1000 || taskEventsVerdictQueueMax != 1500 || tasks.MaxTaskEventsPerBatch != 100 {
		t.Error("the batch and queue sizes changed")
	}
}

func TestTaskEventsBatchAtFiftyEvents(t *testing.T) {
	c := newFakeEventsClient()
	s, _ := testSender(t, c, time.Hour)
	for i := 0; i < 49; i++ {
		s.Emit(intermediate(i))
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(c.snapshot()); n != 0 {
		t.Fatalf("49 events were sent in %d batch(es) before the 50th", n)
	}
	s.Emit(intermediate(49))
	c.waitCalls(t, 1, time.Second)
	call := c.snapshot()[0]
	if len(call.Events) != 50 || call.TaskID != "task-1" || call.JobID != "job-1" || call.BatchID == "" {
		t.Errorf("batch = %d events, task %q, job %q, batch id %q", len(call.Events), call.TaskID, call.JobID, call.BatchID)
	}
	if call.Events[0].Ts == 0 {
		t.Error("Emit left Ts at 0")
	}
}

func TestTaskEventsBatchAfterTheOldestWaitedFlushAfter(t *testing.T) {
	c := newFakeEventsClient()
	s, _ := testSender(t, c, 150*time.Millisecond)
	start := time.Now()
	s.Emit(intermediate(1))
	s.Emit(intermediate(2))
	c.waitCalls(t, 1, time.Second)
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("the batch went after %s, before the oldest event waited 150ms", waited)
	}
	if n := len(c.snapshot()[0].Events); n != 2 {
		t.Errorf("batch has %d events, want 2", n)
	}
}

func TestTaskEventsAStageEventSendsAtOnce(t *testing.T) {
	for _, stageType := range []string{tasks.TaskEventStageStarted, tasks.TaskEventStageFinished} {
		c := newFakeEventsClient()
		s, _ := testSender(t, c, time.Hour)
		s.Emit(intermediate(1))
		s.Emit(tasks.TaskEventV1{Type: stageType, Stage: tasks.TaskEventStageReview, StageDetail: &tasks.TaskEventStageV1{}})
		c.waitCalls(t, 1, time.Second)
		if n := len(c.snapshot()[0].Events); n != 2 {
			t.Errorf("%s: batch has %d events, want both", stageType, n)
		}
	}
}

func TestTaskEventsBatchesHoldAtMostOneHundredInOrderOneAtATime(t *testing.T) {
	c := newFakeEventsClient()
	block := make(chan struct{})
	c.block = block
	s, _ := testSender(t, c, time.Hour)
	for i := 0; i < 250; i++ {
		s.Emit(intermediate(i))
	}
	c.waitCalls(t, 1, time.Second)
	time.Sleep(20 * time.Millisecond)
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("%d batches in flight at once, want 1", n)
	}
	close(block)
	s.Close(time.Second)
	next := 0
	for _, call := range c.snapshot() {
		if len(call.Events) > tasks.MaxTaskEventsPerBatch {
			t.Errorf("a batch carried %d events", len(call.Events))
		}
		for _, e := range call.Events {
			if e.Now != fmt.Sprint(next) {
				t.Fatalf("event %s sent where %d was due", e.Now, next)
			}
			next++
		}
	}
	if next != 250 {
		t.Errorf("sent %d events, want 250", next)
	}
	if c.maxInFl != 1 {
		t.Errorf("%d batches were in flight at once", c.maxInFl)
	}
}

func TestTaskEventsAFailedBatchIsRetriedUnderItsIDThenDropped(t *testing.T) {
	c := newFakeEventsClient()
	fail := true
	var mu sync.Mutex
	c.answer = func(int) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("connection reset\nmore detail")
		}
		return nil
	}
	s, log := testSender(t, c, time.Hour)
	s.Emit(tasks.TaskEventV1{Type: tasks.TaskEventStageStarted, Stage: tasks.TaskEventStageQA, StageDetail: &tasks.TaskEventStageV1{}})
	c.waitCalls(t, 5, 2*time.Second)
	time.Sleep(30 * time.Millisecond)
	calls := c.snapshot()
	if len(calls) != 5 {
		t.Fatalf("%d attempts, want 5", len(calls))
	}
	for _, call := range calls {
		if call.BatchID != calls[0].BatchID {
			t.Errorf("a retry carried batch id %q, want %q", call.BatchID, calls[0].BatchID)
		}
	}
	if want := "warning: could not send 1 progress events: connection reset\n"; !strings.Contains(log.String(), want) {
		t.Errorf("log = %q, want %q", log.String(), want)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	s.Emit(tasks.TaskEventV1{Type: tasks.TaskEventStageFinished, Stage: tasks.TaskEventStageQA, StageDetail: &tasks.TaskEventStageV1{}})
	c.waitCalls(t, 6, time.Second)
	if c.snapshot()[5].BatchID == calls[0].BatchID {
		t.Error("the next batch reused the dropped batch's id")
	}
}

func TestTaskEventsAServerWithoutTheMethodStopsSendingForTheJob(t *testing.T) {
	c := newFakeEventsClient()
	c.answer = func(int) error { return errors.New("rpc: can't find method Jobs.AppendTaskEventsV1") }
	s, log := testSender(t, c, time.Hour)
	stage := tasks.TaskEventV1{Type: tasks.TaskEventStageStarted, Stage: tasks.TaskEventStageQA, StageDetail: &tasks.TaskEventStageV1{}}
	s.Emit(stage)
	c.waitCalls(t, 1, time.Second)
	time.Sleep(20 * time.Millisecond)
	s.Emit(stage)
	s.Emit(stage)
	s.Close(time.Second)
	if n := len(c.snapshot()); n != 1 {
		t.Errorf("%d calls, want the one that found no method", n)
	}
	line := "Progress events aren't supported by this deployment.io server yet; the job goes on without them\n"
	if got := strings.Count(log.String(), line); got != 1 {
		t.Errorf("log = %q, want the line once", log.String())
	}
	if strings.Contains(log.String(), "could not send") {
		t.Errorf("an unsupported server was reported as a failed send: %q", log.String())
	}
}

func TestTaskEventsCloseSendsWhatIsQueued(t *testing.T) {
	c := newFakeEventsClient()
	s, log := testSender(t, c, time.Hour)
	for i := 0; i < 130; i++ {
		s.Emit(intermediate(i))
	}
	s.Close(time.Second)
	total := 0
	for _, call := range c.snapshot() {
		total += len(call.Events)
	}
	if total != 130 || len(c.snapshot()) != 2 {
		t.Errorf("close sent %d events in %d batches, want 130 in 2", total, len(c.snapshot()))
	}
	if log.String() != "" {
		t.Errorf("log = %q", log.String())
	}
	s.Emit(intermediate(999))
	time.Sleep(20 * time.Millisecond)
	if len(c.snapshot()) != 2 {
		t.Error("an event emitted after Close was sent")
	}
}

func TestTaskEventsCloseHonoursItsTimeout(t *testing.T) {
	c := newFakeEventsClient()
	c.block = make(chan struct{}) // never answers
	s, log := testSender(t, c, time.Hour)
	for i := 0; i < 120; i++ {
		s.Emit(intermediate(i))
	}
	c.waitCalls(t, 1, time.Second)
	start := time.Now()
	s.Close(100 * time.Millisecond)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %s with a 100ms timeout", took)
	}
	if want := "warning: could not send 120 progress events: the job ended before they were sent\n"; log.String() != want {
		t.Errorf("log = %q, want %q", log.String(), want)
	}
}

func TestTaskEventsAFullQueueDropsIntermediateEventsFirst(t *testing.T) {
	s := buildTaskEventSender("org-1", "task-1", "job-1", newFakeEventsClient())
	log := &syncLog{}
	s.LogTo(log)
	// Nothing drains the queue: the goroutine is not started.
	for i := 0; i < 1200; i++ {
		s.Emit(intermediate(i))
	}
	if len(s.queue) != 1000 || s.Dropped() != 200 {
		t.Fatalf("queue = %d, dropped = %d; want 1000 and 200", len(s.queue), s.Dropped())
	}
	for i := 0; i < 600; i++ {
		s.Emit(verdict(i))
	}
	if len(s.queue) != 1500 || s.Dropped() != 300 {
		t.Fatalf("queue = %d, dropped = %d; want verdicts queued up to 1500, then dropped", len(s.queue), s.Dropped())
	}
	s.Emit(intermediate(-1))
	if s.Dropped() != 301 {
		t.Error("an intermediate event was queued past 1000")
	}
	s.writeDropWarning()
	s.writeDropWarning()
	if want := "warning: progress events are being dropped: the queue is full\n"; log.String() != want {
		t.Errorf("log = %q, want the warning once", log.String())
	}
}

func TestTaskEventsEmitNeverBlocks(t *testing.T) {
	c := newFakeEventsClient()
	c.block = make(chan struct{}) // a server that never answers
	s, _ := testSender(t, c, time.Millisecond)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			s.Emit(intermediate(i))
			s.Emit(tasks.TaskEventV1{Type: tasks.TaskEventStageStarted, Stage: tasks.TaskEventStageQA, StageDetail: &tasks.TaskEventStageV1{}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked behind a server that never answers")
	}
	s.Close(50 * time.Millisecond)
}

func TestTaskEventsRegistry(t *testing.T) {
	c := newFakeEventsClient()
	original := TaskEventsRPC
	TaskEventsRPC = func() TaskEventsClient { return c }
	t.Cleanup(func() { TaskEventsRPC = original })
	ctx := TaskJobContext{OrganizationID: "org-1", TaskID: "task-1"}

	s := TaskEvents(ctx, "job-registry")
	if TaskEvents(ctx, "job-registry") != s {
		t.Fatal("a second call made a second sender for the same job")
	}
	s.Emit(intermediate(1))
	CloseTaskEvents("job-registry", time.Second)
	if len(c.snapshot()) != 1 {
		t.Errorf("CloseTaskEvents sent %d batches, want 1", len(c.snapshot()))
	}
	if TaskEvents(ctx, "job-registry") == s {
		t.Error("Close left the sender in the registry")
	}
	CloseTaskEvents("job-registry", time.Second)
	CloseTaskEvents("no-such-job", time.Second)

	noop := TaskEvents(TaskJobContext{}, "job-x")
	noop.Emit(intermediate(1))
	noop.Close(time.Second)
	TaskEvents(ctx, "").Emit(intermediate(1))
	if len(c.snapshot()) != 1 {
		t.Error("a sender without a Task or job id sent something")
	}
}

func TestTaskEventsCloseEndsTheSenderBehindACallThatNeverAnswers(t *testing.T) {
	c := newFakeEventsClient()
	c.block = make(chan struct{}) // never answers
	s, _ := testSender(t, c, time.Hour)
	s.Emit(verdict(0))
	s.Emit(tasks.TaskEventV1{Type: tasks.TaskEventStageFinished, Stage: tasks.TaskEventStageQA})
	c.waitCalls(t, 1, time.Second)
	s.Close(50 * time.Millisecond)
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("the sender's goroutine is still waiting on the call after Close")
	}
}

// stalledLog is a job log whose writes never finish.
type stalledLog struct{ release chan struct{} }

func (l stalledLog) Write(p []byte) (int, error) {
	<-l.release
	return len(p), nil
}

func TestTaskEventsCloseDoesNotWaitOnAStalledLog(t *testing.T) {
	defer func(wait time.Duration) { taskEventsCloseLogWait = wait }(taskEventsCloseLogWait)
	taskEventsCloseLogWait = 50 * time.Millisecond
	c := newFakeEventsClient()
	c.block = make(chan struct{}) // never answers
	s := buildTaskEventSender("org-1", "task-1", "job-1", c)
	s.flushAfter = time.Hour
	log := stalledLog{release: make(chan struct{})}
	defer close(log.release)
	s.LogTo(log)
	go s.run()
	s.Emit(tasks.TaskEventV1{Type: tasks.TaskEventStageStarted, Stage: tasks.TaskEventStageQA})
	c.waitCalls(t, 1, time.Second)
	start := time.Now()
	s.Close(50 * time.Millisecond)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %s behind a stalled job log", took)
	}
}
