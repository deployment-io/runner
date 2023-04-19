package utils

import (
	"bufio"
	"bytes"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"strings"
)

func ScanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	i := bytes.IndexByte(data, '\n')
	j := bytes.IndexByte(data, '\r')
	if i >= 0 || j >= 0 {
		// We have a full newline-terminated line.
		if i >= 0 {
			return i + 1, data[0:i], nil
		}
		//fmt.Println("data:-" + string(data[0:j]))
		return j + 1, data[0:j], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

func GetLinesFromBuffer(logBuffer *bytes.Buffer) ([]string, error) {
	var messages []string
	scanner := bufio.NewScanner(logBuffer)
	scanner.Split(ScanCRLF)
	for scanner.Scan() {
		s := scanner.Text()
		s = strings.Trim(s, " \n \r")
		messages = append(messages, s)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func LogError(jobContext *jobs.ContextV1) {

}

func GetJobContext(parameters map[parameters_enums.Key]interface{}) *jobs.ContextV1 {
	environmentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentID)
	if err != nil {
		environmentID = ""
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		deploymentID = ""
	}
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		organizationID = ""
	}
	buildID, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		buildID = ""
	}
	return &jobs.ContextV1{
		OrganizationID: organizationID,
		EnvironmentID:  environmentID,
		DeploymentID:   deploymentID,
		BuildID:        buildID,
	}
}
