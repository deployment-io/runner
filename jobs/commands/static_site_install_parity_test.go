package commands

import "testing"

// TestToolchainParityWithAgentbox pins the resolution this code shares with
// agentbox: for the same repo, both must pick the same package manager and the
// same version of it.
//
// Why this is an acceptance criterion and not a nicety: an agent Task installs
// and verifies against one dependency tree, and the deploy of the same commit
// ships another. Both steps succeed, so nothing surfaces the difference — the
// customer just sees "it passed the Task and broke in production". That is a
// worse failure than the non-reproducibility this change fixes, because it
// looks like nothing went wrong.
//
// HAND-MAINTAINED. agentbox is a separate module and cannot be imported, so
// this table is a copy of a decision that lives in two places:
//
//	agentbox internal/vendoring/node/detector.go
//	  — installCommand, yarnCommand, vendoredYarnRelease, releaseMajor
//
// Changing either side's resolution means changing the other and this table in
// the same breath. A row that disagrees with detector.go is a bug in whichever
// side moved without the other, not a stale test to be re-recorded.
//
// SCOPE — what parity does and does not cover:
//
//   - Manager and version, from the tree alone. That is deliberately all
//     resolveToolchain looks at, because agentbox has no build command to
//     consult and no notion of one.
//
//   - Not the install command itself. agentbox installs so an agent can run
//     the repo; this installs so a customer-supplied build command can. The
//     one place that makes the trees differ is a Yarn Berry repo on the
//     Plug'n'Play linker whose build command does not run through yarn: PnP
//     writes no node_modules, so `next build` cannot resolve through it and
//     the deploy installs with npm instead (see yarnPlan). That divergence is
//     chosen, logged on every build it affects, and the alternative — a yarn
//     install the build then cannot use — is a failed deploy.
//
//   - npm's version is honoured by neither side: a corepack npm shim refuses
//     to run in a repo whose packageManager names yarn or pnpm, so neither
//     image installs one and both run the npm they ship. Pinned or not, both
//     sides agree, which is what this test asserts.
func TestToolchainParityWithAgentbox(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		// want is the manager and version BOTH sides must resolve. An empty
		// version means "whatever the image provides" — and both images
		// provide npm and Yarn Classic, so unpinned is still agreement.
		wantManager string
		wantVersion string
		// wantCorepack is how the version is obtained. It differs per image by
		// design (agentbox bakes its shims into its Dockerfile; this enables
		// one per build), so it is recorded here as documentation of the
		// mechanism rather than asserted as a shared value.
		wantCorepack bool
	}{
		{
			name:        "plain npm repo",
			files:       map[string]string{"package.json": "{}", "package-lock.json": "{}"},
			wantManager: "npm",
		},
		{
			name:        "npm pin, honoured by neither side",
			files:       map[string]string{"package.json": `{"packageManager":"npm@10.9.0"}`, "package-lock.json": "{}"},
			wantManager: "npm",
		},
		{
			name:        "yarn classic, unpinned",
			files:       map[string]string{"yarn.lock": classicYarnLock},
			wantManager: "yarn",
		},
		{
			name: "yarn berry pinned by packageManager",
			files: map[string]string{
				"yarn.lock":    berryYarnLock,
				"package.json": `{"packageManager":"yarn@4.9.4"}`,
			},
			wantManager:  "yarn",
			wantVersion:  "4.9.4",
			wantCorepack: true,
		},
		{
			name: "yarn berry vendored in-tree",
			files: map[string]string{
				"yarn.lock":                     berryYarnLock,
				".yarnrc.yml":                   "yarnPath: .yarn/releases/yarn-4.9.4.cjs\n",
				".yarn/releases/yarn-4.9.4.cjs": "// yarn 4\n",
			},
			wantManager: "yarn",
			wantVersion: "4.9.4",
		},
		{
			// The vendored release is what runs on both sides, so both must
			// see Classic here — including the corepack side, whose shim would
			// otherwise have fetched Berry off the pin.
			name: "vendored classic release beats a berry pin",
			files: map[string]string{
				"yarn.lock":                     berryYarnLock,
				"package.json":                  `{"packageManager":"yarn@4.9.4"}`,
				".yarnrc":                       "yarn-path \".yarn/releases/yarn-1.22.1.js\"\n",
				".yarn/releases/yarn-1.22.1.js": "// yarn 1\n",
			},
			wantManager: "yarn",
			wantVersion: "1.22.1",
		},
		{
			name:         "pnpm, unpinned",
			files:        map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'\n"},
			wantManager:  "pnpm",
			wantCorepack: true,
		},
		{
			name: "pnpm pinned by packageManager",
			files: map[string]string{
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
				"package.json":   `{"packageManager":"pnpm@10.4.1"}`,
			},
			wantManager:  "pnpm",
			wantVersion:  "10.4.1",
			wantCorepack: true,
		},
		{
			// Precedence, which both sides share: pnpm, then yarn, then npm.
			name: "a repo carrying every lockfile",
			files: map[string]string{
				"pnpm-lock.yaml":    "lockfileVersion: '9.0'\n",
				"yarn.lock":         classicYarnLock,
				"package-lock.json": "{}",
			},
			wantManager:  "pnpm",
			wantCorepack: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tcResolved := resolveToolchain(writeRepo(t, tc.files))
			if tcResolved.manager != tc.wantManager || tcResolved.version != tc.wantVersion {
				t.Errorf("resolveToolchain() = %s@%q, want %s@%q — agentbox's detector.go resolves this repo differently now",
					tcResolved.manager, tcResolved.version, tc.wantManager, tc.wantVersion)
			}
			if tcResolved.corepack != tc.wantCorepack {
				t.Errorf("corepack = %v, want %v", tcResolved.corepack, tc.wantCorepack)
			}
		})
	}
}
