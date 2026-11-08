package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
)

// A Step Job's structured progress events (tasks.TaskEventV1) go to
// deployment-server through ONE sender per Job: TaskEvents returns it, every
// event builder calls Emit on it, and MarkStepDone closes it before the Job
// reports done. Nothing else in the Job talks to Jobs.AppendTaskEventsV1.
//
// The sender is built so that the Job never notices it. Emit never blocks and
// never fails; a full queue, a batch the server keeps refusing and a server
// too old to have the method all lose events, never time, and each says so in
// the job log once. The job's own log lines and stage reports are untouched.

const (
	// taskEventsQueueMax is how many events wait to be sent before an
	// intermediate one is dropped; a verdict is still queued up to
	// taskEventsVerdictQueueMax, so the outcomes outlive the chatter.
	taskEventsQueueMax        = 1000
	taskEventsVerdictQueueMax = 1500
	// taskEventsBatchAt is the queue length that sends a batch right away.
	taskEventsBatchAt = 50
)

// Swapped by tests so the sender runs without real time passing or a server.
var (
	// taskEventsFlushAfter is how long the oldest queued event waits at most.
	taskEventsFlushAfter = 2 * time.Second
	// taskEventsRetryDelays are the waits before each retry of a failed batch:
	// five attempts in all.
	taskEventsRetryDelays = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	// taskEventsCloseLogWait is how long Close waits for its warning lines to
	// reach the job log.
	taskEventsCloseLogWait = 1 * time.Second
)

// TaskEventsRPC is the client a new sender sends through: the runner client.
// A var so tests can stand in for deployment-server.
var TaskEventsRPC = func() TaskEventsClient { return client.Get() }

// TaskEventsClient is the part of the runner client the sender uses.
type TaskEventsClient interface {
	AppendTaskEvents(organizationID string, args tasks.AppendTaskEventsArgsV1) error
}

// The sender's own log lines — the only lines it writes.
const (
	taskEventsQueueFullWarning = "warning: progress events are being dropped: the queue is full\n"
	taskEventsUnsupportedLine  = "Progress events aren't supported by this deployment.io server yet; the job goes on without them\n"
	taskEventsNotSentWarning   = "warning: could not send %d progress events: %s\n"
	taskEventsJobEndedReason   = "the job ended before they were sent"
)

type queuedTaskEvent struct {
	event    tasks.TaskEventV1
	queuedAt time.Time
}

// TaskEventSender queues one Job's events and sends them in batches, in queue
// order, one batch at a time, from its own goroutine.
type TaskEventSender struct {
	organizationID string
	taskID         string
	jobID          string
	client         TaskEventsClient
	flushAfter     time.Duration
	retryDelays    []time.Duration
	// noop is the sender of a Job that is not a Task Step (no job or Task id):
	// it accepts everything and sends nothing.
	noop bool

	mu    sync.Mutex
	queue []queuedTaskEvent
	// urgent is set when a stage event is queued: the queue goes at once.
	urgent bool
	// inFlight is how many events the batch being sent holds.
	inFlight int
	closing  bool
	// disabled: the server has no events method, or Close timed out. Nothing
	// more is queued or sent.
	disabled      bool
	dropped       int
	dropWarnDue   bool
	dropWarned    bool
	log           io.Writer
	unsupportedOK bool

	wake      chan struct{}
	abort     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var taskEventSenders = struct {
	sync.Mutex
	byJobID map[string]*TaskEventSender
}{byJobID: map[string]*TaskEventSender{}}

// TaskEvents returns the Job's sender, creating it on first use. A Job without
// a job or Task id gets a sender that sends nothing.
func TaskEvents(ctx TaskJobContext, jobID string) *TaskEventSender {
	if jobID == "" || ctx.TaskID == "" {
		return &TaskEventSender{noop: true}
	}
	taskEventSenders.Lock()
	defer taskEventSenders.Unlock()
	if s, ok := taskEventSenders.byJobID[jobID]; ok {
		return s
	}
	s := newTaskEventSender(ctx.OrganizationID, ctx.TaskID, jobID, TaskEventsRPC())
	taskEventSenders.byJobID[jobID] = s
	return s
}

// CloseTaskEvents closes the Job's sender, if it has one; see Close.
func CloseTaskEvents(jobID string, timeout time.Duration) {
	taskEventSenders.Lock()
	s := taskEventSenders.byJobID[jobID]
	taskEventSenders.Unlock()
	if s != nil {
		s.Close(timeout)
	}
}

func newTaskEventSender(organizationID, taskID, jobID string, c TaskEventsClient) *TaskEventSender {
	s := buildTaskEventSender(organizationID, taskID, jobID, c)
	go s.run()
	return s
}

// buildTaskEventSender is a sender whose goroutine is not started yet.
func buildTaskEventSender(organizationID, taskID, jobID string, c TaskEventsClient) *TaskEventSender {
	return &TaskEventSender{
		organizationID: organizationID,
		taskID:         taskID,
		jobID:          jobID,
		client:         c,
		flushAfter:     taskEventsFlushAfter,
		retryDelays:    append([]time.Duration(nil), taskEventsRetryDelays...),
		wake:           make(chan struct{}, 1),
		abort:          make(chan struct{}),
		done:           make(chan struct{}),
	}
}

// LogTo sets the job log the sender's warnings go to and returns the sender.
func (s *TaskEventSender) LogTo(w io.Writer) *TaskEventSender {
	if s == nil || s.noop || w == nil {
		return s
	}
	s.mu.Lock()
	s.log = w
	s.mu.Unlock()
	return s
}

// Emit queues e. It never blocks and never fails: Ts is set when 0, and a
// full queue drops the event (counted, and said once in the job log).
func (s *TaskEventSender) Emit(e tasks.TaskEventV1) {
	if s == nil || s.noop {
		return
	}
	if e.Ts == 0 {
		e.Ts = time.Now().UnixMilli()
	}
	s.mu.Lock()
	if s.closing || s.disabled {
		s.mu.Unlock()
		return
	}
	limit := taskEventsQueueMax
	if e.IsVerdict() {
		limit = taskEventsVerdictQueueMax
	}
	if len(s.queue) >= limit {
		s.dropped++
		if !s.dropWarned {
			s.dropWarnDue = true
		}
		s.mu.Unlock()
		s.notify()
		return
	}
	s.queue = append(s.queue, queuedTaskEvent{event: e, queuedAt: time.Now()})
	if e.Type == tasks.TaskEventStageStarted || e.Type == tasks.TaskEventStageFinished {
		s.urgent = true
	}
	s.mu.Unlock()
	s.notify()
}

// Dropped is how many events the queue had no room for.
func (s *TaskEventSender) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close stops accepting events, sends what is queued, and returns when the
// queue is empty, sending is disabled, or timeout passes — whatever is left
// then is dropped, with the warning line. The Job's registry entry goes.
func (s *TaskEventSender) Close(timeout time.Duration) {
	if s == nil || s.noop {
		return
	}
	s.closeOnce.Do(func() {
		taskEventSenders.Lock()
		if taskEventSenders.byJobID[s.jobID] == s {
			delete(taskEventSenders.byJobID, s.jobID)
		}
		taskEventSenders.Unlock()

		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.notify()

		timer := time.NewTimer(timeout)
		defer timer.Stop()
		var lines []string
		select {
		case <-s.done:
		case <-timer.C:
			s.mu.Lock()
			left := len(s.queue) + s.inFlight
			s.queue, s.inFlight, s.disabled = nil, 0, true
			s.mu.Unlock()
			close(s.abort)
			if left > 0 {
				lines = append(lines, fmt.Sprintf(taskEventsNotSentWarning, left, taskEventsJobEndedReason))
			}
		}
		if s.takeDropWarning() {
			lines = append(lines, taskEventsQueueFullWarning)
		}
		// The job log can stall behind its pipeline: Close never waits on it
		// longer than taskEventsCloseLogWait.
		s.writeLogWithin(strings.Join(lines, ""), taskEventsCloseLogWait)
	})
}

func (s *TaskEventSender) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *TaskEventSender) run() {
	defer close(s.done)
	for {
		batch, ok := s.nextBatch()
		if !ok {
			return
		}
		s.send(batch)
	}
}

// nextBatch waits until a batch is due and takes it off the queue: at
// taskEventsBatchAt events, flushAfter after the oldest, at once after a stage
// event or once the sender is closing. At most tasks.MaxTaskEventsPerBatch. ok
// is false when there is nothing more to send.
func (s *TaskEventSender) nextBatch() ([]tasks.TaskEventV1, bool) {
	for {
		s.writeDropWarning()
		s.mu.Lock()
		if s.disabled {
			s.mu.Unlock()
			return nil, false
		}
		n := len(s.queue)
		var wait time.Duration
		if n > 0 {
			wait = s.flushAfter - time.Since(s.queue[0].queuedAt)
		}
		if n > 0 && (n >= taskEventsBatchAt || s.urgent || s.closing || wait <= 0) {
			take := n
			if take > tasks.MaxTaskEventsPerBatch {
				take = tasks.MaxTaskEventsPerBatch
			}
			batch := make([]tasks.TaskEventV1, take)
			for i := range batch {
				batch[i] = s.queue[i].event
			}
			s.queue = append([]queuedTaskEvent(nil), s.queue[take:]...)
			if len(s.queue) == 0 {
				s.urgent = false
			}
			s.inFlight = take
			s.mu.Unlock()
			return batch, true
		}
		if s.closing {
			s.mu.Unlock()
			return nil, false
		}
		s.mu.Unlock()
		var timer *time.Timer
		var fire <-chan time.Time
		if n > 0 {
			timer = time.NewTimer(wait)
			fire = timer.C
		}
		select {
		case <-s.wake:
		case <-fire:
		case <-s.abort:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// send delivers one batch under one BatchID, retrying it after each of
// retryDelays; after the last attempt it is dropped with a warning. A server
// without the method disables the sender for the rest of the Job.
func (s *TaskEventSender) send(batch []tasks.TaskEventV1) {
	args := tasks.AppendTaskEventsArgsV1{
		TaskID:  s.taskID,
		JobID:   s.jobID,
		BatchID: newTaskEventsBatchID(),
		Events:  batch,
	}
	var err error
	for attempt := 0; attempt <= len(s.retryDelays); attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(s.retryDelays[attempt-1])
			select {
			case <-timer.C:
			case <-s.abort:
				timer.Stop()
				return
			}
		}
		var aborted bool
		aborted, err = s.call(args)
		if aborted {
			return
		}
		if err == nil {
			s.finishBatch()
			return
		}
		if isTaskEventsMethodMissing(err) {
			s.disableUnsupported()
			return
		}
	}
	if s.finishBatch() {
		s.writeLog(fmt.Sprintf(taskEventsNotSentWarning, len(batch), firstLine(err.Error())))
	}
}

// call sends one attempt of a batch. It runs on its own goroutine so that when
// Close gives up (abort) the sender ends at once; the call itself gives up
// within the client's deadline (client.AppendTaskEvents), and its goroutine
// and batch with it.
func (s *TaskEventSender) call(args tasks.AppendTaskEventsArgsV1) (aborted bool, err error) {
	result := make(chan error, 1)
	c, organizationID := s.client, s.organizationID
	go func() { result <- c.AppendTaskEvents(organizationID, args) }()
	select {
	case err = <-result:
		return false, err
	case <-s.abort:
		return true, nil
	}
}

// finishBatch clears the in-flight count; false when Close already gave up on
// the batch (and said so).
func (s *TaskEventSender) finishBatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight == 0 {
		return false
	}
	s.inFlight = 0
	return true
}

func (s *TaskEventSender) disableUnsupported() {
	s.mu.Lock()
	s.disabled = true
	s.queue, s.inFlight = nil, 0
	first := !s.unsupportedOK
	s.unsupportedOK = true
	s.mu.Unlock()
	if first {
		s.writeLog(taskEventsUnsupportedLine)
	}
}

func (s *TaskEventSender) writeDropWarning() {
	if s.takeDropWarning() {
		s.writeLog(taskEventsQueueFullWarning)
	}
}

// takeDropWarning reports whether the queue-full warning is due, and marks it
// written.
func (s *TaskEventSender) takeDropWarning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	due := s.dropWarnDue && !s.dropWarned
	if due {
		s.dropWarned, s.dropWarnDue = true, false
	}
	return due
}

func (s *TaskEventSender) writeLog(line string) {
	s.mu.Lock()
	w := s.log
	s.mu.Unlock()
	if w != nil {
		io.WriteString(w, line)
	}
}

// writeLogWithin writes text to the job log, waiting at most wait for the
// write to finish; a write still running then goes on without the caller.
func (s *TaskEventSender) writeLogWithin(text string, wait time.Duration) {
	if text == "" {
		return
	}
	done := make(chan struct{})
	go func() {
		s.writeLog(text)
		close(done)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// isTaskEventsMethodMissing reports a deployment-server older than the events
// RPC: net/rpc answers "rpc: can't find method Jobs.AppendTaskEventsV1".
func isTaskEventsMethodMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "can't find method")
}

func newTaskEventsBatchID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
