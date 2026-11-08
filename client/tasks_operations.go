package client

import (
	"fmt"
	"net/rpc"
	"time"

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

// MarkTaskStepStage reports which stage of a Step Job is running right now —
// Implement while the agent edits, Review while the Review stage examines what
// it produced, Implement again when a must-fix finding routes back.
//
// A sibling of UpdateTasks on the same Jobs receiver, and deliberately NOT a
// heartbeat field: a stage transition is an event with a definite order, and
// folding it into the ~5s heartbeat would make "Review started" arrive
// whenever the next beat happened to fire.
//
// Sent one at a time rather than through a batching pipeline. There are at
// most a handful per Job, and the value of the report is that it lands
// promptly — a Step card that says Review ten seconds after the round began
// has already told the user what they wanted to know; one that says it a
// batch-interval later has not.
func (r *RunnerClient) MarkTaskStepStage(organizationID string, update tasks.UpdateTaskStepStageDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := tasks.UpdateTaskStepStageArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Update = update
	var reply tasks.UpdateTaskStepStageReplyV1
	if err := r.c.Call("Jobs.MarkTaskStepStageV1", args, &reply); err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}

// UpdateTaskStepReview sends the Review stage's final result for one Step, to
// be bounded and stored on the Step.
//
// The FINAL round's result, not every round's: the Step carries the review the
// work on the branch was judged by, and the intermediate rounds live in the
// Job's output where the full history belongs.
func (r *RunnerClient) UpdateTaskStepReview(organizationID string, update tasks.UpdateTaskStepReviewDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := tasks.UpdateTaskStepReviewArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Update = update
	var reply tasks.UpdateTaskStepReviewReplyV1
	if err := r.c.Call("Jobs.UpdateTaskStepReviewV1", args, &reply); err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}

// UpdateTaskStepQA sends the QA stage's final result for one Step, to be
// bounded and stored on the Step. Like UpdateTaskStepReview.
func (r *RunnerClient) UpdateTaskStepQA(organizationID string, update tasks.UpdateTaskStepQADtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := tasks.UpdateTaskStepQAArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Update = update
	var reply tasks.UpdateTaskStepQAReplyV1
	if err := r.c.Call("Jobs.UpdateTaskStepQAV1", args, &reply); err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}

// appendTaskEventsTimeout bounds one AppendTaskEvents call. A var so tests can
// shorten it.
var appendTaskEventsTimeout = 30 * time.Second

// AppendTaskEvents sends one batch of a Task run's structured progress events
// (Jobs.AppendTaskEventsV1), to be bounded, numbered and stored on the Job.
// The batching, ordering and retries are the caller's — see
// commandUtils.TaskEvents; this is the one call.
//
// Unlike the other calls it gives up after appendTaskEventsTimeout: a stalled
// events handler must not hold the sender (and its batch) for the rest of the
// runner's life. net/rpc can't cancel a call, so the reply, if it ever comes,
// lands in a buffered channel nobody reads.
func (r *RunnerClient) AppendTaskEvents(organizationID string, args tasks.AppendTaskEventsArgsV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	var reply tasks.AppendTaskEventsReplyV1
	call := r.c.Go("Jobs.AppendTaskEventsV1", args, &reply, make(chan *rpc.Call, 1))
	timer := time.NewTimer(appendTaskEventsTimeout)
	defer timer.Stop()
	select {
	case <-call.Done:
	case <-timer.C:
		return fmt.Errorf("deployment.io didn't answer within %s", appendTaskEventsTimeout)
	}
	if call.Error != nil {
		return call.Error
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
