package common

import "github.com/deployment-io/deployment-runner-kit/enums/commands_enums"

type pendingJobType struct {
	jobID          string
	organizationID string
	commandEnums   []commands_enums.Type
	parameters     map[string]interface{}
}

type completingJobType struct {
	error          string
	id             string
	organizationID string
}
