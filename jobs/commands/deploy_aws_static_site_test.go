package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
)

func TestResolvePublishDirectoryRefusesSymlinkedParent(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "site"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "site", "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := resolvePublishDirectory(repo, filepath.Join(repo, "link/site"))
	if err == nil {
		t.Fatalf("expected refusal, got %q", got)
	}
	if strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), repo) {
		t.Fatalf("error names a host path: %v", err)
	}
}

func TestResolvePublishDirectoryRefusesDotDot(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside", "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	repoKey, err := parameters_enums.RepoDirectoryPath.Key()
	if err != nil {
		t.Fatal(err)
	}
	publishKey, err := parameters_enums.PublishDirectory.Key()
	if err != nil {
		t.Fatal(err)
	}
	// getDistDirectory joins "../outside" onto the repo directory as-is.
	dist, err := getDistDirectory(map[string]interface{}{repoKey: repo, publishKey: "../outside"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolvePublishDirectory(repo, dist); err == nil {
		t.Fatalf("expected refusal, got %q", got)
	}
}

func TestResolvePublishDirectoryFollowsInRepoSymlink(t *testing.T) {
	repo := t.TempDir()
	build := filepath.Join(repo, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("build", filepath.Join(repo, "dist")); err != nil {
		t.Fatal(err)
	}

	got, err := resolvePublishDirectory(repo, filepath.Join(repo, "dist"))
	if err != nil {
		t.Fatalf("in-repo symlink refused: %v", err)
	}
	want, err := filepath.EvalSymlinks(build)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestResolvePublishDirectoryRefusesRepoRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(repo, "dist")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"dist", "build/.."} {
		got, err := resolvePublishDirectory(repo, filepath.Join(repo, p))
		if !errors.Is(err, errPublishDirectoryIsRepoRoot) {
			t.Fatalf("%s: expected repository root refusal, got %q, %v", p, got, err)
		}
		if strings.Contains(err.Error(), repo) {
			t.Fatalf("%s: error names a host path: %v", p, err)
		}
	}
}

func TestResolvePublishDirectoryRefusesGitDirectory(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{".git/site", "build"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/index.html", ".git/site/index.html"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".git/config"), []byte("url = https://oauth2:token@host/r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(repo, "dist")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git/site", filepath.Join(repo, "nested")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"dist", "nested", "build/../.git"} {
		got, err := resolvePublishDirectory(repo, filepath.Join(repo, p))
		if !errors.Is(err, errPublishDirectoryInGitDir) {
			t.Fatalf("%s: expected .git refusal, got %q, %v", p, got, err)
		}
		if strings.Contains(err.Error(), repo) {
			t.Fatalf("%s: error names a host path: %v", p, err)
		}
	}
}
