package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestScrubRemoteToken verifies the installation token is removed from origin's
// URL after a session clone — the credential go-git persists into .git/config
// must not be left readable by the in-container agent.
func TestScrubRemoteToken(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror what go-git persists into .git/config after a tokenized clone.
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://x-access-token:ghs_supersecret@github.com/owner/repo.git"},
	}); err != nil {
		t.Fatal(err)
	}

	const tokenless = "https://github.com/owner/repo.git"
	if err := scrubRemoteToken(repo, tokenless); err != nil {
		t.Fatalf("scrubRemoteToken: %v", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Remotes["origin"].URLs[0]
	if got != tokenless {
		t.Errorf("origin URL = %q, want %q", got, tokenless)
	}
	if strings.Contains(got, "ghs_") || strings.Contains(got, "@") {
		t.Errorf("origin URL still carries a credential: %q", got)
	}
}

// TestScrubRemoteToken_NoOrigin is a no-op and returns no error when there is
// no origin remote to scrub.
func TestScrubRemoteToken_NoOrigin(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := scrubRemoteToken(repo, "https://github.com/owner/repo.git"); err != nil {
		t.Errorf("expected no error with no origin, got %v", err)
	}
}

// TestCheckoutSessionBaseBranch_NotDetached pins the fix for the false
// "switch out of detached HEAD" finding sessions surfaced: checking out the
// base must land HEAD on a local branch (refs/heads/<base>), not the
// remote-tracking ref (which detaches HEAD). Exercises the create-from-remote
// path — base branch name differs from the clone's default.
func TestCheckoutSessionBaseBranch_NotDetached(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0).UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the remote-tracking ref a clone leaves, for a base branch that
	// is NOT the local default ("master" from PlainInit) — the detaching case.
	if err := repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), h)); err != nil {
		t.Fatal(err)
	}

	if err := checkoutSessionBaseBranch(repo, "main"); err != nil {
		t.Fatalf("checkoutSessionBaseBranch: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if want := plumbing.NewBranchReferenceName("main"); head.Name() != want {
		t.Errorf("HEAD = %s, want %s (detached-HEAD regression)", head.Name(), want)
	}
}
