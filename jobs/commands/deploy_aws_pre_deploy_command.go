package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
)

// The pre-deploy command (e.g. database migrations) runs once per deploy of
// new code, as a one-off Fargate task from the newly registered task
// definition, before the service is created or updated. Its output is copied
// into the deploy logs; anything but exit code 0 fails the deploy, so the
// service is never moved to the new task definition.

const preDeployCommandTimeout = 30 * time.Minute

const (
	// GetLogEvents pages read per poll while the task runs, so a command that
	// writes faster than it is read can't hold off the deadline; the rest is
	// read on the next poll.
	preDeployLogPagesPerPoll = 10
	// How long the read after the task stops may take; output still unread
	// then is reported as missing.
	preDeployFinalLogReadTimeout = 2 * time.Minute
	// Longest run of a line written to the deploy logs: their reader
	// (bufio.Scanner) gives up on lines over 64 KiB.
	preDeployMaxLogLineLength = 16 * 1024
	preDeployStopTaskAttempts = 3
)

// Swapped by tests so the wait runs without real time passing.
var (
	preDeployPollInterval = 5 * time.Second
	preDeployNow          = time.Now
	preDeploySleep        = time.Sleep
	// Bound a stalled AWS call or deploy log write, so none can hold off the
	// deadline and StopTask, or hold the deploy forever.
	preDeployEcsCallTimeout  = time.Minute
	preDeployLogCallTimeout  = time.Minute
	preDeployLogWriteTimeout = time.Minute
)

// Swapped by tests so the service step runs without AWS.
var (
	preDeployEcsClient   = func(ecsClient *ecs.Client) preDeployEcsAPI { return ecsClient }
	createEcsServiceStep = createEcsServiceIfNeeded
	updateEcsServiceStep = updateEcsService
)

// preDeployEcsAPI is the part of *ecs.Client the pre-deploy command uses.
type preDeployEcsAPI interface {
	RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
}

// preDeployLogsAPI is the part of *cloudwatchlogs.Client the pre-deploy command uses.
type preDeployLogsAPI interface {
	GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// deployEcsServiceAfterPreDeploy runs the pre-deploy command with the newly
// registered task definition and, only when it succeeds, creates or updates
// the service with it. Both deploy commands call it right after
// registerTaskDefinition.
func deployEcsServiceAfterPreDeploy(parameters map[string]interface{}, ecsClient *ecs.Client, logsClient preDeployLogsAPI,
	ecsClusterArn, targetGroupArn, taskDefinitionArn string, logsWriter io.Writer) error {
	err := runPreDeployCommand(parameters, preDeployEcsClient(ecsClient), logsClient, ecsClusterArn, taskDefinitionArn, logsWriter)
	if err != nil {
		return err
	}
	_, shouldUpdateService, err := createEcsServiceStep(parameters, ecsClient, ecsClusterArn, targetGroupArn, taskDefinitionArn, logsWriter)
	if err != nil {
		return err
	}
	if shouldUpdateService {
		return updateEcsServiceStep(parameters, ecsClient, ecsClusterArn, taskDefinitionArn, logsWriter)
	}
	return nil
}

// runPreDeployCommand runs the deployment's pre-deploy command, if the Job has
// one and isn't a restart, and returns an error the deploy shows as is when it
// doesn't exit with 0.
func runPreDeployCommand(parameters map[string]interface{}, ecsClient preDeployEcsAPI, logsClient preDeployLogsAPI,
	ecsClusterArn, taskDefinitionArn string, logsWriter io.Writer) error {
	command, err := jobs.GetParameterValue[string](parameters, parameters_enums.PreDeployCommand)
	if err != nil || len(command) == 0 {
		return nil
	}
	if restartOnly, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.RestartOnly); restartOnly {
		return nil
	}
	containerName, err := getContainerName(parameters)
	if err != nil {
		return err
	}
	runTaskInput, err := getPreDeployRunTaskInput(parameters, ecsClusterArn, taskDefinitionArn, containerName, command)
	if err != nil {
		return err
	}
	logs, err := newPreDeployLogReader(parameters, logsClient, containerName, logsWriter)
	if err != nil {
		return err
	}

	// Never the command itself: it may carry a secret (DATABASE_URL=… inline).
	logs.write("Running the pre-deploy command\n")
	start := preDeployNow()
	taskArn, failure := startPreDeployTask(ecsClient, runTaskInput)
	if len(failure) > 0 {
		return preDeployFailure(parameters, ecsClient, ecsClusterArn, failure)
	}
	logs.follow(taskArn)
	stoppedTask, stopErr := waitForPreDeployTask(ecsClient, ecsClusterArn, taskArn, logs, start)
	// Read once more after the task stops, for its last lines.
	if !logs.read(math.MaxInt, preDeployNow().Add(preDeployFinalLogReadTimeout)) {
		logs.write("The pre-deploy command's output is incomplete: the rest couldn't be read in time.\n")
	}

	outcome := getPreDeployOutcome(stoppedTask, containerName)
	if stopErr != nil {
		outcome.stopError = stopErr.Error()
	}
	if outcome.exitCode != nil {
		logs.write(fmt.Sprintf("Pre-deploy command finished (exit code %d) in %s\n",
			*outcome.exitCode, preDeployNow().Sub(start).Round(time.Second)))
	}
	if message, ok := outcome.message(); !ok {
		return preDeployFailure(parameters, ecsClient, ecsClusterArn, message)
	}
	return nil
}

// startPreDeployTask returns the started task's ARN, or the first sentence of
// the deploy's error when ECS started none.
func startPreDeployTask(ecsClient preDeployEcsAPI, runTaskInput *ecs.RunTaskInput) (taskArn string, failure string) {
	ctx, cancel := context.WithTimeout(context.Background(), preDeployEcsCallTimeout)
	defer cancel()
	runTaskOutput, err := ecsClient.RunTask(ctx, runTaskInput)
	if err != nil {
		return "", fmt.Sprintf("The pre-deploy command couldn't start: %s.", err)
	}
	if len(runTaskOutput.Tasks) == 0 {
		reason := "ECS started no task"
		if len(runTaskOutput.Failures) > 0 && len(aws.ToString(runTaskOutput.Failures[0].Reason)) > 0 {
			reason = aws.ToString(runTaskOutput.Failures[0].Reason)
		}
		return "", fmt.Sprintf("The pre-deploy command couldn't start: %s.", reason)
	}
	return aws.ToString(runTaskOutput.Tasks[0].TaskArn), ""
}

// waitForPreDeployTask copies the task's output into the deploy logs until it
// stops and returns the stopped task, or stops it and returns nil when it
// doesn't finish in time, with StopTask's error if it couldn't be stopped.
func waitForPreDeployTask(ecsClient preDeployEcsAPI, ecsClusterArn, taskArn string, logs *preDeployLogReader,
	start time.Time) (*ecsTypes.Task, error) {
	deadline := start.Add(preDeployCommandTimeout)
	for {
		logs.read(preDeployLogPagesPerPoll, deadline)
		ctx, cancel := context.WithTimeout(context.Background(), preDeployEcsCallTimeout)
		describeTasksOutput, err := ecsClient.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String(ecsClusterArn),
			Tasks:   []string{taskArn},
		})
		cancel()
		// A failed describe (or a task ECS doesn't return yet) is retried on
		// the next poll; the deadline bounds how long that can go on.
		if err == nil && len(describeTasksOutput.Tasks) > 0 &&
			aws.ToString(describeTasksOutput.Tasks[0].LastStatus) == string(ecsTypes.DesiredStatusStopped) {
			return &describeTasksOutput.Tasks[0], nil
		}
		if !preDeployNow().Before(deadline) {
			return nil, stopPreDeployTask(ecsClient, ecsClusterArn, taskArn)
		}
		preDeploySleep(preDeployPollInterval)
	}
}

// stopPreDeployTask stops the timed-out task, trying a few times since a task
// left running could go on changing the database.
func stopPreDeployTask(ecsClient preDeployEcsAPI, ecsClusterArn, taskArn string) error {
	var err error
	for attempt := 1; attempt <= preDeployStopTaskAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), preDeployEcsCallTimeout)
		_, err = ecsClient.StopTask(ctx, &ecs.StopTaskInput{
			Cluster: aws.String(ecsClusterArn),
			Task:    aws.String(taskArn),
			Reason:  aws.String("The pre-deploy command didn't finish within 30 minutes"),
		})
		cancel()
		if err == nil {
			return nil
		}
		if attempt < preDeployStopTaskAttempts {
			preDeploySleep(preDeployPollInterval)
		}
	}
	return err
}

// getPreDeployRunTaskInput runs the new task definition once, in the network
// the service uses, with the deployment's container running the command
// through sh -c. The image's ENTRYPOINT still applies.
func getPreDeployRunTaskInput(parameters map[string]interface{}, ecsClusterArn, taskDefinitionArn, containerName,
	command string) (*ecs.RunTaskInput, error) {
	networkConfiguration, err := getEcsServiceNetworkConfiguration(parameters)
	if err != nil {
		return nil, err
	}
	return &ecs.RunTaskInput{
		Cluster:              aws.String(ecsClusterArn),
		Count:                aws.Int32(1),
		LaunchType:           ecsTypes.LaunchTypeFargate,
		NetworkConfiguration: networkConfiguration,
		Overrides: &ecsTypes.TaskOverride{
			ContainerOverrides: []ecsTypes.ContainerOverride{{
				Name:    aws.String(containerName),
				Command: []string{"sh", "-c", command},
			}},
		},
		StartedBy:      aws.String("deployment-io-pre-deploy"),
		TaskDefinition: aws.String(taskDefinitionArn),
	}, nil
}

// getPreDeployLogStreamName is the awslogs stream of the task's container:
// <prefix>/<container name>/<task ID>.
func getPreDeployLogStreamName(logStreamPrefix, containerName, taskArn string) string {
	taskID := taskArn[strings.LastIndex(taskArn, "/")+1:]
	return fmt.Sprintf("%s/%s/%s", logStreamPrefix, containerName, taskID)
}

type preDeployOutcome struct {
	exitCode      *int32
	stoppedReason string
	timedOut      bool
	stopError     string // StopTask's error, when a timed-out task couldn't be stopped
}

// getPreDeployOutcome reads the container's exit code from the stopped task;
// stoppedTask is nil when the command timed out.
func getPreDeployOutcome(stoppedTask *ecsTypes.Task, containerName string) preDeployOutcome {
	if stoppedTask == nil {
		return preDeployOutcome{timedOut: true}
	}
	outcome := preDeployOutcome{stoppedReason: aws.ToString(stoppedTask.StoppedReason)}
	for _, container := range stoppedTask.Containers {
		if aws.ToString(container.Name) != containerName {
			continue
		}
		outcome.exitCode = container.ExitCode
		if len(outcome.stoppedReason) == 0 {
			outcome.stoppedReason = aws.ToString(container.Reason)
		}
	}
	return outcome
}

// message is the first sentence of the deploy's error; ok means exit code 0.
func (o preDeployOutcome) message() (message string, ok bool) {
	switch {
	case o.timedOut && len(o.stopError) > 0:
		return fmt.Sprintf("The pre-deploy command didn't finish within 30 minutes and couldn't be stopped: %s.",
			strings.TrimSuffix(o.stopError, ".")), false
	case o.timedOut:
		return "The pre-deploy command didn't finish within 30 minutes and was stopped.", false
	case o.exitCode == nil:
		reason := o.stoppedReason
		if len(reason) == 0 {
			reason = "ECS gave no reason"
		}
		return fmt.Sprintf("The pre-deploy command couldn't run: %s.", strings.TrimSuffix(reason, ".")), false
	case *o.exitCode != 0:
		return fmt.Sprintf("The pre-deploy command failed with exit code %d.", *o.exitCode), false
	}
	return "", true
}

// preDeployFailure completes the message with what is serving now.
func preDeployFailure(parameters map[string]interface{}, ecsClient preDeployEcsAPI, ecsClusterArn, message string) error {
	return errors.New(preDeployFailureMessage(message, hasPreviousVersion(parameters, ecsClient, ecsClusterArn)))
}

func preDeployFailureMessage(message string, previousVersion bool) string {
	if previousVersion {
		return message + " The deploy was stopped; the previous version keeps running."
	}
	return message + " Nothing was deployed."
}

// hasPreviousVersion is whether the deployment's service already exists. When
// that can't be told, the message says only that nothing was deployed, which
// is true either way.
func hasPreviousVersion(parameters map[string]interface{}, ecsClient preDeployEcsAPI, ecsClusterArn string) bool {
	if ecsServiceArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsServiceArn); err == nil && len(ecsServiceArn) > 0 {
		return true
	}
	ecsServiceName, err := aws_utils.GetEcsServiceName(parameters)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), preDeployEcsCallTimeout)
	defer cancel()
	describeServicesOutput, err := ecsClient.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(ecsClusterArn),
		Services: []string{ecsServiceName},
	})
	if err != nil {
		return false
	}
	for _, service := range describeServicesOutput.Services {
		if aws.ToString(service.Status) == "ACTIVE" {
			return true
		}
	}
	return false
}

// preDeployLogReader copies the task's log stream into the deploy logs as it
// grows. The stream doesn't exist until the container starts.
type preDeployLogReader struct {
	client          preDeployLogsAPI
	logGroupName    string
	logStreamPrefix string
	containerName   string
	logStreamName   string
	logsWriter      io.Writer
	nextToken       *string
	reportedError   bool
	writeFailed     bool
}

// newPreDeployLogReader reads from the task definition's awslogs group and
// stream prefix.
func newPreDeployLogReader(parameters map[string]interface{}, client preDeployLogsAPI, containerName string,
	logsWriter io.Writer) (*preDeployLogReader, error) {
	logGroupName, err := commandUtils.GetLogGroupName(parameters)
	if err != nil {
		return nil, err
	}
	logStreamPrefix, err := commandUtils.GetApplicationLogStreamPrefix(parameters)
	if err != nil {
		return nil, err
	}
	return &preDeployLogReader{
		client:          client,
		logGroupName:    logGroupName,
		logStreamPrefix: logStreamPrefix,
		containerName:   containerName,
		logsWriter:      logsWriter,
	}, nil
}

// follow points the reader at the stream of the started task.
func (r *preDeployLogReader) follow(taskArn string) {
	r.logStreamName = getPreDeployLogStreamName(r.logStreamPrefix, r.containerName, taskArn)
}

// read copies at most maxPages pages of new events into the deploy logs,
// stopping at deadline. complete is false when the stream may hold more.
func (r *preDeployLogReader) read(maxPages int, deadline time.Time) (complete bool) {
	for page := 0; page < maxPages; page++ {
		if r.writeFailed {
			// Nothing more can be shown; there is nothing to report missing to.
			return true
		}
		if !preDeployNow().Before(deadline) {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), preDeployLogCallTimeout)
		output, err := r.client.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName:  aws.String(r.logGroupName),
			LogStreamName: aws.String(r.logStreamName),
			NextToken:     r.nextToken,
			StartFromHead: aws.Bool(true),
		})
		cancel()
		if err != nil {
			var notFound *cwTypes.ResourceNotFoundException
			if errors.As(err, &notFound) {
				// No output yet.
				return true
			}
			if !r.reportedError {
				// The output is best-effort; the exit code decides the deploy.
				r.reportedError = true
				r.write(fmt.Sprintf("Couldn't read the pre-deploy command's output: %s\n", err))
			}
			return false
		}
		var lines strings.Builder
		for _, event := range output.Events {
			lines.WriteString(splitLongLogLines(aws.ToString(event.Message)))
		}
		if lines.Len() > 0 {
			r.write(lines.String())
		}
		// Only an unchanged forward token marks the end of the stream; a page
		// in between may be empty.
		if output.NextForwardToken == nil || aws.ToString(output.NextForwardToken) == aws.ToString(r.nextToken) {
			return true
		}
		r.nextToken = output.NextForwardToken
	}
	return false
}

// write copies text into the deploy logs, giving up on them when a write
// fails or doesn't finish within preDeployLogWriteTimeout.
func (r *preDeployLogReader) write(text string) {
	if r.writeFailed {
		return
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(r.logsWriter, text)
		done <- err
	}()
	timer := time.NewTimer(preDeployLogWriteTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		// A failed write means the deploy logs are closed.
		r.writeFailed = err != nil
	case <-timer.C:
		r.writeFailed = true
	}
}

// splitLongLogLines ends message with a newline and breaks any line longer
// than preDeployMaxLogLineLength, at a character boundary, so the deploy logs'
// reader can take it.
func splitLongLogLines(message string) string {
	message = strings.TrimSuffix(message, "\n")
	var b strings.Builder
	b.Grow(len(message) + len(message)/preDeployMaxLogLineLength + 1)
	lineLength := 0
	for i := 0; i < len(message); i++ {
		c := message[i]
		if c == '\n' || c == '\r' {
			b.WriteByte(c)
			lineLength = 0
			continue
		}
		if lineLength >= preDeployMaxLogLineLength && utf8.RuneStart(c) {
			b.WriteByte('\n')
			lineLength = 0
		}
		b.WriteByte(c)
		lineLength++
	}
	b.WriteByte('\n')
	return b.String()
}
