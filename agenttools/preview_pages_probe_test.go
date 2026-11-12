package agenttools

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const pagesPreview = "https://b-app.cloudfront.net"

// pagesProbe runs ProbeServesPages on pagesPreview against handler, with short
// waits, and returns what it found and the requests' hosts and paths.
func pagesProbe(t *testing.T, healthWait time.Duration, handler http.HandlerFunc) (PreviewPagesProbe, []string) {
	t.Helper()
	prevWait, prevInterval, prevRetry := previewPagesHealthWait, previewPagesHealthInterval, previewPagesRetryDelay
	previewPagesHealthWait, previewPagesHealthInterval, previewPagesRetryDelay = healthWait, 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		previewPagesHealthWait, previewPagesHealthInterval, previewPagesRetryDelay = prevWait, prevInterval, prevRetry
	})
	var mu sync.Mutex
	var seen []string
	probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host+r.URL.Path)
		mu.Unlock()
		handler(w, r)
	})
	got := ProbeServesPages(context.Background(), pagesPreview, pagesPreview+"/healthz")
	mu.Lock()
	defer mu.Unlock()
	return got, append([]string{}, seen...)
}

// frontPage answers the health check 200 and / with front.
func frontPage(front http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok"))
			return
		}
		front(w, r)
	}
}

func answer(status int, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
	}
}

func TestProbeServesPages_HealthThenAWebPage(t *testing.T) {
	var healthCalls int
	var accept string
	got, _ := pagesProbe(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if healthCalls++; healthCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html></html>"))
	})
	if want := (PreviewPagesProbe{ServesPages: true, Reason: "GET / answered 200 text/html"}); got != want {
		t.Errorf("probe = %+v, want %+v", got, want)
	}
	if healthCalls != 2 || accept != "text/html" {
		t.Errorf("health calls %d, Accept %q", healthCalls, accept)
	}
}

func TestProbeServesPages_Answers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front http.HandlerFunc
		want  PreviewPagesProbe
	}{
		{"a 404 page", answer(http.StatusNotFound, "text/html"), PreviewPagesProbe{Reason: "GET / answered 404 text/html"}},
		{"JSON", answer(http.StatusOK, "application/json"), PreviewPagesProbe{Reason: "GET / answered 200 application/json"}},
		{"no content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			w.WriteHeader(http.StatusNoContent)
		},
			PreviewPagesProbe{Reason: "GET / answered 204 with no content type"}},
		{"a redirect on its host", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/home", http.StatusFound)
				return
			}
			answer(http.StatusOK, "TEXT/HTML")(w, r)
		}, PreviewPagesProbe{ServesPages: true, Reason: "GET / answered 200 text/html"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := pagesProbe(t, 5*time.Second, frontPage(tc.front)); got != tc.want {
				t.Errorf("probe = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A redirect off the preview's host isn't followed: the redirect decides.
func TestProbeServesPages_RedirectToAnotherHost(t *testing.T) {
	got, seen := pagesProbe(t, 5*time.Second, frontPage(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "evil.example.com" {
			answer(http.StatusOK, "text/html")(w, r)
			return
		}
		http.Redirect(w, r, "https://evil.example.com/", http.StatusFound)
	}))
	if want := (PreviewPagesProbe{Reason: "GET / answered 302 text/html"}); got != want {
		t.Errorf("probe = %+v, want %+v", got, want)
	}
	for _, s := range seen {
		if strings.HasPrefix(s, "evil.example.com") {
			t.Errorf("requested %s", s)
		}
	}
}

// A 502, 503 or 504 is tried again, twice at most.
func TestProbeServesPages_RetriesAGatewayError(t *testing.T) {
	var calls int
	got, _ := pagesProbe(t, 5*time.Second, frontPage(func(w http.ResponseWriter, r *http.Request) {
		if calls++; calls <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		answer(http.StatusOK, "text/html")(w, r)
	}))
	if want := (PreviewPagesProbe{ServesPages: true, Reason: "GET / answered 200 text/html"}); got != want || calls != 3 {
		t.Errorf("probe = %+v after %d calls, want %+v", got, calls, want)
	}

	calls = 0
	got, _ = pagesProbe(t, 5*time.Second, frontPage(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	if want := (PreviewPagesProbe{Reason: "GET / answered 503 with no content type"}); got != want || calls != 3 {
		t.Errorf("probe = %+v after %d calls", got, calls)
	}
}

func TestProbeServesPages_HealthNeverAnswers(t *testing.T) {
	var front bool
	got, _ := pagesProbe(t, 100*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			front = true
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if want := (PreviewPagesProbe{Reason: "its health check didn't answer within 10 minutes"}); got != want || front {
		t.Errorf("probe = %+v, front page requested %v", got, front)
	}
}

// A connection that drops is tried again, and its error is the reason.
func TestProbeServesPages_ATransportError(t *testing.T) {
	var calls int
	got, _ := pagesProbe(t, 5*time.Second, frontPage(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	if got.ServesPages || !strings.HasPrefix(got.Reason, "GET / failed: ") || strings.Contains(got.Reason, "\n") || calls < 3 {
		t.Errorf("probe = %+v after %d calls", got, calls)
	}
}
