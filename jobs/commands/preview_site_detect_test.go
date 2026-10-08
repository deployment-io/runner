package commands

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner/agenttools"
)

// writeSite writes files into a new site directory.
func writeSite(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pkgJSON(build string, deps ...string) string {
	quoted := make([]string, len(deps))
	for i, d := range deps {
		quoted[i] = `"` + d + `": "1.0.0"`
	}
	return `{"scripts": {"build": "` + build + `"}, "dependencies": {` + strings.Join(quoted, ", ") + `}}`
}

func TestDetectPreviewSite(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		want   agenttools.DetectedSite
		reason string // "" → detected
	}{
		{"astro", map[string]string{"package.json": pkgJSON("astro build", "astro", "react-router")}, agenttools.DetectedSite{Framework: "Astro", PublishDirectory: "dist"}, ""},
		{"gatsby", map[string]string{"package.json": pkgJSON("gatsby build", "gatsby")}, agenttools.DetectedSite{Framework: "Gatsby", PublishDirectory: "public"}, ""},
		{"cra", map[string]string{"package.json": pkgJSON("react-scripts build", "react-scripts")}, agenttools.DetectedSite{Framework: "Create React App", PublishDirectory: "build"}, ""},
		{"vue cli", map[string]string{"package.json": pkgJSON("vue-cli-service build", "@vue/cli-service")}, agenttools.DetectedSite{Framework: "Vue CLI", PublishDirectory: "dist"}, ""},
		{"vite", map[string]string{"package.json": pkgJSON("vite build", "vite")}, agenttools.DetectedSite{Framework: "Vite", PublishDirectory: "dist"}, ""},
		{"vite as devDependency", map[string]string{"package.json": `{"scripts": {"build": "vite build"}, "devDependencies": {"vite": "5"}}`}, agenttools.DetectedSite{Framework: "Vite", PublishDirectory: "dist"}, ""},
		{"next export script", map[string]string{"package.json": pkgJSON("next build && next export", "next")}, agenttools.DetectedSite{Framework: "Next.js", PublishDirectory: "out"}, ""},
		{"next without export", map[string]string{"package.json": pkgJSON("next build", "next", "react-router-dom"), "next.config.js": "module.exports = {}"}, agenttools.DetectedSite{}, "a Next.js app without output: 'export' isn't a static site"},
		{"next config symlink", map[string]string{"package.json": pkgJSON("next build", "next")}, agenttools.DetectedSite{}, "a Next.js app without output: 'export' isn't a static site"},
		{"sveltekit static", map[string]string{"package.json": pkgJSON("vite build", "@sveltejs/kit", "@sveltejs/adapter-static", "vite")}, agenttools.DetectedSite{Framework: "SvelteKit", PublishDirectory: "build"}, ""},
		{"sveltekit without adapter-static", map[string]string{"package.json": pkgJSON("vite build", "@sveltejs/kit", "vite")}, agenttools.DetectedSite{}, "a SvelteKit app without @sveltejs/adapter-static isn't a static site"},
		{"no package.json", map[string]string{"index.html": "<html>"}, agenttools.DetectedSite{}, "no package.json"},
		{"no build script", map[string]string{"package.json": `{"scripts": {"dev": "vite"}, "dependencies": {"vite": "5"}}`}, agenttools.DetectedSite{}, `package.json has no "build" script`},
		{"unknown framework", map[string]string{"package.json": pkgJSON("webpack", "webpack")}, agenttools.DetectedSite{}, "none of Next.js, SvelteKit, Astro, Gatsby, Create React App, Vue CLI or Vite is a dependency"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeSite(t, c.files)
			if c.name == "next config symlink" {
				// A next.config that is a symlink is never read.
				target := filepath.Join(t.TempDir(), "next.config.js")
				os.WriteFile(target, []byte(`module.exports = { output: "export" }`), 0o644)
				if err := os.Symlink(target, filepath.Join(dir, "next.config.js")); err != nil {
					t.Fatal(err)
				}
			}
			got, reason, ok := detectPreviewSite(dir)
			if c.reason != "" {
				if ok || reason != c.reason {
					t.Errorf("got %+v, %q, %v; want not detected: %q", got, reason, ok, c.reason)
				}
				return
			}
			c.want.BuildCommand = "npm run build"
			if !ok || got != c.want {
				t.Errorf("got %+v, %q, %v; want %+v", got, reason, ok, c.want)
			}
		})
	}
}

func TestDetectPreviewSiteNextConfigFiles(t *testing.T) {
	for _, name := range nextConfigFiles {
		for _, config := range []string{`output: 'export'`, `output:"export"`, "output :\n  'export'"} {
			dir := writeSite(t, map[string]string{"package.json": pkgJSON("next build", "next"), name: "export default {\n  " + config + ",\n}"})
			got, reason, ok := detectPreviewSite(dir)
			if !ok || got.Framework != "Next.js" || got.PublishDirectory != "out" || got.IsSPA {
				t.Errorf("%s with %q: got %+v, %q, %v", name, config, got, reason, ok)
			}
		}
	}
}

func TestDetectPreviewSiteIsSPA(t *testing.T) {
	for _, framework := range []string{"vite", "react-scripts", "@vue/cli-service"} {
		dir := writeSite(t, map[string]string{"package.json": pkgJSON("build", framework)})
		if got, _, ok := detectPreviewSite(dir); !ok || got.IsSPA {
			t.Errorf("%s without a router: got %+v, %v; want IsSPA false", framework, got, ok)
		}
		for _, router := range spaRouters {
			dir := writeSite(t, map[string]string{"package.json": `{"scripts": {"build": "x"}, "dependencies": {"` + framework + `": "1"}, "devDependencies": {"` + router + `": "1"}}`})
			if got, _, ok := detectPreviewSite(dir); !ok || !got.IsSPA {
				t.Errorf("%s with %s: got %+v, %v; want IsSPA true", framework, router, got, ok)
			}
		}
	}
	// Frameworks whose output isn't an SPA stay false with a router.
	dir := writeSite(t, map[string]string{"package.json": pkgJSON("build", "gatsby", "react-router-dom")})
	if got, _, _ := detectPreviewSite(dir); got.IsSPA {
		t.Errorf("gatsby with a router: got IsSPA true")
	}
}

func TestDetectPreviewSiteManager(t *testing.T) {
	for lockfiles, want := range map[string]string{
		"":                            "npm run build",
		"package-lock.json":           "npm run build",
		"yarn.lock":                   "yarn run build",
		"pnpm-lock.yaml":              "pnpm run build",
		"pnpm-lock.yaml,yarn.lock":    "pnpm run build",
		"yarn.lock,package-lock.json": "yarn run build",
	} {
		files := map[string]string{"package.json": pkgJSON("vite build", "vite")}
		for _, l := range strings.Split(lockfiles, ",") {
			if l != "" {
				files[l] = ""
			}
		}
		dir := writeSite(t, files)
		got, _, ok := detectPreviewSite(dir)
		if !ok || got.BuildCommand != want {
			t.Errorf("lockfiles %q: got %q, %v; want %q", lockfiles, got.BuildCommand, ok, want)
		}
		if tc := resolveToolchain(dir); tc.manager+" run build" != got.BuildCommand {
			t.Errorf("lockfiles %q: detection %q disagrees with resolveToolchain's %q", lockfiles, got.BuildCommand, tc.manager)
		}
	}
}

func TestDetectPreviewSitePackageJSONNotRegular(t *testing.T) {
	const absent = "no package.json"
	t.Run("symlink", func(t *testing.T) {
		target := writeSite(t, map[string]string{"package.json": pkgJSON("vite build", "vite")})
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(target, "package.json"), filepath.Join(dir, "package.json")); err != nil {
			t.Fatal(err)
		}
		if _, reason, ok := detectPreviewSite(dir); ok || reason != absent {
			t.Errorf("got %q, %v; want %q", reason, ok, absent)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		os.Mkdir(filepath.Join(dir, "package.json"), 0o755)
		if _, reason, ok := detectPreviewSite(dir); ok || reason != absent {
			t.Errorf("got %q, %v; want %q", reason, ok, absent)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, "package.json"), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		done := make(chan string, 1)
		go func() {
			_, reason, _ := detectPreviewSite(dir)
			done <- reason
		}()
		select {
		case reason := <-done:
			if reason != absent {
				t.Errorf("got %q, want %q", reason, absent)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("detection blocked on a FIFO")
		}
	})
	t.Run("over 1 MiB", func(t *testing.T) {
		big := `{"scripts": {"build": "vite build"}, "dependencies": {"vite": "5"}, "description": "` + strings.Repeat("x", detectFileMaxBytes) + `"}`
		dir := writeSite(t, map[string]string{"package.json": big})
		if _, reason, ok := detectPreviewSite(dir); ok || reason != absent {
			t.Errorf("got %q, %v; want %q", reason, ok, absent)
		}
	})
}
