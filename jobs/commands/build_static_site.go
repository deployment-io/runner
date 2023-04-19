package commands

import (
	"bytes"
	"context"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/deployment-io/jobs-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/jobs-runner-kit/jobs"
	"os/exec"
	"time"
)

type BuildStaticSite struct {
}

func (b *BuildStaticSite) executeCommand(logBuffer *bytes.Buffer, commandAndArgs []string, directoryPath string) error {
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
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func (b *BuildStaticSite) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger, jobContext *jobs.ContextV1) (map[parameters_enums.Key]interface{}, error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		err := loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			//TODO send message back
		}
	}()

	repoDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return parameters, err
	}
	//TODO handle root directory - needs to be added to repo directory

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

	//install node version, npm install, and build
	if err := b.executeCommand(logBuffer, []string{"bash", "-c", "source $HOME/.nvm/nvm.sh ; nvm install " + nodeVersion + " ; npm install ; " + buildCommand}, repoDirectoryPath); err != nil {
		return parameters, err
	}

	return parameters, nil
}
