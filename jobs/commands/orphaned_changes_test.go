package commands

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// paramsWithFilesChanged builds a JobOutput envelope carrying the agent's
// files_changed list, as RunAgentStep would have written before CommitAndPush.
func paramsWithFilesChanged(t *testing.T, files []string) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(jobOutputData{SchemaVersion: 1, Agent: &agentOutput{FilesChanged: files}})
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]interface{}{}
	jobs.SetParameterValue[string](params, parameters_enums.JobOutput, string(b))
	return params
}

// The failure this guards: the agent wrote files (files_changed non-empty) but
// they landed outside every repo (all repos clean) — so nothing commits and no
// PR opens. Must become a visible error, not a silent success.
func TestDetectOrphanedChanges_ErrorsWhenChangesReportedButNoRepoChanged(t *testing.T) {
	params := paramsWithFilesChanged(t, []string{"/work/HELLO_OPENCODE.md"})
	repos := []repoOutput{{Name: "dashboard", HasChanges: false}}
	err := detectOrphanedChanges(params, repos, io.Discard)
	if err == nil {
		t.Fatal("expected an error when the agent reported files but no repo changed")
	}
	if !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("error should explain the out-of-repo write: %v", err)
	}
}

func TestDetectOrphanedChanges_NoErrorWhenARepoChanged(t *testing.T) {
	params := paramsWithFilesChanged(t, []string{"/work/0-org/repo/x.go"})
	repos := []repoOutput{{Name: "repo", HasChanges: true}}
	if err := detectOrphanedChanges(params, repos, io.Discard); err != nil {
		t.Errorf("no error expected when a repo changed: %v", err)
	}
}

func TestDetectOrphanedChanges_NoErrorOnGenuineNoOp(t *testing.T) {
	// Empty files_changed = the agent legitimately made no change; must not error.
	params := paramsWithFilesChanged(t, nil)
	repos := []repoOutput{{Name: "repo", HasChanges: false}}
	if err := detectOrphanedChanges(params, repos, io.Discard); err != nil {
		t.Errorf("genuine no-op (empty files_changed) must not error: %v", err)
	}
}
