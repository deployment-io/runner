package common

import (
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// A production watch Job and a dependency bump Job belong to a Task but
// aren't its Step, so they never call MarkStepDone (the Task's working
// directory may be in use by a Step); every other job is unchanged.
func TestIsNonStepTaskJob(t *testing.T) {
	cases := []struct {
		name     string
		commands []commands_enums.Type
		want     bool
	}{
		{"production watch", []commands_enums.Type{commands_enums.RunProductionWatch}, true},
		{"dependency bump", []commands_enums.Type{commands_enums.RunDependencyBump}, true},
		{"task step", taskStepCommands, false},
		{"agent step alone", []commands_enums.Type{commands_enums.RunAgentStep}, false},
		{"session", []commands_enums.Type{commands_enums.CheckoutRepo, commands_enums.RunAssistantSession}, false},
		{"production QA", []commands_enums.Type{commands_enums.RunProductionQA}, false},
		{"none", nil, false},
	}
	for _, c := range cases {
		if got := isNonStepTaskJob(c.commands); got != c.want {
			t.Errorf("%s: isNonStepTaskJob = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNeedsMarkStepDone(t *testing.T) {
	task := map[string]interface{}{}
	_ = jobs.SetParameterValue[string](task, parameters_enums.TaskID, "task-1")
	session := map[string]interface{}{}
	_ = jobs.SetParameterValue[string](session, parameters_enums.TaskID, "task-1")
	_ = jobs.SetParameterValue[string](session, parameters_enums.SessionUUID, "s-1")
	cases := []struct {
		name     string
		params   map[string]interface{}
		commands []commands_enums.Type
		want     bool
	}{
		{"task step", task, taskStepCommands, true},
		{"dependency bump", task, []commands_enums.Type{commands_enums.RunDependencyBump}, false},
		{"production watch", task, []commands_enums.Type{commands_enums.RunProductionWatch}, false},
		{"session", session, []commands_enums.Type{commands_enums.CheckoutRepo, commands_enums.RunAssistantSession}, false},
		{"deployment", map[string]interface{}{}, []commands_enums.Type{commands_enums.CheckoutRepo}, false},
	}
	for _, c := range cases {
		if got := needsMarkStepDone(c.params, c.commands); got != c.want {
			t.Errorf("%s: needsMarkStepDone = %v, want %v", c.name, got, c.want)
		}
	}
}
