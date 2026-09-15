package commands

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
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

// tinyPNG is a 1x1 PNG — enough for http.DetectContentType to classify the
// bytes, which is how the pump labels an image's media type.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInputPump_ImagesAreListedWithInContainerPaths(t *testing.T) {
	ip, _ := newTestPump(t)
	shot := tinyPNG(t)
	m := sessions.UserMessageDtoV1{ID: "m1", Ts: 10, Content: "what's wrong here?",
		Attachments: []sessions.SessionAttachmentDtoV1{
			{Name: "notes.md", Path: "abc-1-notes.md.txt", Content: "todo"},
			{Name: "shot.png", Path: "abc-2-shot.png", Bytes: shot, Width: 1280, Height: 800},
		}}
	if !ip.deliver(m) {
		t.Fatal("deliver failed")
	}
	// Both files are on disk, the image byte-for-byte.
	if b, err := os.ReadFile(filepath.Join(ip.uploadsDir, "abc-1-notes.md.txt")); err != nil || string(b) != "todo" {
		t.Errorf("text attachment: got (%q, %v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(ip.uploadsDir, "abc-2-shot.png")); err != nil || !bytes.Equal(b, shot) {
		t.Errorf("image attachment: got (%d bytes, %v)", len(b), err)
	}

	var rec map[string]any
	b, err := os.ReadFile(filepath.Join(ip.dir, "0000000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["id"] != "m1" || rec["content"] != m.Content {
		t.Errorf("record = %s", b)
	}
	want := []any{map[string]any{
		"path":      "/work/uploads/abc-2-shot.png",
		"mediaType": "image/png",
		"width":     float64(1280),
		"height":    float64(800),
	}}
	if !reflect.DeepEqual(rec["images"], want) {
		t.Errorf("images = %#v, want %#v", rec["images"], want)
	}
}

func TestInputPump_TurnWithoutImagesHasNoImagesKey(t *testing.T) {
	ip, _ := newTestPump(t)
	m := sessions.UserMessageDtoV1{ID: "m1", Ts: 5, Content: "see the report",
		Attachments: []sessions.SessionAttachmentDtoV1{{Name: "r.pdf", Path: "abc-1-r.pdf.txt", Content: "[Page 1]"}}}
	if !ip.deliver(m) {
		t.Fatal("deliver failed")
	}
	b, err := os.ReadFile(filepath.Join(ip.dir, "0000000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Byte-for-byte the record an older agentbox already consumes.
	if string(b) != `{"content":"see the report","id":"m1","ts":5}` {
		t.Errorf("record = %s", b)
	}
}

func TestInputPump_FailedImageWriteLeavesTurnUndelivered(t *testing.T) {
	ip, _ := newTestPump(t)
	// The image's path is a directory, so its write fails.
	if err := os.MkdirAll(filepath.Join(ip.uploadsDir, "abc-1-shot.png", "child"), 0755); err != nil {
		t.Fatal(err)
	}
	m := sessions.UserMessageDtoV1{ID: "m1", Ts: 7, Content: "look",
		Attachments: []sessions.SessionAttachmentDtoV1{{Name: "shot.png", Path: "abc-1-shot.png", Bytes: tinyPNG(t), Width: 4, Height: 3}}}
	if ip.deliver(m) {
		t.Fatal("deliver should fail when the image cannot be written")
	}
	if ip.seen["m1"] || ip.afterTs != 0 || ip.seq != 0 {
		t.Errorf("failed delivery must not advance state: seen=%v afterTs=%d seq=%d", ip.seen, ip.afterTs, ip.seq)
	}
	if entries, _ := os.ReadDir(ip.dir); len(entries) != 0 {
		t.Errorf("no record must be written for a failed turn: %v", entries)
	}
	// Once the obstacle is gone the retry delivers the whole turn.
	if err := os.RemoveAll(filepath.Join(ip.uploadsDir, "abc-1-shot.png")); err != nil {
		t.Fatal(err)
	}
	if !ip.deliver(m) {
		t.Fatal("retry failed")
	}
	b, err := os.ReadFile(filepath.Join(ip.dir, "0000000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"/work/uploads/abc-1-shot.png"`) {
		t.Errorf("record = %s", b)
	}
}

func TestImageRecords(t *testing.T) {
	png1 := tinyPNG(t)
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}
	cases := []struct {
		name string
		atts []sessions.SessionAttachmentDtoV1
		want []map[string]any
	}{
		{"no attachments", nil, nil},
		{"text only", []sessions.SessionAttachmentDtoV1{{Name: "a.txt", Path: "a.txt", Content: "x"}}, nil},
		{"a jpeg keeps its own media type", []sessions.SessionAttachmentDtoV1{{Name: "p.jpg", Path: "x-p.jpg", Bytes: jpg, Width: 2, Height: 1}},
			[]map[string]any{{"path": "/work/uploads/x-p.jpg", "mediaType": "image/jpeg", "width": 2, "height": 1}}},
		{"a gif re-encoded to png is labelled by its bytes, not its name",
			[]sessions.SessionAttachmentDtoV1{{Name: "anim.gif", Path: "x-anim.gif", Bytes: png1, Width: 1, Height: 1}},
			[]map[string]any{{"path": "/work/uploads/x-anim.gif", "mediaType": "image/png", "width": 1, "height": 1}}},
		{"a traversing path is anchored under uploads, like the file is",
			[]sessions.SessionAttachmentDtoV1{{Name: "e", Path: "../../etc/shot.png", Bytes: png1, Width: 1, Height: 1}},
			[]map[string]any{{"path": "/work/uploads/etc/shot.png", "mediaType": "image/png", "width": 1, "height": 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageRecords(tc.atts); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("imageRecords = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestPlanModePromptMentionsAttachedImages(t *testing.T) {
	want := "Images the user attaches are shown to you directly with the message and are also saved under /work/uploads if you need to look again."
	if !strings.Contains(planModePrompt, want) {
		t.Errorf("planModePrompt missing the image sentence")
	}
}
