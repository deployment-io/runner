package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/logs"
)

func (r *RunnerClient) AddBuildLogs(addBuildLogs []logs.AddBuildLogDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := logs.AddBuildLogsArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.BuildLogs = addBuildLogs
	var reply logs.AddBuildLogsReplyV1
	err := r.c.Call("Logs.AddForBuildV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
