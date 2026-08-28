package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/docker/docker/api/types"
	"github.com/moby/moby/client"
	"github.com/moby/moby/pkg/archive"
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

type Stream struct {
	Stream string `json:"stream"`
}

func printBodyToLog(rd io.Reader, logsWriter io.Writer) error {
	//ignoring all errors while sending logs
	var lastLine string
	scanner := bufio.NewScanner(rd)
	//scanner.Split(utils.ScanCRLF)
	for scanner.Scan() {
		lastLine = strings.Trim(scanner.Text(), " \n \r")
		//stream := &Stream{}
		//_ = json.Unmarshal([]byte(lastLine), stream)
		//fmt.Print(stream.Stream)
		_, _ = io.WriteString(logsWriter, lastLine)
		_, _ = io.WriteString(logsWriter, "\n")
	}

	errLine := &ErrorLine{}
	_ = json.Unmarshal([]byte(lastLine), errLine)
	if len(errLine.Error) > 0 {
		_, _ = io.WriteString(logsWriter, errLine.Error)
		_, _ = io.WriteString(logsWriter, "\n")
		return errors.New(errLine.Error)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func imageBuild(parameters map[string]interface{}, dockerClient *client.Client, repoDir, dockerImageNameAndTag, dockerFile string, logsWriter io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*1800)
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
		// Remove discards the intermediate build containers, not the image
		// this build produces — that one is untagged after it reaches ECR
		// (UploadDockerImageToEcr.pushAndReclaim) and, if the runner died
		// before it got there, by the startup sweep.
		Remove:    true,
		BuildArgs: buildArgs,
		// Labelled so the sweep can recognize this image as ours once it
		// has been untagged and only its digest is left.
		Labels: reclaim.Labels(reclaim.PurposeDeploymentImage),
	}
	res, err := dockerClient.ImageBuild(ctx, tar, opts)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	err = printBodyToLog(res.Body, logsWriter)
	if err != nil {
		return err
	}

	return nil
}

func (b *BuildDockerImage) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
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

	dockerFile, _ := jobs.GetParameterValue[string](parameters, parameters_enums.DockerFilePath)
	if len(dockerFile) == 0 {
		dockerFile = "Dockerfile"
	}
	var dockerImageNameAndTag string
	dockerImageNameAndTag, err = getDockerImageNameAndTag(parameters)
	if err != nil {
		return parameters, err
	}
	// A build is the largest single consumer of disk on the runner, so the
	// job log says how much room there was going in — the alternative is
	// finding out from an ENOSPC with no context.
	reclaim.LogFreeDisk("before build", logsWriter)
	io.WriteString(logsWriter, fmt.Sprintf("Building docker image\n"))
	err = imageBuild(parameters, cli, repoDirectoryPath, dockerImageNameAndTag, dockerFile, logsWriter)
	if err != nil {
		return parameters, err
	}

	return parameters, nil
}
