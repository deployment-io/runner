package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
)

type DeleteAwsPrivateService struct {
}

func (d *DeleteAwsPrivateService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	//stop service //delete service
	io.WriteString(logsWriter, fmt.Sprintf("Deleting private service\n"))
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return parameters, err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}
	if !isPreview(parameters) {
		//update deployment to deleted and delete domain
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:            deploymentID,
			DeletionState: deployment_enums.DeletionInProcess,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:            previewID,
			DeletionState: deployment_enums.DeletionInProcess,
		})
	}
	clusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return parameters, err
	}
	ecsClient, err := cloud_api_clients.GetEcsClient(parameters)
	if err != nil {
		return parameters, err
	}
	err = deletePrivateServiceEcsService(parameters, ecsClient, clusterArn, logsWriter)
	if err != nil {
		return parameters, err
	}

	err = deleteTaskDefinitions(parameters, ecsClient, logsWriter)
	if err != nil {
		return parameters, err
	}

	deployedFromImage, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DeployedFromImage)
	if !deployedFromImage {
		//delete ecr repository if necessary
		ecrClient, err := cloud_api_clients.GetEcrClient(parameters)
		if err != nil {
			return parameters, err
		}
		ecrRepositoryName, err := getEcrRepositoryName(parameters)
		if err != nil {
			return parameters, err
		}
		io.WriteString(logsWriter, fmt.Sprintf("Deleting ECR repository: %s\n", ecrRepositoryName))
		_, err = ecrClient.DeleteRepository(context.TODO(), &ecr.DeleteRepositoryInput{
			RepositoryName: aws.String(ecrRepositoryName),
			Force:          true,
		})
		if err != nil {
			return parameters, err
		}
	}
	if !isPreview(parameters) {
		//update deployment to deleted and delete domain
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:            deploymentID,
			DeletionState: deployment_enums.DeletionDone,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:            previewID,
			DeletionState: deployment_enums.DeletionDone,
		})
	}

	return parameters, err
}

// deletePrivateServiceECSAPI is the part of *ecs.Client deleting a private
// service's ECS service uses. DescribeServices is also what the inactive
// waiter polls.
type deletePrivateServiceECSAPI interface {
	DeleteService(ctx context.Context, params *ecs.DeleteServiceInput, optFns ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error)
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
}

// deletePrivateServiceEcsService deletes the ECS service, waits for it to go
// and then deletes its DNS name's Cloud Map service. Only the ECS steps can
// fail the delete.
func deletePrivateServiceEcsService(parameters map[string]interface{}, ecsClient deletePrivateServiceECSAPI,
	clusterArn string, logsWriter io.Writer) error {
	ecsServiceName, err := aws_utils.GetEcsServiceName(parameters)
	if err != nil {
		return err
	}
	registryArns := liveServiceRegistryArns(ecsClient, clusterArn, ecsServiceName)
	io.WriteString(logsWriter, fmt.Sprintf("Deleting ECS service: %s in cluster: %s\n", ecsServiceName, clusterArn))
	_, err = ecsClient.DeleteService(context.TODO(), &ecs.DeleteServiceInput{
		Service: aws.String(ecsServiceName),
		Cluster: aws.String(clusterArn),
		Force:   aws.Bool(true),
	})
	if err != nil {
		return err
	}
	inactiveWaiter := ecs.NewServicesInactiveWaiter(ecsClient)
	err = inactiveWaiter.Wait(context.TODO(), &ecs.DescribeServicesInput{
		Services: []string{ecsServiceName},
		Cluster:  aws.String(clusterArn),
	}, 20*time.Minute)
	if err != nil {
		return err
	}
	deleteDnsServices(parameters, registryArns, logsWriter)
	return nil
}
