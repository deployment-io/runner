package utils

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner/internal/gittest"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

const (
	testToken = "s3cret-installation-token"
	// noCommit is a gitlink to a commit no server has.
	noCommit = "0123456789abcdef0123456789abcdef01234567"
)

func TestResolveSubmoduleURL(t *testing.T) {
	const parent = "https://github.com/acme/app.git"
	tests := []struct {
		name, parent, raw string
		wantURL           string
		wantTrusted       bool
	}{
		{"relative sibling", parent, "../lib.git", "https://github.com/acme/lib.git", true},
		{"relative child", parent, "./sub.git", "https://github.com/acme/app.git/sub.git", true},
		{".. beyond the root stays on the host", parent, "../../../../../lib.git", "https://github.com/lib.git", true},
		{"https on the parent's host", parent, "https://github.com/acme/lib.git", "https://github.com/acme/lib.git", true},
		{"host case differs", parent, "https://GitHub.com/acme/lib.git", "https://GitHub.com/acme/lib.git", true},
		{"explicit :443", parent, "https://github.com:443/acme/lib.git", "https://github.com:443/acme/lib.git", true},
		{"parent with :443", "https://github.com:443/acme/app.git", "https://github.com/acme/lib.git", "https://github.com/acme/lib.git", true},
		{"userinfo dropped", parent, "https://user:pw@github.com/acme/lib.git", "https://github.com/acme/lib.git", true},
		{"bitbucket user@ dropped from the parent", "https://arorankit@bitbucket.org/arorankit/app.git", "../lib.git",
			"https://bitbucket.org/arorankit/lib.git", true},
		{"host suffix is another host", parent, "https://github.com.evil.example/acme/lib.git", "https://github.com.evil.example/acme/lib.git", false},
		{"another port is another host", parent, "https://github.com:8443/acme/lib.git", "https://github.com:8443/acme/lib.git", false},
		{"another host", parent, "https://evil.example/lib.git", "https://evil.example/lib.git", false},
		{"userinfo dropped on another host", parent, "https://x-token-auth@evil.example/lib.git", "https://evil.example/lib.git", false},
		{"http on the parent's host", parent, "http://github.com/acme/lib.git", "http://github.com/acme/lib.git", false},
		{"scp-style is ssh", parent, "git@github.com:acme/lib.git", "ssh://github.com/acme/lib.git", false},
		{"ssh", parent, "ssh://git@github.com/acme/lib.git", "ssh://github.com/acme/lib.git", false},
		{"git protocol", parent, "git://github.com/acme/lib.git", "git://github.com/acme/lib.git", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSubmoduleURL(tt.parent, "lib", tt.raw)
			if err != nil {
				t.Fatalf("resolveSubmoduleURL(%q): %v", tt.raw, err)
			}
			if got.URL != tt.wantURL || got.Trusted != tt.wantTrusted {
				t.Errorf("resolveSubmoduleURL(%q) = %q trusted=%v, want %q trusted=%v", tt.raw, got.URL, got.Trusted, tt.wantURL, tt.wantTrusted)
			}
			if u, _ := url.Parse(got.URL); u == nil || u.Host != got.Host {
				t.Errorf("Host = %q, not the URL's host", got.Host)
			}
		})
	}
}

func TestResolveSubmoduleURL_RefusesLocalPaths(t *testing.T) {
	for _, raw := range []string{"file:///srv/lib.git", "FILE:///srv/lib.git", "/srv/lib.git", "lib.git", "libs/lib", "."} {
		_, err := resolveSubmoduleURL("https://github.com/acme/app.git", "vendor/lib", raw)
		if err == nil || err.Error() != "submodule vendor/lib: local paths and file:// URLs aren't supported" {
			t.Errorf("resolveSubmoduleURL(%q) error = %v", raw, err)
		}
	}
}

// basicToken is the Authorization header a request carrying testToken has.
func basicToken() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+testToken))
}

func assertNoAuthorization(t *testing.T, h *gittest.Host, repo string) {
	t.Helper()
	requests := h.Requests(repo)
	if len(requests) == 0 {
		t.Fatalf("%s/%s received no request", h.URL, repo)
	}
	for _, r := range requests {
		if r.Authorization != "" {
			t.Errorf("%s %s carried Authorization %q", h.URL, r.Path, r.Authorization)
		}
	}
}

func assertToken(t *testing.T, h *gittest.Host, repo string) {
	t.Helper()
	requests := h.Requests(repo)
	if len(requests) == 0 {
		t.Fatalf("%s/%s received no request", h.URL, repo)
	}
	for _, r := range requests {
		if r.Authorization != basicToken() {
			t.Errorf("%s %s carried Authorization %q, want the token", h.URL, r.Path, r.Authorization)
		}
	}
}

// assertNoTokenOnDisk fails when .git/config or any .git/modules/**/config
// under dir holds testToken.
func assertNoTokenOnDisk(t *testing.T, dir string) {
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
		if strings.Contains(string(data), testToken) {
			t.Errorf("%s holds the token:\n%s", p, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if configs == 0 {
		t.Fatal("no git config found")
	}
}

func hostOfURL(rawURL string) string {
	u, _ := url.Parse(rawURL)
	return u.Host
}

func TestCloneRepository_SubmoduleOnAnotherHostGetsNoToken(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	lib := b.AddRepo("lib.git")
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: b.URL + "/lib.git", Commit: lib})

	var logs bytes.Buffer
	if _, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), a.URL+"/app.git", testToken, "GitHub", &logs); err != nil {
		t.Fatalf("clone: %v", err)
	}
	assertToken(t, a, "app.git")
	assertNoAuthorization(t, b, "lib.git")
	want := "Submodule lib updated successfully (from " + hostOfURL(b.URL) + ", fetched without credentials because it isn't on " +
		hostOfURL(a.URL) + " over https)\n"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
	if strings.Contains(logs.String(), "lib.git") {
		t.Errorf("logs name the submodule's URL: %q", logs.String())
	}
}

func TestCloneRepository_PrivateSubmoduleOnAnotherHostFails(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: b.URL + "/missing.git", Commit: noCommit})

	_, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), a.URL+"/app.git", testToken, "GitHub", io.Discard)
	want := "error updating submodule lib: it's on " + hostOfURL(b.URL) + ", not " + hostOfURL(a.URL) +
		" over https, so it was fetched without credentials: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("clone error = %v, want %q…", err, want)
	}
	assertNoAuthorization(t, b, "missing.git")
}

func TestUpdateSubmodules_HTTPOnTheParentsHostGetsNoToken(t *testing.T) {
	h := gittest.NewPlainHost(t)
	lib := h.AddRepo("lib.git")
	h.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: h.URL + "/lib.git", Commit: lib})
	dir := filepath.Join(t.TempDir(), "repo")
	repository, err := git.PlainClone(dir, false, &git.CloneOptions{URL: h.URL + "/app.git"})
	if err != nil {
		t.Fatal(err)
	}

	parentURL := "https://" + hostOfURL(h.URL) + "/app.git"
	if err := UpdateSubmodules(repository, parentURL, "oauth2", testToken, io.Discard); err != nil {
		t.Fatalf("UpdateSubmodules: %v", err)
	}
	assertNoAuthorization(t, h, "lib.git")
}

func TestCloneRepository_SubmodulesOnTheParentsHostGetTheToken(t *testing.T) {
	a := gittest.NewHost(t)
	lib := a.AddRepo("acme/lib.git")
	rel := a.AddRepo("acme/rel.git")
	root := a.AddRepo("root.git")
	a.AddRepo("acme/app.git",
		gittest.Submodule{Path: "lib", URL: a.URL + "/acme/lib.git", Commit: lib},
		gittest.Submodule{Path: "rel", URL: "../rel.git", Commit: rel},
		gittest.Submodule{Path: "root", URL: "../../../../root.git", Commit: root},
	)

	var logs bytes.Buffer
	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := CloneRepository(dir, a.URL+"/acme/app.git", testToken, "GitHub", &logs); err != nil {
		t.Fatalf("clone: %v", err)
	}
	for _, repo := range []string{"acme/app.git", "acme/lib.git", "acme/rel.git", "root.git"} {
		assertToken(t, a, repo)
	}
	for _, p := range []string{"lib", "rel", "root"} {
		if !strings.Contains(logs.String(), "Submodule "+p+" updated successfully\n") {
			t.Errorf("logs = %q, want %s updated", logs.String(), p)
		}
		if _, err := os.Stat(filepath.Join(dir, p, "README")); err != nil {
			t.Errorf("submodule %s not checked out: %v", p, err)
		}
	}
	assertNoTokenOnDisk(t, dir)
}

func TestCloneRepository_NestedSubmodules(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	deep := b.AddRepo("deep.git")
	same := a.AddRepo("same.git")
	lib := a.AddRepo("lib.git",
		gittest.Submodule{Path: "deep", URL: b.URL + "/deep.git", Commit: deep},
		gittest.Submodule{Path: "same", URL: "../same.git", Commit: same},
	)
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: "../lib.git", Commit: lib})

	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := CloneRepository(dir, a.URL+"/app.git", testToken, "GitHub", io.Discard); err != nil {
		t.Fatalf("clone: %v", err)
	}
	assertToken(t, a, "lib.git")
	assertToken(t, a, "same.git")
	assertNoAuthorization(t, b, "deep.git")
	if _, err := os.Stat(filepath.Join(dir, "lib", "same", "README")); err != nil {
		t.Errorf("nested submodule not checked out: %v", err)
	}
	assertNoTokenOnDisk(t, dir)
}

// Below a submodule fetched without the token, nothing gets it, even back on
// the parent's host.
func TestCloneRepository_NestedUnderAnotherHostGetsNoToken(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	x := a.AddRepo("x.git")
	other := b.AddRepo("other.git",
		gittest.Submodule{Path: "x", URL: a.URL + "/x.git", Commit: x},
		gittest.Submodule{Path: "y", URL: "../y.git", Commit: b.AddRepo("y.git")},
	)
	a.AddRepo("app.git", gittest.Submodule{Path: "other", URL: b.URL + "/other.git", Commit: other})

	var logs bytes.Buffer
	if _, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), a.URL+"/app.git", testToken, "GitHub", &logs); err != nil {
		t.Fatalf("clone: %v", err)
	}
	assertNoAuthorization(t, b, "other.git")
	assertNoAuthorization(t, b, "y.git")
	assertNoAuthorization(t, a, "x.git")
	if !strings.Contains(logs.String(), "Submodule y updated successfully (from "+hostOfURL(b.URL)+
		", fetched without credentials because its parent submodule was)\n") {
		t.Errorf("logs = %q", logs.String())
	}
}

func TestCloneRepository_RefusesLocalSubmodules(t *testing.T) {
	a := gittest.NewHost(t)
	for i, raw := range []string{"file:///tmp/lib.git", "/tmp/lib.git", "lib.git"} {
		name := "app" + string(rune('0'+i)) + ".git"
		a.AddRepo(name, gittest.Submodule{Path: "lib", URL: raw, Commit: noCommit})
		_, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), a.URL+"/"+name, testToken, "GitHub", io.Discard)
		if err == nil || err.Error() != "submodule lib: local paths and file:// URLs aren't supported" {
			t.Errorf("%s: clone error = %v", raw, err)
		}
	}
}

func TestCloneFunctions_RefuseNonHTTPSCloneURL(t *testing.T) {
	const cloneURL = "http://127.0.0.1:1/acme/app.git"
	_, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), cloneURL, testToken, "GitHub", io.Discard)
	if !errors.Is(err, errCloneURLNotHTTPS) {
		t.Errorf("CloneRepository error = %v", err)
	}
	_, err = CloneRepositoryWithContext(context.Background(), filepath.Join(t.TempDir(), "repo"),
		CloneOptions{CloneURL: cloneURL, Token: testToken, Provider: "GitHub", LogsWriter: io.Discard})
	if !errors.Is(err, errCloneURLNotHTTPS) || err.Error() != "the repository's clone URL must be https" {
		t.Errorf("CloneRepositoryWithContext error = %v", err)
	}
}

// gitConfig runs `git config --file file args...`.
func gitConfig(t *testing.T, file string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"config", "--file", file}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config %v: %v\n%s", args, err, out)
	}
}

// What an earlier Step or the agent left in .git/config and
// .git/modules/<name>/config — another origin, and insteadOf rules pointing
// the real host at another — doesn't decide where the fetch goes.
func TestFetchRepository_IgnoresTheConfiguredRemotes(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	lib := a.AddRepo("lib.git")
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: "../lib.git", Commit: lib})
	b.AddRepo("lib.git")
	b.AddRepo("app.git")

	dir := filepath.Join(t.TempDir(), "repo")
	repository, err := CloneRepository(dir, a.URL+"/app.git", testToken, "GitHub", io.Discard)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	appBefore, libBefore := len(a.Requests("app.git")), len(a.Requests("lib.git"))
	for _, file := range []string{filepath.Join(dir, ".git", "config"), filepath.Join(dir, ".git", "modules", "lib", "config")} {
		gitConfig(t, file, "remote.origin.url", b.URL+"/elsewhere.git")
		gitConfig(t, file, "url."+b.URL+"/.insteadOf", a.URL+"/")
	}

	if err := FetchRepository(repository, a.URL+"/app.git", testToken, "GitHub", io.Discard); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := b.Requests(""); len(got) != 0 {
		t.Errorf("the other host received %+v", got)
	}
	if len(a.Requests("app.git")) == appBefore || len(a.Requests("lib.git")) == libBefore {
		t.Fatal("the fetch didn't reach the clone URL")
	}
	assertToken(t, a, "app.git")
	assertToken(t, a, "lib.git")

	sub, err := git.PlainOpen(filepath.Join(dir, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := sub.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Remotes["origin"].URLs; len(got) != 1 || got[0] != a.URL+"/lib.git" || len(cfg.URLs) != 0 {
		t.Errorf("submodule origin = %q, url rules = %v", got, cfg.URLs)
	}
}

// A repository directory reused from a runner that cloned with a tokenized URL
// holds the token in origin and in every module's origin; after the clone (an
// open) and ScrubRemoteToken, none is left.
func TestCloneRepository_ReusedDirectoryIsScrubbed(t *testing.T) {
	a := gittest.NewHost(t)
	lib := a.AddRepo("lib.git")
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: "../lib.git", Commit: lib})
	cloneURL := a.URL + "/app.git"
	tokenized := "https://oauth2:" + testToken + "@" + hostOfURL(a.URL) + "/app.git"

	// What the old runner did.
	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := git.PlainClone(dir, false, &git.CloneOptions{URL: tokenized, RecurseSubmodules: git.DefaultSubmoduleRecursionDepth}); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".git", "modules", "removed")
	if _, err := git.PlainInit(stale, true); err != nil {
		t.Fatal(err)
	}
	gitConfig(t, filepath.Join(stale, "config"), "remote.origin.url", "https://oauth2:"+testToken+"@example.com/removed.git")

	repository, err := CloneRepository(dir, cloneURL, testToken, "GitHub", io.Discard)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if err := ScrubRemoteToken(repository, cloneURL); err != nil {
		t.Fatalf("ScrubRemoteToken: %v", err)
	}
	assertNoTokenOnDisk(t, dir)
	if err := FetchRepository(repository, cloneURL, testToken, "GitHub", io.Discard); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	assertNoTokenOnDisk(t, dir)
}

func TestScrubRemoteToken(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror what go-git persisted into .git/config after a tokenized clone.
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://x-access-token:ghs_supersecret@github.com/owner/repo.git"},
	}); err != nil {
		t.Fatal(err)
	}

	const tokenless = "https://github.com/owner/repo.git"
	if err := ScrubRemoteToken(repo, tokenless); err != nil {
		t.Fatalf("ScrubRemoteToken: %v", err)
	}

	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Remotes["origin"].URLs; len(got) != 1 || got[0] != tokenless {
		t.Errorf("origin URLs = %q, want [%q]", got, tokenless)
	}
}

// go-git writes a remote's pre-insteadOf URLs back when a rule matched them;
// the scrub must stick anyway.
func TestScrubRemoteToken_WithAMatchingInsteadOfRule(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, ".git", "config")
	gitConfig(t, file, "remote.origin.url", "https://oauth2:"+testToken+"@github.com/owner/repo.git")
	gitConfig(t, file, "url.https://mirror.example/.insteadOf", "https://oauth2:")
	repo, err = git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := ScrubRemoteToken(repo, "https://github.com/owner/repo.git"); err != nil {
		t.Fatalf("ScrubRemoteToken: %v", err)
	}
	assertNoTokenOnDisk(t, dir)
}

// A missing origin is not an error.
func TestScrubRemoteToken_NoOrigin(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScrubRemoteToken(repo, "https://github.com/owner/repo.git"); err != nil {
		t.Errorf("expected no error with no origin, got %v", err)
	}
}

// A module directory's config is scrubbed from its raw text: parsing it
// applies insteadOf rules, which can move the token out of the userinfo.
func TestScrubRemoteToken_ModuleConfigWithAMatchingInsteadOfRule(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".git", "modules", "removed")
	if _, err := git.PlainInit(stale, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(stale, "config")
	gitConfig(t, file, "remote.origin.url", "https://oauth2:"+testToken+"@github.com/acme/lib.git")
	gitConfig(t, file, "url.https://mirror.example/.insteadOf", "https://oauth2:")

	if err := ScrubRemoteToken(repo, "https://github.com/acme/app.git"); err != nil {
		t.Fatalf("ScrubRemoteToken: %v", err)
	}
	assertNoTokenOnDisk(t, dir)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "https://github.com/acme/lib.git") {
		t.Errorf("%s lost origin's URL:\n%s", file, data)
	}
}

// A fresh clone whose submodules fail is removed, as PlainCloneContext
// removed it: a directory left behind would pass for a finished checkout.
func TestCloneRepository_FailedSubmoduleRemovesTheClone(t *testing.T) {
	a := gittest.NewHost(t)
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: "../missing.git", Commit: noCommit})

	dir := filepath.Join(t.TempDir(), "repo")
	if _, err := CloneRepository(dir, a.URL+"/app.git", testToken, "GitHub", io.Discard); err == nil {
		t.Fatal("clone succeeded, want the submodule's error")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists after the failed clone (stat: %v)", dir, err)
	}

	empty := t.TempDir()
	if _, err := CloneRepository(empty, a.URL+"/app.git", testToken, "GitHub", io.Discard); err == nil {
		t.Fatal("clone succeeded, want the submodule's error")
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("%s isn't empty after the failed clone: %v", empty, entries)
	}
}

// A 401 on a trusted submodule reaches the retry wrappers as
// ErrAuthenticationRequired, so they refresh the token.
func TestCloneRepository_SubmoduleAuthErrorIsKept(t *testing.T) {
	a := gittest.NewHost(t)
	lib := a.AddRepo("lib.git")
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: "../lib.git", Commit: lib})
	a.Unauthorized("lib.git")

	_, err := CloneRepository(filepath.Join(t.TempDir(), "repo"), a.URL+"/app.git", testToken, "GitHub", io.Discard)
	if !errors.Is(err, transport.ErrAuthenticationRequired) {
		t.Fatalf("clone error = %v, want ErrAuthenticationRequired", err)
	}
	assertToken(t, a, "lib.git")
}

// The clone's deadline bounds its submodule fetches too.
func TestCloneRepositoryWithContext_DeadlineCoversSubmodules(t *testing.T) {
	a, b := gittest.NewHost(t), gittest.NewHost(t)
	b.AddRepo("lib.git")
	b.Stall("lib.git")
	a.AddRepo("app.git", gittest.Submodule{Path: "lib", URL: b.URL + "/lib.git", Commit: noCommit})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := CloneRepositoryWithContext(ctx, filepath.Join(t.TempDir(), "repo"), CloneOptions{
			CloneURL: a.URL + "/app.git", Token: testToken, Provider: "GitHub", LogsWriter: io.Discard,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("clone succeeded, want the deadline's error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("clone outlived its deadline")
	}
	if len(b.Requests("lib.git")) == 0 {
		t.Fatal("the submodule's host received no request")
	}
}
