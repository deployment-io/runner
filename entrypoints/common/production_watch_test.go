package common

import (
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
)

// A production watch Job never calls MarkStepDone (its Task's working
// directory may be in use by a re-run); every other job is unchanged.
func TestIsProductionWatchJob(t *testing.T) {
	cases := []struct {
		name     string
		commands []commands_enums.Type
		want     bool
	}{
		{"production watch", []commands_enums.Type{commands_enums.RunProductionWatch}, true},
		{"task step", []commands_enums.Type{commands_enums.CheckoutRepo, commands_enums.RunAgentStep, commands_enums.CommitAndPush}, false},
		{"agent step alone", []commands_enums.Type{commands_enums.RunAgentStep}, false},
		{"production QA", []commands_enums.Type{commands_enums.RunProductionQA}, false},
		{"none", nil, false},
	}
	for _, c := range cases {
		if got := isProductionWatchJob(c.commands); got != c.want {
			t.Errorf("%s: isProductionWatchJob = %v, want %v", c.name, got, c.want)
		}
	}
}
