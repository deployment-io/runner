package utils

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/oauth"
)

// A refresh that keeps reporting ErrRefreshInProcess stops polling once its
// context is done, and starts no RPC after that.
func TestRefreshGitTokenForInstallationContextStopsPollingWithItsContext(t *testing.T) {
	var calls atomic.Int32
	rpc, interval := refreshGitTokenRPC, refreshGitTokenRetryInterval
	t.Cleanup(func() { refreshGitTokenRPC, refreshGitTokenRetryInterval = rpc, interval })
	refreshGitTokenRPC = func(string, string) (string, error) {
		calls.Add(1)
		return "", oauth.ErrRefreshInProcess
	}
	refreshGitTokenRetryInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := RefreshGitTokenForInstallationContext(ctx, "inst", "org")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the refresh polled for %s past its context", elapsed)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("RPC calls = %d, want 1", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("RPC calls after the context ended = %d, want 1", n)
	}
}

// Without a deadline the refresh retries ErrRefreshInProcess until it succeeds.
func TestRefreshGitTokenForInstallationRetriesWhileARefreshIsInProcess(t *testing.T) {
	var calls atomic.Int32
	rpc, interval := refreshGitTokenRPC, refreshGitTokenRetryInterval
	t.Cleanup(func() { refreshGitTokenRPC, refreshGitTokenRetryInterval = rpc, interval })
	refreshGitTokenRPC = func(string, string) (string, error) {
		if calls.Add(1) < 3 {
			return "", oauth.ErrRefreshInProcess
		}
		return "token", nil
	}
	refreshGitTokenRetryInterval = time.Millisecond
	token, err := RefreshGitTokenForInstallation("inst", "org")
	if err != nil || token != "token" || calls.Load() != 3 {
		t.Errorf("token %q, err %v, calls %d", token, err, calls.Load())
	}
}
