package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/clusters"
)

func (r *RunnerClient) UpsertClusters(upsertClusters []clusters.UpsertClusterDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := clusters.UpsertClustersArgsV2{}
	args.OrganizationID = r.GetComputedOrganizationID()
	args.Token = r.token
	args.CloudAccountID = r.cloudAccountID
	args.Clusters = upsertClusters
	var reply clusters.UpsertClustersReplyV1
	err := r.c.Call("Clusters.UpsertV2", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
