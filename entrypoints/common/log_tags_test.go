package common

import (
	"errors"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/logs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
)

var taskStepCommands = []commands_enums.Type{
	commands_enums.CheckoutRepo, commands_enums.MaterializeContext, commands_enums.RunAgentStep,
	commands_enums.RunReviewStage, commands_enums.RunQAStage, commands_enums.CommitAndPush, commands_enums.OpenPullRequest,
}

func TestIsTaskStepJob(t *testing.T) {
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
		{"production QA", task, []commands_enums.Type{commands_enums.CheckoutRepo, commands_enums.MaterializeContext}, false},
		{"session", session, taskStepCommands, false},
		{"deployment", map[string]interface{}{}, []commands_enums.Type{commands_enums.CheckoutRepo}, false},
		{"deployment with agent step", map[string]interface{}{}, taskStepCommands, false},
	}
	for _, c := range cases {
		if got := isTaskStepJob(c.params, c.commands); got != c.want {
			t.Errorf("%s: isTaskStepJob = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStageForCommand(t *testing.T) {
	want := map[commands_enums.Type]string{
		commands_enums.CheckoutRepo:       logs.LogStageSetup,
		commands_enums.MaterializeContext: logs.LogStageSetup,
		commands_enums.RunAgentStep:       logs.LogStageImplement,
		commands_enums.RunReviewStage:     logs.LogStageReview,
		commands_enums.RunQAStage:         logs.LogStageQA,
		commands_enums.CommitAndPush:      logs.LogStagePR,
		commands_enums.OpenPullRequest:    logs.LogStagePR,
	}
	for cmd, stage := range want {
		if got, ok := stageForCommand(cmd); !ok || got != stage {
			t.Errorf("stageForCommand(%v) = (%q, %v), want %q", cmd, got, ok, stage)
		}
	}
	if got, ok := stageForCommand(commands_enums.BuildDockerImage); ok {
		t.Errorf("stageForCommand(BuildDockerImage) = %q, want no change", got)
	}
}

func TestHandleLogEndKinds(t *testing.T) {
	var lines []loggers.JobLog
	w := loggers.NewJobLogsWriter(true, func(l loggers.JobLog) { lines = append(lines, l) })
	w.SetStage(logs.LogStageReview)
	handleLogEnd(errors.New("boom"), "job-1", w)
	w.SetStage(logs.LogStagePR)
	handleLogEnd(nil, "job-1", w)
	w.Close()
	want := []loggers.JobLog{
		{Message: "=== Review ===", Stage: "review", Kind: "section"},
		{Message: "Error in executing - job-1 - boom", Stage: "review", Kind: "error"},
		{Message: "=== Pull request ===", Stage: "pr", Kind: "section"},
		{Message: "Successfully executed - job-1", Stage: "pr", Kind: "done"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	for i := range want {
		g := lines[i]
		if g.Message != want[i].Message || g.Stage != want[i].Stage || g.Kind != want[i].Kind || g.Group != nil {
			t.Errorf("line %d = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestHandleLogEndUntagged(t *testing.T) {
	var lines []loggers.JobLog
	w := loggers.NewJobLogsWriter(false, func(l loggers.JobLog) { lines = append(lines, l) })
	handleLogEnd(nil, "job-1", w)
	w.Close()
	if len(lines) != 1 || lines[0].Message != "Successfully executed - job-1" || lines[0].Stage != "" || lines[0].Kind != "" {
		t.Fatalf("lines = %+v", lines)
	}
}
