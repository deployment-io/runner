package cloudwatch

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	jobTypes "github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"time"
)

type Logger struct {
	client        *cloudwatchlogs.Client
	logGroupName  *string
	logStreamName *string
}

func New(parameters map[string]interface{}) (*Logger, error) {

	region, err := jobTypes.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return nil, err
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	cloudwatchClient := cloudwatchlogs.NewFromConfig(cfg, func(options *cloudwatchlogs.Options) {
		options.Region = region_enums.Type(region).String()
	})

	logGroupName, err := commandUtils.GetLogGroupName(parameters)
	if err != nil {
		return nil, err
	}

	createLogGroupInput := &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
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

	logStreamName, err := commandUtils.GetBuildLogStreamName(parameters)
	if err != nil {
		return nil, err
	}
	createLogStreamInput := &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
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
		logGroupName:  aws.String(logGroupName),
		logStreamName: aws.String(logStreamName),
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
	}
	_, err := l.client.PutLogEvents(context.TODO(), putLogEventsInput)
	return err
}
