package reclaim

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/image"
)

// The window between "pulled the image" and "created the container" is the
// one a container-reference check cannot see. A Step holding the ref must
// survive a concurrent Step's superseded-image cleanup.
func TestMarkImageInUseProtectsARefWithNoContainerYet(t *testing.T) {
	release := MarkImageInUse("ghcr.io/deployment-io/agentbox:1.9.9")
	defer release()

	images := []image.Summary{
		summary("sha256:new", "ghcr.io/deployment-io/agentbox:1.9.10"),
		summary("sha256:old", "ghcr.io/deployment-io/agentbox:1.9.9"),
	}
	protected, err := protectedImages(context.Background(), &fakeDocker{})
	if err != nil {
		t.Fatal(err)
	}
	got := selectSupersededRefs("ghcr.io/deployment-io/agentbox:1.9.10", Supersession{SameRepository: true}, images, protected)
	if len(got.remove) != 0 {
		t.Fatalf("removed %v while a step was holding it", got.remove)
	}
	assertRefs(t, "protected", got.protected, []string{"ghcr.io/deployment-io/agentbox:1.9.9"})
}

// Two Steps can hold the same ref; the first to finish must not clear the
// second's protection.
func TestMarkImageInUseIsRefcountedAndReleaseIsIdempotent(t *testing.T) {
	first := MarkImageInUse("node:22-bookworm")
	second := MarkImageInUse("docker.io/library/node:22-bookworm")

	first()
	first() // a second release must not decrement anyone else's hold
	if !inUseRefs()["node:22-bookworm"] {
		t.Fatal("the second holder's protection was dropped")
	}

	second()
	if inUseRefs()["node:22-bookworm"] {
		t.Fatal("the ref should be released once every holder is done")
	}
}
