package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/ping"
)

func (r *RunnerClient) Ping() error {
	args := ping.ArgsV1{}
	args.Send = "ping"
	args.OrganizationID = r.organizationID
	args.Token = r.token
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
