package reclaim

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
)

// Naming schemes the runner creates resources under. The sweep matches on
// these because a resource created by an older runner build carries no
// label, and an unlabelled, unrecognizable resource is not ours to remove.
//
// Each mirrors exactly one producer; when you change a producer, change the
// pattern and its test with it.
var (
	// deployImageTagRe mirrors getDockerImageNameAndTag
	// (jobs/commands/commands.go): <organizationID>-<deploymentID>:<commit-hash>,
	// with both ids Mongo ObjectID hex.
	deployImageTagRe = regexp.MustCompile(`^[0-9a-fA-F]{24}-[0-9a-fA-F]{24}:[^:/]+$`)

	// cacheVolumeRe mirrors cacheVolumeName (jobs/commands/run_agent_step.go):
	// agentbox-cache-<taskID>-<stepIndex>.
	cacheVolumeRe = regexp.MustCompile(`^agentbox-cache-[0-9a-fA-F]{24}-\d+$`)

	// objectIDRe matches the two path segments of
	// commandUtils.GetTaskRepositoriesBaseDir — /<root>/<orgID>/<taskID>.
	// Anything else under the root belongs to someone else.
	objectIDRe = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)
)

// Sweep reclaims the resources a previous runner process left behind.
//
// Every one of these has correct cleanup on the happy path — a deferred
// volume remove, a deferred container remove, an os.RemoveAll when a job is
// marked done. A SIGKILL, an OOM kill or a host restart skips all of them,
// and nothing else ever comes back for what was left.
//
// Startup is the one moment nothing of ours is running, which is what makes
// it safe to be this aggressive; it is also why this must run to completion
// BEFORE the job loop starts polling. A sweep racing a live job could delete
// the work dir out from under it.
//
// Best-effort throughout: every step logs its own failure and the next one
// still runs.
func Sweep() {
	LogFreeDisk("at startup", nil)
	withDocker(nil, "startup sweep", func(ctx context.Context, d Docker) {
		containers, err := d.ContainerList(ctx, container.ListOptions{All: true})
		if err != nil {
			logSweep("Startup sweep: cannot list containers: %s", err)
			return
		}
		reclaiming(nil, "orphaned containers", func() { sweepContainers(ctx, d, containers) })
		// Re-read once the orphaned containers are gone: an image or a cache
		// volume whose only reference was one of them is reclaimable now,
		// and the pre-sweep list would still show it pinned. A failed
		// re-read keeps the stale list, which can only over-protect.
		if fresh, freshErr := d.ContainerList(ctx, container.ListOptions{All: true}); freshErr == nil {
			containers = fresh
		}
		reclaiming(nil, "orphaned images", func() { sweepImages(ctx, d, containers) })
		reclaiming(nil, "orphaned cache volumes", func() { sweepVolumes(ctx, d, containers) })
		reclaiming(nil, "stale task working directories", func() { sweepWorkDirs(containers) })
	})
	PruneBuildCache(nil)
}

// logSweep is Sweep's shorthand for the runner-log-only form of logf (the
// startup sweep has no job log stream to write into).
func logSweep(format string, args ...interface{}) {
	logf(nil, format, args...)
}

// sweepContainers removes the runner's own containers that are no longer
// doing anything — an agentbox or static-site build container whose deferred
// removal never ran. Never force: a container the daemon still considers
// live stays.
func sweepContainers(ctx context.Context, d Docker, containers []types.Container) {
	for _, id := range selectSweepContainers(containers) {
		if err := d.ContainerRemove(ctx, id, container.RemoveOptions{}); err != nil {
			logSweep("Startup sweep: could not remove container %s: %s", shortID(id), err)
			continue
		}
		logSweep("Startup sweep: removed orphaned container %s", shortID(id))
	}
}

// selectSweepContainers picks our own containers in a finished state.
func selectSweepContainers(containers []types.Container) []string {
	var ids []string
	for _, c := range containers {
		if !ownedByRunner(c.Labels) || isActiveContainer(c.State) {
			continue
		}
		ids = append(ids, c.ID)
	}
	return ids
}

// isActiveContainer reports whether a container is still doing something, or
// about to. "created" counts as active: it is the state a container sits in
// between ContainerCreate and ContainerStart, so removing one would break a
// job that is starting rather than reclaim one that died.
func isActiveContainer(state string) bool {
	switch state {
	case "running", "paused", "restarting", "created", "removing":
		return true
	}
	return false
}

// sweepImages removes the runner's own images that nothing references: the
// per-deploy application images a crashed process never got to untag, and
// dangling (untagged) images we built.
//
// A local per-deploy image is safe to drop here even if it never reached
// ECR: the process that built it is gone, so the deploy it belonged to
// failed, and no rollback targets a build that never completed. What a
// rollback does need — the pushed image — lives in ECR.
func sweepImages(ctx context.Context, d Docker, containers []types.Container) {
	images, err := d.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		logSweep("Startup sweep: cannot list images: %s", err)
		return
	}
	protected := protectedImagesFrom(containers, inUseRefs())
	for _, ref := range selectSweepImages(images, protected) {
		if _, err := d.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true}); err != nil {
			logSweep("Startup sweep: could not remove image %s: %s", ref, err)
			continue
		}
		logSweep("Startup sweep: removed orphaned image %s", ref)
	}
}

// selectSweepImages picks images to remove, by tag where the tag identifies
// them as ours and by ID for dangling ones.
//
// Deliberately narrower than `docker image prune`: an untagged image without
// our label may well be a customer's on a shared daemon, so it stays.
func selectSweepImages(images []image.Summary, protected map[string]bool) []string {
	var refs []string
	for _, summary := range images {
		if protected[summary.ID] {
			continue
		}
		if tags := ownedTags(summary); len(tags) > 0 {
			for _, tag := range tags {
				if !protected[tag] {
					refs = append(refs, tag)
				}
			}
			continue
		}
		if isDangling(summary) && ownedByRunner(summary.Labels) {
			refs = append(refs, summary.ID)
		}
	}
	return refs
}

// ownedTags returns the tags on an image that match a naming scheme the
// runner owns.
func ownedTags(summary image.Summary) []string {
	var tags []string
	for _, tag := range summary.RepoTags {
		if deployImageTagRe.MatchString(tag) {
			tags = append(tags, tag)
		}
	}
	return tags
}

// isDangling reports whether an image carries no usable tag. The daemon
// reports either an empty list or the literal "<none>:<none>".
func isDangling(summary image.Summary) bool {
	for _, tag := range summary.RepoTags {
		if tag != "" && tag != "<none>:<none>" {
			return false
		}
	}
	return true
}

// sweepVolumes removes per-Step cache volumes whose Step is gone. Never
// force: the daemon refuses to remove a volume a container still uses, and
// that refusal is a safety check we want rather than one to override.
func sweepVolumes(ctx context.Context, d Docker, containers []types.Container) {
	listed, err := d.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		logSweep("Startup sweep: cannot list volumes: %s", err)
		return
	}
	for _, name := range selectSweepVolumes(listed.Volumes, mountedVolumes(containers)) {
		if err := d.VolumeRemove(ctx, name, false); err != nil {
			logSweep("Startup sweep: could not remove volume %s: %s", name, err)
			continue
		}
		logSweep("Startup sweep: removed orphaned cache volume %s", name)
	}
}

// selectSweepVolumes picks cache volumes that are ours by label or by name
// and that no container mounts.
func selectSweepVolumes(volumes []*volume.Volume, mounted map[string]bool) []string {
	var names []string
	for _, v := range volumes {
		if v == nil || mounted[v.Name] {
			continue
		}
		if ownedByRunner(v.Labels) || cacheVolumeRe.MatchString(v.Name) {
			names = append(names, v.Name)
		}
	}
	return names
}

// mountedVolumes indexes the named volumes any container mounts, in any
// state — a stopped container still owns its volume as far as the daemon is
// concerned.
func mountedVolumes(containers []types.Container) map[string]bool {
	mounted := map[string]bool{}
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Type == mount.TypeVolume && m.Name != "" {
				mounted[m.Name] = true
			}
		}
	}
	return mounted
}

// sweepWorkDirs removes Task working directories a previous process never
// cleaned up. MarkStepDone os.RemoveAll's these on the way out of every Step
// Job; a killed runner leaves a full checkout of every repo in the Task.
func sweepWorkDirs(containers []types.Container) {
	root := taskWorkDirRoot()
	for _, dir := range selectStaleWorkDirs(root, boundPaths(containers)) {
		if err := os.RemoveAll(dir); err != nil {
			logSweep("Startup sweep: could not remove work dir %s: %s", dir, err)
			continue
		}
		logSweep("Startup sweep: removed stale task work dir %s", dir)
	}
}

// selectStaleWorkDirs lists the <orgID>/<taskID> directories under root.
//
// Both path segments must be ObjectID hex — that two-level shape is the
// runner's own scheme, and matching it is what keeps the sweep out of the
// rest of a shared /tmp. A directory an active container bind-mounts is
// skipped, which covers the (unsupported but survivable) case of a second
// runner process on the same host.
func selectStaleWorkDirs(root string, bound map[string]bool) []string {
	orgDirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var stale []string
	for _, orgDir := range orgDirs {
		if !orgDir.IsDir() || !objectIDRe.MatchString(orgDir.Name()) {
			continue
		}
		orgPath := filepath.Join(root, orgDir.Name())
		taskDirs, err := os.ReadDir(orgPath)
		if err != nil {
			continue
		}
		for _, taskDir := range taskDirs {
			if !taskDir.IsDir() || !objectIDRe.MatchString(taskDir.Name()) {
				continue
			}
			taskPath := filepath.Join(orgPath, taskDir.Name())
			if isBound(taskPath, bound) {
				continue
			}
			stale = append(stale, taskPath)
		}
	}
	return stale
}

// taskWorkDirRoot is the directory Task work dirs live two levels under.
// Derived from the real layout function rather than hardcoded, so a change
// to the scheme moves the sweep with it instead of silently orphaning it.
func taskWorkDirRoot() string {
	base := commandUtils.GetTaskRepositoriesBaseDir("organization", "task")
	return filepath.Dir(filepath.Dir(base))
}

// boundPaths indexes host paths that active containers bind-mount.
func boundPaths(containers []types.Container) map[string]bool {
	bound := map[string]bool{}
	for _, c := range containers {
		if !isActiveContainer(c.State) {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == mount.TypeBind && m.Source != "" {
				bound[filepath.Clean(m.Source)] = true
			}
		}
	}
	return bound
}

// shortID trims a container ID to the 12 chars docker itself displays.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// isBound reports whether dir is bind-mounted by an active container, either
// directly or as an ancestor of one of its mounts.
func isBound(dir string, bound map[string]bool) bool {
	dir = filepath.Clean(dir)
	for source := range bound {
		if source == dir || strings.HasPrefix(source, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
