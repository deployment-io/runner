package agenttools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsAllowedPreviewURL locks down the SSRF guard: only https *.cloudfront.net.
func TestIsAllowedPreviewURL(t *testing.T) {
	allowed := []string{
		"https://d1cocjwyiwd0na.cloudfront.net/",
		"https://d1cocjwyiwd0na.cloudfront.net/signin",
		"https://ABC123.cloudfront.net", // hostname is lowercased before the check
	}
	blocked := []string{
		"http://d1.cloudfront.net/",                 // not https
		"https://169.254.169.254/latest/meta-data/", // cloud metadata
		"https://localhost:8080/",                   // localhost
		"https://10.0.0.5/",                         // internal IP
		"https://evil.cloudfront.net.attacker.com/", // lookalike suffix
		"https://cloudfront.net/",                   // no subdomain (no leading dot)
		"https://example.com/",                      // arbitrary host
		"ftp://x.cloudfront.net/",                   // wrong scheme
		"not a url",
	}
	for _, u := range allowed {
		if !isAllowedPreviewURL(u) {
			t.Errorf("expected allowed: %s", u)
		}
	}
	for _, u := range blocked {
		if isAllowedPreviewURL(u) {
			t.Errorf("expected BLOCKED: %s", u)
		}
	}
}

func TestPollURL_BecomesLive(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "<html><title>ok</title></html>")
	}))
	defer srv.Close()

	res := pollURL(context.Background(), srv.URL, "", 10*time.Second, 5*time.Millisecond, nil)
	if !res.Live {
		t.Fatalf("expected live, got %+v", res)
	}
	if res.Attempts < 3 {
		t.Fatalf("expected >=3 attempts, got %d", res.Attempts)
	}
	if res.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", res.StatusCode)
	}
}

func TestPollURL_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	res := pollURL(context.Background(), srv.URL, "", 60*time.Millisecond, 5*time.Millisecond, nil)
	if res.Live {
		t.Fatalf("expected not live, got %+v", res)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected last status 503, got %d", res.StatusCode)
	}
}

func TestPollURL_Contains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "<title>Deployment.io - Dashboard</title>")
	}))
	defer srv.Close()

	// present → live + matched
	res := pollURL(context.Background(), srv.URL, "Deployment.io", 2*time.Second, 5*time.Millisecond, nil)
	if !res.Live || res.Matched == nil || !*res.Matched {
		t.Fatalf("expected live+matched, got %+v", res)
	}
	// absent → 200 but no match → not live, matched=false
	res2 := pollURL(context.Background(), srv.URL, "NOPE", 60*time.Millisecond, 5*time.Millisecond, nil)
	if res2.Live {
		t.Fatalf("expected not live for missing substring, got %+v", res2)
	}
	if res2.Matched == nil || *res2.Matched {
		t.Fatalf("expected matched=false, got %+v", res2)
	}
}
