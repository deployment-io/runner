package commands

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The implementer's own record, as RunAgentStep leaves it before the stage.
func declinedFixImplementerResult() agentResult {
	return agentResult{
		Status: "success", Turns: 10, PRTitle: "Split the settings page",
		ChangesSummary: "Split the settings page into three pages.",
		FilesChanged:   []string{"0-acme/api/handler.go"},
		TokenUsage:     tokenUsage{InputTokens: 1000, OutputTokens: 100},
		CostUSD:        f64(0.40),
		VerifyResult:   &verifyResult{Ran: true, Command: "go test ./...", Steps: []verifyStep{{Repo: "0-acme/api", Passed: true}}},
	}
}

func declinedFixRunResult() agentResult {
	return agentResult{
		Status: "success", Turns: 2, PRTitle: "Leave the pages split",
		ChangesSummary: "I didn't change any code in this round: the Step asked for three pages.",
		FilesChanged:   []string{"0-acme/api/other.go"},
		TokenUsage:     tokenUsage{InputTokens: 300, OutputTokens: 30},
		CostUSD:        f64(0.20),
		VerifyResult:   &verifyResult{Ran: true, Command: "make test", Steps: []verifyStep{{Repo: "0-acme/api", Passed: true}}},
	}
}

// A fix run that changes no file leaves the Step's record of the change
// alone: the description, title, files and verify result stay the
// implementer's, its usage still counts, and its summary is kept on the review
// record as the explanation shown beside the findings.
func TestAFixRunThatChangedNothingDoesNotReplaceTheDescription(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, nil)
	implementer := declinedFixImplementerResult()
	if err := mergeAgentResultIntoJobOutput(stage.parameters, implementer); err != nil {
		t.Fatal(err)
	}
	stage.runFix = func([]reviewFindingOutput) error {
		fix := declinedFixRunResult()
		stage.finishedFix = &fix
		return nil
	}

	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}
	data := readJobOutput(t, out)
	if data.Agent == nil {
		t.Fatal("no agent block")
	}
	if data.Agent.ChangesSummary != implementer.ChangesSummary {
		t.Errorf("changes_summary = %q, want the implementer's", data.Agent.ChangesSummary)
	}
	if data.Agent.PRTitle != implementer.PRTitle {
		t.Errorf("pr_title = %q, want the implementer's", data.Agent.PRTitle)
	}
	if !reflect.DeepEqual(data.Agent.FilesChanged, implementer.FilesChanged) {
		t.Errorf("files_changed = %q, want the implementer's", data.Agent.FilesChanged)
	}
	if data.Agent.VerifyResult == nil || data.Agent.VerifyResult.Command != "go test ./..." {
		t.Errorf("verify_result = %+v, want the implementer's", data.Agent.VerifyResult)
	}
	if data.Usage == nil {
		t.Fatal("usage absent")
	}
	assertStageUsage(t, "implement", data.Usage.Implement,
		tokenUsage{InputTokens: 1300, OutputTokens: 130}, f64(0.40+0.20), false)
	review := data.Review
	if review == nil || !review.StoppedNoChange {
		t.Fatalf("review = %+v, want a no-change stop", review)
	}
	if review.DeclinedExplanation != declinedFixRunResult().ChangesSummary {
		t.Errorf("declined_explanation = %q, want the fix run's summary", review.DeclinedExplanation)
	}
	section := (&taskOpenPR{review: review}).reviewSection()
	if !strings.Contains(section, "_The last fix round changed nothing. The agent's explanation:_\n\n> I didn't change any code") {
		t.Errorf("the review section does not quote the explanation:\n%s", section)
	}
}

// A fix run that changed files is the change now, so its summary replaces
// the implementer's exactly as before, and nothing is recorded as declined.
func TestAFixRunThatChangedFilesStillReplacesTheDescription(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "0-acme/api")
	writeFile(t, filepath.Join(repoDir, "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, nil)
	if err := mergeAgentResultIntoJobOutput(stage.parameters, declinedFixImplementerResult()); err != nil {
		t.Fatal(err)
	}
	fixRuns := 0
	stage.runFix = func([]reviewFindingOutput) error {
		fixRuns++
		writeFile(t, filepath.Join(repoDir, "handler.go"), "the fixed implementation "+strings.Repeat("!", fixRuns)+"\n")
		fix := agentResult{Status: "success", Turns: 2, ChangesSummary: "Split the settings page and checked the session."}
		stage.finishedFix = &fix
		return nil
	}

	out, err := stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}
	data := readJobOutput(t, out)
	if fixRuns == 0 {
		t.Fatal("no fix round ran")
	}
	if got := data.Agent.ChangesSummary; got != "Split the settings page and checked the session." {
		t.Errorf("changes_summary = %q, want the kept fix run's", got)
	}
	if data.Review == nil || data.Review.StoppedNoChange || data.Review.DeclinedExplanation != "" {
		t.Errorf("review = %+v, want no declined fix recorded", data.Review)
	}
}

func TestFixOutcomeLineForANoChangeStop(t *testing.T) {
	if got := fixOutcomeLine(&reviewOutput{StoppedNoChange: true}, ""); got != "\n_The last fix round changed nothing._\n" {
		t.Errorf("without an explanation: %q", got)
	}

	got := fixOutcomeLine(&reviewOutput{
		StoppedNoChange:     true,
		DeclinedExplanation: "Not a bug, @alice: see [the spec](https://example.com).\n\nSecond paragraph.",
	}, "")
	want := "\n_The last fix round changed nothing. The agent's explanation:_\n\n" +
		"> Not a bug, @​alice: see \\[the spec\\](https:​//example.com).\n" +
		">\n" +
		"> Second paragraph.\n"
	if got != want {
		t.Errorf("with an explanation:\n got %q\nwant %q", got, want)
	}

	long := fixOutcomeLine(&reviewOutput{StoppedNoChange: true, DeclinedExplanation: strings.Repeat("x", 5000)}, "")
	quoted := strings.TrimPrefix(long, "\n_The last fix round changed nothing. The agent's explanation:_\n\n> ")
	if n := utf8.RuneCountInString(strings.TrimSuffix(quoted, "\n")); n != reviewDetailMaxRunes*4 {
		t.Errorf("a long explanation was cut to %d runes, want %d", n, reviewDetailMaxRunes*4)
	}
	if !strings.HasSuffix(long, "…\n") {
		t.Errorf("a cut explanation does not say it was cut: %q", long[len(long)-20:])
	}
}
