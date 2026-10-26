package commands

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
)

// TestCopyExecOutputStripsMultiplexHeaders checks that a non-TTY exec stream
// reaches the log as plain text: no 8-byte frame headers, both streams kept.
func TestCopyExecOutputStripsMultiplexHeaders(t *testing.T) {
	var stream bytes.Buffer
	writeStdFrame(&stream, stdcopy.Stdout, "➤ YN0000: ┌ Resolution step\n")
	writeStdFrame(&stream, stdcopy.Stderr, "warning: peer dependency missing\n")
	writeStdFrame(&stream, stdcopy.Stdout, "➤ YN0013: fetched 42 packages\n")

	var logs bytes.Buffer
	if err := copyExecOutput(&logs, &stream); err != nil {
		t.Fatalf("copyExecOutput: %v", err)
	}
	want := "➤ YN0000: ┌ Resolution step\nwarning: peer dependency missing\n➤ YN0013: fetched 42 packages\n"
	if logs.String() != want {
		t.Errorf("logs = %q\nwant %q", logs.String(), want)
	}
}

// writeStdFrame appends payload to buf as one frame of Docker's multiplexed
// stream: an 8-byte header (stream byte, three zero bytes, big-endian uint32
// payload length) followed by the payload. The v29 stdcopy package is
// reader-only, so the test writes the frames itself.
func writeStdFrame(buf *bytes.Buffer, stream stdcopy.StdType, payload string) {
	header := [8]byte{0: byte(stream)}
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	buf.Write(header[:])
	buf.WriteString(payload)
}
