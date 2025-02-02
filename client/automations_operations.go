package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/automations"
)

func (r *RunnerClient) UpdateAutomationResponses(responses []automations.UpdateResponseDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := automations.UpdateResponseArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Responses = responses
	var reply automations.UpdateResponseReplyV1
	err := r.c.Call("Automations.UpdateResponseV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
