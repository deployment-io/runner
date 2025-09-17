package client

import (
	"fmt"

	"github.com/deployment-io/deployment-runner-kit/agents"
)

func (r *RunnerClient) UpdateAgentResponses(responses []agents.UpdateResponseDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := agents.UpdateResponseArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Responses = responses
	var reply agents.UpdateResponseReplyV1
	err := r.c.Call("Agents.UpdateResponseV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
