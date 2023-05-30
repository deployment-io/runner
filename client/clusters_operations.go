package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/clusters"
)

func (r *RunnerClient) UpsertClusters(upsertClusters []clusters.UpsertClusterDtoV1) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := clusters.UpsertClustersArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.Clusters = upsertClusters
	var reply clusters.UpsertClustersReplyV1
	err := r.c.Call("Clusters.UpsertV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}
