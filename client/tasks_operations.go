package client

import (
	"fmt"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// UpdateTasks reports a batch of Task Step "started" events to the control
// plane (Jobs.MarkTaskStepRunningV1), which flips each Task -> Running and
// Step -> StepRunning. Mirrors UpdateBuilds.
func (r *RunnerClient) UpdateTasks(updates []tasks.UpdateTaskStepRunningDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := tasks.UpdateTaskStepRunningArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Updates = updates
	var reply tasks.UpdateTaskStepRunningReplyV1
	err := r.c.Call("Jobs.MarkTaskStepRunningV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
