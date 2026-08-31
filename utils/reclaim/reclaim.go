// Package reclaim keeps the runner's Docker daemon and scratch space from
// growing without bound.
//
// The runner pulls and builds images on every job and, until this package
// existed, never removed one: each deploy added a full application image
// tagged with its commit hash, every agentbox release stacked on top of the
// last, and the build cache grew forever. On a runner that stays up — or one
// that is SIGKILLed mid-job and so skips its deferred cleanup — the root
// volume fills until the next job dies with ENOSPC.
//
// Three rules shape everything here:
//
//   - Never remove something in use. Every removal is checked against the
//     containers on the daemon and against the refs this process has an
//     in-flight Step for (see inuse.go), and nothing is ever force-removed.
//     The agentbox image in particular is pulled per Step and a Step can run
//     concurrently with a deploy.
//
//   - Only touch what this runner owns. A customer's Docker daemon may be
//     shared with their own workloads, so we match on our own labels and our
//     own naming schemes rather than pruning by class. That makes the sweep
//     strictly narrower than `docker system prune` — deliberately.
//
//   - Cleanup is best-effort. A reclaim error is a log line, never a failed
//     deploy or Step. Every exported entry point swallows its errors after
//     logging them and returns nothing.
package reclaim

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/moby/moby/client"
)

const (
	// OwnerLabelKey marks every Docker object this runner creates. The
	// startup sweep reclaims orphans by this label, which is what keeps it
	// from touching a customer's own containers, images or volumes on a
	// shared daemon.
	//
	// Objects created by runner versions older than this label are not
	// swept — the naming-scheme matchers in sweep.go cover the ones that
	// have a name we can recognize (per-deploy images, cache volumes), and
	// nothing else is removable safely.
	OwnerLabelKey = "io.deployment.runner"

	// OwnerLabelValue is the only value OwnerLabelKey ever carries; the
	// sweep matches on the pair so a stray key can't widen it.
	OwnerLabelValue = "true"

	// PurposeLabelKey records what the object was for. Not used for
	// matching — it exists so `docker ps --filter label=...` is readable
	// when someone is looking at a wedged runner.
	PurposeLabelKey = "io.deployment.runner.purpose"
)

// Purposes recorded in PurposeLabelKey.
const (
	PurposeDeploymentImage = "deployment-image"
	PurposeAgentbox        = "agentbox"
	PurposeStaticSiteBuild = "static-site-build"
	PurposeAgentboxCache   = "agentbox-cache"
)

// Labels returns the label set stamped on a Docker object the runner
// creates. Pass one of the Purpose constants.
func Labels(purpose string) map[string]string {
	return map[string]string{
		OwnerLabelKey:   OwnerLabelValue,
		PurposeLabelKey: purpose,
	}
}

// ownedByRunner reports whether a label set marks the object as ours.
func ownedByRunner(labels map[string]string) bool {
	return labels[OwnerLabelKey] == OwnerLabelValue
}

// Docker is the slice of the Docker client this package talks to. It is
// declared here, on the consumer side, so the selection logic can be driven
// against a fake daemon in tests — there is no other way to exercise "this
// image is protected because a container references it" without a real host.
//
// It is wider than the one-method interfaces this repo prefers because
// reclaiming is inherently a multi-verb conversation with the daemon: you
// cannot decide what is removable without first listing what exists and what
// references it. Every method below has at least one caller in this package.
type Docker interface {
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImageRemove(ctx context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	BuildCachePrune(ctx context.Context, opts types.BuildCachePruneOptions) (*types.BuildCachePruneReport, error)
	Close() error
}

// openDocker connects to the daemon the same way every other Docker call in
// this repo does. A package-level var so tests can substitute a fake.
var openDocker = func() (Docker, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// withDocker runs fn against a daemon connection, logging and swallowing a
// connection failure. Cleanup never fails a job, so there is nothing to
// return.
func withDocker(logsWriter io.Writer, what string, fn func(ctx context.Context, d Docker)) {
	d, err := openDocker()
	if err != nil {
		logf(logsWriter, "Skipping %s — cannot reach Docker: %s", what, err)
		return
	}
	defer d.Close()
	fn(context.Background(), d)
}

// logf writes a cleanup line to the job's log stream when there is one, and
// to the runner's own log otherwise (the startup sweep has no job).
func logf(logsWriter io.Writer, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if logsWriter == nil {
		log.Println(message)
		return
	}
	_, _ = io.WriteString(logsWriter, message+"\n")
}

// normalizeRef rewrites a ref into the form the daemon reports in
// Summary.RepoTags, so a ref we hold can be compared against what is on the
// host. Docker Hub library images are reported bare ("node:22-bookworm")
// however they were pulled, and an untagged ref means ":latest".
func normalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	for _, prefix := range []string{"index.docker.io/library/", "docker.io/library/", "index.docker.io/", "docker.io/"} {
		if strings.HasPrefix(ref, prefix) {
			ref = strings.TrimPrefix(ref, prefix)
			break
		}
	}
	if !hasTag(ref) {
		ref += ":latest"
	}
	return ref
}

// hasTag reports whether ref carries a tag. A colon that appears before the
// last slash is a registry port, not a tag separator.
func hasTag(ref string) bool {
	colon := strings.LastIndex(ref, ":")
	return colon > strings.LastIndex(ref, "/")
}

// repositoryOf returns the repository part of a normalized ref — everything
// before the tag. Digest refs (repo@sha256:...) return the repository too.
func repositoryOf(ref string) string {
	if at := strings.LastIndex(ref, "@"); at > 0 {
		return ref[:at]
	}
	if hasTag(ref) {
		return ref[:strings.LastIndex(ref, ":")]
	}
	return ref
}
