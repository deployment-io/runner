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
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	jobTypes "github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"time"
)

// logGroupRetentionDays is the retention set on the log groups the runner creates. CloudWatch deletes each
// log event once it's older than that (a rolling window; the group and newer events stay), including old
// build logs shown in the dashboard.
const logGroupRetentionDays int32 = 90

type logGroupCreator interface {
	CreateLogGroup(ctx context.Context, params *cloudwatchlogs.CreateLogGroupInput,
		optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error)
	PutRetentionPolicy(ctx context.Context, params *cloudwatchlogs.PutRetentionPolicyInput,
		optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutRetentionPolicyOutput, error)
}

// createLogGroup creates the log group and sets its retention. A group that already exists keeps
// whatever retention it has.
func createLogGroup(ctx context.Context, client logGroupCreator, logGroupName string) error {
	_, err := client.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	})
	if err != nil {
		var rae *types.ResourceAlreadyExistsException
		if errors.As(err, &rae) {
			return nil
		}
		return fmt.Errorf("error creating loggroup: %s", err)
	}
	_, err = client.PutRetentionPolicy(ctx, &cloudwatchlogs.PutRetentionPolicyInput{
		LogGroupName:    aws.String(logGroupName),
		RetentionInDays: aws.Int32(logGroupRetentionDays),
	})
	if err != nil {
		return fmt.Errorf("error setting log group retention: %s", err)
	}
	return nil
}

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
		return nil, fmt.Errorf("error loading config for cloudwatch client: %s", err)
	}

	cloudwatchClient := cloudwatchlogs.NewFromConfig(cfg, func(options *cloudwatchlogs.Options) {
		options.Region = region_enums.Type(region).String()
	})

	logGroupName, err := commandUtils.GetLogGroupName(parameters)
	if err != nil {
		return nil, err
	}

	err = createLogGroup(context.TODO(), cloudwatchClient, logGroupName)
	if err != nil {
		return nil, err
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
			return nil, fmt.Errorf("error creating logstream: %s", err)
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
