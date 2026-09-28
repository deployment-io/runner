package commands

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// --- what a review run contributes to the Step's record ----------------------

// A REVIEWER'S BLOCKED HOSTS ARE NOT THE STEP'S. The pull request's blocked-host
// section tells the user to add what it lists to Tasks → Allowed Hosts, and a
// Codex reviewer's startup calls (github.com, api.github.com, chatgpt.com) turned
// up there on a pull request whose implementer never tried to reach them.
func TestAReviewRoundsBlockedHostsStayOutOfTheStepsRecord(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	var logs strings.Builder
	stage := fixLoopStage(workDir, &logs)
	// The implement run's own blocked host is already on the record, and stays.
	if err := mergeAgentResultIntoJobOutput(stage.parameters, agentResult{
		Status: "success", DeniedHosts: []string{"registry.internal"},
	}); err != nil {
		t.Fatalf("mergeAgentResultIntoJobOutput: %s", err)
	}
	stage.runReview = func(int) (agentResult, error) {
		return agentResult{
			Status: "success", Turns: 9,
			DeniedHosts: []string{"api.github.com", "chatgpt.com"},
			ReviewResult: &reviewResult{
				Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}},
			},
		}, nil
	}

	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}

	hosts, err := readDeniedHostsFromJobOutput(out)
	if err != nil {
		t.Fatalf("readDeniedHostsFromJobOutput: %s", err)
	}
	if len(hosts) != 1 || hosts[0] != "registry.internal" {
		t.Errorf("the Step's blocked hosts = %v, want only what the implement run tried to reach", hosts)
	}
	// Which is what keeps them off the pull request, where they read as advice.
	section := (&taskOpenPR{deniedHosts: hosts}).blockedHostsSection()
	for _, host := range []string{"api.github.com", "chatgpt.com"} {
		if strings.Contains(section, host) {
			t.Errorf("the pull request asks the user to allow %s, which only the reviewer wanted:\n%s", host, section)
		}
	}
	// They are not lost, though: whoever is debugging the reviewer needs them.
	if !strings.Contains(logs.String(), "Review round 1: the reviewer's requests to these hosts were blocked: api.github.com, chatgpt.com") {
		t.Errorf("the job log does not carry the reviewer's blocked hosts:\n%s", logs.String())
	}
	// Everything else about the round still counts — the tokens were spent.
	if got := decodeJobOutput(t, out).Agent.Turns; got != 9 {
		t.Errorf("the Step's turns = %d, want the review round's own to still be counted", got)
	}
}

// A FIX RUN'S DO. It is the implementer, run again with a narrower ask, and a
// host it could not reach is a host the Task's allowlist is missing.
func TestAFixRunsBlockedHostsStillReachTheStepsRecord(t *testing.T) {
	parameters := map[string]interface{}{}
	if err := recordFixRunResult(parameters, agentResult{
		Status: "success", DeniedHosts: []string{"proxy.corp"},
	}, nil, io.Discard); err != nil {
		t.Fatalf("recordFixRunResult: %s", err)
	}

	hosts, err := readDeniedHostsFromJobOutput(parameters)
	if err != nil {
		t.Fatalf("readDeniedHostsFromJobOutput: %s", err)
	}
	if len(hosts) != 1 || hosts[0] != "proxy.corp" {
		t.Fatalf("the Step's blocked hosts = %v, want the fix run's own", hosts)
	}
	if !strings.Contains((&taskOpenPR{deniedHosts: hosts}).blockedHostsSection(), "proxy.corp") {
		t.Error("the fix run's blocked host never reached the pull request")
	}
}

// --- what the reviewer is told about the build -------------------------------

// The reviewer needs the implementer's own verify verdict: without it, it either
// re-runs the build out of its round's budget or reports a build concern nobody
// can act on. agentbox 1.9.22 reads REVIEW_VERIFY_RESULT and shows it under
// [Build and tests].
func TestTheReviewerIsToldTheStepsVerifyResult(t *testing.T) {
	parameters := map[string]interface{}{}
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{Status: "success", VerifyResult: &verifyResult{
		Ran: true, Passed: false, Command: "go build ./... && go test ./...",
		StdoutTail: "ok  	acme/api	0.4s", StderrTail: "handler.go:41: undefined: session",
		PreExisting: true,
		Steps: []verifyStep{{
			Repo: "0-acme/api", Command: "go build ./...", Passed: false,
			StderrTail: "handler.go:41: undefined: session",
		}},
	}}); err != nil {
		t.Fatalf("mergeAgentResultIntoJobOutput: %s", err)
	}
	stage := &reviewStage{parameters: parameters, logsWriter: io.Discard}

	payload := stage.verifyResultEnvValue()
	// THE TAILS ARE LEFT OUT. A build log is the one input most likely to fill a
	// review round's context with text it cannot act on.
	for _, tail := range []string{"undefined: session", "stderr_tail", "stdout_tail"} {
		if strings.Contains(payload, tail) {
			t.Errorf("REVIEW_VERIFY_RESULT carries %q:\n%s", tail, payload)
		}
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("REVIEW_VERIFY_RESULT is not JSON (%s): %s", err, payload)
	}
	if decoded["ran"] != true || decoded["passed"] != false || decoded["pre_existing"] != true {
		t.Errorf("REVIEW_VERIFY_RESULT = %v, want the verdict the Step recorded", decoded)
	}
	if decoded["command"] != "go build ./... && go test ./..." {
		t.Errorf("command = %v, want the command that ran", decoded["command"])
	}
	steps, ok := decoded["steps"].([]interface{})
	if !ok || len(steps) != 1 {
		t.Fatalf("steps = %v, want one per repository", decoded["steps"])
	}
	step := steps[0].(map[string]interface{})
	if step["repo"] != "0-acme/api" || step["command"] != "go build ./..." || step["passed"] != false {
		t.Errorf("the repository's step = %v, want its repo, command and verdict", step)
	}
	if _, present := step["stderr_tail"]; present {
		t.Errorf("a repository's step carries its output tail: %v", step)
	}

	// THE LATEST ONE WINS: after a kept fix run, the current verdict is the fix
	// run's, because that is the code the reviewer is about to read.
	if err := mergeFixResultIntoJobOutput(parameters, agentResult{Status: "success", VerifyResult: &verifyResult{
		Ran: true, Passed: true, Command: "go test ./...",
	}}); err != nil {
		t.Fatalf("mergeFixResultIntoJobOutput: %s", err)
	}
	if err := json.Unmarshal([]byte(stage.verifyResultEnvValue()), &decoded); err != nil {
		t.Fatalf("REVIEW_VERIFY_RESULT: %s", err)
	}
	if decoded["passed"] != true || decoded["command"] != "go test ./..." {
		t.Errorf("REVIEW_VERIFY_RESULT = %v, want the latest kept fix run's verdict", decoded)
	}

	// A run that SKIPPED verification says so, with its reason: a reviewer told
	// nothing would have to guess whether the change was ever built.
	skipped := map[string]interface{}{}
	if err := mergeFixResultIntoJobOutput(skipped, agentResult{Status: "success", VerifyResult: &verifyResult{
		Ran: false, SkippedReason: "the change is documentation only",
	}}); err != nil {
		t.Fatalf("mergeFixResultIntoJobOutput: %s", err)
	}
	payload = (&reviewStage{parameters: skipped, logsWriter: io.Discard}).verifyResultEnvValue()
	if !strings.Contains(payload, `"skipped_reason":"the change is documentation only"`) {
		t.Errorf("REVIEW_VERIFY_RESULT = %s, want the skipped reason", payload)
	}
}

// With no verify result recorded, NOTHING is sent. An empty object would claim a
// build that nobody ran, and an inherited value would describe some other run's.
func TestTheReviewerIsToldNothingWhenThereIsNoVerifyResult(t *testing.T) {
	stage := &reviewStage{parameters: map[string]interface{}{}, logsWriter: io.Discard}
	if got := stage.verifyResultEnvValue(); got != "" {
		t.Errorf("REVIEW_VERIFY_RESULT = %q, want nothing for a Step with no verify result", got)
	}

	inherited := applyReviewEnv(
		[]string{`REVIEW_VERIFY_RESULT={"ran":true,"passed":true,"command":"some other run"}`},
		reviewEnvInputs{passes: reviewPasses, baseCommits: `{"0-a/b":"abc"}`, round: 1},
	)
	if got, ok := envMap(inherited)["REVIEW_VERIFY_RESULT"]; ok {
		t.Errorf("REVIEW_VERIFY_RESULT = %q, want an inherited value stripped", got)
	}

	payload := `{"ran":true,"passed":true,"command":"go test ./..."}`
	sent := applyReviewEnv(nil, reviewEnvInputs{
		passes: reviewPasses, baseCommits: `{"0-a/b":"abc"}`, round: 2, verifyResult: payload,
	})
	if got := envMap(sent)["REVIEW_VERIFY_RESULT"]; got != payload {
		t.Errorf("REVIEW_VERIFY_RESULT = %q, want %q", got, payload)
	}
}
