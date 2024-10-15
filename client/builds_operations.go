package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/builds"
)

func (r *RunnerClient) UpdateBuilds(updateBuilds []builds.UpdateBuildDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := builds.UpdateBuildsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Builds = updateBuilds
	var reply builds.UpdateBuildsReplyV1
	err := r.c.Call("Builds.UpdateV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
