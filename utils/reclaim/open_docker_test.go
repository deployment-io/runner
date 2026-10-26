package reclaim

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeDaemon is an HTTP stand-in for dockerd that reports an API version on
// /_ping and records the query of every /build/prune it is sent.
type fakeDaemon struct {
	apiVersion string
	pingStatus int

	mu     sync.Mutex
	prunes []string
}

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_ping":
		w.Header().Set("API-Version", f.apiVersion)
		if f.pingStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.pingStatus)
			_, _ = w.Write([]byte(`{"message":"daemon unavailable"}`))
			return
		}
		_, _ = w.Write([]byte("OK"))
	case strings.HasSuffix(r.URL.Path, "/build/prune"):
		f.mu.Lock()
		f.prunes = append(f.prunes, r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"CachesDeleted":[],"SpaceReclaimed":0}`))
	default:
		http.NotFound(w, r)
	}
}

// useFakeDaemon points openDocker's FromEnv at f for the duration of a test.
func useFakeDaemon(t *testing.T, f *fakeDaemon) {
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
}

// The prune's budget travels as keep-storage up to daemon API 1.47 and as
// reserved-space after; an older daemon ignores reserved-space and prunes
// every unused record. openDocker's ping must settle the version before the
// prune, which is the first request a fresh client would otherwise make.
func TestOpenDockerNegotiatesBeforeTheFirstBuildCachePrune(t *testing.T) {
	daemon := &fakeDaemon{apiVersion: "1.44", pingStatus: http.StatusOK}
	useFakeDaemon(t, daemon)
	t.Setenv(buildCacheKeepBytesEnvVar, "2147483648") // 2 GiB

	var logs bytes.Buffer
	PruneBuildCache(&logs)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.prunes) != 1 {
		t.Fatalf("got %d prune requests, want 1 (logs: %q)", len(daemon.prunes), logs.String())
	}
	query := daemon.prunes[0]
	if !strings.Contains(query, "keep-storage=2147483648") {
		t.Errorf("prune query = %q, want keep-storage with the budget for API 1.44", query)
	}
	if strings.Contains(query, "reserved-space=") {
		t.Errorf("prune query = %q, an API 1.44 daemon ignores reserved-space", query)
	}
}

func TestOpenDockerFailsWhenThePingFails(t *testing.T) {
	useFakeDaemon(t, &fakeDaemon{apiVersion: "1.44", pingStatus: http.StatusServiceUnavailable})

	d, err := openDocker()
	if err == nil {
		_ = d.Close()
		t.Fatal("openDocker returned a client although the daemon failed the ping")
	}

	var logs bytes.Buffer
	PruneBuildCache(&logs)
	if !strings.Contains(logs.String(), "cannot reach Docker") {
		t.Errorf("expected the failed ping to be logged as an unreachable daemon, got %q", logs.String())
	}
}
