package main

import (
	"context"
	"fmt"
	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/jobs/commands"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/joho/godotenv"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

func allocateJobs(pendingJobs []jobs.PendingJobDtoV1) <-chan jobs.PendingJobDtoV1 {
	jobsStream := make(chan jobs.PendingJobDtoV1)
	go func() {
		defer close(jobsStream)
		for _, job := range pendingJobs {
			jobsStream <- job
		}
	}()
	return jobsStream
}

func getJobResult(job jobs.PendingJobDtoV1, error string) jobs.CompletingJobDtoV1 {
	return jobs.CompletingJobDtoV1{
		Error: error,
		ID:    job.JobID,
	}
}

func handleLogEnd(err error, jobID string, logsWriter io.Writer) {
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Error in executing - %s - %s\n", jobID, err.Error()))
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Successfully executed - %s\n", jobID))
	}
}

func executeJobs(jobsStream <-chan jobs.PendingJobDtoV1, noOfWorkers int) <-chan jobs.CompletingJobDtoV1 {
	resultsStream := make(chan jobs.CompletingJobDtoV1)
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
					func(pendingJob jobs.PendingJobDtoV1) {
						parameters := pendingJob.Parameters
						//TODO logger is job level detail. Introduce a job context and add logger there
						//pass job context in each command run.
						//job context will be different based on job type. So job types needs to be passed from the server.
						logger, err := loggers.Get(parameters)
						if err != nil {
							result := getJobResult(pendingJob, err.Error())
							resultsStream <- result
							return
						}
						logsWriter, err := loggers.GetJobLogsWriter(pendingJob.JobID, logger)
						if err != nil {
							result := getJobResult(pendingJob, err.Error())
							resultsStream <- result
							return
						}
						defer logsWriter.Close()
						for _, commandEnum := range pendingJob.CommandEnums {
							command, err := commands.Get(commandEnum)
							if err != nil {
								handleLogEnd(err, pendingJob.JobID, logsWriter)
								result := getJobResult(pendingJob, err.Error())
								resultsStream <- result
								//continue to next job in jobsStream
								return
							}
							parameters, err = command.Run(parameters, logsWriter)
							if err != nil {
								handleLogEnd(err, pendingJob.JobID, logsWriter)
								result := getJobResult(pendingJob, err.Error())
								resultsStream <- result
								//continue to next job in jobsStream
								return
							}
						}
						handleLogEnd(nil, pendingJob.JobID, logsWriter)
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

func sendJobResults(resultsStream <-chan jobs.CompletingJobDtoV1,
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
					jobsDonePipeline.Add("done", result)
				}
			}()
		}
		wg.Wait()
	}()
	return done
}

func getEnvironment() (service, organizationId, token, region, dockerImage, memory, taskExecutionRoleArn,
	taskRoleArn, awsAccountID string) {
	//TODO load .env
	//ignoring err
	_ = godotenv.Load()
	organizationId = os.Getenv("OrganizationID")
	service = os.Getenv("Service")
	token = os.Getenv("Token")
	region = os.Getenv("Region")
	dockerImage = os.Getenv("DockerImage")
	memory = os.Getenv("Memory")
	taskExecutionRoleArn = os.Getenv("ExecutionRoleArn")
	taskRoleArn = os.Getenv("TaskRoleArn")
	awsAccountID = os.Getenv("AWSAccountID")
	return
}

var clientCertPem, clientKeyPem string

func main() {
	service, organizationId, token, region, dockerImage, _, _, _, awsAccountID := getEnvironment()
	stsClient, err := commands.GetStsClient(region)
	if err != nil {
		log.Fatal(err)
	}
	if len(awsAccountID) > 0 {
		//aws case - check account validity
		getCallerIdentityOutput, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
		if err != nil {
			log.Fatal(err)
		}
		if awsAccountID != aws.ToString(getCallerIdentityOutput.Account) {
			log.Fatalf("Invalid AWS account ID")
		}
	}
	client.Connect(service, organizationId, token, clientCertPem, clientKeyPem, dockerImage, region, awsAccountID, false)
	c := client.Get()
	commands.Init()
	loggers.Init()
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
	utils.RunnerData.Set(region, awsAccountID, archEnum, osType)
	shutdownSignal := make(chan struct{})
	goShutdownHook.ADD(func() {
		fmt.Println("waiting for shutdown signal")
		shutdownSignal <- struct{}{}
		close(shutdownSignal)
	})
	shutdown := false
	jobsDonePipeline, _ := goPipeline.NewPipeline(10, time.Second*10, func(s string, i []jobs.CompletingJobDtoV1) {
		e := true
		for e {
			err := c.MarkJobsComplete(i)
			//TODO we can handle for ErrConnection
			if err != nil {
				fmt.Println(err)
				time.Sleep(2 * time.Second)
				continue
			}
			e = false
		}
	})
	goShutdownHook.ADD(func() {
		fmt.Println("waiting for jobs done pipeline shutdown")
		jobsDonePipeline.Shutdown()
		fmt.Println("waiting for jobs done pipeline shutdown -- done")
	})
	jobs.RegisterGobDataTypes()
	for !shutdown {
		select {
		case <-shutdownSignal:
			shutdown = true
		default:
			pendingJobs, err := c.GetPendingJobs()
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}
			jobsStream := allocateJobs(pendingJobs)
			resultsStream := executeJobs(jobsStream, 5)
			<-sendJobResults(resultsStream, 5, jobsDonePipeline)
			time.Sleep(10 * time.Second)
		}
	}
	fmt.Println("waiting for shutdown.wait")
	goShutdownHook.Wait()
}
