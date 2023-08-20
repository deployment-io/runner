package utils

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"strings"
	"sync"
	"time"
)

type UpgradeDataType struct {
	sync.Mutex
	DockerUpgradeImage string
	UpgradeFromTs      int64
	UpgradeToTs        int64
}

func (u *UpgradeDataType) Set(dockerUpgradeImage string, upgradeFromTs, upgradeToTs int64) {
	u.Lock()
	defer u.Unlock()
	u.UpgradeFromTs = upgradeFromTs
	u.UpgradeToTs = upgradeToTs
	u.DockerUpgradeImage = dockerUpgradeImage
}

func (u *UpgradeDataType) Get() (dockerUpgradeImage string, upgradeFromTs, upgradeToTs int64) {
	u.Lock()
	defer u.Unlock()
	dockerUpgradeImage = u.DockerUpgradeImage
	upgradeFromTs = u.UpgradeFromTs
	upgradeToTs = u.UpgradeToTs
	return
}

var UpgradeData = UpgradeDataType{}

func registerDeploymentRunnerTaskDefinition(ecsClient *ecs.Client, service, organizationId, token, region, dockerImage, cpuStr, memory, taskExecutionRoleArn, taskRoleArn string) (taskDefinitionArn string, err error) {

	runnerName := fmt.Sprintf("deployment-runner-%s", cpuStr)

	tags := strings.Split(dockerImage, ":")
	tag := "unknown"
	if len(tags) == 2 {
		tag = tags[1]
	}

	logsStreamPrefix := tag
	logGroupName := fmt.Sprintf("deployment-runner-logs-group-%s", cpuStr)

	envVars := []ecsTypes.KeyValuePair{
		{
			Name:  aws.String("Token"),
			Value: aws.String(token),
		},
		{
			Name:  aws.String("OrganizationID"),
			Value: aws.String(organizationId),
		},
		{
			Name:  aws.String("Service"),
			Value: aws.String(service),
		},
		{
			Name:  aws.String("DockerImage"),
			Value: aws.String(dockerImage),
		},
		{
			Name:  aws.String("Region"),
			Value: aws.String(region),
		},
		{
			Name:  aws.String("CpuArch"),
			Value: aws.String(cpuStr),
		},
		{
			Name:  aws.String("Memory"),
			Value: aws.String(memory),
		},
		{
			Name:  aws.String("ExecutionRoleArn"),
			Value: aws.String(taskExecutionRoleArn),
		},
		{
			Name:  aws.String("TaskRoleArn"),
			Value: aws.String(taskRoleArn),
		},
	}

	containerDefinition := ecsTypes.ContainerDefinition{
		DisableNetworking: aws.Bool(false),
		Environment:       envVars,
		Essential:         aws.Bool(true),
		Image:             aws.String(dockerImage),
		Interactive:       aws.Bool(false),
		LogConfiguration: &ecsTypes.LogConfiguration{
			LogDriver: ecsTypes.LogDriverAwslogs,
			Options: map[string]string{
				"awslogs-create-group":  "true",
				"awslogs-group":         logGroupName,
				"awslogs-region":        region,
				"awslogs-stream-prefix": logsStreamPrefix,
			},
		},
		MountPoints: []ecsTypes.MountPoint{
			{
				ContainerPath: aws.String("/var/run/docker.sock"),
				SourceVolume:  aws.String("docker-socket"),
			},
			{
				ContainerPath: aws.String("/tmp"),
				SourceVolume:  aws.String("temp"),
			},
		},
		Name:                   aws.String(runnerName),
		Privileged:             aws.Bool(false),
		PseudoTerminal:         aws.Bool(false),
		ReadonlyRootFilesystem: aws.Bool(false),
	}

	taskDefinitionFamilyName := runnerName

	cpuArch := ecsTypes.CPUArchitectureX8664

	if cpuStr == "arm" {
		cpuArch = ecsTypes.CPUArchitectureArm64
	}

	registerTaskDefinitionInput := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: []ecsTypes.ContainerDefinition{
			containerDefinition,
		},
		ExecutionRoleArn: aws.String(taskExecutionRoleArn),
		Family:           aws.String(taskDefinitionFamilyName),
		Memory:           aws.String(memory),
		NetworkMode:      ecsTypes.NetworkModeHost,
		RuntimePlatform: &ecsTypes.RuntimePlatform{
			CpuArchitecture:       cpuArch,
			OperatingSystemFamily: ecsTypes.OSFamilyLinux,
		},
		Tags: []ecsTypes.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(taskDefinitionFamilyName),
			},
			{
				Key:   aws.String("created by"),
				Value: aws.String("deployment.io"),
			},
		},
		TaskRoleArn: aws.String(taskRoleArn),
		Volumes: []ecsTypes.Volume{
			{
				Host: &ecsTypes.HostVolumeProperties{SourcePath: aws.String("/var/run/docker.sock")},
				Name: aws.String("docker-socket"),
			},
			{
				Host: &ecsTypes.HostVolumeProperties{SourcePath: aws.String("/tmp")},
				Name: aws.String("temp"),
			},
		},
	}

	registerTaskDefinitionOutput, err := ecsClient.RegisterTaskDefinition(context.TODO(), registerTaskDefinitionInput)

	if err != nil {
		return "", err
	}

	taskDefinitionArn = aws.ToString(registerTaskDefinitionOutput.TaskDefinition.TaskDefinitionArn)

	return taskDefinitionArn, nil
}

func updateDeploymentRunnerService(ecsClient *ecs.Client, organizationId, cpuStr, taskDefinitionArn string) error {
	ccName := fmt.Sprintf("deployment-runner-capacity-provider-%s", cpuStr)
	ecsClusterName := fmt.Sprintf("deployment-runner-%s-%s", cpuStr, organizationId)
	ecsServiceName := fmt.Sprintf("deployment-runner-%s", cpuStr)
	updateServiceInput := &ecs.UpdateServiceInput{
		CapacityProviderStrategy: []ecsTypes.CapacityProviderStrategyItem{{
			CapacityProvider: aws.String(ccName),
			Weight:           1,
		}},
		Cluster: aws.String(ecsClusterName),
		DeploymentConfiguration: &ecsTypes.DeploymentConfiguration{
			MaximumPercent:        aws.Int32(100),
			MinimumHealthyPercent: aws.Int32(0),
		},
		DesiredCount:         aws.Int32(1),
		EnableECSManagedTags: aws.Bool(false),
		EnableExecuteCommand: aws.Bool(false),
		PropagateTags:        ecsTypes.PropagateTagsTaskDefinition,
		Service:              aws.String(ecsServiceName),
		TaskDefinition:       aws.String(taskDefinitionArn),
	}
	_, err := ecsClient.UpdateService(context.TODO(), updateServiceInput)
	return err
}

func UpgradeDeploymentRunner(service, organizationId, token, region, dockerImage, cpuStr, memory, taskExecutionRoleArn, taskRoleArn string) error {
	dockerUpgradeImage, upgradeFromTs, upgradeToTs := UpgradeData.Get()

	fmt.Println("from upgrade time: ", upgradeFromTs)
	fmt.Println("to upgrade time: ", upgradeToTs)
	fmt.Println("upgrade image: ", dockerUpgradeImage)

	now := time.Now().Unix()

	if now > upgradeFromTs && now < upgradeToTs {
		if len(dockerUpgradeImage) > 0 && dockerImage != dockerUpgradeImage {
			//upgrade deployment runner to upgraded image
			cfg, err := config.LoadDefaultConfig(context.TODO())
			if err != nil {
				return err
			}
			ecsClient := ecs.NewFromConfig(cfg, func(o *ecs.Options) {
				o.Region = region
			})

			//register new task definition
			taskDefinitionArn, err := registerDeploymentRunnerTaskDefinition(ecsClient, service, organizationId, token, region, dockerUpgradeImage, cpuStr, memory, taskExecutionRoleArn, taskRoleArn)
			if err != nil {
				return err
			}

			//update service
			err = updateDeploymentRunnerService(ecsClient, organizationId, cpuStr, taskDefinitionArn)

			return err
		}
	}
	return nil
}
