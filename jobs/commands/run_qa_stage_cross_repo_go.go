package commands

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// goCrossRepoLinker is the Go crossRepoLinker: a vendored Go backend whose
// go.mod requires the module of another changed repository is built with
// go mod edit -replace to a copy of it, then go mod vendor, offline against
// the Step's module cache.
type goCrossRepoLinker struct {
	stage *qaStage
}

// Detect is every changed repository (other than backendDir) whose Go module
// backendDir's go.mod requires (crossRepoGoDependencies).
func (l goCrossRepoLinker) Detect(backendDir string, changed []string) []crossRepoDependency {
	var deps []crossRepoDependency
	for _, d := range l.stage.crossRepoGoDependencies(backendDir, changed) {
		deps = append(deps, crossRepoDependency{dir: d.dir, name: d.module, description: "the Go module " + d.module})
	}
	return deps
}

// goVendorModulesMaxBytes bounds a vendor/modules.txt read host-side.
const goVendorModulesMaxBytes = 16 << 20

// Qualifies: backendDir vendors its dependencies (vendor/modules.txt, a
// regular file at its root) and its go.mod's Go version is plain.
func (l goCrossRepoLinker) Qualifies(backendDir string) (bool, string) {
	s := l.stage
	if _, err := s.readHostFile(filepath.Join(s.workDirHost, backendDir, "vendor", "modules.txt"), goVendorModulesMaxBytes); err != nil {
		return false, "it vendors its dependencies (vendor/modules.txt at the repository root)"
	}
	data, err := s.readHostFile(filepath.Join(s.workDirHost, backendDir, "go.mod"), goModMaxBytes)
	if err != nil {
		return false, goPlainVersionWhy
	}
	if _, ok := goBuildVersion(data); !ok {
		return false, goPlainVersionWhy
	}
	return true, ""
}

const goPlainVersionWhy = "its go.mod's Go version is a plain version number"

// CheckService: go.mod is at the repository root, so the service must be too.
func (goCrossRepoLinker) CheckService(buildDir, serviceDir string) string {
	if filepath.Clean(buildDir) != filepath.Clean(serviceDir) {
		return "its service's root directory isn't the repository root"
	}
	return ""
}

// Prepare is, in golang:<version> (goBuildVersion of the build copy's go.mod),
// one go mod edit -replace per dependency to its copy, then go mod vendor —
// offline, against the Step's module cache at /cache/gomod.
//
// No go mod tidy: tidy also loads the tests of every dependency (testify,
// ginkgo, …), modules agentbox's vendor phase never downloads (go mod download
// fetches what the repository's own packages and tests need), so offline it
// fails on modules the build never uses. go mod vendor needs only the
// packages the backend builds and tests — the set the vendor phase fetched.
// A module a replaced dependency newly requires that isn't in the cache still
// fails here, and is reported (prepareCrossRepoBuild).
func (goCrossRepoLinker) Prepare(buildDir string, deps []crossRepoDependency) (crossRepoCommands, error) {
	data, err := readGoModFile(filepath.Join(buildDir, "go.mod"))
	if err != nil {
		return crossRepoCommands{}, fmt.Errorf("couldn't read its go.mod: %w", err)
	}
	version, ok := goBuildVersion(data)
	if !ok {
		return crossRepoCommands{}, errors.New("its go.mod's Go version isn't a plain version number")
	}
	var commands [][]string
	for k, d := range deps {
		commands = append(commands, []string{"go", "mod", "edit", fmt.Sprintf("-replace=%s=./%s", d.name, crossRepoReplaceDir(k))})
	}
	commands = append(commands, []string{"go", "mod", "vendor"})
	return crossRepoCommands{
		image:    "golang:" + version,
		commands: commands,
		env:      goCrossRepoEnv(),
		summary:  "go mod vendor",
	}, nil
}

// goCrossRepoEnv is the go commands' environment: no workspace, no network
// lookups (the proxy and the checksum database off), modules resolved from the
// read-only cache, the image's own toolchain, and the build cache and temporary
// files under /tmp.
func goCrossRepoEnv() []string {
	return []string{
		"GOWORK=off",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"GOMODCACHE=" + crossRepoCacheMount + "/gomod",
		"GOCACHE=/tmp/go-build",
		"GOTMPDIR=/tmp",
	}
}

// goPlainVersion is a plain Go version number: 1.27 or 1.27.1.
var goPlainVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// goBuildVersion is the Go version a go.mod builds with: its toolchain line's
// (toolchain go<version>) when present and plain, else its go line's when
// plain.
func goBuildVersion(data []byte) (string, bool) {
	goLine, toolchain := parseGoModVersions(data)
	if v, ok := strings.CutPrefix(toolchain, "go"); ok && goPlainVersion.MatchString(v) {
		return v, true
	}
	if goPlainVersion.MatchString(goLine) {
		return goLine, true
	}
	return "", false
}

// parseGoModVersions is a go.mod's go and toolchain directives' values ("" when
// absent), outside any block.
func parseGoModVersions(data []byte) (goLine, toolchain string) {
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(line)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if inBlock {
			if fields[0] == ")" {
				inBlock = false
			}
			continue
		}
		if len(fields) >= 2 && fields[1] == "(" {
			inBlock = fields[len(fields)-1] != ")"
			continue
		}
		switch {
		case fields[0] == "go" && len(fields) == 2 && goLine == "":
			goLine = fields[1]
		case fields[0] == "toolchain" && len(fields) == 2 && toolchain == "":
			toolchain = fields[1]
		}
	}
	return goLine, toolchain
}

// goModDependency is a changed repository whose Go module a backend requires.
type goModDependency struct {
	dir, module string
}

// crossRepoGoDependencies is every changed repository (other than backendDir)
// whose Go module backendDir's go.mod requires, in work-dir order. Only go.mod
// files at the repositories' roots are read. A backend without a go.mod isn't
// a Go module and depends on nothing here; a changed repository without a
// go.mod or a module line is skipped. A read error other than "not found" is
// logged and never counts as a dependency.
func (s *qaStage) crossRepoGoDependencies(backendDir string, changed []string) []goModDependency {
	_, required, ok := s.readGoMod(backendDir)
	if !ok || len(required) == 0 {
		return nil
	}
	var deps []goModDependency
	for _, dir := range changed {
		if dir == backendDir {
			continue
		}
		module, _, ok := s.readGoMod(dir)
		if !ok || module == "" {
			continue
		}
		if required[module] {
			deps = append(deps, goModDependency{dir: dir, module: module})
		}
	}
	return deps
}

// readHostFile reads path host-side through the stage's readFile seam, or as
// a regular file of at most max bytes (readBoundedRegularFile).
func (s *qaStage) readHostFile(path string, max int64) ([]byte, error) {
	if s.readFile != nil {
		return s.readFile(path)
	}
	return readBoundedRegularFile(path, max)
}
