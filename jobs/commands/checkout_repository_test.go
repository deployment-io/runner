package commands

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestCloneRepository tests the cloneRepository function
func TestCloneRepository(t *testing.T) {
	repoDirectoryPath, err := ioutil.TempDir("", "test-repo-")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		// Clean up temporary directory
		err = os.RemoveAll(repoDirectoryPath)
		if err != nil {
			t.Fatalf("Failed to remove temporary directory: %v", err)
		}
	}()

	repoCloneUrlWithToken := "https://github.com/your/test-repo"
	repoProviderToken := "test-token"
	repoGitProvider := "github"
	logsWriter := &bytes.Buffer{}

	repo, err := cloneRepository(
		repoDirectoryPath,
		repoCloneUrlWithToken,
		repoProviderToken,
		repoGitProvider,
		logsWriter,
	)
	if err != nil {
		t.Fatalf("Failed to clone repository: %v", err)
	}

	if repo == nil {
		t.Fatal("Expected non-nil repository")
	}
}

// TestFetchRepository tests the fetchRepository function
func TestFetchRepository(t *testing.T) {
	// Set up an in-memory repository for testing
	storage := memory.NewStorage()
	fs := memfs.New()
	repo, err := git.Init(storage, fs)
	if err != nil {
		t.Fatalf("Failed to initialize repository: %v", err)
	}

	// Prepare fake auth and logs writer
	repoProviderToken := "test-token"
	repoGitProvider := "github"
	logsWriter := &bytes.Buffer{}

	// Add a remote to simulate fetching from it
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://github.com/your/test-repo"},
	})
	if err != nil {
		t.Fatalf("Failed to create remote: %v", err)
	}

	err = fetchRepository(
		repo,
		repoProviderToken,
		repoGitProvider,
		logsWriter,
	)
	if err != nil {
		t.Fatalf("Failed to fetch repository: %v", err)
	}
}
