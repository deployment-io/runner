package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/deployment-io/deployment-runner-kit/sessions"
	runnerclient "github.com/deployment-io/deployment-runner/client"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// getSessionTaskContext fetches an Ask session's Task record from
// deployment-server. A seam so tests can replace the RPC.
var getSessionTaskContext = func(jobID, orgID string) (sessions.GetTaskContextReplyV1, error) {
	return runnerclient.Get().GetSessionTaskContext(jobID, orgID)
}

// taskContextRetryDelay is the wait between attempts to fetch the record; a var
// so tests don't wait.
var taskContextRetryDelay = 2 * time.Second

const (
	// taskContextAttempts is how many times the record is fetched before the
	// session runs as a planning session.
	taskContextAttempts = 3
	// taskContextDirRel is where the record is written, relative to the
	// session base dir — the agent's /work/context/task. MaterializeContext
	// (which owns /work/context) ran before this command, so nothing removes it.
	taskContextDirRel = "context/task"
	// taskContextLogTruncatedLine starts a run's or a delivery's log.txt when
	// deployment-server left its oldest lines out.
	taskContextLogTruncatedLine = "[deployment.io: this log was cut to its newest 1 MiB]\n"
)

// prepareTaskContext returns the system prompt for the session: for an Ask
// session (its Task's record found), it writes the record under
// <workDirHost>/context/task and returns askPrompt; otherwise planModePrompt.
// A record that can't be fetched after taskContextAttempts, or can't be
// written, logs one line and falls back to planModePrompt — an older
// deployment-server without the RPC lands here too.
func prepareTaskContext(orgID, jobID, workDirHost string, logsWriter io.Writer) string {
	reply, err := fetchTaskContext(orgID, jobID)
	if err == nil && reply.Found {
		if err = writeTaskContext(filepath.Join(workDirHost, taskContextDirRel), reply); err == nil {
			return askPrompt
		}
	}
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Ask session: couldn't load the Task's record (%s); running as a planning session\n", err))
	}
	return planModePrompt
}

func fetchTaskContext(orgID, jobID string) (sessions.GetTaskContextReplyV1, error) {
	var err error
	for attempt := 1; attempt <= taskContextAttempts; attempt++ {
		var reply sessions.GetTaskContextReplyV1
		reply, err = getSessionTaskContext(jobID, orgID)
		if err == nil {
			return reply, nil
		}
		if attempt < taskContextAttempts {
			time.Sleep(taskContextRetryDelay)
		}
	}
	return sessions.GetTaskContextReplyV1{}, err
}

// taskContextFile is task.json.
type taskContextFile struct {
	ID                 string                      `json:"id"`
	Title              string                      `json:"title"`
	Description        string                      `json:"description"`
	AcceptanceCriteria []string                    `json:"acceptance_criteria"`
	Spec               json.RawMessage             `json:"spec"`
	Repositories       []taskContextRepositoryFile `json:"repositories"`
	Terminus           string                      `json:"terminus"`
	ProductionQA       *taskContextProductionQA    `json:"production_qa,omitempty"`
}

type taskContextRepositoryFile struct {
	Name             string `json:"name"`
	BaseBranch       string `json:"base_branch"`
	TaskBranch       string `json:"task_branch"`
	CheckedOutBranch string `json:"checked_out_branch"`
	PRURL            string `json:"pr_url"`
	PRState          string `json:"pr_state"`
	// DeployTarget is omitted when the repository has none.
	DeployTarget *taskContextDeployTarget `json:"deploy_target,omitempty"`
	MergedAt     int64                    `json:"merged_at,omitempty"`
	Delivery     *taskContextDelivery     `json:"delivery,omitempty"`
	// DeliveryLog is delivery/<n>/log.txt's path relative to the record, set
	// only when that file is written.
	DeliveryLog string `json:"delivery_log,omitempty"`
}

type taskContextDeployTarget struct {
	Deployment  string `json:"deployment"`
	Environment string `json:"environment"`
	Type        string `json:"type"`
}

type taskContextDelivery struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
	Commit string `json:"commit"`
	// MissingVariables is omitted when empty: gob delivers an empty list as
	// nil.
	MissingVariables   []string `json:"missing_variables,omitempty"`
	FollowedAutoDeploy bool     `json:"followed_auto_deploy"`
	StartedAt          int64    `json:"started_at"`
	EndedAt            int64    `json:"ended_at"`
}

type taskContextProductionQA struct {
	State      string `json:"state"`
	Reason     string `json:"reason"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	NotChecked int    `json:"not_checked"`
	NoCriteria bool   `json:"no_criteria"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at"`
	// Run is the production QA run's directory, taskContextProductionQADir,
	// set only when it is written.
	Run string `json:"run,omitempty"`
}

// taskContextProductionQADir is the production QA run's directory in the
// record, written like a run.
const taskContextProductionQADir = "production-qa"

// taskDeliveryLogPath is the n-th repository's delivery log, relative to the
// record.
func taskDeliveryLogPath(n int) string {
	return filepath.Join("delivery", strconv.Itoa(n), "log.txt")
}

// writeTaskContext replaces dir with the record: task.json,
// runs/<n>-<jobID>/ per run, n = 1 for the oldest, delivery/<n>/log.txt per
// repository with a delivery log, n its index in task.json's repositories, and
// production-qa/ (written like a run) when the production QA has a run. Everything is
// world-readable (files 0644, dirs 0755), which is all the read-only agent
// needs.
func writeTaskContext(dir string, reply sessions.GetTaskContextReplyV1) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0755); err != nil {
		return err
	}
	task, err := json.MarshalIndent(taskContextFileFor(reply.Task), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), append(task, '\n'), 0644); err != nil {
		return err
	}
	for i, run := range reply.Runs {
		if err := writeTaskRun(filepath.Join(dir, "runs", taskRunDirName(i+1, run.JobID)), run); err != nil {
			return err
		}
	}
	for i, r := range reply.Task.Repositories {
		if r.DeliveryLog == "" {
			continue
		}
		path := filepath.Join(dir, taskDeliveryLogPath(i))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		log := r.DeliveryLog
		if r.DeliveryLogTruncated {
			log = taskContextLogTruncatedLine + log
		}
		if err := os.WriteFile(path, []byte(log), 0644); err != nil {
			return err
		}
	}
	if qa := reply.Task.ProductionQA; qa != nil && qa.Run != nil {
		if err := writeTaskRun(filepath.Join(dir, taskContextProductionQADir), *qa.Run); err != nil {
			return err
		}
	}
	return nil
}

func taskContextFileFor(t sessions.TaskContextV1) taskContextFile {
	f := taskContextFile{
		ID:                 t.ID,
		Title:              t.Title,
		Description:        t.Description,
		AcceptanceCriteria: t.AcceptanceCriteria,
		Spec:               json.RawMessage("null"),
		Repositories:       []taskContextRepositoryFile{},
		Terminus:           t.Terminus,
	}
	if f.AcceptanceCriteria == nil {
		f.AcceptanceCriteria = []string{}
	}
	if t.SpecJSON != "" && json.Valid([]byte(t.SpecJSON)) {
		f.Spec = json.RawMessage(t.SpecJSON)
	}
	for i, r := range t.Repositories {
		repo := taskContextRepositoryFile{
			Name:             r.Name,
			BaseBranch:       r.BaseBranch,
			TaskBranch:       r.TaskBranch,
			CheckedOutBranch: r.CheckedOutBranch,
			PRURL:            r.PRURL,
			PRState:          r.PRState,
			MergedAt:         r.MergedAt,
		}
		if r.DeployTargetName != "" || r.DeployEnvironmentName != "" || r.DeployTargetType != "" {
			repo.DeployTarget = &taskContextDeployTarget{
				Deployment:  r.DeployTargetName,
				Environment: r.DeployEnvironmentName,
				Type:        r.DeployTargetType,
			}
		}
		if d := r.Delivery; d != nil {
			repo.Delivery = &taskContextDelivery{
				State:              d.State,
				Reason:             d.Reason,
				Commit:             d.CommitHash,
				MissingVariables:   d.MissingVariables,
				FollowedAutoDeploy: d.FollowedAutoDeploy,
				StartedAt:          d.StartedAt,
				EndedAt:            d.EndedAt,
			}
		}
		if r.DeliveryLog != "" {
			repo.DeliveryLog = filepath.ToSlash(taskDeliveryLogPath(i))
		}
		f.Repositories = append(f.Repositories, repo)
	}
	if qa := t.ProductionQA; qa != nil {
		f.ProductionQA = &taskContextProductionQA{
			State:      qa.State,
			Reason:     qa.Reason,
			Passed:     qa.Passed,
			Failed:     qa.Failed,
			NotChecked: qa.NotChecked,
			NoCriteria: qa.NoCriteria,
			StartedAt:  qa.StartedAt,
			EndedAt:    qa.EndedAt,
		}
		if qa.Run != nil {
			f.ProductionQA.Run = taskContextProductionQADir
		}
	}
	return f
}

// taskRunDirName is "<n>-<jobID>"; just "<n>" for a Job ID that isn't one,
// so a server value never becomes a path.
func taskRunDirName(n int, jobID string) string {
	if _, err := primitive.ObjectIDFromHex(jobID); err != nil {
		return strconv.Itoa(n)
	}
	return strconv.Itoa(n) + "-" + jobID
}

func writeTaskRun(dir string, run sessions.TaskRunContextV1) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	output := run.OutputJSON
	if output == "" {
		output = "{}"
	}
	log := run.Log
	if run.LogTruncated {
		log = taskContextLogTruncatedLine + log
	}
	files := []struct{ name, content string }{
		{"output.json", output},
		{"events.jsonl", run.EventsJSONL},
		{"log.txt", log},
	}
	if run.RerunFeedback != "" {
		files = append(files, struct{ name, content string }{"rerun-feedback.txt", run.RerunFeedback})
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), 0644); err != nil {
			return err
		}
	}
	return nil
}
