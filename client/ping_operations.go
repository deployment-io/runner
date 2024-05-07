package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/ping"
	"runtime"
)

func (r *RunnerClient) GetComputedOrganizationID() string {
	if len(r.userID) > 0 {
		return fmt.Sprintf("du_%s", r.userID)
	} else {
		return r.organizationID
	}
}

func (r *RunnerClient) Ping(firstPing bool) error {
	args := ping.ArgsV2{}
	args.Send = "ping"
	args.FirstPing = firstPing
	args.GoArch = runtime.GOARCH
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.GoOS = runtime.GOOS
	args.RunnerRegion = r.runnerRegion
	args.CloudAccountID = r.cloudAccountID
	args.DockerImage = r.currentDockerImage
	var reply ping.ReplyV1
	err := r.c.Call("Ping.SendV2", args, &reply)
	if err != nil {
		return err
	}
	if reply.Send != "pong" {
		return fmt.Errorf("error receiving pong from the server")
	}

	return nil
}
