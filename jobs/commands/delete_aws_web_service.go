package commands

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"time"
)

type DeleteAwsWebService struct {
}

func (d *DeleteAwsWebService) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
	//stop service //delete service
	clusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return parameters, err
	}
	ecsServiceName, err := getEcsServiceName(parameters)
	if err != nil {
		return parameters, err
	}
	ecsClient, err := getEcsClient(parameters)
	_, err = ecsClient.DeleteService(context.TODO(), &ecs.DeleteServiceInput{
		Service: aws.String(ecsServiceName),
		Cluster: aws.String(clusterArn),
		Force:   aws.Bool(true),
	})
	if err != nil {
		return parameters, err
	}
	inactiveWaiter := ecs.NewServicesInactiveWaiter(ecsClient)
	err = inactiveWaiter.Wait(context.TODO(), &ecs.DescribeServicesInput{
		Services: []string{ecsServiceName},
		Cluster:  aws.String(clusterArn),
	}, 10*time.Minute)
	if err != nil {
		return parameters, err
	}

	//delete task definition if needed
	taskDefinitionFamilyName, err := getTaskDefinitionFamilyName(parameters)
	if err != nil {
		return parameters, err
	}
	listTaskDefinitionsOutput, err := ecsClient.ListTaskDefinitions(context.TODO(), &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(taskDefinitionFamilyName),
	})
	if err != nil {
		return parameters, err
	}
	taskDefinitionArns := listTaskDefinitionsOutput.TaskDefinitionArns
	for _, taskDefinitionArn := range taskDefinitionArns {
		_, err = ecsClient.DeregisterTaskDefinition(context.TODO(), &ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String(taskDefinitionArn)})
		if err != nil {
			return parameters, err
		}
	}
	_, err = ecsClient.DeleteTaskDefinitions(context.TODO(), &ecs.DeleteTaskDefinitionsInput{TaskDefinitions: taskDefinitionArns})
	if err != nil {
		return parameters, err
	}

	//delete ecr repository if necessary
	ecrClient, err := getEcrClient(parameters)
	if err != nil {
		return parameters, err
	}
	ecrRepositoryName, err := getEcrRepositoryName(parameters)
	if err != nil {
		return parameters, err
	}
	_, err = ecrClient.DeleteRepository(context.TODO(), &ecr.DeleteRepositoryInput{
		RepositoryName: aws.String(ecrRepositoryName),
		Force:          true,
	})
	if err != nil {
		return parameters, err
	}

	//delete listeners
	//delete lb
	loadBalancerArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.LoadBalancerArn)
	elbClient, err := getElbClient(parameters)
	if err != nil {
		return parameters, err
	}
	_, err = elbClient.DeleteLoadBalancer(context.TODO(), &elasticloadbalancingv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(loadBalancerArn)})
	if err != nil {
		return parameters, err
	}
	loadBalancersDeletedWaiter := elasticloadbalancingv2.NewLoadBalancersDeletedWaiter(elbClient)
	err = loadBalancersDeletedWaiter.Wait(context.TODO(), &elasticloadbalancingv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []string{
			loadBalancerArn,
		},
	}, 10*time.Minute)
	if err != nil {
		return parameters, err
	}

	//sleep after alb is deleted else AWS might give an error
	time.Sleep(1 * time.Minute)

	//delete target group
	targetGroupArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.TargetGroupArn)
	if err != nil {
		return parameters, err
	}
	_, err = elbClient.DeleteTargetGroup(context.TODO(), &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(targetGroupArn)})
	if err != nil {
		return parameters, err
	}

	//delete alb security group
	ec2Client, err := getEC2Client(parameters)
	if err != nil {
		return parameters, err
	}
	albSecurityGroupID, err := jobs.GetParameterValue[string](parameters, parameters_enums.AlbSecurityGroupId)
	if err != nil {
		return parameters, err
	}
	_, err = ec2Client.DeleteSecurityGroup(context.TODO(), &ec2.DeleteSecurityGroupInput{
		DryRun:  aws.Bool(false),
		GroupId: aws.String(albSecurityGroupID),
	})
	if err != nil {
		return parameters, err
	}

	//update deployment to deleted and delete domain
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return parameters, err
	}
	updateDeploymentsPipeline.Add(updateDeploymentsKey, deployments.UpdateDeploymentDtoV1{
		ID:            deploymentID,
		DeletionState: deployment_enums.DeletionDone,
	})

	return parameters, err
}
