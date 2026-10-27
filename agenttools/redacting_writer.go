package agenttools

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// redactingMaxPending bounds what a redactingWriter holds back waiting for a line
// end: past it, all but the last few bytes (that could still start a value) are
// written, so a long line or progress output without a line break doesn't grow the
// runner's memory.
const redactingMaxPending = 64 << 10

// redactingWriter writes to w line by line ("\n" or "\r" ending a line), each
// value of values replaced by "<NAME>" (its name), so a build that prints a wired
// variable doesn't put its value in the Step log. Flush writes the last,
// unterminated line.
type redactingWriter struct {
	mu       sync.Mutex
	w        io.Writer
	replacer *strings.Replacer
	values   [][]byte
	maxLen   int
	pending  []byte
}

// newRedactingWriter is w with values' values hidden; w itself when there are none.
func newRedactingWriter(w io.Writer, values map[string]string) (io.Writer, func()) {
	names := make([]string, 0, len(values))
	for name, value := range values {
		if value != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return w, func() {}
	}
	// Longer values first, so a value that holds another is replaced whole.
	sort.Slice(names, func(i, j int) bool {
		if len(values[names[i]]) != len(values[names[j]]) {
			return len(values[names[i]]) > len(values[names[j]])
		}
		return names[i] < names[j]
	})
	r := &redactingWriter{w: w}
	pairs := make([]string, 0, 2*len(names))
	for _, name := range names {
		pairs = append(pairs, values[name], "<"+name+">")
		r.values = append(r.values, []byte(values[name]))
		if len(values[name]) > r.maxLen {
			r.maxLen = len(values[name])
		}
	}
	r.replacer = strings.NewReplacer(pairs...)
	return r, r.flush
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, p...)
	if end := bytes.LastIndexAny(r.pending, "\n\r"); end >= 0 {
		if err := r.emit(end + 1); err != nil {
			return 0, err
		}
	}
	if len(r.pending) > redactingMaxPending {
		if err := r.emit(r.safeCut(len(r.pending) - (r.maxLen - 1))); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// safeCut is cut, moved back to the start of any value that would straddle it.
func (r *redactingWriter) safeCut(cut int) int {
	for moved := true; moved && cut > 0; {
		moved = false
		for _, v := range r.values {
			for s := max(0, cut-len(v)+1); s < cut; s++ {
				if bytes.HasPrefix(r.pending[s:], v) {
					cut, moved = s, true
					break
				}
			}
		}
	}
	return cut
}

// emit writes pending's first n bytes, redacted, and keeps the rest.
func (r *redactingWriter) emit(n int) error {
	if n <= 0 {
		return nil
	}
	chunk := string(r.pending[:n])
	r.pending = append(r.pending[:0], r.pending[n:]...)
	_, err := io.WriteString(r.w, r.replacer.Replace(chunk))
	return err
}

func (r *redactingWriter) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) > 0 {
		io.WriteString(r.w, r.replacer.Replace(string(r.pending)))
		r.pending = nil
	}
}
