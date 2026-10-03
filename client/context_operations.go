package client

import (
	"net/rpc"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
)

// MaterializeContext asks deployment-server for the org's stored context for the given scopes,
// rendered to files the runner writes under /work/context. refreshRepoCatalog asks the server to
// rebuild the org's repo catalog first (bounded server-side; on timeout or error it uses the stored
// one). The caller treats any error as "no context available" and degrades gracefully — the agent
// falls back to live discovery.
func (r *RunnerClient) MaterializeContext(organizationID string, scopes []context_pack.Scope, refreshRepoCatalog bool) ([]context_pack.ContextFileV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := context_pack.MaterializeContextArgsV1{Scopes: scopes, RefreshRepoCatalog: refreshRepoCatalog}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	var reply context_pack.MaterializeContextReplyV1
	err := r.c.Call("ContextPacks.MaterializeV1", args, &reply)
	if err != nil {
		return nil, err
	}
	return reply.Files, nil
}

// SaveInfraContext sends this runner's fresh infrastructure scan — the []ScopedPack JSON its
// context sources produced — to deployment-server, which stores the Cluster-scope packs. Returns
// how many packs were saved.
//
// The call runs over its own connection whose deadline is timeout from now, closed afterwards: the
// dial, the request write and the reply read all fail once it passes, and a stalled save never
// holds the shared connection (whose net/rpc send lock the following MaterializeContext needs).
func (r *RunnerClient) SaveInfraContext(organizationID string, packsJSON string, timeout time.Duration) (int, error) {
	if !r.isConnected || r.dial == nil {
		return 0, ErrConnection
	}
	deadline := time.Now().Add(timeout)
	conn, err := r.dial(timeout)
	if err != nil {
		return 0, err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return 0, err
	}
	c := rpc.NewClient(conn)
	defer c.Close()

	args := context_pack.SaveInfraContextArgsV1{PacksJSON: packsJSON}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	var reply context_pack.SaveInfraContextReplyV1
	if err := c.Call("ContextPacks.SaveInfraContextV1", args, &reply); err != nil {
		return 0, err
	}
	return reply.Saved, nil
}
