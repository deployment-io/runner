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
