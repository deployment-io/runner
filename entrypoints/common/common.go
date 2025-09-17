package common

import (
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
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

func getJobResult(job pendingJobType, error string) completingJobType {
	return completingJobType{
		error:          error,
		id:             job.jobID,
		organizationID: job.organizationID,
	}
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
							result := getJobResult(pendingJob, err.Error())
							resultsStream <- result
							loggers.AddJobLogsPipeline.Add(pendingJob.jobID, loggers.JobLog{
								Logger:         nil,
								Message:        fmt.Sprintf("Error in executing - %s - %s\n", pendingJob.jobID, err.Error()),
								Ts:             time.Now().Unix(),
								OrganizationID: pendingJob.organizationID,
							})
							//if job is a build type it will be marked done
							<-commands.MarkDeploymentDone(parameters, err)
							return
						}
						logsWriter, err := loggers.GetJobLogsWriter(pendingJob.jobID, pendingJob.organizationID, logger, mode)
						if err != nil {
							result := getJobResult(pendingJob, err.Error())
							resultsStream <- result
							return
						}
						defer logsWriter.Close()
						jobDoneSignal := make(chan struct{})
						defer close(jobDoneSignal)
						stopJobSignal := getJobStopSignal(pendingJob, jobDoneSignal, c)
						for _, commandEnum := range pendingJob.commandEnums {
							select {
							case <-stopJobSignal:
								errStoppedByUser := types.ErrJobStoppedByUser
								handleLogEnd(errStoppedByUser, pendingJob.jobID, logsWriter)
								result := getJobResult(pendingJob, errStoppedByUser.Error())
								resultsStream <- result
								//if job is a deployment/build/preview type, this will be marked them done
								<-commands.MarkDeploymentDone(parameters, errStoppedByUser)
								return
							default:
								command, err := commands.Get(commandEnum)
								if err != nil {
									handleLogEnd(err, pendingJob.jobID, logsWriter)
									result := getJobResult(pendingJob, err.Error())
									resultsStream <- result
									return
								}
								parameters, err = command.Run(parameters, logsWriter)
								if err != nil {
									handleLogEnd(err, pendingJob.jobID, logsWriter)
									result := getJobResult(pendingJob, err.Error())
									resultsStream <- result
									return
								}
							}
						}
						handleLogEnd(nil, pendingJob.jobID, logsWriter)
						result := getJobResult(pendingJob, "")
						resultsStream <- result
					}(pendingJob)
				}
			}()
		}
		wg.Wait()
	}()
	return resultsStream
}

func getJobStopSignal(job pendingJobType, jobDoneSignal <-chan struct{}, c *client.RunnerClient) <-chan struct{} {
	jobStopSignal := make(chan struct{})
	go func() {
		defer close(jobStopSignal)
		for {
			select {
			case <-jobDoneSignal:
				return
			default:
				//ignoring error in client
				isStopping, _ := c.UpsertJobHeartbeat(job.jobID, job.organizationID)
				if isStopping {
					jobStopSignal <- struct{}{}
					return
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
						Error: result.error,
						ID:    result.id,
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
