package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"log"
)

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

func (c *CreateEcsCluster) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {

	ecsClient, err := getEcsClient(parameters)
	if err != nil {
		return parameters, err
	}

	//TODO check if cluster exists
	var ecsClusterName string
	ecsClusterName, err = getDefaultEcsClusterName(parameters)
	if err != nil {
		return parameters, err
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
		return parameters, err
	}
	//TODO sync ecs cluster data to server
	log.Println(aws.ToString(createClusterOutput.Cluster.ClusterArn))
	log.Println("")
	log.Println(aws.ToString(createClusterOutput.Cluster.ClusterName))

	return parameters, nil
}
