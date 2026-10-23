package agenttools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPickProbeKey(t *testing.T) {
	cases := []struct {
		keys []string
		want string
	}{
		{[]string{"assets/app.js", "assets/app.css", "index.html"}, "assets/app.css"},
		{[]string{"index.html", "about/INDEX.HTML", "page.Html"}, ""},
		{[]string{".well-known/x.json", "a/.hidden.js", ".env", "z.js"}, "z.js"},
		{[]string{"b.js", "a.js"}, "a.js"}, // lexical order, whatever the input order
		{nil, ""},
	}
	for _, c := range cases {
		if got := pickProbeKey(c.keys); got != c.want {
			t.Errorf("pickProbeKey(%q) = %q, want %q", c.keys, got, c.want)
		}
	}
}

func TestSortedUploadedKeys(t *testing.T) {
	got := sortedUploadedKeys(map[string]bool{"b.js": true, "a.css": true, "index.html": true})
	if strings.Join(got, ",") != "a.css,b.js,index.html" {
		t.Fatalf("got %q", got)
	}
}

func TestRecordPreviewDeploy_Probe(t *testing.T) {
	const url1 = "https://dnew.cloudfront.net"
	record := NewPreviewRecord(func() ([]string, error) {
		return []string{"https://dlisted.cloudfront.net"}, nil
	}, io.Discard)

	// A first deploy (a new distribution) arms the probe.
	recordPreviewDeploy(record, url1, StaticPreviewDeployResult{DistributionID: "E1", DomainName: "dnew.cloudfront.net",
		UploadedKeys: []string{"assets/a.js", "index.html"}})
	got, firstProbe := record.pendingProbe("dnew.cloudfront.net")
	if got != "assets/a.js" {
		t.Fatalf("first deploy probe = %q", got)
	}
	record.probeServed("dnew.cloudfront.net", firstProbe)
	if got, _ := record.pendingProbe("dnew.cloudfront.net"); got != "" {
		t.Fatalf("a served probe must not be pending, got %q", got)
	}

	// A later deploy in this run to that host (now a reuse) replaces it, unsatisfied.
	recordPreviewDeploy(record, url1, StaticPreviewDeployResult{DistributionID: "E1",
		UploadedKeys: []string{"assets/b.js", "index.html"}})
	if got, _ := record.pendingProbe("DNEW.cloudfront.net"); got != "assets/b.js" {
		t.Fatalf("replaced probe = %q", got)
	}
	// Marking a stale probe served doesn't satisfy the new one.
	record.probeServed("dnew.cloudfront.net", firstProbe)
	if got, _ := record.pendingProbe("dnew.cloudfront.net"); got != "assets/b.js" {
		t.Fatalf("stale probeServed satisfied the new probe: %q", got)
	}

	// A redeploy with the same probe path is a new probe: a request for the
	// earlier upload (in flight across the redeploy) doesn't satisfy it.
	_, inFlight := record.pendingProbe("dnew.cloudfront.net")
	recordPreviewDeploy(record, url1, StaticPreviewDeployResult{DistributionID: "E1",
		UploadedKeys: []string{"assets/b.js", "index.html"}})
	record.probeServed("dnew.cloudfront.net", inFlight)
	if got, _ := record.pendingProbe("dnew.cloudfront.net"); got != "assets/b.js" {
		t.Fatalf("an earlier request satisfied the same-path redeploy's probe: %q", got)
	}

	// A reused distribution from an earlier run sets nothing.
	recordPreviewDeploy(record, "https://dold.cloudfront.net", StaticPreviewDeployResult{DistributionID: "E2",
		UploadedKeys: []string{"assets/a.js"}})
	if got, _ := record.pendingProbe("dold.cloudfront.net"); got != "" {
		t.Fatalf("reuse armed a probe: %q", got)
	}
	if !record.has("dold.cloudfront.net") {
		t.Fatal("reuse must still record the host")
	}

	// An upload with nothing but HTML arms nothing.
	recordPreviewDeploy(record, "https://dhtml.cloudfront.net", StaticPreviewDeployResult{DistributionID: "E3",
		DomainName: "dhtml.cloudfront.net", UploadedKeys: []string{"index.html"}})
	if got, _ := record.pendingProbe("dhtml.cloudfront.net"); got != "" {
		t.Fatalf("html-only upload armed a probe: %q", got)
	}

	// A host from the control plane's list has none.
	if !newPreviewURLCheck(record).allowedRaw("https://dlisted.cloudfront.net/") {
		t.Fatal("listed host must be accepted")
	}
	if got, _ := record.pendingProbe("dlisted.cloudfront.net"); got != "" {
		t.Fatalf("listed host has a probe: %q", got)
	}

	// Nil record is safe.
	var nilRecord *PreviewRecord
	recordPreviewDeploy(nilRecord, url1, StaticPreviewDeployResult{DomainName: "x", UploadedKeys: []string{"a.js"}})
	if got, _ := nilRecord.pendingProbe("x"); got != "" {
		t.Fatal("nil record has a probe")
	}
	nilRecord.probeServed("x", nil)
}

func TestPreviewProbeURL(t *testing.T) {
	const host = "dnew.cloudfront.net"
	cases := map[string]string{
		"assets/app.js":       "https://dnew.cloudfront.net/assets/app.js",
		"assets/a b?#.js":     "https://dnew.cloudfront.net/assets/a%20b%3F%23.js",
		"/evil.example.com/x": "https://dnew.cloudfront.net//evil.example.com/x",
		"user@evil.com/x.js":  "https://dnew.cloudfront.net/user@evil.com/x.js",
		"..%2F..":             "https://dnew.cloudfront.net/..%252F..",
	}
	for path, want := range cases {
		got, ok := previewProbeURL(host, path)
		if !ok || got != want {
			t.Errorf("previewProbeURL(%q) = %q, %v; want %q", path, got, ok, want)
			continue
		}
		u, err := url.Parse(got)
		if err != nil || u.Hostname() != host || u.User != nil {
			t.Errorf("probe URL %q left the host (%v)", got, err)
		}
	}
}

// probeServer is a TLS server every preview host is routed to (through
// previewHTTPTransport); handler sees each request.
func probeServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	prev := previewHTTPTransport
	previewHTTPTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(func() { previewHTTPTransport = prev })
}

func armedRecord(t *testing.T) *PreviewRecord {
	t.Helper()
	record := NewPreviewRecord(nil, io.Discard)
	recordPreviewDeploy(record, "https://dnew.cloudfront.net", StaticPreviewDeployResult{DistributionID: "E1",
		DomainName: "dnew.cloudfront.net", UploadedKeys: []string{"assets/app.css", "index.html"}})
	return record
}

func verifyCall(t *testing.T, record *PreviewRecord, logs io.Writer, maxWait int) verifyPreviewReachableResult {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"url": "https://dnew.cloudfront.net/", "max_wait_seconds": maxWait})
	out, err := handleVerifyPreviewReachable(context.Background(), record, logs, args)
	if err != nil {
		t.Fatal(err)
	}
	var res verifyPreviewReachableResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestVerifyPreviewReachable_WaitsForProbe(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	probeHits := 0
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		if r.Host != "dnew.cloudfront.net" {
			t.Errorf("request to host %q", r.Host)
		}
		if r.URL.Path == "/assets/app.css" {
			probeHits++
			if probeHits < 2 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		io.WriteString(w, "<html>ok</html>")
	})
	record := armedRecord(t)
	var logs strings.Builder
	res := verifyCall(t, record, &logs, 20)
	if !res.Live || res.StatusCode != 200 || !strings.Contains(res.BodySnippet, "ok") {
		t.Fatalf("expected live after the probe turned 200, got %+v", res)
	}
	if probeHits != 2 {
		t.Fatalf("probe requested %d times, want 2", probeHits)
	}
	if want := "verify_preview_reachable: the front page answers; waiting for assets/app.css to be served too\n"; strings.Count(logs.String(), want) != 1 {
		t.Fatalf("logs = %q", logs.String())
	}

	// Satisfied: a later call doesn't request the probe path.
	mu.Lock()
	paths = nil
	mu.Unlock()
	if res := verifyCall(t, record, io.Discard, 5); !res.Live {
		t.Fatalf("expected live, got %+v", res)
	}
	for _, p := range paths {
		if p == "/assets/app.css" {
			t.Fatal("a satisfied probe was requested again")
		}
	}
}

func TestVerifyPreviewReachable_ProbeNeverServed(t *testing.T) {
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/app.css" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, "front")
	})
	record := armedRecord(t)
	start := time.Now()
	res := verifyCall(t, record, io.Discard, 1)
	if res.Live || res.StatusCode != 200 || res.BodySnippet != "front" || res.Message != verifyPreviewProbePendingMessage {
		t.Fatalf("got %+v", res)
	}
	if want := "The front page answers, but the rest of the site isn't served everywhere yet (a new distribution is still propagating). Call verify_preview_reachable again — it returns as soon as it is."; res.Message != want {
		t.Fatalf("message = %q", res.Message)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("the probe must share the call's max_wait, took %s", time.Since(start))
	}
	if got, _ := record.pendingProbe("dnew.cloudfront.net"); got == "" {
		t.Fatal("an unserved probe must stay pending")
	}
}

func TestVerifyPreviewReachable_NoProbeUnchanged(t *testing.T) {
	var probed bool
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			probed = true
		}
		io.WriteString(w, "front")
	})
	record := NewPreviewRecord(nil, io.Discard)
	recordPreviewDeploy(record, "https://dnew.cloudfront.net", StaticPreviewDeployResult{DistributionID: "E1",
		UploadedKeys: []string{"assets/app.css"}}) // a reuse from an earlier run
	res := verifyCall(t, record, io.Discard, 5)
	if !res.Live || res.Message != "preview is live" || probed {
		t.Fatalf("got %+v (probed=%v)", res, probed)
	}
}
