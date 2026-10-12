//go:build unix

package agenttools

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCopyRepositoryForBuildRefusesRepositorySwappedForPipe(t *testing.T) {
	workDir, repoDir := newWorkDir(t)
	if err := os.RemoveAll(repoDir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(repoDir, 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		dst string
		err error
	}
	done := make(chan result, 1)
	logs := &syncBuffer{}
	go func() {
		dst, err := copyRepositoryForBuild(repoDir, workDir, logs)
		done <- result{dst, err}
	}()
	select {
	case r := <-done:
		defer os.RemoveAll(r.dst)
		if r.err == nil || !strings.Contains(r.err.Error(), "0-acme/web") || strings.Contains(r.err.Error(), workDir) {
			t.Fatalf("got %v, want an error naming 0-acme/web and not the host path", r.err)
		}
		if !strings.Contains(logs.String(), r.err.Error()) {
			t.Errorf("the log doesn't hold %q: %q", r.err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the copy blocked opening a repository swapped for a pipe")
	}
}
