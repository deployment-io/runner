package cloudwatch

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/deployment-io/jobs-runner-kit/enums/parameters_enums"
	"log"
	"time"
)

type Logger struct {
	client        *cloudwatchlogs.Client
	logGroupName  *string
	logStreamName *string
}

func New(parameters map[parameters_enums.Key]interface{}) (*Logger, error) {
	if _, ok := parameters[parameters_enums.OrganizationID]; !ok {
		return nil, fmt.Errorf("organization id is missing")
	}

	if _, ok := parameters[parameters_enums.DeploymentID]; !ok {
		return nil, fmt.Errorf("deployment id is missing")
	}

	if _, ok := parameters[parameters_enums.BuildID]; !ok {
		return nil, fmt.Errorf("build id is missing")
	}

	organizationIDString, organizationOk := parameters[parameters_enums.OrganizationID].(string)
	deploymentIDString, deploymentOk := parameters[parameters_enums.DeploymentID].(string)
	buildIDString, buildOk := parameters[parameters_enums.BuildID].(string)
	if organizationOk && deploymentOk && buildOk {
		cfg, err := config.LoadDefaultConfig(context.TODO())
		if err != nil {
			log.Fatal(err)
		}
		cloudwatchClient := cloudwatchlogs.NewFromConfig(cfg)

		//log group name will be a combination of organization id and deployment id
		logGroupName := aws.String(fmt.Sprintf("%s/%s", organizationIDString, deploymentIDString))
		createLogGroupInput := &cloudwatchlogs.CreateLogGroupInput{
			LogGroupName: logGroupName,
			//KmsKeyId:     nil,
			//Tags:         nil,
		}
		_, err = cloudwatchClient.CreateLogGroup(context.TODO(), createLogGroupInput)
		if err != nil {
			logGroupAlreadyExists := false
			var rae *types.ResourceAlreadyExistsException
			if errors.As(err, &rae) {
				logGroupAlreadyExists = true
			}
			if !logGroupAlreadyExists {
				return nil, err
			}
		}

		logStreamName := aws.String(fmt.Sprintf("%s/%s", "build", buildIDString))
		createLogStreamInput := &cloudwatchlogs.CreateLogStreamInput{
			LogGroupName:  logGroupName,
			LogStreamName: logStreamName,
		}
		_, err = cloudwatchClient.CreateLogStream(context.TODO(), createLogStreamInput)
		if err != nil {
			logStreamAlreadyExists := false
			var rae *types.ResourceAlreadyExistsException
			if errors.As(err, &rae) {
				logStreamAlreadyExists = true
			}
			if !logStreamAlreadyExists {
				return nil, err
			}
		}

		return &Logger{
			client:        cloudwatchClient,
			logGroupName:  logGroupName,
			logStreamName: logStreamName,
		}, nil
	} else {
		return nil, fmt.Errorf("organization id, deployment id, and build id is invalid")
	}
}

func (l *Logger) Log(messages []string) error {
	var logEvents []types.InputLogEvent
	for _, message := range messages {
		logEvent := types.InputLogEvent{
			Message:   aws.String(message),
			Timestamp: aws.Int64(time.Now().UnixMilli()),
		}
		logEvents = append(logEvents, logEvent)
	}
	putLogEventsInput := &cloudwatchlogs.PutLogEventsInput{
		LogEvents:     logEvents,
		LogGroupName:  l.logGroupName,
		LogStreamName: l.logStreamName,
		//SequenceToken: nil,
	}
	_, err := l.client.PutLogEvents(context.TODO(), putLogEventsInput)
	return err
}
