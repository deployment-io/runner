package commands

import (
	"bytes"
	"context"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"os"
	"os/exec"
	"strings"
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

// decodes envVariables map to key=value slice
func decodeEnvironmentVariablesToSlice(envVariables string) ([]string, error) {
	var envVariablesSlice []string
	variableEntries := strings.Split(envVariables, "\n")
	for _, entry := range variableEntries {
		if len(entry) == 0 {
			continue
		}
		keyValue := strings.Split(entry, "=")
		if len(keyValue) != 2 {
			return nil, fmt.Errorf("env variables not in correct format")
		}
		envVariablesSlice = append(envVariablesSlice, fmt.Sprintf("%s=%s", keyValue[0], keyValue[1]))
	}
	return envVariablesSlice, nil
}

func (b *BuildStaticSite) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
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
		envVariablesSlice, err = decodeEnvironmentVariablesToSlice(envVariables)
		if err != nil {
			return parameters, err
		}
	}

	//install node version, npm install, and build
	if err = b.executeCommand(logBuffer, envVariablesSlice, []string{"bash", "-c", "source $HOME/.nvm/nvm.sh ; nvm install " + nodeVersion + " ; npm install ; " + buildCommand}, repoDirectoryPath); err != nil {
		return parameters, err
	}

	return parameters, nil
}
