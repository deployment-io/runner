package loggers

import (
	"bytes"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/loggers_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers/cloudwatch"
)

func Get(parameters map[parameters_enums.Key]interface{}) (jobs.Logger, error) {
	loggerType, ok := parameters[parameters_enums.LoggerType]
	if !ok {
		return nil, fmt.Errorf("logger type is missing in parameters")
	}
	if lt, ok := loggerType.(int64); ok {
		switch uint(lt) {
		case uint(loggers_enums.Cloudwatch):
			return cloudwatch.New(parameters)
		}
		return nil, fmt.Errorf("invalid logger type")
	} else {
		return nil, fmt.Errorf("invalid logger type")
	}
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
