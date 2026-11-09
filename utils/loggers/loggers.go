package loggers

import (
	"bytes"
	"fmt"
	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/deployment-io/deployment-runner-kit/enums/loggers_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/logs"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers/cloudwatch"
	"log"
	"time"
)

type JobLog struct {
	Logger         jobs.Logger
	Message        string
	ErrorMessage   string
	Ts             int64
	OrganizationID string
	// Stage, Kind and Group are set on a Task Step job's lines; see JobLogsWriter.
	Stage string
	Kind  string
	Group *logs.JobLogGroupV1
}

var AddJobLogsPipeline *goPipeline.Pipeline[string, JobLog]

func Init() {
	c := client.Get()
	AddJobLogsPipeline, _ = goPipeline.NewPipeline(20, 5*time.Second,
		func(jobId string, jobLogs []JobLog) {
			var logger jobs.Logger
			var organizationId string
			if len(jobLogs) > 0 {
				logger = jobLogs[0].Logger
				organizationId = jobLogs[0].OrganizationID
			}
			messages, jobLogsDto := toJobLogDtos(jobId, jobLogs)
			if logger != nil {
				err := logger.Log(messages)
				if err != nil {
					fmt.Println(err)
				}
			}

			e := true
			for e {
				err := c.AddJobLogs(jobLogsDto, organizationId)
				//TODO we can handle for ErrConnection
				//will block till error
				if err != nil {
					log.Println(err)
					time.Sleep(2 * time.Second)
					continue
				}
				e = false
			}
		})
	goShutdownHook.ADD(func() {
		//fmt.Println("waiting for logs add pipeline shutdown")
		//AddJobLogsPipeline.Shutdown()
		//fmt.Println("waiting for logs add pipeline shutdown -- done")
	})

}

// toJobLogDtos builds the messages for the job's Logger and the DTOs for the
// API from one batch of a job's lines.
func toJobLogDtos(jobId string, jobLogs []JobLog) ([]string, []logs.AddJobLogDtoV1) {
	var messages []string
	var jobLogsDto []logs.AddJobLogDtoV1
	for _, jobLog := range jobLogs {
		message := jobLog.Message
		if len(jobLog.ErrorMessage) > 0 {
			message = jobLog.ErrorMessage
		}
		messages = append(messages, message)
		jobLogsDto = append(jobLogsDto, logs.AddJobLogDtoV1{
			ID:           jobId,
			Message:      jobLog.Message,
			ErrorMessage: jobLog.ErrorMessage,
			Ts:           jobLog.Ts,
			Stage:        jobLog.Stage,
			Kind:         jobLog.Kind,
			Group:        jobLog.Group,
		})
	}
	return messages, jobLogsDto
}

func Get(parameters map[string]interface{}) (jobs.Logger, error) {
	loggerType, err := jobs.GetParameterValue[int64](parameters, parameters_enums.LoggerType)
	if err != nil {
		//loggerType is not needed for all job types.
		//TODO We'll revisit this later
		return nil, nil
	}

	switch uint(loggerType) {
	case uint(loggers_enums.Cloudwatch):
		return cloudwatch.New(parameters)
	}
	return nil, fmt.Errorf("invalid logger type")
}

func LogBuffer(logBuffer *bytes.Buffer, logger jobs.Logger) error {
	if logBuffer.Len() == 0 {
		return nil
	}
	messages, err := utils.GetLinesFromBuffer(logBuffer)
	if err != nil {
		return err
	}
	//TODO can run below in a different goroutine
	return logger.Log(messages)
}

// GetJobLogsWriter returns the job's log writer. Each line it completes goes
// to AddJobLogsPipeline; taskStep tags them (see JobLogsWriter).
func GetJobLogsWriter(jobId, organizationId string, logger jobs.Logger, mode runner_enums.Mode, taskStep bool) (*JobLogsWriter, error) {
	w := NewJobLogsWriter(taskStep, func(l JobLog) {
		l.Logger = logger
		l.OrganizationID = organizationId
		AddJobLogsPipeline.Add(jobId, l)
	})
	w.local = mode == runner_enums.LOCAL
	return w, nil
}

func Shutdown() {
	AddJobLogsPipeline.Shutdown()
}
