package commands

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

func TestInputPump_TracksDeliveredIDsAtWatermark(t *testing.T) {
	ip, _ := newTestPump(t)
	msg := func(id string, ts int64) sessions.UserMessageDtoV1 {
		return sessions.UserMessageDtoV1{ID: id, Ts: ts, Content: id}
	}
	if !ip.deliver(msg("a", 10)) {
		t.Fatal("deliver a")
	}
	if ip.afterTs != 10 || !reflect.DeepEqual(ip.deliveredAtAfterTs, []string{"a"}) {
		t.Fatalf("after a: afterTs=%d delivered=%v", ip.afterTs, ip.deliveredAtAfterTs)
	}
	// a same-second sibling accumulates at the same watermark
	if !ip.deliver(msg("b", 10)) {
		t.Fatal("deliver b")
	}
	if ip.afterTs != 10 || !reflect.DeepEqual(ip.deliveredAtAfterTs, []string{"a", "b"}) {
		t.Fatalf("after b: afterTs=%d delivered=%v", ip.afterTs, ip.deliveredAtAfterTs)
	}
	// a later second moves the watermark and resets the list
	if !ip.deliver(msg("c", 11)) {
		t.Fatal("deliver c")
	}
	if ip.afterTs != 11 || !reflect.DeepEqual(ip.deliveredAtAfterTs, []string{"c"}) {
		t.Fatalf("after c: afterTs=%d delivered=%v", ip.afterTs, ip.deliveredAtAfterTs)
	}
	// a failed delivery changes nothing
	ip.uploadsDir = filepath.Join(ip.uploadsDir, "nope")
	_ = os.WriteFile(ip.uploadsDir, []byte("file"), 0644)
	bad := msg("d", 12)
	bad.Attachments = []sessions.SessionAttachmentDtoV1{{Name: "x", Path: "x.txt", Content: "y"}}
	if ip.deliver(bad) {
		t.Fatal("expected failure")
	}
	if ip.afterTs != 11 || !reflect.DeepEqual(ip.deliveredAtAfterTs, []string{"c"}) {
		t.Fatalf("failed delivery moved state: afterTs=%d delivered=%v", ip.afterTs, ip.deliveredAtAfterTs)
	}
}

func TestInputPump_BatchStopsAtFirstFailure(t *testing.T) {
	ip, _ := newTestPump(t)
	// M2's attachment path is a directory-as-file so its write fails.
	blocker := filepath.Join(ip.uploadsDir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocker, "child"), 0755); err != nil {
		t.Fatal(err)
	}
	batch := []sessions.UserMessageDtoV1{
		{ID: "m3", Ts: 3, Content: "three"},
		{ID: "m1", Ts: 1, Content: "one"},
		{ID: "m2", Ts: 2, Content: "two", Attachments: []sessions.SessionAttachmentDtoV1{{Name: "b", Path: "blocked", Content: "x"}}},
	}
	ip.deliverBatch(batch)
	if !ip.seen["m1"] || ip.seen["m2"] || ip.seen["m3"] {
		t.Errorf("seen = %v, want only m1", ip.seen)
	}
	if ip.afterTs != 1 || !reflect.DeepEqual(ip.deliveredAtAfterTs, []string{"m1"}) {
		t.Errorf("watermark advanced past the failed turn: afterTs=%d delivered=%v", ip.afterTs, ip.deliveredAtAfterTs)
	}
	if entries, _ := os.ReadDir(ip.dir); len(entries) != 1 {
		t.Errorf("only m1's record should exist: %v", entries)
	}
	// once the obstacle is gone the next poll picks up from m2, in order
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	ip.deliverBatch(batch)
	if !ip.seen["m2"] || !ip.seen["m3"] || ip.afterTs != 3 {
		t.Errorf("retry did not complete in order: seen=%v afterTs=%d", ip.seen, ip.afterTs)
	}
	names := []string{}
	entries, _ := os.ReadDir(ip.dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{"0000000001.json", "0000000002.json", "0000000003.json"}) {
		t.Errorf("records = %v", names)
	}
}
