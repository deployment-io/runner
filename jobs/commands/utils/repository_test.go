package utils

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The mid-session repository add bounds its checkout with a 5-minute deadline
// so a stuck clone can't hold back every later turn of a live conversation.
// With the deadline already past, the clone must come back with an error
// immediately rather than hang (and without reaching the network — the HTTP
// client checks the context before it dials).
func TestCloneRepositoryWithContext_ExpiredContextReturnsInsteadOfHanging(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	dir := filepath.Join(t.TempDir(), "repo")
	done := make(chan error, 1)
	go func() {
		_, err := CloneRepositoryWithContext(ctx, dir, CloneOptions{
			CloneURLWithToken: "https://token@github.invalid/owner/repo.git",
			Token:             "token",
			Provider:          "GitHub",
			LogsWriter:        io.Discard,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an expired context must fail the clone")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the clone hung past its deadline")
	}
}

// GetSessionRepositoryDir and SessionRepositoryDir must agree: a repo added
// mid-session (which goes through the base-dir form) has to land on the same
// path a crash-recovery re-pickup clones it to (which goes through the org/job
// form). An "<owner>/<repo>" name nests, as it does today.
func TestSessionRepositoryDirMatchesTheSessionStartLayout(t *testing.T) {
	const org, job = "org1", "job1"
	base := GetSessionRepositoriesBaseDir(org, job)
	for _, name := range []string{"owner/repo", "group/sub/svc", "flat"} {
		want := GetSessionRepositoryDir(org, job, 3, name)
		if got := SessionRepositoryDir(base, 3, name); got != want {
			t.Errorf("SessionRepositoryDir(%q) = %q, want %q", name, got, want)
		}
	}
	if got := GetSessionRepositoryDir(org, job, 0, "owner/repo"); got != base+"/0-owner/repo" {
		t.Errorf("layout changed: %q", got)
	}
	// Sanity: the base dir is not something a test could clobber.
	if _, err := os.Stat(base); err == nil {
		t.Skipf("%s exists on this host", base)
	}
}
