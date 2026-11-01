// Package redact hides known secret values in output written to a log.
package redact

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// maxPending bounds what a writer holds back waiting for a line
// end: past it, all but the last few bytes (that could still start a value) are
// written, so a long line or progress output without a line break doesn't grow the
// runner's memory.
const maxPending = 64 << 10

// minLineBytes is the shortest line of a multi-line value hidden on its own: a
// shorter one would mask common output.
const minLineBytes = 8

// writer writes to w line by line ("\n" or "\r" ending a line), each
// value of values replaced by "<NAME>" (its name), so a build that prints a wired
// variable or a build secret doesn't put its value in the log. Each line of a
// multi-line value (of at least minLineBytes) is hidden on its own too, since
// output reaches w a line at a time, possibly prefixed. Flush writes the last,
// unterminated line.
type writer struct {
	mu       sync.Mutex
	w        io.Writer
	patterns []pattern
	names    []string
	maxLen   int
	pending  []byte
}

// pattern is text to hide; rank is the order of the value it came from (longer
// first, then by name), its index in names: the lowest names a span.
type pattern struct {
	text []byte
	rank int
}

// NewWriter is w with values' values hidden; w itself when there are none.
func NewWriter(w io.Writer, values map[string]string) (io.Writer, func()) {
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
	r := &writer{w: w, names: names}
	seen := map[string]bool{}
	add := func(text string, rank int) {
		if seen[text] {
			return // A value earlier by the order already names it.
		}
		seen[text] = true
		r.patterns = append(r.patterns, pattern{text: []byte(text), rank: rank})
		r.maxLen = max(r.maxLen, len(text))
	}
	for rank, name := range names {
		value := values[name]
		add(value, rank)
		if !strings.ContainsAny(value, "\n\r") {
			continue
		}
		lines := strings.ReplaceAll(value, "\r\n", "\n")
		lines = strings.ReplaceAll(lines, "\r", "\n")
		for _, line := range strings.Split(lines, "\n") {
			if len(line) >= minLineBytes {
				add(line, rank)
			}
		}
	}
	// Longer patterns first, so a pattern that holds another is replaced whole.
	sort.SliceStable(r.patterns, func(i, j int) bool {
		return len(r.patterns[i].text) > len(r.patterns[j].text)
	})
	return r, r.flush
}

func (r *writer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, p...)
	if end := bytes.LastIndexAny(r.pending, "\n\r"); end >= 0 {
		if err := r.emit(end + 1); err != nil {
			return 0, err
		}
	}
	if len(r.pending) > maxPending {
		if err := r.emit(r.safeCut(len(r.pending) - (r.maxLen - 1))); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// safeCut is cut, moved back to the start of any pattern that would straddle it.
func (r *writer) safeCut(cut int) int {
	for moved := true; moved && cut > 0; {
		moved = false
		for _, p := range r.patterns {
			v := p.text
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
func (r *writer) emit(n int) error {
	if n <= 0 {
		return nil
	}
	chunk := string(r.pending[:n])
	r.pending = append(r.pending[:0], r.pending[n:]...)
	_, err := io.WriteString(r.w, r.redact(chunk))
	return err
}

func (r *writer) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) > 0 {
		io.WriteString(r.w, r.redact(string(r.pending)))
		r.pending = nil
	}
}

// redact is s with every match of every pattern hidden: overlapping or touching
// matches (of one pattern or several) are merged into one span, replaced by one
// "<NAME>", named after the earliest value by the order among its patterns, so no
// value's text is left visible between two matches. Matches are kept per start
// position (the furthest end and the earliest rank), not one by one, so the
// bookkeeping stays proportional to s however many matches overlap.
func (r *writer) redact(s string) string {
	var ends, ranks []int // ends[i] > 0: a match starts at i.
	for _, p := range r.patterns {
		text := string(p.text)
		for from := 0; ; {
			i := strings.Index(s[from:], text)
			if i < 0 {
				break
			}
			start := from + i
			if ends == nil {
				ends, ranks = make([]int, len(s)), make([]int, len(s))
			}
			if ends[start] == 0 {
				ends[start], ranks[start] = start+len(text), p.rank
			} else {
				ends[start] = max(ends[start], start+len(text))
				ranks[start] = min(ranks[start], p.rank)
			}
			from = start + 1
		}
	}
	if ends == nil {
		return s
	}
	var b strings.Builder
	at, spanStart, spanEnd, spanRank := 0, -1, 0, 0
	writeSpan := func() {
		b.WriteString(s[at:spanStart])
		b.WriteString("<" + r.names[spanRank] + ">")
		at = spanEnd
	}
	for i, end := range ends {
		if end == 0 {
			continue
		}
		if spanStart >= 0 && i <= spanEnd {
			spanEnd = max(spanEnd, end)
			spanRank = min(spanRank, ranks[i])
			continue
		}
		if spanStart >= 0 {
			writeSpan()
		}
		spanStart, spanEnd, spanRank = i, end, ranks[i]
	}
	writeSpan()
	b.WriteString(s[at:])
	return b.String()
}
