package commands

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// --- the reviewer's view of the Job ----------------------------------------
//
// A Task may run its REVIEW rounds on a model other than the one that
// implemented the change. The runner's whole share of that is a shallow copy of
// the Job parameters with four keys replaced, handed to the SAME env builder —
// so Bedrock vending, per-provider model rendering, the subscription swap and
// the proxy allowlist apply to the reviewer with no second code path.

// implementerJobParameters is a Job as it arrives with no reviewer named: the
// shape every Task had before reviewers existed.
func implementerJobParameters(t *testing.T) map[string]interface{} {
	t.Helper()
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[map[string]string](parameters, parameters_enums.AgentEnvVars,
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-implementer"})
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentProvider, llm_provider_enums.AnthropicDirect.Key())
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentType, "claude-code")
	jobs.SetParameterValue[string](parameters, parameters_enums.Model, "claude-sonnet-4-6")
	jobs.SetParameterValue[string](parameters, parameters_enums.StepPrompt, "implement the export endpoint")
	jobs.SetParameterValue[int64](parameters, parameters_enums.MaxTurns, 40)
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentboxImage, "deploymenthq/agentbox:1.9.13")
	return parameters
}

// withReviewer stamps the four reviewer keys the way kit and deployment-server
// do: 125/126 at Job creation, 127/128 at pickup.
func withReviewer(t *testing.T, parameters map[string]interface{}, agentType, model string,
	provider *llm_provider_enums.Provider, envVars map[string]string) map[string]interface{} {
	t.Helper()
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewAgentType, agentType)
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewModel, model)
	if provider != nil {
		jobs.SetParameterValue[string](parameters, parameters_enums.ReviewAgentProvider, provider.Key())
	}
	if envVars != nil {
		jobs.SetParameterValue[map[string]string](parameters, parameters_enums.ReviewAgentEnvVars, envVars)
	}
	return parameters
}

func reviewStageFor(t *testing.T, parameters map[string]interface{}) *reviewStage {
	t.Helper()
	view, failure := reviewerParameters(parameters)
	return &reviewStage{
		parameters:      parameters,
		reviewerParams:  view,
		reviewerFailure: failure,
		logsWriter:      io.Discard,
		participation:   participationOn,
	}
}

// The review round spawns with the REVIEWER's agent, model and credentials
// while the fix round — an implement run wearing a narrower ask — spawns with
// the implementer's. Both come out of the same builder.
func TestTheReviewSpawnsAsTheReviewerAndTheFixSpawnsAsTheImplementer(t *testing.T) {
	openAI := llm_provider_enums.OpenAIDirect
	parameters := withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", &openAI,
		map[string]string{"OPENAI_API_KEY": "sk-openai-reviewer", "CODEX_API_KEY": "sk-openai-reviewer"})
	stage := reviewStageFor(t, parameters)

	reviewEnv, err := stage.reviewSpawnEnv(1)
	if err != nil {
		t.Fatalf("reviewSpawnEnv: %s", err)
	}
	review := envMap(reviewEnv)
	if review["AGENT_TYPE"] != "codex" {
		t.Errorf("the review spawn's AGENT_TYPE = %q, want the reviewer's", review["AGENT_TYPE"])
	}
	if review["OPENAI_API_KEY"] != "sk-openai-reviewer" {
		t.Errorf("the review spawn is missing the reviewer's credentials: %v", review)
	}
	if _, present := review["ANTHROPIC_API_KEY"]; present {
		t.Error("the review spawn carries the implementer's key; the reviewer's bundle replaces it, never merges with it")
	}
	// Rendered through the same per-provider path the implementer's model goes
	// through — the point of reusing the builder rather than writing a second.
	wantModel := renderedModelFor(t, "gpt-5.5", llm_provider_enums.OpenAIDirect, llm_provider_enums.Codex)
	if review["MODEL"] != wantModel {
		t.Errorf("the review spawn's MODEL = %q, want the reviewer's rendered id %q", review["MODEL"], wantModel)
	}

	// The fix round is an IMPLEMENT run and must be untouched by any of this.
	fixEnv, err := buildAgentSpawnEnvVars(stage.parameters, io.Discard)
	if err != nil {
		t.Fatalf("the fix run's env: %s", err)
	}
	fix := envMap(applyMustFixEnv(fixEnv, "fix these findings"))
	if fix["AGENT_TYPE"] != "claude-code" {
		t.Errorf("the fix spawn's AGENT_TYPE = %q, want the implementer's", fix["AGENT_TYPE"])
	}
	if fix["ANTHROPIC_API_KEY"] != "sk-ant-implementer" {
		t.Errorf("the fix spawn lost the implementer's credentials: %v", fix)
	}
	if _, present := fix["OPENAI_API_KEY"]; present {
		t.Error("the fix spawn carries the reviewer's credentials; a fix run is an implement run")
	}
}

// With no reviewer named — every Task that never chose one, every Job written
// before reviewers existed — the review and the fix spawn identically, which
// is exactly today's behaviour.
func TestWithNoReviewerBothSpawnsAreTheImplementers(t *testing.T) {
	parameters := implementerJobParameters(t)
	if view, failure := reviewerParameters(parameters); view != nil || failure != "" {
		t.Fatalf("a Job with no reviewer produced a reviewer view (%v) / failure (%q)", view, failure)
	}
	stage := reviewStageFor(t, parameters)

	reviewEnv, err := stage.reviewSpawnEnv(1)
	if err != nil {
		t.Fatalf("reviewSpawnEnv: %s", err)
	}
	review := envMap(reviewEnv)
	implEnv, err := buildAgentSpawnEnvVars(parameters, io.Discard)
	if err != nil {
		t.Fatalf("the implement run's env: %s", err)
	}
	impl := envMap(implEnv)
	for _, key := range []string{"AGENT_TYPE", "MODEL", "ANTHROPIC_API_KEY"} {
		if review[key] != impl[key] {
			t.Errorf("%s = %q in the review spawn, %q in the implement spawn; with no reviewer they must agree",
				key, review[key], impl[key])
		}
	}
	if stage.jobModel() != "claude-sonnet-4-6" || stage.jobAgentType() != "claude-code" {
		t.Errorf("the record names %s/%s, want the Job's own agent and model",
			stage.jobAgentType(), stage.jobModel())
	}
}

// THE MISSING-CREDENTIALS TEST IS THE PROVIDER. deployment-server stamps the
// reviewer's provider and bundle together or neither, so a reviewer with no
// provider is a reviewer with no credentials: the round fails naming it, and
// the implementer's model is never used as a silent substitute.
func TestAReviewerWithNoCredentialsFailsTheRoundByName(t *testing.T) {
	parameters := withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", nil, nil)
	view, failure := reviewerParameters(parameters)

	if failure != "no credentials for the reviewer model gpt-5.5" {
		t.Errorf("the failure reason = %q, want it to name the reviewer's model", failure)
	}
	if model, _ := jobs.GetParameterValue[string](view, parameters_enums.Model); model != "gpt-5.5" {
		t.Errorf("the failed round's view names model %q, want the reviewer's — the implementer's model is never a fallback", model)
	}

	// Recorded through the ordinary failed-round path: coverage naming the
	// reason, the reviewer's own provenance, and never a failed Step.
	stage := reviewStageFor(t, parameters)
	stage.recordFailedRound(1, stage.reviewerFailure, agentResult{})
	if len(stage.rounds) != 1 {
		t.Fatalf("recorded %d rounds, want 1", len(stage.rounds))
	}
	round := stage.rounds[0]
	if round.Completed {
		t.Error("the round was recorded as completed")
	}
	if round.Model != "gpt-5.5" || round.AgentType != "codex" {
		t.Errorf("the round record names %s/%s, want the reviewer that could not run", round.AgentType, round.Model)
	}
	if !strings.Contains(round.Error, "no credentials for the reviewer model gpt-5.5") {
		t.Errorf("the round's error = %q, want the named reason", round.Error)
	}
	for _, c := range round.Coverage {
		if !strings.Contains(c.Reason, "no credentials for the reviewer model") {
			t.Errorf("coverage for %s reads %q, want the reviewer reason", c.Parameter, c.Reason)
			break
		}
	}
}

// An EMPTY or absent bundle with a provider present is valid. For Bedrock or a
// strict subscription org the reviewer legitimately carries no secrets at all —
// exactly as the implementer's own bundle already can — and failing the round
// over it would make those orgs unable to name a reviewer.
func TestAReviewerWithAProviderAndNoSecretsRunsNormally(t *testing.T) {
	bedrock := llm_provider_enums.AWSBedrock
	for _, tc := range []struct {
		name    string
		envVars map[string]string
	}{
		{"an empty bundle", map[string]string{}},
		{"an absent bundle", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := withReviewer(t, implementerJobParameters(t), "claude-code", "claude-sonnet-4-6",
				&bedrock, tc.envVars)
			view, failure := reviewerParameters(parameters)
			if failure != "" {
				t.Fatalf("a reviewer with a provider and no secrets failed the round: %q", failure)
			}
			if provider, _ := jobs.GetParameterValue[string](view, parameters_enums.AgentProvider); provider != bedrock.Key() {
				t.Errorf("the reviewer's provider = %q, want %q", provider, bedrock.Key())
			}
			creds, err := jobs.GetParameterValue[map[string]string](view, parameters_enums.AgentEnvVars)
			if err != nil {
				t.Fatalf("the reviewer view has no credential bundle at all: %s", err)
			}
			if len(creds) != 0 {
				t.Errorf("the reviewer's bundle = %v, want it empty rather than the implementer's secrets", creds)
			}
		})
	}
}

// The RECORD names the reviewer that ran — the round entries and, through
// them, the final ReviewResultDtoV1 the Step stores.
func TestTheRoundRecordNamesTheReviewer(t *testing.T) {
	openAI := llm_provider_enums.OpenAIDirect
	parameters := withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", &openAI,
		map[string]string{"OPENAI_API_KEY": "sk-openai-reviewer"})
	stage := reviewStageFor(t, parameters)

	stage.recordRound(1, nil, agentResult{Status: "success", Turns: 3})
	round := stage.rounds[0]
	if round.AgentType != "codex" || round.Model != "gpt-5.5" {
		t.Errorf("the round record names %s/%s, want the reviewer", round.AgentType, round.Model)
	}
	// The DTO the Step stores is built from the FINAL round, so naming the
	// reviewer there is the same fact reaching the dashboard.
	if stage.jobModel() != "gpt-5.5" {
		t.Errorf("the review provenance = %q, want the reviewer's model", stage.jobModel())
	}
}

// A review run is priced against the REVIEWER's model and route. The same
// tokens cost different amounts on different models, so pricing a Codex review
// at the implementer's Claude rate would misreport the Task's cost — and the
// envelope must still land on the Job's real parameters, not on the shallow
// copy the reviewer runs under.
func TestReviewCostIsPricedWithTheReviewersModel(t *testing.T) {
	openAI := llm_provider_enums.OpenAIDirect
	parameters := withReviewer(t, implementerJobParameters(t), "codex", "gpt-5.5", &openAI,
		map[string]string{"OPENAI_API_KEY": "sk-openai-reviewer"})
	stage := reviewStageFor(t, parameters)

	// No agent-reported cost, so the catalogue's (model, provider) rate is
	// what prices it — which is the path the reviewer's model has to reach.
	if err := accumulateReviewRunUsage(stage.parameters, stage.reviewerView(), agentResult{
		Status: "success", Turns: 4,
		TokenUsage: tokenUsage{InputTokens: 10_000, OutputTokens: 2_000},
	}); err != nil {
		t.Fatalf("accumulateReviewRunUsage: %s", err)
	}

	raw, err := jobs.GetParameterValue[string](stage.parameters, parameters_enums.JobOutput)
	if err != nil {
		t.Fatalf("the review's usage was written to the reviewer's shallow copy, not the Job: %s", err)
	}
	data := jobOutputData{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("job output: %s", err)
	}
	if data.Cost == nil {
		t.Fatal("the review run was not priced at all")
	}
	if data.Cost.Model != "gpt-5.5" {
		t.Errorf("the review was priced as %q, want the reviewer's model", data.Cost.Model)
	}
	if data.Cost.Provider != llm_provider_enums.OpenAIDirect.String() {
		t.Errorf("the review was priced via %q, want the reviewer's provider", data.Cost.Provider)
	}
	if data.Agent == nil || data.Agent.TokenUsage.InputTokens != 10_000 {
		t.Errorf("the review's tokens did not reach the Step's totals: %+v", data.Agent)
	}
}

// renderedModelFor is what the shared builder would write for this (model,
// provider, agent) triple — derived rather than hardcoded, so this test pins
// "the reviewer goes through the same rendering" and not a particular id.
func renderedModelFor(t *testing.T, logical string, provider llm_provider_enums.Provider,
	agentType llm_provider_enums.AgentType) string {
	t.Helper()
	env := map[string]string{"MODEL": logical}
	if err := applyAgentModelEnv(env, agentSpawn{provider: provider, agentType: agentType, model: logical}, nil, io.Discard); err != nil {
		t.Fatalf("applyAgentModelEnv: %s", err)
	}
	return env["MODEL"]
}

// --- the pull-request body -------------------------------------------------

// The body says WHO reviewed, because a Task's reviewer is no longer implied by
// its model — and a reader weighing a finding is weighing that model's
// judgement.
func TestTheReviewSectionSaysWhoReviewed(t *testing.T) {
	opr := &taskOpenPR{review: &reviewOutput{
		Participation: "on",
		Rounds: []reviewRoundOutput{{
			Round: 1, Completed: true, AgentType: "codex", Model: "gpt-5.5",
			Coverage: []reviewCoverageOutput{{Parameter: "security", State: "checked"}},
		}},
	}}
	section := opr.reviewSection()
	if !strings.Contains(section, "Reviewed by gpt-5.5") {
		t.Errorf("the review section does not name the reviewer:\n%s", section)
	}
	if strings.Contains(section, "rounds") {
		t.Errorf("a single-round review announced a round count:\n%s", section)
	}

	// More than one round means the first found something that had to be
	// fixed, which is part of reading what follows.
	opr.review.Rounds = append(opr.review.Rounds, reviewRoundOutput{
		Round: 2, Completed: true, AgentType: "codex", Model: "gpt-5.5",
		Coverage: []reviewCoverageOutput{{Parameter: "security", State: "checked"}},
	})
	if section := opr.reviewSection(); !strings.Contains(section, "Reviewed by gpt-5.5, 2 rounds") {
		t.Errorf("a two-round review does not carry its round count:\n%s", section)
	}

	// A review recorded by a runner older than this line names nobody, and
	// saying nothing beats guessing.
	opr.review.Rounds = []reviewRoundOutput{{Round: 1, Completed: true}}
	if section := opr.reviewSection(); strings.Contains(section, "Reviewed by") {
		t.Errorf("a review with no recorded model claimed one anyway:\n%s", section)
	}
}
