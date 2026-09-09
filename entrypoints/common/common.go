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
	"github.com/deployment-io/deployment-runner/jobs/resources"
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

// releaseJobsPipeline batches jobs the runner accepted but could not
// start, and hands them back to the server keyed by organization.
//
// A pipeline rather than a direct call, matching jobsDonePipeline and
// loggers.AddJobLogsPipeline, for two reasons:
//
//   - Batching. ReleasingJobsArgsV1.JobIDs is a slice precisely so one
//     RPC can return many jobs; releasing them one at a time made N calls
//     per poll where one would do.
//   - It keeps the retry OFF the job worker. Retrying inline slept inside
//     one of the runner's 15 workers, so a refused job tied up a worker
//     doing nothing for the length of the backoff.
//
// The retry is UNBOUNDED, matching MarkJobsComplete, and that is not
// belt-and-braces: a job whose release never lands is stranded forever.
// It stays Running, and the server's stuck-job cron cannot rescue it —
// that cron finds work through running_jobs heartbeat records, and a
// requeued job returns before the heartbeat goroutine ever starts, so no
// record exists. Nothing else reaps a Running job. Giving up here would
// leave a job invisible, never retried and never failed.
var releaseJobsPipeline *goPipeline.Pipeline[string, string]

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
						// First in the function on purpose, and load-bearing in
						// two directions — do not move it below getJobStopSignal.
						//
						// That call starts the heartbeat, and a heartbeat creates
						// a running_jobs record. Those records are the ONLY thing
						// the server's stuck-job cron looks at (FindTimedOut
						// queries heartBeatTs), and MarkManyRunningTimedOut
						// matches Pending as well as Running. So:
						//
						//   - Refusing BEFORE the heartbeat means a requeued job
						//     has no record, and cannot be marked TimedOut while
						//     it sits legitimately in the pending queue. Refuse
						//     after it, and any job blocked for more than five
						//     minutes gets failed — exactly what requeuing exists
						//     to prevent.
						//   - Because there is no record, nothing reaps a job
						//     whose release never lands either, which is why
						//     releaseJobsPipeline retries forever rather than
						//     giving up.
						//
						// A requeued job must also leave no other trace: no
						// logger, no job log stream, and above all no log lines. The
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
						if requiredMemory := resources.JobMemoryBytes(pendingJob.commandEnums); requiredMemory > 0 {
							releaseMemory, admitted := resources.TryAcquireMemory(requiredMemory)
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
									requiredMemory/(1024*1024), resources.MemoryBudgetBytes()/(1024*1024),
									pendingJob.jobID)
								// Handed to the pipeline rather than sent
								// here, so releases batch per org and the
								// retry runs off this worker.
								//
								// Add is an UNBUFFERED send, and the pipeline
								// invokes its handler synchronously, so this
								// frees the worker only while the pipeline is
								// healthy. If the control plane is unreachable
								// the handler sits in its retry loop, stops
								// reading, and this call blocks.
								//
								// That is back-pressure, and it is deliberate.
								// While disconnected GetPendingJobs fails too,
								// so no new work enters; a freed worker would
								// only pick up another job and possibly start a
								// container whose completion cannot be
								// reported, leaving the cron to time it out.
								// Blocking stops the runner accumulating work
								// it cannot report. It is a stall, not a
								// deadlock — everything drains on reconnect
								// with nothing lost.
								//
								// It also matches what the runner already does:
								// jobsDonePipeline retries forever the same
								// way, so a disconnect already wedges the
								// result workers on Add and, through the
								// unbuffered resultsStream, the job workers
								// behind them. This path is consistent with
								// that rather than a new behaviour.
								releaseJobsPipeline.Add(pendingJob.organizationID, pendingJob.jobID)
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
	releaseJobsPipeline, _ = goPipeline.NewPipeline(10, time.Second*10, func(organizationId string, jobIDs []string) {
		// Retry until it lands. See the note on releaseJobsPipeline: a
		// release that never succeeds strands the job in Running with
		// nothing to reap it, so giving up is worse than retrying forever.
		for {
			err := c.ReleaseJobs(jobIDs, organizationId, "runner at memory capacity")
			if err == nil {
				return
			}
			log.Printf("error returning %d job(s) to the pending queue, retrying: %s", len(jobIDs), err)
			time.Sleep(2 * time.Second)
		}
	})

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
	// After jobsDonePipeline: both drain to the same server, and a release
	// left buffered at exit strands its job in Running with nothing to
	// reap it.
	releaseJobsPipeline.Shutdown()
	goShutdownHook.Wait()
	log.Println("No pending deployment jobs left - exiting now.")
}
