package utils

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"strings"
)

func GetDockerBuildArgs(parameters map[string]interface{}) (map[string]*string, error) {
	dockerBuildArgs, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.DockerBuildArgs)
	if err != nil || len(dockerBuildArgs) == 0 {
		//no docker build args
		return nil, nil
	}
	var dockerBuildArgsMap = make(map[string]*string)
	dockerBuildArgsStrings, err := ConvertPrimitiveAToStringSlice(dockerBuildArgs)
	if err != nil {
		return nil, fmt.Errorf("error getting docker build args: %s", err)
	}
	for _, dockerBuildArg := range dockerBuildArgsStrings {
		entry := strings.Split(dockerBuildArg, "=")
		if len(entry) == 2 {
			dockerBuildArgsMap[entry[0]] = &entry[1]
		}
	}
	return dockerBuildArgsMap, nil
}
