package commands

import (
	"context"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type BuildStaticSite struct {
}

func (b *BuildStaticSite) executeCommand(logsWriter io.Writer, envVariablesSlice []string, commandAndArgs []string, directoryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if len(commandAndArgs) == 1 {
		cmd = exec.CommandContext(ctx, commandAndArgs[0])
	} else {
		cmd = exec.CommandContext(ctx, commandAndArgs[0], commandAndArgs[1:]...)
	}
	cmd.Dir = directoryPath
	cmd.Stdout = logsWriter
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

func execCommand(containerID, repoDir string, command []string, env []string, logsWriter io.Writer) error {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	config := types.ExecConfig{
		AttachStderr: true,
		AttachStdout: true,
		WorkingDir:   repoDir,
		Cmd:          command,
		Env:          env,
	}

	idResponse, err := cli.ContainerExecCreate(ctx, containerID, config)
	if err != nil {
		return err
	}

	execID := idResponse.ID

	resp, err := cli.ContainerExecAttach(ctx, execID,
		types.ExecStartCheck{
			Detach: false,
			Tty:    false,
		},
	)
	defer resp.Close()

	io.Copy(logsWriter, resp.Reader)

	res, err := cli.ContainerExecInspect(ctx, execID)
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("error running command: %v", command)
	}

	return nil
}

var imagePullLock = sync.Mutex{}

func pullDockerImageForBuilding(imageID string) error {
	imagePullLock.Lock()
	defer imagePullLock.Unlock()
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	reader, err := cli.ImagePull(ctx, fmt.Sprintf("docker.io/library/%s", imageID), types.ImagePullOptions{})
	if err != nil {
		return err
	}

	defer reader.Close()

	_, err = io.ReadAll(reader)
	if err != nil {
		return err
	}

	return nil
}

func startBuildContainer(imageId, repoDir string) (string, error) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", err
	}
	defer cli.Close()

	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: imageId,
		Cmd:   []string{"tail", "-f", "/dev/null"},
		Tty:   false,
	}, &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: repoDir,
			Target: repoDir,
		}},
	}, nil, nil, "")
	if err != nil {
		return "", err
	}

	if err = cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return "", err
	}

	return resp.ID, nil
}

func stopContainer(containerID string) error {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	t := 1
	err = cli.ContainerStop(ctx, containerID, container.StopOptions{
		Timeout: &t,
	})
	if err != nil {
		return err
	}
	return nil
}

func (b *BuildStaticSite) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			markBuildDone(parameters, err, logsWriter)
		}
	}()
	repoDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Building static site\n"))

	//checking if package.json file exists
	if _, err = os.Stat(repoDirectoryPath + "/package.json"); err != nil {
		if os.IsNotExist(err) {
			io.WriteString(logsWriter, fmt.Sprintf("package.json file doesn't exists in root directory\n"))
			return parameters, err
		} else {
			return parameters, err
		}
	}

	buildCommand, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildCommand)
	if err != nil {
		return parameters, err
	}

	nodeVersion, err := jobs.GetParameterValue[string](parameters, parameters_enums.NodeVersion)
	if err != nil {
		return parameters, err
	}

	//if node version is missing install and use latest lts
	//get node docker image id according to node version
	imageId := "node:lts-buster"
	if len(nodeVersion) == 0 {
		nodeVersion = "--lts"
	}

	envVariables, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentVariables)
	var envVariablesSlice []string
	if err == nil {
		envVariablesSlice, err = decodeEnvironmentVariablesToSlice(envVariables)
		if err != nil {
			return parameters, err
		}
	}

	err = pullDockerImageForBuilding(imageId)
	if err != nil {
		return parameters, err
	}

	containerID, err := startBuildContainer(imageId, repoDirectoryPath)
	if err != nil {
		return parameters, err
	}

	defer func() {
		stopContainer(containerID)
	}()

	err = execCommand(containerID, repoDirectoryPath, []string{"bash", "-c", "npm install;" + buildCommand}, envVariablesSlice, logsWriter)
	//err = execCommand(containerID, repoDirectoryPath, []string{"bash", "-c", "npm install; npm run clean; npm run build"}, envVariablesSlice, logsWriter)

	if err != nil {
		return parameters, err
	}

	//if err = b.executeCommand(logsWriter, envVariablesSlice, []string{"bash", "-c", "source $HOME/.nvm/nvm.sh ; nvm install " + nodeVersion + " ; npm install ; " + buildCommand}, repoDirectoryPath); err != nil {
	//	return parameters, err
	//}

	return parameters, nil
}
