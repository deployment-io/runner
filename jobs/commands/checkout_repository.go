package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// CheckoutRepository is responsible for managing the cloning and updating of repository branches and commits.
type CheckoutRepository struct {
}

// addFile creates a file at the specified filePath with the provided contents. Returns an error if file creation or writing fails.
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

// addRootDirectory appends a root directory from parameters to the repository directory path if specified.
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

// Run clones a specified repository, checks out the designated branch or commit, updates submodules, and updates build or preview status.
// It takes a map of parameters and a writer for logs, returning a modified set of parameters and an error if any occurs during execution.
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

	repoCloneUrlWithToken, err := commandUtils.GetRepoUrlWithToken(repoGitProvider, repoProviderToken, repoCloneUrl)
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Checking out branch %s for repository: %s\n", repoBranch, repoCloneUrl))

	var repoDirectoryPath string
	repoDirectoryPath, err = commandUtils.GetRepositoryDirectoryPath(parameters)
	if err != nil {
		return parameters, err
	}
	var repository *git.Repository
	repository, err = commandUtils.CloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider, logsWriter)
	if err != nil {
		if commandUtils.IsErrorAuthenticationRequired(err) {
			repoProviderToken, err = commandUtils.RefreshGitToken(parameters)
			if err != nil {
				return parameters, err
			}
			repoCloneUrlWithToken, err = commandUtils.GetRepoUrlWithToken(repoGitProvider, repoProviderToken, repoCloneUrl)
			if err != nil {
				return parameters, err
			}
			repository, err = commandUtils.CloneRepository(repoDirectoryPath, repoCloneUrlWithToken, repoProviderToken, repoGitProvider, logsWriter)
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

	err = commandUtils.FetchRepository(repository, repoProviderToken, repoGitProvider, logsWriter)
	if err != nil {
		if commandUtils.IsErrorAuthenticationRequired(err) {
			repoProviderToken, err = commandUtils.RefreshGitToken(parameters)
			if err != nil {
				return parameters, err
			}
			err = commandUtils.FetchRepository(repository, repoProviderToken, repoGitProvider, logsWriter)
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

	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
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
		username := commandUtils.GetUsernameForProvider(repoGitProvider)
		// Ensure submodules are updated after the checkout
		err = commandUtils.UpdateSubmodules(repository, username, repoProviderToken, logsWriter)
		if err != nil {
			return parameters, err
		}
		if !isPreview {
			//update build
			commandUtils.UpdateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
				ID:     buildOrPreviewID,
				Status: build_enums.Running,
			})
		} else {
			//update preview
			commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
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
		username := commandUtils.GetUsernameForProvider(repoGitProvider)
		// Ensure submodules are updated after the checkout
		err = commandUtils.UpdateSubmodules(repository, username, repoProviderToken, logsWriter)
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
			commandUtils.UpdateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
				ID:            buildOrPreviewID,
				CommitHash:    commitHash,
				CommitMessage: commitMessage,
				Status:        build_enums.Running,
			})
		} else {
			commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
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

	return parameters, nil
}
