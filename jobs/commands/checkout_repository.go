package commands

import (
	"bytes"
	"fmt"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/deployment-io/jobs-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/jobs-runner-kit/jobs/types"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	"strings"
)

type CheckoutRepository struct {
}

func (cr *CheckoutRepository) Run(parameters map[parameters_enums.Key]interface{}, logger types.Logger) (map[parameters_enums.Key]interface{}, error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		err := loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			//TODO send message back
		}
	}()
	repoCloneUrl, ok := parameters[parameters_enums.RepoCloneUrl]
	if !ok {
		return parameters, fmt.Errorf("repository url is missing")
	}
	repoBranch, ok := parameters[parameters_enums.RepoBranch]
	if !ok {
		return parameters, fmt.Errorf("repository branch is missing")
	}
	repoProviderToken, ok := parameters[parameters_enums.RepoProviderToken]
	if !ok {
		return parameters, fmt.Errorf("repository provider token is missing")
	}

	if repoCloneUrlString, isOK := repoCloneUrl.(string); isOK {
		if repoProviderTokenString, isOK := repoProviderToken.(string); isOK {
			after, found := strings.CutPrefix(repoCloneUrlString, "https://")
			if found {
				//TODO currently it's common for GitLab and GitHub. Might change in the future
				repoCloneUrlWithToken := "https://oauth2:" + repoProviderTokenString + "@" + after
				repoName := parameters[parameters_enums.RepoName]
				if repoNameString, isOK := repoName.(string); isOK {
					repoGitProvider := parameters[parameters_enums.RepoGitProvider]
					if repoGitProviderString, isOK := repoGitProvider.(string); isOK {
						repoNameString = strings.ReplaceAll(repoNameString, " ", "")
						repoDirectoryPath := "/tmp/" + repoGitProviderString + "/" + repoNameString
						repository, err := git.PlainClone(repoDirectoryPath, false, &git.CloneOptions{
							URL:      repoCloneUrlWithToken,
							Progress: logBuffer,
							Auth: &http.BasicAuth{
								Username: "oauth2",
								Password: repoProviderTokenString,
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
						if repoBranchString, isOk := repoBranch.(string); isOk {
							err = repository.Fetch(&git.FetchOptions{
								Auth: &http.BasicAuth{
									Username: "oauth2",
									Password: repoProviderTokenString,
								},
								RemoteName: "origin",
								Progress:   logBuffer,
								Force:      true,
							})

							if err != nil && err != git.NoErrAlreadyUpToDate {
								return parameters, err
							}

							err = worktree.Checkout(&git.CheckoutOptions{
								Branch: plumbing.NewRemoteReferenceName("origin", repoBranchString),
							})

							if err != nil {
								return parameters, err
							}
						}
					}
				}
			} else {
				//clone url doesn't start with https://
				//TODO send message back
			}

		}
	}

	return parameters, nil
}
