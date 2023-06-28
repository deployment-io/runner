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

type BuildLog struct {
	logger       jobs.Logger
	message      string
	errorMessage string
	ts           int64
}

var addBuildLogsPipeline *goPipeline.Pipeline[string, BuildLog]

func Init() {
	c := client.Get()
	addBuildLogsPipeline, _ = goPipeline.NewPipeline(20, 10*time.Second,
		func(buildId string, buildLogs []BuildLog) {
			var logger jobs.Logger
			if len(buildLogs) > 0 {
				logger = buildLogs[0].logger
			}
			var messages []string
			var buildLogsDto []logs.AddBuildLogDtoV1
			for _, buildLog := range buildLogs {
				message := buildLog.message
				if len(buildLog.errorMessage) > 0 {
					message = buildLog.errorMessage
				}
				messages = append(messages, message)
				buildLogsDto = append(buildLogsDto, logs.AddBuildLogDtoV1{
					ID:           buildId,
					Message:      buildLog.message,
					ErrorMessage: buildLog.errorMessage,
					Ts:           buildLog.ts,
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
				err := c.AddBuildLogs(buildLogsDto)
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
		addBuildLogsPipeline.Shutdown()
		fmt.Println("waiting for logs add pipeline shutdown -- done")
	})

}

func Get(parameters map[string]interface{}) (jobs.Logger, error) {
	loggerType, err := jobs.GetParameterValue[int64](parameters, parameters_enums.LoggerType)
	if err != nil {
		return nil, err
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

func GetBuildLogsWriter(parameters map[string]interface{}, logger jobs.Logger) (*io.PipeWriter, error) {
	buildId, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return nil, err
	}
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
				addBuildLogsPipeline.Add(buildId, BuildLog{
					logger:  logger,
					message: s,
					ts:      time.Now().Unix(),
				})
			}
		}
		if err := scanner.Err(); err != nil {
			addBuildLogsPipeline.Add(buildId, BuildLog{
				logger:       logger,
				errorMessage: err.Error(),
				ts:           time.Now().Unix(),
			})
		}
	}()
	return writer, nil
}
