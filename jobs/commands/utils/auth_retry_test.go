package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/internal/gittest"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func TestIsErrorAuthenticationRequired(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bare", transport.ErrAuthenticationRequired, true},
		{"wrapped with the provider's reason", fmt.Errorf("%w: %s", transport.ErrAuthenticationRequired,
			"Invalid username or token. Password authentication is not supported for Git operations."), true},
		{"authorization failed", transport.ErrAuthorizationFailed, false},
		{"repository not found", transport.ErrRepositoryNotFound, false},
		{"unrelated", errors.New("authentication required"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsErrorAuthenticationRequired(tt.err); got != tt.want {
				t.Errorf("IsErrorAuthenticationRequired(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// Against real go-git: a provider answering 401 with a body (as GitHub always
// does) yields an error the retry recognises.
func TestCloneRepository_401WithBodyIsAuthenticationRequired(t *testing.T) {
	srv := gittest.New(t, "good-token")
	urlWithToken, err := GetRepoUrlWithToken("GitHub", "bad-token", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = CloneRepository(filepath.Join(t.TempDir(), "repo"), urlWithToken, "bad-token", "GitHub", io.Discard)
	if err == nil {
		t.Fatal("clone with a rejected token succeeded")
	}
	if !IsErrorAuthenticationRequired(err) {
		t.Fatalf("IsErrorAuthenticationRequired(%q) = false", err)
	}

	// And with the accepted token the same server serves the repository.
	urlWithToken, _ = GetRepoUrlWithToken("GitHub", "good-token", srv.URL)
	if _, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), urlWithToken, "good-token", "GitHub", io.Discard); err != nil {
		t.Fatalf("clone with the accepted token: %v", err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestGitTokenSHA256(t *testing.T) {
	if got := GitTokenSHA256(""); got != "" {
		t.Errorf(`GitTokenSHA256("") = %q, want ""`, got)
	}
	// echo -n abc | sha256sum
	if got, want := GitTokenSHA256("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("GitTokenSHA256(abc) = %q, want %q", got, want)
	}
}

// The refresh helpers send the rejected token's SHA-256 — never the token —
// and "" for a first token.
func TestRefreshGitTokenSendsTheRejectedTokensHash(t *testing.T) {
	var gotHashes []string
	restore := SwapRefreshGitTokenRPCForTest(func(installationID, orgID, rejected string) (string, error) {
		gotHashes = append(gotHashes, rejected)
		return "fresh", nil
	})
	t.Cleanup(restore)

	params := map[string]interface{}{}
	jobs.SetParameterValue(params, parameters_enums.InstallationId, "inst")
	jobs.SetParameterValue(params, parameters_enums.OrganizationIdFromJob, "org")
	if _, err := RefreshGitToken(params, "rejected-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshGitTokenForInstallation("inst", "org", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{sha256Hex("rejected-token"), ""}
	if fmt.Sprint(gotHashes) != fmt.Sprint(want) {
		t.Fatalf("hashes sent = %q, want %q", gotHashes, want)
	}
}
