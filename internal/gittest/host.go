package gittest

import (
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Host is a git smart-HTTP server for any number of bare repositories, which
// serves every request whatever its credentials and records the Authorization
// header each one carried. A path naming no repository gets a 404.
type Host struct {
	// URL is the server's base URL (https://127.0.0.1:<port>, or http:// for
	// NewPlainHost), without a trailing slash.
	URL string

	t        *testing.T
	git      string
	env      []string
	root     string
	mu       sync.Mutex
	requests []Request
	answers  map[string]answer
	// closing is closed when the test ends, releasing stalled requests so the
	// server can shut down.
	closing chan struct{}
}

// answer is how a Host answers a repository's requests instead of serving it.
type answer int

const (
	answerUnauthorized answer = iota + 1
	answerStall
	answerRejectPushOnce
)

// Unauthorized makes every request for repository repo get a 401.
func (h *Host) Unauthorized(repo string) { h.setAnswer(repo, answerUnauthorized) }

// RejectPushOnce makes the next push request (git-receive-pack) for
// repository repo get a 401; every other request is served.
func (h *Host) RejectPushOnce(repo string) { h.setAnswer(repo, answerRejectPushOnce) }

// Stall makes every request for repository repo hang until the client gives up.
func (h *Host) Stall(repo string) { h.setAnswer(repo, answerStall) }

func (h *Host) setAnswer(repo string, a answer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.answers == nil {
		h.answers = map[string]answer{}
	}
	h.answers[repo] = a
}

func (h *Host) answerFor(path string) answer {
	for repo, a := range h.answers {
		if strings.HasPrefix(path, "/"+repo+"/") {
			return a
		}
	}
	return 0
}

// clearAnswer drops the answer for the repository path names; h.mu is held.
func (h *Host) clearAnswer(path string) {
	for repo := range h.answers {
		if strings.HasPrefix(path, "/"+repo+"/") {
			delete(h.answers, repo)
		}
	}
}

// Request is one request a Host received.
type Request struct {
	Path          string
	Authorization string
}

// Submodule is a gitlink a repository added with AddRepo records.
type Submodule struct {
	Path   string
	URL    string
	Commit string
}

// NewHost starts an https Host and points go-git's https transport at it for
// the test's duration. Every Host shares httptest's certificate, so any of
// them can install the client. Tests using it must not run in parallel. It
// skips the test when the git binary is not installed.
func NewHost(t *testing.T) *Host {
	t.Helper()
	h := newHost(t)
	srv := httptest.NewTLSServer(h.handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(h.closing) })
	client.InstallProtocol("https", githttp.NewClient(srv.Client()))
	t.Cleanup(func() { client.InstallProtocol("https", githttp.DefaultClient) })
	h.URL = srv.URL
	return h
}

// NewPlainHost starts an http Host.
func NewPlainHost(t *testing.T) *Host {
	t.Helper()
	h := newHost(t)
	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(h.closing) })
	h.URL = srv.URL
	return h
}

func newHost(t *testing.T) *Host {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed")
	}
	return &Host{
		t:       t,
		git:     gitPath,
		env:     []string{"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"},
		root:    t.TempDir(),
		closing: make(chan struct{}),
	}
}

func (h *Host) handler() http.Handler {
	backend := &cgi.Handler{
		Path: h.git,
		Args: []string{"http-backend"},
		Env:  append([]string{"GIT_PROJECT_ROOT=" + h.root, "GIT_HTTP_EXPORT_ALL=1"}, h.env...),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, Request{Path: r.URL.Path, Authorization: r.Header.Get("Authorization")})
		a := h.answerFor(r.URL.Path)
		if a == answerRejectPushOnce {
			if r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
				h.clearAnswer(r.URL.Path)
				a = answerUnauthorized
			} else {
				a = 0
			}
		}
		h.mu.Unlock()
		switch a {
		case answerUnauthorized:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case answerStall:
			select {
			case <-r.Context().Done():
			case <-h.closing:
			}
		default:
			backend.ServeHTTP(w, r)
		}
	})
}

// Requests returns the requests for repository repo (every request for
// repo == ""), in order.
func (h *Host) Requests(repo string) []Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Request
	for _, r := range h.requests {
		if repo == "" || strings.HasPrefix(r.Path, "/"+repo+"/") {
			out = append(out, r)
		}
	}
	return out
}

// AddRepo creates the bare repository <name> with one commit on "main", holding
// a README and the given submodules (a .gitmodules and their gitlinks), and
// returns the commit's hash. Its URL is h.URL + "/" + name.
func (h *Host) AddRepo(name string, submodules ...Submodule) string {
	h.t.Helper()
	work := filepath.Join(h.t.TempDir(), "work")
	h.run(h.root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README"), []byte(name+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.run(work, "add", "README")
	if len(submodules) > 0 {
		var gitmodules strings.Builder
		for _, s := range submodules {
			fmt.Fprintf(&gitmodules, "[submodule %q]\n\tpath = %s\n\turl = %s\n", s.Path, s.Path, s.URL)
			h.run(work, "update-index", "--add", "--cacheinfo", "160000,"+s.Commit+","+s.Path)
		}
		if err := os.WriteFile(filepath.Join(work, ".gitmodules"), []byte(gitmodules.String()), 0o644); err != nil {
			h.t.Fatal(err)
		}
		h.run(work, "add", ".gitmodules")
	}
	h.run(work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "initial")
	bare := filepath.Join(h.root, name)
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.run(h.root, "clone", "-q", "--bare", work, bare)
	h.run(bare, "config", "http.receivepack", "true")
	return strings.TrimSpace(h.output(work, "rev-parse", "HEAD"))
}

// Commit commits files (slash-separated path → content) onto branch of the
// repository <name>, creating branch from "main" when it doesn't exist, and
// returns the commit's hash.
func (h *Host) Commit(name, branch string, files map[string]string) string {
	h.t.Helper()
	work := filepath.Join(h.t.TempDir(), "work")
	h.run(h.root, "clone", "-q", h.RepoDir(name), work)
	if strings.TrimSpace(h.output(work, "branch", "-r", "--list", "origin/"+branch)) != "" {
		h.run(work, "checkout", "-q", branch)
	} else {
		h.run(work, "checkout", "-q", "-b", branch, "origin/main")
	}
	for path, content := range files {
		full := filepath.Join(work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.run(work, "add", "-A")
	h.run(work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "commit on "+branch)
	h.run(work, "push", "-q", "origin", branch)
	return strings.TrimSpace(h.output(work, "rev-parse", "HEAD"))
}

// Git runs git with args in the bare repository <name> and returns its
// output, trimmed.
func (h *Host) Git(name string, args ...string) string {
	h.t.Helper()
	return strings.TrimSpace(h.output(h.RepoDir(name), args...))
}

// RepoDir is the bare repository <name> on disk.
func (h *Host) RepoDir(name string) string {
	return filepath.Join(h.root, name)
}

func (h *Host) run(dir string, args ...string) {
	h.t.Helper()
	h.output(dir, args...)
}

func (h *Host) output(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command(h.git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), h.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
