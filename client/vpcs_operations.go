package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/vpcs"
)

func (r *RunnerClient) UpsertVpcs(upsertVpcs []vpcs.UpsertVpcDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := vpcs.UpsertVpcsArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.Vpcs = upsertVpcs
	var reply vpcs.UpsertVpcsReplyV1
	err := r.c.Call("Vpcs.UpsertV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
