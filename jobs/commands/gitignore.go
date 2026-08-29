package commands

import (
	"bufio"
	"os"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// Staging a Task's changes has to ignore exactly what `git add .` ignores.
// go-git does not: gitignore.ReadPatterns recurses into EVERY directory and
// collects the .gitignore of each, ignored ones included. Its matcher is
// last-match-wins, so a deeper negation beats a shallower exclusion.
//
// Git's rule is the opposite, and stronger: "It is not possible to re-include
// a file if a parent directory of that file is excluded." Git never descends
// into an excluded directory, so it never reads the .gitignore inside one.
//
// The gap is not theoretical. tailwindcss ships
// node_modules/tailwindcss/stubs/.gitignore containing `!*`. Under go-git that
// re-includes the stubs even though the repo's own .gitignore excludes
// node_modules/, so a Task on any tailwind repo committed ~1100 lines of
// vendored dependency files into its PR. Real `git check-ignore` on the same
// files reports them ignored by `.gitignore:2:node_modules/`.
//
// Fixed by handing the worktree a pattern set collected with git's semantics.
// Worktree.Excludes is appended AFTER go-git's own patterns (worktree_status.go
// excludeIgnoredChanges), and last-match-wins, so ours decides every path it
// has an opinion on — which is every path reachable through an excluded
// directory. Paths ours says nothing about fall through to go-git's set
// unchanged, so this narrows behaviour to the buggy case and leaves the rest
// alone. In particular a legitimate nested negation still works: `*.log` at the
// root does not exclude the DIRECTORY logs/, so we descend, collect
// `!important.log`, and it wins for us exactly as it does for git.

const gitignoreFileName = ".gitignore"

// gitSemanticsPatterns collects a repo's ignore patterns the way git resolves
// them: top-down, skipping the subtree under any directory the
// patterns-so-far already exclude.
//
// Order matters and is preserved — shallower patterns first, then deeper —
// because the matcher resolves by last match.
func gitSemanticsPatterns(fs billy.Filesystem) []gitignore.Pattern {
	// .git/info/exclude is repo-local and applies from the root, the same
	// place go-git reads it from.
	patterns := readIgnoreFileAt(fs, []string{".git", "info"}, "exclude", nil)
	return collectPatterns(fs, nil, patterns)
}

// collectPatterns walks dir and its subdirectories, returning inherited plus
// everything found at or below dir.
func collectPatterns(fs billy.Filesystem, dir []string, inherited []gitignore.Pattern) []gitignore.Pattern {
	patterns := readIgnoreFileAt(fs, dir, gitignoreFileName, inherited)

	entries, err := fs.ReadDir(fs.Join(dir...))
	if err != nil {
		// An unreadable directory contributes no patterns. Staging will fail
		// on its own if this matters; guessing here could only ever widen
		// what gets committed.
		return patterns
	}

	// Rebuilt per directory rather than once, because a .gitignore deeper in
	// the tree can legitimately re-include something a shallower one
	// excluded, and that must be visible when deciding whether to descend.
	matcher := gitignore.NewMatcher(patterns)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}
		child := append(append([]string{}, dir...), entry.Name())
		// The isDir=true match is the whole point: a pattern like
		// `node_modules/` excludes the DIRECTORY, and git then never looks
		// inside — so neither do we, and the .gitignore in there is never
		// read.
		if matcher.Match(child, true) {
			continue
		}
		patterns = collectPatterns(fs, child, patterns)
	}
	return patterns
}

// readIgnoreFileAt appends the patterns in dir/name to base. Missing files are
// normal — most directories have no .gitignore.
func readIgnoreFileAt(fs billy.Filesystem, dir []string, name string, base []gitignore.Pattern) []gitignore.Pattern {
	f, err := fs.Open(fs.Join(append(append([]string{}, dir...), name)...))
	if err != nil {
		if !os.IsNotExist(err) {
			return base
		}
		return base
	}
	defer f.Close()

	patterns := base
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Blank lines and comments carry no pattern. A literal '#' has to be
		// escaped as \#, which ParsePattern handles.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(line, dir))
	}
	return patterns
}
