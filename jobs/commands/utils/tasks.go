package utils

import (
	"encoding/json"
	"fmt"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// IsTasksMode reports whether the Job is running for a Task. CheckoutRepo
// branches on this to take the multi-repo Tasks path instead of the
// existing single-repo deployment path. Checks TaskID presence (the
// canonical Task-job indicator, mirroring deployment-server's
// !job.TaskID.IsZero() dispatch); the multi-repo Repositories parameter
// is validated inside ParseTaskJobContext.
func IsTasksMode(parameters map[string]interface{}) bool {
	v, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
	return err == nil && len(v) > 0
}

// GetTaskRepositoriesBaseDir is the on-disk base under which a Task's
// per-repo working directories live. Computed deterministically from
// (orgID, taskID) so all commands inside the same Step Job agree on
// the layout without passing it as a parameter.
//
// Phase 5 bind-mounts this path into the agentbox container as /work.
func GetTaskRepositoriesBaseDir(orgID, taskID string) string {
	return fmt.Sprintf("/tmp/%s/%s", orgID, taskID)
}

// GetTaskRepositoryDir is the per-repo working directory for index idx
// in the Task's repo list. Index prefix avoids collisions when two repos
// share the same name across orgs.
func GetTaskRepositoryDir(orgID, taskID string, idx int, name string) string {
	return fmt.Sprintf("%s/%d-%s", GetTaskRepositoriesBaseDir(orgID, taskID), idx, name)
}

// TaskJobContext bundles the per-Task-Job inputs all Tasks runner commands
// (CheckoutRepo, CommitAndPush, OpenPullRequest) need. Parsed once at the
// top of each command's Run() and passed into the per-repo loop.
type TaskJobContext struct {
	OrganizationID string
	TaskID         string
	TaskTitle      string
	DashboardURL   string // optional — empty if APP_URL wasn't set at job creation
	StepIndex      int64
	BranchName     string
	Entries        []tasks.RepositoryEntry
}

// ParseTaskJobContext reads and validates the Tasks-mode parameters.
// Returns an error if any required parameter is missing or malformed.
// TaskTitle is required; DashboardURL is optional.
func ParseTaskJobContext(parameters map[string]interface{}) (TaskJobContext, error) {
	var ctx TaskJobContext
	orgID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return ctx, fmt.Errorf("organization id missing: %s", err)
	}
	taskID, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
	if err != nil {
		return ctx, fmt.Errorf("task id missing: %s", err)
	}
	taskTitle, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskTitle)
	if err != nil {
		return ctx, fmt.Errorf("task title missing: %s", err)
	}
	stepIndex, err := jobs.GetParameterValue[int64](parameters, parameters_enums.StepIndex)
	if err != nil {
		return ctx, fmt.Errorf("step index missing: %s", err)
	}
	branchName, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskBranchName)
	if err != nil {
		return ctx, fmt.Errorf("task branch name missing: %s", err)
	}
	repositoriesJSON, err := jobs.GetParameterValue[string](parameters, parameters_enums.Repositories)
	if err != nil {
		return ctx, fmt.Errorf("repositories missing: %s", err)
	}
	var entries []tasks.RepositoryEntry
	if err := json.Unmarshal([]byte(repositoriesJSON), &entries); err != nil {
		return ctx, fmt.Errorf("error unmarshalling repositories: %s", err)
	}
	if len(entries) == 0 {
		return ctx, fmt.Errorf("repositories list is empty")
	}
	dashboardURL, _ := jobs.GetParameterValue[string](parameters, parameters_enums.DashboardURL)
	ctx = TaskJobContext{
		OrganizationID: orgID,
		TaskID:         taskID,
		TaskTitle:      taskTitle,
		DashboardURL:   dashboardURL,
		StepIndex:      stepIndex,
		BranchName:     branchName,
		Entries:        entries,
	}
	return ctx, nil
}
