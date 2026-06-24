package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner/jobs/commands/agents"
	"github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

const cloudfrontRegion = "us-east-1"

func Get(p commands_enums.Type) (jobs.Command, error) {
	switch p {
	case commands_enums.CheckoutRepo:
		return &CheckoutRepository{}, nil
	case commands_enums.BuildStaticSite:
		return &BuildStaticSite{}, nil
	case commands_enums.DeployAwsStaticSite:
		return &DeployAwsStaticSite{}, nil
	case commands_enums.BuildDockerImage:
		return &BuildDockerImage{}, nil
	case commands_enums.BuildNixPacksImage:
		return &BuildNixPacksImage{}, nil
	case commands_enums.DeployAwsWebService:
		return &DeployAwsWebService{}, nil
	case commands_enums.DeployAwsPrivateService:
		return &DeployAwsPrivateService{}, nil
	case commands_enums.DeployAwsRdsDatabase:
		return &DeployAwsRdsDatabase{}, nil
	case commands_enums.CreateAwsVpc:
		return &CreateDefaultAwsVPC{}, nil
	case commands_enums.CreateEcsCluster:
		return &CreateEcsCluster{}, nil
	case commands_enums.UploadImageToEcr:
		return &UploadDockerImageToEcr{}, nil
	case commands_enums.AddAwsStaticSiteResponseHeaders:
		return &AddAwsStaticSiteResponseHeaders{}, nil
	case commands_enums.UpdateAwsStaticSiteDomains:
		return &UpdateAwsStaticSiteDomains{}, nil
	case commands_enums.DeployAwsCloudfrontViewerRequestFunction:
		return &DeployAwsCloudfrontViewerRequestFunction{}, nil
	case commands_enums.CreateAcmCertificate:
		return &CreateAcmCertificate{}, nil
	case commands_enums.UpdateAwsWebServiceDomain:
		return &AddAwsWebServiceDomain{}, nil
	case commands_enums.VerifyAcmCertificate:
		return &VerifyAcmCertificate{}, nil
	case commands_enums.DeleteAwsStaticSite:
		return &DeleteAwsStaticSite{}, nil
	case commands_enums.DeleteAwsWebService:
		return &DeleteAwsWebService{}, nil
	case commands_enums.DeleteAwsPrivateService:
		return &DeleteAwsPrivateService{}, nil
	case commands_enums.DeleteAwsRdsDatabase:
		return &DeleteAwsRdsDatabase{}, nil
	case commands_enums.ListCloudWatchMetricsAwsEcsWebService:
		return &ListCloudWatchMetricsAwsEcsWebService{}, nil
	case commands_enums.CreateSecretAwsSecretManager:
		return &CreateSecretAwsSecretManager{}, nil
	case commands_enums.RunNewAutomation:
		return &RunNewAutomation{}, nil
	case commands_enums.RunNewAgent:
		return &agents.RunNewAgent{}, nil
	case commands_enums.GetDeploymentLogsAws:
		return &GetDeploymentLogsAws{}, nil
	case commands_enums.RunAgentStep:
		return &RunAgentStep{}, nil
	case commands_enums.RunAssistantSession:
		return &RunAssistantSession{}, nil
	case commands_enums.CommitAndPush:
		return &CommitAndPush{}, nil
	case commands_enums.OpenPullRequest:
		return &OpenPullRequest{}, nil
	case commands_enums.BuildInfraContext:
		return &BuildInfraContext{}, nil
	case commands_enums.MaterializeContext:
		return &MaterializeContext{}, nil
	}
	return nil, fmt.Errorf("error getting command for %s", p)
}

func isPreview(parameters map[string]interface{}) bool {
	p, err := jobs.GetParameterValue[bool](parameters, parameters_enums.IsPreview)
	if err != nil {
		p = false
	}
	return p
}

// MarkStepDone is the Tasks-mode equivalent of MarkDeploymentDone, scoped
// to working-directory cleanup only. Status updates flow through the
// RunTaskStep.OnCompletion* hooks in deployment-server (fired from
// MarkCompleteV1 after the Job result arrives), not from here.
//
// Removes /tmp/<orgID>/<taskID>/ — the per-Task base under which all
// per-repo subdirs (<idx>-<name>) for this Step Job live. The whole base
// is removable per Step Job because each Step Job re-clones via
// CheckoutRepo from origin; nothing on disk needs to persist between
// Step Jobs (commits do, via the pushed Task branch).
//
// The err parameter is reserved for future use (e.g., per-Job error
// logging); currently unused. Channel-return shape mirrors
// MarkDeploymentDone so callers consume identically:
// `<-MarkStepDone(parameters, err)`.
func MarkStepDone(parameters map[string]interface{}, err error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		orgID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
		taskID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
		if len(orgID) == 0 || len(taskID) == 0 {
			return
		}
		os.RemoveAll(utils.GetTaskRepositoriesBaseDir(orgID, taskID))
	}()
	return done
}

func MarkDeploymentDone(parameters map[string]interface{}, err error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer func() {
			//Delete old repo directory to clean up
			repoDirectoryPath, _ := utils.GetRepositoryDirectoryPath(parameters)
			if len(repoDirectoryPath) > 0 {
				os.RemoveAll(repoDirectoryPath)
			}
			close(done)
		}()
		status := build_enums.Success
		errorMessage := ""
		if err != nil {
			status = build_enums.Error
			errorMessage = err.Error()
		}
		organizationIdFromJob, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
		nowEpoch := time.Now().Unix()
		if isPreview(parameters) {
			//update preview and return
			previewID, e := jobs.GetParameterValue[string](parameters, parameters_enums.PreviewID)
			if e != nil {
				//Weird error. Would show up in logs. Return for now.
				return
			}
			utils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
				ID:           previewID,
				BuildTs:      nowEpoch,
				Status:       status,
				ErrorMessage: errorMessage,
			})
			return
		}

		deploymentID, e := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
		if e != nil {
			//job is not a deployment type
			return
		}
		utils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:               deploymentID,
			LastDeploymentTs: nowEpoch,
			Status:           status,
			ErrorMessage:     errorMessage,
		})

		buildID, e := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
		if e != nil {
			//job is not a build type
			return
		}
		utils.UpdateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
			ID:           buildID,
			BuildTs:      nowEpoch,
			Status:       status,
			ErrorMessage: errorMessage,
		})
	}()
	return done
}

func listAllS3Objects(s3Client *s3.Client, bucketName string) ([]s3Types.Object, error) {
	params := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	}

	listObjectsPaginator := s3.NewListObjectsV2Paginator(s3Client, params)

	var i int
	//log.Println("Objects:")
	var objects []s3Types.Object
	for listObjectsPaginator.HasMorePages() {
		i++
		page, err := listObjectsPaginator.NextPage(context.TODO())
		if err != nil {
			return nil, fmt.Errorf("failed to get page %v, %v", i, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

func deleteAllS3Files(s3Client *s3.Client, bucketName string) error {
	allS3Objects, err := listAllS3Objects(s3Client, bucketName)
	if err != nil {
		return err
	}
	var objectIds []s3Types.ObjectIdentifier
	for _, object := range allS3Objects {
		objectIds = append(objectIds, s3Types.ObjectIdentifier{Key: object.Key})
		if len(objectIds) == 9000 {
			//delete 9000 at a time. Limit is 10000
			_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
				Bucket: aws.String(bucketName),
				Delete: &s3Types.Delete{Objects: objectIds},
			})
			if err != nil {
				return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
			}
			objectIds = nil
		}
	}
	if len(objectIds) > 0 {
		_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &s3Types.Delete{Objects: objectIds},
		})
		if err != nil {
			return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
		}
	}

	return nil
}

func getDockerImageNameAndTag(parameters map[string]interface{}) (string, error) {
	//ex. <organizationID>-<deploymentID>:<commit-hash>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	commitHashFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s:%s", organizationID, deploymentID, commitHashFromParams), nil
}
