package cloudwatch

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	jobTypes "github.com/deployment-io/deployment-runner-kit/jobs"
	"log"
	"time"
)

type Logger struct {
	client        *cloudwatchlogs.Client
	logGroupName  *string
	logStreamName *string
}

func New(parameters map[parameters_enums.Key]interface{}) (*Logger, error) {
	organizationID, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return nil, err
	}

	deploymentID, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return nil, err
	}

	buildIDString, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return nil, err
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatal(err)
	}
	cloudwatchClient := cloudwatchlogs.NewFromConfig(cfg)

	//log group name will be a combination of organization id and deployment id
	logGroupName := aws.String(fmt.Sprintf("%s/%s", organizationID, deploymentID))
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
}

func (l *Logger) Log(messages []string) error {
	var logEvents []types.InputLogEvent
	for _, message := range messages {
		if len(message) > 0 {
			logEvent := types.InputLogEvent{
				Message:   aws.String(message),
				Timestamp: aws.Int64(time.Now().UnixMilli()),
			}
			logEvents = append(logEvents, logEvent)
		}
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
