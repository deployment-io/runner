package commands

import (
	"strings"
	"testing"
)

func TestPlanModePromptHasSpecSelfCheck(t *testing.T) {
	first := `Before you set readiness to "ready", check the spec against these rules and fix anything that fails in the same message.`
	for _, want := range []string{
		first,
		"Values are written out.",
		"Names exist.",
		"User-facing text is true in every state.",
		"4. Every acceptance criterion is an observable outcome.",
		"Scope is explicit.",
		"Edge cases are decided.",
		"It is consistent.",
		"New configuration is named.",
		"Nothing tells the executing agent to commit, push or open a pull request",
		`If a rule cannot be met yet because only the user can decide, keep readiness at "partial" and put the open question in readiness_notes.`,
	} {
		if !strings.Contains(planModePrompt, want) {
			t.Errorf("planModePrompt missing %q", want)
		}
	}

	readiness := strings.Index(planModePrompt, `Set readiness to "ready" only when`)
	check := strings.Index(planModePrompt, first)
	suggestion := strings.Index(planModePrompt, "If investigating or implementing the outcome genuinely needs a repository")
	if readiness < 0 || check < 0 || suggestion < 0 {
		t.Fatalf("missing anchor: readiness=%d check=%d suggestion=%d", readiness, check, suggestion)
	}
	if !(readiness < check && check < suggestion) {
		t.Errorf("self-check must follow the readiness paragraph and precede the repo-suggestion paragraph: readiness=%d check=%d suggestion=%d", readiness, check, suggestion)
	}
}

func TestPlanModePromptKeepsVerificationOutOfCriteria(t *testing.T) {
	for _, gone := range []string{
		"note what should be verified in the spec's acceptance criteria",
		"verified or prepared for retest in the acceptance criteria",
		"Every acceptance criterion is decidable.",
		"say that only a person can check it after deploy",
	} {
		if strings.Contains(planModePrompt, gone) {
			t.Errorf("planModePrompt still contains %q", gone)
		}
	}
	for _, want := range []string{
		`in the context under a line starting "Verification:"`,
		`Build, test, grep and "file unchanged" checks are not criteria: they go in the context under "Verification:".`,
		`put how the findings will be verified or prepared for retest in the context under "Verification:"`,
		"Don't annotate a criterion with who checks it or how.",
	} {
		if !strings.Contains(planModePrompt, want) {
			t.Errorf("planModePrompt missing %q", want)
		}
	}
}
