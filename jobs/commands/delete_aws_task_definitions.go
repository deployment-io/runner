package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

// Swapped by tests so deleting runs without real time passing.
var deleteTaskDefinitionPause = time.Second

// deleteTaskDefinitionsAPI is the part of *ecs.Client deleting a service's
// task definitions uses.
type deleteTaskDefinitionsAPI interface {
	ListTaskDefinitions(ctx context.Context, params *ecs.ListTaskDefinitionsInput, optFns ...func(*ecs.Options)) (*ecs.ListTaskDefinitionsOutput, error)
	DeregisterTaskDefinition(ctx context.Context, params *ecs.DeregisterTaskDefinitionInput, optFns ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error)
	DeleteTaskDefinitions(ctx context.Context, params *ecs.DeleteTaskDefinitionsInput, optFns ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error)
}

// deleteTaskDefinitions deregisters and deletes the active revisions of the
// deployment's task definition family, td-<deploymentID>, and of the family
// its pre-deploy command runs from, td-<deploymentID>-pre-deploy. A family
// that doesn't exist has no revisions.
func deleteTaskDefinitions(parameters map[string]interface{}, ecsClient deleteTaskDefinitionsAPI, logsWriter io.Writer) error {
	taskDefinitionFamilyName, err := getTaskDefinitionFamilyName(parameters)
	if err != nil {
		return err
	}
	for _, family := range []string{taskDefinitionFamilyName, taskDefinitionFamilyName + "-pre-deploy"} {
		if err := deleteTaskDefinitionFamily(ecsClient, family, logsWriter); err != nil {
			return err
		}
	}
	return nil
}

func deleteTaskDefinitionFamily(ecsClient deleteTaskDefinitionsAPI, family string, logsWriter io.Writer) error {
	var taskDefinitionArns []string
	paginator := ecs.NewListTaskDefinitionsPaginator(ecsClient, &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(family),
	})
	for paginator.HasMorePages() {
		listTaskDefinitionsOutput, err := paginator.NextPage(context.TODO())
		if err != nil {
			return err
		}
		taskDefinitionArns = append(taskDefinitionArns, listTaskDefinitionsOutput.TaskDefinitionArns...)
	}
	for _, taskDefinitionArn := range taskDefinitionArns {
		io.WriteString(logsWriter, fmt.Sprintf("Deleting task definition: %s\n", taskDefinitionArn))
		_, err := ecsClient.DeregisterTaskDefinition(context.TODO(), &ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String(taskDefinitionArn)})
		if err != nil {
			return err
		}
		time.Sleep(deleteTaskDefinitionPause)
	}
	// DeleteTaskDefinitions takes at most 10 revisions at a time.
	for len(taskDefinitionArns) > 0 {
		n := min(8, len(taskDefinitionArns))
		_, err := ecsClient.DeleteTaskDefinitions(context.TODO(), &ecs.DeleteTaskDefinitionsInput{TaskDefinitions: taskDefinitionArns[:n]})
		if err != nil {
			return err
		}
		taskDefinitionArns = taskDefinitionArns[n:]
	}
	return nil
}
