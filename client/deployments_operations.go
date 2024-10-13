package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/deployments"
)

func (r *RunnerClient) UpdateDeployments(updateDeployments []deployments.UpdateDeploymentDtoV1, organizationID string) error {
	if !r.isConnected {
		return ErrConnection
	}
	args := deployments.UpdateDeploymentsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.Deployments = updateDeployments
	var reply deployments.UpdateDeploymentsReplyV1
	err := r.c.Call("Deployments.UpdateV1", args, &reply)
	if err != nil {
		return err
	}
	if !reply.Done {
		return fmt.Errorf("error receiving done from the server")
	}
	return nil
}

func (r *RunnerClient) GetDeploymentData(deploymentIDs []string, organizationID string) ([]deployments.GetDeploymentDtoV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := deployments.GetDeploymentsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.IDs = deploymentIDs
	var reply deployments.GetDeploymentsReplyV1
	err := r.c.Call("Deployments.GetV1", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply.Deployments, nil
}
