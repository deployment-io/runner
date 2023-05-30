package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/clusters"
	"github.com/deployment-io/deployment-runner-kit/enums/cluster_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

const upsertClusterKey = "upsertClusters"

type CreateEcsCluster struct {
}

func getEcsClient(parameters map[parameters_enums.Key]interface{}) (*ecs.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return nil, err
	}

	ecsClient := ecs.NewFromConfig(cfg, func(o *ecs.Options) {
		o.Region = region.String()
	})
	return ecsClient, nil
}

func getDefaultEcsClusterName(parameters map[parameters_enums.Key]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ecs-%s", organizationID), nil
}

func createEcsClusterIfNeeded(ecsClient *ecs.Client, parameters map[parameters_enums.Key]interface{}) error {
	ecsClusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err == nil && len(ecsClusterArn) > 0 {
		return nil
	}
	ecsClusterName, err := getDefaultEcsClusterName(parameters)
	if err != nil {
		return err
	}

	describeClustersOutput, err := ecsClient.DescribeClusters(context.TODO(), &ecs.DescribeClustersInput{
		Clusters: []string{
			ecsClusterName,
		},
	})

	for _, cluster := range describeClustersOutput.Clusters {
		if ecsClusterName == aws.ToString(cluster.ClusterName) {
			return nil
		}
	}

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
		return err
	}

	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return err
	}
	upsertClustersPipeline.Add(upsertClusterKey, clusters.UpsertClusterDtoV1{
		Type:       cluster_enums.ECS,
		Region:     region,
		Name:       aws.ToString(createClusterOutput.Cluster.ClusterName),
		ClusterArn: aws.ToString(createClusterOutput.Cluster.ClusterArn),
	})

	return nil
}

func (c *CreateEcsCluster) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {

	ecsClient, err := getEcsClient(parameters)
	if err != nil {
		return parameters, err
	}

	err = createEcsClusterIfNeeded(ecsClient, parameters)
	if err != nil {
		return parameters, err
	}

	return parameters, nil
}
