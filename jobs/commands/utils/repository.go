package utils

// TODO(tasks-v1.1): introduce a small interface (GitRepoOpener or
// similar) around CloneRepository / FetchRepository / PlainOpen so
// the task-mode clone/fetch/push paths in
// jobs/commands/checkout_repository_for_task.go,
// jobs/commands/commit_and_push.go, and the retry-on-401 logic become
// unit-testable. Today these paths are integration-only because the
// helpers call go-git directly with concrete *git.Repository return
// types.
//
// Approximate scope: ~30 LoC for the interface, ~50 LoC for a
// goGitOpener wrapper around the existing functions, ~50-100 LoC of
// refactor in taskCheckout / taskCommitPush to accept the interface,
// ~200-300 LoC of memfs-backed tests (storage.NewStorage() +
// memfs.New() to get a real *git.Repository instance without disk or
// network). The HTTPS-level auth-retry path additionally needs an
// httptest.Server fake; that's another ~100 LoC if desired but
// skippable.
//
// The deleted checkout_repository_test.go in PR #51 made real
// network calls and was the wrong approach. The memfs path is the
// right one; this TODO captures the design so the next person
// picking it up doesn't have to re-derive it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/git_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GetRepoUrlWithToken generates a repository URL with an embedded OAuth token for authentication with the given git provider.
func GetRepoUrlWithToken(gitProvider, repoProviderToken, repoCloneUrl string) (string, error) {
	switch gitProvider {
	case git_provider_enums.GitHub.String():
		after, found := strings.CutPrefix(repoCloneUrl, "https://")
		if !found {
			return "", errors.New("could not parse git provider url")
		}
		return "https://oauth2:" + repoProviderToken + "@" + after, nil
	case git_provider_enums.GitLab.String():
		after, found := strings.CutPrefix(repoCloneUrl, "https://")
		if !found {
			return "", errors.New("could not parse git provider url")
		}
		return "https://oauth2:" + repoProviderToken + "@" + after, nil
	case git_provider_enums.BitBucket.String():
		after, found := strings.CutPrefix(repoCloneUrl, "https://")
		if !found {
			return "", errors.New("could not parse git provider url")
		}
		_, after, found = strings.Cut(after, "@bitbucket.org")
		if found {
			return "https://x-token-auth:" + repoProviderToken + "@bitbucket.org" + after, nil
		}
		return "https://x-token-auth:" + repoProviderToken + "@" + after, nil

	//git clone https://x-token-auth:{access_token}@bitbucket.org/user/repo.git
	//https://arorankit@bitbucket.org/arorankit/dezyna.git
	default:
		return "", errors.New("unknown git provider type")
	}
}

// GetRepositoryDirectoryPath returns the directory path for the repository based on organization and build ID parameters.
func GetRepositoryDirectoryPath(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	buildID, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/tmp/%s/%s", organizationID, buildID), nil
}

// IsErrorAuthenticationRequired reports whether err is go-git's "authentication
// required" (an HTTP 401 from the git provider). go-git wraps it with the
// provider's message ("authentication required: Invalid username or token..."),
// so it is matched with errors.Is, not by its text.
func IsErrorAuthenticationRequired(err error) bool {
	return err != nil && errors.Is(err, transport.ErrAuthenticationRequired)
}

// FetchRepository fetches the latest changes of the provided repository from
// cloneURL (the plain clone URL the job carries) and updates its submodules.
// The fetch names its URL instead of using origin's: .git/config may have been
// changed since the clone (a Task's agent owns the repository), and go-git's
// url.<x>.insteadOf rules apply only to a remote's configured URLs, not to
// FetchOptions.RemoteURL.
func FetchRepository(repository *git.Repository, cloneURL, repoProviderToken, repoGitProvider string, logsWriter io.Writer) error {
	username := GetUsernameForProvider(repoGitProvider)

	err := repository.Fetch(&git.FetchOptions{
		Auth: &http.BasicAuth{
			Username: username,
			Password: repoProviderToken,
		},
		RemoteName: "origin",
		RemoteURL:  cloneURL,
		RefSpecs:   []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
		Force:      true,
	})

	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}

	// Update submodules
	err = UpdateSubmodules(repository, cloneURL, username, repoProviderToken, logsWriter)
	if err != nil {
		return err
	}

	return nil
}

// RefreshGitToken refreshes the Git token using provided parameters with installation ID and organization ID.
// rejectedToken is the token the git provider just rejected, or "" when asking
// for a token to start with; see RefreshGitTokenForInstallationContext.
func RefreshGitToken(parameters map[string]interface{}, rejectedToken string) (string, error) {
	installationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.InstallationId)
	if err != nil {
		return "", err
	}
	orgIdFromJob, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return "", err
	}
	return RefreshGitTokenForInstallation(installationID, orgIdFromJob, rejectedToken)
}

// RefreshGitTokenForInstallation refreshes the Git token for a specific
// installation directly. Used by Tasks-mode flows where multiple repos
// (each with its own installation) are processed inside one Job and the
// parameters-map helper's "single InstallationID" assumption doesn't hold.
// Polls every 10s on ErrRefreshInProcess (another caller holds the lock).
func RefreshGitTokenForInstallation(installationID, orgID, rejectedToken string) (string, error) {
	return RefreshGitTokenForInstallationContext(context.Background(), installationID, orgID, rejectedToken)
}

// refreshGitTokenRPC and refreshGitTokenRetryInterval are the refresh's RPC
// and its poll interval; tests replace them.
var (
	refreshGitTokenRPC = func(installationID, orgID, rejectedTokenSHA256 string) (string, error) {
		return client.Get().RefreshGitToken(installationID, orgID, rejectedTokenSHA256)
	}
	refreshGitTokenRetryInterval = 10 * time.Second
)

// SwapRefreshGitTokenRPCForTest replaces the refresh RPC (which receives the
// rejected token's SHA-256, or "") and returns a function restoring it. Only
// for tests in other packages that drive the retry-after-401 paths.
func SwapRefreshGitTokenRPCForTest(rpc func(installationID, orgID, rejectedTokenSHA256 string) (string, error)) (restore func()) {
	orig := refreshGitTokenRPC
	refreshGitTokenRPC = rpc
	return func() { refreshGitTokenRPC = orig }
}

// GitTokenSHA256 is the lower-case hex SHA-256 of token, or "" for an empty
// token. It is how a refresh request names the token the provider rejected
// without sending the token back.
func GitTokenSHA256(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RefreshGitTokenForInstallationContext is RefreshGitTokenForInstallation
// that stops polling once ctx is done and returns ctx's error. An RPC already
// in flight still runs to its end; no new one starts after ctx is done.
//
// rejectedToken is the token the git provider just rejected (a retry after a
// 401), or "" for a first token. Only its SHA-256 is sent: the server mints a
// new token when it still holds the rejected one, instead of handing it back.
func RefreshGitTokenForInstallationContext(ctx context.Context, installationID, orgID, rejectedToken string) (string, error) {
	rejectedTokenSHA256 := GitTokenSHA256(rejectedToken)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token, err := refreshGitTokenRPC(installationID, orgID, rejectedTokenSHA256)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, oauth.ErrRefreshInProcess) {
			return "", err
		}
		timer := time.NewTimer(refreshGitTokenRetryInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		}
	}
}

// errCloneURLNotHTTPS is returned for a clone URL whose scheme isn't https:
// the token is only ever sent over https.
var errCloneURLNotHTTPS = errors.New("the repository's clone URL must be https")

func requireHTTPSCloneURL(cloneURL string) error {
	u, err := url.Parse(cloneURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return errCloneURLNotHTTPS
	}
	return nil
}

// CloneRepository clones a Git repository to the specified directory path and
// updates its submodules. cloneURL is the plain clone URL (no credentials): the
// token authenticates only through the clone's Auth, so it never lands in
// .git/config. Submodules are fetched by UpdateSubmodules alone, which decides
// per submodule whether it may see the token.
// If the repository already exists locally, it opens the existing repository instead of cloning it.
func CloneRepository(repoDirectoryPath, cloneURL, repoProviderToken, repoGitProvider string, logsWriter io.Writer) (*git.Repository, error) {
	return CloneRepositoryWithContext(context.Background(), repoDirectoryPath, CloneOptions{
		CloneURL:   cloneURL,
		Token:      repoProviderToken,
		Provider:   repoGitProvider,
		LogsWriter: logsWriter,
	})
}

// CloneOptions groups what a clone needs besides its destination directory
// (Rule 2.3). CloneURL is the plain https clone URL; Token authenticates the
// clone and the submodules on the same host.
type CloneOptions struct {
	CloneURL   string
	Token      string
	Provider   string
	LogsWriter io.Writer
}

// CloneRepositoryWithContext is CloneRepository bounded by ctx: PlainCloneContext
// aborts the fetch when ctx is cancelled or its deadline passes, instead of
// hanging on a huge or unreachable repository.
//
// Only ONE caller wants a deadline — the mid-session repository add, where a
// stuck clone would hold back every later turn in a live conversation. Tasks,
// deployments and session-START clones go through CloneRepository, which
// passes a context with no deadline.
func CloneRepositoryWithContext(ctx context.Context, repoDirectoryPath string, opts CloneOptions) (*git.Repository, error) {
	if err := requireHTTPSCloneURL(opts.CloneURL); err != nil {
		return nil, err
	}
	username := GetUsernameForProvider(opts.Provider)
	cleanup, err := cloneCleanup(repoDirectoryPath)
	if err != nil {
		return nil, err
	}

	cloned := true
	repository, err := git.PlainCloneContext(ctx, repoDirectoryPath, false, &git.CloneOptions{
		URL: opts.CloneURL,
		Auth: &http.BasicAuth{
			Username: username,
			Password: opts.Token,
		},
		RecurseSubmodules: git.NoRecurseSubmodules,
	})
	if err != nil {
		if err == git.ErrRepositoryAlreadyExists {
			cloned = false
			repository, err = git.PlainOpen(repoDirectoryPath)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	if err := updateSubmodules(ctx, repository, opts.CloneURL, username, opts.Token, opts.LogsWriter); err != nil {
		// A fresh clone whose submodules failed is removed, as PlainCloneContext
		// removed it when it fetched them itself: a directory left behind would
		// pass for a finished checkout.
		if cloned {
			_ = cleanup()
		}
		return nil, err
	}
	return repository, nil
}

// cloneCleanup returns what undoes a clone into dir, as PlainCloneContext
// does on a failed clone: remove dir if it didn't exist, empty it if it was
// empty, and nothing otherwise.
func cloneCleanup(dir string) (func() error, error) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return func() error { return os.RemoveAll(dir) }, nil
	case err != nil:
		return nil, err
	case len(entries) == 0:
		return func() error {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
			return nil
		}, nil
	default:
		return func() error { return nil }, nil
	}
}

// GetUsernameForProvider returns the appropriate username for the specified Git provider.
// For BitBucket, it returns "x-token-auth"; otherwise, it returns "oauth2".
func GetUsernameForProvider(repoGitProvider string) string {
	if repoGitProvider == git_provider_enums.BitBucket.String() {
		return "x-token-auth"
	}
	return "oauth2"
}
