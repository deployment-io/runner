package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/context_pack_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// MaterializeContext pulls the org's stored context from the control plane and writes it under
// /work/context for the agent to read. It runs BEFORE CheckoutRepo and creates its own dir, so
// it works even when no repos are checked out (the repo-less Assistant plan session, where the
// catalog is the input to repo discovery). Failures degrade gracefully — a missing or
// unavailable context never fails the job; the agent falls back to live discovery.
type MaterializeContext struct{}

func (m *MaterializeContext) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	orgID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Skipping context materialization (organization id missing): %s\n", err))
		return parameters, nil
	}
	contextDir, ok := contextDirFor(orgID, parameters)
	if !ok {
		// Not a Task/Session run — there's no /work to materialize into.
		return parameters, nil
	}

	// v1: org-wide context only (the repo catalog). The server owns the Org scope's id, so an
	// empty/best-effort id here is fine. Environment/cluster scopes ride in with the infra source.
	scopes := []context_pack.Scope{{Level: context_pack_enums.Org, ID: orgID}}
	files, err := client.Get().MaterializeContext(orgID, scopes)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Context unavailable, continuing without it: %s\n", err))
		return parameters, nil
	}
	if len(files) == 0 {
		io.WriteString(logsWriter, "No context to materialize (none built yet); continuing.\n")
		return parameters, nil
	}

	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not create context dir, continuing without it: %s\n", err))
		return parameters, nil
	}
	written := 0
	for _, f := range files {
		// Anchor under contextDir; filepath.Clean("/"+Path) defends against a stray ".." in Path.
		dest := filepath.Join(contextDir, filepath.Clean("/"+f.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("  skipping %s: %s\n", f.Path, err))
			continue
		}
		if err := os.WriteFile(dest, []byte(f.Content), 0o644); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("  skipping %s: %s\n", f.Path, err))
			continue
		}
		written++
	}
	// Make the tree readable by the agentbox (UID 1000) through the bind mount.
	if err := chownTreeToAgentbox(contextDir); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not chown context dir: %s\n", err))
	}
	io.WriteString(logsWriter, fmt.Sprintf("Materialized %d context file(s) into /work/context\n", written))
	return parameters, nil
}

// contextDirFor returns the host path that bind-mounts to /work/context for a Task or Session
// run, and whether this is such a run.
func contextDirFor(orgID string, parameters map[string]interface{}) (string, bool) {
	if commandUtils.IsTasksMode(parameters) {
		taskID, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
		if err != nil {
			return "", false
		}
		return filepath.Join(commandUtils.GetTaskRepositoriesBaseDir(orgID, taskID), "context"), true
	}
	if commandUtils.IsSessionMode(parameters) {
		jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
		if err != nil {
			return "", false
		}
		return filepath.Join(commandUtils.GetSessionRepositoriesBaseDir(orgID, jobID), "context"), true
	}
	return "", false
}
