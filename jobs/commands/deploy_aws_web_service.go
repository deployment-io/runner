package commands

import (
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

type DeployAwsWebService struct {
}

/**
New deployment
1.
*/

func (d *DeployAwsWebService) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {
	return parameters, nil
}
