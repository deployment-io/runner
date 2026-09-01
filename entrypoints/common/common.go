package common

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goConcurrentPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline"
	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/jobs/commands"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers"
)

func allocateJobs(pendingJobs []pendingJobType) <-chan pendingJobType {
	jobsStream := make(chan pendingJobType)
	go func() {
		defer close(jobsStream)
		for _, job := range pendingJobs {
			jobsStream <- job
		}
	}()
	return jobsStream
}

func getJobResult(job pendingJobType, error string, parameters map[string]interface{}) completingJobType {
	result := completingJobType{
		error:          error,
		id:             job.jobID,
		organizationID: job.organizationID,
	}
	// Extract job output JSON string from parameters if present (e.g., from get_deployment_logs command)
	// Passed through as-is to deployment-server which unmarshals before persisting
	if parameters != nil {
		jobOutputJSON, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
		if err == nil && len(jobOutputJSON) > 0 {
			result.output = jobOutputJSON
		}
	}
	return result
}

// releaseJobAttempts / releaseJobRetryInterval bound the retry when handing
// a job back to the server. The interval is deliberately longer than the
// client's 5s reconnect cycle, so a release refused because the RPC
// connection was momentarily down gets at least one attempt after it
// returns.
//
// Retrying matters because the alternative is severe out of proportion to
// the cause: a failed release leaves the job Running, the stuck-job cron
// marks it TimedOut, and the user sees a FAILED deployment whose only
// cause was the runner being briefly busy and briefly disconnected.
// Holding a worker for a few seconds is far cheaper than that.
const (
	releaseJobAttempts      = 3
	releaseJobRetryInterval = 6 * time.Second
)

// releaseJobWithRetry hands a job back to the pending queue, retrying a
// bounded number of times. Returns the last error if every attempt fails.
func releaseJobWithRetry(c *client.RunnerClient, pendingJob pendingJobType) error {
	var err error
	for attempt := 0; attempt < releaseJobAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(releaseJobRetryInterval)
		}
		if err = c.ReleaseJobs([]string{pendingJob.jobID}, pendingJob.organizationID,
			"runner at memory capacity"); err == nil {
			return nil
		}
	}
	return err
}

func handleLogEnd(err error, jobID string, logsWriter io.Writer) {
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Error in executing - %s - %s\n", jobID, err.Error()))
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Successfully executed - %s\n", jobID))
	}
}

func executeJobs(jobsStream <-chan pendingJobType, noOfWorkers int, mode runner_enums.Mode, globalOrganizationIdFromEnv string, c *client.RunnerClient) <-chan completingJobType {
	resultsStream := make(chan completingJobType)
	go func() {
		defer close(resultsStream)
		var wg sync.WaitGroup
		for i := 0; i < noOfWorkers; i++ {
			wg.Add(1)
			//each job executed concurrently
			go func() {
				defer func() {
					wg.Done()
				}()
				for pendingJob := range jobsStream {
					func(pendingJob pendingJobType) {
						parameters := pendingJob.parameters
						// Reserve host memory for the WHOLE command sequence
						// before doing ANYTHING else, and hand the job back if
						// there is no room.
						//
						// First in the function on purpose. A requeued job must
						// leave no trace: no logger, no job log stream, no
						// heartbeat goroutine, and above all no log lines. The
						// runner re-polls every 10s, so a job blocked behind a
						// long Assistant session (up to 4h) is offered and
						// returned ~1440 times — writing "at capacity" into the
						// job's own log each time would bury the real output
						// under a thousand identical lines and ship every one of
						// them. The user's signal is that the job simply stays
						// pending, which the dashboard already shows.
						//
						// Reserving here rather than beside each container spawn
						// also means a job that cannot be admitted has done no
						// work: reserve inside RunAgentStep and the Step would
						// already have cloned its repo, and would redo that clone
						// on every retry.
						//
						// Jobs that spawn no container (most of the 32 command
						// types are AWS API calls) report zero and skip this
						// entirely, so a busy runner never delays them.
						if requiredMemory := commands.JobMemoryBytes(pendingJob.commandEnums); requiredMemory > 0 {
							releaseMemory, admitted := commands.TryAcquireMemory(requiredMemory)
							if !admitted {
								// NOT a failure. No result is pushed to
								// resultsStream — that path marks the job
								// complete. The job returns to pending and is
								// offered again once capacity frees.
								// Logged to the RUNNER's log, not the job's.
								// The job's log has to stay clean for its
								// eventual real run (this fires ~1440 times
								// behind a 4h session), but going silent would
								// leave an operator unable to tell "runner at
								// capacity" apart from a job that was never
								// picked up at all. The runner's log is
								// operator-facing and ships to CloudWatch,
								// where this cadence is unremarkable.
								log.Printf("runner at memory capacity (%d MB needed of a %d MB budget); "+
									"returning job %s to the pending queue",
									requiredMemory/(1024*1024), commands.MemoryBudgetBytes()/(1024*1024),
									pendingJob.jobID)
								if releaseErr := releaseJobWithRetry(c, pendingJob); releaseErr != nil {
									// Every attempt failed, so the job stays
									// Running and the server's stuck-job cron
									// will time it out — a FAILED deployment
									// caused only by the runner being busy,
									// which is what this mechanism exists to
									// remove. Nothing better is available from
									// here, but it must be loud.
									log.Printf("FAILED to return job %s to the pending queue after retries; "+
										"the server will time it out: %s", pendingJob.jobID, releaseErr)
								}
								return
							}
							defer releaseMemory()
						}
						//add job id in parameters
						_ = jobs.SetParameterValue(parameters, parameters_enums.JobID, pendingJob.jobID)
						//add organization id from job in parameters
						_ = jobs.SetParameterValue(parameters, parameters_enums.OrganizationIdFromJob, pendingJob.organizationID)
						if mode == runner_enums.Saas {
							//rewrite to global org id for saas mode
							_ = jobs.SetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace, globalOrganizationIdFromEnv)
						}
						logger, err := loggers.Get(parameters)
						if err != nil {
							result := getJobResult(pendingJob, err.Error(), nil)
							resultsStream <- result
							loggers.AddJobLogsPipeline.Add(pendingJob.jobID, loggers.JobLog{
								Logger:         nil,
								Message:        fmt.Sprintf("Error in executing - %s - %s\n", pendingJob.jobID, err.Error()),
								Ts:             time.Now().Unix(),
								OrganizationID: pendingJob.organizationID,
							})
							//if job is a build type it will be marked done; if it's a Task
							//Step Job the per-Task working dir is cleaned up instead
							if commandUtils.IsSessionMode(parameters) {
								cleanupSessionWorkDir(parameters)
							} else if commandUtils.IsTasksMode(parameters) {
								<-commands.MarkStepDone(parameters, err)
							} else {
								<-commands.MarkDeploymentDone(parameters, err)
							}
							return
						}
						logsWriter, err := loggers.GetJobLogsWriter(pendingJob.jobID, pendingJob.organizationID, logger, mode)
						if err != nil {
							result := getJobResult(pendingJob, err.Error(), nil)
							resultsStream <- result
							return
						}
						defer logsWriter.Close()
						jobDoneSignal := make(chan struct{})
						defer close(jobDoneSignal)
						// liveProgress is the per-job atomic snapshot a
						// ProgressEmittingCommand publishes into via the sink
						// callback installed below. The heartbeat poller reads
						// it on each call and forwards to the server. nil
						// pointer = "no progress yet" — runner sends a nil
						// LiveProgress on the heartbeat, server skips the
						// progress write.
						var liveProgress atomic.Pointer[jobs.LiveProgressV1]
						stopJobSignal := getJobStopSignal(pendingJob, jobDoneSignal, c, &liveProgress, logsWriter)
						for _, commandEnum := range pendingJob.commandEnums {
							select {
							case <-stopJobSignal:
								errStoppedByUser := types.ErrJobStoppedByUser
								handleLogEnd(errStoppedByUser, pendingJob.jobID, logsWriter)
								// Pass parameters (not nil) so any JobOutput merged by
								// prior commands — e.g. RunAgentStep's partial agent
								// block with token usage / cost — is persisted for the
								// stopped Job rather than silently dropped.
								result := getJobResult(pendingJob, errStoppedByUser.Error(), parameters)
								resultsStream <- result
								//if job is a deployment/build/preview type, this will be marked them done;
								//if it's a Task Step Job, the per-Task working dir is cleaned up instead
								if commandUtils.IsSessionMode(parameters) {
									cleanupSessionWorkDir(parameters)
								} else if commandUtils.IsTasksMode(parameters) {
									<-commands.MarkStepDone(parameters, errStoppedByUser)
								} else {
									<-commands.MarkDeploymentDone(parameters, errStoppedByUser)
								}
								return
							default:
								command, err := commands.Get(commandEnum)
								if err != nil {
									handleLogEnd(err, pendingJob.jobID, logsWriter)
									result := getJobResult(pendingJob, err.Error(), parameters)
									resultsStream <- result
									return
								}
								// Plumb the stop signal into commands that opt in.
								// Outer loop only checks stopJobSignal between
								// commands; long-running commands (RunAgentStep,
								// build steps) need to honor the signal mid-run
								// to actually preempt their subprocess. Short
								// commands don't implement and remain unaffected.
								if stoppable, ok := command.(jobs.StoppableCommand); ok {
									stoppable.SetStopSignal(stopJobSignal)
								}
								// Plumb the progress sink into commands that opt in.
								// The sink stores the snapshot into the per-job
								// atomic; the heartbeat poller reads it on its next
								// tick. Closure captures liveProgress by reference
								// so the same atomic survives across the per-Step-
								// Job command sequence (CheckoutRepo → RunAgentStep
								// → CommitAndPush → OpenPullRequest); only
								// RunAgentStep emits progress today, but the others
								// inherit the same atomic in case they ever do.
								if emitting, ok := command.(jobs.ProgressEmittingCommand); ok {
									emitting.SetProgressSink(func(p jobs.LiveProgressV1) {
										liveProgress.Store(&p)
									})
								}
								parameters, err = command.Run(parameters, logsWriter)
								if err != nil {
									handleLogEnd(err, pendingJob.jobID, logsWriter)
									// Pass parameters (not nil): RunAgentStep merges its
									// partial result (token usage / cost / changes summary)
									// into JobOutput before returning a failure or stop, so
									// preserve it for the failed/stopped Job's projection.
									result := getJobResult(pendingJob, err.Error(), parameters)
									resultsStream <- result
									return
								}
							}
						}
						// Success path: every command in the sequence ran
						// without error. For Tasks Step Jobs we still need
						// to clean up the per-Task working dir under
						// /tmp/<orgID>/<taskID>/ — the deferred MarkStepDone
						// in each command only fires on error. Done here
						// (rather than from inside the last command, e.g.
						// OpenPullRequest) so the cleanup site doesn't
						// shift if the command sequence is reordered or
						// extended. Non-Tasks jobs handle their own
						// completion bookkeeping inside the build/preview
						// commands themselves.
						if commandUtils.IsSessionMode(parameters) {
							cleanupSessionWorkDir(parameters)
						} else if commandUtils.IsTasksMode(parameters) {
							<-commands.MarkStepDone(parameters, nil)
						}
						handleLogEnd(nil, pendingJob.jobID, logsWriter)
						result := getJobResult(pendingJob, "", parameters)
						resultsStream <- result
					}(pendingJob)
				}
			}()
		}
		wg.Wait()
	}()
	return resultsStream
}

// cleanupSessionWorkDir removes an interactive Assistant session's cloned repo
// and agentbox IO dirs under /tmp/<org>/sessions/<jobID>. A session is neither a
// deployment nor a Task Step, so neither MarkDeploymentDone nor MarkStepDone
// applies; this is the session analog of MarkStepDone's per-Task cleanup, keyed
// by OrganizationIDNamespace to match where runForSession cloned.
func cleanupSessionWorkDir(parameters map[string]interface{}) {
	orgID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	jobID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	if len(orgID) == 0 || len(jobID) == 0 {
		return
	}
	_ = os.RemoveAll(commandUtils.GetSessionRepositoriesBaseDir(orgID, jobID))
}

// getJobStopSignal polls UpsertJobHeartbeat every 5 seconds and returns
// a channel that is CLOSED when the server reports the Job has been
// moved to Stopping (or when jobDoneSignal closes, indicating shutdown).
//
// Each heartbeat call also forwards the latest live-progress snapshot
// from the per-job atomic if one has been published by a Progress-
// EmittingCommand (Phase 5.5b). Pointer is nil for command sequences
// that don't include any progress-emitting command, or for the first
// few heartbeats before the agent's parser has produced a snapshot.
//
// Closed-channel-as-broadcast (vs. send-then-close) is deliberate:
//
//   - The same channel is read by two consumers — the outer loop's
//     between-commands select AND any StoppableCommand's SetStopSignal
//     watcher (RunAgentStep, today). Sending a single value to an
//     unbuffered channel only wakes one reader; the other relies on
//     the post-send close. Closing directly wakes ALL current and
//     future readers atomically and idempotently — the canonical Go
//     pattern for one-shot broadcast.
//
//   - Send-then-close also has a goroutine-leak edge case: if the
//     consumer side returns via an error path BEFORE reading the
//     pending send, the producer is wedged on the unbuffered send
//     (it's not in the select anymore, so jobDoneSignal closing
//     doesn't free it). Close-only never blocks the producer.
//
// Consumers don't need to change — `case <-jobStopSignal:` reads the
// zero value from a closed channel just as it would have read the
// sent struct{}{}. Both fire the case identically.
func getJobStopSignal(job pendingJobType, jobDoneSignal <-chan struct{}, c *client.RunnerClient, liveProgress *atomic.Pointer[jobs.LiveProgressV1], logsWriter io.Writer) <-chan struct{} {
	jobStopSignal := make(chan struct{})
	go func() {
		defer close(jobStopSignal)
		for {
			select {
			case <-jobDoneSignal:
				return
			default:
				//ignoring error in client
				isStopping, _ := c.UpsertJobHeartbeat(job.jobID, job.organizationID, liveProgress.Load())
				if isStopping {
					// Surface the stop in the job's log stream so the user sees
					// the runner acting on their request (a Task Step SIGTERMs
					// its agent; a build aborts) instead of the stream going
					// quiet until the cancelled result lands.
					io.WriteString(logsWriter, "Stop requested by user. Stopping...\n")
					return // deferred close broadcasts to all readers
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()
	return jobStopSignal
}

func sendJobResults(resultsStream <-chan completingJobType,
	noOfResultWorkers int, jobsDonePipeline *goPipeline.Pipeline[string, jobs.CompletingJobDtoV1]) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer func() {
			done <- struct{}{}
		}()
		var wg sync.WaitGroup
		for i := 0; i < noOfResultWorkers; i++ {
			wg.Add(1)
			go func() {
				defer func() {
					wg.Done()
				}()
				for result := range resultsStream {
					jobsDonePipeline.Add(result.organizationID, jobs.CompletingJobDtoV1{
						Error:  result.error,
						ID:     result.id,
						Output: result.output,
					})
				}
			}()
		}
		wg.Wait()
	}()
	return done
}

func Init() {
	commandUtils.Init()
	loggers.Init()
	jobs.RegisterGobDataTypes()
}

func GetRuntimeEnvironment() (cpu_architecture_enums.Type, os_enums.Type) {
	goarch := runtime.GOARCH
	archEnum := cpu_architecture_enums.AMD
	if strings.HasPrefix(goarch, "arm") {
		archEnum = cpu_architecture_enums.ARM
	}
	goos := runtime.GOOS
	osType := os_enums.LINUX
	if strings.HasPrefix(goos, "windows") {
		osType = os_enums.WINDOWS
	}
	return archEnum, osType
}

func GetAndRunJobs(c *client.RunnerClient, mode runner_enums.Mode, globalOrganizationIdFromEnv string) {
	shutdownSignal := make(chan struct{})
	goShutdownHook.ADD(func() {
		log.Println("Waiting for pending deployment jobs to complete......Please wait.")
		shutdownSignal <- struct{}{}
		close(shutdownSignal)
	})
	shutdown := false
	jobsDonePipeline, _ := goPipeline.NewPipeline(10, time.Second*10, func(organizationId string, completingJobs []jobs.CompletingJobDtoV1) {
		e := true
		for e {
			err := c.MarkJobsComplete(completingJobs, organizationId)
			//TODO we can handle for ErrConnection
			if err != nil {
				fmt.Println(err)
				time.Sleep(2 * time.Second)
				continue
			}
			e = false
		}
		if mode == runner_enums.LOCAL {
			for _, completingJob := range completingJobs {
				if len(completingJob.Error) > 0 {
					log.Println("Error executing deployment job: ", completingJob.Error)
				}
			}
		}
	})
	executePendingJobsConcurrentPipeline, _ := goConcurrentPipeline.NewConcurrentPipeline(3, 20,
		1*time.Second, func(s string, pendingJobs []pendingJobType) {
			jobsStream := allocateJobs(pendingJobs)
			resultsStream := executeJobs(jobsStream, 5, mode, globalOrganizationIdFromEnv, c)
			<-sendJobResults(resultsStream, 5, jobsDonePipeline)
		})

	printPendingJobsMessage := true
	for !shutdown {
		select {
		case <-shutdownSignal:
			shutdown = true
		default:
			if mode == runner_enums.Saas {
				pendingJobsForSaas, err := c.GetPendingJobsForSaas(globalOrganizationIdFromEnv)
				if len(pendingJobsForSaas) == 0 {
					if printPendingJobsMessage {
						log.Println("Waiting for new deployment jobs. You can create them at https://app.deployment.io ......")
						printPendingJobsMessage = false
					}
					if err != nil {
						time.Sleep(10 * time.Second)
						continue
					}
				} else {
					for _, pendingJob := range pendingJobsForSaas {
						executePendingJobsConcurrentPipeline.Add("executeJob", pendingJobType{
							jobID:          pendingJob.JobID,
							organizationID: pendingJob.OrganizationID,
							commandEnums:   pendingJob.CommandEnums,
							parameters:     pendingJob.Parameters,
						})
					}
				}
			} else {
				pendingJobs, err := c.GetPendingJobs(globalOrganizationIdFromEnv)
				if len(pendingJobs) == 0 {
					if printPendingJobsMessage {
						log.Println("Waiting for new deployment jobs. You can create them at https://app.deployment.io ......")
						printPendingJobsMessage = false
					}
					if err != nil {
						time.Sleep(10 * time.Second)
						continue
					}
				} else {
					for _, pendingJob := range pendingJobs {
						executePendingJobsConcurrentPipeline.Add("executeJob", pendingJobType{
							jobID:          pendingJob.JobID,
							organizationID: globalOrganizationIdFromEnv,
							commandEnums:   pendingJob.CommandEnums,
							parameters:     pendingJob.Parameters,
						})
					}
				}
			}
			time.Sleep(10 * time.Second)
		}
	}
	executePendingJobsConcurrentPipeline.Shutdown()
	commandUtils.Shutdown()
	loggers.Shutdown()
	jobsDonePipeline.Shutdown()
	goShutdownHook.Wait()
	log.Println("No pending deployment jobs left - exiting now.")
}
