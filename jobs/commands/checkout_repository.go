package commands

import (
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/git_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner/client"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	"io"
	"os"
	"strings"
	"time"
)

type CheckoutRepository struct {
}

func getRepositoryDirectoryPath(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	buildID, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/tmp/%s/%s", organizationID, buildID), nil
}

func addFile(filePath, contents string) error {
	//delete file. ignoring error since file may not exist
	_ = os.Remove(filePath)
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	_, err = file.WriteString(contents)
	if err != nil {
		return err
	}
	return nil
}

func isErrorAuthenticationRequired(err error) bool {
	if err.Error() == "authentication required" {
		return true
	}
	return false
}

func cloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider string, logsWriter io.Writer) (*git.Repository, error) {
	username := "oauth2"
	if repoGitProvider == git_provider_enums.BitBucket.String() {
		username = "x-token-auth"
	}
	var repository *git.Repository
	repository, err := git.PlainClone(repoDirectoryPath, false, &git.CloneOptions{
		URL:      repoCloneUrlWithToken,
		Progress: logsWriter,
		Auth: &http.BasicAuth{
			Username: username,
			Password: repoProviderToken,
		},
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
	return repository, nil
}

func fetchRepository(repository *git.Repository, repoProviderToken, repoGitProvider string, logsWriter io.Writer) error {
	username := "oauth2"
	if repoGitProvider == git_provider_enums.BitBucket.String() {
		username = "x-token-auth"
	}
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
	return nil
}

func refreshGitToken(parameters map[string]interface{}) (string, error) {
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

func addRootDirectory(parameters map[string]interface{}, repoDirectoryPath string) string {
	var rootDirectoryPath string
	rootDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RootDirectory)
	if err == nil && len(rootDirectoryPath) > 0 {
		//remove . and/or / from the beginning and / from the end
		rootDirectoryPath = strings.TrimPrefix(rootDirectoryPath, ".")
		rootDirectoryPath = strings.TrimPrefix(rootDirectoryPath, "/")
		rootDirectoryPath = strings.TrimSuffix(rootDirectoryPath, "/")
		if len(rootDirectoryPath) > 0 {
			repoDirectoryPath = fmt.Sprintf("%s/%s", repoDirectoryPath, rootDirectoryPath)
		}
	}
	return repoDirectoryPath
}

func getRepoUrlWithToken(gitProvider, repoProviderToken, repoCloneUrl string) (string, error) {
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

func (cr *CheckoutRepository) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		//_ = loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()
	repoCloneUrl, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoCloneUrl)
	if err != nil {
		return parameters, err
	}

	repoBranch, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoBranch)
	if err != nil {
		return parameters, err
	}

	repoProviderToken, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoProviderToken)
	if err != nil {
		return parameters, err
	}

	repoGitProvider, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoGitProvider)
	if err != nil {
		return parameters, err
	}

	repoCloneUrlWithToken, err := getRepoUrlWithToken(repoGitProvider, repoProviderToken, repoCloneUrl)
	if err != nil {
		return parameters, err
	}

	//after, found := strings.CutPrefix(repoCloneUrl, "https://")
	//if found {
	io.WriteString(logsWriter, fmt.Sprintf("Checking out branch %s for repository: %s\n", repoBranch, repoCloneUrl))

	//repoCloneUrlWithToken := "https://oauth2:" + repoProviderToken + "@" + after

	var repoDirectoryPath string
	repoDirectoryPath, err = getRepositoryDirectoryPath(parameters)
	if err != nil {
		return parameters, err
	}
	var repository *git.Repository
	repository, err = cloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider, logsWriter)
	if err != nil {
		if isErrorAuthenticationRequired(err) {
			repoProviderToken, err = refreshGitToken(parameters)
			if err != nil {
				return parameters, err
			}
			//repoCloneUrlWithToken = "https://oauth2:" + repoProviderToken + "@" + after
			repoCloneUrlWithToken, err = getRepoUrlWithToken(repoGitProvider, repoProviderToken, repoCloneUrl)
			if err != nil {
				return parameters, err
			}
			repository, err = cloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider, logsWriter)
			if err != nil {
				return parameters, err
			}
			jobs.SetParameterValue(parameters, parameters_enums.RepoProviderToken, repoProviderToken)
		} else {
			return parameters, err
		}
	}

	var worktree *git.Worktree
	worktree, err = repository.Worktree()
	if err != nil {
		return parameters, err
	}

	err = fetchRepository(repository, repoProviderToken, repoGitProvider, logsWriter)
	if err != nil {
		if isErrorAuthenticationRequired(err) {
			repoProviderToken, err = refreshGitToken(parameters)
			if err != nil {
				return parameters, err
			}
			err = fetchRepository(repository, repoProviderToken, repoGitProvider, logsWriter)
			if err != nil {
				return parameters, err
			}
			jobs.SetParameterValue(parameters, parameters_enums.RepoProviderToken, repoProviderToken)
		} else {
			return parameters, err
		}
	}
	isPreview := isPreview(parameters)
	var buildOrPreviewID string
	if !isPreview {
		//get buildID
		buildOrPreviewID, err = jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
		if err != nil {
			return parameters, err
		}
	} else {
		//get previewID
		buildOrPreviewID, err = jobs.GetParameterValue[string](parameters, parameters_enums.PreviewID)
		if err != nil {
			return parameters, err
		}
	}

	var commitHashFromParams string
	commitHashFromParams, err = jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)

	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}
	if err == nil && len(commitHashFromParams) > 0 {
		hash := plumbing.NewHash(commitHashFromParams)
		err = worktree.Checkout(&git.CheckoutOptions{
			Hash: hash,
		})
		if err != nil {
			return parameters, err
		}
		if !isPreview {
			//update build
			updateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
				ID:     buildOrPreviewID,
				Status: build_enums.Running,
			})
		} else {
			//update preview
			updatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
				ID:     buildOrPreviewID,
				Status: build_enums.Running,
			})
		}
	} else {
		referenceName := plumbing.NewRemoteReferenceName("origin", repoBranch)
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: referenceName,
		})
		if err != nil {
			return parameters, err
		}
		var reference *plumbing.Reference
		reference, err = repository.Reference(referenceName, true)
		hash := reference.Hash()
		var commitObject *object.Commit
		commitObject, err = repository.CommitObject(hash)
		if err != nil {
			return parameters, err
		}
		commitHash := hash.String()
		commitMessage := strings.TrimSpace(commitObject.Message)

		if !isPreview {
			updateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
				ID:            buildOrPreviewID,
				CommitHash:    commitHash,
				CommitMessage: commitMessage,
				Status:        build_enums.Running,
			})
		} else {
			updatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
				ID:            buildOrPreviewID,
				CommitHash:    commitHash,
				CommitMessage: commitMessage,
				Status:        build_enums.Running,
			})
		}

		jobs.SetParameterValue(parameters, parameters_enums.CommitHash, commitHash)
	}

	//root directory added to repo directory
	repoDirectoryPath = addRootDirectory(parameters, repoDirectoryPath)

	//add environment files to the source code
	var environmentFiles map[string]string
	environmentFiles, err = jobs.GetParameterValue[map[string]string](parameters, parameters_enums.EnvironmentFiles)
	if err == nil && len(environmentFiles) > 0 {
		//create and add the environment files in repoDirectoryPath
		for name, contents := range environmentFiles {
			filePath := repoDirectoryPath + "/" + name
			err = addFile(filePath, contents)
			if err != nil {
				return parameters, err
			}
		}
	}

	jobs.SetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath, repoDirectoryPath)

	//} else {
	//	err = fmt.Errorf("clone URL doesn't start with https://")
	//	return parameters, err
	//}

	return parameters, nil
}
