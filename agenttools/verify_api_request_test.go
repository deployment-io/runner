package agenttools

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

const apiTestURL = "https://dapi.cloudfront.net"

func apiTestTool(logs io.Writer) *verifyAPIRequest {
	record := NewPreviewRecord(nil, io.Discard)
	record.AddURL(apiTestURL)
	return &verifyAPIRequest{record: record, logsWriter: logs}
}

func apiCall(t *testing.T, tool *verifyAPIRequest, args map[string]any) (verifyAPIRequestResult, string) {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := tool.handle(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > verifyAPIRequestMaxResultBytes {
		t.Fatalf("result is %d bytes", len(out))
	}
	var res verifyAPIRequestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return res, out
}

func apiCallErr(t *testing.T, tool *verifyAPIRequest, args map[string]any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	_, err := tool.handle(context.Background(), raw)
	if err == nil {
		t.Fatalf("%v: want an error", args)
	}
	return err
}

func TestVerifyAPIRequestRegistered(t *testing.T) {
	srv := agentmcp.New("test", "0")
	RegisterVerifyAPIRequest(srv, NewPreviewRecord(nil, nil), nil)
	if got := strings.Join(srv.ToolNames(), ","); got != "verify_api_request" {
		t.Errorf("tools = %s", got)
	}
	if !json.Valid([]byte(verifyAPIRequestInputSchema)) {
		t.Error("the input schema isn't JSON")
	}
}

// Each method is sent as asked, with the caller's headers and body as given and
// no Content-Type of the tool's own.
func TestVerifyAPIRequestSendsTheRequestAsGiven(t *testing.T) {
	type seen struct {
		method, path, query, body string
		header                    http.Header
	}
	var got seen
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()}
		w.WriteHeader(http.StatusCreated)
	})
	tool := apiTestTool(nil)
	for _, method := range verifyAPIRequestMethods {
		args := map[string]any{"method": method, "url": apiTestURL + "/items?x=1"}
		withBody := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		if withBody {
			args["body"] = `{"name":"a"}`
		}
		res, _ := apiCall(t, tool, args)
		if res.Status != http.StatusCreated || got.method != method || got.path != "/items" || got.query != "x=1" {
			t.Errorf("%s: status %d, server saw %+v", method, res.Status, got)
		}
		if withBody && got.body != `{"name":"a"}` {
			t.Errorf("%s: body = %q", method, got.body)
		}
		if ct := got.header.Get("Content-Type"); ct != "" {
			t.Errorf("%s: the tool added Content-Type %q", method, ct)
		}
	}
	res, _ := apiCall(t, tool, map[string]any{"url": apiTestURL + "/"})
	if got.method != "GET" || res.Status != http.StatusCreated {
		t.Errorf("the default method is %s", got.method)
	}
	apiCall(t, tool, map[string]any{"method": "POST", "url": apiTestURL + "/items", "body": "{}",
		"headers": map[string]string{"Content-Type": "application/json", "X-Api-Version": "2", "accept": "text/plain"}})
	if got.header.Get("Content-Type") != "application/json" || got.header.Get("X-Api-Version") != "2" || got.header.Get("Accept") != "text/plain" {
		t.Errorf("headers = %v", got.header)
	}
}

func TestVerifyAPIRequestRefusesBadArguments(t *testing.T) {
	probeServer(t, func(http.ResponseWriter, *http.Request) { t.Error("a refused request was sent") })
	tool := apiTestTool(nil)
	many := map[string]string{}
	for i := 0; i < 21; i++ {
		many[fmt.Sprintf("X-H%d", i)] = "v"
	}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"url": "https://evil.example.com/"}, "url must be a backend preview URL this Task deployed"},
		{map[string]any{"url": "http://dapi.cloudfront.net/"}, "https on the default port"},
		{map[string]any{"url": "https://dapi.cloudfront.net:8443/"}, "https on the default port"},
		{map[string]any{"url": "https://u:p@dapi.cloudfront.net/"}, "no user info"},
		{map[string]any{"url": "https://169.254.169.254/latest/meta-data"}, "this Task deployed"},
		{map[string]any{}, "url is required"},
		{map[string]any{"url": apiTestURL, "method": "get"}, "method must be one of"},
		{map[string]any{"url": apiTestURL, "method": "TRACE"}, "method must be one of"},
		{map[string]any{"url": apiTestURL, "body": "x"}, "a body can't be sent with GET"},
		{map[string]any{"url": apiTestURL, "method": "HEAD", "body": "x"}, "with HEAD"},
		{map[string]any{"url": apiTestURL, "method": "OPTIONS", "body": "x"}, "with OPTIONS"},
		{map[string]any{"url": apiTestURL, "method": "POST", "body": strings.Repeat("x", 64*1024+1)}, "at most 65536"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"host": "evil.example.com"}}, "header host is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Content-Length": "5"}}, "header Content-Length is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Transfer-Encoding": "chunked"}}, "Transfer-Encoding is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Connection": "close"}}, "Connection is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Upgrade": "h2c"}}, "Upgrade is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"TE": "trailers"}}, "TE is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Trailer": "x"}}, "Trailer is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Keep-Alive": "1"}}, "Keep-Alive is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"proxy-authorization": "x"}}, "proxy-authorization is not allowed"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"X-A": "1\r\nX-B: 2"}}, "control character"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"Bad Name": "1"}}, "not a valid HTTP header name"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{strings.Repeat("X", 101): "1"}}, "longer than 100"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"X-A": strings.Repeat("v", 2001)}}, "longer than 2000"},
		{map[string]any{"url": apiTestURL, "headers": map[string]string{"x-a": "1", "X-A": "2"}}, "given twice"},
		{map[string]any{"url": apiTestURL, "headers": many}, "at most 20 headers"},
		{map[string]any{"url": apiTestURL, "timeout_seconds": 0}, "timeout_seconds must be between 1 and 60"},
		{map[string]any{"url": apiTestURL, "timeout_seconds": 61}, "timeout_seconds must be between 1 and 60"},
	} {
		if err := apiCallErr(t, tool, tc.args); !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error %q, want %q", tc.args, err, tc.want)
		}
	}
}

// A redirect is the result itself: its status and Location, not followed.
func TestVerifyAPIRequestDoesNotFollowRedirects(t *testing.T) {
	followed := false
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/new" {
			followed = true
		}
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	res, _ := apiCall(t, apiTestTool(nil), map[string]any{"url": apiTestURL + "/old"})
	if res.Status != http.StatusFound || res.Headers["Location"] != "/new" || followed {
		t.Errorf("status %d, Location %q, followed %v", res.Status, res.Headers["Location"], followed)
	}
}

func TestVerifyAPIRequestResponseHeaders(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h["x-multi"] = []string{"a", "b"}
		h.Add("X-Multi", "c")
		h.Set("X-Long", strings.Repeat("é", 1500))
		if r.URL.Path == "/many" {
			for i := 0; i < 10; i++ {
				h.Set(fmt.Sprintf("X-Big-%d", i), strings.Repeat("v", 900))
			}
		}
	})
	tool := apiTestTool(nil)
	res, _ := apiCall(t, tool, map[string]any{"url": apiTestURL + "/"})
	if got := res.Headers["X-Multi"]; got != "a, b, c" && got != "c, a, b" {
		t.Errorf("X-Multi = %q", got)
	}
	if got := res.Headers["X-Long"]; got != strings.Repeat("é", 1000) {
		t.Errorf("X-Long has %d characters", len([]rune(got)))
	}
	if res.HeadersTruncated {
		t.Error("headers_truncated without a cut")
	}
	res, _ = apiCall(t, tool, map[string]any{"url": apiTestURL + "/many"})
	if !res.HeadersTruncated {
		t.Error("headers over 4 KiB aren't truncated")
	}
	size := 0
	for name, value := range res.Headers {
		size += len(jsonText(name)) + len(jsonText(value)) + 2
	}
	if size > verifyAPIRequestMaxHeadersBytes || len(res.Headers) < 2 {
		t.Errorf("headers are %d bytes, %d of them", size, len(res.Headers))
	}
}

func TestVerifyAPIRequestBody(t *testing.T) {
	bodies := map[string][]byte{
		"/small":  []byte(`{"ok":true}`),
		"/large":  bytes.Repeat([]byte("a"), 20000),
		"/binary": {0xff, 0xfe, 0x00, 0x01},
		"/huge":   bytes.Repeat([]byte("b"), 2*1024*1024),
		// A three-byte rune straddles the 8 KiB cut.
		"/runes": append(bytes.Repeat([]byte("a"), 8191), []byte("€€")...),
	}
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(bodies[r.URL.Path])
	})
	tool := apiTestTool(nil)
	res, _ := apiCall(t, tool, map[string]any{"url": apiTestURL + "/small"})
	if res.Status != 200 || res.Body != `{"ok":true}` || res.BodyBytes != 11 || res.BodyTruncated || res.BodyBinary || res.BodyBytesCapped || res.ElapsedMS < 0 {
		t.Errorf("small: %+v", res)
	}
	res, _ = apiCall(t, tool, map[string]any{"url": apiTestURL + "/large"})
	if len(res.Body) != 8192 || res.BodyBytes != 20000 || !res.BodyTruncated || res.BodyBytesCapped {
		t.Errorf("large: body %d, %+v", len(res.Body), res.BodyBytes)
	}
	res, _ = apiCall(t, tool, map[string]any{"url": apiTestURL + "/binary"})
	if res.Body != "" || !res.BodyBinary || res.BodyBytes != 4 || !res.BodyTruncated {
		t.Errorf("binary: %+v", res)
	}
	res, _ = apiCall(t, tool, map[string]any{"url": apiTestURL + "/huge"})
	if res.BodyBytes != 1024*1024 || !res.BodyBytesCapped || !res.BodyTruncated {
		t.Errorf("huge: body_bytes %d, capped %v", res.BodyBytes, res.BodyBytesCapped)
	}
	res, _ = apiCall(t, tool, map[string]any{"url": apiTestURL + "/runes"})
	if res.BodyBinary || len(res.Body) != 8191 || !res.BodyTruncated {
		t.Errorf("runes: binary %v, body %d", res.BodyBinary, len(res.Body))
	}
	res, _ = apiCall(t, tool, map[string]any{"method": "HEAD", "url": apiTestURL + "/small"})
	if res.Status != 200 || res.Body != "" || res.BodyBytes != 0 {
		t.Errorf("HEAD: %+v", res)
	}
}

// A body that grows when escaped is cut further so the result fits.
func TestVerifyAPIRequestEscapedBodyFits(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte{0x01}, 8192))
	})
	res, out := apiCall(t, apiTestTool(nil), map[string]any{"url": apiTestURL + "/"})
	if len(out) > verifyAPIRequestMaxResultBytes || !res.BodyTruncated || res.Body == "" || len(res.Body) >= 8192 || res.BodyBytes != 8192 {
		t.Errorf("result %d bytes, body %d, truncated %v", len(out), len(res.Body), res.BodyTruncated)
	}
}

func TestVerifyAPIRequestNoAnswer(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	})
	var logs bytes.Buffer
	res, _ := apiCall(t, apiTestTool(&logs), map[string]any{"url": apiTestURL + "/slow", "timeout_seconds": 1})
	if res.Status != 0 || res.Error != "no answer: timed out after 1 s" {
		t.Errorf("timeout: %+v", res)
	}
	if want := "verify_api_request: GET https://dapi.cloudfront.net/slow → no answer (timed out after 1 s)\n"; logs.String() != want {
		t.Errorf("log = %q, want %q", logs.String(), want)
	}

	// Nothing listens: connection refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	prev := previewHTTPTransport
	previewHTTPTransport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	defer func() { previewHTTPTransport = prev }()
	res, _ = apiCall(t, apiTestTool(nil), map[string]any{"url": apiTestURL + "/"})
	if res.Status != 0 || res.Error != "no answer: connection refused" {
		t.Errorf("refused: %+v", res)
	}
}

// The log says the method, host, path and status — never the query string,
// headers or bodies.
func TestVerifyAPIRequestLogLine(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Response-Secret", "resp-header-secret")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "response-body-secret")
	})
	var logs bytes.Buffer
	apiCall(t, apiTestTool(&logs), map[string]any{"method": "POST", "url": apiTestURL + "/items/1?token=query-secret",
		"headers": map[string]string{"X-Request-Secret": "req-header-secret"}, "body": "request-body-secret"})
	line := logs.String()
	if !strings.HasPrefix(line, "verify_api_request: POST https://dapi.cloudfront.net/items/1 → 202 in ") || !strings.HasSuffix(line, " ms\n") {
		t.Errorf("log = %q", line)
	}
	for _, secret := range []string{"query-secret", "token", "req-header-secret", "X-Request-Secret", "request-body-secret", "resp-header-secret", "response-body-secret"} {
		if strings.Contains(line, secret) {
			t.Errorf("the log carries %q: %q", secret, line)
		}
	}
}

// Every call counts, errors included; the 61st is refused.
func TestVerifyAPIRequestCallCap(t *testing.T) {
	sent := 0
	probeServer(t, func(http.ResponseWriter, *http.Request) { sent++ })
	tool := apiTestTool(nil)
	apiCallErr(t, tool, map[string]any{"url": "https://evil.example.com/"})
	for i := 1; i < verifyAPIRequestMaxCalls; i++ {
		apiCall(t, tool, map[string]any{"url": apiTestURL + "/"})
	}
	err := apiCallErr(t, tool, map[string]any{"url": apiTestURL + "/"})
	if !strings.Contains(err.Error(), "already been called 60 times") || sent != verifyAPIRequestMaxCalls-1 {
		t.Errorf("61st call: %v, %d sent", err, sent)
	}
}

// A redirect whose Location net/http can't parse still comes back as it is,
// and nothing of it reaches the log beyond the status.
func TestVerifyAPIRequestBadLocationRedirect(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/new%zz?token=location-secret")
		w.WriteHeader(http.StatusFound)
	})
	var logs bytes.Buffer
	res, _ := apiCall(t, apiTestTool(&logs), map[string]any{"url": apiTestURL + "/old"})
	if res.Status != http.StatusFound || res.Headers["Location"] != "/new%zz?token=location-secret" || res.Error != "" {
		t.Errorf("result: %+v", res)
	}
	if strings.Contains(logs.String(), "location-secret") || !strings.Contains(logs.String(), "→ 302 in ") {
		t.Errorf("log = %q", logs.String())
	}
}

// Header names are matched case-insensitively by the transport too: a
// lower-case accept-encoding or user-agent replaces the transport's own.
// Without one, nothing asks for compression and nothing is decompressed.
func TestVerifyAPIRequestHeadersAndCompression(t *testing.T) {
	var seen http.Header
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path == "/gzip" {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write([]byte("hello"))
			zw.Close()
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
			w.Write(buf.Bytes())
		}
	})
	tool := apiTestTool(nil)
	apiCall(t, tool, map[string]any{"url": apiTestURL + "/", "headers": map[string]string{"accept-encoding": "identity", "user-agent": "qa-test"}})
	if got := seen.Values("Accept-Encoding"); len(got) != 1 || got[0] != "identity" {
		t.Errorf("Accept-Encoding = %q", got)
	}
	if got := seen.Values("User-Agent"); len(got) != 1 || got[0] != "qa-test" {
		t.Errorf("User-Agent = %q", got)
	}
	apiCall(t, tool, map[string]any{"url": apiTestURL + "/"})
	if got := seen.Values("Accept-Encoding"); len(got) != 0 {
		t.Errorf("the tool asked for compression: %q", got)
	}
	res, _ := apiCall(t, tool, map[string]any{"url": apiTestURL + "/gzip", "headers": map[string]string{"Accept-Encoding": "gzip"}})
	if res.Headers["Content-Encoding"] != "gzip" || res.Headers["Content-Length"] == "" || !res.BodyBinary {
		t.Errorf("gzip response: %+v", res)
	}
}

// A body that stops before its end is reported, not taken as complete.
func TestVerifyAPIRequestBodyReadError(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	res, _ := apiCall(t, apiTestTool(nil), map[string]any{"url": apiTestURL + "/"})
	if res.Status != 200 || res.Body != "partial" || res.BodyReadError != "the body stopped before its end: the connection closed" {
		t.Errorf("result: %+v", res)
	}
}

// One request per call: the server sees exactly one request, on its own
// connection.
func TestVerifyAPIRequestSendsOnce(t *testing.T) {
	requests := 0
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !r.Close {
			t.Error("the connection is kept alive")
		}
	})
	tool := apiTestTool(nil)
	for i := 0; i < 3; i++ {
		apiCall(t, tool, map[string]any{"method": "POST", "url": apiTestURL + "/", "body": "x"})
	}
	if requests != 3 {
		t.Errorf("%d requests for 3 calls", requests)
	}
}

// A refused call gets its log line too, without query string or headers.
func TestVerifyAPIRequestRefusedLogLines(t *testing.T) {
	probeServer(t, func(http.ResponseWriter, *http.Request) {})
	var logs bytes.Buffer
	tool := apiTestTool(&logs)
	apiCallErr(t, tool, map[string]any{"url": "https://evil.example.com/x?token=query-secret"})
	apiCallErr(t, tool, map[string]any{"method": "GET", "url": apiTestURL + "/y?token=query-secret", "body": "body-secret"})
	apiCallErr(t, tool, map[string]any{"url": apiTestURL + "/z", "headers": map[string]string{"Proxy-Authorization": "header-secret"}})
	tool.calls = verifyAPIRequestMaxCalls
	apiCallErr(t, tool, map[string]any{"url": apiTestURL + "/"})
	want := "verify_api_request: GET https://evil.example.com/x → refused (not a preview this Task deployed)\n" +
		"verify_api_request: GET https://dapi.cloudfront.net/y → refused (invalid arguments)\n" +
		"verify_api_request: GET https://dapi.cloudfront.net/z → refused (invalid arguments)\n" +
		"verify_api_request: refused (the 60-call cap is reached)\n"
	if logs.String() != want {
		t.Errorf("log = %q", logs.String())
	}
}

// Headers Go's parser takes out of resp.Header (Transfer-Encoding, Trailer,
// Connection: close) are still in the result.
func TestVerifyAPIRequestParsedResponseHeaders(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "chunk one")
		w.(http.Flusher).Flush()
		io.WriteString(w, ", chunk two")
		w.Header().Set("X-Checksum", "abc")
	})
	res, _ := apiCall(t, apiTestTool(nil), map[string]any{"url": apiTestURL + "/"})
	if res.Body != "chunk one, chunk two" {
		t.Errorf("body = %q", res.Body)
	}
	for name, want := range map[string]string{"Transfer-Encoding": "chunked", "Trailer": "X-Checksum", "Connection": "close"} {
		if got := res.Headers[name]; got != want {
			t.Errorf("%s = %q, want %q (headers %v)", name, got, want, res.Headers)
		}
	}
	if res.HeadersTruncated {
		t.Error("headers_truncated without a cut")
	}
}
