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
		"Every acceptance criterion is decidable.",
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
