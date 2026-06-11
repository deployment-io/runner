package commands

import (
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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
