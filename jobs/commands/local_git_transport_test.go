package commands

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// The start-up bump clones the Step's checkout and moves it to the bump commit
// without running the host's git: on a runner the checkout belongs to the
// agentbox user and, on a re-run, holds the agent's .git, which host git
// refuses ("detected dubious ownership") and mustn't act on. Here no git is
// on PATH while the bump runs, so any git it ran would fail the bump.
func TestStartUpBumpRunsNoHostGit(t *testing.T) {
	h := newStartUpHarness(t)
	path := os.Getenv("PATH")
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()})
	if serr := os.Setenv("PATH", path); serr != nil {
		t.Fatal(serr)
	}
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	head := h.head(0)
	if head == h.stepTips[0] || h.gitIn(0, "rev-parse", head+"^") != h.stepTips[0] {
		t.Fatalf("HEAD %s isn't one commit on the checkout %s\nlog:\n%s", head, h.stepTips[0], h.log.String())
	}
	if status := h.gitIn(0, "status", "--porcelain"); status != "" {
		t.Errorf("the checkout isn't clean:\n%s", status)
	}
	out := h.jobOutput()
	if len(out.StartUpBumps) != 1 || out.StartUpBumps[0].State != tasks.DependencyBumpStateBumped || out.StartUpBumps[0].CommitSHA != head {
		t.Errorf("start-up bumps %+v", out.StartUpBumps)
	}
}

func TestLocalGitDir(t *testing.T) {
	workTree := t.TempDir()
	if _, err := git.PlainInit(workTree, false); err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	symlinked := t.TempDir()
	if err := os.Symlink(filepath.Join(workTree, ".git"), filepath.Join(symlinked, ".git")); err != nil {
		t.Fatal(err)
	}
	gitFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(gitFile, ".git"), []byte("gitdir: "+filepath.Join(workTree, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, want string
	}{
		{"work tree", workTree, filepath.Join(workTree, ".git")},
		{"bare", bare, bare},
		{"symlinked .git", symlinked, ""},
		{"gitdir file", gitFile, ""},
		{"no repository", t.TempDir(), ""},
	} {
		got, err := localGitDir(tc.path)
		if tc.want == "" {
			if !errors.Is(err, transport.ErrRepositoryNotFound) {
				t.Errorf("%s: (%q, %v), want ErrRepositoryNotFound", tc.name, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: (%q, %v), want %q", tc.name, got, err, tc.want)
		}
	}
}

// commitFiles writes files (path → content) in repository's work tree and
// commits them, returning the commit.
func commitFiles(t *testing.T, repository *git.Repository, files map[string]string) plumbing.Hash {
	t.Helper()
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		full := filepath.Join(worktree.Filesystem.Root(), path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(path); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := worktree.Commit("commit", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestCopyBumpCommit(t *testing.T) {
	stepDir := t.TempDir()
	step, err := git.PlainInit(stepDir, false)
	if err != nil {
		t.Fatal(err)
	}
	parent := commitFiles(t, step, map[string]string{"go.mod": "module a\n", "a/b/c.txt": "c\n", "a/keep.txt": "keep\n", "z.txt": "z\n"})
	cloneDir := t.TempDir()
	clone, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: stepDir})
	if err != nil {
		t.Fatal(err)
	}
	bump := commitFiles(t, clone, map[string]string{"go.mod": "module a\nrequire b v1\n", "a/b/c.txt": "c2\n", "a/new/d.txt": "d\n"})

	if err := copyBumpCommit(clone.Storer, step.Storer, bump, parent); err != nil {
		t.Fatalf("copyBumpCommit: %v", err)
	}
	commit, err := object.GetCommit(step.Storer, bump)
	if err != nil {
		t.Fatalf("the bump commit isn't in the checkout: %v", err)
	}
	want := map[string]string{"go.mod": "module a\nrequire b v1\n", "a/b/c.txt": "c2\n", "a/keep.txt": "keep\n", "a/new/d.txt": "d\n", "z.txt": "z\n"}
	files, err := commit.Files()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	if err := files.ForEach(func(f *object.File) error {
		content, err := f.Contents()
		got[f.Name] = content
		return err
	}); err != nil {
		t.Fatalf("reading the copied tree: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("%s = %q, want %q", name, got[name], content)
		}
	}

	// A commit that isn't one commit on parent is refused.
	if err := copyBumpCommit(clone.Storer, step.Storer, bump, bump); err == nil {
		t.Error("a commit whose parent isn't the checkout's tip was copied")
	}
}
