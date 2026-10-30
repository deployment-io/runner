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
	"github.com/deployment-io/deployment-runner/jobs/resources"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/moby/go-archive"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"
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

	// Memory / CPU caps on the image build. Until these were added the
	// build was the ONLY heavy workload on the runner with no limit at
	// all: agentbox and the static-site build container were both capped,
	// while `docker build` ran unbounded. That matters more than it looks
	// because an image build executes inside DOCKERD, not in the runner's
	// cgroup — so it is invisible to ECS accounting and to every
	// runner-side limit, and a heavy build could consume the whole host
	// and let the kernel OOM-killer pick a victim (potentially dockerd or
	// the runner itself, killing every in-flight job with no useful
	// error).
	//
	// Sized from the host and deliberately generous: this path has been
	// unbounded for a long time, so a cap that is too tight turns builds
	// that succeed today into failures. BUILD_IMAGE_MEMORY_BYTES is the
	// escape hatch for a build that legitimately needs more.
	buildMemoryBytes, buildCores := resources.LimitsForImageBuild()

	opts := client.ImageBuildOptions{
		Dockerfile: dockerFile,
		Tags:       []string{dockerImageNameAndTag},
		// Remove discards the intermediate build containers, not the image
		// this build produces — that one is untagged after it reaches ECR
		// (UploadDockerImageToEcr.pushAndReclaim) and, if the runner died
		// before it got there, by the startup sweep.
		Remove:    true,
		BuildArgs: buildArgs,
		Memory:    buildMemoryBytes,
		// MemorySwap == Memory disables swap for the build. Without this
		// Docker grants swap equal to twice Memory, which turns a memory
		// overrun into minutes of thrashing instead of a fast, clearly
		// attributed failure.
		MemorySwap: buildMemoryBytes,
		// ImageBuildOptions expresses CPU as a CFS quota/period pair in
		// MICROSECONDS, unlike the NanoCPUs used by ContainerCreate
		// elsewhere in this package. quota = cores * period.
		CPUPeriod: resources.CPUPeriodMicroseconds,
		CPUQuota:  buildCores * resources.CPUPeriodMicroseconds,
		// Pin the classic builder EXPLICITLY. Memory, MemorySwap, CPUPeriod
		// and CPUQuota above are honoured only by the v1 builder; BuildKit
		// silently ignores them. Left unset, the builder is chosen by the
		// daemon, so a host with `features: {"buildkit": true}` in
		// daemon.json would run this build completely unbounded while every
		// comment here, and admission control, assumed it was capped -- the
		// exact host-OOM path this cap exists to close, in the one place
		// nobody would look for it.
		Version: build.BuilderV1,
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

// classicImageBuild and buildkitImageBuild are the two builds Run chooses
// between; vars so tests can see which one ran.
var (
	classicImageBuild  = imageBuild
	buildkitImageBuild = buildImageWithBuildkit
)

func (b *BuildDockerImage) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()
	cli, err := client.New(client.FromEnv)
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
	// Build secrets need BuildKit (RUN --mount=type=secret); every other
	// build stays on the classic builder. A Task preview's parameters never
	// carry them.
	buildSecrets, _ := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.BuildSecrets)
	if len(buildSecrets) > 0 {
		var deploymentID string
		deploymentID, err = jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
		if err != nil {
			return parameters, err
		}
		err = buildkitImageBuild(parameters, buildkitRequest{
			deploymentID:    deploymentID,
			repoDir:         repoDirectoryPath,
			dockerFile:      dockerFile,
			imageNameAndTag: dockerImageNameAndTag,
			secrets:         buildSecrets,
		}, logsWriter)
		if err != nil {
			return parameters, err
		}
		return parameters, nil
	}
	io.WriteString(logsWriter, fmt.Sprintf("Building docker image\n"))
	err = classicImageBuild(parameters, cli, repoDirectoryPath, dockerImageNameAndTag, dockerFile, logsWriter)
	if err != nil {
		return parameters, err
	}

	return parameters, nil
}
