package commands

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The static-site build used to run `npm install` for every repo, whatever
// the repo actually uses. That is wrong in two ways.
//
// Loud: a repo whose .yarnrc redirects `yarn` at a vendored Yarn Berry release
// gets an npm-installed tree, and Berry validates the project against
// yarn.lock before running a script — so the BUILD fails, with an error that
// names the lockfile rather than the install:
//
//	Internal Error: <pkg>@workspace:.: This package doesn't seem to be present
//	in your lockfile; run "yarn install" to update the lockfile
//
// Quiet, and worse: npm install ignores yarn.lock and pnpm-lock.yaml entirely,
// resolving fresh from package.json ranges. Every yarn and pnpm customer has
// therefore been getting non-reproducible builds — their lockfile pins
// nothing, and a transitive release can change what ships with no commit.
// Nothing fails, so nobody reports it.
//
// Installs stay PERMISSIVE here — plain `yarn install`, not `--immutable`.
// A frozen install is the more correct end state, but switching to one would
// start failing deploys that pass today for any customer whose lockfile has
// drifted from package.json, and that reads as us breaking their site. A
// permissive yarn install still honours a satisfiable lockfile, which
// recovers most of the reproducibility, and updates it rather than aborting
// when it cannot. The frozen-install decision, and pnpm support, are a
// separate change.

// npmInstall is the fallback for every repo this cannot do better for, and is
// exactly what ran before for all of them.
const npmInstall = "npm install"

// installCommandForRepo returns the shell install command to run before the
// build, plus a one-line reason for the build log. Written to be decided on
// the runner from the cloned tree, before the container starts.
//
// Precedence matches agentbox's vendor detector (pnpm, then yarn, then npm)
// so the two cannot disagree about a repo, even though the fallbacks differ:
// agentbox has pnpm and a corepack yarn shim in its image, and this build
// image has neither.
func installCommandForRepo(repoDir string) (command, reason string) {
	switch {
	case fileExistsAt(repoDir, "pnpm-lock.yaml"):
		// The build image (node:22-bookworm) ships no pnpm, so there is
		// nothing better to run. npm install at least produces a working
		// tree; it just won't match the lockfile.
		return npmInstall, "pnpm-lock.yaml found, but the build image has no pnpm — installing with npm (dependency versions will not match the lockfile)"

	case fileExistsAt(repoDir, "yarn.lock"):
		if yarnLockIsBerryAt(repoDir) && vendoredYarnReleaseAt(repoDir) == "" {
			// Berry lockfile, nothing pinning a Berry binary. The image's
			// yarn is Classic, which cannot parse this lockfile and aborts.
			// Falling back keeps the deploy working — and the build command
			// will resolve `yarn` to that same Classic yarn, which runs
			// scripts happily against an npm tree, which is how these repos
			// have always built here.
			return npmInstall, "yarn.lock is Yarn Berry but the repo pins no Yarn version and the build image ships Yarn Classic — installing with npm (dependency versions will not match the lockfile)"
		}
		// Either a Classic lockfile the image's yarn understands, or a repo
		// that ships its own release and redirects to it. `yarn install`
		// lands on the right binary in both cases.
		return "yarn install", "yarn.lock found — installing with yarn"

	default:
		return npmInstall, ""
	}
}

// logInstallChoice writes the reason to the build log when there is something
// worth saying. Silence is the ordinary npm repo, which should not gain a line
// of narration it never had.
func logInstallChoice(logsWriter io.Writer, reason string) {
	if reason == "" {
		return
	}
	io.WriteString(logsWriter, reason+"\n")
}

func fileExistsAt(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !st.IsDir()
}

// yarnLockIsBerryAt reports whether the repo's yarn.lock was written by Yarn
// Berry. Berry lockfiles are YAML carrying a `__metadata:` block a few lines
// in; Classic's are a bespoke format headed by "# yarn lockfile v1". Only the
// head is read — lockfiles run to megabytes.
func yarnLockIsBerryAt(repoDir string) bool {
	f, err := os.Open(filepath.Join(repoDir, "yarn.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 8<<10))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "__metadata:") {
			return true
		}
	}
	return false
}

// vendoredYarnReleaseAt returns the in-tree Yarn release the repo pins, or ""
// if it pins none. .yarnrc.yml is Berry's config and .yarnrc is Classic's —
// a Berry repo commonly keeps the latter precisely so an unmigrated `yarn` on
// the PATH redirects into the vendored binary instead of failing on the
// lockfile. A yarnPath naming a file that isn't there is ignored rather than
// trusted.
func vendoredYarnReleaseAt(repoDir string) string {
	for _, cfg := range []struct{ file, key string }{
		{".yarnrc.yml", "yarnPath:"},
		{".yarnrc", "yarn-path"},
	} {
		p := yarnConfigValue(filepath.Join(repoDir, cfg.file), cfg.key)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoDir, p)
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	matches, _ := filepath.Glob(filepath.Join(repoDir, ".yarn", "releases", "*.cjs"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
			return m
		}
	}
	return ""
}

// yarnConfigValue returns the value of a top-level `key` line in a yarn config
// file — `yarnPath: .yarn/releases/yarn-4.9.4.cjs` (.yarnrc.yml) or
// `yarn-path ".yarn/releases/yarn-1.22.1.js"` (.yarnrc) — unquoted, or "" if
// the file or key is absent. Matching only at column 0 keeps nested YAML keys
// of the same name out of the result; this is deliberately not a YAML parse,
// since one key of one shape is all that is needed.
func yarnConfigValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, key) {
			continue
		}
		return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key)), `"'`)
	}
	return ""
}
