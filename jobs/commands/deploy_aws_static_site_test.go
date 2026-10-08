package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// publishDirFixture is a temp dir holding a checkout `co` and an `outside`
// directory with an index.html, the shape a malicious link would aim at.
type publishDirFixture struct {
	base     string
	checkout string
	outside  string
}

func newPublishDirFixture(t *testing.T) publishDirFixture {
	t.Helper()
	base := t.TempDir()
	f := publishDirFixture{
		base:     base,
		checkout: filepath.Join(base, "co"),
		outside:  filepath.Join(base, "outside"),
	}
	f.mkdir(t, "co/.git")
	f.write(t, "co/.git/config", "[remote \"origin\"]\n\turl = https://token@example.com/r.git\n")
	f.mkdir(t, "outside")
	f.write(t, "outside/index.html", "x")
	return f
}

func (f publishDirFixture) mkdir(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join(f.base, rel)
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f publishDirFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(f.base, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func (f publishDirFixture) symlink(t *testing.T, target, rel string) {
	t.Helper()
	path := filepath.Join(f.base, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func (f publishDirFixture) distDirectory(t *testing.T, repoDirectory, publishDirectory string) string {
	t.Helper()
	parameters := map[string]interface{}{}
	if err := jobs.SetParameterValue(parameters, parameters_enums.RepoDirectoryPath, repoDirectory); err != nil {
		t.Fatal(err)
	}
	if err := jobs.SetParameterValue(parameters, parameters_enums.PublishDirectory, publishDirectory); err != nil {
		t.Fatal(err)
	}
	dist, err := getDistDirectory(parameters)
	if err != nil {
		t.Fatal(err)
	}
	return dist
}

func (f publishDirFixture) assertRefused(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if strings.Contains(err.Error(), f.base) {
		t.Errorf("error %q leaks the host path %q", err, f.base)
	}
}

func TestCheckPublishDirectoryAcceptsRealDirectories(t *testing.T) {
	t.Run("nested build directory", func(t *testing.T) {
		f := newPublishDirFixture(t)
		f.write(t, "co/web/build/index.html", "x")
		if err := checkPublishDirectory(f.checkout, f.checkout, filepath.Join(f.checkout, "web/build")); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	t.Run("real root directory", func(t *testing.T) {
		f := newPublishDirFixture(t)
		f.write(t, "co/app/dist/index.html", "x")
		repo := filepath.Join(f.checkout, "app")
		if err := checkPublishDirectory(f.checkout, repo, f.distDirectory(t, repo, "dist")); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
}

func TestCheckPublishDirectoryRefusesOutsideRepo(t *testing.T) {
	// getDistDirectory trims only one leading `.` and then one `/`, so these
	// spellings still climb out of the repository after trimming.
	for _, publish := range []string{"../../outside", "./../outside"} {
		t.Run(publish, func(t *testing.T) {
			f := newPublishDirFixture(t)
			err := checkPublishDirectory(f.checkout, f.checkout, f.distDirectory(t, f.checkout, publish))
			f.assertRefused(t, err, errPublishDirectoryOutsideRepo)
		})
	}
	t.Run("root directory with ../build", func(t *testing.T) {
		f := newPublishDirFixture(t)
		f.write(t, "co/app/index.html", "x")
		f.write(t, "co/build/index.html", "x")
		repo := filepath.Join(f.checkout, "app")
		err := checkPublishDirectory(f.checkout, repo, filepath.Join(repo, "../build"))
		f.assertRefused(t, err, errPublishDirectoryOutsideRepo)
	})
}

func TestCheckPublishDirectoryRefusesRepoRoot(t *testing.T) {
	f := newPublishDirFixture(t)
	f.mkdir(t, "co/build")
	err := checkPublishDirectory(f.checkout, f.checkout, f.checkout+"/build/..")
	f.assertRefused(t, err, errPublishDirectoryIsRepoRoot)
}

func TestCheckPublishDirectoryRefusesGitDir(t *testing.T) {
	f := newPublishDirFixture(t)
	f.mkdir(t, "co/build")
	err := checkPublishDirectory(f.checkout, f.checkout, f.checkout+"/build/../.git")
	f.assertRefused(t, err, errPublishDirectoryInGitDir)
}

func TestCheckPublishDirectoryRefusesSymlinks(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, f publishDirFixture)
		root    string
		publish string
	}{
		{
			name: "parent links outside",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "outside/site/index.html", "x")
				f.symlink(t, f.outside, "co/link")
			},
			publish: "link/site",
		},
		{
			name: "dist links to build inside the repo",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "co/build/index.html", "x")
				f.symlink(t, "build", "co/dist")
			},
			publish: "dist",
		},
		{
			name: "dist links to a config directory",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "co/config/index.html", "x")
				f.write(t, "co/config/secret.env", "TOKEN=1")
				f.symlink(t, "config", "co/dist")
			},
			publish: "dist",
		},
		{
			name: "parent links to config",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "co/config/site/index.html", "x")
				f.symlink(t, "config", "co/out")
			},
			publish: "out/site",
		},
		{
			name: "dist links to the repo root",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "co/index.html", "x")
				f.symlink(t, ".", "co/dist")
			},
			publish: "dist",
		},
		{
			name: "dist links to .git",
			setup: func(t *testing.T, f publishDirFixture) {
				f.symlink(t, ".git", "co/dist")
			},
			publish: "dist",
		},
		{
			name: "nested links into .git",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "co/.git/site/index.html", "x")
				f.symlink(t, ".git/site", "co/nested")
			},
			publish: "nested",
		},
		{
			name: "root directory links outside",
			setup: func(t *testing.T, f publishDirFixture) {
				f.write(t, "outside/site/index.html", "x")
				f.symlink(t, f.outside, "co/web")
			},
			root:    "web",
			publish: "site",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublishDirFixture(t)
			tc.setup(t, f)
			repo := f.checkout
			if tc.root != "" {
				repo = filepath.Join(f.checkout, tc.root)
			}
			err := checkPublishDirectory(f.checkout, repo, f.distDirectory(t, repo, tc.publish))
			f.assertRefused(t, err, errPublishDirectoryHasSymlink)
		})
	}
}

func TestCheckPublishDirectoryRefusesMissingDirectory(t *testing.T) {
	f := newPublishDirFixture(t)
	err := checkPublishDirectory(f.checkout, f.checkout, filepath.Join(f.checkout, "dist"))
	f.assertRefused(t, err, errPublishDirectoryNotFound)
}
