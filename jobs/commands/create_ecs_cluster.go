package commands

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/clusters"
	"github.com/deployment-io/deployment-runner-kit/enums/cluster_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
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

	for _, cluster := range describeClustersOutput.Clusters {
		status := aws.ToString(cluster.Status)
		if ecsClusterName == aws.ToString(cluster.ClusterName) && status != "INACTIVE" {
			ecsClusterArn = aws.ToString(cluster.ClusterArn)
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

func (c *CreateEcsCluster) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
	logBuffer := new(bytes.Buffer)
	defer func() {
		_ = loggers.LogBuffer(logBuffer, logger)
		if err != nil {
			markBuildDone(parameters, err)
		}
	}()
	ecsClient, err := getEcsClient(parameters)
	if err != nil {
		return parameters, err
	}

	clusterArn, err := createEcsClusterIfNeeded(ecsClient, parameters)
	if err != nil {
		return parameters, err
	}

	jobs.SetParameterValue(parameters, parameters_enums.EcsClusterArn, clusterArn)

	return parameters, nil
}
