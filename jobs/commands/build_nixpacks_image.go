package commands

import (
	"context"
	"fmt"
	"github.com/ankit-arora/nixpacks-go"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/docker/docker/api/types/registry"
	"github.com/moby/moby/client"
	"io"
)

type BuildNixPacksImage struct {
}

func (b *BuildNixPacksImage) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()

	repoDirectoryPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return parameters, err
	}
	var dockerImageNameAndTag string
	dockerImageNameAndTag, err = getDockerImageNameAndTag(parameters)
	if err != nil {
		return parameters, err
	}

	runtimeInt, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Runtime)
	if err != nil {
		return parameters, err
	}
	runtime := deployment_enums.Runtime(runtimeInt).String()

	io.WriteString(logsWriter, fmt.Sprintf("Building %s application\n", runtime))

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return parameters, err
	}

	nixPacksUsername, err := jobs.GetParameterValue[string](parameters, parameters_enums.NixPacksUsernameKey)
	if err != nil {
		return parameters, err
	}

	nixPacksPassword, err := jobs.GetParameterValue[string](parameters, parameters_enums.NixPacksPasswordKey)
	if err != nil {
		return parameters, err
	}

	a, err := cli.RegistryLogin(context.TODO(), registry.AuthConfig{
		Username:      nixPacksUsername,
		Password:      nixPacksPassword,
		ServerAddress: "ghcr.io",
	})
	if err != nil {
		return parameters, err
	}

	if a.Status != "Login Succeeded" {
		return parameters, fmt.Errorf("failed to login to nixpacks builder")
	}

	buildCommand, _ := jobs.GetParameterValue[string](parameters, parameters_enums.BuildCommand)

	startCommand, _ := jobs.GetParameterValue[string](parameters, parameters_enums.StartCommand)

	n, err := nixpacks.NewNixpacks()
	if err != nil {
		return parameters, err
	}

	// PARTIAL GAP: this build is COUNTED by admission control but is not
	// memory-CAPPED, unlike every other container the runner spawns.
	//
	// nixpacks-go's BuildOptions exposes no memory or CPU fields, and it
	// shells out to the `nixpacks` CLI binary, which runs its own
	// `docker build` internally — there is no seam to pass --memory
	// through. So the build itself is unbounded, and because it executes
	// inside dockerd rather than in the runner's cgroup, nothing here can
	// observe its usage either.
	//
	// Reserving the weight anyway is worth doing even without a cap. It
	// does not stop this build from overrunning, but it does stop it
	// running CONCURRENTLY with an agent container or another build that
	// have already been promised that memory. Without the reservation,
	// admission control would believe the host was free while an unbounded
	// build was consuming it — the budget would not be authoritative and
	// the host OOM this whole mechanism exists to prevent would stay
	// reachable via this one path.
	//
	// The real fix is to stop letting nixpacks do the build:
	// BuildOptions.Output makes it emit a Dockerfile instead of an image,
	// which we could then build through imageBuild() in
	// build_docker_image.go and inherit the cap too. That is a larger
	// change, so it is called out here rather than done silently.
	buildOptions := nixpacks.BuildOptions{
		Path:       repoDirectoryPath,
		Name:       dockerImageNameAndTag,
		LogsWriter: logsWriter,
	}

	if len(startCommand) > 0 {
		buildOptions.StartCommand = startCommand
	}
	if len(buildCommand) > 0 {
		buildOptions.BuildCommand = buildCommand
	}

	cmd, err := n.Build(context.Background(), buildOptions)
	if err != nil {
		return parameters, err
	}

	err = cmd.ResultAsync()
	if err != nil {
		return parameters, err
	}

	//fmt.Println(out.ImageName)
	//fmt.Println("language:", out.Language)
	//fmt.Println("install:", out.Install)
	//fmt.Println("build:", out.Build)
	//fmt.Println("start:", out.Start)
	//fmt.Println("buildError:", out.BuildError)

	//if len(out.BuildError) > 0 {
	//	return parameters, fmt.Errorf(out.BuildError)
	//}

	return parameters, nil
}
