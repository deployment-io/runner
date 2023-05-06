package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/ping"
)

func (r *RunnerClient) GetPendingJobs() ([]jobs.PendingJobDtoV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := jobs.PendingJobsArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	var jobsDto jobs.PendingJobsDtoV1
	err := r.c.Call("Jobs.GetPendingV1", args, &jobsDto)
	if err != nil {
		return nil, err
	}
	return jobsDto.Jobs, nil
}

func (r *RunnerClient) Ping() error {
	args := ping.ArgsV1{Send: "ping"}
	var reply ping.ReplyV1
	err := r.c.Call("Ping.SendV1", args, &reply)
	if err != nil {
		return err
	}
	if reply.Send != "pong" {
		return fmt.Errorf("error receiving pong from the server")
	}
	return nil
}

func (r *RunnerClient) MarkJobsComplete(completingJobs []jobs.CompletingJobDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := jobs.CompletingJobsArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.Jobs = completingJobs
	var reply jobs.CompletingJobsReplyV1
	err := r.c.Call("Jobs.MarkCompleteV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
