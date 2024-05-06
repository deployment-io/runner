package loggers

import (
	"bufio"
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
	"io"
	"log"
	"strings"
	"time"
)

type JobLog struct {
	Logger       jobs.Logger
	Message      string
	ErrorMessage string
	Ts           int64
}

var AddJobLogsPipeline *goPipeline.Pipeline[string, JobLog]

func Init() {
	c := client.Get()
	AddJobLogsPipeline, _ = goPipeline.NewPipeline(20, 5*time.Second,
		func(jobId string, jobLogs []JobLog) {
			var logger jobs.Logger
			if len(jobLogs) > 0 {
				logger = jobLogs[0].Logger
			}
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

func GetJobLogsWriter(jobId string, logger jobs.Logger, mode runner_enums.Mode) (*io.PipeWriter, error) {
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
				AddJobLogsPipeline.Add(jobId, JobLog{
					Logger:  logger,
					Message: s,
					Ts:      time.Now().Unix(),
				})
				if mode == runner_enums.LOCAL {
					log.Println(s)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			AddJobLogsPipeline.Add(jobId, JobLog{
				Logger:       logger,
				ErrorMessage: err.Error(),
				Ts:           time.Now().Unix(),
			})
			if mode == runner_enums.LOCAL {
				log.Println(err.Error())
			}
		}
	}()
	return writer, nil
}

func Shutdown() {
	AddJobLogsPipeline.Shutdown()
}
