package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
)

type DeleteAwsWebService struct {
}

func (d *DeleteAwsWebService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	//stop service //delete service
	io.WriteString(logsWriter, fmt.Sprintf("Deleting web service\n"))
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
	clients, err := newDeleteWebServiceClients(parameters)
	if err != nil {
		return parameters, err
	}
	err = deleteWebServiceResources(parameters, clients, logsWriter)
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

// Swapped by tests so deleting runs without real time passing.
var deleteWebServiceLoadBalancerPause = time.Minute

// deleteWebServiceECSAPI is the part of *ecs.Client deleting a web service uses.
// DescribeServices is what the inactive waiter polls.
type deleteWebServiceECSAPI interface {
	deleteTaskDefinitionsAPI
	DeleteService(ctx context.Context, params *ecs.DeleteServiceInput, optFns ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error)
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
}

// deleteWebServiceECRAPI is the part of *ecr.Client deleting a web service uses.
type deleteWebServiceECRAPI interface {
	DeleteRepository(ctx context.Context, params *ecr.DeleteRepositoryInput, optFns ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error)
}

// deleteWebServiceELBAPI is the part of *elasticloadbalancingv2.Client deleting a
// web service uses. DescribeLoadBalancers is also what the deleted waiter polls.
type deleteWebServiceELBAPI interface {
	DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error)
	DeleteLoadBalancer(ctx context.Context, params *elasticloadbalancingv2.DeleteLoadBalancerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error)
	DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error)
	DeleteTargetGroup(ctx context.Context, params *elasticloadbalancingv2.DeleteTargetGroupInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteTargetGroupOutput, error)
}

// deleteWebServiceEC2API is the part of *ec2.Client deleting a web service uses.
type deleteWebServiceEC2API interface {
	DescribeSecurityGroups(ctx context.Context, params *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, params *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, params *ec2.RevokeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
}

type deleteWebServiceClients struct {
	ecs deleteWebServiceECSAPI
	// ecr is nil for a service deployed from an image: it has no repository.
	ecr deleteWebServiceECRAPI
	elb deleteWebServiceELBAPI
	ec2 deleteWebServiceEC2API
	// cloudfront is nil without a CloudFront distribution (production web services).
	cloudfront deleteCloudfrontDistributionAPI
}

func newDeleteWebServiceClients(parameters map[string]interface{}) (clients deleteWebServiceClients, err error) {
	if clients.ecs, err = cloud_api_clients.GetEcsClient(parameters); err != nil {
		return clients, err
	}
	if deployedFromImage, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.DeployedFromImage); !deployedFromImage {
		if clients.ecr, err = cloud_api_clients.GetEcrClient(parameters); err != nil {
			return clients, err
		}
	}
	if clients.elb, err = cloud_api_clients.GetElbClient(parameters); err != nil {
		return clients, err
	}
	if clients.ec2, err = cloud_api_clients.GetEC2Client(parameters); err != nil {
		return clients, err
	}
	if cloudfrontID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID); len(cloudfrontID) > 0 {
		if clients.cloudfront, err = cloud_api_clients.GetCloudfrontClient(parameters, cloudfrontRegion); err != nil {
			return clients, err
		}
	}
	return clients, nil
}

// deleteWebServiceResources deletes what a web service deploy creates: the
// CloudFront distribution in front of a backend preview, the ECS service, its
// task definitions, its ECR repository, the load balancer, its target group and
// its security group. It is safe on a half-built or partly deleted service: a
// handle missing from the parameters is looked up by the name the deploy gives
// it, and a resource that doesn't exist counts as already deleted.
//
// It never touches what other services share: the VPC default security group's
// ingress rule for the port, the Service Connect namespace, the cluster and the VPC.
// Last, it removes the service's Task preview stack group, if it was in one and
// nothing else uses it (deletePreviewStackSecurityGroups).
func deleteWebServiceResources(parameters map[string]interface{}, clients deleteWebServiceClients, logsWriter io.Writer) error {
	// Only a backend preview has a distribution; it goes first so nothing is
	// left serving a preview whose service is gone.
	if cloudfrontID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID); len(cloudfrontID) > 0 {
		if err := deleteWebServiceCloudfrontDistribution(parameters, clients.cloudfront, cloudfrontID, logsWriter); err != nil {
			return err
		}
	}
	securityGroupIDs, err := deleteWebServiceEcsService(parameters, clients.ecs, logsWriter)
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
	if err := deleteWebServiceLoadBalancer(parameters, clients.elb, logsWriter); err != nil {
		return err
	}
	if err := deleteWebServiceTargetGroup(parameters, clients.elb, logsWriter); err != nil {
		return err
	}
	if err := deleteWebServiceAlbSecurityGroup(parameters, clients.ec2, securityGroupIDs, logsWriter); err != nil {
		return err
	}
	deletePreviewStackSecurityGroups(clients.ec2, securityGroupIDs, logsWriter)
	return nil
}

func logAlreadyGone(logsWriter io.Writer, resource, nameOrID string) {
	io.WriteString(logsWriter, fmt.Sprintf("%s %s was already gone\n", resource, nameOrID))
}

func deleteWebServiceCloudfrontDistribution(parameters map[string]interface{}, cloudfrontClient deleteCloudfrontDistributionAPI, cloudfrontID string, logsWriter io.Writer) error {
	// The CloudFront actions are in the static-site policy a preview deploy grants.
	ensurePreviewDeployAccess(parameters, logsWriter)
	_, err := deleteCloudfrontDistribution(cloudfrontClient, cloudfrontID, logsWriter)
	var noSuchDistribution *cloudfrontTypes.NoSuchDistribution
	if errors.As(err, &noSuchDistribution) {
		logAlreadyGone(logsWriter, "CloudFront distribution", cloudfrontID)
		return nil
	}
	return err
}

// deleteWebServiceEcsService deletes the ECS service and waits for it to go. It
// returns the security groups the service ran in, described before the delete.
func deleteWebServiceEcsService(parameters map[string]interface{}, ecsClient deleteWebServiceECSAPI, logsWriter io.Writer) ([]string, error) {
	clusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return nil, err
	}
	ecsServiceName, err := aws_utils.GetEcsServiceName(parameters)
	if err != nil {
		return nil, err
	}
	service, err := describeServiceBeforeDelete(ecsClient, clusterArn, ecsServiceName)
	if err != nil {
		return nil, err
	}
	securityGroupIDs := serviceSecurityGroupIDs(service)
	io.WriteString(logsWriter, fmt.Sprintf("Deleting ECS service: %s in cluster: %s\n", ecsServiceName, clusterArn))
	_, err = ecsClient.DeleteService(context.TODO(), &ecs.DeleteServiceInput{
		Service: aws.String(ecsServiceName),
		Cluster: aws.String(clusterArn),
		Force:   aws.Bool(true),
	})
	var serviceNotFound *ecsTypes.ServiceNotFoundException
	if errors.As(err, &serviceNotFound) {
		logAlreadyGone(logsWriter, "ECS service", ecsServiceName)
		return securityGroupIDs, nil
	}
	// A service that is no longer active is already being deleted: wait for it.
	var serviceNotActive *ecsTypes.ServiceNotActiveException
	if err != nil && !errors.As(err, &serviceNotActive) {
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
	return securityGroupIDs, nil
}

func deleteWebServiceEcrRepository(parameters map[string]interface{}, ecrClient deleteWebServiceECRAPI, logsWriter io.Writer) error {
	ecrRepositoryName, err := getEcrRepositoryName(parameters)
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting ECR repository: %s\n", ecrRepositoryName))
	_, err = ecrClient.DeleteRepository(context.TODO(), &ecr.DeleteRepositoryInput{
		RepositoryName: aws.String(ecrRepositoryName),
		Force:          true,
	})
	var repositoryNotFound *ecrTypes.RepositoryNotFoundException
	if errors.As(err, &repositoryNotFound) {
		logAlreadyGone(logsWriter, "ECR repository", ecrRepositoryName)
		return nil
	}
	return err
}

func isLoadBalancerNotFoundError(err error) bool {
	var loadBalancerNotFound *elbTypes.LoadBalancerNotFoundException
	return errors.As(err, &loadBalancerNotFound)
}

func isTargetGroupNotFoundError(err error) bool {
	var targetGroupNotFound *elbTypes.TargetGroupNotFoundException
	return errors.As(err, &targetGroupNotFound)
}

// isSecurityGroupNotFoundError reports whether err is the EC2
// InvalidGroup.NotFound API error, as isDuplicateSecurityGroupRuleError does.
func isSecurityGroupNotFoundError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidGroup.NotFound"
	}
	return false
}

func deleteWebServiceLoadBalancer(parameters map[string]interface{}, elbClient deleteWebServiceELBAPI, logsWriter io.Writer) error {
	//deleting the load balancer deletes its listeners
	loadBalancerArn, _ := jobs.GetParameterValue[string](parameters, parameters_enums.LoadBalancerArn)
	if len(loadBalancerArn) == 0 {
		albName, err := getAlbName(parameters)
		if err != nil {
			return err
		}
		output, err := elbClient.DescribeLoadBalancers(context.TODO(), &elasticloadbalancingv2.DescribeLoadBalancersInput{
			Names: []string{albName},
		})
		if isLoadBalancerNotFoundError(err) || (err == nil && len(output.LoadBalancers) == 0) {
			logAlreadyGone(logsWriter, "Load balancer", albName)
			return nil
		}
		if err != nil {
			return err
		}
		loadBalancerArn = aws.ToString(output.LoadBalancers[0].LoadBalancerArn)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting load balancer: %s\n", loadBalancerArn))
	_, err := elbClient.DeleteLoadBalancer(context.TODO(), &elasticloadbalancingv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(loadBalancerArn)})
	if isLoadBalancerNotFoundError(err) {
		logAlreadyGone(logsWriter, "Load balancer", loadBalancerArn)
		return nil
	}
	if err != nil {
		return err
	}
	loadBalancersDeletedWaiter := elasticloadbalancingv2.NewLoadBalancersDeletedWaiter(elbClient)
	err = loadBalancersDeletedWaiter.Wait(context.TODO(), &elasticloadbalancingv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []string{
			loadBalancerArn,
		},
	}, 20*time.Minute)
	if err != nil {
		return err
	}
	//sleep after alb is deleted else AWS might give an error
	time.Sleep(deleteWebServiceLoadBalancerPause)
	return nil
}

func deleteWebServiceTargetGroup(parameters map[string]interface{}, elbClient deleteWebServiceELBAPI, logsWriter io.Writer) error {
	targetGroupArn, _ := jobs.GetParameterValue[string](parameters, parameters_enums.TargetGroupArn)
	if len(targetGroupArn) == 0 {
		targetGroupName, err := getAlbTargetGroupName(parameters)
		if err != nil {
			return err
		}
		output, err := elbClient.DescribeTargetGroups(context.TODO(), &elasticloadbalancingv2.DescribeTargetGroupsInput{
			Names: []string{targetGroupName},
		})
		if isTargetGroupNotFoundError(err) || (err == nil && len(output.TargetGroups) == 0) {
			logAlreadyGone(logsWriter, "Target group", targetGroupName)
			return nil
		}
		if err != nil {
			return err
		}
		targetGroupArn = aws.ToString(output.TargetGroups[0].TargetGroupArn)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting target group: %s\n", targetGroupArn))
	_, err := elbClient.DeleteTargetGroup(context.TODO(), &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(targetGroupArn)})
	if isTargetGroupNotFoundError(err) {
		logAlreadyGone(logsWriter, "Target group", targetGroupArn)
		return nil
	}
	return err
}

// deleteWebServiceAlbSecurityGroup deletes the load balancer's security group. A
// backend preview's stack group (among serviceGroupIDs, the service's groups)
// admits the API's port from it; that rule goes first, since EC2 refuses to
// delete a group another group's rule references.
func deleteWebServiceAlbSecurityGroup(parameters map[string]interface{}, ec2Client deleteWebServiceEC2API, serviceGroupIDs []string, logsWriter io.Writer) error {
	albSecurityGroupID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.AlbSecurityGroupId)
	if len(albSecurityGroupID) == 0 {
		albSecurityGroupName, err := getAlbSecurityGroupName(parameters)
		if err != nil {
			return err
		}
		output, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{
			Filters: []ec2Types.Filter{{
				Name:   aws.String("group-name"),
				Values: []string{albSecurityGroupName},
			}},
		})
		if isSecurityGroupNotFoundError(err) || (err == nil && len(output.SecurityGroups) == 0) {
			logAlreadyGone(logsWriter, "Security group", albSecurityGroupName)
			return nil
		}
		if err != nil {
			return err
		}
		albSecurityGroupID = aws.ToString(output.SecurityGroups[0].GroupId)
	}
	revokePreviewStackIngressFrom(ec2Client, serviceGroupIDs, albSecurityGroupID, logsWriter)
	io.WriteString(logsWriter, fmt.Sprintf("Deleting security group: %s\n", albSecurityGroupID))
	_, err := ec2Client.DeleteSecurityGroup(context.TODO(), &ec2.DeleteSecurityGroupInput{
		DryRun:  aws.Bool(false),
		GroupId: aws.String(albSecurityGroupID),
	})
	if isSecurityGroupNotFoundError(err) {
		logAlreadyGone(logsWriter, "Security group", albSecurityGroupID)
		return nil
	}
	return err
}
