package client

import (
	"fmt"

	"github.com/deployment-io/deployment-runner-kit/sessions"
)

// UpdateSessionMessages streams a batch of assistant-message deltas for an
// interactive Assistant session to deployment-server, which appends them to the
// session thread's message_stream (driving the SSE the browser tails).
func (r *RunnerClient) UpdateSessionMessages(messages []sessions.AppendMessageDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := sessions.AppendMessageArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Messages = messages
	var reply sessions.AppendMessageReplyV1
	err := r.c.Call("Sessions.AppendMessageV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}

// GetSessionInput pulls the session thread's user turns newer than afterTs so
// the runner can feed them to the live agent. Returns oldest-first.
func (r *RunnerClient) GetSessionInput(jobID string, afterTs int64, organizationID string) ([]sessions.UserMessageDtoV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := sessions.GetInputArgsV1{
		JobID:   jobID,
		AfterTs: afterTs,
	}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	var reply sessions.GetInputReplyV1
	err := r.c.Call("Sessions.GetInputV1", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply.Messages, nil
}

// UpdateSessionSpec forwards the latest structured task-spec the planning agent
// emitted to deployment-server, which persists it to Session.StructuredSpec.
func (r *RunnerClient) UpdateSessionSpec(spec sessions.UpdateSpecDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := sessions.UpdateSpecArgsV1{Spec: spec}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	var reply sessions.UpdateSpecReplyV1
	err := r.c.Call("Sessions.UpdateSpecV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
