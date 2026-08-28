package reclaim

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
)

const (
	orgID        = "6a8fc4300aaee89eaf15b035"
	deploymentID = "6a8fe8be1bcfd79ebf25c046"
	deployTag    = orgID + "-" + deploymentID + ":abc1234"
	ecrTag       = "1234.dkr.ecr.us-east-1.amazonaws.com/ecr-" + orgID + "-" + deploymentID + ":abc1234"
)

func summary(id string, tags ...string) image.Summary {
	return image.Summary{ID: id, RepoTags: tags}
}

func ownedSummary(id string, tags ...string) image.Summary {
	s := summary(id, tags...)
	s.Labels = Labels(PurposeDeploymentImage)
	return s
}

func TestSelectImagesToRemove(t *testing.T) {
	images := []image.Summary{
		summary("sha256:deploy", deployTag, ecrTag),
		summary("sha256:agentbox", "ghcr.io/deployment-io/agentbox:1.9.10"),
	}
	tests := []struct {
		name      string
		refs      []string
		protected map[string]bool
		remove    []string
		kept      []string
		absent    []string
	}{
		{
			name:   "both commit-scoped tags of a pushed image go",
			refs:   []string{ecrTag, deployTag},
			remove: []string{ecrTag, deployTag},
		},
		{
			name:      "a tag a container was created from is kept",
			refs:      []string{deployTag, ecrTag},
			protected: map[string]bool{deployTag: true},
			remove:    []string{ecrTag},
			kept:      []string{deployTag},
		},
		{
			name:      "every tag of a referenced image is kept, matched by content id",
			refs:      []string{deployTag, ecrTag},
			protected: map[string]bool{"sha256:deploy": true},
			kept:      []string{deployTag, ecrTag},
		},
		{
			name:   "a ref that is not on the daemon is not an error",
			refs:   []string{"never-built:abc1234"},
			absent: []string{"never-built:abc1234"},
		},
		{
			name:   "the same ref twice is removed once",
			refs:   []string{deployTag, deployTag},
			remove: []string{deployTag},
		},
		{
			name:   "a fully qualified hub ref matches the bare tag on the host",
			refs:   []string{"docker.io/deployment-io/agentbox:1.9.10"},
			absent: []string{"deployment-io/agentbox:1.9.10"},
		},
		{
			name:   "empty refs select nothing",
			refs:   []string{"", "   "},
			remove: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectImagesToRemove(test.refs, images, test.protected)
			assertRefs(t, "remove", got.remove, test.remove)
			assertRefs(t, "protected", got.protected, test.kept)
			assertRefs(t, "absent", got.absent, test.absent)
		})
	}
}

func TestSelectSupersededRefs(t *testing.T) {
	images := []image.Summary{
		summary("sha256:new", "ghcr.io/deployment-io/agentbox:1.9.10"),
		summary("sha256:old", "ghcr.io/deployment-io/agentbox:1.9.9"),
		summary("sha256:older", "ghcr.io/deployment-io/agentbox:1.9.7"),
		summary("sha256:node22", "node:22-bookworm"),
		summary("sha256:nodeold", "node:lts-buster"),
		summary("sha256:node20", "node:20"),
		summary("sha256:customer", "customer/api:latest"),
	}
	tests := []struct {
		name      string
		pulled    string
		by        Supersession
		protected map[string]bool
		remove    []string
		kept      []string
	}{
		{
			name:   "older tags of our own repository go, other repositories are untouched",
			pulled: "ghcr.io/deployment-io/agentbox:1.9.10",
			by:     Supersession{SameRepository: true},
			remove: []string{"ghcr.io/deployment-io/agentbox:1.9.9", "ghcr.io/deployment-io/agentbox:1.9.7"},
		},
		{
			name:      "an older agentbox a step is still running stays",
			pulled:    "ghcr.io/deployment-io/agentbox:1.9.10",
			by:        Supersession{SameRepository: true},
			protected: map[string]bool{"ghcr.io/deployment-io/agentbox:1.9.9": true},
			remove:    []string{"ghcr.io/deployment-io/agentbox:1.9.7"},
			kept:      []string{"ghcr.io/deployment-io/agentbox:1.9.9"},
		},
		{
			name:   "the ref just pulled is never a candidate",
			pulled: "ghcr.io/deployment-io/agentbox:1.9.10",
			by:     Supersession{SameRepository: true, RetiredRefs: []string{"ghcr.io/deployment-io/agentbox:1.9.10"}},
			remove: []string{"ghcr.io/deployment-io/agentbox:1.9.9", "ghcr.io/deployment-io/agentbox:1.9.7"},
		},
		{
			name:   "a public base image only gives up the refs we retired by name",
			pulled: "node:22-bookworm",
			by:     Supersession{RetiredRefs: []string{"node:lts-buster"}},
			remove: []string{"node:lts-buster"},
		},
		{
			name:   "a customer tag in a shared repository is not superseded",
			pulled: "node:22-bookworm",
			by:     Supersession{RetiredRefs: []string{"node:lts-buster"}},
			remove: []string{"node:lts-buster"},
			// node:20 and customer/api:latest are deliberately absent from
			// remove — sweeping the `node` repository would take them.
		},
		{
			name:   "no policy removes nothing",
			pulled: "node:22-bookworm",
			by:     Supersession{},
			remove: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectSupersededRefs(test.pulled, test.by, images, test.protected)
			assertRefs(t, "remove", got.remove, test.remove)
			assertRefs(t, "protected", got.protected, test.kept)
			for _, ref := range got.remove {
				if ref == "node:20" || ref == "customer/api:latest" {
					t.Errorf("selected a ref the runner does not own: %s", ref)
				}
			}
		})
	}
}

func TestProtectedImagesFromCoversEveryContainerState(t *testing.T) {
	containers := []types.Container{
		{ID: "c1", State: "running", Image: "ghcr.io/deployment-io/agentbox:1.9.9", ImageID: "sha256:old"},
		{ID: "c2", State: "exited", Image: "node:lts-buster", ImageID: "sha256:nodeold"},
	}
	protected := protectedImagesFrom(containers, map[string]bool{"ghcr.io/deployment-io/agentbox:1.9.10": true})
	for _, want := range []string{
		"ghcr.io/deployment-io/agentbox:1.9.9", // running container's ref
		"sha256:old",                           // ...and its content id
		"node:lts-buster",                      // a stopped container still pins its image
		"sha256:nodeold",
		"ghcr.io/deployment-io/agentbox:1.9.10", // held by an in-flight step
	} {
		if !protected[want] {
			t.Errorf("expected %s to be protected", want)
		}
	}
}

// A push that failed must leave the image alone; a push that succeeded must
// take both tags. The removal path itself must never force.
func TestRemoveImageRefsNeverForcesAndSkipsReferenced(t *testing.T) {
	fake := &fakeDocker{
		images:     []image.Summary{summary("sha256:deploy", deployTag, ecrTag)},
		containers: []types.Container{{ID: "c1", State: "running", ImageID: "sha256:deploy"}},
	}
	var logs bytes.Buffer
	removeImageRefs(context.Background(), fake, []string{deployTag, ecrTag}, &logs)
	if len(fake.removedImages) != 0 {
		t.Fatalf("removed %v while a container referenced the image", fake.removedImages)
	}

	fake.containers = nil
	removeImageRefs(context.Background(), fake, []string{deployTag, ecrTag}, &logs)
	if !reflect.DeepEqual(fake.removedImages, []string{deployTag, ecrTag}) {
		t.Fatalf("removed %v, want both commit-scoped tags", fake.removedImages)
	}
	for _, forced := range fake.removedImageForce {
		if forced {
			t.Fatal("image removal must never force")
		}
	}
}

// A daemon that can't answer means we don't know what is referenced, so
// nothing is removed.
func TestRemoveImageRefsDoesNothingWhenTheDaemonCannotAnswer(t *testing.T) {
	for _, fake := range []*fakeDocker{
		{listImagesErr: errors.New("daemon down")},
		{images: []image.Summary{summary("sha256:deploy", deployTag)}, listContainersErr: errors.New("daemon down")},
	} {
		var logs bytes.Buffer
		removeImageRefs(context.Background(), fake, []string{deployTag}, &logs)
		if len(fake.removedImages) != 0 {
			t.Errorf("removed %v despite an unusable daemon", fake.removedImages)
		}
		if logs.Len() == 0 {
			t.Error("expected the skipped cleanup to be logged")
		}
	}
}

func TestNormalizeRef(t *testing.T) {
	tests := []struct{ in, want string }{
		{"node:22-bookworm", "node:22-bookworm"},
		{"docker.io/library/node:22-bookworm", "node:22-bookworm"},
		{"index.docker.io/library/node:22-bookworm", "node:22-bookworm"},
		{"node", "node:latest"},
		{"ghcr.io/deployment-io/agentbox:1.9.10", "ghcr.io/deployment-io/agentbox:1.9.10"},
		{"registry:5000/app", "registry:5000/app:latest"},
		{"registry:5000/app:v1", "registry:5000/app:v1"},
		{"", ""},
	}
	for _, test := range tests {
		if got := normalizeRef(test.in); got != test.want {
			t.Errorf("normalizeRef(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestRepositoryOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"node:22-bookworm", "node"},
		{"ghcr.io/deployment-io/agentbox:1.9.10", "ghcr.io/deployment-io/agentbox"},
		{"registry:5000/app:v1", "registry:5000/app"},
		{"node@sha256:abc", "node"},
	}
	for _, test := range tests {
		if got := repositoryOf(test.in); got != test.want {
			t.Errorf("repositoryOf(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func assertRefs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
