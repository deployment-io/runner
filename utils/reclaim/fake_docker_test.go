package reclaim

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
)

// fakeDocker is a stand-in daemon: it answers the list calls from fixed
// state and records what the package asked it to remove. Enough to assert
// both halves of every rule — what goes, and what is left alone.
type fakeDocker struct {
	images     []image.Summary
	containers []types.Container
	volumes    []*volume.Volume

	listImagesErr     error
	listContainersErr error
	removeImageErr    error

	removedImages     []string
	removedImageForce []bool
	removedContainers []string
	removedVolumes    []string
	removedVolForce   []bool
	prunedKeepStorage []int64
	closed            bool
}

func (f *fakeDocker) ImageList(context.Context, image.ListOptions) ([]image.Summary, error) {
	return f.images, f.listImagesErr
}

func (f *fakeDocker) ImageRemove(_ context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error) {
	if f.removeImageErr != nil {
		return nil, f.removeImageErr
	}
	f.removedImages = append(f.removedImages, imageID)
	f.removedImageForce = append(f.removedImageForce, options.Force)
	return nil, nil
}

func (f *fakeDocker) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return f.containers, f.listContainersErr
}

func (f *fakeDocker) ContainerRemove(_ context.Context, containerID string, options container.RemoveOptions) error {
	if options.Force {
		return fmt.Errorf("startup sweep must never force-remove containers")
	}
	f.removedContainers = append(f.removedContainers, containerID)
	return nil
}

func (f *fakeDocker) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	return volume.ListResponse{Volumes: f.volumes}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, volumeID string, force bool) error {
	f.removedVolumes = append(f.removedVolumes, volumeID)
	f.removedVolForce = append(f.removedVolForce, force)
	return nil
}

func (f *fakeDocker) BuildCachePrune(_ context.Context, opts types.BuildCachePruneOptions) (*types.BuildCachePruneReport, error) {
	f.prunedKeepStorage = append(f.prunedKeepStorage, opts.KeepStorage)
	return &types.BuildCachePruneReport{SpaceReclaimed: 1 << 20}, nil
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
