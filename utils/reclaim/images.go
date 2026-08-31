package reclaim

import (
	"context"
	"io"
	"os"
	"strconv"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

const (
	// buildCacheKeepBytesEnvVar overrides how much build cache survives a
	// prune.
	buildCacheKeepBytesEnvVar = "RUNNER_BUILD_CACHE_KEEP_BYTES"

	// defaultBuildCacheKeepBytes bounds the cache rather than emptying it.
	// The cache is what makes a repeat build fast, so the prune keeps a
	// working set and drops the rest — unconditional pruning would trade
	// this leak for a permanent build slowdown.
	defaultBuildCacheKeepBytes = 10 << 30 // 10 GiB
)

// imageSelection is the outcome of deciding what to remove. Split three ways
// so the caller can log why something survived, and so the selection rules
// can be table-tested without a daemon.
type imageSelection struct {
	// remove holds refs that exist locally and nothing references.
	remove []string
	// protected holds refs held back because a container references them or
	// an in-flight Step is holding them.
	protected []string
	// absent holds refs that aren't on the daemon — already gone, nothing
	// to do.
	absent []string
}

// RemoveLocalImages drops local tags for images that are safely durable
// elsewhere — in practice the two commit-scoped tags a deploy leaves behind
// once the image is in ECR.
//
// Removing by tag is an untag: the underlying image goes away only when its
// last tag does, which is why both the build tag and the ECR tag are passed
// together. Nothing is force-removed, so a tag the daemon still considers in
// use fails the call rather than tearing anything down.
func RemoveLocalImages(refs []string, logsWriter io.Writer) {
	if len(refs) == 0 {
		return
	}
	reclaiming(logsWriter, "local image tags", func() {
		withDocker(logsWriter, "local image cleanup", func(ctx context.Context, d Docker) {
			removeImageRefs(ctx, d, refs, logsWriter)
		})
	})
}

// Supersession describes which older images a successful pull makes
// reclaimable.
type Supersession struct {
	// SameRepository removes every other tag of the pulled repository. Only
	// set this for a repository the runner owns end to end (the agentbox
	// image), where every tag present is one we pulled ourselves.
	SameRepository bool

	// RetiredRefs are exact refs this runner used to pull and no longer
	// does. Public base images list them explicitly instead of sweeping the
	// whole repository, because a customer sharing the daemon may have
	// their own tags of `node` that are none of our business.
	RetiredRefs []string
}

// PruneSupersededImages removes the older images a pull has replaced. Called
// after the pull succeeds, so the replacement is on disk before its
// predecessor leaves.
func PruneSupersededImages(pulled string, by Supersession, logsWriter io.Writer) {
	withDocker(logsWriter, "superseded image cleanup", func(ctx context.Context, d Docker) {
		images, err := d.ImageList(ctx, image.ListOptions{All: false})
		if err != nil {
			logf(logsWriter, "Skipping superseded image cleanup — cannot list images: %s", err)
			return
		}
		protected, err := protectedImages(ctx, d)
		if err != nil {
			logf(logsWriter, "Skipping superseded image cleanup — cannot list containers: %s", err)
			return
		}
		selection := selectSupersededRefs(pulled, by, images, protected)
		if len(selection.remove) == 0 {
			logSkipped(selection, logsWriter)
			return
		}
		reclaiming(logsWriter, "superseded images", func() {
			removeSelected(ctx, d, selection, logsWriter)
		})
	})
}

// PruneBuildCache trims the Docker build cache to a bounded working set. The
// cache grows across builds and nothing else ever reclaims it.
func PruneBuildCache(logsWriter io.Writer) {
	reclaiming(logsWriter, "docker build cache", func() {
		withDocker(logsWriter, "build cache prune", func(ctx context.Context, d Docker) {
			keep := buildCacheKeepBytes()
			report, err := d.BuildCachePrune(ctx, types.BuildCachePruneOptions{
				// All=false leaves cache that is still attached to an image
				// on the host alone; only unused records are candidates.
				All:         false,
				KeepStorage: keep,
			})
			if err != nil {
				logf(logsWriter, "Build cache prune failed: %s", err)
				return
			}
			if report == nil || report.SpaceReclaimed == 0 {
				logf(logsWriter, "Build cache already within its %s budget", humanBytes(uint64(keep)))
				return
			}
			logf(logsWriter, "Build cache pruned: %s recovered, %s budget kept",
				humanBytes(report.SpaceReclaimed), humanBytes(uint64(keep)))
		})
	})
}

// removeImageRefs is the reference-checked removal shared by every image
// path in this package.
func removeImageRefs(ctx context.Context, d Docker, refs []string, logsWriter io.Writer) {
	images, err := d.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		logf(logsWriter, "Skipping image cleanup — cannot list images: %s", err)
		return
	}
	protected, err := protectedImages(ctx, d)
	if err != nil {
		logf(logsWriter, "Skipping image cleanup — cannot list containers: %s", err)
		return
	}
	selection := selectImagesToRemove(refs, images, protected)
	removeSelected(ctx, d, selection, logsWriter)
}

// removeSelected performs the removals a selection allows and logs the rest.
func removeSelected(ctx context.Context, d Docker, selection imageSelection, logsWriter io.Writer) {
	logSkipped(selection, logsWriter)
	for _, ref := range selection.remove {
		// Force stays off: a forced remove would tear an image out from
		// under whatever the daemon knows about that we don't.
		if _, err := d.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true}); err != nil {
			logf(logsWriter, "Could not remove image %s: %s", ref, err)
			continue
		}
		logf(logsWriter, "Removed local image %s", ref)
	}
}

// logSkipped explains what survived, so a runner that is still filling up
// says why.
func logSkipped(selection imageSelection, logsWriter io.Writer) {
	for _, ref := range selection.protected {
		logf(logsWriter, "Keeping image %s — a container or a running step references it", ref)
	}
}

// protectedImages indexes everything that must not be removed: every image a
// container references (by content ID and by the ref the container was
// created from, since either may be how we know it) plus every ref an
// in-flight Step in this process is holding.
//
// Containers in every state count, not just running ones — a stopped
// container still pins its image, and removing it would need force, which we
// never use.
func protectedImages(ctx context.Context, d Docker) (map[string]bool, error) {
	containers, err := d.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	return protectedImagesFrom(containers, inUseRefs()), nil
}

// protectedImagesFrom is the pure half of protectedImages.
func protectedImagesFrom(containers []types.Container, held map[string]bool) map[string]bool {
	protected := map[string]bool{}
	for _, c := range containers {
		if c.ImageID != "" {
			protected[c.ImageID] = true
		}
		if c.Image != "" {
			protected[normalizeRef(c.Image)] = true
			protected[c.Image] = true
		}
	}
	for ref := range held {
		protected[ref] = true
	}
	return protected
}

// selectImagesToRemove splits the requested refs into what can go, what is
// pinned, and what was never there.
func selectImagesToRemove(refs []string, images []image.Summary, protected map[string]bool) imageSelection {
	byTag := imagesByTag(images)
	var selection imageSelection
	seen := map[string]bool{}
	for _, raw := range refs {
		ref := normalizeRef(raw)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		summary, present := byTag[ref]
		if !present {
			selection.absent = append(selection.absent, ref)
			continue
		}
		if protected[ref] || protected[summary.ID] {
			selection.protected = append(selection.protected, ref)
			continue
		}
		selection.remove = append(selection.remove, ref)
	}
	return selection
}

// selectSupersededRefs works out which older refs a pull replaced, then runs
// them through the same reference check as any other removal.
//
// The pulled ref itself is never a candidate, whatever the policy says.
func selectSupersededRefs(pulled string, by Supersession, images []image.Summary, protected map[string]bool) imageSelection {
	current := normalizeRef(pulled)
	var candidates []string
	if by.SameRepository && current != "" {
		repository := repositoryOf(current)
		for _, summary := range images {
			for _, tag := range summary.RepoTags {
				tag = normalizeRef(tag)
				if tag != current && repositoryOf(tag) == repository {
					candidates = append(candidates, tag)
				}
			}
		}
	}
	for _, retired := range by.RetiredRefs {
		if normalizeRef(retired) != current {
			candidates = append(candidates, retired)
		}
	}
	return selectImagesToRemove(candidates, images, protected)
}

// imagesByTag indexes the daemon's images by every tag they carry, in the
// normalized form refs are compared in.
func imagesByTag(images []image.Summary) map[string]image.Summary {
	byTag := map[string]image.Summary{}
	for _, summary := range images {
		for _, tag := range summary.RepoTags {
			if tag == "" || tag == "<none>:<none>" {
				continue
			}
			byTag[normalizeRef(tag)] = summary
		}
	}
	return byTag
}

// buildCacheKeepBytes resolves the cache budget, falling back to the default
// on anything unparseable.
func buildCacheKeepBytes() int64 {
	if v := os.Getenv(buildCacheKeepBytesEnvVar); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 0 {
			return parsed
		}
	}
	return defaultBuildCacheKeepBytes
}
