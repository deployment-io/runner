package loggers

import (
	"io"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/logs"
	"github.com/deployment-io/deployment-runner/utils"
)

// maxLineBytes is the longest line the writer holds before it emits what it
// has as one line and carries on with the rest.
const maxLineBytes = 64 * 1024

// groupWindow is how long a group of folded lines stays open.
const groupWindow = 5 * time.Second

// sectionLines are the lines written when a Task Step job enters a stage.
// Setup gets none: it is where the log starts.
var sectionLines = map[string]string{
	logs.LogStageImplement: "--- Implement stage ---",
	logs.LogStageReview:    "--- Review stage ---",
	logs.LogStageQA:        "--- QA stage ---",
	logs.LogStagePR:        "--- Pull request ---",
}

// JobLogsWriter is a job's log writer. It splits lines inside Write, so each
// line is tagged with the stage that was current when it was written. For a
// Task Step job (tagged) it also sets Stage and Kind on every line, writes a
// section line on each stage change and folds noise into counted groups; for
// every other job lines go out exactly as written, untagged.
type JobLogsWriter struct {
	mu     sync.Mutex
	closed bool
	tagged bool
	stage  string
	// local log.Println's every line as it completes (LOCAL mode).
	local bool

	emit      func(JobLog)
	now       func() time.Time
	afterFunc func(time.Duration, func()) (stop func() bool)

	base *SourceWriter
	// sources are the other source writers holding a partial line, in the
	// order they started holding it.
	sources []*SourceWriter

	pending    *pendingGroup
	stopTimer  func() bool
	generation uint64
}

type pendingGroup struct {
	name  string
	stage string
	ts    int64
	last  string
	count int
}

// SourceWriter is one stream of lines into a JobLogsWriter, with its own
// partial-line buffer so two streams never splice into one line.
type SourceWriter struct {
	w    *JobLogsWriter
	buf  []byte
	kind string
	// agent is set for an agent container's output; agentType picks the
	// classifier's rules.
	agent     bool
	agentType string
}

func newJobLogsWriter(tagged bool, emit func(JobLog), now func() time.Time,
	afterFunc func(time.Duration, func()) func() bool) *JobLogsWriter {
	w := &JobLogsWriter{tagged: tagged, emit: emit, now: now, afterFunc: afterFunc}
	if tagged {
		w.stage = logs.LogStageSetup
	}
	w.base = &SourceWriter{w: w, kind: logs.LogKindText}
	return w
}

// NewJobLogsWriter returns a job's log writer that hands each line to emit.
// taskStep tags the lines of a Task Step job.
func NewJobLogsWriter(taskStep bool, emit func(JobLog)) *JobLogsWriter {
	return newJobLogsWriter(taskStep, emit, time.Now, realAfterFunc)
}

func realAfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Write splits p into lines on the job's own stream.
func (w *JobLogsWriter) Write(p []byte) (int, error) {
	return w.base.Write(p)
}

// Close emits every pending partial line and any pending group, and stops the
// group timer. A Write after Close returns io.ErrClosedPipe.
func (w *JobLogsWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.flushSourceLocked(w.base)
	for _, s := range w.sources {
		w.flushSourceLocked(s)
	}
	w.sources = nil
	w.flushGroupLocked()
	w.closed = true
	return nil
}

// SetStage makes stage the current stage of a Task Step job: it emits any
// pending group, then the new stage's section line. Setting the current stage
// again writes nothing. It does nothing on an untagged job.
func (w *JobLogsWriter) SetStage(stage string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.tagged || w.closed || stage == w.stage {
		return
	}
	w.flushGroupLocked()
	w.stage = stage
	if line, ok := sectionLines[stage]; ok {
		w.emit(JobLog{Message: line, Ts: w.now().Unix(), Stage: stage, Kind: logs.LogKindSection})
	}
}

// Stage returns the current stage ("" on an untagged job).
func (w *JobLogsWriter) Stage() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stage
}

func (w *JobLogsWriter) newSource(kind string, agent bool, agentType string) *SourceWriter {
	return &SourceWriter{w: w, kind: kind, agent: agent, agentType: agentType}
}

// Write splits p into lines, holding a trailing partial line until its end
// arrives, the source ends or the job's writer closes.
func (s *SourceWriter) Write(p []byte) (int, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	hadPartial := len(s.buf) > 0
	s.buf = append(s.buf, p...)
	for len(s.buf) > 0 {
		window := s.buf
		if len(window) > maxLineBytes {
			window = window[:maxLineBytes]
		}
		advance, token, _ := utils.ScanCRLF(window, false)
		if advance > 0 {
			w.lineLocked(s, string(token))
			s.buf = s.buf[advance:]
			continue
		}
		if len(s.buf) < maxLineBytes {
			break
		}
		cut := utf8Cut(s.buf, maxLineBytes)
		w.lineLocked(s, string(s.buf[:cut]))
		s.buf = s.buf[cut:]
	}
	if len(s.buf) == 0 {
		s.buf = nil
	}
	if s != w.base {
		if len(s.buf) > 0 && !hadPartial {
			w.sources = append(w.sources, s)
		} else if len(s.buf) == 0 && hadPartial {
			w.dropSourceLocked(s)
		}
	}
	return len(p), nil
}

// End marks the source's stream as ended: its partial line is emitted and the
// source is dropped from the job's writer.
func (s *SourceWriter) End() {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.flushSourceLocked(s)
	if s != w.base {
		w.dropSourceLocked(s)
	}
}

// utf8Cut returns where to cut b, at most n bytes in, on a rune boundary.
func utf8Cut(b []byte, n int) int {
	for cut := n; cut > 0 && cut > n-utf8.UTFMax; cut-- {
		if cut == len(b) {
			// b[:cut] is all of b: it ends on a boundary when its last
			// rune is complete.
			if utf8.FullRune(b[lastRuneStart(b):]) {
				return cut
			}
			continue
		}
		if utf8.RuneStart(b[cut]) {
			return cut
		}
	}
	return n
}

// lastRuneStart is the index where b's last rune starts.
func lastRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return len(b) - 1
}

func (w *JobLogsWriter) flushSourceLocked(s *SourceWriter) {
	if len(s.buf) > 0 {
		w.lineLocked(s, string(s.buf))
	}
	s.buf = nil
}

func (w *JobLogsWriter) dropSourceLocked(s *SourceWriter) {
	for i, o := range w.sources {
		if o == s {
			w.sources = append(w.sources[:i], w.sources[i+1:]...)
			return
		}
	}
}

// lineLocked handles one complete raw line from source s.
func (w *JobLogsWriter) lineLocked(s *SourceWriter, raw string) {
	line := strings.Trim(raw, " \n \r")
	if len(line) == 0 {
		return
	}
	if w.local {
		log.Println(line)
	}
	ts := w.now().Unix()
	if !w.tagged {
		w.emit(JobLog{Message: line, Ts: ts})
		return
	}
	kind, group := s.kind, ""
	if s.agent {
		kind, group = classifyAgentLine(s.agentType, line)
	} else if kind == logs.LogKindText {
		group = noiseGroup(line)
	}
	if group != "" {
		if w.pending != nil && w.pending.name == group {
			w.pending.count++
			w.pending.last = line
			return
		}
		w.flushGroupLocked()
		w.pending = &pendingGroup{name: group, stage: w.stage, ts: ts, last: line, count: 1}
		w.generation++
		gen := w.generation
		w.stopTimer = w.afterFunc(groupWindow, func() { w.groupTimerFired(gen) })
		return
	}
	w.flushGroupLocked()
	w.emit(JobLog{Message: line, Ts: ts, Stage: w.stage, Kind: kind})
}

func (w *JobLogsWriter) groupTimerFired(gen uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.pending == nil || gen != w.generation {
		return
	}
	w.flushGroupLocked()
}

// flushGroupLocked emits the pending group, if any: a group of one line as
// that plain line, a larger one as its last line with the count.
func (w *JobLogsWriter) flushGroupLocked() {
	if w.stopTimer != nil {
		w.stopTimer()
		w.stopTimer = nil
	}
	g := w.pending
	if g == nil {
		return
	}
	w.pending = nil
	l := JobLog{Message: g.last, Ts: g.ts, Stage: g.stage, Kind: logs.LogKindText}
	if g.count > 1 {
		l.Group = &logs.JobLogGroupV1{Name: g.name, Count: g.count}
	}
	w.emit(l)
}

// NewSource returns a source writer on w's job writer with the default kind,
// for a stream (a container's stdout or stderr) whose partial lines must not
// splice with another's. When w is not a tagged job writer it returns w
// unchanged, so an untagged job's streams are written as before.
func NewSource(w io.Writer) io.Writer {
	if jw, ok := w.(*JobLogsWriter); ok && jw.tagged {
		return jw.newSource(logs.LogKindText, false, "")
	}
	return w
}

// WithKind returns a source writer on w's job writer whose lines get kind.
// When w is not a tagged job writer it returns w unchanged.
func WithKind(w io.Writer, kind string) io.Writer {
	if jw, ok := w.(*JobLogsWriter); ok && jw.tagged {
		return jw.newSource(kind, false, "")
	}
	return w
}

// AgentOutput returns a source writer on w's job writer for one stream of an
// agent container's output; its lines are classified by agentType's formatter
// prefixes. When w is not a tagged job writer it returns w unchanged.
func AgentOutput(w io.Writer, agentType string) io.Writer {
	if jw, ok := w.(*JobLogsWriter); ok && jw.tagged {
		return jw.newSource(logs.LogKindText, true, agentType)
	}
	return w
}

// EndSource ends w's stream when w is a source writer: its partial line is
// emitted and it is dropped from the job's writer. Anything else is left as is.
func EndSource(w io.Writer) {
	if s, ok := w.(*SourceWriter); ok {
		s.End()
	}
}
