package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const renderedPreview = "https://d111.cloudfront.net/"

// fakeBrowser records the requests it gets and answers with outcome/err.
type fakeBrowser struct {
	requests []VerifyBrowserRequest
	outcome  VerifyBrowserOutcome
	err      error
}

func (f *fakeBrowser) run(_ context.Context, req VerifyBrowserRequest, _ io.Writer) (VerifyBrowserOutcome, error) {
	f.requests = append(f.requests, req)
	return f.outcome, f.err
}

func (f *fakeBrowser) input(t *testing.T) verifyBrowserInput {
	t.Helper()
	if len(f.requests) != 1 {
		t.Fatalf("browser ran %d times, want 1", len(f.requests))
	}
	var in verifyBrowserInput
	if err := json.Unmarshal(f.requests[0].Input, &in); err != nil {
		t.Fatalf("input.json: %v", err)
	}
	return in
}

func passResult() []byte {
	return []byte(`{"schema_version":1,"status":"completed","verdict":"pass","http_status":200,
		"final_url":"https://d111.cloudfront.net/","steps":[],"assertions":[{"index":0,"type":"text_visible","passed":true,"detail":"visible"}],
		"page_errors":[],"console_errors":[],"failed_requests":[],"blocked_navigations":[],
		"timings_ms":{"dom_content_loaded":120,"load":300},"dom_text_snippet":"Hello"}`)
}

func newRenderedTool(record *PreviewRecord, fb *fakeBrowser) (*verifyPreviewRendered, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	return &verifyPreviewRendered{record: record, run: fb.run, logsWriter: logs}, logs
}

func renderedRecord(urls ...string) *PreviewRecord {
	r := NewPreviewRecord(nil, nil)
	for _, u := range urls {
		r.AddURL(u)
	}
	return r
}

func call(t *testing.T, tool *verifyPreviewRendered, args string) (string, error) {
	t.Helper()
	return tool.handle(context.Background(), json.RawMessage(args))
}

func TestVerifyRendered_AllowedURLRunsWithRecordHosts(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, logs := newRenderedTool(renderedRecord(renderedPreview, "https://d222.cloudfront.net/"), fb)
	out, err := call(t, tool, `{"url":"https://D111.cloudfront.net/about","steps":[{"action":"goto","url":"https://d222.cloudfront.net/x"},{"action":"fill","selector":"#q","value":""}],"assertions":[{"type":"url_path","path":"/x"}]}`)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	in := fb.input(t)
	if strings.Join(in.AllowedHosts, ",") != "d111.cloudfront.net,d222.cloudfront.net" {
		t.Errorf("allowed_hosts = %v", in.AllowedHosts)
	}
	if in.SchemaVersion != 1 || in.TimeoutMS != 45000 || fb.requests[0].Timeout != 45*time.Second {
		t.Errorf("schema_version %d, timeout_ms %d, timeout %s", in.SchemaVersion, in.TimeoutMS, fb.requests[0].Timeout)
	}
	if len(in.Steps) != 2 || in.Steps[1].Value == nil || *in.Steps[1].Value != "" {
		t.Errorf("steps = %+v; an empty fill value must be sent", in.Steps)
	}
	var res renderedResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Message != "pass — 1 of 1 assertions passed; no page errors; no failed requests" {
		t.Errorf("message = %q", res.Message)
	}
	if !strings.Contains(logs.String(), "checking https://D111.cloudfront.net/about") || !strings.Contains(logs.String(), "verify_preview_rendered: pass after") {
		t.Errorf("logs = %q", logs.String())
	}
}

func TestVerifyRendered_SendsEveryRecordedHost(t *testing.T) {
	urls := []string{renderedPreview}
	for i := 0; i < 150; i++ {
		urls = append(urls, fmt.Sprintf("https://p%03d.example.net/", i))
	}
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, _ := newRenderedTool(renderedRecord(urls...), fb)
	if _, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err != nil {
		t.Fatal(err)
	}
	if got := len(fb.input(t).AllowedHosts); got != len(urls) {
		t.Fatalf("allowed_hosts has %d hosts, want all %d in the record", got, len(urls))
	}
}

func TestVerifyRendered_RefusesURLNotInRecord(t *testing.T) {
	for _, u := range []string{
		"https://d999.cloudfront.net/",
		"http://d111.cloudfront.net/",
		"https://d111.cloudfront.net:8443/",
		"https://user@d111.cloudfront.net/",
		"https://169.254.169.254/latest/meta-data/",
	} {
		fb := &fakeBrowser{}
		tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
		_, err := call(t, tool, fmt.Sprintf(`{"url":%q}`, u))
		if err == nil || !strings.Contains(err.Error(), "must be a preview URL this Task deployed") {
			t.Errorf("%s: err = %v", u, err)
		}
		if len(fb.requests) != 0 {
			t.Errorf("%s: browser ran", u)
		}
	}
}

func TestVerifyRendered_RefusesGotoNotInRecord(t *testing.T) {
	fb := &fakeBrowser{}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	_, err := call(t, tool, `{"url":"https://d111.cloudfront.net/","steps":[{"action":"goto","url":"https://www.iana.org/"}]}`)
	if err == nil || !strings.Contains(err.Error(), "steps[0]: goto url must be a preview URL this Task deployed") {
		t.Fatalf("err = %v", err)
	}
	if len(fb.requests) != 0 {
		t.Fatal("browser ran")
	}
}

func TestVerifyRendered_ListsTaskPreviewsOncePerCall(t *testing.T) {
	lists := 0
	record := NewPreviewRecord(func() ([]string, error) {
		lists++
		return []string{"https://d333.cloudfront.net/"}, nil
	}, nil)
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, _ := newRenderedTool(record, fb)
	_, err := call(t, tool, `{"url":"https://d333.cloudfront.net/","steps":[{"action":"goto","url":"https://d444.cloudfront.net/"}]}`)
	if err == nil {
		t.Fatal("goto to an unlisted host was accepted")
	}
	if lists != 1 {
		t.Fatalf("listed %d times, want 1", lists)
	}
	if _, err := call(t, tool, `{"url":"https://d333.cloudfront.net/"}`); err != nil {
		t.Fatalf("listed preview refused: %v", err)
	}
	if hosts := fb.input(t).AllowedHosts; len(hosts) != 1 || hosts[0] != "d333.cloudfront.net" {
		t.Fatalf("allowed_hosts = %v", hosts)
	}
}

func TestVerifyRendered_InputLimits(t *testing.T) {
	long := strings.Repeat("a", verifyRenderedMaxStringChars+1)
	steps := func(n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(`{"action":"click","selector":"a"},`, n), ",") + "]"
	}
	assertions := func(n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(`{"type":"text_visible","text":"a"},`, n), ",") + "]"
	}
	cases := map[string]string{
		"no url":                  `{}`,
		"timeout too long":        `{"url":"` + renderedPreview + `","timeout_seconds":91}`,
		"timeout too short":       `{"url":"` + renderedPreview + `","timeout_seconds":4}`,
		"too many steps":          `{"url":"` + renderedPreview + `","steps":` + steps(31) + `}`,
		"too many assertions":     `{"url":"` + renderedPreview + `","assertions":` + assertions(31) + `}`,
		"unknown action":          `{"url":"` + renderedPreview + `","steps":[{"action":"hover","selector":"a"}]}`,
		"click no selector":       `{"url":"` + renderedPreview + `","steps":[{"action":"click"}]}`,
		"fill no value":           `{"url":"` + renderedPreview + `","steps":[{"action":"fill","selector":"a"}]}`,
		"press no key":            `{"url":"` + renderedPreview + `","steps":[{"action":"press","selector":"a"}]}`,
		"wait_for both":           `{"url":"` + renderedPreview + `","steps":[{"action":"wait_for","selector":"a","text":"b"}]}`,
		"wait_for neither":        `{"url":"` + renderedPreview + `","steps":[{"action":"wait_for"}]}`,
		"unknown assertion":       `{"url":"` + renderedPreview + `","assertions":[{"type":"title","text":"a"}]}`,
		"relative url_path":       `{"url":"` + renderedPreview + `","assertions":[{"type":"url_path","path":"x"}]}`,
		"long selector":           `{"url":"` + renderedPreview + `","steps":[{"action":"click","selector":"` + long + `"}]}`,
		"long assertion text":     `{"url":"` + renderedPreview + `","assertions":[{"type":"text_absent","text":"` + long + `"}]}`,
		"long unused step value":  `{"url":"` + renderedPreview + `","steps":[{"action":"click","selector":"a","value":"` + long + `"}]}`,
		"long unused assert path": `{"url":"` + renderedPreview + `","assertions":[{"type":"text_visible","text":"a","path":"` + long + `"}]}`,
		"arguments not an object": `[]`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			lists := 0
			record := NewPreviewRecord(func() ([]string, error) { lists++; return nil, nil }, nil)
			record.AddURL(renderedPreview)
			fb := &fakeBrowser{}
			tool, _ := newRenderedTool(record, fb)
			if _, err := call(t, tool, args); err == nil {
				t.Fatal("accepted")
			}
			if len(fb.requests) != 0 || lists != 0 {
				t.Fatal("started something before rejecting the input")
			}
		})
	}
	// The limits themselves are accepted.
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	if _, err := call(t, tool, `{"url":"`+renderedPreview+`","timeout_seconds":90,"steps":`+steps(30)+`,"assertions":`+assertions(30)+`}`); err != nil {
		t.Fatalf("limits refused: %v", err)
	}
	if in := fb.input(t); in.TimeoutMS != 90000 || len(in.Steps) != 30 || len(in.Assertions) != 30 {
		t.Fatalf("input = %+v", in)
	}
}

func TestVerifyRendered_Preparing(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Preparing: true}}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	out, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "preparing" || got["message"] != verifyRenderedPreparingMessage {
		t.Fatalf("result = %s", out)
	}
	if tool.calls != 1 {
		t.Fatalf("calls = %d, want the preparing call counted", tool.calls)
	}
}

func TestVerifyRendered_CallLimit(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	// Every call counts: one the browser couldn't run, one with invalid input,
	// one "preparing", then checks that ran.
	fb.err = fmt.Errorf("%w: busy", ErrVerifyBrowserNotRun)
	if _, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v", err)
	}
	fb.err = nil
	if _, err := call(t, tool, `{"url":"https://elsewhere.example.com/"}`); err == nil {
		t.Fatal("url outside the record accepted")
	}
	fb.outcome = VerifyBrowserOutcome{Preparing: true}
	if _, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err != nil {
		t.Fatal(err)
	}
	fb.outcome = VerifyBrowserOutcome{Result: passResult()}
	for i := 3; i < verifyRenderedMaxCalls; i++ {
		if _, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	_, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err == nil || !strings.Contains(err.Error(), "report what you have found") {
		t.Fatalf("call %d: err = %v", verifyRenderedMaxCalls+1, err)
	}
	if len(fb.requests) != verifyRenderedMaxCalls-1 {
		t.Fatalf("browser ran %d times", len(fb.requests))
	}
}

func TestVerifyRendered_ErrorsCarryOutputTail(t *testing.T) {
	output := strings.Repeat("x", 3000) + "chromium: fatal error"
	cases := map[string]struct {
		outcome VerifyBrowserOutcome
		want    string
		tail    bool
	}{
		"image status error": {
			outcome: VerifyBrowserOutcome{Result: []byte(`{"schema_version":1,"status":"error","error":"the page could not be loaded: net::ERR_NAME_NOT_RESOLVED"}`), Output: output},
			want:    "ERR_NAME_NOT_RESOLVED",
		},
		"missing result": {outcome: VerifyBrowserOutcome{ExitCode: 1, Output: output}, want: "without a result", tail: true},
		"invalid result": {outcome: VerifyBrowserOutcome{Result: []byte("{"), Output: output}, want: "not valid JSON", tail: true},
		"wrong schema":   {outcome: VerifyBrowserOutcome{Result: []byte(`{"schema_version":2,"status":"completed","verdict":"pass"}`), Output: output}, want: "schema_version 2", tail: true},
		"timeout":        {outcome: VerifyBrowserOutcome{TimedOut: true, Output: output}, want: "did not finish in time", tail: true},
		"oom":            {outcome: VerifyBrowserOutcome{OOMKilled: true, ExitCode: 137, Output: output}, want: "ran out of memory", tail: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fb := &fakeBrowser{outcome: c.outcome}
			tool, logs := newRenderedTool(renderedRecord(renderedPreview), fb)
			_, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
			if err == nil {
				t.Fatal("no error")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "QA couldn't check the page: ") || !strings.Contains(msg, c.want) {
				t.Errorf("err = %q", firstLine(msg))
			}
			if c.tail {
				if !strings.Contains(msg, "chromium: fatal error") {
					t.Error("error lacks the output tail")
				}
				if strings.Count(msg, "x") > verifyRenderedOutputTailBytes {
					t.Error("error carries more than the output's tail")
				}
			}
			if !strings.Contains(logs.String(), "chromium: fatal error") {
				t.Error("container output not logged on failure")
			}
		})
	}
}

func TestVerifyRendered_NoOutputLoggedOnSuccess(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult(), Output: "chromium noise"}}
	tool, logs := newRenderedTool(renderedRecord(renderedPreview), fb)
	if _, err := call(t, tool, `{"url":"`+renderedPreview+`"}`); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "chromium noise") {
		t.Fatal("container output logged on success")
	}
}

func bigResult() []byte {
	many := func(n, chars int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%d%s", i, strings.Repeat("e", chars))
		}
		return out
	}
	var steps []renderedStepResult
	for i := 0; i < 40; i++ {
		steps = append(steps, renderedStepResult{Index: i, Action: "click", OK: i < 39, Error: strings.Repeat("s", 900)})
	}
	var assertions []renderedAssertionResult
	for i := 0; i < 40; i++ {
		assertions = append(assertions, renderedAssertionResult{Index: i, Type: "text_visible", Passed: false, Detail: strings.Repeat("d", 900)})
	}
	var failed []renderedFailedRequest
	for i := 0; i < 30; i++ {
		failed = append(failed, renderedFailedRequest{URL: "https://d111.cloudfront.net/" + strings.Repeat("u", 3000), Method: "GET", Status: 500})
	}
	b, _ := json.Marshal(map[string]any{
		"schema_version": 1, "status": "completed", "verdict": "fail", "http_status": 200,
		"final_url": "https://d111.cloudfront.net/" + strings.Repeat("f", 5000),
		"steps":     steps, "assertions": assertions,
		"page_errors": many(30, 2000), "console_errors": many(40, 2000), "failed_requests": failed,
		"blocked_navigations": many(30, 3000), "timings_ms": map[string]int{"load": 1},
		"dom_text_snippet": strings.Repeat("t", 5000),
	})
	return b
}

func TestRenderVerifyBrowserOutcome_Caps(t *testing.T) {
	r := renderedResult{}
	if err := json.Unmarshal(bigResult(), &r); err != nil {
		t.Fatal(err)
	}
	capRenderedResult(&r)
	maxChars := func(list []string) int {
		m := 0
		for _, s := range list {
			if n := utf8.RuneCountInString(s); n > m {
				m = n
			}
		}
		return m
	}
	checks := []struct {
		name      string
		got, want int
	}{
		{"steps", len(r.Steps), 30},
		{"assertions", len(r.Assertions), 30},
		{"page_errors", len(r.PageErrors), 10},
		{"page_error chars", maxChars(r.PageErrors), 500},
		{"console_errors", len(r.ConsoleErrors), 20},
		{"console_error chars", maxChars(r.ConsoleErrors), 300},
		{"failed_requests", len(r.FailedRequests), 20},
		{"blocked_navigations", len(r.BlockedNavigations), 10},
		{"step error chars", utf8.RuneCountInString(r.Steps[0].Error), 300},
		{"assertion detail chars", utf8.RuneCountInString(r.Assertions[0].Detail), 300},
		{"dom_text_snippet chars", utf8.RuneCountInString(r.DOMTextSnippet), 1000},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if r.Message != "fail — all 30 steps succeeded; 0 of 30 assertions passed; 10 page errors; 20 failed requests; 10 blocked navigations" {
		t.Errorf("message = %q", r.Message)
	}
}

func TestRenderVerifyBrowserOutcome_SizeCap(t *testing.T) {
	text, verdict, err := renderVerifyBrowserOutcome(VerifyBrowserOutcome{Result: bigResult()}, false)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != "fail" {
		t.Errorf("verdict = %q", verdict)
	}
	if len(text) > verifyRenderedMaxResultBytes {
		t.Fatalf("result is %d bytes, cap %d", len(text), verifyRenderedMaxResultBytes)
	}
	var r renderedResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || !strings.HasSuffix(r.Message, "(result shortened to fit)") {
		t.Errorf("truncated = %v, message = %q", r.Truncated, r.Message)
	}
	if len(r.Steps) != 30 || len(r.Assertions) != 30 {
		t.Errorf("steps/assertions dropped: %d/%d", len(r.Steps), len(r.Assertions))
	}
}

func TestRenderVerifyBrowserOutcome_Summary(t *testing.T) {
	res := `{"schema_version":1,"status":"completed","verdict":"fail","http_status":404,
		"steps":[{"index":0,"action":"click","ok":true},{"index":1,"action":"wait_for","ok":false,"error":"Timeout"}],
		"page_errors":["TypeError: x"],"failed_requests":[{"url":"https://d111.cloudfront.net/a.json","method":"GET","status":404}],
		"blocked_navigations":["https://evil.example/"]}`
	text, _, err := renderVerifyBrowserOutcome(VerifyBrowserOutcome{Result: []byte(res)}, false)
	if err != nil {
		t.Fatal(err)
	}
	var r renderedResult
	_ = json.Unmarshal([]byte(text), &r)
	want := "fail — the first load returned HTTP 404; step 1 (wait_for) failed; 1 page error; 1 failed request; 1 blocked navigation"
	if r.Message != want {
		t.Errorf("message = %q\nwant      %q", r.Message, want)
	}
	if !strings.Contains(text, `"assertions":[]`) {
		t.Errorf("missing assertions not rendered as []: %s", text)
	}
}

func TestVerifyRendered_RunErrorIsReported(t *testing.T) {
	fb := &fakeBrowser{err: errors.New("creating the QA browser container: no space left on device")}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	_, err := call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err == nil || !strings.Contains(err.Error(), "QA couldn't check the page: creating the QA browser container") {
		t.Fatalf("err = %v", err)
	}
	if tool.calls != 1 {
		t.Fatalf("calls = %d; a run that failed after starting counts", tool.calls)
	}
}

func TestPreviewRecord_Hosts(t *testing.T) {
	r := renderedRecord("https://B.example.net/x", "https://a.example.net/", "https://b.example.net/y")
	if got := strings.Join(r.Hosts(), ","); got != "a.example.net,b.example.net" {
		t.Fatalf("Hosts() = %s", got)
	}
	var nilRecord *PreviewRecord
	if nilRecord.Hosts() != nil {
		t.Fatal("nil record has hosts")
	}
}

// withOutline adds an outline of n entries, each with text and href of the given rune
// counts, to a result.
func withOutline(t *testing.T, result []byte, n, textChars, hrefChars int) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatal(err)
	}
	outline := make([]map[string]any, n)
	for i := range outline {
		outline[i] = map[string]any{"kind": "link", "text": strings.Repeat("x", textChars),
			"href": "https://d111.cloudfront.net/" + strings.Repeat("h", hrefChars)}
	}
	outline[0] = map[string]any{"kind": "heading", "text": strings.Repeat("x", textChars), "level": 1}
	m["outline"] = outline
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func renderOutcome(t *testing.T, result []byte) (string, renderedResult) {
	t.Helper()
	text, _, err := renderVerifyBrowserOutcome(VerifyBrowserOutcome{Result: result}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > verifyRenderedMaxResultBytes {
		t.Fatalf("result is %d bytes, cap %d", len(text), verifyRenderedMaxResultBytes)
	}
	var r renderedResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatal(err)
	}
	return text, r
}

func TestRenderVerifyBrowserOutcome_OutlinePassesThroughCapped(t *testing.T) {
	// Short entries, so the result fits; entry 1 is long and gets cut.
	res := withOutline(t, passResult(), 61, 10, 10)
	res = bytes.Replace(res, []byte(`"href":"https://d111.cloudfront.net/hhhhhhhhhh","kind":"link","text":"xxxxxxxxxx"`),
		[]byte(`"href":"https://d111.cloudfront.net/`+strings.Repeat("h", 1000)+`","kind":"link","text":"`+strings.Repeat("x", 200)+`"`), 1)
	_, r := renderOutcome(t, res)
	if len(r.Outline) != 60 {
		t.Fatalf("outline has %d entries, want 60", len(r.Outline))
	}
	h := r.Outline[0]
	if h.Kind != "heading" || h.Level == nil || *h.Level != 1 || h.Href != "" {
		t.Errorf("heading entry = %+v", h)
	}
	if r.Outline[1].Level != nil {
		t.Errorf("link entry got a level")
	}
	if n := utf8.RuneCountInString(r.Outline[1].Text); n != 120 {
		t.Errorf("text chars = %d, want 120", n)
	}
	if n := utf8.RuneCountInString(r.Outline[1].Href); n != 500 {
		t.Errorf("href chars = %d, want 500", n)
	}
	if r.Truncated {
		t.Errorf("capped outline needed shortening")
	}
	if !strings.HasSuffix(r.Message, "; outline: 60 entries") {
		t.Errorf("message = %q", r.Message)
	}
}

func TestRenderVerifyBrowserOutcome_OutlineHalvedBeforeDOMText(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal(passResult(), &m)
	m["dom_text_snippet"] = strings.Repeat("t", 1000)
	res, _ := json.Marshal(m)
	// 60 entries of ~650 bytes don't fit; 15 do.
	_, r := renderOutcome(t, withOutline(t, res, 60, 120, 600))
	if !r.Truncated {
		t.Errorf("not marked truncated")
	}
	if len(r.Outline) == 0 || len(r.Outline) >= 60 {
		t.Errorf("outline has %d entries, want halved", len(r.Outline))
	}
	if n := utf8.RuneCountInString(r.DOMTextSnippet); n != 1000 {
		t.Errorf("dom_text_snippet cut to %d chars though halving the outline sufficed", n)
	}
	if !strings.Contains(r.Message, "outline: 60 entries") {
		t.Errorf("message = %q", r.Message)
	}
}

func TestRenderVerifyBrowserOutcome_OutlineDropped(t *testing.T) {
	text, r := renderOutcome(t, withOutline(t, bigResult(), 60, 120, 600))
	if strings.Contains(text, `"outline"`) {
		t.Errorf("outline kept in an oversized result")
	}
	if !r.Truncated {
		t.Errorf("not marked truncated")
	}
}

func TestRenderVerifyBrowserOutcome_NoOutlineFor100(t *testing.T) {
	text, r := renderOutcome(t, passResult())
	if strings.Contains(text, `"outline"`) {
		t.Errorf("1.0.0 result got an outline: %s", text)
	}
	if strings.Contains(r.Message, "outline") {
		t.Errorf("message = %q", r.Message)
	}
}

// mobileResult is passResult as a 1.2.0 image reports it for the mobile preset.
func mobileResult(t *testing.T) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(passResult(), &m); err != nil {
		t.Fatal(err)
	}
	m["viewport"] = map[string]any{"width": 412, "height": 839, "preset": "mobile", "mobile": true}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyRendered_SchemaAndDescriptionMentionViewport(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(verifyPreviewRenderedInputSchema), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	vp, ok := schema.Properties["viewport"]
	if !ok {
		t.Fatal("schema has no viewport")
	}
	for _, key := range []string{"preset", "width", "height"} {
		if _, ok := vp.Properties[key]; !ok {
			t.Errorf("viewport schema has no %s", key)
		}
	}
	if vp.AdditionalProperties == nil || *vp.AdditionalProperties {
		t.Error("viewport schema allows other keys")
	}
	for _, want := range []string{`"desktop"`, `"tablet"`, `"mobile"`, `"minimum": 320`, `"maximum": 2560`, `"minimum": 480`, `"maximum": 1600`} {
		if !strings.Contains(verifyPreviewRenderedInputSchema, want) {
			t.Errorf("schema is missing %s", want)
		}
	}
	for _, want := range []string{"viewport", "1280×720", "712×1138", "412×839", "Galaxy Tab S4", "Pixel 7", "one size", "320–2560", "480–1600", "At most 30 calls"} {
		if !strings.Contains(verifyPreviewRenderedDescription, want) {
			t.Errorf("description is missing %q", want)
		}
	}
}

func TestVerifyRendered_ViewportValidation(t *testing.T) {
	bad := map[string]string{
		"width 319":      `{"width":319,"height":600}`,
		"width 2561":     `{"width":2561,"height":600}`,
		"height 479":     `{"width":800,"height":479}`,
		"height 1601":    `{"width":800,"height":1601}`,
		"fractional":     `{"width":800.5,"height":600}`,
		"string width":   `{"width":"800","height":600}`,
		"unknown preset": `{"preset":"watch"}`,
		"preset number":  `{"preset":1}`,
		"both forms":     `{"preset":"mobile","width":800,"height":600}`,
		"width alone":    `{"width":800}`,
		"unknown key":    `{"preset":"mobile","orientation":"landscape"}`,
		"empty":          `{}`,
		"not an object":  `"mobile"`,
		"null":           `null`,
	}
	for name, vp := range bad {
		t.Run(name, func(t *testing.T) {
			lists := 0
			record := NewPreviewRecord(func() ([]string, error) { lists++; return nil, nil }, nil)
			record.AddURL(renderedPreview)
			fb := &fakeBrowser{}
			tool, _ := newRenderedTool(record, fb)
			_, err := call(t, tool, `{"url":"`+renderedPreview+`","viewport":`+vp+`}`)
			if err == nil || !strings.Contains(err.Error(), "viewport") {
				t.Fatalf("err = %v, want one naming viewport", err)
			}
			if len(fb.requests) != 0 || lists != 0 {
				t.Fatal("started something before rejecting the viewport")
			}
		})
	}
	good := map[string]string{
		`{"preset":"desktop"}`:          `{"preset":"desktop"}`,
		`{"preset":"tablet"}`:           `{"preset":"tablet"}`,
		`{"preset":"mobile"}`:           `{"preset":"mobile"}`,
		`{"width":320,"height":480}`:    `{"width":320,"height":480}`,
		`{"width":2560,"height":1600}`:  `{"width":2560,"height":1600}`,
		`{"height":720,"width":1279.0}`: `{"width":1279,"height":720}`,
	}
	for vp, want := range good {
		t.Run(vp, func(t *testing.T) {
			fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: mobileResult(t)}}
			tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
			if _, err := call(t, tool, `{"url":"`+renderedPreview+`","viewport":`+vp+`}`); err != nil {
				t.Fatalf("handle: %v", err)
			}
			var in map[string]json.RawMessage
			if err := json.Unmarshal(fb.requests[0].Input, &in); err != nil {
				t.Fatal(err)
			}
			if string(in["viewport"]) != want {
				t.Fatalf("input.json viewport = %s, want %s", in["viewport"], want)
			}
		})
	}
}

func TestVerifyRendered_InputHasViewportOnlyWhenGiven(t *testing.T) {
	for _, args := range []string{`{"url":"` + renderedPreview + `"}`} {
		fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
		tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
		if _, err := call(t, tool, args); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(fb.requests[0].Input), "viewport") {
			t.Errorf("%s: input.json = %s", args, fb.requests[0].Input)
		}
	}
}

func TestVerifyRendered_ReplyCarriesTheViewport(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: mobileResult(t)}}
	tool, logs := newRenderedTool(renderedRecord(renderedPreview), fb)
	out, err := call(t, tool, `{"url":"`+renderedPreview+`","viewport":{"preset":"mobile"}}`)
	if err != nil {
		t.Fatal(err)
	}
	var res renderedResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Viewport == nil || *res.Viewport != (renderedViewport{Width: 412, Height: 839, Preset: "mobile", Mobile: true}) {
		t.Errorf("viewport = %+v", res.Viewport)
	}
	if res.Message != "pass at 412×839 (mobile) — 1 of 1 assertions passed; no page errors; no failed requests" {
		t.Errorf("message = %q", res.Message)
	}
	if !strings.Contains(logs.String(), "viewport mobile") {
		t.Errorf("logs = %q", logs.String())
	}
}

func TestRenderVerifyBrowserOutcome_CustomAndDefaultViewport(t *testing.T) {
	custom := bytes.Replace(passResult(), []byte(`"status":"completed",`), []byte(`"status":"completed","viewport":{"width":1279,"height":720,"mobile":false},`), 1)
	_, r := renderOutcome(t, custom)
	if !strings.HasPrefix(r.Message, "pass at 1279×720 — ") {
		t.Errorf("message = %q", r.Message)
	}
	// The desktop default, which a 1.2.0 image reports when none was asked for.
	desktop := bytes.Replace(passResult(), []byte(`"status":"completed",`), []byte(`"status":"completed","viewport":{"width":1280,"height":720,"preset":"desktop","mobile":false},`), 1)
	text, r := renderOutcome(t, desktop)
	if !strings.HasPrefix(r.Message, "pass at 1280×720 (desktop) — ") || !strings.Contains(text, `"viewport":{"width":1280,"height":720,"preset":"desktop","mobile":false}`) {
		t.Errorf("message = %q, reply = %s", r.Message, text)
	}
}

func TestVerifyRendered_OlderImageIgnoringTheViewport(t *testing.T) {
	const ref = "docker.io/deploymenthq/verify-browser:1.1.0"
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{ImageRef: ref, Result: passResult()}}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	out, err := call(t, tool, `{"url":"`+renderedPreview+`","viewport":{"width":1279,"height":720}}`)
	want := "this runner's QA browser (" + ref + ") can't set the window size; it needs verify-browser 1.2.0 or later"
	if err == nil || err.Error() != want {
		t.Fatalf("out = %q, err = %v\nwant error %q", out, err, want)
	}
	// Without a viewport, the same image's result is a normal reply.
	out, err = call(t, tool, `{"url":"`+renderedPreview+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res renderedResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Viewport != nil || strings.Contains(out, "viewport") || res.Message != "pass — 1 of 1 assertions passed; no page errors; no failed requests" {
		t.Errorf("reply = %s", out)
	}
	// An older image's error result is still that error, viewport or not.
	fb.outcome = VerifyBrowserOutcome{ImageRef: ref, Result: []byte(`{"schema_version":1,"status":"error","error":"the page could not be loaded"}`)}
	if _, err := call(t, tool, `{"url":"`+renderedPreview+`","viewport":{"preset":"mobile"}}`); err == nil || !strings.Contains(err.Error(), "could not be loaded") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyRendered_CallCapAllowsTwoSizesPerPage(t *testing.T) {
	if verifyRenderedMaxCalls != 30 {
		t.Fatalf("verifyRenderedMaxCalls = %d, want 30", verifyRenderedMaxCalls)
	}
}

func TestVerifyRendered_BackStep(t *testing.T) {
	if err := checkRenderedStep(verifyRenderedStep{Action: "back"}); err != nil {
		t.Errorf("back step: %v", err)
	}
	if err := checkRenderedStep(verifyRenderedStep{Action: "hover"}); err == nil || !strings.Contains(err.Error(), "wait_for or back") {
		t.Errorf("unknown action error = %v", err)
	}
	if !strings.Contains(verifyPreviewRenderedInputSchema, `"enum": ["goto", "click", "fill", "press", "wait_for", "back"]`) {
		t.Error("the steps action enum is missing back")
	}
	if !strings.Contains(verifyPreviewRenderedInputSchema, `{\"action\":\"back\"} (go back one page, like the browser's Back button; client-side route changes count)`) {
		t.Error("the steps description is missing back")
	}
	if err := json.Unmarshal([]byte(verifyPreviewRenderedInputSchema), new(any)); err != nil {
		t.Fatalf("schema: %v", err)
	}
}

func TestVerifyRendered_BackStepNeedsImage140(t *testing.T) {
	for args, want := range map[string]string{
		`{"url":"` + renderedPreview + `","steps":[{"action":"click","selector":"a"},{"action":"back"}]}`: "1.4.0",
		`{"url":"` + renderedPreview + `","steps":[{"action":"click","selector":"a"}]}`:                   "",
		`{"url":"` + renderedPreview + `"}`: "",
	} {
		fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
		tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
		if _, err := call(t, tool, args); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if got := fb.requests[0].MinImageVersion; got != want {
			t.Errorf("%s: MinImageVersion = %q, want %q", args, got, want)
		}
	}
}
