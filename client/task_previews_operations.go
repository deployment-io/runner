package client

import (
	"runtime"

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
