package get_application_logs

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ankit-arora/bloom"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/automation_enums"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-playground/validator/v10"
	"io"
	"math"
	"time"
)

type Tool struct {
	Params           map[string]interface{}
	LogsWriter       io.Writer
	CallbacksHandler callbacks.Handler
	Entities         []automation_enums.Entity
	DebugOpenAICalls bool
}

func (t *Tool) entitiesString() string {
	entities := ""
	for index, entity := range t.Entities {
		entities += entity.String()
		if index < len(t.Entities)-1 {
			entities += ", "
		}
	}
	return entities
}

func getCurrentTimeString(timeZoneLocation string) (string, error) {
	// Load the specified timezone location
	location, err := time.LoadLocation(timeZoneLocation)
	if err != nil {
		return "", fmt.Errorf("failed to load timezone location: %w", err)
	}

	// Get the current time in the specified timezone
	currentTime := time.Now().In(location)

	// Format the current time using the desired layout
	timeStr := currentTime.Format("Jan 2, 2006, 3:04PM MST")

	return fmt.Sprintf("The time right now is: %s", timeStr), nil
}

func (t *Tool) Name() string {
	return "getApplicationLogs"
}

type Log struct {
	Message   string `json:"message"`
	TimeInUTC string `json:"timeInUTC"`
}

type Output struct {
	Logs []Log `json:"logs"`
}

func (t *Tool) Description() string {
	entitiesString := t.entitiesString()
	currentTimeStr, err := getCurrentTimeString("")
	if err != nil {
		currentTimeStr = ""
	}
	info := "Gets application logs for %s"
	inputInfo := `This function takes in a time range and a search pattern as an input. The time range is mandatory and the search pattern is optional. 
You should ask the user if the time range is not available.`
	outputInfo := `The output is in a structured JSON object format with the following fields:
1. "logs" (array of objects):
  An array of objects containing the following fields:
  1. "message" (string):
    The content of the log message.
  2. "timeInUTC" (string):
    The time of the log message in UTC. The time is in the format "Jan 2, 2022, 3:04PM MST".

Output Format Example:
{
 "logs": [
     {
       "message": "INFO  [main] org.apache.catalina.startup.Catalina: Starting Server",
       "timeInUTC": "Jan 2, 2022, 3:04PM MST"
     }
  ],
}
`
	description := ""
	info = fmt.Sprintf(info, entitiesString)
	description += fmt.Sprintf("%s\n", info)
	description += fmt.Sprintf("%s\n", inputInfo)
	description += fmt.Sprintf("%s\n", outputInfo)
	if len(currentTimeStr) > 0 {
		description += fmt.Sprintf("%s\n", currentTimeStr)
	}
	return description
}

func convertEpochMilliToTimeString(epochInMilli int64, timeZoneLocation string) (string, error) {
	timeStr := "--"
	if epochInMilli > 0 {
		t := time.UnixMilli(epochInMilli)
		location, err := time.LoadLocation(timeZoneLocation)
		if err != nil {

			return "", err
		}
		t = t.In(location)
		timeStr = t.Format("Jan 2, 2006, 3:04PM MST")
	}
	return timeStr, nil
}

func convertTimeStringToEpoch(timeString, timeZoneLocation string) (int64, error) {
	// Layout for parsing time
	layout := "Jan 2, 2006, 3:04PM MST"
	// Load the specified timezone location
	location, err := time.LoadLocation(timeZoneLocation)
	if err != nil {
		return 0, err
	}
	// Parse the input time string into a time object
	parsedTime, err := time.ParseInLocation(layout, timeString, location)
	if err != nil {
		return 0, err
	}
	// Return the epoch time in seconds
	return parsedTime.UnixMilli(), nil
}

type Input struct {
	StartTime     string `json:"start_time" validate:"required"`
	EndTime       string `json:"end_time" validate:"required"`
	SearchPattern string `json:"search_pattern"`
}

func parseAndValidateInput(response, location string, input *Input) (int64, int64, string, error) {
	// Parse the JSON response into a map
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(response), &raw); err != nil {
		return 0, 0, "", fmt.Errorf("failed to parse JSON: %w", err)
	}
	startTime, ok := raw["start_time"]
	if !ok {
		return 0, 0, "", fmt.Errorf("start_time key not found")
	}
	endTime, ok := raw["end_time"]
	if !ok {
		return 0, 0, "", fmt.Errorf("end_time key not found")
	}
	sp, searchPatternExists := raw["search_pattern"]
	s, ok := startTime.(string)
	if !ok {
		return 0, 0, "", fmt.Errorf("start time is not a string")
	}
	e, ok := endTime.(string)
	if !ok {
		return 0, 0, "", fmt.Errorf("end time is not a string")
	}
	// Map the parsed response into the struct
	input.StartTime = s
	input.EndTime = e
	if searchPatternExists {
		spStr, spIsString := sp.(string)
		if !spIsString {
			return 0, 0, "", fmt.Errorf("search pattern is not a string")
		}
		if len(spStr) > 0 {
			input.SearchPattern = spStr
		}
	}
	// Validate the struct
	validate := validator.New()
	if err := validate.Struct(input); err != nil {
		return 0, 0, "", fmt.Errorf("validation error: %w", err)
	}
	startTimeTs, err := convertTimeStringToEpoch(input.StartTime, location)
	if err != nil {
		return 0, 0, "", fmt.Errorf("failed to convert start time to epoch: %w", err)
	}
	endTimeTs, err := convertTimeStringToEpoch(input.EndTime, location)
	if err != nil {
		return 0, 0, "", fmt.Errorf("failed to convert end time to epoch: %w", err)
	}
	return startTimeTs, endTimeTs, input.SearchPattern, nil
}

func getEventLogs(cloudwatchLogsClient *cloudwatchlogs.Client, logGroupName, logStreamPrefix string,
	endTimeInMilliseconds, startTimeInMilliSeconds int64, logsWriter io.Writer, filter *bloom.BloomFilter, k int,
	threshold float64) ([]Log, error) {
	var logs []Log
	describeLogStreamsOutput, err := cloudwatchLogsClient.DescribeLogStreams(context.TODO(), &cloudwatchlogs.DescribeLogStreamsInput{
		Descending:          aws.Bool(true),
		Limit:               aws.Int32(1),
		LogGroupName:        aws.String(logGroupName),
		LogStreamNamePrefix: aws.String(logStreamPrefix),
		//NextToken:           nil,
		//OrderBy: "",
	})
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Error getting log streams: %s\n", err))
		return nil, fmt.Errorf("failed to get log streams: %w", err)
	}
	if len(describeLogStreamsOutput.LogStreams) == 0 {
		io.WriteString(logsWriter, fmt.Sprintf("No log streams found for the given log group name and log stream prefix: %s\n", logGroupName))
		return logs, nil
	}
	logStreamName := aws.ToString(describeLogStreamsOutput.LogStreams[0].LogStreamName)
	var allLogEvents []types.OutputLogEvent
	var nextBackwardToken *string
	for {
		getLogEventsOutput, err := cloudwatchLogsClient.GetLogEvents(context.TODO(), &cloudwatchlogs.GetLogEventsInput{
			LogStreamName: aws.String(logStreamName),
			EndTime:       aws.Int64(endTimeInMilliseconds),
			LogGroupName:  aws.String(logGroupName),
			StartTime:     aws.Int64(startTimeInMilliSeconds),
			NextToken:     nextBackwardToken,
			StartFromHead: aws.Bool(false),
		})

		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Error getting logs from cloudwatch: %s\n", err))
			return nil, fmt.Errorf("failed to get logs from cloudwatch: %w", err)
		}

		if len(getLogEventsOutput.Events) > 0 {
			allLogEvents = append(allLogEvents, getLogEventsOutput.Events...)
		}

		if getLogEventsOutput.NextBackwardToken == nil || aws.ToString(getLogEventsOutput.NextBackwardToken) == aws.ToString(nextBackwardToken) {
			break // No more pages
		}
		nextBackwardToken = getLogEventsOutput.NextBackwardToken
	}

	originalLogsCount = len(allLogEvents)

	for _, event := range allLogEvents {
		eventMessage := *event.Message
		searchResult := searchSimilarLogs(eventMessage, filter, k, threshold)
		if !searchResult {
			addLogToBloomFilter(eventMessage, filter, k)
			tsInMilli := *event.Timestamp
			//assume UCT for now
			timeZoneLocation := ""
			timeString, err := convertEpochMilliToTimeString(tsInMilli, timeZoneLocation)
			if err != nil {
				io.WriteString(logsWriter, fmt.Sprintf("Error converting epoch to time: %s\n", err))
				return nil, fmt.Errorf("failed to convert epoch to time: %w", err)
			}
			logs = append(logs, Log{
				Message:   eventMessage,
				TimeInUTC: timeString,
			})
		}
	}
	filteredLogsCount = len(logs)
	return logs, nil
}

func getFilteredEventLogs(cloudwatchLogsClient *cloudwatchlogs.Client, logGroupName, logStreamPrefix, filterPattern string,
	endTimeInMilliseconds, startTimeInMilliseconds int64, logsWriter io.Writer, filter *bloom.BloomFilter, k int,
	threshold float64) ([]Log, error) {
	var logs []Log
	var filteredLogEvents []types.FilteredLogEvent
	var nextToken *string
	for {
		filterLogEventsOutput, err := cloudwatchLogsClient.FilterLogEvents(context.TODO(), &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:        aws.String(logGroupName),
			LogStreamNamePrefix: aws.String(logStreamPrefix),
			StartTime:           aws.Int64(startTimeInMilliseconds),
			EndTime:             aws.Int64(endTimeInMilliseconds),
			FilterPattern:       aws.String(filterPattern),
			NextToken:           nextToken,
		})
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Error filtering logs from cloudwatch: %s\n", err))
			return nil, fmt.Errorf("failed to filter logs from cloudwatch: %w", err)
		}

		filteredLogEvents = append(filteredLogEvents, filterLogEventsOutput.Events...)

		if filterLogEventsOutput.NextToken == nil || aws.ToString(filterLogEventsOutput.NextToken) == aws.ToString(nextToken) {
			break // No more pages
		}
		nextToken = filterLogEventsOutput.NextToken
	}
	for _, event := range filteredLogEvents {
		eventMessage := *event.Message
		searchResult := searchSimilarLogs(eventMessage, filter, k, threshold)
		if !searchResult {
			addLogToBloomFilter(eventMessage, filter, k)
			tsInMilli := *event.Timestamp
			//assume UCT for now
			timeZoneLocation := ""
			timeString, err := convertEpochMilliToTimeString(tsInMilli, timeZoneLocation)
			if err != nil {
				io.WriteString(logsWriter, fmt.Sprintf("Error converting epoch to time: %s\n", err))
				return nil, fmt.Errorf("failed to convert epoch to time: %w", err)
			}
			logs = append(logs, Log{
				Message:   eventMessage,
				TimeInUTC: timeString,
			})
		}
	}
	return logs, nil
}

func generateShingles(log string, k int) []string {
	if k <= 0 || k > len(log) { // Ensure k is valid
		return []string{}
	}

	var shingles []string
	for i := 0; i <= len(log)-k; i++ {
		shingles = append(shingles, log[i:i+k])
	}
	return shingles
}

func addLogToBloomFilter(log string, filter *bloom.BloomFilter, k int) {
	shingles := generateShingles(log, k)
	for _, shingle := range shingles {
		filter.Add([]byte(shingle)) // Add shingle to Bloom Filter
	}
}

func searchSimilarLogs(query string, filter *bloom.BloomFilter, k int, threshold float64) bool {
	shingles := generateShingles(query, k)
	matchCount := 0
	totalShingles := len(shingles)
	if totalShingles == 0 {
		return false
	}

	for _, shingle := range shingles {
		if filter.Test([]byte(shingle)) { // Check membership in Bloom Filter
			matchCount++
		}
	}

	similarity := float64(matchCount) / float64(totalShingles)
	return similarity >= threshold
}

var originalLogsCount int
var filteredLogsCount int

func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	if len(input) == 0 {
		return "Input cannot be empty. Please provide required parameters of start time and end time.", nil
	}

	if t.CallbacksHandler != nil {
		info := fmt.Sprintf("Getting application logs with input: %s", input)
		t.CallbacksHandler.HandleToolStart(ctx, info)
	}

	inputJsonObj := &Input{}
	startTimeInMilliSeconds, endTimeInMilliseconds, searchPattern, err := parseAndValidateInput(input, "", inputJsonObj)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error parsing input: %s", err))
		}
		return "Please make sure that the start time and end time are present in the following format: 'Jan 2, 2022, 3:04PM MST'", nil
	}
	if startTimeInMilliSeconds >= endTimeInMilliseconds {
		return "start_time must be before end_time.", nil
	}
	//TODO assume that services use Cloudwatch logs for now
	cloudwatchLogsClient, err := cloud_api_clients.GetCloudwatchLogsClient(t.Params)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting Cloudwatch client: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	logGroupName, err := commandUtils.GetLogGroupName(t.Params)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting log group name: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	logStreamPrefix, err := commandUtils.GetApplicationLogStreamPrefix(t.Params)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting application log stream prefix: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	var logs []Log
	numElements := 1000000
	falsePositiveRate := 0.001
	m := uint(-float64(numElements) * math.Log(falsePositiveRate) / (math.Log(2) * math.Log(2)))
	numHashFunctions := math.Ceil((float64(m) / float64(numElements)) * math.Log(2))
	filter := bloom.New(m, uint(numHashFunctions))
	k := 4
	threshold := 0.87
	if len(searchPattern) > 0 {
		logs, err = getFilteredEventLogs(cloudwatchLogsClient, logGroupName, logStreamPrefix, searchPattern,
			endTimeInMilliseconds, startTimeInMilliSeconds, t.LogsWriter, filter, k, threshold)
	} else {
		logs, err = getEventLogs(cloudwatchLogsClient, logGroupName, logStreamPrefix, endTimeInMilliseconds,
			startTimeInMilliSeconds, t.LogsWriter, filter, k, threshold)
	}
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting logs from Cloudwatch: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}

	if len(logs) == 0 {
		return "No logs found for the given time range", nil
	}

	outBytes, err := json.Marshal(Output{Logs: logs})
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error marshalling output: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	out := string(outBytes)
	if t.CallbacksHandler != nil {
		smallOut := out
		if !t.DebugOpenAICalls {
			//show all logs only in debug mode
			if len(smallOut) > 200 {
				smallOut = smallOut[:200] + "..."
			}
		}
		info := fmt.Sprintf("Exiting get application logs with output: %s : original logs count: %d : "+
			"filtered logs count: %d", smallOut, originalLogsCount, filteredLogsCount)
		t.CallbacksHandler.HandleToolEnd(ctx, info)
	}
	return out, nil
}

func getSearchPatternDescription() string {
	return `The search pattern is in the regex pattern supported by CloudWatch Logs for filtering the log events. 
When using regex to search and filter log data, you must surround your expressions with %.

Rules:
1. Supported characters:
   - Alphanumeric characters: A-Z, a-z, 0-9.
   - Symbol characters: '_', '#', '=', '@', '/', ';', ',', and '-'.
	 - For example, %something!% would be rejected since '!' is not supported.

2. Supported operators:
   - ^ : Anchors the match to the beginning of a string (e.g., %^[hc]at% matches "hat" or "cat" at the start of a string).
   - $ : Anchors the match to the end of a string (e.g., %[hc]at$% matches "hat" or "cat" at the end of a string).
   - ? : Matches zero or one instances of the preceding term (e.g., %colou?r% matches "color" and "colour").
   - [] : Defines a character class (e.g., %[a-z]% matches any lowercase letter from "a" to "z").
   - {m,n} : Matches the preceding term at least m and at most n times (e.g., %a{3,5}% matches "aaa", "aaaa", or "aaaaa").
   - | : Boolean "Or" operator, matching the term on either side (e.g., %gra|ey% matches "gray" or "grey").
   - \\ : Escape character for literal matching (e.g., %10\\.10\\.0\\.1% matches the IP address "10.10.0.1").
   - * : Matches zero or more instances of the preceding term (e.g., %ab*c% matches "ac", "abc", "abbbc").
   - + : Matches one or more instances of the preceding term (e.g., %ab+c% matches "abc", "abbc", "abbbc").
   - . : Matches any single character (e.g., %.at% matches three-character strings like "hat", "cat").

3. Special sequences:
   - \d, \D : Matches a digit/non-digit character (e.g., %\d% is equivalent to %[0-9]%, and %\D% is equivalent to %[^0-9]%).
   - \s, \S : Matches a space/non-space character (e.g., tabs, spaces, newlines).
   - \w, \W : Matches an alphanumeric/non-alphanumeric character (e.g., %\w% is equivalent to %[a-zA-Z_0-9]%, and %\W% is equivalent to %[^a-zA-Z_0-9]%).
   - \xhh : Matches ASCII characters by hexadecimal code (e.g., %\x3A% matches ':' and %\x28% matches '(').

4. Unsupported features:
   - Parentheses () for subpattern grouping are not supported.
   - Multi-byte characters are not supported.

Make sure your regex adheres to these rules to successfully filter CloudWatch log data.`
}

func (t *Tool) Parameters() map[string]any {
	// getSearchPatternDescription returns the description of the search pattern
	// rules supported by CloudWatch logs regex filtering.

	properties := map[string]any{
		"start_time": map[string]string{"title": "start_time", "type": "string", "description": "The start time for getting the logs." +
			" The time should be in the format 'Jan 2, 2022, 3:04PM MST'."},
		"end_time": map[string]string{"title": "end_time", "type": "string", "description": "The end time for getting the logs. " +
			"The time should be in the format 'Jan 2, 2022, 3:04PM MST'."},
	}
	//TODO assume that cloudwatch is getting used for now
	searchPatternDescription := getSearchPatternDescription()
	properties["search_pattern"] = map[string]string{"title": "search_pattern", "type": "string", "description": searchPatternDescription}
	parameters := map[string]any{
		"properties": properties,
		"required":   []string{"start_time", "end_time"},
		"type":       "object",
	}
	return parameters
}

var _ tools.ToolWithParameters = &Tool{}
