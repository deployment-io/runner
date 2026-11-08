package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A preview deploy grants the runner the static-site policy before it builds its
// AWS clients, with the Job's own parameters.
func TestEnsurePreviewDeployAccessGrants(t *testing.T) {
	saved := grantPreviewDeployAccess
	defer func() { grantPreviewDeployAccess = saved }()
	var got map[string]interface{}
	calls := 0
	grantPreviewDeployAccess = func(parameters map[string]interface{}) error {
		calls++
		got = parameters
		return nil
	}
	parameters := map[string]interface{}{"k": "v"}
	var logs bytes.Buffer
	ensurePreviewDeployAccess(context.Background(), parameters, &logs)
	if calls != 1 || got["k"] != "v" {
		t.Fatalf("grant called %d times with %v, want once with the Job's parameters", calls, got)
	}
	if logs.Len() != 0 {
		t.Errorf("a successful grant should log nothing, got %q", logs.String())
	}
}

// A failed grant never fails the deploy: it is logged and the deploy carries on,
// so a runner whose permissions come from elsewhere keeps working.
func TestEnsurePreviewDeployAccessFailureIsLoggedNotFatal(t *testing.T) {
	saved := grantPreviewDeployAccess
	defer func() { grantPreviewDeployAccess = saved }()
	grantPreviewDeployAccess = func(map[string]interface{}) error {
		return errors.New("AccessDenied: iam:GetRolePolicy")
	}
	var logs bytes.Buffer
	ensurePreviewDeployAccess(context.Background(), map[string]interface{}{}, &logs)
	if !strings.Contains(logs.String(), "could not grant this runner the static-site permissions") ||
		!strings.Contains(logs.String(), "AccessDenied: iam:GetRolePolicy") || !strings.Contains(logs.String(), "continuing") {
		t.Errorf("log = %q, want the failure and that the deploy continues", logs.String())
	}
}

// A grant still waiting for IAM when the agent run ends is given up within 5 s,
// and it logs nothing once it finishes.
func TestEnsurePreviewDeployAccessReturnsOnCancel(t *testing.T) {
	saved := grantPreviewDeployAccess
	defer func() { grantPreviewDeployAccess = saved }()
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	grantPreviewDeployAccess = func(map[string]interface{}) error {
		defer close(finished)
		close(started)
		<-release
		return errors.New("AccessDenied")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- ensurePreviewDeployAccess(ctx, map[string]interface{}{}, &logs) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("didn't return within 5s of its context being cancelled")
	}
	close(release)
	<-finished
	time.Sleep(10 * time.Millisecond)
	if logs.Len() != 0 {
		t.Errorf("a grant given up on logged %q", logs.String())
	}
}
