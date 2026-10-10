package commands

import (
	"bytes"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"
)

// TestCopyExecOutputStripsMultiplexHeaders checks that a non-TTY exec stream
// reaches the log as plain text: no 8-byte frame headers, both streams kept.
func TestCopyExecOutputStripsMultiplexHeaders(t *testing.T) {
	var stream bytes.Buffer
	stdout := stdcopy.NewStdWriter(&stream, stdcopy.Stdout)
	stderr := stdcopy.NewStdWriter(&stream, stdcopy.Stderr)
	_, _ = stdout.Write([]byte("➤ YN0000: ┌ Resolution step\n"))
	_, _ = stderr.Write([]byte("warning: peer dependency missing\n"))
	_, _ = stdout.Write([]byte("➤ YN0013: fetched 42 packages\n"))

	var logs bytes.Buffer
	if err := copyExecOutput(&logs, &stream); err != nil {
		t.Fatalf("copyExecOutput: %v", err)
	}
	want := "➤ YN0000: ┌ Resolution step\nwarning: peer dependency missing\n➤ YN0013: fetched 42 packages\n"
	if logs.String() != want {
		t.Errorf("logs = %q\nwant %q", logs.String(), want)
	}
}
