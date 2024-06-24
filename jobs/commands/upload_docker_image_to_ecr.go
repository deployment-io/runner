package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"io"
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

func createEcrRepositoryIfNeeded(parameters map[string]interface{}, ecrClient *ecr.Client, logsWriter io.Writer) (ecrRepositoryUri string, err error) {
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

	io.WriteString(logsWriter, fmt.Sprintf("Created ECR repository: %s\n", ecrRepositoryUri))

	var deploymentID string
	deploymentID, err = jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}

	if !isPreview(parameters) {
		updateDeploymentsPipeline.Add(updateDeploymentsKey, deployments.UpdateDeploymentDtoV1{
			ID:               deploymentID,
			EcrRepositoryUri: ecrRepositoryUri,
		})
	} else {
		//for preview
		previewID := deploymentID
		updatePreviewsPipeline.Add(updatePreviewsKey, previews.UpdatePreviewDtoV1{
			ID:               previewID,
			EcrRepositoryUri: ecrRepositoryUri,
		})
	}

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

func pushDockerImageToEcr(parameters map[string]interface{}, ecrClient *ecr.Client, ecrRepositoryUriWithTag string, logsWriter io.Writer) error {
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
	io.WriteString(logsWriter, fmt.Sprintf("Pushing docker image to ECR: %s\n", ecrRepositoryUriWithTag))
	_, err = io.Copy(logsWriter, push)
	if err != nil {
		return err
	}

	return nil
}

func (u *UploadDockerImageToEcr) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkBuildDone(parameters, err)
		}
	}()

	//check and add policy for AWS ECR upload
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return parameters, err
	}
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsEcrUpload, runnerData.OsType.String(),
		runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
	if err != nil {
		return parameters, err
	}

	ecrClient, err := cloud_api_clients.GetEcrClient(parameters)
	if err != nil {
		return parameters, err
	}

	ecrRepositoryUri, err := createEcrRepositoryIfNeeded(parameters, ecrClient, logsWriter)
	if err != nil {
		return parameters, err
	}

	ecrRepositoryUriWithTag, err := tagDockerImageToRepositoryUri(parameters, ecrRepositoryUri)
	if err != nil {
		return parameters, err
	}

	//ecrRepositoryName, err := getEcrRepositoryName(parameters)
	//if err != nil {
	//	return parameters, err
	//}
	//commitHash, err := jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)
	//if err != nil {
	//	return parameters, err
	//}
	//describeImagesOutput, _ := ecrClient.DescribeImages(context.TODO(), &ecr.DescribeImagesInput{
	//	RepositoryName: aws.String(ecrRepositoryName),
	//	ImageIds: []ecrTypes.ImageIdentifier{
	//		{
	//			ImageTag: aws.String(commitHash),
	//		},
	//	},
	//})

	//if describeImagesOutput == nil || len(describeImagesOutput.ImageDetails) == 0 {
	err = pushDockerImageToEcr(parameters, ecrClient, ecrRepositoryUriWithTag, logsWriter)
	if err != nil {
		return parameters, err
	}
	//}

	jobs.SetParameterValue(parameters, parameters_enums.EcrRepositoryUri, ecrRepositoryUri)
	jobs.SetParameterValue(parameters, parameters_enums.DockerRepositoryUriWithTag, ecrRepositoryUriWithTag)

	return parameters, err
}
