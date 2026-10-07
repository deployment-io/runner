package client

import (
	"net/rpc"
	"runtime"
	"time"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// EnsureTaskPreview asks deployment-server to find-or-create the task's ephemeral
// preview environment and, within it, the lean Deployment for one service, returning
// the ids the runner deploys into. Idempotent — safe to call on every deploy. The
// runner's identity (cloud account, region, arch, os) is sent the same way as the
// job poll so the server pins the ephemeral env to this runner. serviceType is a
// task_previews.ServiceType* token.
func (r *RunnerClient) EnsureTaskPreview(organizationID, taskID, serviceName, serviceType string) (previewID, existingDistID, existingDomain string, err error) {
	if !r.isConnected {
		return "", "", "", ErrConnection
	}
	args := task_previews.EnsureTaskPreviewArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.TaskID = taskID
	args.CloudAccountID = r.cloudAccountID
	args.Region = r.runnerRegion
	args.GoArch = runtime.GOARCH
	args.GoOS = runtime.GOOS
	args.ServiceName = serviceName
	args.ServiceType = serviceType
	var reply task_previews.EnsureTaskPreviewReplyV1
	if err = r.c.Call("TaskPreviews.EnsureV1", args, &reply); err != nil {
		return "", "", "", err
	}
	return reply.PreviewID, reply.ExistingDistID, reply.ExistingDomain, nil
}

// ListTaskPreviews returns the previews of the task's active ephemeral preview
// environment (one per service that has a URL, sorted by service name) — empty when
// the task has none. Used to validate the URLs the agent's preview tools fetch and to
// list the previews in the task's pull request body.
func (r *RunnerClient) ListTaskPreviews(organizationID, taskID string) ([]task_previews.TaskPreviewV1, error) {
	if !r.isConnected {
		return nil, ErrConnection
	}
	args := task_previews.ListTaskPreviewsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.TaskID = taskID
	var reply task_previews.ListTaskPreviewsReplyV1
	if err := r.c.Call("TaskPreviews.ListV1", args, &reply); err != nil {
		return nil, err
	}
	return reply.Previews, nil
}

// StaticSiteBuildSettings returns how the org deploys the repository at cloneURL as a
// static site (empty Sites when deployment.io doesn't) and, when it does, the org's
// preview configuration. The reply carries the configuration's values: never log it.
//
// Like SaveInfraContext, the call runs over its own connection whose deadline is
// timeout from now, so a stalled request fails instead of holding the agent's tool
// call (or the shared connection).
func (r *RunnerClient) StaticSiteBuildSettings(organizationID, taskID, cloneURL string, timeout time.Duration) (task_previews.StaticSiteBuildSettingsReplyV1, error) {
	if !r.isConnected || r.dial == nil {
		return task_previews.StaticSiteBuildSettingsReplyV1{}, ErrConnection
	}
	deadline := time.Now().Add(timeout)
	conn, err := r.dial(timeout)
	if err != nil {
		return task_previews.StaticSiteBuildSettingsReplyV1{}, err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return task_previews.StaticSiteBuildSettingsReplyV1{}, err
	}
	c := rpc.NewClient(conn)
	defer c.Close()
	args := task_previews.StaticSiteBuildSettingsArgsV1{}
	args.OrganizationID = r.GetComputedOrganizationID(organizationID)
	args.Token = r.token
	args.TaskID = taskID
	args.CloneURL = cloneURL
	var reply task_previews.StaticSiteBuildSettingsReplyV1
	if err := c.Call("TaskPreviews.StaticSiteBuildSettingsV1", args, &reply); err != nil {
		return task_previews.StaticSiteBuildSettingsReplyV1{}, err
	}
	return reply, nil
}
