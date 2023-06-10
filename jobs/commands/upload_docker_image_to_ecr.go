package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"io"
	"os"
	"strings"
)

type UploadDockerImageToEcr struct {
}

func getEcrRepositoryName(parameters map[string]interface{}) (string, error) {
	//ecr-<organizationID>-<deploymentID>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ecr-%s-%s", organizationID, deploymentID), nil
}

func createEcrRepositoryIfNeeded(parameters map[string]interface{}, ecrClient *ecr.Client) (ecrRepositoryUri string, err error) {
	ecrRepositoryName, err := getEcrRepositoryName(parameters)
	if err != nil {
		return "", err
	}

	ecrRepositoryUriFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcrRepositoryUri)
	if err == nil && len(ecrRepositoryUriFromParams) > 0 {
		return ecrRepositoryUriFromParams, nil
	}

	describeRepositoriesOutput, _ := ecrClient.DescribeRepositories(context.TODO(), &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{
			ecrRepositoryName,
		},
	})

	if describeRepositoriesOutput != nil {
		for _, repository := range describeRepositoriesOutput.Repositories {
			if aws.ToString(repository.RepositoryName) == ecrRepositoryName {
				ecrRepositoryUri = aws.ToString(repository.RepositoryUri)
			}
		}
	}

	if len(ecrRepositoryUri) == 0 {
		createRepositoryInput := &ecr.CreateRepositoryInput{
			RepositoryName:     aws.String(ecrRepositoryName),
			ImageTagMutability: ecrTypes.ImageTagMutabilityMutable,
			Tags: []ecrTypes.Tag{
				{
					Key:   aws.String("Name"),
					Value: aws.String(ecrRepositoryName),
				},
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
		}
		createRepositoryOutput, err := ecrClient.CreateRepository(context.TODO(), createRepositoryInput)
		if err != nil {
			return "", err
		}
		ecrRepositoryUri = aws.ToString(createRepositoryOutput.Repository.RepositoryUri)
	}

	var deploymentID string
	deploymentID, err = jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	updateDeploymentsPipeline.Add(updateDeploymentsKey, deployments.UpdateDeploymentDtoV1{
		ID:               deploymentID,
		EcrRepositoryUri: ecrRepositoryUri,
	})

	return ecrRepositoryUri, nil
}

func tagDockerImageToRepositoryUri(parameters map[string]interface{}, ecrRepositoryUri string) (string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", err
	}
	dockerImageNameAndTag, err := getDockerImageNameAndTag(parameters)
	if err != nil {
		return "", err
	}
	commitHash, err := jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)
	if err != nil {
		return "", err
	}
	ecrRepositoryUriWithTag := ecrRepositoryUri + ":" + commitHash
	err = cli.ImageTag(context.TODO(), dockerImageNameAndTag, ecrRepositoryUriWithTag)
	if err != nil {
		return "", err
	}
	return ecrRepositoryUriWithTag, nil
}

func pushDockerImageToEcr(parameters map[string]interface{}, ecrClient *ecr.Client, ecrRepositoryUriWithTag string) error {
	getAuthorizationTokenOutput, err := ecrClient.GetAuthorizationToken(context.TODO(), &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return err
	}

	if len(getAuthorizationTokenOutput.AuthorizationData) < 1 {
		return fmt.Errorf("no auth token from ECR")
	}

	encodedToken := aws.ToString(getAuthorizationTokenOutput.AuthorizationData[0].AuthorizationToken)
	decodedBytes, err := base64.StdEncoding.DecodeString(encodedToken)
	if err != nil {
		return err
	}

	fullToken := string(decodedBytes)
	_, token, found := strings.Cut(fullToken, ":")
	if !found {
		return fmt.Errorf("full token not in valid format: %s", fullToken)
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	authConfig := registry.AuthConfig{
		Username: "AWS",
		Password: token,
	}
	encodedJSON, err := json.Marshal(authConfig)
	if err != nil {
		panic(err)
	}
	authStr := base64.URLEncoding.EncodeToString(encodedJSON)

	push, err := cli.ImagePush(context.TODO(), ecrRepositoryUriWithTag, types.ImagePushOptions{
		RegistryAuth: authStr,
	})

	defer func() {
		_ = push.Close()
	}()
	_, err = io.Copy(os.Stdout, push)

	if err != nil {
		return err
	}

	return nil
}

func (u *UploadDockerImageToEcr) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		_ = loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			markBuildDone(parameters, err)
		}
	}()
	ecrClient, err := getEcrClient(parameters)
	if err != nil {
		return parameters, err
	}

	ecrRepositoryUri, err := createEcrRepositoryIfNeeded(parameters, ecrClient)
	if err != nil {
		return parameters, err
	}

	ecrRepositoryUriWithTag, err := tagDockerImageToRepositoryUri(parameters, ecrRepositoryUri)
	if err != nil {
		return parameters, err
	}

	ecrRepositoryName, err := getEcrRepositoryName(parameters)
	if err != nil {
		return parameters, err
	}
	commitHash, err := jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)
	if err != nil {
		return parameters, err
	}
	describeImagesOutput, _ := ecrClient.DescribeImages(context.TODO(), &ecr.DescribeImagesInput{
		RepositoryName: aws.String(ecrRepositoryName),
		ImageIds: []ecrTypes.ImageIdentifier{
			{
				ImageTag: aws.String(commitHash),
			},
		},
	})

	if describeImagesOutput == nil || len(describeImagesOutput.ImageDetails) == 0 {
		err = pushDockerImageToEcr(parameters, ecrClient, ecrRepositoryUriWithTag)
		if err != nil {
			return parameters, err
		}
	}

	jobs.SetParameterValue(parameters, parameters_enums.EcrRepositoryUri, ecrRepositoryUri)
	jobs.SetParameterValue(parameters, parameters_enums.EcrRepositoryUriWithTag, ecrRepositoryUriWithTag)

	return parameters, err
}
