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
