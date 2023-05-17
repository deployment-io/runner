package commands

import (
	"bytes"
	"context"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"os"
	"os/exec"
	"time"
)

type BuildStaticSite struct {
}

func (b *BuildStaticSite) executeCommand(logBuffer *bytes.Buffer, envVariablesSlice []string, commandAndArgs []string, directoryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if len(commandAndArgs) == 1 {
		cmd = exec.CommandContext(ctx, commandAndArgs[0])
	} else {
		cmd = exec.CommandContext(ctx, commandAndArgs[0], commandAndArgs[1:]...)
	}
	cmd.Dir = directoryPath
	cmd.Stdout = logBuffer
	if len(envVariablesSlice) > 0 {
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, envVariablesSlice...)
	}
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
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

func (b *BuildStaticSite) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		//ignore
		_ = loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			markBuildDone(parameters, err)
		}
	}()

	repoDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return parameters, err
	}

	//TODO handle root directory - needs to be added to repo directory
	rootDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RootDirectory)
	if err == nil {
		repoDirectoryPath += rootDirectoryPath
	}

	environmentFiles, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.EnvironmentFiles)
	if err == nil {
		//create and add the environment files in repoDirectoryPath
		for name, contents := range environmentFiles {
			filePath := repoDirectoryPath + "/" + name
			err = addFile(filePath, contents)
			if err != nil {
				return parameters, err
			}
		}
	}

	buildCommand, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildCommand)
	if err != nil {
		return parameters, err
	}

	nodeVersion, err := jobs.GetParameterValue[string](parameters, parameters_enums.NodeVersion)
	if err != nil {
		return parameters, nil
	}

	//if node version is missing install and use latest lts
	if len(nodeVersion) == 0 {
		nodeVersion = "--lts"
	}

	////rm node modules - clean up after deployment
	//if err := b.executeCommand(logBuffer, []string{"rm", "-rf", "node_modules"}, repoDirectoryPath); err != nil {
	//	return parameters, err
	//}
	//
	////rm publish folder
	//if err := b.executeCommand(logBuffer, []string{"bash", "-c", "source $HOME/.nvm/nvm.sh ; nvm install " + nodeVersion + " l "}, repoDirectoryPath); err != nil {
	//	return parameters, err
	//}

	envVariables, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentVariables)
	var envVariablesSlice []string
	if err == nil {
		envVariablesSlice = jobs.DecodeEnvironmentVariablesToSlice(envVariables)
	}

	//install node version, npm install, and build
	if err = b.executeCommand(logBuffer, envVariablesSlice, []string{"bash", "-c", "source $HOME/.nvm/nvm.sh ; nvm install " + nodeVersion + " ; npm install ; " + buildCommand}, repoDirectoryPath); err != nil {
		return parameters, err
	}

	parameters[parameters_enums.RepoDirectoryPath] = repoDirectoryPath

	return parameters, nil
}
