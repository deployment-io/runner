package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/docker/docker/api/types/mount"
)

// --- the blind-to boundary -------------------------------------------------

// testPrepareOutputDir is the preparer these tests inject: it creates the fresh
// round directory exactly as the real one does, but does NOT chown it.
//
// The chown is to UID 1000 and fails for an unprivileged process, so calling
// the real preparer here would make every test of the rename logic — which is
// the half with the interesting failure modes — a root-only test. Ownership is
// still covered, by the one euid-gated case below.
func testPrepareOutputDir(workDirHost string) error {
	return os.MkdirAll(filepath.Join(workDirHost, agentboxResultDirRel), 0o755)
}

// The review must not be able to read what the implement run wrote. The
// enforcement is a host-side RENAME rather than mount layering, so it depends
// on no Docker daemon behaviour and can be exercised here.
func TestSwapInReviewOutputDirHidesTheImplementRunsOutput(t *testing.T) {
	workDir := t.TempDir()
	implementerDir := filepath.Join(workDir, agentboxResultDirRel)
	writeFile(t, filepath.Join(implementerDir, "result.json"), `{"status":"success"}`)
	writeFile(t, filepath.Join(implementerDir, "progress.json"), `{"turns":7}`)

	swap, err := swapInReviewOutputDir(workDir, 1, testPrepareOutputDir)
	if err != nil {
		t.Fatalf("swapInReviewOutputDir: %s", err)
	}

	// The directory the container sees at /work/.agentbox-output exists and
	// is EMPTY: this is the round's own, and the implement run's files are
	// not reachable from anywhere under the work dir.
	entries, err := os.ReadDir(implementerDir)
	if err != nil {
		t.Fatalf("the round's output directory was not created: %s", err)
	}
	if len(entries) != 0 {
		t.Errorf("the round's output directory contains %v — the review can see the implement run's files", entries)
	}
	for _, name := range []string{"result.json", "progress.json"} {
		if _, err := os.Stat(filepath.Join(implementerDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still visible at /work/.agentbox-output", name)
		}
	}
	// And the stash is OUTSIDE the work dir, not a dot-directory inside it:
	// everything under the work dir is visible at /work, and a dot-prefix
	// only hides a path from repo discovery and the commit diff.
	stash := implementerOutputStashPath(workDir)
	if strings.HasPrefix(stash, strings.TrimRight(workDir, "/")+"/") {
		t.Errorf("the stash %q is inside the work dir — the review container can still read it", stash)
	}
	if _, err := os.Stat(filepath.Join(stash, "result.json")); err != nil {
		t.Errorf("the implement run's result.json was not preserved: %s", err)
	}

	swap.restore(io.Discard)

	// Restored: the implement run's output is back under its own name.
	if got := readFile(t, filepath.Join(implementerDir, "result.json")); got != `{"status":"success"}` {
		t.Errorf("the implement run's result.json was not restored, got %q", got)
	}
	if _, err := os.Stat(stash); !os.IsNotExist(err) {
		t.Errorf("the stash survived the restore: %v", err)
	}
}

// The round's own output reaches the JOB LOG and then leaves the host. Keeping
// the directory as a sibling left one per round per Step on a volume nothing
// ever swept; the log is the durable record.
func TestRestoreLogsTheRoundsOutputAndLeavesNothingBehind(t *testing.T) {
	workDir := t.TempDir()
	implementerDir := filepath.Join(workDir, agentboxResultDirRel)
	writeFile(t, filepath.Join(implementerDir, "result.json"), `{"status":"success"}`)

	swap, err := swapInReviewOutputDir(workDir, 2, testPrepareOutputDir)
	if err != nil {
		t.Fatalf("swapInReviewOutputDir: %s", err)
	}
	writeFile(t, filepath.Join(implementerDir, "result.json"), `{"status":"success","review_result":{}}`)
	var logs strings.Builder
	swap.restore(&logs)

	if !strings.Contains(logs.String(), "review_result") {
		t.Errorf("round 2's result.json did not reach the job log:\n%s", logs.String())
	}
	if _, err := os.Stat(reviewRoundOutputPath(workDir, 2)); !os.IsNotExist(err) {
		t.Errorf("round 2's output directory survived on the host: %v", err)
	}
	if got := readFile(t, filepath.Join(implementerDir, "result.json")); got != `{"status":"success"}` {
		t.Errorf("the implement run's result.json was not restored, got %q", got)
	}
}

// Whatever a failed restore left beside the work dir is swept when the stage
// ends — a host directory nothing else collects is a leak, however it got
// there.
func TestCleanupReviewStageSiblingsRemovesEveryParkedDirectory(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "work")
	writeFile(t, filepath.Join(workDir, agentboxResultDirRel, "result.json"), `{"status":"success"}`)
	writeFile(t, filepath.Join(implementerOutputStashPath(workDir), "result.json"), `{}`)
	writeFile(t, filepath.Join(reviewRoundOutputPath(workDir, 1), "result.json"), `{}`)
	writeFile(t, filepath.Join(reviewRoundOutputPath(workDir, 2), "result.json"), `{}`)
	// A fix round's undo copy is a whole checkout — the one leak nothing else
	// would ever collect.
	writeFile(t, filepath.Join(fixRoundSnapshotPath(workDir, 1), "0-acme/api", "main.go"), "package main\n")

	cleanupReviewStageSiblings(workDir)

	for _, dir := range []string{
		implementerOutputStashPath(workDir),
		reviewRoundOutputPath(workDir, 1),
		reviewRoundOutputPath(workDir, 2),
		fixRoundSnapshotPath(workDir, 1),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep: %v", dir, err)
		}
	}
	// And the work dir's own output is untouched — it is not a sibling.
	if got := readFile(t, filepath.Join(workDir, agentboxResultDirRel, "result.json")); got != `{"status":"success"}` {
		t.Errorf("the sweep removed the Step's own output: %q", got)
	}
}

// The restore runs on a DEFERRED path, so a failed, timed-out or cancelled
// round cannot leave the implementer's output displaced. A Step whose Review
// stage crashed and whose agent block then vanished would look like a Step
// that never ran an agent.
func TestRestoreRunsEvenWhenTheRoundFails(t *testing.T) {
	workDir := t.TempDir()
	implementerDir := filepath.Join(workDir, agentboxResultDirRel)
	writeFile(t, filepath.Join(implementerDir, "result.json"), `{"status":"success"}`)

	func() {
		swap, err := swapInReviewOutputDir(workDir, 1, testPrepareOutputDir)
		if err != nil {
			t.Fatalf("swapInReviewOutputDir: %s", err)
		}
		defer swap.restore(io.Discard)
		// The round dies here — no result written, no orderly finish.
	}()

	if got := readFile(t, filepath.Join(implementerDir, "result.json")); got != `{"status":"success"}` {
		t.Errorf("after a failed round the implement run's output is %q, want it restored", got)
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, agentboxResultDirRel, "result.json"), `{"status":"success"}`)

	swap, err := swapInReviewOutputDir(workDir, 1, testPrepareOutputDir)
	if err != nil {
		t.Fatalf("swapInReviewOutputDir: %s", err)
	}
	swap.restore(io.Discard)
	swap.restore(io.Discard) // a deferred restore after an explicit one

	if got := readFile(t, filepath.Join(workDir, agentboxResultDirRel, "result.json")); got != `{"status":"success"}` {
		t.Errorf("the second restore damaged the implement run's output: %q", got)
	}
}

// The fresh round directory must be writable by the container, which runs as
// UID 1000 through the bind mount.
func TestSwapInReviewOutputDirOwnsTheFreshDirectoryToTheAgentboxUser(t *testing.T) {
	if os.Geteuid() != commandUtils.AgentboxUID && os.Geteuid() != 0 {
		t.Skipf("chown to %d is not permitted as uid %d", commandUtils.AgentboxUID, os.Geteuid())
	}
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, agentboxResultDirRel, "result.json"), `{}`)

	if _, err := swapInReviewOutputDir(workDir, 1, prepareAgentboxHostDirs); err != nil {
		t.Fatalf("swapInReviewOutputDir: %s", err)
	}
	assertOwnedByAgentbox(t, filepath.Join(workDir, agentboxResultDirRel))
}

// Every spawn starts from a clean output directory. Without this a run that
// crashed before flushing its own result.json left the PREVIOUS run's file in
// place, and readAgentResult — which cannot tell one run's file from another's
// — returned it: the Step then passed the verify gate on a verification that
// never happened, counted the same usage twice, and carried on against a
// half-edited tree.
func TestClearAgentboxOutputArtifactsRemovesThePreviousRunsResult(t *testing.T) {
	workDir := t.TempDir()
	outputDir := filepath.Join(workDir, agentboxResultDirRel)
	writeFile(t, filepath.Join(outputDir, agentboxResultFile), `{"status":"success"}`)
	writeFile(t, filepath.Join(outputDir, agentboxProgressFile), `{"turns":7}`)
	// Anything else in the directory is not ours to remove.
	writeFile(t, filepath.Join(outputDir, "messages.jsonl"), "{}\n")

	if err := clearAgentboxOutputArtifacts(workDir); err != nil {
		t.Fatalf("clearAgentboxOutputArtifacts: %s", err)
	}

	for _, name := range []string{agentboxResultFile, agentboxProgressFile} {
		if _, err := os.Stat(filepath.Join(outputDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived; the next run could read it as its own", name)
		}
	}
	if _, err := os.Stat(filepath.Join(outputDir, "messages.jsonl")); err != nil {
		t.Errorf("the sweep removed a file that is not the run's own output: %s", err)
	}
	// And a directory with nothing to clear is not an error — the first run
	// of a Step has nothing to clear.
	if err := clearAgentboxOutputArtifacts(workDir); err != nil {
		t.Errorf("clearing an already-clean directory failed: %s", err)
	}
	if err := clearAgentboxOutputArtifacts(t.TempDir()); err != nil {
		t.Errorf("clearing a directory with no output dir at all failed: %s", err)
	}
}

// --- the review container's environment ------------------------------------

// The review's work item is the diff. Handing it the implementer's prompt
// would invite it to grade the change against what the implementer was told to
// do rather than against what the change does — and to carry on where the
// implementer left off.
func TestReviewSpawnEnvCarriesNeitherStepPromptNorPreviousStepsSummary(t *testing.T) {
	env := applyReviewEnv([]string{
		"ANTHROPIC_API_KEY=sk-ant-test",
		"STEP_PROMPT=implement the login page",
		"PREVIOUS_STEPS_SUMMARY=step 1 did the schema",
		"MAX_TURNS=30",
		"AGENT_TYPE=claude-code",
	}, reviewEnvInputs{
		spec:        `{"title":"Add login"}`,
		passes:      reviewPasses,
		baseCommits: `{"0-acme-api":"abc123"}`,
		round:       1,
	})

	byKey := envMap(env)
	for _, forbidden := range []string{"STEP_PROMPT", "PREVIOUS_STEPS_SUMMARY"} {
		if _, ok := byKey[forbidden]; ok {
			t.Errorf("the review spawn env carries %s — the review must not see the implementer's prompt", forbidden)
		}
	}
	for key, want := range map[string]string{
		"AGENT_MODE":          "review",
		"REVIEW_PASSES":       "security,correctness",
		"REVIEW_BASE_COMMITS": `{"0-acme-api":"abc123"}`,
		"REVIEW_ROUND":        "1",
		"REVIEW_SPEC":         `{"title":"Add login"}`,
		"MAX_TURNS":           "80",
		// The credentials and agent selection are the implement run's: the
		// Review stage runs on the Task's own agent and model.
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_TYPE":        "claude-code",
	} {
		if byKey[key] != want {
			t.Errorf("%s = %q, want %q", key, byKey[key], want)
		}
	}
}

// The repositories are read-only FOR THE REVIEWER, and the runner is what says
// so: the round's own agent may have no working sandbox at all (Codex's
// bwrap-based one cannot create a namespace inside a CapDrop-ALL container),
// and a reviewer that cannot sandbox itself must still be unable to edit the
// change it is reviewing.
func TestTheReviewSpawnMountsEveryRepositoryReadOnly(t *testing.T) {
	workDir := t.TempDir()
	for _, dir := range []string{"0-acme/api", "1-acme/web"} {
		writeFile(t, filepath.Join(workDir, dir, "main.go"), "package main\n")
	}
	dirs, complete := readOnlyRepoDirs(workDir, []string{"0-acme/api", "1-acme/web"}, io.Discard)
	if !complete {
		t.Fatal("two real repositories were reported as an incomplete read-only set")
	}

	mounts := agentboxMounts(agentboxSpawnSpec{
		workDirHost: workDir, cacheVolume: "agentbox-cache-task-1-0", readOnlyRepoDirs: dirs,
	})

	// /work ITSELF STAYS WRITABLE. agentbox writes the round's diff files under
	// /work/.review and its own result under /work/.agentbox-output; a
	// read-only /work would leave the round with nowhere to report from.
	work := findMount(t, mounts, agentboxWorkDirInContainer)
	if work.ReadOnly {
		t.Error("/work is mounted read-only — the round can write neither its diff files nor its result")
	}
	if work.Source != workDir {
		t.Errorf("/work is bound from %q, want the Task's work dir %q", work.Source, workDir)
	}
	// ...and one read-only bind per repository on top of it.
	readOnly := 0
	for _, m := range mounts {
		if m.ReadOnly {
			readOnly++
		}
	}
	if readOnly != 2 {
		t.Errorf("%d read-only mount(s), want one per repository:\n%+v", readOnly, mounts)
	}
	for _, dir := range []string{"0-acme/api", "1-acme/web"} {
		m := findMount(t, mounts, agentboxWorkDirInContainer+"/"+dir)
		if !m.ReadOnly {
			t.Errorf("%s is writable — the reviewer can edit the change it is reviewing", m.Target)
		}
		if m.Type != mount.TypeBind || m.Source != filepath.Join(workDir, dir) {
			t.Errorf("mount %+v, want a bind from the repository's own host directory", m)
		}
	}
}

// An implement run and a fix run EXIST to edit the checkouts. Neither sets
// readOnlyRepoDirs, and a spec without it mounts nothing read-only.
func TestTheImplementAndFixSpawnsMountNothingReadOnly(t *testing.T) {
	for _, spec := range []agentboxSpawnSpec{
		// RunAgentStep's spawn.
		{workDirHost: "/tasks/org-1/task-1", cacheVolume: "agentbox-cache-task-1-0", mcpSocketHost: "/tasks/org-1/task-1-agent-mcp.sock"},
		// runMustFixRound's spawn, which is the same shape with a wait cap.
		{workDirHost: "/tasks/org-1/task-1", cacheVolume: "agentbox-cache-task-1-0", mcpSocketHost: "/tasks/org-1/task-1-agent-mcp.sock", waitTimeout: mustFixRunTimeout},
	} {
		for _, m := range agentboxMounts(spec) {
			if m.ReadOnly {
				t.Errorf("a run that has to edit the checkouts got the read-only mount %+v", m)
			}
		}
	}
}

// The base commits are keyed by whatever CheckoutRepository recorded, and the
// work dir is writable by the agent. A key that escapes the work dir would name
// a host directory outside the Task and Docker would mount it into the
// container; one that is not a directory cannot be mounted at all. Both are
// SKIPPED WITH A LOG LINE — the stage is fail-open, and a review over the
// repositories that could be mounted beats no review.
func TestReadOnlyRepoDirsSkipsAnythingThatIsNotARepositoryDirectory(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "main.go"), "package main\n")
	writeFile(t, filepath.Join(workDir, "notes.md"), "not a repository\n")
	var logs strings.Builder

	got, complete := readOnlyRepoDirs(workDir, []string{
		"0-acme/api",
		"../elsewhere",      // outside the work dir
		"/etc",              // absolute
		"2-acme/never-came", // recorded but not checked out
		"notes.md",          // not a directory
	}, &logs)
	if complete {
		t.Error("a set with skipped directories was reported complete")
	}

	if len(got) != 1 || got[0] != "0-acme/api" {
		t.Errorf("read-only dirs = %v, want only the repository that is really there", got)
	}
	for _, skipped := range []string{"../elsewhere", "/etc", "2-acme/never-came", "notes.md"} {
		if !strings.Contains(logs.String(), skipped) {
			t.Errorf("the job log does not say %q was skipped:\n%s", skipped, logs.String())
		}
	}
}

// The mount is the enforcement, so the agent is told about it: a reviewer that
// starts its own sandbox on top of a read-only mount gains nothing, and Codex's
// bwrap-based one gains less than nothing — it cannot create a namespace in
// these containers, so every command it ran failed and the round examined
// nothing while still counting as a completed review.
func TestTheReviewEnvSaysTheRepositoriesAreReadOnlyAtTheMount(t *testing.T) {
	byKey := envMap(applyReviewEnv([]string{"ANTHROPIC_API_KEY=sk-ant-test"},
		reviewEnvInputs{passes: reviewPasses, baseCommits: `{"0-a/b":"abc"}`, round: 1}))
	if byKey["REVIEW_READONLY_MOUNTS"] != "1" {
		t.Errorf("REVIEW_READONLY_MOUNTS = %q, want 1 on a review run", byKey["REVIEW_READONLY_MOUNTS"])
	}
	// A fix run writes the checkouts, and nothing about it is read-only.
	if _, ok := envMap(applyMustFixEnv([]string{"STEP_PROMPT=the original"}, "the fix prompt"))["REVIEW_READONLY_MOUNTS"]; ok {
		t.Error("a fix run was told its repositories are read-only — it exists to edit them")
	}
}

// findMount returns the mount for a container path, failing the test when there
// is none.
func findMount(t *testing.T, mounts []mount.Mount, target string) mount.Mount {
	t.Helper()
	for _, m := range mounts {
		if m.Target == target {
			return m
		}
	}
	t.Fatalf("no mount at %s:\n%+v", target, mounts)
	return mount.Mount{}
}

// A fix run is an ordinary batch run: the Step's original prompt plus the
// must-fix findings, and nothing from the reviewer's transcript.
func TestMustFixPromptCarriesTheOriginalPromptAndOnlyTheMustFixFindings(t *testing.T) {
	prompt := buildMustFixPrompt("Implement the export endpoint.", []reviewFindingOutput{{
		Parameter: "security", Severity: "high", Location: "0-acme/api/handler.go:41",
		What:    "the new /export handler does not check the caller's session",
		Why:     "any unauthenticated caller can read another org's data",
		MustFix: true,
	}})

	if !strings.HasPrefix(prompt, "Implement the export endpoint.") {
		t.Errorf("the fix prompt does not start with the Step's own prompt:\n%s", prompt)
	}
	for _, want := range []string{"security", "high", "handler.go:41", "does not check the caller's session", "any unauthenticated caller"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the fix prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// A fix run keeps the implement run's turn cap — the Task's own MaxTurns —
// rather than a smaller one of its own. A ceiling below the user's would fail
// the Step, and discard a finished implementation, when a bounded cleanup ran
// out of room.
func TestApplyMustFixEnvReplacesThePromptAndKeepsTheTaskTurnCap(t *testing.T) {
	byKey := envMap(applyMustFixEnv([]string{
		"STEP_PROMPT=the original",
		"MAX_TURNS=80",
		"ANTHROPIC_API_KEY=sk-ant-test",
	}, "the fix prompt"))

	if byKey["STEP_PROMPT"] != "the fix prompt" {
		t.Errorf("STEP_PROMPT = %q, want the fix prompt", byKey["STEP_PROMPT"])
	}
	if byKey["MAX_TURNS"] != "80" {
		t.Errorf("MAX_TURNS = %q, want the implement run's own cap carried through", byKey["MAX_TURNS"])
	}
	if _, ok := envMap(applyMustFixEnv([]string{"STEP_PROMPT=x"}, "fix"))["MAX_TURNS"]; ok {
		t.Error("a fix run invented a MAX_TURNS the implement run did not have")
	}
	if _, ok := byKey["AGENT_MODE"]; ok {
		t.Error("a fix run must be an ordinary batch run, with no AGENT_MODE override")
	}
}

// A fix run is an implement run and gets the implement run's tool channel.
// Without it, an agent asked to fix a finding in code that deploys a preview
// loses the tools the original run used to do that work, and fails for a reason
// that has nothing to do with the finding.
func TestMustFixEnvKeepsTheAgentsToolSocket(t *testing.T) {
	byKey := envMap(applyMustFixEnv([]string{
		"STEP_PROMPT=the original",
		agentMCPSocketEnvVar + "=" + agentboxMCPSocketInContainer,
	}, "the fix prompt"))

	if byKey[agentMCPSocketEnvVar] != agentboxMCPSocketInContainer {
		t.Errorf("%s = %q, want the implement run's socket", agentMCPSocketEnvVar, byKey[agentMCPSocketEnvVar])
	}
}

// The review container gets NO tool socket: review needs no runner tools, and
// a channel the reviewer cannot use is a channel it cannot misuse.
func TestReviewSpawnEnvDropsTheToolSocket(t *testing.T) {
	byKey := envMap(applyReviewEnv([]string{
		"ANTHROPIC_API_KEY=sk-ant-test",
		agentMCPSocketEnvVar + "=" + agentboxMCPSocketInContainer,
	}, reviewEnvInputs{passes: reviewPasses, baseCommits: `{"0-a/b":"abc"}`, round: 1}))

	if _, ok := byKey[agentMCPSocketEnvVar]; ok {
		t.Errorf("the review spawn env carries %s — a reviewer needs no runner tools", agentMCPSocketEnvVar)
	}
}

// Advisory annotates EVERYTHING. The threshold still exists and a finding may
// well meet it, but a Task whose review is advisory opted out of the must-fix
// half — marking the finding must-fix would make its pull request say the
// change is blocked when nothing is blocking it.
func TestClassifyMarksNothingMustFixUnderAdvisory(t *testing.T) {
	stage := &reviewStage{participation: participationAdvisory, thresholds: map[uint]uint{1: 4}}

	findings := stage.classify(agentResult{ReviewResult: &reviewResult{Findings: []reviewFinding{
		{Parameter: "security", Severity: "critical", What: "no session check", Location: "a.go:1"},
	}}})

	if len(findings) != 1 || findings[0].MustFix {
		t.Errorf("findings = %+v, want the finding annotated rather than gated", findings)
	}
}

// --- must-fix classification ------------------------------------------------

// MustFixOpen is the runner's decision, never the agent's. These pin the
// mapping: names through the wire mirror, at-or-above against the stamped
// thresholds, and annotate-rather-than-gate for anything unreadable.
func TestIsMustFixAppliesTheStampedThresholds(t *testing.T) {
	// Security and Correctness must-fix at High — the platform default.
	stage := &reviewStage{thresholds: map[uint]uint{1: 4, 2: 4}}

	for _, tc := range []struct {
		name      string
		parameter string
		severity  string
		want      bool
	}{
		{"a high security finding is must-fix", "security", "high", true},
		{"a critical security finding is must-fix", "Security", "CRITICAL", true},
		{"a medium security finding is annotated", "security", "medium", false},
		{"spelling and separators do not matter", "  Correctness ", "High", true},
		{"a parameter with no threshold is annotated", "performance", "critical", false},
		{"an unreadable parameter is annotated, never must-fix", "banana", "critical", false},
		{"an unreadable severity is annotated, never must-fix", "security", "catastrophic", false},
		{"an absent severity is annotated", "security", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stage.isMustFix(reviewFinding{Parameter: tc.parameter, Severity: tc.severity})
			if got != tc.want {
				t.Errorf("isMustFix(%q, %q) = %t, want %t", tc.parameter, tc.severity, got, tc.want)
			}
		})
	}
}

// A zero threshold is annotate-only and must not be met by anything, including
// Critical. It is NOT "Info and above".
func TestIsMustFixTreatsAZeroThresholdAsAnnotateOnly(t *testing.T) {
	stage := &reviewStage{thresholds: map[uint]uint{1: 0}}
	if stage.isMustFix(reviewFinding{Parameter: "security", Severity: "critical"}) {
		t.Error("a Critical finding met a zero threshold — zero means nothing must be fixed")
	}
}

func TestDecodeMustFixThresholds(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewMustFixThresholds, `{"1":4,"2":4}`)

	got := decodeMustFixThresholds(parameters, io.Discard)
	if got[1] != 4 || got[2] != 4 || len(got) != 2 {
		t.Errorf("thresholds = %v, want {1:4, 2:4}", got)
	}

	// An unreadable payload gates nothing rather than inventing a threshold
	// nobody wrote and holding a pull request on it.
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewMustFixThresholds, "not json")
	if got := decodeMustFixThresholds(parameters, io.Discard); len(got) != 0 {
		t.Errorf("thresholds = %v, want none for an unreadable payload", got)
	}
}

// Findings pair across rounds on Key, with a synthesised key when the producer
// omitted one — so "fixed" and "still open" are real distinctions rather than
// list arithmetic.
func TestClassifyMarksFixedStillOpenAndNew(t *testing.T) {
	stage := &reviewStage{participation: participationOn, thresholds: map[uint]uint{1: 4}}
	stage.rememberOpenMustFix([]reviewFindingOutput{
		{Key: "sec-1", Parameter: "security", Severity: "high", What: "was open", MustFix: true},
		{Key: "sec-2", Parameter: "security", Severity: "high", What: "also open", MustFix: true},
	})

	findings := stage.classify(agentResult{ReviewResult: &reviewResult{Findings: []reviewFinding{
		{Key: "sec-1", Parameter: "security", Severity: "high", What: "was open"},  // still open
		{Key: "sec-3", Parameter: "security", Severity: "low", What: "new, minor"}, // new, annotated
	}}})

	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2", findings)
	}
	if !findings[0].MustFix || findings[1].MustFix {
		t.Errorf("must-fix marking is wrong: %+v", findings)
	}
	if len(stage.fixedInLoop) != 1 || stage.fixedInLoop[0].Key != "sec-2" {
		t.Errorf("fixedInLoop = %+v, want the finding that disappeared", stage.fixedInLoop)
	}
}

func TestFindingKeyIsSynthesisedWhenTheProducerOmitsOne(t *testing.T) {
	long := strings.Repeat("x", 200)
	a := findingKey(reviewFindingOutput{Parameter: "Security", Location: "a.go:1", What: long})
	b := findingKey(reviewFindingOutput{Parameter: "security", Location: "a.go:1", What: long + " and more text"})
	if a != b {
		t.Errorf("the same finding reworded past 80 runes produced two keys:\n%q\n%q", a, b)
	}
	c := findingKey(reviewFindingOutput{Parameter: "security", Location: "b.go:2", What: long})
	if a == c {
		t.Error("two findings in different files produced the same key")
	}
}

// --- a review round that does not complete ----------------------------------

// THE STEP IS NEVER FAILED FOR A REVIEW-RUN FAILURE. Each shape below ends the
// loop, records why, and lets the Step commit and open its pull request.
func TestReviewRoundFailureNamesEachShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result agentResult
		err    error
		want   string
	}{
		{
			name:   "a non-success status",
			result: agentResult{Status: "failure", Error: "claude exited with error: boom"},
			want:   "boom",
		},
		{
			name: "a missing result.json",
			err:  errors.New("error reading /work/.agentbox-output/result.json: no such file or directory"),
			want: "no such file",
		},
		{
			name:   "a result.json with no review_result",
			result: agentResult{Status: "success"},
			want:   "no review_result",
		},
		{
			name:   "an agentbox image that predates review mode",
			result: agentResult{Status: "failure", Error: `config error: invalid AGENT_MODE "review": must be "batch", "interactive" or "review"`},
			want:   "predates the Review stage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewRoundFailure(tc.result, tc.err)
			if got == "" {
				t.Fatal("reviewRoundFailure reported success for a round that produced nothing usable")
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", got, tc.want)
			}
		})
	}

	// And a round that DID complete reports no failure.
	if got := reviewRoundFailure(agentResult{Status: "success", ReviewResult: &reviewResult{}}, nil); got != "" {
		t.Errorf("reviewRoundFailure on a good round = %q, want empty", got)
	}
}

// A REVIEW THAT EXAMINED NOTHING IS A FAILED ROUND, NOT A CLEAN ONE. An agent
// that ran and checked no parameter found nothing because it looked at nothing,
// and recorded as a completed round it is indistinguishable on the pull request
// from a change a reviewer read and passed.
func TestAReviewThatRanAndExaminedNothingIsAFailedRound(t *testing.T) {
	ran := agentResult{Status: "success", Turns: 12, ReviewResult: &reviewResult{Coverage: []reviewCoverage{
		{Parameter: "security", State: "skipped", Reason: "every command failed: bwrap: No permissions to create new namespace"},
		{Parameter: "correctness", State: "not checked", Reason: "the pass did not run"},
	}}}

	got := reviewRoundFailure(ran, nil)
	if !strings.Contains(got, "the reviewer ran but could not examine the change") {
		t.Errorf("reason = %q, want a round that examined nothing recorded as failed", got)
	}
	// The reviewer's OWN account of why, so the record names the cause rather
	// than restating that nothing was checked.
	if !strings.Contains(got, "No permissions to create new namespace") {
		t.Errorf("reason = %q, want the first skipped pass's reason carried into it", got)
	}

	// One checked parameter is a review. The state is a free string from an
	// agent, so it is read case-insensitively.
	for _, state := range []string{"checked", "Checked", " CHECKED "} {
		partial := agentResult{Status: "success", Turns: 12, ReviewResult: &reviewResult{Coverage: []reviewCoverage{
			{Parameter: "security", State: state},
			{Parameter: "correctness", State: "skipped", Reason: "out of turns"},
		}}}
		if got := reviewRoundFailure(partial, nil); got != "" {
			t.Errorf("a round that checked %q was recorded as failed: %q", state, got)
		}
	}

	// TURNS 0 IS UNCHANGED: agentbox's cost gate ran no agent at all — a
	// documentation-only change, say — and that round is exactly as clean as it
	// was before.
	noAgent := ran
	noAgent.Turns = 0
	if got := reviewRoundFailure(noAgent, nil); got != "" {
		t.Errorf("a round where no agent ran was recorded as failed: %q", got)
	}
}

// ...and the loop treats it as any other failed round: recorded with the reason,
// no second round, and the Step still commits and opens its pull request.
func TestTheLoopEndsOnAReviewThatExaminedNothing(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "the implementation\n")
	stage := fixLoopStage(workDir, io.Discard)
	rounds := 0
	stage.runReview = func(int) (agentResult, error) {
		rounds++
		return agentResult{Status: "success", Turns: 12, ReviewResult: &reviewResult{Coverage: []reviewCoverage{
			{Parameter: "security", State: "skipped", Reason: "every command failed: bwrap: No permissions to create new namespace"},
			{Parameter: "correctness", State: "not checked", Reason: "the pass did not run"},
		}}}, nil
	}

	out, err := stage.run()
	if err != nil {
		t.Fatalf("a review that examined nothing failed the Step: %s", err)
	}
	if rounds != 1 {
		t.Errorf("the loop ran %d round(s), want it to end on the failed one", rounds)
	}
	review := readReviewFromJobOutput(out)
	if review == nil || len(review.Rounds) != 1 {
		t.Fatalf("review = %+v, want the round recorded", review)
	}
	if review.Rounds[0].Completed {
		t.Error("a round that examined nothing was recorded as completed — the pull request would read as reviewed")
	}
	if !strings.Contains(review.Rounds[0].Error, "the reviewer ran but could not examine the change") ||
		!strings.Contains(review.Rounds[0].Error, "No permissions to create new namespace") {
		t.Errorf("round error = %q, want the named reason", review.Rounds[0].Error)
	}

	// A round where no agent ran keeps the behaviour it had: completed, with
	// whatever coverage it reported.
	stage = fixLoopStage(workDir, io.Discard)
	stage.runReview = func(int) (agentResult, error) {
		return agentResult{Status: "success", ReviewResult: &reviewResult{Coverage: []reviewCoverage{
			{Parameter: "security", State: "skipped", Reason: "the change is documentation only"},
		}}}, nil
	}
	out, err = stage.run()
	if err != nil {
		t.Fatalf("run: %s", err)
	}
	review = readReviewFromJobOutput(out)
	if review == nil || len(review.Rounds) != 1 || !review.Rounds[0].Completed {
		t.Errorf("review = %+v, want a round where no agent ran left as it was", review)
	}
}

// A failed round asserts nothing: it records coverage as NotChecked for every
// parameter with the reason, and opens no must-fix finding of its own.
func TestRecordFailedRoundRecordsNotCheckedCoverageForEveryParameter(t *testing.T) {
	stage := &reviewStage{}
	stage.recordFailedRound(1, "the review run produced no review_result", agentResult{})

	if len(stage.rounds) != 1 || stage.rounds[0].Completed {
		t.Fatalf("rounds = %+v, want one round marked not completed", stage.rounds)
	}
	coverage := stage.rounds[0].Coverage
	if len(coverage) != 8 {
		t.Fatalf("coverage has %d entries, want all eight parameters", len(coverage))
	}
	for _, c := range coverage {
		if c.State != "not checked" || !strings.Contains(c.Reason, "review run failed") {
			t.Errorf("coverage entry %+v, want not checked with the failure reason", c)
		}
	}
	if len(stage.openMustFix) != 0 {
		t.Errorf("a failed round opened %d must-fix finding(s)", len(stage.openMustFix))
	}
}

// --- participation ----------------------------------------------------------

// Off is the answer for every Job created before this stage existed, so a
// missing parameter must read as Off rather than as On.
func TestReadReviewParticipationDefaultsToOff(t *testing.T) {
	var logs strings.Builder
	if got := readReviewParticipation(map[string]interface{}{}, &logs); got != participationOff {
		t.Errorf("participation = %d, want Off for a Job with no review parameter", got)
	}
	// An ABSENT parameter is the ordinary pre-stage Job and says nothing.
	if logs.Len() != 0 {
		t.Errorf("a Job with no review parameter logged a warning: %s", logs.String())
	}
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[int64](parameters, parameters_enums.ReviewParticipation, int64(participationAdvisory))
	if got := readReviewParticipation(parameters, io.Discard); got != participationAdvisory {
		t.Errorf("participation = %d, want Advisory", got)
	}
	jobs.SetParameterValue[int64](parameters, parameters_enums.ReviewParticipation, int64(99))
	if got := readReviewParticipation(parameters, io.Discard); got != participationOff {
		t.Errorf("participation = %d, want Off for a value outside the enum", got)
	}
}

// A parameter that is PRESENT but not a usable int64 is a stamping bug, not an
// opt-out. Read silently as Off it would turn the whole stage off across every
// Task with nothing in any log to say it had ever been on.
func TestReadReviewParticipationLogsAPresentButUnreadableValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value interface{}
	}{
		{"a float64 off the wire", float64(1)},
		{"a string", "on"},
		{"an out-of-range number", int64(42)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := parameters_enums.ReviewParticipation.Key()
			if err != nil {
				t.Fatalf("ReviewParticipation has no persisted key: %s", err)
			}
			parameters := map[string]interface{}{key: tc.value}
			var logs strings.Builder
			if got := readReviewParticipation(parameters, &logs); got != participationOff {
				t.Errorf("participation = %d, want Off", got)
			}
			if logs.Len() == 0 {
				t.Errorf("a present-but-unreadable participation value (%v) was read as Off in silence", tc.value)
			}
		})
	}
}

// A Task whose review participation is Off behaves exactly as it did before
// this stage existed: nothing is spawned, nothing is recorded.
func TestRunReturnsImmediatelyWhenParticipationIsOff(t *testing.T) {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[int64](parameters, parameters_enums.ReviewParticipation, int64(participationOff))

	out, err := (&RunReviewStage{}).Run(parameters, io.Discard)
	if err != nil {
		t.Fatalf("Run: %s", err)
	}
	if readReviewFromJobOutput(out) != nil {
		t.Error("a Task with review Off recorded a review block")
	}
}

// --- the JobOutput envelope -------------------------------------------------

func TestBaseCommitsRoundTripThroughJobOutput(t *testing.T) {
	parameters := map[string]interface{}{}
	if err := mergeBaseCommitsIntoJobOutput(parameters, []baseCommitOutput{
		{Index: 0, Name: "acme/api", Dir: "0-acme/api", CommitSHA: "abc123"},
		{Index: 1, Name: "acme/web", Dir: "1-acme/web"}, // no resolvable HEAD
	}); err != nil {
		t.Fatalf("mergeBaseCommitsIntoJobOutput: %s", err)
	}

	got := readBaseCommitsFromJobOutput(parameters)
	if got["0-acme/api"] != "abc123" {
		t.Errorf("base commits = %v, want the recorded one", got)
	}
	if _, ok := got["1-acme/web"]; ok {
		t.Error("a repository with no recorded commit contributed a baseline")
	}
}

// The implement run, every fix run and every review run are one Step's work.
// Overwriting here would report the Step's usage as the LAST run's alone.
func TestJobOutputAccumulatesAcrossTheStepsRuns(t *testing.T) {
	parameters := map[string]interface{}{}
	implementCost := 0.40
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", Turns: 20, PRTitle: "Add OAuth login",
		ChangesSummary: "Added the login endpoint.",
		FilesChanged:   []string{"0-acme/api/handler.go"},
		TokenUsage:     tokenUsage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 10, CacheCreationTokens: 5},
		CostUSD:        &implementCost,
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}
	// A review round: usage and cost only, never the prose.
	reviewCost := 0.10
	if err := accumulateReviewRunUsage(parameters, parameters, agentResult{
		Status: "success", Turns: 4,
		ChangesSummary: "I reviewed the change and found one thing.",
		TokenUsage:     tokenUsage{InputTokens: 500, OutputTokens: 50},
		CostUSD:        &reviewCost,
	}); err != nil {
		t.Fatalf("accumulate: %s", err)
	}
	// A fix run that emitted no pr_title.
	fixCost := 0.20
	if err := mergeAgentResultIntoJobOutput(parameters, agentResult{
		Status: "success", Turns: 6,
		ChangesSummary: "Added the missing session check.",
		FilesChanged:   []string{"0-acme/api/handler.go", "0-acme/api/auth.go"},
		TokenUsage:     tokenUsage{InputTokens: 800, OutputTokens: 80},
		CostUSD:        &fixCost,
	}); err != nil {
		t.Fatalf("merge: %s", err)
	}

	data := decodeJobOutput(t, parameters)
	if data.Agent.Turns != 30 {
		t.Errorf("turns = %d, want 20+4+6", data.Agent.Turns)
	}
	if data.Agent.TokenUsage.InputTokens != 2300 || data.Agent.TokenUsage.OutputTokens != 230 {
		t.Errorf("token usage = %+v, want every run's counted", data.Agent.TokenUsage)
	}
	if data.Agent.PRTitle != "Add OAuth login" {
		t.Errorf("pr_title = %q — a fix run that emitted none must not erase the implementer's", data.Agent.PRTitle)
	}
	if len(data.Agent.FilesChanged) != 2 {
		t.Errorf("files changed = %v, want the union", data.Agent.FilesChanged)
	}
	if !strings.Contains(data.Agent.ChangesSummary, "Added the login endpoint.") ||
		!strings.Contains(data.Agent.ChangesSummary, "Added the missing session check.") {
		t.Errorf("changes summary = %q, want both runs' accounts", data.Agent.ChangesSummary)
	}
	if strings.Contains(data.Agent.ChangesSummary, "I reviewed the change") {
		t.Errorf("the reviewer's prose reached the commit message: %q", data.Agent.ChangesSummary)
	}
	if data.Cost == nil || fmt.Sprintf("%.2f", data.Cost.USD) != "0.70" {
		t.Errorf("cost = %+v, want the three runs summed", data.Cost)
	}
}

// --- helpers ----------------------------------------------------------------

func decodeJobOutput(t *testing.T, parameters map[string]interface{}) jobOutputData {
	t.Helper()
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil {
		t.Fatalf("no job output: %s", err)
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("job output is not valid JSON: %s", err)
	}
	return data
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		out[key] = value
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %s", path, err)
	}
	return string(b)
}

// assertOwnedByAgentbox checks the fresh round directory is writable by the
// container's UID through the bind mount.
func assertOwnedByAgentbox(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %s", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("ownership is not inspectable on this platform")
	}
	if int(stat.Uid) != commandUtils.AgentboxUID || int(stat.Gid) != commandUtils.AgentboxGID {
		t.Errorf("%s is owned by %d:%d, want %d:%d — the container could not write its result through the bind mount",
			path, stat.Uid, stat.Gid, commandUtils.AgentboxUID, commandUtils.AgentboxGID)
	}
}

// A second round's findings are not an undifferentiated list: a finding the
// previous round already reported means the fix did not work, and one it did
// not means the fix introduced something else. Those are different things for
// a reader to see.
func TestClassifyMarksFindingsNewOnlyAfterTheFirstRound(t *testing.T) {
	stage := &reviewStage{participation: participationOn, thresholds: map[uint]uint{1: 4}}

	first := stage.classify(agentResult{ReviewResult: &reviewResult{Findings: []reviewFinding{
		{Key: "sec-1", Parameter: "security", Severity: "high", What: "no session check"},
	}}})
	if first[0].New {
		t.Error("a first-round finding was marked new — everything is new in the first round, so the flag means nothing there")
	}
	stage.recordRound(1, first, agentResult{})
	stage.rememberOpenMustFix(mustFixOnly(first))

	second := stage.classify(agentResult{ReviewResult: &reviewResult{Findings: []reviewFinding{
		{Key: "sec-1", Parameter: "security", Severity: "high", What: "no session check"},
		{Key: "sec-2", Parameter: "security", Severity: "high", What: "the fix logs the token"},
	}}})
	if second[0].New {
		t.Errorf("%+v was marked new — it was open in round 1 and is still open", second[0])
	}
	if !second[1].New {
		t.Errorf("%+v was not marked new — round 1 never reported it", second[1])
	}
}

// The base-commit key is the repository's path AS THE CONTAINER SEES IT.
// A repository lands in "<idx>-<owner>/<repo>", so the key is two segments;
// the directory's base name would name a path that does not exist inside the
// container, and the review would silently find no diff for that repository.
func TestBaseCommitDirIsThePathTheContainerSees(t *testing.T) {
	baseDir := commandUtils.GetTaskRepositoriesBaseDir("org-1", "task-1")
	repoDir := commandUtils.GetTaskRepositoryDir("org-1", "task-1", 0, "deployment-io/kit")

	if got := repoDirRelativeToWorkDir(repoDir, baseDir); got != "0-deployment-io/kit" {
		t.Errorf("dir = %q, want the owner/repo path relative to /work", got)
	}
}

// Every round records the turns it spent, completed or not. That number is
// the evidence for the review turn cap: without it nobody can tell a stage
// whose reviewer finished with room to spare from one that ran out of it.
func TestRoundsRecordTheTurnsTheySpent(t *testing.T) {
	stage := &reviewStage{participation: participationOn, thresholds: map[uint]uint{1: 4}}
	stage.recordRound(1, nil, agentResult{Turns: 7})
	stage.recordFailedRound(2, "no report", agentResult{Turns: 20})
	if len(stage.rounds) != 2 || stage.rounds[0].Turns != 7 || stage.rounds[1].Turns != 20 {
		t.Errorf("rounds = %+v, want turns 7 then 20 carried onto the record", stage.rounds)
	}
}

func TestEnvValueReadsTheLastOccurrence(t *testing.T) {
	env := []string{"A=1", "MAX_TURNS=30", "B=x=y", "MAX_TURNS=80"}
	if got := envValue(env, "MAX_TURNS"); got != "80" {
		t.Errorf("envValue(MAX_TURNS) = %q, want the last occurrence 80", got)
	}
	if got := envValue(env, "B"); got != "x=y" {
		t.Errorf("envValue(B) = %q, want the value with its own '=' intact", got)
	}
	if got := envValue(env, "MISSING"); got != "" {
		t.Errorf("envValue(MISSING) = %q, want empty", got)
	}
}

// A repository with no recorded start commit (a new, empty one) is still
// committed and pushed, so it is mounted read-only like the rest.
func TestTheReadOnlySetIncludesARepositoryWithNoStartCommit(t *testing.T) {
	workDir := t.TempDir()
	for _, dir := range []string{"0-acme/api", "1-acme/new"} {
		writeFile(t, filepath.Join(workDir, dir, "README.md"), "x\n")
	}
	stage := &reviewStage{baseCommits: map[string]string{"0-acme/api": "abc"}, repoDirs: []string{"0-acme/api", "1-acme/new"}}
	got, complete := readOnlyRepoDirs(workDir, stage.allRepositoryDirs(), io.Discard)
	if !complete || len(got) != 2 || got[1] != "1-acme/new" {
		t.Errorf("read-only dirs = %v (complete %v), want both, including the repository with no start commit", got, complete)
	}
}

// A round that reported findings examined the change, whatever it called its
// coverage; failing it would discard the findings.
func TestARoundWithFindingsIsNotFailedForItsCoverageLabels(t *testing.T) {
	result := agentResult{Status: "success", Turns: 6, ReviewResult: &reviewResult{
		Findings: []reviewFinding{{Key: "sec-1", Parameter: "security", Severity: "high", What: "x"}},
		Coverage: []reviewCoverage{{Parameter: "security", State: "complete"}},
	}}
	if reason := reviewRoundFailure(result, nil); reason != "" {
		t.Errorf("a round with findings was failed: %q", reason)
	}
	result.ReviewResult.Findings = nil
	if reason := reviewRoundFailure(result, nil); reason == "" {
		t.Error("a round with no findings and nothing checked was not failed")
	}
}

func TestWithoutEnvRemovesEveryOccurrence(t *testing.T) {
	got := withoutEnv([]string{"A=1", "REVIEW_READONLY_MOUNTS=1", "B=2", "REVIEW_READONLY_MOUNTS=1"}, "REVIEW_READONLY_MOUNTS")
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("withoutEnv = %v", got)
	}
}
