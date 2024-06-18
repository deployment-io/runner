package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"runtime"
)

func (r *RunnerClient) GetPendingJobs() ([]jobs.PendingJobDtoV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := jobs.PendingJobsArgsV2{}
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.CloudAccountID = r.cloudAccountID
	args.RunnerRegion = r.runnerRegion
	args.GoArch = runtime.GOARCH
	args.GoOS = runtime.GOOS
	var jobsDto jobs.PendingJobsDtoV1
	err := r.c.Call("Jobs.GetPendingV2", args, &jobsDto)
	if err != nil {
		return nil, err
	}
	return jobsDto.Jobs, nil
}

func (r *RunnerClient) MarkJobsComplete(completingJobs []jobs.CompletingJobDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := jobs.CompletingJobsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID()
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

func (r *RunnerClient) UpsertJobHeartbeat(jobID string) (bool, error) {
	if !r.isConnected {
		return false, ErrConnection
	}
	args := jobs.UpsertJobHeartbeatArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.JobID = jobID
	var reply jobs.UpsertJobHeartbeatReplyV1
	err := r.c.Call("Jobs.UpsertHeartbeatV1", args, &reply)
	if err != nil {
		return false, err
	}
	return reply.Stopping, nil
}

func (r *RunnerClient) UpdateJobOutputs(jobOutputs []jobs.UpdateJobOutputDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := jobs.UpdateJobOutputArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.Jobs = jobOutputs
	var reply jobs.UpdateJobOutputReplyV1
	err := r.c.Call("Jobs.UpdateOutputV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
