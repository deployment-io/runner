package commands

// preview_site_detect.go detects how to build the static site of a repository
// deployment.io doesn't deploy, so deploy_static_site_preview can build its preview on
// the runner the way it builds a deployed site's: the build command from package.json,
// the publish directory from the framework, is_spa from the router.
//
// Detection is static and reads only package.json and, for Next.js, its next.config
// file — through readDetectFile, which never follows a symlink, never opens anything
// but a regular file, and reads at most detectFileMaxBytes. Nothing it reads is
// executed.

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/deployment-io/deployment-runner/agenttools"
)

// detectFileMaxBytes caps a file detection reads; a larger one counts as absent.
const detectFileMaxBytes = 1 << 20

// nextConfigFiles are the Next.js config files detection reads for a static export.
var nextConfigFiles = []string{"next.config.js", "next.config.mjs", "next.config.cjs", "next.config.ts"}

// nextStaticExport matches a Next.js config's `output: 'export'`.
var nextStaticExport = regexp.MustCompile(`output\s*:\s*['"]export['"]`)

// spaRouters are the client-side routers that make a site a single-page app.
var spaRouters = []string{"react-router-dom", "react-router", "@tanstack/react-router", "vue-router", "wouter", "preact-router"}

// previewPackageJSON is what detection reads from package.json.
type previewPackageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]any    `json:"dependencies"`
	DevDependencies map[string]any    `json:"devDependencies"`
}

// has reports whether name is a dependency or devDependency.
func (pkg previewPackageJSON) has(name string) bool {
	_, dep := pkg.Dependencies[name]
	_, dev := pkg.DevDependencies[name]
	return dep || dev
}

// detectPreviewSite detects how to build the static site in siteDir: the site, or why
// nothing was detected.
func detectPreviewSite(siteDir string) (agenttools.DetectedSite, string, bool) {
	data, ok := readDetectFile(siteDir, "package.json")
	if !ok {
		return agenttools.DetectedSite{}, "no package.json", false
	}
	var pkg previewPackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return agenttools.DetectedSite{}, "package.json isn't valid JSON", false
	}
	if strings.TrimSpace(pkg.Scripts["build"]) == "" {
		return agenttools.DetectedSite{}, `package.json has no "build" script`, false
	}
	site, reason, ok := detectFramework(siteDir, pkg)
	if !ok {
		return agenttools.DetectedSite{}, reason, false
	}
	site.BuildCommand = lockfileManager(siteDir) + " run build"
	return site, "", true
}

// detectFramework picks the first framework of pkg's dependencies that builds a static
// site, with its publish directory and is_spa.
func detectFramework(siteDir string, pkg previewPackageJSON) (agenttools.DetectedSite, string, bool) {
	isSPA := false
	for _, r := range spaRouters {
		isSPA = isSPA || pkg.has(r)
	}
	site := func(framework, publish string, spa bool) (agenttools.DetectedSite, string, bool) {
		return agenttools.DetectedSite{Framework: framework, PublishDirectory: publish, IsSPA: spa}, "", true
	}
	switch {
	case pkg.has("next"):
		if strings.Contains(pkg.Scripts["build"], "next export") || nextConfigExports(siteDir) {
			return site("Next.js", "out", false)
		}
		return agenttools.DetectedSite{}, "a Next.js app without output: 'export' isn't a static site", false
	case pkg.has("@sveltejs/kit"):
		if pkg.has("@sveltejs/adapter-static") {
			return site("SvelteKit", "build", false)
		}
		return agenttools.DetectedSite{}, "a SvelteKit app without @sveltejs/adapter-static isn't a static site", false
	case pkg.has("astro"):
		return site("Astro", "dist", false)
	case pkg.has("gatsby"):
		return site("Gatsby", "public", false)
	case pkg.has("react-scripts"):
		return site("Create React App", "build", isSPA)
	case pkg.has("@vue/cli-service"):
		return site("Vue CLI", "dist", isSPA)
	case pkg.has("vite"):
		return site("Vite", "dist", isSPA)
	}
	return agenttools.DetectedSite{}, "none of Next.js, SvelteKit, Astro, Gatsby, Create React App, Vue CLI or Vite is a dependency", false
}

// nextConfigExports reports whether a next.config file in siteDir sets output: 'export'.
func nextConfigExports(siteDir string) bool {
	for _, name := range nextConfigFiles {
		if data, ok := readDetectFile(siteDir, name); ok && nextStaticExport.Match(data) {
			return true
		}
	}
	return false
}

// readDetectFile reads <dir>/<name> when it is a regular file of at most
// detectFileMaxBytes: never through a symlink, never a FIFO or device (opened
// non-blocking, so a FIFO can't hang it). Anything else — missing, a symlink, a
// directory, too large, unreadable — is ok=false, as if absent.
func readDetectFile(dir, name string) ([]byte, bool) {
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// Re-checked on the opened file: the path may have been swapped since Lstat.
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, detectFileMaxBytes+1))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	if len(data) > detectFileMaxBytes {
		return nil, false
	}
	return data, true
}
