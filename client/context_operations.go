package client

import (
	"github.com/deployment-io/deployment-runner-kit/context_pack"
)

// MaterializeContext asks deployment-server for the org's stored context for the given scopes,
// rendered to files the runner writes under /work/context. The caller treats any error as "no
// context available" and degrades gracefully — the agent falls back to live discovery.
func (r *RunnerClient) MaterializeContext(organizationID string, scopes []context_pack.Scope) ([]context_pack.ContextFileV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := context_pack.MaterializeContextArgsV1{Scopes: scopes}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	var reply context_pack.MaterializeContextReplyV1
	err := r.c.Call("ContextPacks.MaterializeV1", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply.Files, nil
}
