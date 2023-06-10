package utils

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	jobTypes "github.com/deployment-io/deployment-runner-kit/jobs"
)

func GetLogGroupName(parameters map[string]interface{}) (string, error) {
	//<organizationId>/<deploymentId>
	organizationID, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}

	deploymentID, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s/%s", organizationID, deploymentID), nil
}

func GetBuildLogStreamName(parameters map[string]interface{}) (string, error) {
	//build/<buildId>
	buildIDString, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s/%s", "build", buildIDString), err
}

func GetApplicationLogStreamPrefix(parameters map[string]interface{}) (string, error) {
	//application/<buildId>
	buildIDString, err := jobTypes.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s/%s", "application", buildIDString), err
}
