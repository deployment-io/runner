package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamTypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/clusters"
	"github.com/deployment-io/deployment-runner-kit/enums/cluster_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils"
	"io"
)

const upsertClusterKey = "upsertClusters"

type CreateEcsCluster struct {
}

func getDefaultEcsClusterName(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ecs-%s", organizationID), nil
}

func createEcsClusterIfNeeded(ecsClient *ecs.Client, parameters map[string]interface{}) (ecsClusterArn string, err error) {
	ecsClusterArn, err = jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err == nil && len(ecsClusterArn) > 0 {
		return ecsClusterArn, nil
	}
	ecsClusterName, err := getDefaultEcsClusterName(parameters)
	if err != nil {
		return "", err
	}

	describeClustersOutput, err := ecsClient.DescribeClusters(context.TODO(), &ecs.DescribeClustersInput{
		Clusters: []string{
			ecsClusterName,
		},
	})

	if describeClustersOutput != nil {
		for _, cluster := range describeClustersOutput.Clusters {
			status := aws.ToString(cluster.Status)
			if ecsClusterName == aws.ToString(cluster.ClusterName) && status != "INACTIVE" {
				ecsClusterArn = aws.ToString(cluster.ClusterArn)
			}
		}
	}

	if len(ecsClusterArn) == 0 {
		createClusterInput := &ecs.CreateClusterInput{
			CapacityProviders: []string{"FARGATE"},
			ClusterName:       aws.String(ecsClusterName),
			Tags: []ecsTypes.Tag{
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
		}

		createClusterOutput, err := ecsClient.CreateCluster(context.TODO(), createClusterInput)
		if err != nil {
			return "", err
		}
		ecsClusterArn = aws.ToString(createClusterOutput.Cluster.ClusterArn)
	}

	region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}

	upsertClustersPipeline.Add(upsertClusterKey, clusters.UpsertClusterDtoV1{
		Type:       cluster_enums.ECS,
		Region:     region_enums.Type(region),
		Name:       ecsClusterName,
		ClusterArn: ecsClusterArn,
	})

	return ecsClusterArn, nil
}

func getDefaultTaskExecutionRoleName(parameters map[string]interface{}) (string, error) {
	//eTERole-<os>-<cpuArch>-<organizationID>-<runner region>
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("eTERole-%s%s-%s-%s", runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion), nil
}

func getEcsTaskTrustPolicyForTaskExecutionRole() string {
	ecsTasksTrustPolicy := `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "",
      "Effect": "Allow",
      "Principal": {
        "Service": "ecs-tasks.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}`
	return ecsTasksTrustPolicy
}

func createEcsTaskExecutionRole(iamClient *iam.Client, taskExecutionRoleName string) (ecsTaskExecutionRoleArn string, err error) {
	createRoleOutput, err := iamClient.CreateRole(context.TODO(), &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(getEcsTaskTrustPolicyForTaskExecutionRole()),
		RoleName:                 aws.String(taskExecutionRoleName),
		Tags: []iamTypes.Tag{{
			Key:   aws.String("created by"),
			Value: aws.String("deployment.io"),
		}},
	})
	if err != nil {
		return "", err
	}
	_, err = iamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
		PolicyArn: aws.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
		RoleName:  createRoleOutput.Role.RoleName,
	})

	if err != nil {
		return "", err
	}

	_, err = iamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
		PolicyArn: aws.String("arn:aws:iam::aws:policy/CloudWatchFullAccess"),
		RoleName:  createRoleOutput.Role.RoleName,
	})

	if err != nil {
		return "", err
	}

	_, err = iamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
		PolicyArn: aws.String("arn:aws:iam::aws:policy/SecretsManagerReadWrite"),
		RoleName:  createRoleOutput.Role.RoleName,
	})

	if err != nil {
		return "", err
	}

	return aws.ToString(createRoleOutput.Role.Arn), nil
}

func getEcsTaskExecutionRoleIfNeeded(iamClient *iam.Client, parameters map[string]interface{}) (ecsTaskExecutionRoleArn string, err error) {
	ecsTaskExecutionRoleArn, err = jobs.GetParameterValue[string](parameters, parameters_enums.EcsTaskExecutionRoleArn)
	if err == nil && len(ecsTaskExecutionRoleArn) > 0 {
		return
	}
	taskExecutionRoleName, err := getDefaultTaskExecutionRoleName(parameters)
	if err != nil {
		return "", err
	}

	getRoleOutput, err := iamClient.GetRole(context.TODO(), &iam.GetRoleInput{RoleName: aws.String(taskExecutionRoleName)})

	if err != nil {
		runnerData := utils.RunnerData.Get()
		if runnerData.Mode != runner_enums.LOCAL {
			return "", err
		}
		err = nil
	}

	if getRoleOutput != nil && getRoleOutput.Role != nil && getRoleOutput.Role.Arn != nil && len(aws.ToString(getRoleOutput.Role.Arn)) > 0 {
		ecsTaskExecutionRoleArn = aws.ToString(getRoleOutput.Role.Arn)
		var region int64
		region, err = jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
		if err != nil {
			return "", err
		}
		upsertClustersPipeline.Add(upsertClusterKey, clusters.UpsertClusterDtoV1{
			Type:                    cluster_enums.ECS,
			Region:                  region_enums.Type(region),
			EcsTaskExecutionRoleArn: ecsTaskExecutionRoleArn,
		})
		return
	} else {
		//create new task execution role
		return createEcsTaskExecutionRole(iamClient, taskExecutionRoleName)
	}
	//return "", fmt.Errorf("ecs task execution role not found")
}

func (c *CreateEcsCluster) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkBuildDone(parameters, err)
		}
	}()

	ecsClient, err := cloud_api_clients.GetEcsClient(parameters)
	if err != nil {
		return parameters, err
	}

	clusterArn, err := createEcsClusterIfNeeded(ecsClient, parameters)
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Created ECS cluster: %s\n", clusterArn))

	iamClient, err := cloud_api_clients.GetIamClient(parameters)
	if err != nil {
		return parameters, err
	}

	ecsTaskExecutionRoleArn, err := getEcsTaskExecutionRoleIfNeeded(iamClient, parameters)
	if err != nil {
		return parameters, err
	}

	jobs.SetParameterValue(parameters, parameters_enums.EcsClusterArn, clusterArn)
	jobs.SetParameterValue(parameters, parameters_enums.EcsTaskExecutionRoleArn, ecsTaskExecutionRoleArn)

	return parameters, nil
}
