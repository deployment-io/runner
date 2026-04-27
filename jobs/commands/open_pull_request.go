package commands

import (
	"fmt"
	"io"
)

// OpenPullRequest is a Tasks-only runner command. Final command in the
// Step Job sequence: opens PRs across all TaskJobContext.Entries with
// HasChanges=true and persists the PR URL/number into the JobOutput
// repositories block.
//
// Phase 4 stub: succeeds without doing anything so the Step Job's
// command sequence [CheckoutRepo, CommitAndPush, OpenPullRequest]
// completes cleanly even though the real PR-opening implementation
// hasn't landed yet. Chunk #4 replaces this with provider-dispatched
// REST calls (GitHub / GitLab / BitBucket).
type OpenPullRequest struct{}

func (opr *OpenPullRequest) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	io.WriteString(logsWriter, fmt.Sprintf("OpenPullRequest stub (Phase 4) — no PRs opened. Chunk #4 will add provider-dispatched REST calls.\n"))
	// TODO(chunk #4): per-repo PR opening + JobOutput merge of pr_url/pr_number.
	// TODO(chunk #4): MarkStepDone(parameters, nil) here on success — final
	//                 command in the Step Job is responsible for cleanup.
	return parameters, nil
}
