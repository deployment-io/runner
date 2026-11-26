// Package dependencies holds the per-ecosystem adapters a Task's dependency
// handling uses: reading which modules or packages a repository provides and
// requires, and what to run to bump and verify a dependent.
//
// Everything ecosystem-specific lives here, and the package is pure: it reads
// files and returns commands, and never runs docker, git or an RPC. The
// RunDependencyBump command and the graph recorded after every Task Step
// (jobs/commands) only call Lookup, Registered and the Ecosystem interface.
//
// Adding an ecosystem is a new file implementing Ecosystem, its fixtures, and
// one line in registry. Nothing outside this package changes, nor kit,
// deployment-server or the dashboard.
package dependencies

// Ecosystem is one language's or package manager's dependency handling.
type Ecosystem interface {
	// Name is the ecosystem's wire name; tasks.ValidDependencyEcosystem holds for it ("go").
	Name() string
	// Release is how a merged library is released: tasks.DependencyReleaseCommit, or tasks.DependencyReleaseRegistry (no adapter uses that yet).
	Release() string
	// Describe is how messages and the dashboard name a module or package ("the Go module github.com/acme/kit").
	Describe(name string) string
	// ReadManifest reads repoDir's manifest; ok is false when repoDir isn't this ecosystem's.
	ReadManifest(repoDir string) (m Manifest, ok bool, err error)
	// Source is the git repository that provides name; ok is false for a name no git repository on a known host provides.
	Source(name string) (RepositoryRef, bool)
	// Bump is what runs with network but no credentials, in repoDir mounted at /src, to point the dependent at each bump's commit. private are the module-name prefixes served from the mirrors.
	Bump(repoDir string, bumps []Bump, private []string) (Commands, error)
	// Verify is what runs with no network after the bump, from the cache Bump filled.
	Verify(repoDir string, private []string) (Commands, error)
	// Allowed reports whether a changed path (slash-separated, relative to the repository root) may be part of a bump.
	Allowed(path string) bool
}

// Manifest is what a repository provides and requires in one ecosystem.
type Manifest struct {
	Provides []string
	// Requires is name → the version pinned now.
	Requires map[string]string
}

// Bump is one dependency to point at a merged commit.
type Bump struct {
	Name   string
	Commit string
}

// RepositoryRef is a git repository on a host: github.com, acme, kit.
type RepositoryRef struct {
	Host, Owner, Name string
}

// Commands is what runs in a container: Commands in order, in Image, with
// Env. Summary names them for the job log ("go get, go mod tidy").
type Commands struct {
	Image    string
	Commands [][]string
	Env      []string
	Summary  string
}

// registry is every ecosystem this runner supports. Tests may replace it.
var registry = []Ecosystem{goEcosystem{}}

// Registered is every ecosystem this runner supports.
func Registered() []Ecosystem {
	return registry
}

// Lookup is the registered ecosystem named name.
func Lookup(name string) (Ecosystem, bool) {
	for _, e := range registry {
		if e.Name() == name {
			return e, true
		}
	}
	return nil, false
}

// SetRegistryForTest replaces the registry until the returned restore is
// called. Tests in other packages use it to register a made-up ecosystem.
func SetRegistryForTest(ecosystems []Ecosystem) (restore func()) {
	previous := registry
	registry = ecosystems
	return func() { registry = previous }
}
