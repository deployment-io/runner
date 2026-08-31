package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/docker/docker/api/types/image"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
)

// UploadDockerImageToEcr pushes the built application image to ECR and then
// drops the local copy.
//
// pushImage and reclaimLocalImages are injected rather than called directly
// so the ordering rule this command has to honor — the local tags go only
// after a push that succeeded — is testable without a Docker daemon and an
// ECR account. Both default to the production implementations in
// newUploadDockerImageToEcr; the same seam the S3 uploader uses for
// uploadFile.
type UploadDockerImageToEcr struct {
	pushImage          func(ecrClient *ecr.Client, ecrRepositoryUriWithTag string, logsWriter io.Writer) error
	reclaimLocalImages func(refs []string, logsWriter io.Writer)
}

func newUploadDockerImageToEcr() *UploadDockerImageToEcr {
	return &UploadDockerImageToEcr{
		pushImage:          pushDockerImageToEcr,
		reclaimLocalImages: reclaim.RemoveLocalImages,
	}
}

func getEcrRepositoryName(parameters map[string]interface{}) (string, error) {
	//ecr-<organizationID>-<deploymentID>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
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
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return "", err
	}
	if !isPreview(parameters) {
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:               deploymentID,
			EcrRepositoryUri: ecrRepositoryUri,
		})
	} else {
		//for preview
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
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
	defer cli.Close()
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

func pushDockerImageToEcr(ecrClient *ecr.Client, ecrRepositoryUriWithTag string, logsWriter io.Writer) error {
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
	defer cli.Close()

	authConfig := registry.AuthConfig{
		Username: "AWS",
		Password: token,
	}
	encodedJSON, err := json.Marshal(authConfig)
	if err != nil {
		panic(err)
	}
	authStr := base64.URLEncoding.EncodeToString(encodedJSON)

	push, err := cli.ImagePush(context.TODO(), ecrRepositoryUriWithTag, image.PushOptions{
		RegistryAuth: authStr,
	})
	if err != nil {
		return err
	}

	defer func() {
		_ = push.Close()
	}()
	io.WriteString(logsWriter, fmt.Sprintf("Pushing docker image to ECR: %s\n", ecrRepositoryUriWithTag))
	// The daemon reports a push failure inside the response stream rather
	// than as an error from ImagePush: an "unauthorized" or a registry 5xx
	// arrives as a final {"error":...} line, which the previous io.Copy
	// wrote to the log and then returned nil for. Parsing the stream — the
	// same way the image build does — is what makes the caller's "push
	// succeeded" real, and the local image is only removed on the strength
	// of this return value.
	return printBodyToLog(push, logsWriter)
}

// pushAndReclaim pushes the image to ECR and then removes the two local tags
// that point at it — but only when the push succeeded.
//
// Both tags are commit-scoped (<orgID>-<deploymentID>:<hash> from the build,
// <ecrRepositoryUri>:<hash> from the tag step), so every deploy adds a whole
// application image locally instead of replacing the last one. Removing both
// untags the image and the daemon reclaims its layers; a rollback re-pulls
// from ECR, which is why this is safe only after the push lands. A failed
// push leaves the image exactly where it is.
func (u *UploadDockerImageToEcr) pushAndReclaim(ecrClient *ecr.Client, dockerImageNameAndTag,
	ecrRepositoryUriWithTag string, logsWriter io.Writer) error {
	if err := u.pushImage(ecrClient, ecrRepositoryUriWithTag, logsWriter); err != nil {
		return err
	}
	u.reclaimLocalImages([]string{ecrRepositoryUriWithTag, dockerImageNameAndTag}, logsWriter)
	return nil
}

func (u *UploadDockerImageToEcr) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()

	//check and add policy for AWS ECR upload
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
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

	dockerImageNameAndTag, err := getDockerImageNameAndTag(parameters)
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
	err = u.pushAndReclaim(ecrClient, dockerImageNameAndTag, ecrRepositoryUriWithTag, logsWriter)
	if err != nil {
		return parameters, err
	}
	//}

	// The build that produced this image also grew the shared build cache,
	// which nothing else ever trims. Bounded, so repeat builds keep their
	// working set.
	reclaim.PruneBuildCache(logsWriter)

	jobs.SetParameterValue(parameters, parameters_enums.EcrRepositoryUri, ecrRepositoryUri)
	jobs.SetParameterValue(parameters, parameters_enums.DockerRepositoryUriWithTag, ecrRepositoryUriWithTag)

	return parameters, err
}
