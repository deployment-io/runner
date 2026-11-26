package dependencies

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// fixtureRepo is a repository in t.TempDir() holding files.
func fixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const appGoMod = `module github.com/acme/app-server

go 1.27

require github.com/acme/kit v1.4.0

require (
	github.com/aws/aws-sdk-go-v2 v1.30.0
	"github.com/acme/quoted" v0.1.0
	golang.org/x/sys v0.20.0 // indirect
	github.com/acme/local v0.0.1
	github.com/acme/forked v1.0.0
	github.com/acme/pinned v1.1.0
	github.com/acme/other v1.2.0
)

exclude (
	github.com/acme/excluded v0.0.1
)

exclude github.com/acme/kit v1.3.0

replace github.com/acme/local => ../local

replace github.com/acme/pinned v1.1.0 => github.com/fork/pinned v1.1.1

replace (
	github.com/acme/forked => github.com/fork/forked v1.0.1
	github.com/acme/other v1.2.0 => ./other
)
`

func TestGoReadManifest(t *testing.T) {
	m, ok, err := goEcosystem{}.ReadManifest(fixtureRepo(t, map[string]string{"go.mod": appGoMod}))
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if want := []string{"github.com/acme/app-server"}; !reflect.DeepEqual(m.Provides, want) {
		t.Errorf("Provides = %v, want %v", m.Provides, want)
	}
	want := map[string]string{
		"github.com/acme/kit":          "v1.4.0",
		"github.com/aws/aws-sdk-go-v2": "v1.30.0",
		"github.com/acme/quoted":       "v0.1.0",
		"golang.org/x/sys":             "v0.20.0",
	}
	if !reflect.DeepEqual(m.Requires, want) {
		t.Errorf("Requires = %v, want %v", m.Requires, want)
	}
}

func TestGoReadManifestWithoutAModuleLine(t *testing.T) {
	m, ok, err := goEcosystem{}.ReadManifest(fixtureRepo(t, map[string]string{"go.mod": "go 1.27\n\nrequire github.com/acme/kit v1.4.0\n"}))
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if len(m.Provides) != 0 || m.Requires["github.com/acme/kit"] != "v1.4.0" {
		t.Errorf("manifest = %+v", m)
	}
}

func TestGoReadManifestWithoutAGoMod(t *testing.T) {
	_, ok, err := goEcosystem{}.ReadManifest(fixtureRepo(t, map[string]string{"package.json": "{}"}))
	if ok || err != nil {
		t.Errorf("ok %v, err %v; want not ok and no error", ok, err)
	}
}

func TestGoReadManifestRefusesASymlinkedGoMod(t *testing.T) {
	target := fixtureRepo(t, map[string]string{"go.mod": appGoMod})
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(target, "go.mod"), filepath.Join(dir, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := (goEcosystem{}).ReadManifest(dir); ok || err == nil {
		t.Errorf("ok %v, err %v; want an error", ok, err)
	}
}

func TestParseGoModReplaces(t *testing.T) {
	got := ParseGoMod([]byte(appGoMod)).Replaced
	want := map[string]bool{"github.com/acme/local": true, "github.com/acme/pinned": true, "github.com/acme/forked": true, "github.com/acme/other": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Replaced = %v, want %v", got, want)
	}
}

func TestGoSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		want RepositoryRef
		ok   bool
	}{
		{"github.com/acme/kit", RepositoryRef{"github.com", "acme", "kit"}, true},
		{"github.com/acme/kit/v2/sub", RepositoryRef{"github.com", "acme", "kit"}, true},
		{"gitlab.com/acme/kit", RepositoryRef{"gitlab.com", "acme", "kit"}, true},
		{"bitbucket.org/acme/kit/v3", RepositoryRef{"bitbucket.org", "acme", "kit"}, true},
		{"github.com/acme", RepositoryRef{}, false},
		{"golang.org/x/sys", RepositoryRef{}, false},
		{"example.com/acme/kit", RepositoryRef{}, false},
	} {
		got, ok := goEcosystem{}.Source(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Source(%q) = %+v %v, want %+v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestGoBump(t *testing.T) {
	bumps := []Bump{{Name: "github.com/acme/kit", Commit: "0123456789abcdef0123456789abcdef01234567"}, {Name: "github.com/acme/lib", Commit: "fedcba9876543210fedcba9876543210fedcba98"}}
	get := []string{"go", "get", "github.com/acme/kit@0123456789abcdef0123456789abcdef01234567", "github.com/acme/lib@fedcba9876543210fedcba9876543210fedcba98"}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  [][]string
	}{
		{"without vendor", map[string]string{"go.mod": appGoMod}, [][]string{get, {"go", "mod", "tidy"}}},
		{"with vendor", map[string]string{"go.mod": appGoMod, "vendor/modules.txt": "# x\n"}, [][]string{get, {"go", "mod", "tidy"}, {"go", "mod", "vendor"}}},
	} {
		c, err := goEcosystem{}.Bump(fixtureRepo(t, tc.files), bumps, []string{"github.com/acme", "gitlab.com/acme"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(c.Commands, tc.want) {
			t.Errorf("%s: commands = %v, want %v", tc.name, c.Commands, tc.want)
		}
		if c.Image != "golang:1.27" {
			t.Errorf("%s: image = %q", tc.name, c.Image)
		}
		want := []string{"GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOPRIVATE=github.com/acme,gitlab.com/acme", "GOMODCACHE=/cache/gomod", "GOCACHE=/tmp/go-build", "GOTMPDIR=/tmp"}
		if !reflect.DeepEqual(c.Env, want) {
			t.Errorf("%s: env = %v, want %v", tc.name, c.Env, want)
		}
	}
}

func TestGoBumpImage(t *testing.T) {
	for _, tc := range []struct {
		goMod, want string
	}{
		{"module m\n\ngo 1.26\n\ntoolchain go1.27.3\n", "golang:1.27.3"},
		{"module m\n\ngo 1.26.1\n", "golang:1.26.1"},
	} {
		c, err := goEcosystem{}.Bump(fixtureRepo(t, map[string]string{"go.mod": tc.goMod}), nil, nil)
		if err != nil || c.Image != tc.want {
			t.Errorf("%q: image %q, err %v; want %q", tc.goMod, c.Image, err, tc.want)
		}
	}
	_, err := goEcosystem{}.Bump(fixtureRepo(t, map[string]string{"go.mod": "module github.com/acme/app\n"}), nil, nil)
	if err == nil || err.Error() != "couldn't tell which Go version github.com/acme/app builds with from its go.mod" {
		t.Errorf("err = %v", err)
	}
	if _, err := (goEcosystem{}).Verify(fixtureRepo(t, map[string]string{"go.mod": "module m\n"}), nil); err == nil {
		t.Error("Verify: err = nil, want an error")
	}
}

func TestGoVerify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		flags string
	}{
		{"without vendor", map[string]string{"go.mod": appGoMod}, "GOFLAGS=-mod=readonly"},
		{"with vendor", map[string]string{"go.mod": appGoMod, "vendor/modules.txt": "# x\n"}, "GOFLAGS=-mod=vendor"},
	} {
		c, err := goEcosystem{}.Verify(fixtureRepo(t, tc.files), []string{"github.com/acme"})
		if err != nil {
			t.Fatal(err)
		}
		if want := [][]string{{"go", "build", "-o", "/dev/null", "./..."}, {"go", "vet", "./..."}}; !reflect.DeepEqual(c.Commands, want) {
			t.Errorf("%s: commands = %v", tc.name, c.Commands)
		}
		env := strings.Join(c.Env, " ")
		for _, want := range []string{tc.flags, "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOPRIVATE=github.com/acme", "GOMODCACHE=/cache/gomod"} {
			if !strings.Contains(env, want) {
				t.Errorf("%s: env %v lacks %s", tc.name, c.Env, want)
			}
		}
		if c.Image != "golang:1.27" {
			t.Errorf("%s: image = %q", tc.name, c.Image)
		}
	}
}

func TestGoAllowed(t *testing.T) {
	for path, want := range map[string]bool{
		"go.mod": true, "go.sum": true, "vendor/x": true, "vendor/modules.txt": true,
		"main.go": false, "README.md": false, "go.work": false, "sub/go.mod": false, "vendor": false,
	} {
		if got := (goEcosystem{}).Allowed(path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestGoNames(t *testing.T) {
	g := goEcosystem{}
	if !tasks.ValidDependencyEcosystem(g.Name()) || g.Name() != "go" || g.Release() != tasks.DependencyReleaseCommit {
		t.Errorf("name %q, release %q", g.Name(), g.Release())
	}
	if got := g.Describe("github.com/acme/kit"); got != "the Go module github.com/acme/kit" {
		t.Errorf("Describe = %q", got)
	}
}

func TestLookup(t *testing.T) {
	if e, ok := Lookup("go"); !ok || e.Name() != "go" {
		t.Errorf("Lookup(go) = %v %v", e, ok)
	}
	if _, ok := Lookup("npm"); ok {
		t.Error("Lookup(npm) found an ecosystem")
	}
	if got := Registered(); len(got) != 1 || got[0].Name() != "go" {
		t.Errorf("Registered = %v", got)
	}
}
