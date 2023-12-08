package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/logs"
)

func (r *RunnerClient) AddJobLogs(addBuildLogs []logs.AddJobLogDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := logs.AddJobLogsArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.JobLogs = addBuildLogs
	var reply logs.AddJobLogsReplyV1
	err := r.c.Call("Logs.AddForJobV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
