package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
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
	clients, err := newDeletePrivateServiceClients(parameters)
	if err != nil {
		return parameters, err
	}
	err = deletePrivateServiceResources(parameters, clients, logsWriter)
	if err != nil {
		return parameters, err
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

// deletePrivateServiceClients are the AWS clients deleting a private service uses.
type deletePrivateServiceClients struct {
	ecs deleteWebServiceECSAPI
	// ecr is nil for a service deployed from an image: it has no repository.
	ecr deleteWebServiceECRAPI
	ec2 securityGroupDeleteAPI
}

func newDeletePrivateServiceClients(parameters map[string]interface{}) (clients deletePrivateServiceClients, err error) {
	if clients.ecs, err = cloud_api_clients.GetEcsClient(parameters); err != nil {
		return clients, err
	}
	if deployedFromImage, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DeployedFromImage); !deployedFromImage {
		if clients.ecr, err = cloud_api_clients.GetEcrClient(parameters); err != nil {
			return clients, err
		}
	}
	if clients.ec2, err = cloud_api_clients.GetEC2Client(parameters); err != nil {
		return clients, err
	}
	return clients, nil
}

// deletePrivateServiceResources deletes what a private service deploy creates:
// the ECS service (and its DNS name), its task definitions and its ECR
// repository, then its Task preview stack group if it was in one and nothing
// else uses it. A resource that's already gone counts as deleted: a preview
// database is deployed from an image but its Deployment doesn't say so, so its
// ECR repository never existed.
func deletePrivateServiceResources(parameters map[string]interface{}, clients deletePrivateServiceClients, logsWriter io.Writer) error {
	clusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return err
	}
	securityGroupIDs, err := deletePrivateServiceEcsService(parameters, clients.ecs, clusterArn, logsWriter)
	if err != nil {
		return err
	}
	if err := deleteTaskDefinitions(parameters, clients.ecs, logsWriter); err != nil {
		return err
	}
	if deployedFromImage, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DeployedFromImage); !deployedFromImage {
		if err := deleteWebServiceEcrRepository(parameters, clients.ecr, logsWriter); err != nil {
			return err
		}
	}
	deletePreviewStackSecurityGroups(clients.ec2, securityGroupIDs, logsWriter)
	return nil
}

// deletePrivateServiceECSAPI is the part of *ecs.Client deleting a private
// service's ECS service uses. DescribeServices is also what the inactive
// waiter polls.
type deletePrivateServiceECSAPI interface {
	DeleteService(ctx context.Context, params *ecs.DeleteServiceInput, optFns ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error)
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
}

// deletePrivateServiceEcsService deletes the ECS service, waits for it to go
// and then deletes its DNS name's Cloud Map service. A service that's already
// gone is skipped. Only the ECS steps can fail the delete. It returns the
// security groups the service ran in, described before the delete.
func deletePrivateServiceEcsService(parameters map[string]interface{}, ecsClient deletePrivateServiceECSAPI,
	clusterArn string, logsWriter io.Writer) ([]string, error) {
	ecsServiceName, err := aws_utils.GetEcsServiceName(parameters)
	if err != nil {
		return nil, err
	}
	service, err := describeServiceBeforeDelete(ecsClient, clusterArn, ecsServiceName)
	if err != nil {
		return nil, err
	}
	registryArns, securityGroupIDs := serviceRegistryArns(service), serviceSecurityGroupIDs(service)
	io.WriteString(logsWriter, fmt.Sprintf("Deleting ECS service: %s in cluster: %s\n", ecsServiceName, clusterArn))
	_, err = ecsClient.DeleteService(context.TODO(), &ecs.DeleteServiceInput{
		Service: aws.String(ecsServiceName),
		Cluster: aws.String(clusterArn),
		Force:   aws.Bool(true),
	})
	var serviceNotFound *ecsTypes.ServiceNotFoundException
	if errors.As(err, &serviceNotFound) {
		logAlreadyGone(logsWriter, "ECS service", ecsServiceName)
		deleteDnsServices(parameters, registryArns, logsWriter)
		return securityGroupIDs, nil
	}
	if err != nil {
		return nil, err
	}
	inactiveWaiter := ecs.NewServicesInactiveWaiter(ecsClient)
	err = inactiveWaiter.Wait(context.TODO(), &ecs.DescribeServicesInput{
		Services: []string{ecsServiceName},
		Cluster:  aws.String(clusterArn),
	}, 20*time.Minute)
	if err != nil {
		return nil, err
	}
	deleteDnsServices(parameters, registryArns, logsWriter)
	return securityGroupIDs, nil
}
