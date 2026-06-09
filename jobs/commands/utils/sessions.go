package utils

import (
	"fmt"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// IsSessionMode reports whether the Job is an interactive Assistant session.
// CheckoutRepo branches on this to take the read-only clone path
// (runForSession). Keys on SessionUUID presence (stamped by
// RunAssistantSessionBehavior); a session Job carries no TaskID, so it won't
// match IsTasksMode either.
func IsSessionMode(parameters map[string]interface{}) bool {
	v, err := jobs.GetParameterValue[string](parameters, parameters_enums.SessionUUID)
	return err == nil && len(v) > 0
}

// GetSessionRepositoriesBaseDir is the on-disk base under which an interactive
// Assistant session's checked-out repos live, bind-mounted into agentbox as
// /work. Keyed by (orgID, sessionJobID): a session has exactly one long-lived
// Job, so the Job id is a stable per-session key that CheckoutRepo and
// RunAssistantSession agree on without passing the path as a parameter.
func GetSessionRepositoriesBaseDir(orgID, sessionJobID string) string {
	return fmt.Sprintf("/tmp/%s/sessions/%s", orgID, sessionJobID)
}

// GetSessionRepositoryDir is one repo's clone path under the session base dir,
// as <baseDir>/<idx>-<name> — mirrors GetTaskRepositoryDir so a session lays its
// repos out identically to a Task (the agent's cwd is /work; each repo is an
// <idx>-<name> subdirectory).
func GetSessionRepositoryDir(orgID, sessionJobID string, idx int, name string) string {
	return fmt.Sprintf("%s/%d-%s", GetSessionRepositoriesBaseDir(orgID, sessionJobID), idx, name)
}
