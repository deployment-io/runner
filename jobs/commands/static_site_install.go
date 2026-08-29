package commands

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
// buildCommand is consulted only for the one case where the lockfile alone
// cannot answer: a Yarn Berry repo on the Plug'n'Play linker, where whether
// node_modules is needed depends on how the build is invoked.
//
// Precedence matches agentbox's vendor detector (pnpm, then yarn, then npm)
// so the two cannot disagree about a repo, even though the fallbacks differ:
// agentbox has pnpm and a corepack yarn shim in its image, and this build
// image has neither.
func installCommandForRepo(repoDir, buildCommand string) (command, reason string) {
	switch {
	case fileExistsAt(repoDir, "pnpm-lock.yaml"):
		// The build image (node:22-bookworm) ships no pnpm, so there is
		// nothing better to run. npm install at least produces a working
		// tree; it just won't match the lockfile.
		return npmInstall, "pnpm-lock.yaml found, but the build image has no pnpm — installing with npm (dependency versions will not match the lockfile)"

	case fileExistsAt(repoDir, "yarn.lock"):
		// A Classic lockfile is safe either way: the image's yarn reads it,
		// and so does a vendored release of any version.
		if !yarnLockIsBerryAt(repoDir) {
			return "yarn install", "yarn.lock found — installing with yarn"
		}
		// A Berry lockfile only works if the yarn that will actually run is
		// Berry. The image ships Classic and no enabled corepack, so the ONLY
		// thing that makes it Berry is an in-tree release the repo's
		// .yarnrc/.yarnrc.yml redirects to. A packageManager field alone does
		// not, however precise it looks — nothing here reads it.
		//
		// Presence of a release is not enough, and this is the trap:
		// `yarn policies set-version` on Yarn 1 vendors a CLASSIC release and
		// writes the same .yarnrc redirect, a combination that outlives a
		// migration to Berry. deployment-io/website-svc carried exactly that
		// shape until 2026-08-28. Selecting `yarn install` there sends a Berry
		// lockfile to Classic, which aborts — turning a deploy that works
		// today into one that fails. So the release's own version decides.
		release := vendoredYarnReleaseAt(repoDir)
		if release == "" {
			return npmInstall, "yarn.lock is Yarn Berry but the repo vendors no Yarn release, and the build image ships Yarn Classic — installing with npm (dependency versions will not match the lockfile)"
		}
		if yarnReleaseMajor(release) < 2 {
			return npmInstall, "yarn.lock is Yarn Berry but " + filepath.Base(release) + " is Yarn Classic, which cannot read it — installing with npm (dependency versions will not match the lockfile)"
		}
		// Berry we can actually run. One thing left to check, and it is not
		// about the install — it is about whether the BUILD can use what the
		// install produces.
		//
		// Yarn 2+ defaults to Plug'n'Play, which writes .pnp.cjs and
		// .yarn/cache and NO node_modules. A build command that goes through
		// yarn resolves fine against that; one that does not — `next build`,
		// `npm run build` — needs a real node_modules and would fail on a
		// tree that has none.
		//
		// So a PnP repo splits on the build command, and both halves are
		// live: `yarn build` is broken TODAY (npm's tree, then Berry refusing
		// it — website-svc's exact failure) and fixed by installing with
		// yarn, while `next build` works today and would break. Choosing on
		// the lockfile alone would just trade one for the other. The real
		// invariant is that the install has to produce a tree the build
		// command can use, so that is what gets asked.
		if !berryProducesNodeModules(repoDir) && !buildRunsThroughYarn(buildCommand) {
			return npmInstall, "yarn.lock is Yarn Berry on the Plug'n'Play linker, which writes no node_modules, and the build command does not run through yarn — installing with npm (dependency versions will not match the lockfile)"
		}
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

// yarnReleaseMajor returns the Yarn major version named by a vendored
// release's filename — yarn-4.9.4.cjs -> 4, yarn-1.22.1.js -> 1 — or 0 when
// the name carries none. Both `yarn set version` and `yarn policies
// set-version` write this yarn-<version>.<ext> shape.
//
// 0 means undetermined, and callers must treat that as "not Berry": guessing
// Berry for an unrecognized name would hand a Berry lockfile to a yarn that
// may not read it, which is a failed deploy. Guessing the other way only
// costs the npm fallback.
func yarnReleaseMajor(path string) int {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	rest, ok := strings.CutPrefix(name, "yarn-")
	if !ok {
		return 0
	}
	major, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// berryProducesNodeModules reports whether a Berry install will leave a real
// node_modules behind. Yarn 2+ defaults to Plug'n'Play, which does not;
// nodeLinker opts out of it. The "pnpm" linker counts too — it builds a
// node_modules out of symlinks, which is still a node_modules to anything
// resolving through it.
//
// Absent config means the default, which is PnP.
func berryProducesNodeModules(repoDir string) bool {
	switch yarnConfigValue(filepath.Join(repoDir, ".yarnrc.yml"), "nodeLinker:") {
	case "node-modules", "pnpm":
		return true
	}
	return false
}

// buildRunsThroughYarn reports whether the build command invokes yarn, and so
// resolves through whatever linker the repo uses rather than needing a real
// node_modules.
//
// Deliberately a token scan rather than shell parsing: build commands are
// user-supplied strings that may chain (`yarn content && yarn build`) or pipe,
// and the question is only ever "does yarn run at some point". Shell
// metacharacters are treated as separators so `a&&yarn build` is not read as
// one token. A false positive costs the yarn path for a repo that would also
// have worked under npm; a false negative costs the npm fallback. Neither
// fails the build, which is why a crude scan is the right amount of machinery.
func buildRunsThroughYarn(buildCommand string) bool {
	for _, token := range strings.FieldsFunc(buildCommand, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ';', '&', '|', '(', ')':
			return true
		}
		return false
	}) {
		if token == "yarn" {
			return true
		}
	}
	return false
}
