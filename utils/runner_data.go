package utils

import (
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
)

type RunnerDataType struct {
	//sync.Mutex
	RunnerRegion string
	AWSAccountID string
	CpuArchEnum  cpu_architecture_enums.Type
	OsType       os_enums.Type
}

func (r *RunnerDataType) Set(runnerRegion, awsAccountID string, cpuArchEnum cpu_architecture_enums.Type, osType os_enums.Type) {
	//r.Lock()
	//defer r.Unlock()
	r.RunnerRegion = runnerRegion
	r.AWSAccountID = awsAccountID
	r.CpuArchEnum = cpuArchEnum
	r.OsType = osType
}

func (r *RunnerDataType) Get() (runnerRegion, awsAccountID string, cpuArchEnum cpu_architecture_enums.Type, osType os_enums.Type) {
	//r.Lock()
	//defer r.Unlock()
	runnerRegion = r.RunnerRegion
	awsAccountID = r.AWSAccountID
	cpuArchEnum = r.CpuArchEnum
	osType = r.OsType
	return
}

var RunnerData = RunnerDataType{}
