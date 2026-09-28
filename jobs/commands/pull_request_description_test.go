package commands

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// The pull request describes the change AS IT FINALLY STANDS. The implement
// run names it; a kept fix run describes it, replacing the implement run's
// account rather than being appended after it under a label.
func TestAKeptFixRunTitlesNothingAndDescribesEverything(t *testing.T) {
	parameters := map[string]interface{}{}
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", PRTitle: "Add a /debug/env route",
		ChangesSummary: "Adds /debug/env, which returns the full environment. I left it ungated.\n\nSay the word and I'll add either.",
	}); err != nil {
		t.Fatalf("merge implement run: %s", err)
	}
	if err := recordFixRunResult(parameters, agentResult{
		Status: "success", PRTitle: "Gate /debug/env behind a token and redact secret env values",
		ChangesSummary: "Adds /debug/env, gated behind a token, with secret values redacted.",
	}, nil, io.Discard); err != nil {
		t.Fatalf("record fix run: %s", err)
	}

	data := jobOutputFor(t, parameters)
	if data.Agent.PRTitle != "Add a /debug/env route" {
		t.Errorf("pr_title = %q, want the implement run's — a fix run's title names its own errand", data.Agent.PRTitle)
	}
	if data.Agent.ChangesSummary != "Adds /debug/env, gated behind a token, with secret values redacted." {
		t.Errorf("changes summary = %q, want the fix run's alone", data.Agent.ChangesSummary)
	}
	if strings.Contains(data.Agent.ChangesSummary, "[Review fixes]") {
		t.Errorf("the two summaries were joined under a label: %q", data.Agent.ChangesSummary)
	}
}

// The rule is "the FIRST non-empty title stands", not "the implement run's
// title stands": an implement run from an agentbox image that emits no
// pr_title leaves the fix run's the only one there is.
func TestAFixRunFillsATitleNoRunRecorded(t *testing.T) {
	parameters := map[string]interface{}{}
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", ChangesSummary: "Added the login endpoint.",
	}); err != nil {
		t.Fatalf("merge implement run: %s", err)
	}
	if err := recordFixRunResult(parameters, agentResult{
		Status: "success", PRTitle: "Add OAuth login to auth-service",
		ChangesSummary: "Adds the login endpoint with a session check.",
	}, nil, io.Discard); err != nil {
		t.Fatalf("record fix run: %s", err)
	}
	if got := jobOutputFor(t, parameters).Agent.PRTitle; got != "Add OAuth login to auth-service" {
		t.Errorf("pr_title = %q, want the fix run's — no earlier run recorded one", got)
	}
}

// A fix run that said nothing is not a fix run that erased the description.
func TestAnEmptyFixSummaryLeavesTheDescriptionStanding(t *testing.T) {
	parameters := map[string]interface{}{}
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", PRTitle: "Add OAuth login", ChangesSummary: "Added the login endpoint.",
	}); err != nil {
		t.Fatalf("merge implement run: %s", err)
	}
	if err := recordFixRunResult(parameters, agentResult{
		Status: "success", ChangesSummary: "  \n ",
	}, nil, io.Discard); err != nil {
		t.Fatalf("record fix run: %s", err)
	}
	if got := jobOutputFor(t, parameters).Agent.ChangesSummary; got != "Added the login endpoint." {
		t.Errorf("changes summary = %q, want the implement run's left standing", got)
	}
}

// A fix run whose work is rolled back must not describe the pull request.
// Regression guard on recordFixRunResult's two gates — the status and the
// verify gate — with the usage still counted on both.
func TestARolledBackFixRunLeavesTheTitleAndDescription(t *testing.T) {
	cases := []struct {
		name   string
		result agentResult
	}{
		{
			name: "the run did not succeed",
			result: agentResult{Status: "failure", Turns: 5, PRTitle: "fix: check the caller's session",
				ChangesSummary: "Adds the session check.", TokenUsage: tokenUsage{InputTokens: 50}},
		},
		{
			name: "the run succeeded but failed its verify gate",
			result: agentResult{Status: "success", Turns: 5, PRTitle: "fix: check the caller's session",
				ChangesSummary: "Adds the session check.", TokenUsage: tokenUsage{InputTokens: 50},
				VerifyResult: &verifyResult{Ran: true, Passed: false, Command: "go test ./...",
					StderrTail: "auth_test.go:31: want 200, got 500"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parameters := map[string]interface{}{}
			if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
				Status: "success", Turns: 10, PRTitle: "Add session handling",
				ChangesSummary: "Adds session handling.", TokenUsage: tokenUsage{InputTokens: 100},
				VerifyResult: &verifyResult{Ran: true, Passed: true, Command: "go test ./..."},
			}); err != nil {
				t.Fatalf("merge implement run: %s", err)
			}
			if err := recordFixRunResult(parameters, tc.result, nil, io.Discard); err == nil {
				t.Fatal("a rolled-back fix run was reported as kept")
			}
			data := jobOutputFor(t, parameters)
			if data.Agent.PRTitle != "Add session handling" {
				t.Errorf("pr_title = %q, want the implement run's", data.Agent.PRTitle)
			}
			if data.Agent.ChangesSummary != "Adds session handling." {
				t.Errorf("changes summary = %q, want the implement run's", data.Agent.ChangesSummary)
			}
			if data.Agent.VerifyResult == nil || !data.Agent.VerifyResult.Passed {
				t.Errorf("verify result = %+v, want the implement run's passing one", data.Agent.VerifyResult)
			}
			if data.Agent.Turns != 15 || data.Agent.TokenUsage.InputTokens != 150 {
				t.Errorf("turns %d, input tokens %d; want the rolled-back run's usage still counted (15, 150)",
					data.Agent.Turns, data.Agent.TokenUsage.InputTokens)
			}
		})
	}
}

// Replacing the description changes nothing about what the two runs cost: the
// tokens, turns, cost, denied hosts and files are one Step's.
func TestUsageStillSumsAcrossTheImplementAndFixRuns(t *testing.T) {
	parameters := map[string]interface{}{}
	implementCost, fixCost := 0.40, 0.20
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", Turns: 20, PRTitle: "Add OAuth login",
		ChangesSummary: "Added the login endpoint.",
		FilesChanged:   []string{"0-acme/api/handler.go"},
		DeniedHosts:    []string{"pypi.example.com"},
		TokenUsage:     tokenUsage{InputTokens: 1000, OutputTokens: 100},
		CostUSD:        &implementCost,
	}); err != nil {
		t.Fatalf("merge implement run: %s", err)
	}
	if err := recordFixRunResult(parameters, agentResult{
		Status: "success", Turns: 6,
		ChangesSummary: "Adds the login endpoint, with the caller's session checked.",
		FilesChanged:   []string{"0-acme/api/handler.go", "0-acme/api/auth.go"},
		DeniedHosts:    []string{"registry.internal"},
		TokenUsage:     tokenUsage{InputTokens: 800, OutputTokens: 80},
		CostUSD:        &fixCost,
	}, nil, io.Discard); err != nil {
		t.Fatalf("record fix run: %s", err)
	}

	data := jobOutputFor(t, parameters)
	if data.Agent.Turns != 26 {
		t.Errorf("turns = %d, want 20+6", data.Agent.Turns)
	}
	if data.Agent.TokenUsage.InputTokens != 1800 || data.Agent.TokenUsage.OutputTokens != 180 {
		t.Errorf("token usage = %+v, want both runs counted", data.Agent.TokenUsage)
	}
	if len(data.Agent.FilesChanged) != 2 {
		t.Errorf("files changed = %v, want the union", data.Agent.FilesChanged)
	}
	if len(data.Agent.DeniedHosts) != 2 {
		t.Errorf("denied hosts = %v, want the union", data.Agent.DeniedHosts)
	}
	if data.Agent.CostUSD == nil || fmt.Sprintf("%.2f", *data.Agent.CostUSD) != "0.60" {
		t.Errorf("agent-reported cost = %v, want both runs summed", data.Agent.CostUSD)
	}
}

// The fix run is asked for a summary that can stand as the pull request's
// description, since its own will replace the implement run's.
func TestMustFixPromptAsksForADescriptionOfTheWholeChange(t *testing.T) {
	prompt := buildMustFixPrompt("Add a /debug/env route.", []reviewFindingOutput{{
		Parameter: "security", Severity: "high", Location: "handler.go:41",
		What: "the route returns every environment variable", Why: "secrets leak to any caller", MustFix: true,
	}})
	const instruction = "Your final summary becomes the pull request description, replacing the earlier one. Describe the whole change as it now stands — what the Step changed and why, including these fixes — not only what you changed in this run. Write it for a reviewer reading the pull request: do not address the user, ask questions, or offer further work."
	if !strings.Contains(prompt, instruction) {
		t.Errorf("the fix prompt does not carry the summary instruction:\n%s", prompt)
	}
	findingIdx := strings.Index(prompt, "the route returns every environment variable")
	if findingIdx == -1 || strings.Index(prompt, instruction) < findingIdx {
		t.Errorf("the summary instruction must come after the findings:\n%s", prompt)
	}
}
