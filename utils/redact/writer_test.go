package redact

import (
	"bytes"
	"strings"
	"testing"
)

// Lines are redacted; a value split across writes is still replaced whole.
func TestRedactingWriterLines(t *testing.T) {
	var out bytes.Buffer
	w, flush := NewWriter(&out, map[string]string{"API_URL": "https://d2.cloudfront.net/v1"})
	w.Write([]byte("calling https://d2.clo"))
	w.Write([]byte("udfront.net/v1\rprogress\nlast https://d2.cloudfront.net/v1"))
	flush()
	if got, want := out.String(), "calling <API_URL>\rprogress\nlast <API_URL>"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// Output with no line end doesn't pile up: past the bound it is written, and a
// value straddling the cut is held back whole.
func TestRedactingWriterBoundsPending(t *testing.T) {
	var out bytes.Buffer
	value := "https://d2.cloudfront.net/v1"
	w, flush := NewWriter(&out, map[string]string{"API_URL": value})
	r := w.(*writer)
	chunk := strings.Repeat(".", 1000)
	for i := 0; i < 200; i++ {
		w.Write([]byte(chunk))
		w.Write([]byte(value))
		if len(r.pending) > maxPending+len(chunk)+len(value) {
			t.Fatalf("pending grew to %d bytes", len(r.pending))
		}
	}
	if out.Len() == 0 {
		t.Fatal("nothing written before the flush")
	}
	flush()
	if strings.Contains(out.String(), "cloudfront") {
		t.Error("a value reached the output")
	}
	if n := strings.Count(out.String(), "<API_URL>"); n != 200 {
		t.Errorf("%d values replaced, want 200", n)
	}
}

const twoLines = "first-secret-line\nsecond-secret-line"

// Each line of a multi-line value is hidden on its own, written apart or prefixed
// by BuildKit, and the whole value printed at once leaves nothing of it.
func TestRedactingWriterMultiLineValue(t *testing.T) {
	for _, test := range []struct {
		name   string
		writes []string
		want   string
	}{
		{"two writes", []string{"key: first-secret-line\n", "second-secret-line\n"}, "key: <KEY>\n<KEY>\n"},
		{"prefixed", []string{"#18 52.91 first-secret-line\n#18 52.91 second-secret-line\n"}, "#18 52.91 <KEY>\n#18 52.91 <KEY>\n"},
		{"one write", []string{"key: " + twoLines + " done\n"}, "key: <KEY> done\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			w, flush := NewWriter(&out, map[string]string{"KEY": twoLines})
			for _, s := range test.writes {
				w.Write([]byte(s))
			}
			flush()
			if out.String() != test.want {
				t.Errorf("out = %q, want %q", out.String(), test.want)
			}
		})
	}
}

// A value's CRLF line ends don't stop its lines from being hidden in output with
// "\n" line ends.
func TestRedactingWriterCRLFValue(t *testing.T) {
	var out bytes.Buffer
	w, flush := NewWriter(&out, map[string]string{"KEY": "line-one-value\r\nline-two-value"})
	w.Write([]byte("#3 line-one-value\n#3 line-two-value\n"))
	flush()
	if got, want := out.String(), "#3 <KEY>\n#3 <KEY>\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// A line shorter than minLineBytes isn't hidden on its own (the accepted limit);
// the long lines are.
func TestRedactingWriterShortLine(t *testing.T) {
	var out bytes.Buffer
	w, flush := NewWriter(&out, map[string]string{"KEY": "long-first-line\nshort\nlong-third-line"})
	w.Write([]byte("long-first-line\nshort\nlong-third-line\n"[:16]))
	w.Write([]byte("short\nlong-third-line\n"))
	flush()
	if got, want := out.String(), "<KEY>\nshort\n<KEY>\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// A line two values share is hidden once, named after the value first by the
// order (longer first); neither value's text remains.
func TestRedactingWriterSharedLine(t *testing.T) {
	var out bytes.Buffer
	values := map[string]string{
		"LONG":  "shared-line-text\nalpha-only-line",
		"SHORT": "shared-line-text\nbeta-line",
	}
	w, flush := NewWriter(&out, values)
	w.Write([]byte("a shared-line-text\nb alpha-only-line\nc beta-line\n"))
	flush()
	if got, want := out.String(), "a <LONG>\nb <LONG>\nc <SHORT>\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// Overlapping matches of two values are hidden as one span, so neither leaves a
// part visible.
func TestRedactingWriterOverlappingMatches(t *testing.T) {
	var out bytes.Buffer
	values := map[string]string{
		"A": "3456789ABC",
		"B": "XX345678\nanother-line",
	}
	w, flush := NewWriter(&out, values)
	w.Write([]byte("#1 XX3456789ABC end\n#2 3456789ABCXX345678\n"))
	flush()
	if got, want := out.String(), "#1 <B> end\n#2 <B>\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	for _, s := range []string{"345678", "9ABC", "XX"} {
		if strings.Contains(out.String(), s) {
			t.Errorf("out %q contains %q", out.String(), s)
		}
	}
}

// The maxPending cut doesn't split a line pattern of a multi-line value: each
// write crosses the bound with a line of the value straddling the cut point.
func TestRedactingWriterBoundsPendingLinePattern(t *testing.T) {
	var out bytes.Buffer
	value := twoLines + "\n" + strings.Repeat("x", 100)
	w, flush := NewWriter(&out, map[string]string{"KEY": value})
	// The cut keeps the last len(value)-1 bytes; the line starts before them.
	tail := strings.Repeat(".", len(value)-1-10)
	write := strings.Repeat(".", maxPending) + "second-secret-line" + tail
	for i := 0; i < 20; i++ {
		w.Write([]byte(write))
	}
	flush()
	if strings.Contains(out.String(), "secret") {
		t.Error("a line of the value reached the output")
	}
	if n := strings.Count(out.String(), "<KEY>"); n != 20 {
		t.Errorf("%d lines replaced, want 20", n)
	}
}

// A value of many nested lines matched by a long run produces millions of
// overlapping matches; they collapse into one span without holding each one.
func TestRedactingWriterManyOverlappingMatches(t *testing.T) {
	var lines []string
	for n := minLineBytes; n <= 360; n++ {
		lines = append(lines, strings.Repeat("x", n))
	}
	var out bytes.Buffer
	w, flush := NewWriter(&out, map[string]string{"KEY": strings.Join(lines, "\n")})
	if _, err := w.Write([]byte(strings.Repeat("x", 32<<10) + "\n")); err != nil {
		t.Fatal(err)
	}
	flush()
	if got := out.String(); got != "<KEY>\n" {
		t.Fatalf("got %q, want %q", got[:min(len(got), 80)], "<KEY>\n")
	}
}
