package loggers

import (
	"bufio"
	"bytes"
	"fmt"
	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/deployment-io/deployment-runner-kit/enums/loggers_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/logs"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers/cloudwatch"
	"io"
	"strings"
	"time"
)

type JobLog struct {
	logger       jobs.Logger
	message      string
	errorMessage string
	ts           int64
}

var addJobLogsPipeline *goPipeline.Pipeline[string, JobLog]

func Init() {
	c := client.Get()
	addJobLogsPipeline, _ = goPipeline.NewPipeline(20, 5*time.Second,
		func(jobId string, jobLogs []JobLog) {
			var logger jobs.Logger
			if len(jobLogs) > 0 {
				logger = jobLogs[0].logger
			}
			var messages []string
			var jobLogsDto []logs.AddJobLogDtoV1
			for _, jobLog := range jobLogs {
				message := jobLog.message
				if len(jobLog.errorMessage) > 0 {
					message = jobLog.errorMessage
				}
				messages = append(messages, message)
				jobLogsDto = append(jobLogsDto, logs.AddJobLogDtoV1{
					ID:           jobId,
					Message:      jobLog.message,
					ErrorMessage: jobLog.errorMessage,
					Ts:           jobLog.ts,
				})
			}
			if logger != nil {
				err := logger.Log(messages)
				if err != nil {
					fmt.Println(err)
				}
			}

			e := true
			for e {
				err := c.AddJobLogs(jobLogsDto)
				//TODO we can handle for ErrConnection
				//will block till error
				if err != nil {
					fmt.Println(err)
					time.Sleep(2 * time.Second)
					continue
				}
				e = false
			}
		})
	goShutdownHook.ADD(func() {
		fmt.Println("waiting for logs add pipeline shutdown")
		addJobLogsPipeline.Shutdown()
		fmt.Println("waiting for logs add pipeline shutdown -- done")
	})

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

func GetJobLogsWriter(jobId string, logger jobs.Logger) (*io.PipeWriter, error) {
	reader, writer := io.Pipe()
	go func() {
		defer reader.Close()
		//read from pipe
		scanner := bufio.NewScanner(reader)
		scanner.Split(utils.ScanCRLF)
		for scanner.Scan() {
			s := scanner.Text()
			s = strings.Trim(s, " \n \r")
			if len(s) > 0 {
				addJobLogsPipeline.Add(jobId, JobLog{
					logger:  logger,
					message: s,
					ts:      time.Now().Unix(),
				})
			}
		}
		if err := scanner.Err(); err != nil {
			addJobLogsPipeline.Add(jobId, JobLog{
				logger:       logger,
				errorMessage: err.Error(),
				ts:           time.Now().Unix(),
			})
		}
	}()
	return writer, nil
}
