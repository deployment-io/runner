package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/ping"
)

func (r *RunnerClient) GetPendingJobs() ([]jobs.JobDtoV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := jobs.ArgsV1{A: "", P: "password"}
	var jobsDto jobs.DtoV1
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
