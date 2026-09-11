package commands

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/sessions"
)

func newTestPump(t *testing.T) (*inputPump, string) {
	t.Helper()
	base := t.TempDir()
	ip := &inputPump{
		dir:        filepath.Join(base, ".agentbox-input", "messages"),
		uploadsDir: filepath.Join(base, sessionUploadsDirRel),
		logsWriter: io.Discard,
		seen:       map[string]bool{},
	}
	for _, d := range []string{ip.dir, ip.uploadsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return ip, base
}

func TestInputPump_WritesAttachmentsBeforeTurn(t *testing.T) {
	ip, _ := newTestPump(t)
	m := sessions.UserMessageDtoV1{ID: "m1", Ts: 10, Content: "see /work/uploads/abc-1-report.pdf.txt",
		Attachments: []sessions.SessionAttachmentDtoV1{
			{Name: "report.pdf", Path: "abc-1-report.pdf.txt", Content: "[Page 1]\nfinding"},
			{Name: "notes.md", Path: "abc-2-notes.md.txt", Content: "todo"},
		}}
	if !ip.deliver(m) {
		t.Fatal("deliver failed")
	}
	for path, want := range map[string]string{"abc-1-report.pdf.txt": "[Page 1]\nfinding", "abc-2-notes.md.txt": "todo"} {
		b, err := os.ReadFile(filepath.Join(ip.uploadsDir, path))
		if err != nil || string(b) != want {
			t.Errorf("%s: got (%q, %v)", path, b, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(ip.dir, "0000000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil || rec["id"] != "m1" || rec["content"] != m.Content {
		t.Errorf("record = %s (err %v)", b, err)
	}
	if !ip.seen["m1"] || ip.afterTs != 10 || ip.seq != 1 {
		t.Errorf("state after deliver: seen=%v afterTs=%d seq=%d", ip.seen, ip.afterTs, ip.seq)
	}
	if entries, _ := os.ReadDir(ip.uploadsDir); len(entries) != 2 {
		t.Errorf("no temp files should remain: %v", entries)
	}
}

func TestInputPump_TurnWithoutAttachmentsUnchanged(t *testing.T) {
	ip, _ := newTestPump(t)
	if !ip.deliver(sessions.UserMessageDtoV1{ID: "m1", Ts: 5, Content: "hi"}) {
		t.Fatal("deliver failed")
	}
	b, err := os.ReadFile(filepath.Join(ip.dir, "0000000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"content":"hi","id":"m1","ts":5}` {
		t.Errorf("record = %s", b)
	}
	if entries, _ := os.ReadDir(ip.uploadsDir); len(entries) != 0 {
		t.Errorf("uploads dir must stay empty: %v", entries)
	}
}

func TestInputPump_PathTraversalIsAnchored(t *testing.T) {
	ip, base := newTestPump(t)
	m := sessions.UserMessageDtoV1{ID: "m1", Ts: 1, Content: "x", Attachments: []sessions.SessionAttachmentDtoV1{
		{Name: "evil", Path: "../../../escape.txt", Content: "nope"},
		{Name: "abs", Path: "/etc/passwd", Content: "nope"},
	}}
	if !ip.deliver(m) {
		t.Fatal("deliver failed")
	}
	if _, err := os.Stat(filepath.Join(base, "escape.txt")); err == nil {
		t.Error("traversal escaped the uploads dir")
	}
	for _, p := range []string{"escape.txt", "etc/passwd"} {
		if _, err := os.Stat(filepath.Join(ip.uploadsDir, p)); err != nil {
			t.Errorf("%s should have been anchored under uploads: %v", p, err)
		}
	}
}

func TestInputPump_FailedWriteIsNotMarkedDelivered(t *testing.T) {
	ip, _ := newTestPump(t)
	ip.uploadsDir = filepath.Join(ip.uploadsDir, "missing-and-unwritable")
	if err := os.WriteFile(ip.uploadsDir, []byte("a file, not a dir"), 0644); err != nil {
		t.Fatal(err)
	}
	m := sessions.UserMessageDtoV1{ID: "m1", Ts: 7, Content: "x",
		Attachments: []sessions.SessionAttachmentDtoV1{{Name: "a", Path: "a.txt", Content: "b"}}}
	if ip.deliver(m) {
		t.Fatal("deliver should fail when the attachment cannot be written")
	}
	if ip.seen["m1"] || ip.afterTs != 0 || ip.seq != 0 {
		t.Errorf("failed delivery must not advance state: seen=%v afterTs=%d seq=%d", ip.seen, ip.afterTs, ip.seq)
	}
	if entries, _ := os.ReadDir(ip.dir); len(entries) != 0 {
		t.Errorf("no record must be written for a failed turn: %v", entries)
	}
}

func TestInputPump_ReplyFromOlderServerHasNoAttachments(t *testing.T) {
	ip, _ := newTestPump(t)
	// gob on an older server leaves Attachments nil; that must be a plain turn.
	if !ip.deliver(sessions.UserMessageDtoV1{ID: "m1", Ts: 1, Content: "legacy", Attachments: nil}) {
		t.Fatal("deliver failed")
	}
}

func TestPlanModePromptMentionsUploadsAndUntrustedAttachments(t *testing.T) {
	for _, want := range []string{"/work/uploads", "untrusted", "one implementation-ready outcome", "[Page N]"} {
		if !strings.Contains(planModePrompt, want) {
			t.Errorf("planModePrompt missing %q", want)
		}
	}
}
