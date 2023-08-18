package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/ping"
	"runtime"
)

func (r *RunnerClient) Ping(firstPing bool) (string, int64, int64, error) {
	args := ping.ArgsV1{}
	args.Send = "ping"
	args.FirstPing = firstPing
	args.GoArch = runtime.GOARCH
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.DockerImage = r.currentDockerImage
	var reply ping.ReplyV1
	err := r.c.Call("Ping.SendV1", args, &reply)
	if err != nil {
		return "", 0, 0, err
	}
	if reply.Send != "pong" {
		return "", 0, 0, fmt.Errorf("error receiving pong from the server")
	}

	return reply.DockerUpgradeImage, reply.UpgradeFromTs, reply.UpgradeToTs, nil
}
