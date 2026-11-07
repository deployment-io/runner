package commands

import (
	"context"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/internal/gittest"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const submoduleTestToken = "s3cret-installation-token"

// hostWithSubmodule serves app.git, whose submodule lib is ../lib.git.
func hostWithSubmodule(t *testing.T) *gittest.Host {
	t.Helper()
	h := gittest.NewHost(t)
	lib := h.AddRepo("lib.git")
	h.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: "../lib.git", Commit: lib})
	return h
}

// assertNoTokenInGitConfigs fails when .git/config or any .git/modules/**/config
// under dir holds submoduleTestToken.
func assertNoTokenInGitConfigs(t *testing.T, dir string) {
	t.Helper()
	configs := 0
	err := filepath.WalkDir(filepath.Join(dir, ".git"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "config" {
			return err
		}
		configs++
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), submoduleTestToken) {
			t.Errorf("%s holds the token:\n%s", p, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if configs < 2 {
		t.Fatalf("found %d git configs, want the repository's and its submodule's", configs)
	}
}

func hostPort(rawURL string) string {
	u, _ := url.Parse(rawURL)
	return u.Host
}

// recordBuildUpdates swaps UpdateBuildsPipeline for one that drops updates.
func recordBuildUpdates(t *testing.T) {
	t.Helper()
	original := commandUtils.UpdateBuildsPipeline
	commandUtils.UpdateBuildsPipeline, _ = goPipeline.NewPipeline(5, time.Hour, func(string, []builds.UpdateBuildDtoV1) {})
	t.Cleanup(func() {
		commandUtils.UpdateBuildsPipeline.Shutdown()
		commandUtils.UpdateBuildsPipeline = original
	})
}

// The deploy checkout of a repository directory an older runner cloned with a
// tokenized URL leaves no token in any git config the build can read.
func TestDeployCheckout_LeavesNoTokenInGitConfigs(t *testing.T) {
	h := hostWithSubmodule(t)
	recordBuildUpdates(t)
	org := filepath.Base(filepath.Dir(t.TempDir()))
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/tmp", org)) })
	params := map[string]interface{}{}
	for k, v := range map[parameters_enums.Key]string{
		parameters_enums.RepoCloneUrl:            h.URL + "/app.git",
		parameters_enums.RepoBranch:              "main",
		parameters_enums.RepoProviderToken:       submoduleTestToken,
		parameters_enums.RepoGitProvider:         "GitHub",
		parameters_enums.OrganizationIDNamespace: org,
		parameters_enums.OrganizationIdFromJob:   org,
		parameters_enums.BuildID:                 "build-1",
	} {
		if err := jobs.SetParameterValue(params, k, v); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := commandUtils.GetRepositoryDirectoryPath(params)
	if err != nil {
		t.Fatal(err)
	}
	// What the old runner left behind.
	tokenized := "https://oauth2:" + submoduleTestToken + "@" + hostPort(h.URL) + "/app.git"
	if _, err := git.PlainClone(dir, false, &git.CloneOptions{URL: tokenized, RecurseSubmodules: git.DefaultSubmoduleRecursionDepth}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&CheckoutRepository{}).Run(params, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertNoTokenInGitConfigs(t, dir)
	if _, err := os.Stat(filepath.Join(dir, "lib", "README")); err != nil {
		t.Errorf("submodule not checked out: %v", err)
	}
}

func TestTaskCheckout_LeavesNoTokenInGitConfigs(t *testing.T) {
	h := hostWithSubmodule(t)
	tc := &taskCheckout{ctx: commandUtils.TaskJobContext{OrganizationID: "org-1"}, tokenCache: map[string]string{}, logsWriter: io.Discard}
	entry := tasks.RepositoryEntry{Name: "acme/app", CloneURL: h.URL + "/app.git", BaseBranch: "main", Provider: "GitHub", InstallationID: "inst-1"}
	dir := filepath.Join(t.TempDir(), "repo")

	repository, token, err := tc.cloneWithRetry(dir, entry, submoduleTestToken)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	assertNoTokenInGitConfigs(t, dir)
	if err := tc.fetchWithRetry(repository, entry, token); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	assertNoTokenInGitConfigs(t, dir)
}

func TestSessionCheckout_LeavesNoTokenInGitConfigs(t *testing.T) {
	h := hostWithSubmodule(t)
	entry := tasks.RepositoryEntry{Name: "acme/app", CloneURL: h.URL + "/app.git", BaseBranch: "main", Provider: "GitHub", InstallationID: "inst-1"}
	req := sessionCloneRequest{entry: entry, orgID: "org-1", tokenCache: map[string]string{}, logsWriter: io.Discard}
	dir := filepath.Join(t.TempDir(), "repo")

	if _, _, err := cloneSessionWithRetry(context.Background(), dir, req, submoduleTestToken); err != nil {
		t.Fatalf("clone: %v", err)
	}
	assertNoTokenInGitConfigs(t, dir)
}

// The agent owns .git/config: pointing origin elsewhere, or rewriting the
// provider's URL with an insteadOf rule, doesn't move the push.
func TestTaskPush_GoesToTheCloneURL(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	a.AddRepo("app.git")
	b.AddRepo("app.git")
	entry := tasks.RepositoryEntry{Name: "acme/app", CloneURL: a.URL + "/app.git", BaseBranch: "main", Provider: "GitHub", InstallationID: "inst-1"}
	dir := filepath.Join(t.TempDir(), "repo")
	repository, err := commandUtils.CloneRepository(dir, entry.CloneURL, submoduleTestToken, "GitHub", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
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
	for _, args := range [][]string{
		{"remote.origin.url", b.URL + "/app.git"},
		{"url." + b.URL + "/.insteadOf", a.URL + "/"},
	} {
		cmd := exec.Command("git", append([]string{"config", "--file", filepath.Join(dir, ".git", "config")}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
	}
	repository, err = git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	fakeRefresh(t, submoduleTestToken)
	tcp := &taskCommitPush{ctx: commandUtils.TaskJobContext{OrganizationID: "org-1", BranchName: branch},
		tokenCache: map[string]string{}, logsWriter: io.Discard}

	if err := tcp.pushWithRetry(repository, entry); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := b.Requests(""); len(got) != 0 {
		t.Errorf("the other URL received %+v", got)
	}
	bare, err := git.PlainOpen(a.RepoDir("app.git"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Reference(plumbing.NewBranchReferenceName(branch), true); err != nil {
		t.Errorf("task branch not pushed to the clone URL: %v", err)
	}
}
