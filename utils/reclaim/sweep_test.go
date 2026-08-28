package reclaim

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
)

const taskID = "6a8fe8be1bcfd79ebf25c046"

// What the sweep must NOT match matters more than what it does: a customer's
// Docker daemon may be shared with their own workloads.
func TestSelectSweepContainers(t *testing.T) {
	ours := func(id, state string) types.Container {
		return types.Container{ID: id, State: state, Labels: Labels(PurposeAgentbox)}
	}
	tests := []struct {
		name       string
		containers []types.Container
		want       []string
	}{
		{
			name:       "our exited agentbox container is an orphan",
			containers: []types.Container{ours("exited-agentbox", "exited")},
			want:       []string{"exited-agentbox"},
		},
		{
			name:       "our dead container is an orphan too",
			containers: []types.Container{ours("dead", "dead")},
			want:       []string{"dead"},
		},
		{
			name:       "a running container of ours is a live job",
			containers: []types.Container{ours("running", "running")},
		},
		{
			name:       "a created container is mid-start, not an orphan",
			containers: []types.Container{ours("created", "created")},
		},
		{
			name:       "a paused or restarting container is still alive",
			containers: []types.Container{ours("paused", "paused"), ours("restarting", "restarting")},
		},
		{
			name: "a customer's exited container is none of our business",
			containers: []types.Container{
				{ID: "customer", State: "exited"},
				{ID: "customer-labelled", State: "exited", Labels: map[string]string{"com.example.app": "true"}},
				{ID: "wrong-value", State: "exited", Labels: map[string]string{OwnerLabelKey: "false"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRefs(t, "containers", selectSweepContainers(test.containers), test.want)
		})
	}
}

func TestSelectSweepImages(t *testing.T) {
	tests := []struct {
		name      string
		images    []image.Summary
		protected map[string]bool
		want      []string
	}{
		{
			name:   "a per-deploy image left by a crashed process goes, by its own tag",
			images: []image.Summary{summary("sha256:deploy", deployTag)},
			want:   []string{deployTag},
		},
		{
			name:      "a per-deploy image a container references stays",
			images:    []image.Summary{summary("sha256:deploy", deployTag)},
			protected: map[string]bool{"sha256:deploy": true},
		},
		{
			name:   "our dangling image goes by content id",
			images: []image.Summary{ownedSummary("sha256:dangling", "<none>:<none>")},
			want:   []string{"sha256:dangling"},
		},
		{
			name:   "an untagged image without our label may be the customer's",
			images: []image.Summary{summary("sha256:theirs")},
		},
		{
			name:   "a customer's tagged image is never matched",
			images: []image.Summary{summary("sha256:customer", "customer/api:latest"), summary("sha256:node", "node:22-bookworm")},
		},
		{
			name:   "an image whose repository merely looks id-shaped but is registry-qualified stays",
			images: []image.Summary{summary("sha256:registry", "registry.example.com/"+orgID+"-"+deploymentID+":abc1234")},
		},
		{
			name:   "the agentbox image is never swept — it is pulled per step",
			images: []image.Summary{summary("sha256:agentbox", "ghcr.io/deployment-io/agentbox:1.9.10")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRefs(t, "images", selectSweepImages(test.images, test.protected), test.want)
		})
	}
}

func TestSelectSweepVolumes(t *testing.T) {
	named := func(name string, labels map[string]string) *volume.Volume {
		return &volume.Volume{Name: name, Labels: labels}
	}
	tests := []struct {
		name    string
		volumes []*volume.Volume
		mounted map[string]bool
		want    []string
	}{
		{
			name:    "a cache volume matching our scheme is an orphan",
			volumes: []*volume.Volume{named("agentbox-cache-"+taskID+"-0", nil)},
			want:    []string{"agentbox-cache-" + taskID + "-0"},
		},
		{
			name:    "our label is enough even when the name is not recognizable",
			volumes: []*volume.Volume{named("agentbox-cache-legacy", Labels(PurposeAgentboxCache))},
			want:    []string{"agentbox-cache-legacy"},
		},
		{
			name:    "a cache volume a container still mounts stays",
			volumes: []*volume.Volume{named("agentbox-cache-"+taskID+"-2", Labels(PurposeAgentboxCache))},
			mounted: map[string]bool{"agentbox-cache-" + taskID + "-2": true},
		},
		{
			name: "customer volumes are never matched",
			volumes: []*volume.Volume{
				named("postgres-data", nil),
				named("agentbox-cache-not-an-object-id-0", nil),
				named("my-agentbox-cache-"+taskID+"-0", nil),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRefs(t, "volumes", selectSweepVolumes(test.volumes, test.mounted), test.want)
		})
	}
}

func TestMountedVolumesAndBoundPaths(t *testing.T) {
	containers := []types.Container{
		{
			ID:    "live",
			State: "running",
			Mounts: []types.MountPoint{
				{Type: mount.TypeVolume, Name: "agentbox-cache-" + taskID + "-0"},
				{Type: mount.TypeBind, Source: "/tmp/" + orgID + "/" + taskID},
			},
		},
		{
			ID:     "finished",
			State:  "exited",
			Mounts: []types.MountPoint{{Type: mount.TypeBind, Source: "/tmp/" + orgID + "/old"}},
		},
	}
	if !mountedVolumes(containers)["agentbox-cache-"+taskID+"-0"] {
		t.Error("a mounted cache volume must be indexed")
	}
	bound := boundPaths(containers)
	if !bound["/tmp/"+orgID+"/"+taskID] {
		t.Error("a live container's bind source must be indexed")
	}
	if bound["/tmp/"+orgID+"/old"] {
		t.Error("an exited container does not hold its bind source")
	}
}

func TestSelectStaleWorkDirs(t *testing.T) {
	root := t.TempDir()
	ourDir := filepath.Join(root, orgID, taskID)
	liveDir := filepath.Join(root, orgID, "6a8fc4300aaee89eaf15b099")
	for _, dir := range []string{
		ourDir,
		liveDir,
		filepath.Join(root, orgID, "not-an-object-id"),        // wrong task segment
		filepath.Join(root, "systemd-private-abcdef", taskID), // wrong org segment
		filepath.Join(root, orgID+"extra", taskID),            // near-miss org segment
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A loose file at the top level of the root must never be considered.
	if err := os.WriteFile(filepath.Join(root, "customer.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := selectStaleWorkDirs(root, map[string]bool{
		// A second runner process would still be bind-mounting this one.
		filepath.Join(liveDir, "0-repo"): true,
	})
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{ourDir}) {
		t.Fatalf("stale dirs = %v, want just %v", got, ourDir)
	}
}

func TestSelectStaleWorkDirsOnAMissingRoot(t *testing.T) {
	if got := selectStaleWorkDirs(filepath.Join(t.TempDir(), "does-not-exist"), nil); got != nil {
		t.Fatalf("expected no candidates under a missing root, got %v", got)
	}
}

// The sweep derives its root from the same function the Task commands build
// work dirs with, so the two cannot drift apart silently.
func TestTaskWorkDirRootTracksTheLayout(t *testing.T) {
	if got := taskWorkDirRoot(); got != "/tmp" {
		t.Fatalf("taskWorkDirRoot() = %q, want the parent of <root>/<orgID>/<taskID>", got)
	}
}

func TestIsBound(t *testing.T) {
	bound := map[string]bool{"/tmp/org/task/0-repo": true}
	if !isBound("/tmp/org/task", bound) {
		t.Error("a directory containing a live bind mount is in use")
	}
	if isBound("/tmp/org/other", bound) {
		t.Error("an unrelated directory is not in use")
	}
	if isBound("/tmp/org/task-2", bound) {
		t.Error("a prefix match must respect path separators")
	}
}
