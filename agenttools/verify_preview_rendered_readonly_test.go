package agenttools

import (
	"encoding/json"
	"strings"
	"testing"
)

func readOnlyResult() []byte {
	return []byte(`{"schema_version":1,"status":"completed","verdict":"pass","http_status":200,
		"final_url":"https://d111.cloudfront.net/","steps":[],"assertions":[],
		"page_errors":[],"console_errors":[],"failed_requests":[],"blocked_navigations":[],
		"blocked_requests":[{"url":"https://d111.cloudfront.net/api/subscribe","method":"POST"},{"url":"wss://d111.cloudfront.net/live","method":"WEBSOCKET"}],
		"timings_ms":{},"dom_text_snippet":"Hello"}`)
}

func newReadOnlyTool(record *PreviewRecord, fb *fakeBrowser, onUnsupported func(string)) *verifyPreviewRendered {
	tool, _ := newRenderedTool(record, fb)
	tool.readOnly = true
	tool.onReadOnlyUnsupported = onUnsupported
	return tool
}

func TestVerifyRenderedReadOnly_SendsReadOnlyOnEveryCall(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: readOnlyResult()}}
	tool := newReadOnlyTool(renderedRecord(renderedPreview), fb, nil)
	for i := 0; i < 3; i++ {
		out, err := call(t, tool, `{"url":"https://d111.cloudfront.net/"}`)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		var res renderedResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.BlockedRequests) != 2 || res.BlockedRequests[0].Method != "POST" {
			t.Errorf("blocked_requests = %+v", res.BlockedRequests)
		}
		if !strings.Contains(res.Message, "2 blocked requests") {
			t.Errorf("message = %q", res.Message)
		}
	}
	for i, req := range fb.requests {
		var in map[string]interface{}
		if err := json.Unmarshal(req.Input, &in); err != nil {
			t.Fatal(err)
		}
		if in["read_only"] != true {
			t.Errorf("request %d: read_only = %v, want true", i, in["read_only"])
		}
	}
}

func TestVerifyRendered_PreviewChecksSendNoReadOnly(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult()}}
	tool, _ := newRenderedTool(renderedRecord(renderedPreview), fb)
	if _, err := call(t, tool, `{"url":"https://d111.cloudfront.net/"}`); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fb.requests[0].Input), "read_only") {
		t.Errorf("a preview check sent read_only: %s", fb.requests[0].Input)
	}
}

func TestVerifyRenderedReadOnly_NoBlockedRequestsStopsTheRun(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: passResult(), ImageRef: "docker.io/deploymenthq/verify-browser:1.2.0"}}
	var reasons []string
	tool := newReadOnlyTool(renderedRecord(renderedPreview), fb, func(reason string) { reasons = append(reasons, reason) })
	_, err := call(t, tool, `{"url":"https://d111.cloudfront.net/"}`)
	if err == nil || !strings.Contains(err.Error(), VerifyBrowserReadOnlyUnsupportedReason) {
		t.Fatalf("err = %v, want the read-only refusal", err)
	}
	if len(reasons) != 1 || reasons[0] != "the QA browser doesn't support read-only checks" {
		t.Errorf("onUnsupported = %q", reasons)
	}
	// Every later call is refused without starting a browser, and the run
	// is told only once.
	if _, err := call(t, tool, `{"url":"https://d111.cloudfront.net/"}`); err == nil {
		t.Error("a later call was allowed")
	}
	if len(fb.requests) != 1 || len(reasons) != 1 {
		t.Errorf("browser runs %d, notifications %d; want 1 and 1", len(fb.requests), len(reasons))
	}

	// blocked_requests: null is not support either.
	fb = &fakeBrowser{outcome: VerifyBrowserOutcome{Result: []byte(strings.Replace(string(readOnlyResult()),
		`"blocked_requests":[{"url":"https://d111.cloudfront.net/api/subscribe","method":"POST"},{"url":"wss://d111.cloudfront.net/live","method":"WEBSOCKET"}]`,
		`"blocked_requests":null`, 1))}}
	tool = newReadOnlyTool(renderedRecord(renderedPreview), fb, nil)
	if _, err := call(t, tool, `{"url":"https://d111.cloudfront.net/"}`); err == nil {
		t.Error("blocked_requests null was accepted")
	}
}

func TestVerifyRenderedReadOnly_EmptyBlockedRequestsIsSupport(t *testing.T) {
	result := strings.Replace(string(passResult()), `"blocked_navigations":[]`, `"blocked_navigations":[],"blocked_requests":[]`, 1)
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: []byte(result)}}
	called := false
	tool := newReadOnlyTool(renderedRecord(renderedPreview), fb, func(string) { called = true })
	if _, err := call(t, tool, `{"url":"https://d111.cloudfront.net/"}`); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("an empty blocked_requests stopped the run")
	}
}

func TestVerifyRenderedReadOnly_RefusesURLOutsideTheRecord(t *testing.T) {
	fb := &fakeBrowser{outcome: VerifyBrowserOutcome{Result: readOnlyResult()}}
	tool := newReadOnlyTool(renderedRecord(renderedPreview, "https://www.acme.com/"), fb, nil)
	if _, err := call(t, tool, `{"url":"https://evil.example.com/"}`); err == nil {
		t.Error("a URL outside the record was opened")
	}
	if len(fb.requests) != 0 {
		t.Error("the browser ran")
	}
	if _, err := call(t, tool, `{"url":"https://www.acme.com/pricing"}`); err != nil {
		t.Errorf("a custom domain in the record was refused: %v", err)
	}
}
