package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"io"
	"time"
)

type BuildDockerImage struct {
}

type ErrorLine struct {
	Error       string      `json:"error"`
	ErrorDetail ErrorDetail `json:"errorDetail"`
}

type ErrorDetail struct {
	Message string `json:"message"`
}

func printBodyToLog(rd io.Reader) error {
	var lastLine string

	scanner := bufio.NewScanner(rd)
	for scanner.Scan() {
		//TODO handle logging
		lastLine = scanner.Text()
		fmt.Println(scanner.Text())
	}

	errLine := &ErrorLine{}
	err := json.Unmarshal([]byte(lastLine), errLine)
	if err != nil {
		return err
	}
	if errLine.Error != "" {
		return errors.New(errLine.Error)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func imageBuild(parameters map[string]interface{}, dockerClient *client.Client, repoDir, dockerImageNameAndTag, dockerFile string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*500)
	defer cancel()
	tar, err := archive.TarWithOptions(repoDir, &archive.TarOptions{
		ExcludePatterns: []string{},
	})
	if err != nil {
		return err
	}

	buildArgs, err := commandUtils.GetDockerBuildArgs(parameters)
	if err != nil {
		return err
	}

	opts := types.ImageBuildOptions{
		Dockerfile: dockerFile,
		Tags:       []string{dockerImageNameAndTag},
		Remove:     true,
		BuildArgs:  buildArgs,
	}
	res, err := dockerClient.ImageBuild(ctx, tar, opts)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	err = printBodyToLog(res.Body)
	if err != nil {
		return err
	}

	return nil
}

func (b *BuildDockerImage) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		_ = loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			markBuildDone(parameters, err)
		}
	}()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return parameters, err
	}

	repoDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return parameters, err
	}

	//TODO support for only Dockerfile for now
	dockerFile := "Dockerfile"
	var dockerImageNameAndTag string
	dockerImageNameAndTag, err = getDockerImageNameAndTag(parameters)
	if err != nil {
		return parameters, err
	}
	err = imageBuild(parameters, cli, repoDirectoryPath, dockerImageNameAndTag, dockerFile)
	if err != nil {
		return parameters, err
	}

	return parameters, nil
}
