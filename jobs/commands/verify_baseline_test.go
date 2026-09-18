package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// The commit gate's whole decision table.
//
// The case that motivates the new branch is "failed, pre-existing": agentbox
// replayed the failing command on the commit each repo was checked out at
// when the run began and found the same failure there. Discarding the Step's
// work over a build that was already red punishes the wrong run. Everything
// else must behave exactly as it did before — in particular a legacy payload
// with no steps, which is what every already-deployed agentbox image emits.
func TestDecideVerifyGate(t *testing.T) {
	cases := []struct {
		name string
		vr   *verifyResult
		want verifyGateDecision
	}{
		{
			name: "no verify reported (older image, or agent stayed silent)",
			vr:   nil,
			want: verifyGateProceed,
		},
		{
			name: "skipped — docs-only change",
			vr:   &verifyResult{Ran: false, SkippedReason: "docs-only"},
			want: verifyGateProceed,
		},
		{
			name: "passed",
			vr:   &verifyResult{Ran: true, Passed: true, Command: "go test ./..."},
			want: verifyGateProceed,
		},
		{
			name: "failed, and the failure is new",
			vr: &verifyResult{Ran: true, Passed: false, Command: "go test ./...", Steps: []verifyStep{
				{Repo: "0-acme/api", Command: "go test ./...", Passed: false, BaselineRan: true, BaselinePassed: true},
			}},
			want: verifyGateFail,
		},
		{
			name: "failed, pre-existing",
			vr: &verifyResult{Ran: true, Passed: false, Command: "go test ./...", PreExisting: true, Steps: []verifyStep{
				{Repo: "0-acme/api", Command: "go test ./...", Passed: false, BaselineRan: true, BaselinePassed: false},
			}},
			want: verifyGateWarnPreExisting,
		},
		{
			name: "failed, baseline could not be established",
			vr: &verifyResult{Ran: true, Passed: false, Command: "go test ./...", Steps: []verifyStep{
				{Repo: "0-acme/api", Command: "go test ./...", Passed: false, BaselineRan: false},
			}},
			want: verifyGateFail,
		},
		{
			name: "legacy shape — failed with no steps at all",
			vr:   &verifyResult{Ran: true, Passed: false, Command: "go test ./...", StderrTail: "boom"},
			want: verifyGateFail,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideVerifyGate(tc.vr); got != tc.want {
				t.Errorf("decideVerifyGate = %v, want %v", got, tc.want)
			}
		})
	}
}

// A legacy payload must decode with PreExisting false. If a future agentbox
// ever emitted the flag without the steps to back it, the runner would push
// on a claim it can't see the evidence for — so pin the decode.
func TestVerifyResultDecodesLegacyAndStepsPayloads(t *testing.T) {
	var legacy verifyResult
	if err := json.Unmarshal([]byte(`{"ran":true,"passed":false,"command":"go test ./...","stderr_tail":"boom"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.PreExisting || len(legacy.Steps) != 0 {
		t.Errorf("legacy payload decoded as %+v, want no steps and pre_existing false", legacy)
	}
	if decideVerifyGate(&legacy) != verifyGateFail {
		t.Error("a legacy failing verify must still fail the Step")
	}

	var modern verifyResult
	payload := `{"ran":true,"passed":false,"command":"go test ./...","pre_existing":true,"steps":[` +
		`{"repo":"0-acme/api","command":"go test ./...","passed":false,"stderr_tail":"want 200, got 500",` +
		`"baseline_ran":true,"baseline_passed":false,"baseline_stderr_tail":"want 200, got 500"},` +
		`{"repo":"1-acme/web","command":"npm test","passed":true}]}`
	if err := json.Unmarshal([]byte(payload), &modern); err != nil {
		t.Fatal(err)
	}
	if !modern.PreExisting || len(modern.Steps) != 2 {
		t.Fatalf("decoded %+v, want pre_existing with 2 steps", modern)
	}
	if !modern.Steps[0].BaselineRan || modern.Steps[0].BaselinePassed {
		t.Errorf("failing step decoded as %+v, want a failing baseline", modern.Steps[0])
	}
	if modern.Steps[0].BaselineStderrTail == "" {
		t.Error("the baseline tail was dropped at decode — the PR body has nothing to quote")
	}
	if got := preExistingVerifySteps(&modern); len(got) != 1 || got[0].Repo != "0-acme/api" {
		t.Errorf("preExistingVerifySteps = %+v, want only the failing repo", got)
	}
}

// The warning is the only account of the failure in the job log. A PR that
// lands over a red build with the log saying merely "verification failed but
// looks pre-existing" trades a lost Step for an unexplained green one.
func TestFormatPreExistingVerifyWarning(t *testing.T) {
	vr := &verifyResult{
		Ran: true, Passed: false, Command: "go test ./...", PreExisting: true,
		Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./...", Passed: false,
				StderrTail: "user_test.go:31: want 200, got 500", BaselineRan: true, BaselinePassed: false},
			{Repo: "1-acme/web", Command: "npm test", Passed: true},
			{Repo: "2-acme/cli", Command: "cargo test", Passed: false,
				BaselineRan: true, BaselinePassed: false, BaselineStderrTail: "error[E0308]: mismatched types"},
		},
	}
	got := formatPreExistingVerifyWarning(vr)

	for _, want := range []string{
		"0-acme/api", "go test ./...", "user_test.go:31: want 200, got 500",
		"2-acme/cli", "cargo test", "error[E0308]: mismatched types",
		"fails on the base commit as well",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1-acme/web") {
		t.Errorf("a PASSING step was named as pre-existing:\n%s", got)
	}
	if !strings.HasPrefix(got, "warning: ") {
		t.Errorf("the line must read as a warning, got: %q", got)
	}
}

// A runaway tail reaches the job log and the PR body, so it is bounded from
// the END — where the failure is.
func TestPreExistingWarningBoundsTheTail(t *testing.T) {
	vr := &verifyResult{
		Ran: true, Passed: false, PreExisting: true,
		Steps: []verifyStep{{
			Repo: "0-acme/api", Command: "go test ./...", Passed: false,
			StderrTail:  strings.Repeat("compiling\n", 500) + "FAIL: the actual reason",
			BaselineRan: true,
		}},
	}
	got := formatPreExistingVerifyWarning(vr)
	if !strings.Contains(got, "FAIL: the actual reason") {
		t.Error("truncation dropped the end, which is where the failure is")
	}
	if len(got) > verifyTailMaxBytes+500 {
		t.Errorf("warning is %d bytes, want it bounded near %d", len(got), verifyTailMaxBytes)
	}
}

// verify_result has to survive the hop from RunAgentStep to OpenPullRequest.
// They are separate commands: OpenPullRequest never sees /result.json, only
// the JobOutput envelope, so an unplumbed field means the PR body silently
// loses the section.
func TestVerifyResultReachesOpenPullRequestThroughJobOutput(t *testing.T) {
	parameters := map[string]interface{}{}
	res := agentResult{
		Status:         "success",
		ChangesSummary: "Did the thing",
		VerifyResult: &verifyResult{
			Ran: true, Passed: false, Command: "go test ./...", PreExisting: true,
			Steps: []verifyStep{{
				Repo: "0-acme/api", Command: "go test ./...", Passed: false,
				StderrTail: "want 200, got 500", BaselineRan: true, BaselinePassed: false,
			}},
		},
	}
	if err := mergeAgentResultIntoJobOutput(parameters, res); err != nil {
		t.Fatal(err)
	}

	got := readVerifyResultFromJobOutput(parameters)
	if got == nil {
		t.Fatal("verify_result did not survive the JobOutput round trip")
	}
	if !got.PreExisting || len(got.Steps) != 1 {
		t.Fatalf("read back %+v, want pre_existing with one step", got)
	}
	if got.Steps[0].Repo != "0-acme/api" || got.Steps[0].StderrTail != "want 200, got 500" {
		t.Errorf("step lost detail in transit: %+v", got.Steps[0])
	}
}

func TestReadVerifyResultFromJobOutputDegradesQuietly(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"no job output":               {},
		"empty string":                jobOutputParams(""),
		"malformed":                   jobOutputParams("{not json"),
		"no agent block":              jobOutputParams(`{"schema_version":1,"repositories":[]}`),
		"agent without verify_result": jobOutputParams(`{"schema_version":1,"agent":{"changes_summary":"x"}}`),
	}
	for name, parameters := range cases {
		t.Run(name, func(t *testing.T) {
			if got := readVerifyResultFromJobOutput(parameters); got != nil {
				t.Errorf("got %+v, want nil — a bad envelope must not stop the PR from landing", got)
			}
		})
	}
}

func jobOutputParams(payload string) map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, payload)
	return parameters
}

// The PR is where the reviewer is. A Step that landed only because its
// verification failure predates it has to say so there, not only in a job log
// the reviewer would have to know to open.
func TestPRBodyCarriesVerificationSectionWhenPreExisting(t *testing.T) {
	opr := &taskOpenPR{
		ctx: commandUtils.TaskJobContext{
			OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task", StepIndex: 0,
		},
		agentSummary: "Add OAuth login",
		verifyResult: &verifyResult{
			Ran: true, Passed: false, Command: "go test ./...", PreExisting: true,
			Steps: []verifyStep{
				{Repo: "0-acme/api", Command: "go test ./...", Passed: false,
					StderrTail: "user_test.go:31: want 200, got 500", BaselineRan: true, BaselinePassed: false},
				{Repo: "1-acme/web", Command: "npm test", Passed: true},
			},
		},
	}
	_, body := opr.buildPRTitleAndBody()

	for _, want := range []string{
		"**Verification: failing before this Step**",
		"`go test ./...`",
		"`0-acme/api`",
		"fails on the base commit as well",
		"user_test.go:31: want 200, got 500",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "1-acme/web") {
		t.Errorf("a passing repo was reported as failing:\n%s", body)
	}
	// Optional detail, like denied-hosts: after the trailer, never before
	// the agent's own narrative.
	trailerIdx := strings.Index(body, "Generated-By:")
	sectionIdx := strings.Index(body, "**Verification:")
	if trailerIdx == -1 || sectionIdx <= trailerIdx {
		t.Errorf("the Verification section must follow the trailer:\n%s", body)
	}
}

// Every ordinary PR: nothing pre-existing, so nothing to explain. A section
// that showed up on green PRs would train reviewers to skip it.
func TestPRBodyHasNoVerificationSectionWhenNothingIsPreExisting(t *testing.T) {
	cases := map[string]*verifyResult{
		"no verify at all":                     nil,
		"passed":                               {Ran: true, Passed: true, Command: "go test ./..."},
		"skipped":                              {Ran: false, SkippedReason: "docs-only"},
		"legacy failing payload with no steps": {Ran: true, Passed: false, Command: "go test ./...", StderrTail: "boom"},
		"failed with a passing baseline": {Ran: true, Passed: false, Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./...", Passed: false, BaselineRan: true, BaselinePassed: true},
		}},
		"failed with no baseline": {Ran: true, Passed: false, Steps: []verifyStep{
			{Repo: "0-acme/api", Command: "go test ./...", Passed: false, BaselineRan: false},
		}},
	}
	for name, vr := range cases {
		t.Run(name, func(t *testing.T) {
			opr := &taskOpenPR{
				ctx:          commandUtils.TaskJobContext{OrganizationID: "org-1", TaskID: "task-1", TaskTitle: "My Task"},
				agentSummary: "Add OAuth login",
				verifyResult: vr,
			}
			if _, body := opr.buildPRTitleAndBody(); strings.Contains(body, "**Verification:") {
				t.Errorf("unexpected Verification section:\n%s", body)
			}
		})
	}
}
