package commands

import (
	"encoding/json"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// qaResult mirrors agentbox's result.json qa_result object (agentbox 1.9.32,
// docs/CONTRACT.md "qa_result"). agentbox writes it on EVERY qa-mode outcome,
// failures included; its absence means the image did not run QA mode at all.
// Every field is a string, as for review_result: agentbox imports nothing of
// ours.
type qaResult struct {
	Status       string          `json:"status"`
	StatusReason string          `json:"status_reason,omitempty"`
	NoCriteria   bool            `json:"no_criteria,omitempty"`
	Criteria     []qaCriterion   `json:"criteria,omitempty"`
	Findings     []reviewFinding `json:"findings,omitempty"`
	Observations []string        `json:"observations,omitempty"`
}

// qaCriterion is one acceptance criterion as QA reported it: passed, failed or
// not_checked, with QA's reason.
type qaCriterion struct {
	Criterion string `json:"criterion"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// qaOutput is the QA stage's record on the Job output (key "qa"). Status,
// StatusReason and NoCriteria are the FINAL result's: the gate's when nothing
// ran, else the last round's. OpenPullRequest renders it; it never holds the
// pull request.
type qaOutput struct {
	Status       string `json:"status"`
	StatusReason string `json:"status_reason,omitempty"`
	NoCriteria   bool   `json:"no_criteria,omitempty"`
	// Repositories is the QA_REPOSITORIES list: the changed repositories the
	// runner can build a preview of, in work-dir order — on the stack path, every
	// repository whose site preview the runner deployed.
	Repositories []string `json:"repositories,omitempty"`
	// Backends are the changed repositories (on the stack path, every
	// repository) the runner deployed as backend previews before each round, in
	// work-dir order.
	Backends []string        `json:"backends,omitempty"`
	Rounds   []qaRoundOutput `json:"rounds,omitempty"`
	// FixedByQA is the criteria that failed in round 1 and passed in the final
	// round, after a fix run.
	FixedByQA []string `json:"fixed_by_qa,omitempty"`
	// FixError is why the fix run did not finish (the tree was put back), or
	// empty; FixNotAttempted is true when its undo copy could not be taken.
	FixError        string `json:"fix_error,omitempty"`
	FixNotAttempted bool   `json:"fix_not_attempted,omitempty"`
	// StoppedNoChange is true when the fix run finished and changed no file;
	// DeclinedExplanation is its summary — why.
	StoppedNoChange     bool   `json:"stopped_no_change,omitempty"`
	DeclinedExplanation string `json:"declined_explanation,omitempty"`
}

// qaRoundOutput is one QA run.
type qaRoundOutput struct {
	Round        int                   `json:"round"`
	Status       string                `json:"status"`
	StatusReason string                `json:"status_reason,omitempty"`
	NoCriteria   bool                  `json:"no_criteria,omitempty"`
	Criteria     []qaCriterion         `json:"criteria,omitempty"`
	Findings     []reviewFindingOutput `json:"findings,omitempty"`
	Observations []string              `json:"observations,omitempty"`
	AgentType    string                `json:"agent_type,omitempty"`
	Model        string                `json:"model,omitempty"`
	Turns        int                   `json:"turns,omitempty"`
	TokenUsage   tokenUsage            `json:"token_usage"`
	CostUSD      *float64              `json:"cost_usd,omitempty"`
	// MergeResults is, per repository the round built a preview of, whether
	// the preview was built from the merge result with the base branch or the
	// branch as built. Omitted when the round built no preview.
	MergeResults []qaMergeResultOutput `json:"merge_results,omitempty"`
	// Wiring is, on the stack path, one entry per (site, backend) pair of the
	// round's previews: whether the site's preview was built to call the
	// backend's. Variable names only, never a value or a path.
	Wiring []qaWiringOutput `json:"wiring,omitempty"`
}

// Values of qaWiringOutput.Reason.
const (
	qaWiringNoLink        = "no link"
	qaWiringBackendFailed = "backend failed"
	qaWiringLookupFailed  = "lookup failed"
	qaWiringSingleOrigin  = "single origin"
)

// qaWiringOutput is how a stack-path QA round connected one site preview
// (Frontend) to one backend preview (API), both named as their repositories.
type qaWiringOutput struct {
	Frontend  string `json:"frontend"`
	API       string `json:"api"`
	Connected bool   `json:"connected"`
	// FrontendVariables are the site's variables set to the backend preview's
	// address; APIVariables the backend's set to (or, for "single origin", the
	// one already holding another site's) origin. Names, sorted.
	FrontendVariables []string `json:"frontend_variables,omitempty"`
	APIVariables      []string `json:"api_variables,omitempty"`
	// Reason is why the site isn't connected (qaWiring*), "" when it is.
	Reason string `json:"reason,omitempty"`
}

// finalRound is the round whose result is the stage's, or nil when the gate
// decided without a run.
func (q *qaOutput) finalRound() *qaRoundOutput {
	if q == nil || len(q.Rounds) == 0 {
		return nil
	}
	return &q.Rounds[len(q.Rounds)-1]
}

// mergeQAIntoJobOutput writes the qa block onto the accumulated envelope.
func mergeQAIntoJobOutput(parameters map[string]interface{}, out *qaOutput) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.QA = out
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// readQAFromJobOutput pulls the qa block back off the envelope. Nil when the
// stage did not run, or the gate found no changed repository.
func readQAFromJobOutput(parameters map[string]interface{}) *qaOutput {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil
	}
	return data.QA
}
