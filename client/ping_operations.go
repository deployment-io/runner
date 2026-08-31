package client

import (
	"fmt"
	"runtime"

	"github.com/deployment-io/deployment-runner-kit/ping"
	"github.com/deployment-io/deployment-runner/utils/hostinfo"
)

func (r *RunnerClient) GetComputedOrganizationID(organizationID string) string {
	if len(r.userID) > 0 {
		return fmt.Sprintf("du_%s", r.userID)
	} else {
		return organizationID
	}
}

func (r *RunnerClient) Ping(firstPing bool, organizationID string) error {
	args := ping.ArgsV2{}
	args.Send = "ping"
	args.FirstPing = firstPing
	args.GoArch = runtime.GOARCH
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.GoOS = runtime.GOOS
	args.RunnerRegion = r.runnerRegion
	args.CloudAccountID = r.cloudAccountID
	args.DockerImage = r.currentDockerImage
	// Host specs of the EC2 instance this runner is on. Only the runner
	// can report these -- the aws-controller is a Fargate task with no
	// view of the EC2 host -- and the runner scales to zero between jobs,
	// so these ride the ping that happens when it boots.
	//
	// InstanceType may block for up to the IMDS timeout on its first call
	// only; it is cached thereafter and returns empty rather than erroring
	// off EC2.
	args.HostMemoryBytes = hostinfo.MemoryBytes()
	args.HostCPUCores = hostinfo.CPUCores()
	args.InstanceType = hostinfo.InstanceType()
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
