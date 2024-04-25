package utils

import (
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
)

type RunnerDataType struct {
	//sync.Mutex
	RunnerRegion string
	AWSAccountID string
	CpuArchEnum  cpu_architecture_enums.Type
	OsType       os_enums.Type
	Mode         runner_enums.Mode
	TargetCloud  runner_enums.TargetCloud
}

func (r *RunnerDataType) Set(runnerRegion, awsAccountID string, cpuArchEnum cpu_architecture_enums.Type,
	osType os_enums.Type, mode runner_enums.Mode, cloud runner_enums.TargetCloud) {
	//r.Lock()
	//defer r.Unlock()
	r.RunnerRegion = runnerRegion
	r.AWSAccountID = awsAccountID
	r.CpuArchEnum = cpuArchEnum
	r.OsType = osType
	r.Mode = mode
	r.TargetCloud = cloud
}

func (r *RunnerDataType) Get() RunnerDataType {
	//r.Lock()
	//defer r.Unlock()
	return *r
}

var RunnerData = RunnerDataType{}
