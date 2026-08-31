package commands

import (
	"strings"
	"testing"
)

// TestFormatVerifyFailure pins that the error carries the CAUSE, not just the
// command that produced it.
//
// The regression this guards is the exact message a Step produced on
// 2026-08-29 — `agent self-verification failed: GOWORK=off go build ./... &&
// go vet ./... && go test ./...` — which named the ritual and nothing else.
// The reason turned out to be one pre-existing vet warning in a package the
// agent never opened, and finding that out required reproducing the failure
// by hand.
func TestFormatVerifyFailure(t *testing.T) {
	cases := []struct {
		name       string
		vr         verifyResult
		wantHas    []string
		wantNotHas []string
	}{
		{
			name: "stderr tail is carried",
			vr: verifyResult{
				Command:    "go test ./...",
				StderrTail: "pkg/auth/token.go:42:9: undefined: ParseJWT",
			},
			wantHas: []string{"go test ./...", "undefined: ParseJWT"},
		},
		{
			// Some tools report failures on stdout; the message must not go
			// blank just because stderr happened to be empty.
			name: "stdout is the fallback",
			vr: verifyResult{
				Command:    "npm test",
				StdoutTail: "1 failing\n  AssertionError: expected 200 to equal 404",
			},
			wantHas: []string{"npm test", "AssertionError"},
		},
		{
			name: "stderr wins when both are present",
			vr: verifyResult{
				Command:    "go test ./...",
				StdoutTail: "ok  	example/pkg	0.1s",
				StderrTail: "FAIL	example/pkg [build failed]",
			},
			wantHas:    []string{"build failed"},
			wantNotHas: []string{"0.1s"},
		},
		{
			// Every agentbox older than the release that started asking for a
			// tail reports none. That must degrade to the previous message,
			// not to a dangling separator.
			name:       "no tail degrades to the command alone",
			vr:         verifyResult{Command: "go build ./..."},
			wantHas:    []string{"go build ./..."},
			wantNotHas: []string{"—"},
		},
		{
			name:       "whitespace-only tail counts as none",
			vr:         verifyResult{Command: "make check", StderrTail: "  \n\t "},
			wantHas:    []string{"make check"},
			wantNotHas: []string{"—"},
		},
		{
			name:    "missing command still says something",
			vr:      verifyResult{StderrTail: "boom"},
			wantHas: []string{"(unspecified command)", "boom"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatVerifyFailure(&tc.vr).Error()
			if !strings.HasPrefix(got, "agent self-verification failed: ") {
				t.Errorf("prefix changed: %q", got)
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("message %q is missing %q", got, want)
				}
			}
			for _, notWant := range tc.wantNotHas {
				if strings.Contains(got, notWant) {
					t.Errorf("message %q must not contain %q", got, notWant)
				}
			}
		})
	}
}

// The error reaches the dashboard and feeds Re-run-with-feedback, so a
// runaway tail has to be bounded — and bounded from the END, because a
// compiler or test runner puts the failure last and the head is progress
// noise. Truncating the wrong end would keep the useless half.
func TestVerifyFailureTailIsBoundedKeepingTheEnd(t *testing.T) {
	noise := strings.Repeat("compiling package blah blah\n", 500)
	vr := verifyResult{Command: "go test ./...", StderrTail: noise + "FAIL: the actual reason"}

	tail := verifyFailureTail(&vr)
	if len(tail) > verifyTailMaxBytes+len("…") {
		t.Errorf("tail is %d bytes, want it bounded near %d", len(tail), verifyTailMaxBytes)
	}
	if !strings.Contains(tail, "FAIL: the actual reason") {
		t.Error("truncation dropped the end, which is where the failure is")
	}
	if !strings.HasPrefix(tail, "…") {
		t.Error("a truncated tail should say it was truncated")
	}
}
