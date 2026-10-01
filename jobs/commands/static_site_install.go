package commands

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The static-site build's JS toolchain is the REPO's, not whatever the build
// image happens to ship.
//
// #115 took the first step: pick `npm install` vs `yarn install` from the
// lockfile instead of always running npm. It left three gaps, which are one
// problem seen from three sides, and this file closes them together:
//
//  1. package.json's `packageManager` is honoured, via corepack. Nothing read
//     it before, so a repo pinning yarn@4.9.4 without vendoring a release fell
//     back to npm and its lockfile was ignored. Node 22 bundles corepack (it
//     ships with every official release from 14.19/16.9 — yarnpkg.com/corepack),
//     the build container has a writable rootfs and unproxied network, so the
//     two things that make corepack awkward inside agentbox — a read-only
//     COREPACK_HOME and a proxy allowlist — do not apply here.
//
//     Only the shim we need is enabled, never bare `corepack enable`: that also
//     installs an npm shim, and a corepack npm shim REFUSES to run in a repo
//     whose packageManager names yarn. agentbox's Dockerfile documents the same
//     trap.
//
//     Corepack fetches the manager it is asked for at install time: pnpm from
//     the npm registry, yarn from repo.yarnpkg.com. The build container talks
//     to the internet directly, so both are reachable; a future egress
//     allowlist in front of these builds has to carry repo.yarnpkg.com or
//     every yarn pin falls back to npm.
//
//  2. pnpm actually runs. A pnpm repo used to get `npm install` because the
//     image has no pnpm; `corepack enable pnpm` is the same mechanism as (1),
//     so it lands in the same change rather than a second one.
//
//  3. Installs are frozen — with one escape hatch, described below.
//
// THE FROZEN-INSTALL DECISION, stated plainly because it is the part that can
// break customers. A frozen install (`npm ci`, `yarn --immutable`,
// `pnpm --frozen-lockfile`) is what makes a build reproducible, but it FAILS
// where a permissive one succeeded for anyone whose lockfile has drifted from
// package.json — and to that customer it looks like we broke their site.
//
// So the install attempts frozen and, on a lockfile-drift failure SPECIFICALLY,
// falls back to a permissive install with a loud log naming the drift and the
// fix (see frozenWithDriftFallback). Every other failure — network, a failing
// postinstall, a corrupt lockfile — is left exactly as it lands: no second
// install runs, and the frozen install's own error is what the build log shows.
// Swallowing those is what the fallback must not do; a permissive retry that
// "recovers" from an unrelated failure is how a broken install becomes a silent
// one again.
//
// Reproducibility therefore arrives for every repo whose lockfile is honest,
// and no deploy that passes today starts failing because of it.
//
// TWO RISKS THIS CARRIES, both narrowed rather than assumed away:
//
//   - Yarn Berry does not read .npmrc; it wants npmScopes/npmAuthToken in
//     .yarnrc.yml. A repo whose private-registry auth lives only in .npmrc
//     installs fine under npm today and would fail under Berry. That only
//     bites repos this change newly routes into Berry (a packageManager pin
//     with no vendored release), so those are the only ones checked — see
//     npmrcCarriesRegistryConfig. A repo that already ran Berry via a vendored
//     release is left alone; its behaviour is not ours to change here. Yarn
//     Classic does read .npmrc, so classic repos are unaffected either way.
//
//   - The build image must ship what is selected. `yarn install` has always
//     assumed node:22-bookworm carries Yarn Classic, and this file now also
//     assumes it carries corepack. Both are true; neither is guaranteed by
//     anything but the tag. The coupling is called out at the imageId constant
//     in build_static_site.go, and every corepack path degrades through the
//     `else` branch of a `corepack enable` test rather than exploding, so an
//     image that drops corepack loses reproducibility loudly instead of
//     failing every yarn and pnpm deploy.

// The install commands, permissive and frozen, per manager.
//
// The frozen flags were checked against primary docs rather than memory,
// because a wrong one turns a reproducible install into a silently permissive
// one:
//
//   - npm ci: fails when package.json and package-lock.json are out of sync.
//   - yarn Berry: `--immutable`, "Abort with an error exit code if the lockfile
//     was to be modified" (yarnpkg.com/cli/install). It also accepts
//     `--frozen-lockfile` as a deprecated alias — not used here, since the
//     alias is documented as going away.
//   - yarn Classic: `--frozen-lockfile`, "Don't generate a yarn.lock lockfile
//     and fail if an update is needed" (classic.yarnpkg.com/lang/en/docs/cli/install).
//     Classic knows nothing of `--immutable` and does not error on it. Run
//     against yarn 1.22.22 with a deliberately drifted lockfile, `--immutable`
//     exits 0 and saves the rewritten lockfile, while `--frozen-lockfile`
//     exits 1 — so swapping the two flags loses reproducibility silently
//     rather than loudly. The Classic/Berry split is why yarn's flag is chosen
//     from the version that will actually run and never from the lockfile
//     alone.
//   - pnpm: `--frozen-lockfile`, "pnpm fails to install if the lockfile is out
//     of sync with the manifest" (pnpm.io/cli/install).
const (
	npmInstall  = "npm install"
	npmFrozen   = "npm ci"
	yarnInstall = "yarn install"
	// Classic and Berry disagree — see above. Never interchange these.
	yarnClassicFrozen = "yarn install --frozen-lockfile"
	yarnBerryFrozen   = "yarn install --immutable"
	pnpmInstall       = "pnpm install"
	pnpmFrozen        = "pnpm install --frozen-lockfile"
)

// What each manager says when, and ONLY when, the frozen install failed
// because the lockfile no longer matches package.json. These strings are the
// entire basis for telling drift apart from every other install failure, so
// each is the stable, documented part of the message rather than the whole of
// it:
//
//   - npm prints "`npm ci` can only install packages when your package.json
//     and package-lock.json or npm-shrinkwrap.json are in sync."
//   - yarn Classic prints "Your lockfile needs to be updated, but yarn was run
//     with `--frozen-lockfile`."
//   - yarn Berry prints error code YN0028, FROZEN_LOCKFILE_EXCEPTION, "Your
//     lockfile would be modified if Yarn was to finish the install"
//     (yarnpkg.com/advanced/error-codes).
//   - pnpm prints ERR_PNPM_OUTDATED_LOCKFILE (pnpm.io/errors). Matching the
//     bare OUTDATED_LOCKFILE suffix also catches
//     ERR_PNPM_FROZEN_LOCKFILE_WITH_OUTDATED_LOCKFILE, which newer pnpm raises
//     for the same situation when the packageManager pin is what drifted.
//
// Three of the four were produced by drifting a real lockfile and running the
// real binary — npm 10.9.8, pnpm 12.3.4 (whose message text has changed but
// still carries the error code on its first line), yarn 1.22.22. Berry's is
// from its documented error-code table; nothing here could reach
// repo.yarnpkg.com to run it.
//
// A marker that stops matching costs reproducibility-preserving behaviour in
// the safe direction: the drift fails the build with pnpm's or yarn's own
// message instead of falling back. It cannot cause a silent permissive install.
const (
	npmDriftMarker         = "can only install packages when your package.json and package-lock.json"
	yarnClassicDriftMarker = "lockfile needs to be updated"
	yarnBerryDriftMarker   = "YN0028"
	pnpmDriftMarker        = "OUTDATED_LOCKFILE"
)

// installLogPath is where the frozen install's output is teed inside the build
// container so the drift check can read it back. /tmp, not the repo: the repo
// dir is a bind mount of the runner's clone, and a stray file there could end
// up in the deploy artifact.
const installLogPath = "/tmp/deployment-io-install.log"

// installPlan is the decision: what runs the install, what to enable first,
// and what to do when the lockfile turns out to have drifted.
type installPlan struct {
	// manager is what actually installs: "npm", "yarn" or "pnpm".
	manager string
	// lockfile is the file the frozen install validates against, named in the
	// drift message so the log says which file to commit.
	lockfile string
	// corepack is the single shim to enable before installing ("yarn" or
	// "pnpm"), or "" to use the binary the image ships.
	corepack string
	// noCorepack is what to run if that shim cannot be enabled — always the
	// choice this code would have made without corepack at all.
	noCorepack string
	// frozen is the install that fails rather than update the lockfile, or ""
	// when there is nothing to freeze against.
	frozen string
	// drift identifies "the lockfile is out of sync" in frozen's output, and
	// nothing else.
	drift string
	// permissive is the install that updates the lockfile instead of failing.
	permissive string
	// reason is the one line for the build log. Empty only for the ordinary
	// npm repo, which should gain no narration it never had.
	reason string
}

// installCommandForRepo returns the shell install command to run before the
// build, plus a one-line reason for the build log. Written to be decided on
// the runner from the cloned tree, before the container starts; the container
// only runs the result.
//
// buildCommand is consulted only for the one case where the tree alone cannot
// answer: a Yarn Berry repo on the Plug'n'Play linker, where whether
// node_modules is needed depends on how the build is invoked.
func installCommandForRepo(repoDir, buildCommand string) (command, reason string) {
	p := planInstall(repoDir, buildCommand)
	return p.shell(), p.reason
}

// planInstall decides what to install with. Manager and version come from
// resolveToolchain, which is the half that must agree with agentbox; the rest
// is this build's own problem — a customer-supplied build command has to be
// able to use the tree that comes out.
func planInstall(repoDir, buildCommand string) installPlan {
	tc := resolveToolchain(repoDir)
	switch tc.manager {
	case "pnpm":
		return pnpmPlan(repoDir, tc)
	case "yarn":
		return yarnPlan(repoDir, buildCommand, tc)
	default:
		return npmPlan(repoDir, "")
	}
}

// toolchain is the package manager a repo asks for and the version of it that
// will actually run. Deliberately a pure function of the tree: this is the
// resolution that must match agentbox's for the same repo, and agentbox has no
// build command to consult. See TestToolchainParityWithAgentbox.
type toolchain struct {
	// manager is "npm", "yarn" or "pnpm".
	manager string
	// version is what the repo pins, or "" when it pins nothing and whatever
	// the runtime provides will run.
	version string
	// major is version's major, or the runtime default's when unpinned — 1 for
	// yarn, since both images provide Yarn Classic. 0 means undetermined.
	major int
	// corepack is true when version comes from packageManager and a corepack
	// shim is what will honour it.
	corepack bool
	// source names where the choice came from, for the build log.
	source string
	// conflict names the manager packageManager pins when that is NOT the
	// manager the lockfile selects. Corepack refuses to run any manager other
	// than the pinned one, so a conflict rules corepack out entirely.
	conflict string
}

// resolveToolchain picks the manager from the lockfile — the record of what
// actually resolved the tree — and the version from what will run it.
//
// Precedence (pnpm, then yarn, then npm) matches agentbox's vendor detector so
// the two cannot disagree about a repo.
func resolveToolchain(repoDir string) toolchain {
	pinned, pinnedVersion := packageManagerPin(repoDir)
	switch {
	case fileExistsAt(repoDir, "pnpm-lock.yaml"):
		tc := toolchain{manager: "pnpm", corepack: true, source: "pnpm-lock.yaml"}
		switch pinned {
		case "pnpm":
			tc.version, tc.major = pinnedVersion, majorOf(pinnedVersion)
			tc.source = "pinned by package.json's packageManager"
		case "":
		default:
			tc.conflict = pinned
		}
		return tc

	case fileExistsAt(repoDir, "yarn.lock"):
		return resolveYarn(repoDir, pinned, pinnedVersion)

	default:
		// npm is the only manager neither image can pin: a corepack npm shim
		// refuses to run in a repo pinned to yarn or pnpm, so neither side
		// enables one and both run the image's npm. packageManager's npm
		// version is therefore honoured by neither — deliberately, and the
		// same way on both sides.
		return toolchain{manager: "npm", source: "the build image's npm"}
	}
}

// resolveYarn decides WHICH yarn runs, which is the whole question for a yarn
// repo: Classic cannot read a Berry lockfile, and the frozen-install flag
// differs between them.
//
// A vendored release wins over a packageManager pin because it wins at
// runtime: .yarnrc/.yarnrc.yml redirect whatever yarn starts into the in-tree
// binary. Presence of a release is not enough and this is the trap — `yarn
// policies set-version` on Yarn 1 vendors a CLASSIC release and writes the
// same redirect, a combination that outlives a migration to Berry
// (deployment-io/website-svc carried exactly that shape until 2026-08-28) — so
// the release's own version decides, never the fact that it exists.
func resolveYarn(repoDir, pinned, pinnedVersion string) toolchain {
	if release := vendoredYarnReleaseAt(repoDir); release != "" {
		version := yarnReleaseVersion(release)
		return toolchain{
			manager: "yarn",
			version: version,
			major:   majorOf(version),
			source:  "the vendored release " + filepath.Base(release),
		}
	}
	if pinned == "yarn" {
		return toolchain{
			manager:  "yarn",
			version:  pinnedVersion,
			major:    majorOf(pinnedVersion),
			corepack: true,
			source:   "pinned by package.json's packageManager",
		}
	}
	tc := toolchain{manager: "yarn", major: 1, source: "the build image's yarn"}
	if pinned != "" {
		tc.conflict = pinned
	}
	return tc
}

// pnpmPlan installs with pnpm, which exists only because corepack fetches it —
// the image ships none.
//
// A repo that pins no version still gets pnpm: corepack's shim falls back to
// its own default, which is currently well ahead of most committed lockfiles.
// That skew was the obvious way for this to go wrong, so it was tried — pnpm
// 12.3.4 installed a lockfileVersion 9.0 lockfile under --frozen-lockfile and
// reported it up to date. pnpm reads older lockfiles; it is writing a newer one
// that would be a change, and a frozen install writes nothing.
func pnpmPlan(repoDir string, tc toolchain) installPlan {
	if tc.conflict != "" {
		// Corepack refuses to run pnpm in a project whose packageManager names
		// something else, so there is no pnpm to be had. npm keeps the deploy
		// alive, exactly as it did before this change.
		return npmPlan(repoDir, "pnpm-lock.yaml found, but package.json's packageManager names "+tc.conflict+
			" — corepack will not run pnpm in a project pinned to another manager, so installing with npm "+
			"(dependency versions will not match the lockfile)")
	}
	return installPlan{
		manager:    "pnpm",
		lockfile:   "pnpm-lock.yaml",
		corepack:   "pnpm",
		noCorepack: npmInstall,
		frozen:     pnpmFrozen,
		drift:      pnpmDriftMarker,
		permissive: pnpmInstall,
		reason:     "pnpm-lock.yaml found — installing with " + managerPhrase("pnpm", tc),
	}
}

// yarnPlan installs with yarn when the yarn that will run can read the repo's
// lockfile AND leaves behind a tree the build command can use. It falls back to
// npm — loudly — when either is false, which is what happened before this
// change for every one of these shapes.
func yarnPlan(repoDir, buildCommand string, tc toolchain) installPlan {
	berryLock := yarnLockIsBerryAt(repoDir)
	if tc.major < 2 {
		// major 0 is an unrecognized release filename: undetermined, which must
		// mean "not Berry". Guessing Berry hands a Berry lockfile to a yarn
		// that may not read it — a failed deploy. Guessing the other way costs
		// only the npm fallback.
		if berryLock {
			return npmPlan(repoDir, "yarn.lock is a Yarn Berry lockfile but the yarn that will run is "+managerPhrase("yarn", tc)+
				", which cannot read it — installing with npm (dependency versions will not match the lockfile)")
		}
		return yarnClassicPlan(tc)
	}
	if tc.corepack && npmrcCarriesRegistryConfig(repoDir) && !yarnrcCarriesRegistryConfig(repoDir) {
		// Newly routed into Berry by a packageManager pin, and Berry does not
		// read .npmrc — see the header. This repo installs today because npm
		// reads it, and would start failing to resolve its private packages.
		return npmPlan(repoDir, "this build would install with "+managerPhrase("Yarn Berry", tc)+", but .npmrc carries registry configuration that "+
			"Yarn Berry does not read (it wants npmScopes/npmAuthToken in .yarnrc.yml) — installing with npm "+
			"(dependency versions will not match the lockfile). Move the registry settings to .yarnrc.yml to install with yarn")
	}
	// Yarn 2+ defaults to Plug'n'Play, which writes .pnp.cjs and .yarn/cache
	// and NO node_modules. A build command that goes through yarn resolves
	// against that fine; one that does not — `next build`, `npm run build` —
	// needs a real node_modules. The install has to produce a tree the build
	// command can use, so that is what gets asked. This is #115's rule, and it
	// now applies to any repo Berry will install, not only Berry-lockfile ones.
	if !berryProducesNodeModules(repoDir) && !buildRunsThroughYarn(buildCommand) {
		return npmPlan(repoDir, "yarn.lock would be installed by "+managerPhrase("yarn", tc)+
			" on the Plug'n'Play linker, which writes no node_modules, and the build command does not run through yarn"+
			" — installing with npm (dependency versions will not match the lockfile)")
	}
	p := installPlan{
		manager:    "yarn",
		lockfile:   "yarn.lock",
		noCorepack: npmInstall,
		frozen:     yarnBerryFrozen,
		drift:      yarnBerryDriftMarker,
		permissive: yarnInstall,
		reason:     "yarn.lock found — installing with " + managerPhrase("Yarn Berry", tc),
	}
	if tc.corepack {
		p.corepack = "yarn"
	}
	if !berryLock {
		// Berry rewrites a Classic lockfile into its own format as part of
		// installing, so a frozen install would abort on a repo that is merely
		// mid-migration rather than drifted. Permissive is the only thing that
		// works here, and the log says so rather than implying reproducibility
		// this install does not have.
		p.frozen, p.drift = "", ""
		p.reason = "yarn.lock is a Yarn Classic lockfile but " + managerPhrase("yarn", tc) + " is Yarn Berry, which rewrites it on install" +
			" — installing permissively with yarn (a frozen install would abort on the rewrite)"
		p.noCorepack = yarnClassicFrozen
	}
	return p
}

// yarnClassicPlan is the Yarn 1 repo: a Classic lockfile and a Classic yarn,
// whether that is the image's or a version corepack fetches for a
// packageManager pin.
func yarnClassicPlan(tc toolchain) installPlan {
	p := installPlan{
		manager:    "yarn",
		lockfile:   "yarn.lock",
		noCorepack: yarnClassicFrozen,
		frozen:     yarnClassicFrozen,
		drift:      yarnClassicDriftMarker,
		permissive: yarnInstall,
		reason:     "yarn.lock found — installing with " + managerPhrase("Yarn Classic", tc),
	}
	if tc.corepack {
		p.corepack = "yarn"
	}
	return p
}

// npmPlan is the fallback for every repo the others cannot do better for, and
// for the ordinary npm repo it is what ran before all of this. reason is empty
// for that ordinary case and set for every fallback — a deploy that succeeds
// while silently ignoring a lockfile is the failure mode that hid all of this.
//
// npm ci is only possible with a lockfile to install from; without one it
// refuses outright, and the yarn and pnpm repos that land here through a
// fallback usually have none.
func npmPlan(repoDir, reason string) installPlan {
	p := installPlan{manager: "npm", permissive: npmInstall, reason: reason}
	switch {
	case fileExistsAt(repoDir, "npm-shrinkwrap.json"):
		p.lockfile, p.frozen, p.drift = "npm-shrinkwrap.json", npmFrozen, npmDriftMarker
	case fileExistsAt(repoDir, "package-lock.json"):
		p.lockfile, p.frozen, p.drift = "package-lock.json", npmFrozen, npmDriftMarker
	}
	return p
}

// managerPhrase names the manager, the version when the repo pins one, and
// where that came from. Every build-log line about the toolchain goes through
// it, so a customer reading the log can tell a pin from a default without
// knowing this code exists.
func managerPhrase(manager string, tc toolchain) string {
	switch {
	case tc.version == "" && tc.corepack:
		return manager + " via corepack (package.json pins no version, so corepack's default runs)"
	case tc.version == "":
		return manager + " (" + tc.source + ")"
	case tc.corepack:
		return manager + " " + tc.version + " via corepack (" + tc.source + ")"
	default:
		return manager + " " + tc.version + " (" + tc.source + ")"
	}
}

// shell renders the plan as the single command string the build container runs
// before the build command.
func (p installPlan) shell() string {
	install := p.permissive
	if p.frozen != "" {
		install = frozenWithDriftFallback(p.frozen, p.drift, p.permissive, p.lockfile)
	}
	if p.corepack == "" {
		return install
	}
	// COREPACK_ENABLE_DOWNLOAD_PROMPT=0 keeps corepack from printing its
	// "about to download" prompt, which it does by default when a shim is
	// invoked implicitly. Enabling the shim is a test, not an assumption: if
	// this image has no corepack, or nowhere writable to put the shim, the
	// deploy falls back to what it did before corepack existed and says so.
	return "export COREPACK_ENABLE_DOWNLOAD_PROMPT=0\n" +
		"if corepack enable " + p.corepack + " >/dev/null 2>&1; then\n" +
		install + "\n" +
		"else\n" +
		"echo " + shellQuote("corepack is not available in this build image, so the "+p.corepack+
		" this repo asks for cannot be fetched — falling back to `"+p.noCorepack+
		"` (dependency versions may not match "+p.lockfile+")") + "\n" +
		p.noCorepack + "\n" +
		"fi"
}

// frozenWithDriftFallback renders "install frozen; if and only if that failed
// because the lockfile has drifted from package.json, say so loudly and redo it
// permissively".
//
// The drift check reads the frozen install's own output, teed so the build log
// still streams it live. Any other failure falls straight through: nothing else
// runs, nothing is retried, and the frozen install's error is what the customer
// sees. pipefail is scoped to a subshell so it applies to the tee pipeline and
// not to the user's build command, which is appended to this string.
func frozenWithDriftFallback(frozen, drift, permissive, lockfile string) string {
	message := "WARNING: " + lockfile + " is out of sync with package.json, so `" + frozen +
		"` refused to install. Falling back to `" + permissive + "`, which resolves fresh from package.json" +
		" instead of installing what " + lockfile + " pins — this build is NOT reproducible." +
		" Fix it by running `" + permissive + "` locally and committing the updated " + lockfile + "."
	return "if ! ( set -o pipefail; " + frozen + " 2>&1 | tee " + installLogPath + " ); then\n" +
		"if grep -qF " + shellQuote(drift) + " " + installLogPath + "; then\n" +
		"echo " + shellQuote(message) + "\n" +
		permissive + "\n" +
		"fi\n" +
		"fi"
}

// shellQuote wraps s for bash as a single-quoted string. Every one of these
// strings is written here rather than taken from the repo, but they are long
// English sentences that will be edited, and one apostrophe would otherwise
// turn the install command into a syntax error for every deploy.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// packageManagerPin returns the manager and version package.json's
// `packageManager` field pins — ("yarn", "4.9.4") for "yarn@4.9.4+sha224.abc" —
// or ("", "") when the field is absent, malformed, or names something corepack
// does not proxy. This is the field corepack itself reads, and until this
// change nothing here did.
func packageManagerPin(repoDir string) (manager, version string) {
	f, err := os.Open(filepath.Join(repoDir, "package.json"))
	if err != nil {
		return "", ""
	}
	defer f.Close()
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	// Capped: package.json is a manifest, and this runs against arbitrary
	// customer repos. A file too large to be one is simply not read.
	if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&pkg) != nil {
		return "", ""
	}
	name, rest, ok := strings.Cut(pkg.PackageManager, "@")
	if !ok {
		return "", ""
	}
	// The optional "+sha224.<hash>" suffix is corepack's integrity check, not
	// part of the version.
	version, _, _ = strings.Cut(rest, "+")
	switch name {
	case "npm", "yarn", "pnpm":
		return name, version
	}
	return "", ""
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

// yarnReleaseVersion returns the Yarn version named by a vendored release's
// filename — yarn-4.9.4.cjs -> "4.9.4", yarn-1.22.1.js -> "1.22.1" — or "" when
// the name carries none. Both `yarn set version` and `yarn policies
// set-version` write this yarn-<version>.<ext> shape.
func yarnReleaseVersion(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	version, ok := strings.CutPrefix(name, "yarn-")
	if !ok {
		return ""
	}
	return version
}

// majorOf returns the leading major of a version string, or 0 when there isn't
// one. 0 means undetermined, and callers must treat that as "not Berry": see
// yarnPlan.
func majorOf(version string) int {
	major, _, _ := strings.Cut(version, ".")
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

// npmrcCarriesRegistryConfig reports whether .npmrc holds registry settings a
// Yarn Berry install would ignore: credentials, or a registry that isn't the
// public one. A .npmrc of ordinary knobs (engine-strict, save-exact, a
// registry line pointing at npmjs.org) is not that, and must not cost the repo
// its yarn install.
func npmrcCarriesRegistryConfig(repoDir string) bool {
	f, err := os.Open(filepath.Join(repoDir, ".npmrc"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		switch {
		case strings.Contains(key, "_auth"), strings.Contains(key, "_password"), strings.Contains(key, "certfile"):
			return true
		case strings.HasSuffix(key, "registry") && !strings.Contains(value, "registry.npmjs.org"):
			// A scoped `@acme:registry=` or a private mirror. Berry resolves
			// everything from the public registry without it.
			return true
		}
	}
	return false
}

// yarnrcCarriesRegistryConfig reports whether .yarnrc.yml already tells Berry
// what .npmrc would have: a registry, scopes, or credentials. When it does, the
// repo has done the migration and Berry is safe to run.
func yarnrcCarriesRegistryConfig(repoDir string) bool {
	f, err := os.Open(filepath.Join(repoDir, ".yarnrc.yml"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		if strings.HasPrefix(strings.TrimSpace(sc.Text()), "npm") {
			// npmScopes, npmRegistryServer, npmAuthToken, npmAlwaysAuth — all
			// of Berry's registry settings share the prefix.
			return true
		}
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
