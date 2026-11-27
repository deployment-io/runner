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

// PinnedCommit is the revision of a pseudo-version, with or without
// +incompatible: vX.0.0-<timestamp>-<rev>, vX.Y.Z-0.<timestamp>-<rev> or
// vX.Y.Z-<pre>.0.<timestamp>-<rev>, where timestamp is 14 digits and rev 12
// lower-case hex characters. A tagged version names no commit.
func (goEcosystem) PinnedCommit(_, version string) (string, bool) {
	version = strings.TrimSuffix(version, "+incompatible")
	dash := strings.LastIndexByte(version, '-')
	if dash < 0 {
		return "", false
	}
	base, rev := version[:dash], version[dash+1:]
	if len(rev) != 12 || !allBytes(rev, isLowerHex) || len(base) < 14 {
		return "", false
	}
	prefix, timestamp := base[:len(base)-14], base[len(base)-14:]
	if !allBytes(timestamp, isDigit) {
		return "", false
	}
	core, rest, ok := strings.Cut(prefix, "-")
	if !ok || !isSemverCore(core) {
		return "", false
	}
	switch {
	case rest == "" && strings.SplitN(core, ".", 2)[1] == "0.0":
	case rest == "0.":
	case strings.HasSuffix(rest, ".0.") && isPrerelease(strings.TrimSuffix(rest, ".0.")):
	default:
		return "", false
	}
	return rev, true
}

// isSemverCore reports whether s is v<major>.<minor>.<patch>.
func isSemverCore(s string) bool {
	if !strings.HasPrefix(s, "v") {
		return false
	}
	parts := strings.Split(s[1:], ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if !isNumericID(p) {
			return false
		}
	}
	return true
}

// isNumericID reports whether s is a semver numeric identifier: digits, with
// no leading zero unless it is "0".
func isNumericID(s string) bool {
	return s != "" && allBytes(s, isDigit) && (s == "0" || s[0] != '0')
}

// isPrerelease reports whether s is a semver prerelease: dot-separated,
// non-empty identifiers of ASCII letters, digits and hyphens.
func isPrerelease(s string) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" || !allBytes(id, isIdentChar) || (allBytes(id, isDigit) && !isNumericID(id)) {
			return false
		}
	}
	return true
}

func isIdentChar(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-'
}

func allBytes(s string, f func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !f(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool    { return c >= '0' && c <= '9' }
func isLowerHex(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'f') }

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
