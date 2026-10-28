package commands

import (
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
)

// RunNewAutomation and RunNewAgent belonged to the removed automations
// feature; the runner refuses them like any unknown command, so a leftover job
// of either kind fails.
func TestGetRefusesRemovedAutomationCommands(t *testing.T) {
	for _, command := range []commands_enums.Type{commands_enums.RunNewAutomation, commands_enums.RunNewAgent} {
		if c, err := Get(command); err == nil {
			t.Errorf("Get(%s) = %T, want an error", command, c)
		}
	}
}

// Nixpacks builds were removed; a leftover BuildNixPacksImage job fails with
// a message telling the user to switch to a Dockerfile.
func TestGetRefusesNixpacksBuild(t *testing.T) {
	c, err := Get(commands_enums.BuildNixPacksImage)
	if c != nil {
		t.Errorf("Get(BuildNixPacksImage) = %T, want nil", c)
	}
	want := "Nixpacks builds are no longer supported: deployment.io builds web and private services from a Dockerfile. Add a Dockerfile to the repository and create the service with the Docker runtime."
	if err == nil || err.Error() != want {
		t.Errorf("Get(BuildNixPacksImage) error = %v, want %q", err, want)
	}
}

func TestGetBuildDockerImage(t *testing.T) {
	c, err := Get(commands_enums.BuildDockerImage)
	if err != nil {
		t.Fatalf("Get(BuildDockerImage) error = %v", err)
	}
	if _, ok := c.(*BuildDockerImage); !ok {
		t.Errorf("Get(BuildDockerImage) = %T, want *BuildDockerImage", c)
	}
}
