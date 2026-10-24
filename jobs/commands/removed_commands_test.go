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
