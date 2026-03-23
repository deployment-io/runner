package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

const maxLogLines = 500

type GetDeploymentLogsAws struct{}

func (g *GetDeploymentLogsAws) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	io.WriteString(logsWriter, "Getting deployment logs from CloudWatch...\n")

	startTimeSeconds, err := jobs.GetParameterValue[int64](parameters, parameters_enums.StartTime)
	if err != nil {
		return parameters, fmt.Errorf("start_time is required: %w", err)
	}
	endTimeSeconds, err := jobs.GetParameterValue[int64](parameters, parameters_enums.EndTime)
	if err != nil {
		return parameters, fmt.Errorf("end_time is required: %w", err)
	}
	startTimeMs := startTimeSeconds * 1000
	endTimeMs := endTimeSeconds * 1000

	searchPattern, _ := jobs.GetParameterValue[string](parameters, parameters_enums.SearchPattern)

	cloudwatchLogsClient, err := cloud_api_clients.GetCloudwatchLogsClient(parameters)
	if err != nil {
		return parameters, fmt.Errorf("failed to create CloudWatch client: %w", err)
	}

	logGroupName, err := utils.GetLogGroupName(parameters)
	if err != nil {
		return parameters, fmt.Errorf("failed to get log group name: %w", err)
	}

	// Log stream prefix is optional — databases and deployments without builds won't have one
	logStreamPrefix, _ := utils.GetApplicationLogStreamPrefix(parameters)

	var logs []map[string]interface{}
	if len(searchPattern) > 0 {
		logs, err = getFilteredLogs(cloudwatchLogsClient, logGroupName, logStreamPrefix, searchPattern, startTimeMs, endTimeMs)
	} else {
		logs, err = getLogs(cloudwatchLogsClient, logGroupName, logStreamPrefix, startTimeMs, endTimeMs)
	}
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Retrieved %d log entries\n", len(logs)))

	output := map[string]interface{}{
		"logs":       logs,
		"count":      len(logs),
		"truncated":  len(logs) >= maxLogLines,
		"start_time": startTimeSeconds,
		"end_time":   endTimeSeconds,
	}
	if len(searchPattern) > 0 {
		output["search_pattern"] = searchPattern
	}

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return parameters, fmt.Errorf("failed to encode output: %w", err)
	}
	_ = jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(outputJSON))
	return parameters, nil
}

func getLogs(client *cloudwatchlogs.Client, logGroupName, logStreamPrefix string,
	startTimeMs, endTimeMs int64) ([]map[string]interface{}, error) {

	// Find the most recent log stream matching the prefix (or any stream if prefix is empty)
	describeInput := &cloudwatchlogs.DescribeLogStreamsInput{
		Descending:   aws.Bool(true),
		Limit:        aws.Int32(1),
		LogGroupName: aws.String(logGroupName),
	}
	if len(logStreamPrefix) > 0 {
		describeInput.LogStreamNamePrefix = aws.String(logStreamPrefix)
	}
	describeOutput, err := client.DescribeLogStreams(context.TODO(), describeInput)
	if err != nil {
		return nil, fmt.Errorf("failed to describe log streams: %w", err)
	}
	if len(describeOutput.LogStreams) == 0 {
		return nil, nil
	}

	logStreamName := aws.ToString(describeOutput.LogStreams[0].LogStreamName)
	var allEvents []cwTypes.OutputLogEvent
	var nextToken *string
	for {
		output, err := client.GetLogEvents(context.TODO(), &cloudwatchlogs.GetLogEventsInput{
			LogStreamName: aws.String(logStreamName),
			LogGroupName:  aws.String(logGroupName),
			StartTime:     aws.Int64(startTimeMs),
			EndTime:       aws.Int64(endTimeMs),
			NextToken:     nextToken,
			StartFromHead: aws.Bool(true),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to get log events: %w", err)
		}
		allEvents = append(allEvents, output.Events...)
		if len(allEvents) >= maxLogLines {
			allEvents = allEvents[:maxLogLines]
			break
		}
		if output.NextForwardToken == nil || aws.ToString(output.NextForwardToken) == aws.ToString(nextToken) {
			break
		}
		nextToken = output.NextForwardToken
	}

	return eventsToLogs(allEvents), nil
}

func getFilteredLogs(client *cloudwatchlogs.Client, logGroupName, logStreamPrefix, filterPattern string,
	startTimeMs, endTimeMs int64) ([]map[string]interface{}, error) {

	filterInput := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:  aws.String(logGroupName),
		StartTime:     aws.Int64(startTimeMs),
		EndTime:       aws.Int64(endTimeMs),
		FilterPattern: aws.String(fmt.Sprintf("%%%s%%", filterPattern)),
	}
	if len(logStreamPrefix) > 0 {
		filterInput.LogStreamNamePrefix = aws.String(logStreamPrefix)
	}

	var allEvents []cwTypes.FilteredLogEvent
	var nextToken *string
	for {
		filterInput.NextToken = nextToken
		output, err := client.FilterLogEvents(context.TODO(), filterInput)
		if err != nil {
			return nil, fmt.Errorf("failed to filter log events: %w", err)
		}
		allEvents = append(allEvents, output.Events...)
		if len(allEvents) >= maxLogLines {
			allEvents = allEvents[:maxLogLines]
			break
		}
		if output.NextToken == nil || aws.ToString(output.NextToken) == aws.ToString(nextToken) {
			break
		}
		nextToken = output.NextToken
	}

	return filteredEventsToLogs(allEvents), nil
}

func eventsToLogs(events []cwTypes.OutputLogEvent) []map[string]interface{} {
	logs := make([]map[string]interface{}, 0, len(events))
	for _, event := range events {
		logs = append(logs, map[string]interface{}{
			"message":       aws.ToString(event.Message),
			"timestamp_utc": time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC().Format(time.RFC3339),
		})
	}
	return logs
}

func filteredEventsToLogs(events []cwTypes.FilteredLogEvent) []map[string]interface{} {
	logs := make([]map[string]interface{}, 0, len(events))
	for _, event := range events {
		logs = append(logs, map[string]interface{}{
			"message":       aws.ToString(event.Message),
			"timestamp_utc": time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC().Format(time.RFC3339),
		})
	}
	return logs
}
