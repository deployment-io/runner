package commands

import (
	"bytes"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	"os"
	"strings"
	"time"
)

type CheckoutRepository struct {
}

func getRepositoryDirectoryPath(parameters map[string]interface{}) (string, error) {
	environmentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentID)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	repoGitProvider, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoGitProvider)
	if err != nil {
		return "", err
	}
	repoName, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoName)
	if err != nil {
		return "", err
	}
	repoName = strings.ReplaceAll(repoName, " ", "")

	return fmt.Sprintf("/tmp/%s/%s/%s/%s/%s", organizationID, environmentID, deploymentID, repoGitProvider, repoName), nil

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

func cloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken string, logBuffer *bytes.Buffer) (*git.Repository, error) {
	var repository *git.Repository
	repository, err := git.PlainClone(repoDirectoryPath, false, &git.CloneOptions{
		URL:      repoCloneUrlWithToken,
		Progress: logBuffer,
		Auth: &http.BasicAuth{
			Username: "oauth2",
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

func fetchRepository(repository *git.Repository, repoProviderToken string, logBuffer *bytes.Buffer) error {
	err := repository.Fetch(&git.FetchOptions{
		Auth: &http.BasicAuth{
			Username: "oauth2",
			Password: repoProviderToken,
		},
		RemoteName: "origin",
		Progress:   logBuffer,
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
	token, err := client.Get().RefreshGitToken(installationID)
	if err == nil {
		return token, nil
	}
	for err == oauth.ErrRefreshInProcess {
		time.Sleep(10 * time.Second)
		token, err = client.Get().RefreshGitToken(installationID)
		if err == nil {
			return token, nil
		}
	}
	return "", err
}

func (cr *CheckoutRepository) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		_ = loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			markBuildDone(parameters, err)
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

	after, found := strings.CutPrefix(repoCloneUrl, "https://")
	if found {
		//TODO currently it's common for GitLab and GitHub. Might change in the future
		repoCloneUrlWithToken := "https://oauth2:" + repoProviderToken + "@" + after
		var repoDirectoryPath string
		repoDirectoryPath, err = getRepositoryDirectoryPath(parameters)
		if err != nil {
			return parameters, err
		}
		var repository *git.Repository
		repository, err = cloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, logBuffer)
		if err != nil {
			if isErrorAuthenticationRequired(err) {
				repoProviderToken, err = refreshGitToken(parameters)
				if err != nil {
					return parameters, err
				}
				repoCloneUrlWithToken = "https://oauth2:" + repoProviderToken + "@" + after
				repository, err = cloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, logBuffer)
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

		err = fetchRepository(repository, repoProviderToken, logBuffer)
		if err != nil {
			if isErrorAuthenticationRequired(err) {
				repoProviderToken, err = refreshGitToken(parameters)
				if err != nil {
					return parameters, err
				}
				err = fetchRepository(repository, repoProviderToken, logBuffer)
				if err != nil {
					return parameters, err
				}
				jobs.SetParameterValue(parameters, parameters_enums.RepoProviderToken, repoProviderToken)
			} else {
				return parameters, err
			}
		}
		var buildID string
		buildID, err = jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
		if err != nil {
			return parameters, err
		}

		var commitHashFromParams string
		commitHashFromParams, err = jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)

		if err == nil && len(commitHashFromParams) > 0 {
			hash := plumbing.NewHash(commitHashFromParams)
			err = worktree.Checkout(&git.CheckoutOptions{
				Hash: hash,
			})
			if err != nil {
				return parameters, err
			}
			updateBuildsPipeline.Add(updateBuildsKey, builds.UpdateBuildDtoV1{
				ID:     buildID,
				Status: build_enums.Running,
			})
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

			updateBuildsPipeline.Add(updateBuildsKey, builds.UpdateBuildDtoV1{
				ID:            buildID,
				CommitHash:    commitHash,
				CommitMessage: commitMessage,
				Status:        build_enums.Running,
			})

			jobs.SetParameterValue(parameters, parameters_enums.CommitHash, commitHash)
		}

		//root directory added to repo directory
		var rootDirectoryPath string
		rootDirectoryPath, err = jobs.GetParameterValue[string](parameters, parameters_enums.RootDirectory)
		if err == nil && len(rootDirectoryPath) > 0 {
			repoDirectoryPath += rootDirectoryPath
		}

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

	} else {
		err = fmt.Errorf("clone URL doesn't start with https://")
		return parameters, err
	}

	return parameters, nil
}
