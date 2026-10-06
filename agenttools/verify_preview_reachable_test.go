package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

	res := pollURL(context.Background(), srv.URL, "", 10*time.Second, 5*time.Millisecond, nil, nil)
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

	res := pollURL(context.Background(), srv.URL, "", 60*time.Millisecond, 5*time.Millisecond, nil, nil)
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
	res := pollURL(context.Background(), srv.URL, "Deployment.io", 2*time.Second, 5*time.Millisecond, nil, nil)
	if !res.Live || res.Matched == nil || !*res.Matched {
		t.Fatalf("expected live+matched, got %+v", res)
	}
	// absent → 200 but no match → not live, matched=false
	res2 := pollURL(context.Background(), srv.URL, "NOPE", 2*time.Second, 5*time.Millisecond, nil, nil)
	if !res2.Live {
		t.Fatalf("expected live (200) even with a missing substring, got %+v", res2)
	}
	if res2.Matched == nil || *res2.Matched {
		t.Fatalf("expected matched=false, got %+v", res2)
	}
}

func TestPollURL_ZeroIntervalDoesNotSpin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// interval 0 must be floored, not turned into a tight spin. With the floor the
	// loop makes only a handful of attempts before maxWait expires.
	res := pollURL(context.Background(), srv.URL, "", 100*time.Millisecond, 0, nil, nil)
	if res.Live {
		t.Fatalf("expected not live, got %+v", res)
	}
	if res.Attempts > 20 {
		t.Fatalf("interval=0 spun: %d attempts (expected it to be floored)", res.Attempts)
	}
}

// recordWith returns a record holding the given preview URLs' hosts and listing
// listed (or failing with listErr).
func recordWith(recorded []string, listed []string, listErr error) (*PreviewRecord, *int) {
	calls := 0
	r := NewPreviewRecord(func() ([]string, error) {
		calls++
		return listed, listErr
	}, io.Discard)
	for _, u := range recorded {
		r.AddURL(u)
	}
	return r, &calls
}

// TestPreviewURLCheck locks down the SSRF guard: only https URLs on the default port,
// without user info, whose host is a preview this Task deployed.
func TestPreviewURLCheck(t *testing.T) {
	record, _ := recordWith([]string{"https://d1cocjwyiwd0na.cloudfront.net"}, nil, nil)
	allowed := []string{
		"https://d1cocjwyiwd0na.cloudfront.net/",
		"https://d1cocjwyiwd0na.cloudfront.net/signin",
		"https://D1COCJWYIWD0NA.cloudfront.net",      // host compared case-insensitively
		"https://d1cocjwyiwd0na.cloudfront.net:443/", // explicit default port
	}
	blocked := []string{
		"http://d1cocjwyiwd0na.cloudfront.net/",               // not https
		"https://user:pw@d1cocjwyiwd0na.cloudfront.net/",      // user info
		"https://d1cocjwyiwd0na.cloudfront.net:8443/",         // non-default port
		"https://d2other.cloudfront.net/",                     // a CloudFront host this Task didn't deploy
		"https://d1cocjwyiwd0na.cloudfront.net.attacker.com/", // lookalike
		"https://d1.cloudfront.net.attacker.com/",             // lookalike
		"https://169.254.169.254/latest/meta-data/",           // unrecorded IP address
		"https://localhost/",
		"https://example.com/",
		"not a url",
		"https://",
	}
	for _, u := range allowed {
		if !newPreviewURLCheck(record).allowedRaw(u) {
			t.Errorf("expected allowed: %s", u)
		}
	}
	for _, u := range blocked {
		if newPreviewURLCheck(record).allowedRaw(u) {
			t.Errorf("expected BLOCKED: %s", u)
		}
	}
}

func TestPreviewRecord_EmptyHostNotAdded(t *testing.T) {
	record := NewPreviewRecord(nil, nil)
	record.AddURL("https://")
	if newPreviewURLCheck(record).allowedRaw("https:///") {
		t.Fatal("an empty host must never be recorded")
	}
}

func TestPreviewURLCheck_ListsTaskPreviewsOncePerCall(t *testing.T) {
	record, calls := recordWith(
		[]string{"https://dthisrun.cloudfront.net"},
		[]string{"https://dearlier.cloudfront.net"},
		nil,
	)
	check := newPreviewURLCheck(record)
	if !check.allowedRaw("https://dthisrun.cloudfront.net/") || *calls != 0 {
		t.Fatalf("a recorded host must not list; calls=%d", *calls)
	}
	if !check.allowedRaw("https://dearlier.cloudfront.net/") {
		t.Fatal("a host the lister returns must be accepted")
	}
	if check.allowedRaw("https://dunknown.cloudfront.net/") {
		t.Fatal("a host neither recorded nor listed must be refused")
	}
	if *calls != 1 {
		t.Fatalf("expected one list per call, got %d", *calls)
	}
	// The listed host stays recorded for later calls.
	if !newPreviewURLCheck(record).allowedRaw("https://dearlier.cloudfront.net/") || *calls != 1 {
		t.Fatalf("listed host should now be recorded; calls=%d", *calls)
	}
}

func TestPreviewURLCheck_ListErrorFallsBackToRecord(t *testing.T) {
	var logs strings.Builder
	record := NewPreviewRecord(func() ([]string, error) {
		return nil, errors.New("rpc: can't find method TaskPreviews.ListV1")
	}, &logs)
	record.AddURL("https://dthisrun.cloudfront.net")
	check := newPreviewURLCheck(record)
	if check.allowedRaw("https://dother.cloudfront.net/") {
		t.Fatal("an unrecorded host must be refused when listing fails")
	}
	if !check.allowedRaw("https://dthisrun.cloudfront.net/") {
		t.Fatal("this run's record must still be accepted when listing fails")
	}
	if !strings.Contains(logs.String(), "TaskPreviews.ListV1") {
		t.Fatalf("list error should be logged, got %q", logs.String())
	}
}

func TestHandleVerifyPreviewReachable_RefusesUnrecordedURL(t *testing.T) {
	record, _ := recordWith([]string{"https://dthisrun.cloudfront.net"}, nil, nil)
	args, _ := json.Marshal(map[string]string{"url": "https://dother.cloudfront.net/"})
	_, err := handleVerifyPreviewReachable(context.Background(), record, io.Discard, args)
	want := `url must be a preview URL this Task deployed (the URL deploy_static_site_preview returned) — got "https://dother.cloudfront.net/"`
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestPreviewRedirectPolicy(t *testing.T) {
	record, _ := recordWith([]string{"https://dthisrun.cloudfront.net"}, nil, nil)
	check := newPreviewURLCheck(record)
	var blocked *redirectBlockedError
	policy := previewRedirectPolicy(check.allowed, func(e *redirectBlockedError) { blocked = e })
	req := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		return &http.Request{URL: u}
	}

	if err := policy(req("https://dthisrun.cloudfront.net/index.html"), []*http.Request{req("https://dthisrun.cloudfront.net/")}); err != nil || blocked != nil {
		t.Fatalf("a redirect on a recorded host must be followed, got %v (blocked=%v)", err, blocked)
	}
	for _, target := range []string{"https://evil.example.com/", "http://dthisrun.cloudfront.net/"} {
		blocked = nil
		err := policy(req(target), []*http.Request{req("https://dthisrun.cloudfront.net/")})
		if !errors.Is(err, http.ErrUseLastResponse) || blocked == nil {
			t.Fatalf("redirect to %s must stop, got %v (blocked=%v)", target, err, blocked)
		}
	}
	if got, want := (&redirectBlockedError{host: "evil.example.com"}).Error(), "redirected to evil.example.com, which is not a preview this Task deployed"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestPollURL_StopsOnBlockedRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/steal", http.StatusFound)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)
	allowSrv := func(u *url.URL) bool { return u.Host == srvURL.Host }

	res := pollURL(context.Background(), srv.URL, "", 2*time.Second, 5*time.Millisecond, allowSrv, nil)
	if res.Live {
		t.Fatalf("expected not live, got %+v", res)
	}
	if res.StatusCode != http.StatusFound {
		t.Fatalf("expected last status 302, got %d", res.StatusCode)
	}
	if res.Attempts != 1 {
		t.Fatalf("a blocked redirect must stop polling, got %d attempts", res.Attempts)
	}
	if want := "redirected to evil.example.com, which is not a preview this Task deployed"; res.Message != want {
		t.Fatalf("message = %q, want %q", res.Message, want)
	}
}

func TestPollURL_FollowsAllowedRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/home", http.StatusFound)
	})
	mux.HandleFunc("/home", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)
	allowSrv := func(u *url.URL) bool { return u.Host == srvURL.Host }

	res := pollURL(context.Background(), srv.URL, "", 2*time.Second, 5*time.Millisecond, allowSrv, nil)
	if !res.Live || res.StatusCode != http.StatusOK {
		t.Fatalf("expected live after an allowed redirect, got %+v", res)
	}
}
