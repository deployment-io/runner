package commands

import (
	"bytes"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	"strings"
)

type CheckoutRepository struct {
}

func (cr *CheckoutRepository) getRepositoryDirectoryPath(parameters map[parameters_enums.Key]interface{}) (string, error) {
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

func (cr *CheckoutRepository) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {
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
		repoDirectoryPath, err := cr.getRepositoryDirectoryPath(parameters)
		if err != nil {
			return parameters, err
		}
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
					return parameters, nil
				}
			} else {
				return parameters, err
			}
		}

		worktree, err := repository.Worktree()
		if err != nil {
			return parameters, err
		}

		err = repository.Fetch(&git.FetchOptions{
			Auth: &http.BasicAuth{
				Username: "oauth2",
				Password: repoProviderToken,
			},
			RemoteName: "origin",
			Progress:   logBuffer,
			Force:      true,
		})

		//TODO check for oauth error and get new token from server

		if err != nil && err != git.NoErrAlreadyUpToDate {
			return parameters, err
		}

		referenceName := plumbing.NewRemoteReferenceName("origin", repoBranch)

		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: referenceName,
		})

		if err != nil {
			return parameters, err
		}

		reference, err := repository.Reference(referenceName, true)
		hash := reference.Hash()
		commitObject, err := repository.CommitObject(hash)
		if err != nil {
			return parameters, err
		}
		commitHash := hash.String()
		commitMessage := strings.TrimSpace(commitObject.Message)

		parameters[parameters_enums.RepoDirectoryPath] = repoDirectoryPath

		buildID, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
		if err != nil {
			return parameters, err
		}

		updateBuildsPipeline.Add("updateBuilds", builds.UpdateBuildDtoV1{
			ID:            buildID,
			CommitHash:    commitHash,
			CommitMessage: commitMessage,
			Status:        build_enums.Running,
		})
	} else {
		return parameters, fmt.Errorf("clone URL doesn't start with https://")
	}

	return parameters, nil
}
