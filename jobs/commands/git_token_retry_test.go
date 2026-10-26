package commands

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/internal/gittest"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// refreshCall is one call of the refresh RPC as the runner made it.
type refreshCall struct {
	installationID      string
	rejectedTokenSHA256 string
}

// fakeRefresh replaces the refresh RPC: it records every call and answers the
// i-th call with tokens[i].
func fakeRefresh(t *testing.T, tokens ...string) *[]refreshCall {
	t.Helper()
	var mu sync.Mutex
	var calls []refreshCall
	restore := commandUtils.SwapRefreshGitTokenRPCForTest(func(installationID, orgID, rejected string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, refreshCall{installationID, rejected})
		if len(calls) > len(tokens) {
			t.Fatalf("unexpected refresh call %d", len(calls))
		}
		return tokens[len(calls)-1], nil
	})
	t.Cleanup(restore)
	return &calls
}

func assertRefreshCalls(t *testing.T, got []refreshCall, want ...refreshCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("refresh calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("refresh call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func rejected(installationID, token string) refreshCall {
	return refreshCall{installationID, commandUtils.GitTokenSHA256(token)}
}

func firstToken(installationID string) refreshCall {
	return refreshCall{installationID, ""}
}

func assertSeenTokens(t *testing.T, srv *gittest.Server, want ...string) {
	t.Helper()
	got := srv.SeenTokens()
	if len(got) != len(want) {
		t.Fatalf("tokens presented to the provider = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens presented to the provider = %q, want %q", got, want)
		}
	}
}

func deployParameters(t *testing.T, token string) map[string]interface{} {
	t.Helper()
	params := map[string]interface{}{}
	for k, v := range map[parameters_enums.Key]string{
		parameters_enums.InstallationId:        "inst-1",
		parameters_enums.OrganizationIdFromJob: "org-1",
		parameters_enums.RepoProviderToken:     token,
	} {
		if err := jobs.SetParameterValue(params, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return params
}

// cloneAccepted clones srv with the token it accepts, for tests that start
// from an existing clone.
func cloneAccepted(t *testing.T, srv *gittest.Server, token string) (*git.Repository, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	url, err := commandUtils.GetRepoUrlWithToken("GitHub", token, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := commandUtils.CloneRepository(dir, url, token, "GitHub", io.Discard)
	if err != nil {
		t.Fatalf("setup clone: %v", err)
	}
	return repository, dir
}

func TestDeployCheckout_CloneRetrySendsRejectedTokenHash(t *testing.T) {
	srv := gittest.New(t, "fresh")
	calls := fakeRefresh(t, "fresh")
	params := deployParameters(t, "stale")

	_, token, err := cloneDeployRepositoryWithRetry(params, filepath.Join(t.TempDir(), "repo"), srv.URL, "stale", "GitHub", io.Discard)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if token != "fresh" {
		t.Errorf("token = %q, want fresh", token)
	}
	assertRefreshCalls(t, *calls, rejected("inst-1", "stale"))
	assertSeenTokens(t, srv, "stale", "fresh")
	if got, _ := jobs.GetParameterValue[string](params, parameters_enums.RepoProviderToken); got != "fresh" {
		t.Errorf("RepoProviderToken parameter = %q, want fresh", got)
	}
}

func TestDeployCheckout_FetchRetrySendsRejectedTokenHash(t *testing.T) {
	srv := gittest.New(t, "first")
	repository, _ := cloneAccepted(t, srv, "first")
	srv.Accept("second")
	calls := fakeRefresh(t, "second")
	params := deployParameters(t, "first")

	token, err := fetchDeployRepositoryWithRetry(params, repository, "first", "GitHub", io.Discard)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if token != "second" {
		t.Errorf("token = %q, want second", token)
	}
	assertRefreshCalls(t, *calls, rejected("inst-1", "first"))
	if got, _ := jobs.GetParameterValue[string](params, parameters_enums.RepoProviderToken); got != "second" {
		t.Errorf("RepoProviderToken parameter = %q, want second", got)
	}
}

func taskEntry(srv *gittest.Server) tasks.RepositoryEntry {
	return tasks.RepositoryEntry{Name: "acme/api", CloneURL: srv.URL, BaseBranch: "main", Provider: "GitHub", InstallationID: "inst-1"}
}

func TestTaskCheckout_CloneRetrySendsRejectedTokenHash(t *testing.T) {
	srv := gittest.New(t, "fresh")
	calls := fakeRefresh(t, "stale", "fresh")
	tc := &taskCheckout{ctx: commandUtils.TaskJobContext{OrganizationID: "org-1"}, tokenCache: map[string]string{}, logsWriter: io.Discard}
	entry := taskEntry(srv)

	token, err := tc.getToken(entry.InstallationID)
	if err != nil || token != "stale" {
		t.Fatalf("getToken = %q, %v", token, err)
	}
	_, token, err = tc.cloneWithRetry(filepath.Join(t.TempDir(), "repo"), entry, token)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if token != "fresh" || tc.tokenCache["inst-1"] != "fresh" {
		t.Errorf("token = %q, cache = %q; want fresh", token, tc.tokenCache["inst-1"])
	}
	assertRefreshCalls(t, *calls, firstToken("inst-1"), rejected("inst-1", "stale"))
	assertSeenTokens(t, srv, "stale", "fresh")
}

func TestTaskCheckout_FetchRetrySendsRejectedTokenHash(t *testing.T) {
	srv := gittest.New(t, "first")
	repository, _ := cloneAccepted(t, srv, "first")
	srv.Accept("second")
	calls := fakeRefresh(t, "second")
	tc := &taskCheckout{ctx: commandUtils.TaskJobContext{OrganizationID: "org-1"}, tokenCache: map[string]string{}, logsWriter: io.Discard}

	if err := tc.fetchWithRetry(repository, taskEntry(srv), "first"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	assertRefreshCalls(t, *calls, rejected("inst-1", "first"))
	if tc.tokenCache["inst-1"] != "second" {
		t.Errorf("cache = %q, want second", tc.tokenCache["inst-1"])
	}
}

func TestSessionClone_RetrySendsRejectedTokenHash(t *testing.T) {
	srv := gittest.New(t, "fresh")
	calls := fakeRefresh(t, "stale", "fresh")
	cache := map[string]string{}
	req := sessionCloneRequest{entry: taskEntry(srv), orgID: "org-1", tokenCache: cache, logsWriter: io.Discard}

	token, err := sessionToken(cache, "inst-1", "org-1")
	if err != nil || token != "stale" {
		t.Fatalf("sessionToken = %q, %v", token, err)
	}
	_, token, err = cloneSessionWithRetry(context.Background(), filepath.Join(t.TempDir(), "repo"), req, token)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if token != "fresh" || cache["inst-1"] != "fresh" {
		t.Errorf("token = %q, cache = %q; want fresh", token, cache["inst-1"])
	}
	assertRefreshCalls(t, *calls, firstToken("inst-1"), rejected("inst-1", "stale"))
}

func TestTaskPush_RetrySendsRejectedTokenHash(t *testing.T) {
	srv := gittest.New(t, "setup")
	repository, _ := cloneAccepted(t, srv, "setup")
	const branch = "deployment-io/task-1"
	wt, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("task work", &git.CommitOptions{AllowEmptyCommits: true,
		Author: &object.Signature{Name: "test", Email: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}
	srv.Accept("fresh")
	calls := fakeRefresh(t, "stale", "fresh")
	tcp := &taskCommitPush{ctx: commandUtils.TaskJobContext{OrganizationID: "org-1", BranchName: branch},
		tokenCache: map[string]string{}, logsWriter: io.Discard}

	if err := tcp.pushWithRetry(repository, taskEntry(srv)); err != nil {
		t.Fatalf("push: %v", err)
	}
	assertRefreshCalls(t, *calls, firstToken("inst-1"), rejected("inst-1", "stale"))
	if tcp.tokenCache["inst-1"] != "fresh" {
		t.Errorf("cache = %q, want fresh", tcp.tokenCache["inst-1"])
	}
	bare, err := git.PlainOpen(srv.RepoDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Reference(plumbing.NewBranchReferenceName(branch), true); err != nil {
		t.Errorf("task branch not pushed: %v", err)
	}
}

// First-token callers ask for a token without naming a rejected one, so the
// server never mints for them.
func TestFirstTokenCallersSendNoRejectedToken(t *testing.T) {
	calls := fakeRefresh(t, "a", "b", "c", "d", "e")
	ctx := commandUtils.TaskJobContext{OrganizationID: "org-1",
		Entries: []tasks.RepositoryEntry{{InstallationID: "inst-v"}}}

	tc := &taskCheckout{ctx: ctx, tokenCache: map[string]string{}}
	if _, err := tc.getToken("inst-tc"); err != nil {
		t.Fatal(err)
	}
	tcp := &taskCommitPush{ctx: ctx, tokenCache: map[string]string{}}
	if _, err := tcp.getToken("inst-tcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := tcp.getTokenContext(context.Background(), "inst-tcp-ctx"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionToken(map[string]string{}, "inst-s", "org-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := vendorGitToken(ctx); err != nil {
		t.Fatal(err)
	}
	assertRefreshCalls(t, *calls, firstToken("inst-tc"), firstToken("inst-tcp"), firstToken("inst-tcp-ctx"),
		firstToken("inst-s"), firstToken("inst-v"))
}
