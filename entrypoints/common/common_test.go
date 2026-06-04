package common

import (
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// getJobResult must carry merged JobOutput into the completion result on the
// failure/stop paths too — otherwise a failed or user-stopped Step Job loses
// the agent block (token usage / cost / changes summary) that RunAgentStep
// merged into parameters before returning the error.
func TestGetJobResult_CarriesJobOutputWhenParametersPassed(t *testing.T) {
	params := map[string]interface{}{}
	jobs.SetParameterValue[string](params, parameters_enums.JobOutput,
		`{"agent":{"token_usage":{"input_tokens":10},"cost_usd":0.02}}`)

	got := getJobResult(pendingJobType{jobID: "j1", organizationID: "o1"}, "stopped by user", params)

	if got.output == "" {
		t.Fatal("expected merged JobOutput to be carried into the completion result, got empty")
	}
	if got.error != "stopped by user" {
		t.Errorf("error = %q, want it preserved", got.error)
	}
	if got.id != "j1" {
		t.Errorf("id = %q, want j1", got.id)
	}
}

// nil parameters (e.g. a pre-run failure with nothing to preserve) still
// yields empty output — the omitempty-friendly default.
func TestGetJobResult_NilParametersYieldsNoOutput(t *testing.T) {
	got := getJobResult(pendingJobType{jobID: "j1"}, "boom", nil)
	if got.output != "" {
		t.Errorf("nil parameters should yield empty output, got %q", got.output)
	}
}
