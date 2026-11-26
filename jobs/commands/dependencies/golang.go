package dependencies

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// goEcosystem is the Go adapter: a repository's go.mod at its root. A merged
// library is released by its commit, which go get turns into a
// pseudo-version.
type goEcosystem struct{}

func (goEcosystem) Name() string    { return tasks.DependencyEcosystemGo }
func (goEcosystem) Release() string { return tasks.DependencyReleaseCommit }

func (goEcosystem) Describe(name string) string { return "the Go module " + name }

// ReadManifest reads repoDir/go.mod only (no nested modules), as a regular
// file of at most GoModMaxBytes. Provides is its module path; Requires leaves
// out every module go.mod replaces, since bumping a replaced module's require
// line doesn't change what builds.
func (goEcosystem) ReadManifest(repoDir string) (Manifest, bool, error) {
	data, err := ReadBoundedRegularFile(filepath.Join(repoDir, "go.mod"), GoModMaxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, fmt.Errorf("couldn't read go.mod: %w", err)
	}
	gomod := ParseGoMod(data)
	m := Manifest{Requires: map[string]string{}}
	if gomod.Module != "" {
		m.Provides = []string{gomod.Module}
	}
	for name, version := range gomod.Requires {
		if !gomod.Replaced[name] {
			m.Requires[name] = version
		}
	}
	return m, true, nil
}

// goSourceHosts are the hosts whose module paths start with
// <host>/<owner>/<repository>.
var goSourceHosts = map[string]bool{"github.com": true, "gitlab.com": true, "bitbucket.org": true}

// Source is the first three path elements of a module path on a known host:
// github.com/acme/kit/v2/sub → github.com, acme, kit.
func (goEcosystem) Source(name string) (RepositoryRef, bool) {
	parts := strings.Split(name, "/")
	if len(parts) < 3 || !goSourceHosts[parts[0]] || parts[1] == "" || parts[2] == "" {
		return RepositoryRef{}, false
	}
	return RepositoryRef{Host: parts[0], Owner: parts[1], Name: parts[2]}, true
}

// Bump is go get of every bump's commit at once, go mod tidy, and go mod
// vendor when the repository vendors. Public modules come from the default
// proxy and checksum database; private ones, by git, from the mirrors.
func (g goEcosystem) Bump(repoDir string, bumps []Bump, private []string) (Commands, error) {
	image, err := goImage(repoDir)
	if err != nil {
		return Commands{}, err
	}
	get := []string{"go", "get"}
	for _, b := range bumps {
		get = append(get, b.Name+"@"+b.Commit)
	}
	commands := [][]string{get, {"go", "mod", "tidy"}}
	summary := "go get, go mod tidy"
	if goVendors(repoDir) {
		commands = append(commands, []string{"go", "mod", "vendor"})
		summary += ", go mod vendor"
	}
	return Commands{
		Image:    image,
		Commands: commands,
		Env: []string{
			"GOWORK=off",
			"GOFLAGS=-mod=mod",
			"GOTOOLCHAIN=local",
			"GOPRIVATE=" + strings.Join(private, ","),
			"GOMODCACHE=/cache/gomod",
			"GOCACHE=/tmp/go-build",
			"GOTMPDIR=/tmp",
		},
		Summary: summary,
	}, nil
}

// Verify is go build and go vet of every package, offline from the module
// cache Bump filled (or the vendor directory). The build writes nothing:
// /src is read-only. Tests aren't run: they may need services the offline
// container doesn't have, and the pull request's own CI runs them.
func (goEcosystem) Verify(repoDir string, private []string) (Commands, error) {
	image, err := goImage(repoDir)
	if err != nil {
		return Commands{}, err
	}
	mod := "-mod=readonly"
	if goVendors(repoDir) {
		mod = "-mod=vendor"
	}
	return Commands{
		Image:    image,
		Commands: [][]string{{"go", "build", "-o", "/dev/null", "./..."}, {"go", "vet", "./..."}},
		Env: []string{
			"GOWORK=off",
			"GOPROXY=off",
			"GOSUMDB=off",
			"GOTOOLCHAIN=local",
			"GOPRIVATE=" + strings.Join(private, ","),
			"GOMODCACHE=/cache/gomod",
			"GOCACHE=/tmp/go-build",
			"GOTMPDIR=/tmp",
			"GOFLAGS=" + mod,
		},
		Summary: "go build, go vet",
	}, nil
}

// Allowed: go.mod, go.sum and the vendor directory.
func (goEcosystem) Allowed(path string) bool {
	return path == "go.mod" || path == "go.sum" || strings.HasPrefix(path, "vendor/")
}

// goImage is golang:<GoBuildVersion of repoDir's go.mod>.
func goImage(repoDir string) (string, error) {
	data, err := ReadBoundedRegularFile(filepath.Join(repoDir, "go.mod"), GoModMaxBytes)
	if err != nil {
		return "", fmt.Errorf("couldn't read go.mod: %w", err)
	}
	version, ok := GoBuildVersion(data)
	if !ok {
		name := ParseGoMod(data).Module
		if name == "" {
			name = "the repository"
		}
		return "", fmt.Errorf("couldn't tell which Go version %s builds with from its go.mod", name)
	}
	return "golang:" + version, nil
}

// goVendors reports whether repoDir vendors its dependencies: vendor/modules.txt
// is a regular file.
func goVendors(repoDir string) bool {
	info, err := os.Lstat(filepath.Join(repoDir, "vendor", "modules.txt"))
	return err == nil && info.Mode().IsRegular()
}
