package utils

import "fmt"

// GetSessionRepositoriesBaseDir is the on-disk base under which an interactive
// Assistant session's checked-out repo(s) live, bind-mounted into agentbox as
// /work. Keyed by (orgID, sessionJobID): a session has exactly one long-lived
// Job, so the Job id is a stable per-session key that CheckoutRepo and
// RunAssistantSession agree on without passing the path as a parameter.
func GetSessionRepositoriesBaseDir(orgID, sessionJobID string) string {
	return fmt.Sprintf("/tmp/%s/sessions/%s", orgID, sessionJobID)
}
