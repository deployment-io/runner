// Package gittest serves a real git repository over HTTPS for tests, behind
// the HTTP basic auth a git provider uses: any password but the accepted
// token gets a 401 with GitHub's message, so go-git's "authentication
// required" error is produced exactly as in production.
package gittest

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// RejectedMessage is the body GitHub sends with a 401 for a bad token.
const RejectedMessage = "Invalid username or token. Password authentication is not supported for Git operations."

// Server is a git smart-HTTP server for one bare repository with a "main"
// branch holding one commit.
type Server struct {
	// URL is the repository's https clone URL, without credentials.
	URL string
	// RepoDir is the bare repository on disk.
	RepoDir string

	mu       sync.Mutex
	accepted string
	seen     []string
}

// New starts a Server accepting only acceptedToken, and points go-git's https
// transport at it for the test's duration (so tests using it must not run in
// parallel). It skips the test when the git binary is not installed.
func New(t *testing.T, acceptedToken string) *Server {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed")
	}
	root := t.TempDir()
	home := t.TempDir()
	env := []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	work := filepath.Join(root, "work")
	run(root, "init", "-q", "-b", "main", work)
	run(work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "initial")
	repoDir := filepath.Join(root, "repo.git")
	run(root, "clone", "-q", "--bare", work, repoDir)
	run(repoDir, "config", "http.receivepack", "true")

	s := &Server{RepoDir: repoDir, accepted: acceptedToken}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, env...),
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, _ := r.BasicAuth()
		s.mu.Lock()
		s.seen = append(s.seen, password)
		ok := password != "" && password == s.accepted
		s.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(RejectedMessage + "\n"))
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	client.InstallProtocol("https", githttp.NewClient(srv.Client()))
	t.Cleanup(func() { client.InstallProtocol("https", githttp.DefaultClient) })

	s.URL = srv.URL + "/repo.git"
	return s
}

// Accept changes the token the server accepts.
func (s *Server) Accept(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = token
}

// SeenTokens returns the distinct passwords presented so far, in first-seen order.
func (s *Server) SeenTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, p := range s.seen {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
