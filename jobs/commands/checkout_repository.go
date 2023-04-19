package commands

import (
	"bytes"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	git "gopkg.in/src-d/go-git.v4"
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

func (cr *CheckoutRepository) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger, jobContext *jobs.ContextV1) (map[parameters_enums.Key]interface{}, error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		err := loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			//TODO send message back
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

		if err != nil && err != git.NoErrAlreadyUpToDate {
			return parameters, err
		}

		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewRemoteReferenceName("origin", repoBranch),
		})

		if err != nil {
			return parameters, err
		}

		parameters[parameters_enums.RepoDirectoryPath] = repoDirectoryPath
	} else {
		return parameters, fmt.Errorf("clone URL doesn't start with https://")
	}

	return parameters, nil
}
