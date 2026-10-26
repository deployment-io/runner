package reclaim

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeDocker is a stand-in daemon: it answers the list calls from fixed
// state and records what the package asked it to remove. Enough to assert
// both halves of every rule — what goes, and what is left alone.
type fakeDocker struct {
	images     []image.Summary
	containers []container.Summary
	volumes    []volume.Volume

	listImagesErr     error
	listContainersErr error
	removeImageErr    error

	removedImages       []string
	removedImageForce   []bool
	removedContainers   []string
	removedVolumes      []string
	removedVolForce     []bool
	prunedReservedSpace []int64
	closed              bool
}

func (f *fakeDocker) ImageList(context.Context, client.ImageListOptions) (client.ImageListResult, error) {
	return client.ImageListResult{Items: f.images}, f.listImagesErr
}

func (f *fakeDocker) ImageRemove(_ context.Context, imageID string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	if f.removeImageErr != nil {
		return client.ImageRemoveResult{}, f.removeImageErr
	}
	f.removedImages = append(f.removedImages, imageID)
	f.removedImageForce = append(f.removedImageForce, options.Force)
	return client.ImageRemoveResult{}, nil
}

func (f *fakeDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: f.containers}, f.listContainersErr
}

func (f *fakeDocker) ContainerRemove(_ context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if options.Force {
		return client.ContainerRemoveResult{}, fmt.Errorf("startup sweep must never force-remove containers")
	}
	f.removedContainers = append(f.removedContainers, containerID)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
	return client.VolumeListResult{Items: f.volumes}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.removedVolumes = append(f.removedVolumes, volumeID)
	f.removedVolForce = append(f.removedVolForce, options.Force)
	return client.VolumeRemoveResult{}, nil
}

func (f *fakeDocker) BuildCachePrune(_ context.Context, opts client.BuildCachePruneOptions) (client.BuildCachePruneResult, error) {
	f.prunedReservedSpace = append(f.prunedReservedSpace, opts.ReservedSpace)
	return client.BuildCachePruneResult{Report: build.CachePruneReport{SpaceReclaimed: 1 << 20}}, nil
}

func (f *fakeDocker) Close() error {
	f.closed = true
	return nil
}

// useFakeDocker points the package's daemon connection at f for the duration
// of a test.
func useFakeDocker(t interface{ Cleanup(func()) }, f *fakeDocker) {
	previous := openDocker
	openDocker = func() (Docker, error) { return f, nil }
	t.Cleanup(func() { openDocker = previous })
}
