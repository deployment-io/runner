package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

var screenshotJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'J', 'F', 'I', 'F'}

// shotResult is result.json with the given verdict and, unless "", the
// screenshot object (raw JSON) and a screenshot_error.
func shotResult(verdict, screenshot string) []byte {
	r := strings.Replace(string(passResult()), `"verdict":"pass"`, `"verdict":"`+verdict+`"`, 1)
	if screenshot != "" {
		r = strings.Replace(r, `"dom_text_snippet":"Hello"`, `"dom_text_snippet":"Hello","screenshot":`+screenshot+`,"screenshot_error":"none","check_id":"forged"`, 1)
	}
	return []byte(r)
}

func newScreenshotTool(t *testing.T, fb *fakeBrowser) (*verifyPreviewRendered, *QAScreenshots, *strings.Builder) {
	t.Helper()
	store, err := NewQAScreenshots(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	logs := &strings.Builder{}
	tool.logsWriter = logs
	tool.screenshots = store
	return tool, store, logs
}

func replyFields(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &fields); err != nil {
		t.Fatalf("reply %q: %v", out, err)
	}
	return fields
}

// With a store every check asks for a screenshot and every pass or fail
// result carries check_id c<n>, n the call's number; the screenshot is kept
// under it at the result's size. The reply never carries screenshot or
// screenshot_error, nor a check_id the image wrote.
func TestVerifyRenderedScreenshots_CheckIDsAndStore(t *testing.T) {
	fb := &fakeBrowser{}
	tool, store, logs := newScreenshotTool(t, fb)
	for i, verdict := range []string{"pass", "fail"} {
		fb.outcome = VerifyBrowserOutcome{Result: shotResult(verdict, `{"width":1280,"height":720}`), Screenshot: screenshotJPEG}
		out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("c%d", i+1)
		fields := replyFields(t, out)
		if string(fields["check_id"]) != `"`+want+`"` {
			t.Errorf("%s: check_id = %s, want %s", verdict, fields["check_id"], want)
		}
		for _, key := range []string{"screenshot", "screenshot_error"} {
			if _, ok := fields[key]; ok {
				t.Errorf("%s: the reply carries %s", verdict, key)
			}
		}
		if !strings.HasPrefix(out, `{"schema_version":1,"check_id":"`+want+`"`) {
			t.Errorf("check_id is not near the top: %.60s", out)
		}
		image, w, h, err := store.Get(want)
		if err != nil || string(image) != string(screenshotJPEG) || w != 1280 || h != 720 {
			t.Errorf("%s: stored %q %d×%d, %v", want, image, w, h, err)
		}
		var in verifyBrowserInput
		if err := json.Unmarshal(fb.requests[i].Input, &in); err != nil || !in.Screenshot || !fb.requests[i].Screenshot {
			t.Errorf("input.json %s: want screenshot true", fb.requests[i].Input)
		}
	}
	if strings.Contains(logs.String(), "dropped") {
		t.Errorf("logs = %q", logs.String())
	}
	info, err := os.Stat(filepath.Join(store.Dir(), "c1.jpg"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("c1.jpg: %v, %v", info, err)
	}
	if info, err := os.Stat(store.Dir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("store dir: %v, %v", info, err)
	}
}

// A check without a screenshot (an older image) still gets its check_id; the
// runner drops it at upload.
func TestVerifyRenderedScreenshots_CheckIDWithoutScreenshot(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, store, logs := newScreenshotTool(t, fb)
	out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(replyFields(t, out)["check_id"]) != `"c1"` || store.Has("c1") || logs.String() != strings.Split(logs.String(), "dropped")[0] {
		t.Errorf("reply %s, stored %v, logs %q", out, store.Has("c1"), logs.String())
	}
}

// A check that errors, or answers preparing, has no check_id, and its call
// still counts: the next result is c3.
func TestVerifyRenderedScreenshots_NoCheckIDOnErrorsOrPreparing(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Preparing: true}}
	tool, _, _ := newScreenshotTool(t, fb)
	out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err != nil || strings.Contains(out, "check_id") {
		t.Errorf("preparing: %s, %v", out, err)
	}
	fb.outcome, fb.err = VerifyBrowserOutcome{}, errors.New("busy")
	if out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err == nil || strings.Contains(out+err.Error(), "check_id") {
		t.Errorf("error: %s, %v", out, err)
	}
	fb.outcome, fb.err = VerifyBrowserOutcome{Result: []byte(`{"schema_version":1,"status":"error","error":"boom"}`), Screenshot: screenshotJPEG}, nil
	if out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err == nil || strings.Contains(out+err.Error(), "check_id") {
		t.Errorf("status error: %s, %v", out, err)
	}
	fb.outcome = VerifyBrowserOutcome{Result: passResult()}
	out, err = call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err != nil || string(replyFields(t, out)["check_id"]) != `"c4"` {
		t.Errorf("fourth call: %s, %v; want c4", out, err)
	}
}

// Without a store nothing changes: no screenshot asked for, no check_id.
func TestVerifyRenderedScreenshots_NoneWithoutAStore(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: shotResult("pass", `{"width":1280,"height":720}`), Screenshot: screenshotJPEG}}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	fields := replyFields(t, out)
	for _, key := range []string{"check_id", "screenshot", "screenshot_error"} {
		if _, ok := fields[key]; ok {
			t.Errorf("the reply carries %s", key)
		}
	}
	if strings.Contains(string(fb.requests[0].Input), "screenshot") || fb.requests[0].Screenshot {
		t.Errorf("input.json = %s", fb.requests[0].Input)
	}
}

// A screenshot without the result's size, or with a size out of range, is
// dropped with a line in the Step log; the check's reply is unchanged.
func TestVerifyRenderedScreenshots_BadSizeDropped(t *testing.T) {
	for _, screenshot := range []string{"", `null`, `{"width":0,"height":720}`, `{"width":1280,"height":10001}`, `{"width":"wide","height":720}`} {
		fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: shotResult("pass", screenshot), Screenshot: screenshotJPEG}}
		tool, store, logs := newScreenshotTool(t, fb)
		out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
		if err != nil || string(replyFields(t, out)["check_id"]) != `"c1"` {
			t.Errorf("%s: %s, %v", screenshot, out, err)
		}
		if store.Has("c1") || !strings.Contains(logs.String(), "QA: dropped a check's screenshot: ") {
			t.Errorf("%s: stored %v, logs %q", screenshot, store.Has("c1"), logs.String())
		}
	}
}

// Concurrent checks each get their own number and screenshot.
func TestVerifyRenderedScreenshots_ConcurrentChecks(t *testing.T) {
	store, err := NewQAScreenshots(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, req VerifyBrowserRequest, _ io.Writer) (VerifyBrowserOutcome, error) {
		return VerifyBrowserOutcome{Result: shotResult("pass", `{"width":412,"height":839}`), Screenshot: screenshotJPEG}, nil
	}
	tool := &verifyPreviewRendered{record: renderedRecord(renderedPreview), run: run, screenshots: store}
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := tool.handle(context.Background(), json.RawMessage(`{"url":"`+renderedPreview+`"}`))
			if err != nil {
				t.Error(err)
				return
			}
			var r struct {
				CheckID string `json:"check_id"`
			}
			_ = json.Unmarshal([]byte(out), &r)
			ids <- r.CheckID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] || !store.Has(id) {
			t.Errorf("check id %q repeated or not stored", id)
		}
		seen[id] = true
	}
	if len(seen) != 20 {
		t.Errorf("%d distinct ids, want 20", len(seen))
	}
}

// The registrations: with a store the check asks for a screenshot; the plain
// one without a store (implement and fix runs) and the read-only one
// (production QA) don't.
func TestVerifyRenderedScreenshots_Registrations(t *testing.T) {
	store, err := NewQAScreenshots(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		register func(*agentmcp.Server, RunVerifyBrowser)
		want     bool
	}{
		{"plain without a store", func(s *agentmcp.Server, run RunVerifyBrowser) {
			RegisterVerifyPreviewRendered(s, renderedRecord(renderedPreview), run, io.Discard, nil)
		}, false},
		{"plain with a store", func(s *agentmcp.Server, run RunVerifyBrowser) {
			RegisterVerifyPreviewRendered(s, renderedRecord(renderedPreview), run, io.Discard, store)
		}, true},
		{"sign-in with a store", func(s *agentmcp.Server, run RunVerifyBrowser) {
			RegisterVerifyPreviewRenderedSignIn(s, renderedRecord(renderedPreview), run, io.Discard, nil, nil, store)
		}, true},
		{"read-only", func(s *agentmcp.Server, run RunVerifyBrowser) {
			RegisterVerifyPreviewRenderedReadOnly(s, renderedRecord(renderedPreview), run, io.Discard, nil)
		}, false},
	} {
		var input string
		run := func(_ context.Context, req VerifyBrowserRequest, _ io.Writer) (VerifyBrowserOutcome, error) {
			input = string(req.Input)
			return VerifyBrowserOutcome{}, errors.New("stop")
		}
		srv := agentmcp.New("test", "0")
		tc.register(srv, run)
		tool, _ := srv.Lookup("verify_preview_rendered")
		_, _ = tool.Handler(context.Background(), json.RawMessage(`{"url":"`+renderedPreview+`"}`))
		if got := strings.Contains(input, `"screenshot":true`); got != tc.want || (!tc.want && strings.Contains(input, "screenshot")) {
			t.Errorf("%s: input.json = %s", tc.name, input)
		}
	}
}

func TestQAScreenshots_Put(t *testing.T) {
	store, err := NewQAScreenshots(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.put("../x", screenshotJPEG, 1, 1); err == nil {
		t.Error("an invalid check id was stored")
	}
	if err := store.put("c1", make([]byte, QAScreenshotMaxBytes+1), 1, 1); err == nil {
		t.Error("an oversized screenshot was stored")
	}
	if _, _, _, err := store.Get("c1"); err == nil {
		t.Error("Get found an unstored check")
	}
	var nilStore *QAScreenshots
	if nilStore.Has("c1") {
		t.Error("a nil store has c1")
	}
}
