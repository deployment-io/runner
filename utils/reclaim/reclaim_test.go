package reclaim

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
)

func TestSweepContainersRemovesOnlyOurFinishedOnes(t *testing.T) {
	fake := &fakeDocker{}
	containers := []types.Container{
		{ID: "ours-exited", State: "exited", Labels: Labels(PurposeStaticSiteBuild)},
		{ID: "ours-running", State: "running", Labels: Labels(PurposeAgentbox)},
		{ID: "theirs-exited", State: "exited"},
	}
	// fakeDocker.ContainerRemove errors on Force, so a forced removal would
	// fail this test rather than silently pass.
	sweepContainers(context.Background(), fake, containers)
	if !reflect.DeepEqual(fake.removedContainers, []string{"ours-exited"}) {
		t.Fatalf("removed %v, want just our exited container", fake.removedContainers)
	}
}

func TestSweepVolumesNeverForces(t *testing.T) {
	fake := &fakeDocker{volumes: []*volume.Volume{
		{Name: "agentbox-cache-" + taskID + "-0", Labels: Labels(PurposeAgentboxCache)},
		{Name: "customer-data"},
	}}
	sweepVolumes(context.Background(), fake, nil)
	if !reflect.DeepEqual(fake.removedVolumes, []string{"agentbox-cache-" + taskID + "-0"}) {
		t.Fatalf("removed %v, want just our cache volume", fake.removedVolumes)
	}
	for _, forced := range fake.removedVolForce {
		if forced {
			t.Fatal("the sweep must let the daemon refuse a volume that is in use")
		}
	}
}

func TestSweepImagesSkipsWhatAContainerReferences(t *testing.T) {
	fake := &fakeDocker{images: []image.Summary{
		summary("sha256:deploy", deployTag),
		summary("sha256:live", orgID+"-"+deploymentID+":live0001"),
	}}
	containers := []types.Container{{ID: "c1", State: "running", ImageID: "sha256:live"}}
	sweepImages(context.Background(), fake, containers)
	if !reflect.DeepEqual(fake.removedImages, []string{deployTag}) {
		t.Fatalf("removed %v, want only the unreferenced per-deploy image", fake.removedImages)
	}
}

func TestPruneBuildCacheIsBoundedNotUnconditional(t *testing.T) {
	fake := &fakeDocker{}
	useFakeDocker(t, fake)
	t.Setenv(buildCacheKeepBytesEnvVar, "2147483648") // 2 GiB
	var logs bytes.Buffer
	PruneBuildCache(&logs)
	if !reflect.DeepEqual(fake.prunedKeepStorage, []int64{2 << 30}) {
		t.Fatalf("prune keep-storage = %v, want the configured budget", fake.prunedKeepStorage)
	}
	if !strings.Contains(logs.String(), "Build cache pruned") {
		t.Errorf("expected the reclaim to be logged, got %q", logs.String())
	}
}

// Cleanup is best-effort: an unreachable daemon is a log line, never an
// error a caller could fail a deploy on.
func TestReclaimEntryPointsSurviveAnUnreachableDaemon(t *testing.T) {
	previous := openDocker
	openDocker = func() (Docker, error) { return nil, errors.New("docker not running") }
	t.Cleanup(func() { openDocker = previous })

	var logs bytes.Buffer
	RemoveLocalImages([]string{deployTag}, &logs)
	PruneSupersededImages("node:22-bookworm", Supersession{RetiredRefs: []string{"node:lts-buster"}}, &logs)
	PruneBuildCache(&logs)
	if !strings.Contains(logs.String(), "cannot reach Docker") {
		t.Errorf("expected the unreachable daemon to be logged, got %q", logs.String())
	}
}

func TestRemoveLocalImagesWithNoRefsDoesNotTouchTheDaemon(t *testing.T) {
	fake := &fakeDocker{}
	useFakeDocker(t, fake)
	RemoveLocalImages(nil, &bytes.Buffer{})
	if fake.closed {
		t.Error("no refs means there is nothing to ask the daemon about")
	}
}

func TestLabels(t *testing.T) {
	labels := Labels(PurposeAgentbox)
	if !ownedByRunner(labels) {
		t.Error("a label set we produce must be recognized as ours")
	}
	if labels[PurposeLabelKey] != PurposeAgentbox {
		t.Errorf("purpose = %q, want %q", labels[PurposeLabelKey], PurposeAgentbox)
	}
	if ownedByRunner(map[string]string{OwnerLabelKey: "1"}) {
		t.Error("only the exact owner value counts")
	}
	if ownedByRunner(nil) {
		t.Error("an unlabelled object is not ours")
	}
}
