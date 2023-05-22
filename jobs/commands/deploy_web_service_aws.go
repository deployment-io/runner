package commands

import (
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

type DeployWebServiceAWS struct {
}

/**
New deployment
1.
*/

func (d *DeployWebServiceAWS) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {
	return parameters, nil
}
