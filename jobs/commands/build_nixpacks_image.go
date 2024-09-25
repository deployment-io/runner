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
