package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// writeRepoFiles lays out a worktree from a path→content map and inits a repo
// in it. Paths are slash-separated and may nest.
func writeRepoFiles(t *testing.T, files map[string]string) (*git.Worktree, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	return wt, dir
}

// ignored reports what the corrected pattern set decides for path, assembled
// exactly as commitAndPushOne applies it: go-git's own patterns first, ours
// appended last, resolved by last match. Asserting on the composition rather
// than on gitSemanticsPatterns alone is the point — ours only has to WIN, not
// to be consulted in isolation.
func ignored(t *testing.T, wt *git.Worktree, path string) bool {
	t.Helper()
	base, err := gitignore.ReadPatterns(wt.Filesystem, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := append(base, gitSemanticsPatterns(wt.Filesystem)...)
	return gitignore.NewMatcher(all).Match(strings.Split(path, "/"), false)
}

// TestNegatingGitignoreUnderAnExcludedDirCannotReInclude is the bug that put
// ~1100 lines of vendored tailwind into a Task's PR. tailwindcss ships
// node_modules/tailwindcss/stubs/.gitignore containing `!*`; go-git reads it
// and, being last-match-wins, re-includes files the repo's own .gitignore
// excluded. Git's rule is that a file under an excluded directory cannot be
// re-included at all.
func TestNegatingGitignoreUnderAnExcludedDirCannotReInclude(t *testing.T) {
	wt, _ := writeRepoFiles(t, map[string]string{
		".gitignore": "node_modules/\n",
		"node_modules/tailwindcss/stubs/.gitignore":     "!*\n",
		"node_modules/tailwindcss/stubs/config.full.js": "module.exports = {}\n",
		"app/page.tsx": "export default function Page() {}\n",
	})

	if !ignored(t, wt, "node_modules/tailwindcss/stubs/config.full.js") {
		t.Error("a file under an excluded directory must stay ignored — a nested " +
			"`!*` cannot re-include it, and go-git alone says otherwise")
	}
	if ignored(t, wt, "app/page.tsx") {
		t.Error("the actual source change must still be stageable")
	}
}

// A nested negation is legitimate when no ancestor DIRECTORY is excluded:
// `*.log` excludes files, not the logs/ directory, so git descends and honors
// the re-inclusion. The fix must not over-correct into ignoring it.
func TestNestedNegationStillAppliesWhenNoParentDirIsExcluded(t *testing.T) {
	wt, _ := writeRepoFiles(t, map[string]string{
		".gitignore":         "*.log\n",
		"logs/.gitignore":    "!important.log\n",
		"logs/important.log": "keep me\n",
		"logs/verbose.log":   "drop me\n",
	})

	if ignored(t, wt, "logs/important.log") {
		t.Error("a nested negation under a directory that is NOT excluded must " +
			"still re-include, exactly as git does")
	}
	if !ignored(t, wt, "logs/verbose.log") {
		t.Error("the shallower *.log exclusion must still apply to everything else")
	}
}

// Nothing ignored means nothing suppressed — the ordinary repo, where this
// machinery must be inert.
func TestPlainRepoIgnoresNothingUnexpected(t *testing.T) {
	wt, _ := writeRepoFiles(t, map[string]string{
		"main.go":    "package main\n",
		"README.md":  "# hi\n",
		".gitignore": "\n# just a comment\n\n",
	})
	for _, f := range []string{"main.go", "README.md"} {
		if ignored(t, wt, f) {
			t.Errorf("%s must be stageable in a repo that ignores nothing", f)
		}
	}
}

// An excluded directory's contents stay excluded several levels down, and a
// second excluded tree does not interfere with the first.
func TestExclusionAppliesDeepAndAcrossSiblings(t *testing.T) {
	wt, _ := writeRepoFiles(t, map[string]string{
		".gitignore":                 "node_modules/\nvendor/\n",
		"node_modules/a/b/c/deep.js": "x\n",
		"node_modules/a/.gitignore":  "!**\n",
		"vendor/pkg/.gitignore":      "!*\n",
		"vendor/pkg/lib.go":          "package pkg\n",
		"src/main.go":                "package main\n",
	})
	for _, f := range []string{"node_modules/a/b/c/deep.js", "vendor/pkg/lib.go"} {
		if !ignored(t, wt, f) {
			t.Errorf("%s is under an excluded directory and must stay ignored", f)
		}
	}
	if ignored(t, wt, "src/main.go") {
		t.Error("src/main.go must remain stageable")
	}
}
