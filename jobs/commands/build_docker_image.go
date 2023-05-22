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
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"strings"
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

func printBody(rd io.Reader) error {
	var lastLine string

	scanner := bufio.NewScanner(rd)
	for scanner.Scan() {
		lastLine = scanner.Text()
		fmt.Println(scanner.Text())
	}

	errLine := &ErrorLine{}
	json.Unmarshal([]byte(lastLine), errLine)
	if errLine.Error != "" {
		return errors.New(errLine.Error)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func convertPrimitiveAToStringSlice(a primitive.A) ([]string, error) {
	var out []string
	for _, s := range a {
		sVal, ok := s.(string)
		if !ok {
			return nil, fmt.Errorf("error convering primitive A to string slice")
		}
		out = append(out, sVal)
	}
	return out, nil
}

func getDockerBuildArgs(parameters map[parameters_enums.Key]interface{}) (map[string]*string, error) {
	dockerBuildArgs, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.DockerBuildArgs)
	if err != nil || len(dockerBuildArgs) == 0 {
		//no docker build args
		return nil, nil
	}
	var dockerBuildArgsMap = make(map[string]*string)
	dockerBuildArgsStrings, err := convertPrimitiveAToStringSlice(dockerBuildArgs)
	if err != nil {
		return nil, fmt.Errorf("error getting doecker build args: %s", err)
	}
	for _, dockerBuildArg := range dockerBuildArgsStrings {
		entry := strings.Split(dockerBuildArg, "=")
		if len(entry) == 2 {
			dockerBuildArgsMap[entry[0]] = &entry[1]
		}
	}
	return dockerBuildArgsMap, nil
}

func imageBuild(parameters map[parameters_enums.Key]interface{}, dockerClient *client.Client, repoDir, dockerImageNameAndTag, dockerFile string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*500)
	defer cancel()
	tar, err := archive.TarWithOptions(repoDir, &archive.TarOptions{
		ExcludePatterns: []string{},
	})
	if err != nil {
		return err
	}

	buildArgs, err := getDockerBuildArgs(parameters)
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

	err = printBody(res.Body)
	if err != nil {
		return err
	}

	return nil
}

func getDockerImageNameAndTag(parameters map[parameters_enums.Key]interface{}) (string, error) {
	//ex. <repo-name>:<commit-hash>
	repoName, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoName)
	if err != nil {
		return "", err
	}
	commitHashFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s", repoName, commitHashFromParams), nil
}

func (b *BuildDockerImage) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {
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

	//root directory added to repo directory
	rootDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RootDirectory)
	if err == nil {
		repoDirectoryPath += rootDirectoryPath
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
