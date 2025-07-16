package utils

import (
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/git_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"io"
	"strings"
	"time"
)

// GetRepoUrlWithToken generates a repository URL with an embedded OAuth token for authentication with the given git provider.
func GetRepoUrlWithToken(gitProvider, repoProviderToken, repoCloneUrl string) (string, error) {
	switch gitProvider {
	case git_provider_enums.GitHub.String():
		after, found := strings.CutPrefix(repoCloneUrl, "https://")
		if !found {
			return "", errors.New("could not parse git provider url")
		}
		return "https://oauth2:" + repoProviderToken + "@" + after, nil
	case git_provider_enums.GitLab.String():
		after, found := strings.CutPrefix(repoCloneUrl, "https://")
		if !found {
			return "", errors.New("could not parse git provider url")
		}
		return "https://oauth2:" + repoProviderToken + "@" + after, nil
	case git_provider_enums.BitBucket.String():
		after, found := strings.CutPrefix(repoCloneUrl, "https://")
		if !found {
			return "", errors.New("could not parse git provider url")
		}
		_, after, found = strings.Cut(after, "@bitbucket.org")
		if found {
			return "https://x-token-auth:" + repoProviderToken + "@bitbucket.org" + after, nil
		}
		return "https://x-token-auth:" + repoProviderToken + "@" + after, nil

	//git clone https://x-token-auth:{access_token}@bitbucket.org/user/repo.git
	//https://arorankit@bitbucket.org/arorankit/dezyna.git
	default:
		return "", errors.New("unknown git provider type")
	}
}

// GetRepositoryDirectoryPath returns the directory path for the repository based on organization and build ID parameters.
func GetRepositoryDirectoryPath(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	automationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.AutomationID)
	if err == nil && len(automationID) > 0 {
		jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
		if err == nil && len(jobID) > 0 {
			return fmt.Sprintf("/tmp/%s/%s/%s", organizationID, automationID, jobID), nil
		}
	}
	buildID, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/tmp/%s/%s", organizationID, buildID), nil
}

// IsErrorAuthenticationRequired checks if the provided error indicates an authentication requirement.
func IsErrorAuthenticationRequired(err error) bool {
	if err.Error() == "authentication required" {
		return true
	}
	return false
}

// FetchRepository fetches the latest changes of the provided repository using the given authentication tokens and logs progress.
func FetchRepository(repository *git.Repository, repoProviderToken, repoGitProvider string, logsWriter io.Writer) error {
	username := GetUsernameForProvider(repoGitProvider)

	err := repository.Fetch(&git.FetchOptions{
		Auth: &http.BasicAuth{
			Username: username,
			Password: repoProviderToken,
		},
		RemoteName: "origin",
		Progress:   logsWriter,
		Force:      true,
	})

	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}

	// Update submodules
	err = UpdateSubmodules(repository, username, repoProviderToken, logsWriter)
	if err != nil {
		return err
	}

	return nil
}

// RefreshGitToken refreshes the Git token using provided parameters with installation ID and organization ID.
func RefreshGitToken(parameters map[string]interface{}) (string, error) {
	installationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.InstallationId)
	if err != nil {
		return "", err
	}
	orgIdFromJob, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return "", err
	}
	token, err := client.Get().RefreshGitToken(installationID, orgIdFromJob)
	if err == nil {
		return token, nil
	}
	for errors.Is(err, oauth.ErrRefreshInProcess) {
		time.Sleep(10 * time.Second)
		token, err = client.Get().RefreshGitToken(installationID, orgIdFromJob)
		if err == nil {
			return token, nil
		}
	}
	return "", err
}

// UpdateSubmodules initializes and updates all submodules in a given Git repository.
// repository: the Git repository containing submodules to update.
// username: the username for authentication with the repository provider.
// repoProviderToken: the token used for authentication with the repository provider.
// logsWriter: the writer where log messages will be output.
// Returns an error if it fails to initialize or update any submodule.
func UpdateSubmodules(repository *git.Repository, username, repoProviderToken string, logsWriter io.Writer) error {
	wt, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("error updating submodules: %s", err)
	}

	// Get the submodules
	submodules, err := wt.Submodules()
	if err != nil {
		return fmt.Errorf("error updating submodules: %s", err)
	}

	for _, submodule := range submodules {
		// ignore err if submodule is already initialized
		err = submodule.Init()
		if err != nil {
			// Handle specific error if needed
			if errors.Is(err, git.ErrSubmoduleAlreadyInitialized) {
				//io.WriteString(logsWriter, fmt.Sprintf("Submodule %s is already initialized\n", submodule.Config().Path))
			} else {
				return fmt.Errorf("error updating submodules: %s", err)
			}
		}

		// Update each submodule
		err = submodule.Update(&git.SubmoduleUpdateOptions{
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
			Auth: &http.BasicAuth{
				Username: username,
				Password: repoProviderToken,
			},
		})
		if err != nil {
			return fmt.Errorf("error updating submodule: %s: %s", submodule.Config().Path, err)
		}

		io.WriteString(logsWriter, fmt.Sprintf("Submodule %s updated successfully\n", submodule.Config().Path))
	}

	return nil
}

// CloneRepository clones a Git repository to the specified directory path.
// It supports authentication using a provided token and handles submodule initialization and updating.
// If the repository already exists locally, it opens the existing repository instead of cloning it.
func CloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider string, logsWriter io.Writer) (*git.Repository, error) {
	username := GetUsernameForProvider(repoGitProvider)

	repository, err := git.PlainClone(repoDirectoryPath, false, &git.CloneOptions{
		URL:      repoCloneUrlWithToken,
		Progress: logsWriter,
		Auth: &http.BasicAuth{
			Username: username,
			Password: repoProviderToken,
		},
		RecurseSubmodules: git.DefaultSubmoduleRecursionDepth, // Ensure submodules are initialized
	})

	if err != nil {
		if err == git.ErrRepositoryAlreadyExists {
			repository, err = git.PlainOpen(repoDirectoryPath)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// Initialize and update submodules
	err = UpdateSubmodules(repository, username, repoProviderToken, logsWriter)
	if err != nil {
		return nil, err
	}

	return repository, nil
}

// GetUsernameForProvider returns the appropriate username for the specified Git provider.
// For BitBucket, it returns "x-token-auth"; otherwise, it returns "oauth2".
func GetUsernameForProvider(repoGitProvider string) string {
	if repoGitProvider == git_provider_enums.BitBucket.String() {
		return "x-token-auth"
	}
	return "oauth2"
}
