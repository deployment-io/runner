package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/llm_provider_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
)

func TestGetMapsRunQAStage(t *testing.T) {
	cmd, err := Get(commands_enums.RunQAStage)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cmd.(*RunQAStage); !ok {
		t.Errorf("Get(RunQAStage) = %T, want *RunQAStage", cmd)
	}
}

// --- fixtures ----------------------------------------------------------------

const qaTestSpec = "## Acceptance criteria\n- The home page shows a Sign up button\n- The pricing page lists three plans"

// qaTestRepo is a checked-out repository with one commit: its start-of-run
// commit.
func qaTestRepo(t *testing.T, workDir, dir string) string {
	t.Helper()
	repoDir := filepath.Join(workDir, dir)
	writeFile(t, filepath.Join(repoDir, "index.html"), "<h1>home</h1>\n")
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatal(err)
	}
	return commitEverything(t, repoDir, "start")
}

// qaTestStage is a QA stage over the given repositories (dir → base commit),
// with every container run replaced: QA rounds answer from rounds in order,
// every repository can be previewed, and the cache volume is still there.
func qaTestStage(t *testing.T, workDir string, bases []baseCommitOutput, logs io.Writer) *qaStage {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	parameters := implementerJobParameters(t)
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewSpec, qaTestSpec)
	encoded, err := json.Marshal(jobOutputData{SchemaVersion: jobOutputSchemaVersion, BaseCommits: bases})
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(encoded))
	return &qaStage{
		ctx:          commandUtils.TaskJobContext{OrganizationID: "org-1", TaskID: "task-1"},
		parameters:   parameters,
		workDirHost:  workDir,
		logsWriter:   logs,
		deadline:     time.Now().Add(qaStageBudget),
		checkPreview: func(string) agenttools.RunnerBuiltPreviewCheck { return agenttools.RunnerBuiltPreviewCheck{OK: true} },
		checkBackend: func(dir string) error {
			return fmt.Errorf("deployment.io doesn't deploy %s as a web service, so it has no backend preview.", dir)
		},
		cacheExists: func(string) bool { return true },
		reviewer:    func(rs *reviewStage) { rs.copyTree = testCopyTree },
		// Never touch the runner's Docker daemon from a test.
		prefetchBrowser: func(context.Context, string, io.Writer) {},
	}
}

func qaRounds(results ...agentResult) (func(int) (agentResult, error), *[]int) {
	var calls []int
	return func(round int) (agentResult, error) {
		calls = append(calls, round)
		if len(calls) > len(results) {
			return agentResult{}, fmt.Errorf("unexpected QA round %d", round)
		}
		return results[len(calls)-1], nil
	}, &calls
}

func qaCompleted(criteria ...qaCriterion) agentResult {
	cost := 0.25
	return agentResult{Status: "success", Turns: 7, CostUSD: &cost,
		TokenUsage: tokenUsage{InputTokens: 100, OutputTokens: 10},
		QAResult:   &qaResult{Status: tasks.QAStatusCompleted, Criteria: criteria}}
}

var (
	signUpPassed  = qaCriterion{Criterion: "The home page shows a Sign up button", State: tasks.QACriterionPassed}
	signUpFailed  = qaCriterion{Criterion: "The home page shows a Sign up button", State: tasks.QACriterionFailed, Reason: "no button on /"}
	pricingPassed = qaCriterion{Criterion: "The pricing page lists three plans", State: tasks.QACriterionPassed}
)

// changedQAStage is a stage over one repository the implementer changed.
func changedQAStage(t *testing.T, logs io.Writer) (*qaStage, string) {
	t.Helper()
	workDir := t.TempDir()
	base := qaTestRepo(t, workDir, "0-acme-site")
	writeFile(t, filepath.Join(workDir, "0-acme-site", "index.html"), "<h1>home</h1><button>Sign up</button>\n")
	return qaTestStage(t, workDir, []baseCommitOutput{{Index: 0, Dir: "0-acme-site", CommitSHA: base}}, logs), workDir
}

// --- the gate ----------------------------------------------------------------

func TestQAGateNoChangedRepositoryRunsNothingAndRecordsNothing(t *testing.T) {
	workDir := t.TempDir()
	base := qaTestRepo(t, workDir, "0-acme-site")
	stage := qaTestStage(t, workDir, []baseCommitOutput{{Index: 0, Dir: "0-acme-site", CommitSHA: base}}, nil)
	run, calls := qaRounds()
	stage.runQA = run
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("QA ran %d round(s) on an unchanged tree", len(*calls))
	}
	if qa := readQAFromJobOutput(stage.parameters); qa != nil {
		t.Errorf("an unchanged tree recorded a QA result: %+v", qa)
	}
}

// The QA browser's download starts once, for the Job's image, before round 1.
func TestQAStagePrefetchesTheBrowserBeforeRound1(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	const jobBrowser = "docker.io/deploymenthq/verify-browser:1.4.0"
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.VerifyBrowserImage, jobBrowser)
	var events []string
	stage.prefetchBrowser = func(ctx context.Context, jobRef string, _ io.Writer) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("prefetch's context has no deadline")
		}
		events = append(events, "prefetch "+jobRef)
	}
	stage.runQA = func(round int) (agentResult, error) {
		events = append(events, fmt.Sprintf("qa %d", round))
		return qaCompleted(signUpPassed, pricingPassed), nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"prefetch " + jobBrowser, "qa 1"}; !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func TestQAStageNothingChangedDoesNotPrefetch(t *testing.T) {
	workDir := t.TempDir()
	base := qaTestRepo(t, workDir, "0-acme-site")
	logs := &lockedBuffer{}
	stage := qaTestStage(t, workDir, []baseCommitOutput{{Index: 0, Dir: "0-acme-site", CommitSHA: base}}, logs)
	stage.runQA, _ = qaRounds()
	stage.prefetchBrowser = func(context.Context, string, io.Writer) { t.Error("prefetched with nothing to check") }
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "no repository changed") {
		t.Errorf("logs = %q, want the no-change line", logs.String())
	}
}

func TestQAGateACommitTheAgentMadeCountsAsAChange(t *testing.T) {
	workDir := t.TempDir()
	base := qaTestRepo(t, workDir, "0-acme-site")
	writeFile(t, filepath.Join(workDir, "0-acme-site", "pricing.html"), "<p>plans</p>\n")
	commitEverything(t, filepath.Join(workDir, "0-acme-site"), "the agent's own commit")
	stage := qaTestStage(t, workDir, []baseCommitOutput{{Index: 0, Dir: "0-acme-site", CommitSHA: base}}, nil)
	if got := stage.changedRepositories(readQARepositoriesFromJobOutput(stage.parameters)); !reflect.DeepEqual(got, []string{"0-acme-site"}) {
		t.Errorf("changed = %v, want the repository the agent committed in", got)
	}
}

func TestQAGateOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		arrange    func(t *testing.T, stage *qaStage)
		noBases    bool
		wantStatus string
		wantReason string
	}{
		{
			name:       "no base commits",
			noBases:    true,
			wantStatus: tasks.QAStatusCouldNotRun,
			wantReason: "no start-of-run commit was recorded",
		},
		{
			name: "changed but not buildable",
			arrange: func(_ *testing.T, stage *qaStage) {
				stage.checkPreview = func(string) agenttools.RunnerBuiltPreviewCheck {
					return agenttools.RunnerBuiltPreviewCheck{Reason: "no package.json build script"}
				}
			},
			wantStatus: tasks.QAStatusSkipped,
			wantReason: "no static site or backend service in this change",
		},
		{
			name: "every lookup failed",
			arrange: func(_ *testing.T, stage *qaStage) {
				stage.checkPreview = func(dir string) agenttools.RunnerBuiltPreviewCheck {
					return agenttools.RunnerBuiltPreviewCheck{LookupErr: fmt.Errorf("look up how deployment.io deploys %s: timeout", dir)}
				}
			},
			wantStatus: tasks.QAStatusCouldNotRun,
			wantReason: "look up how deployment.io deploys 0-acme-site: timeout",
		},
		{
			name: "a QA model without credentials",
			arrange: func(_ *testing.T, stage *qaStage) {
				jobs.SetParameterValue[string](stage.parameters, parameters_enums.QAAgentType, "codex")
				jobs.SetParameterValue[string](stage.parameters, parameters_enums.QAModel, "gpt-5.5")
				stage.qaParams, stage.qaFailure = qaParameters(stage.parameters)
			},
			wantStatus: tasks.QAStatusCouldNotRun,
			wantReason: "no credentials for the QA model gpt-5.5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			stage, workDir := changedQAStage(t, &logs)
			if tc.noBases {
				stage = qaTestStage(t, workDir, []baseCommitOutput{{Index: 0, Dir: "0-acme-site"}}, &logs)
			}
			if tc.arrange != nil {
				tc.arrange(t, stage)
			}
			run, calls := qaRounds()
			stage.runQA = run
			if _, err := stage.run(); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 0 {
				t.Errorf("the gate spawned %d QA round(s)", len(*calls))
			}
			qa := readQAFromJobOutput(stage.parameters)
			if qa == nil {
				t.Fatal("the gate recorded no QA result")
			}
			if qa.Status != tc.wantStatus || qa.StatusReason != tc.wantReason {
				t.Errorf("QA = %s %q, want %s %q", qa.Status, qa.StatusReason, tc.wantStatus, tc.wantReason)
			}
			if !strings.Contains(logs.String(), "--- QA stage ---") || !strings.Contains(logs.String(), tc.wantReason) {
				t.Errorf("the job log does not carry the gate's outcome:\n%s", logs.String())
			}
		})
	}
}

func TestQARepositoriesAreTheChangedBuildableOnes(t *testing.T) {
	workDir := t.TempDir()
	var bases []baseCommitOutput
	for i, dir := range []string{"0-site", "1-api", "2-docs"} {
		bases = append(bases, baseCommitOutput{Index: i, Dir: dir, CommitSHA: qaTestRepo(t, workDir, dir)})
	}
	// 0-site and 1-api changed; only 0-site is a static site. 2-docs is
	// unchanged.
	writeFile(t, filepath.Join(workDir, "0-site", "new.html"), "new\n")
	writeFile(t, filepath.Join(workDir, "1-api", "main.go"), "package main\n")
	stage := qaTestStage(t, workDir, bases, nil)
	var checked []string
	stage.checkPreview = func(dir string) agenttools.RunnerBuiltPreviewCheck {
		checked = append(checked, dir)
		return agenttools.RunnerBuiltPreviewCheck{OK: dir != "1-api", Reason: "not a static site"}
	}
	var env []string
	stage.runQA = func(int) (agentResult, error) {
		var err error
		env, err = stage.qaSpawnEnv()
		return qaCompleted(signUpPassed), err
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checked, []string{"0-site", "1-api"}) {
		t.Errorf("checked %v, want only the changed repositories", checked)
	}
	if got := envMap(env)["QA_REPOSITORIES"]; got != "0-site" {
		t.Errorf("QA_REPOSITORIES = %q, want only the changed, buildable repository", got)
	}
	if qa := readQAFromJobOutput(stage.parameters); qa == nil || !reflect.DeepEqual(qa.Repositories, []string{"0-site"}) {
		t.Errorf("recorded repositories = %+v", qa)
	}
}

// --- the QA spawn --------------------------------------------------------------

func TestQASpawnEnvIsBlindAndInQAMode(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.PreviousStepsSummary, "step 1 did X")
	stage.repositories = []string{"0-acme-site", "2-docs"}
	env, err := stage.qaSpawnEnv()
	if err != nil {
		t.Fatal(err)
	}
	// Inherited values the QA run must never see, even when a credential
	// bundle carries them.
	env = applyQAEnv(append(env, "REVIEW_SPEC=x", "REVIEW_BASE_COMMITS={}"), qaTestSpec, stage.repositories)
	got := envMap(env)
	for key, want := range map[string]string{
		"AGENT_MODE":          "qa",
		"QA_SPEC":             qaTestSpec,
		"QA_REPOSITORIES":     "0-acme-site,2-docs",
		"MCP_TOOL_RPC_SOCKET": "/run/agentbox/tool-rpc.sock",
		"MAX_TURNS":           "80",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "STEP_PROMPT" || key == "PREVIOUS_STEPS_SUMMARY" || strings.HasPrefix(key, "REVIEW_") {
			t.Errorf("the QA spawn carries %s", key)
		}
	}
	if _, present := envMap(applyQAEnv(nil, "  ", nil))["QA_SPEC"]; present {
		t.Error("a blank spec was sent as QA_SPEC")
	}
}

func TestQASpawnUsesTheQAModelsCredentials(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	openAI := llm_provider_enums.OpenAIDirect
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.QAAgentType, "codex")
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.QAModel, "gpt-5.5")
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.QAAgentProvider, openAI.Key())
	jobs.SetParameterValue[map[string]string](stage.parameters, parameters_enums.QAAgentEnvVars,
		map[string]string{"OPENAI_API_KEY": "sk-openai-qa", "CODEX_API_KEY": "sk-openai-qa"})
	stage.qaParams, stage.qaFailure = qaParameters(stage.parameters)
	if stage.qaFailure != "" {
		t.Fatal(stage.qaFailure)
	}
	env, err := stage.qaSpawnEnv()
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	if got["AGENT_TYPE"] != "codex" || got["OPENAI_API_KEY"] != "sk-openai-qa" {
		t.Errorf("the QA spawn does not run as the QA model: %v", got)
	}
	if _, present := got["ANTHROPIC_API_KEY"]; present {
		t.Error("the QA spawn carries the implementer's key")
	}
	if stage.agentType() != "codex" || stage.model() != "gpt-5.5" {
		t.Errorf("the QA record names %s/%s", stage.agentType(), stage.model())
	}
}

func TestQASpawnSpecMountsTheQADirAndBuildsPreviewsFromTheTaskDir(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	stage.contextCopyDir = "/tmp/org-1/task-1-qa-context-1"
	qaDir := qaRoundDirPath(workDir, 1)
	deps := buildStaticSitePreviewDeps(stage.ctx, stage.parameters, workDir, io.Discard)
	spec := stage.qaSpawnSpec("agentbox:1.9.32", qaDir, nil, deps)
	if spec.workDirHost != qaDir || strings.HasPrefix(qaDir, workDir+"/") {
		t.Errorf("/work is %q, want a sibling of the Task dir %q", spec.workDirHost, workDir)
	}
	if spec.previewDeps == nil || spec.previewDeps.WorkDirHost != workDir {
		t.Errorf("previews build from %v, want the Task dir", spec.previewDeps)
	}
	// The preview tools are registered when both are set (spawnAgentboxAndWait).
	if spec.mcpSocketHost == "" || strings.HasPrefix(spec.mcpSocketHost, qaDir+"/") {
		t.Errorf("the tool socket is %q, want one beside the QA dir", spec.mcpSocketHost)
	}
	if spec.cacheVolume != "" || len(spec.readOnlyRepoDirs) != 0 || spec.waitTimeout != qaRunTimeout {
		t.Errorf("spec = %+v, want no cache, no repository mounts, the QA timeout", spec)
	}
	for _, m := range agentboxMounts(spec) {
		if strings.HasPrefix(m.Source, workDir+"/") || m.Source == workDir {
			t.Errorf("the QA container mounts the Task dir: %+v", m)
		}
	}
}

// --- results -----------------------------------------------------------------

func TestQARoundResults(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	for _, tc := range []struct {
		name       string
		result     agentResult
		err        error
		wantStatus string
		wantReason string
	}{
		{"qa_result read", qaCompleted(signUpFailed), nil, tasks.QAStatusCompleted, ""},
		{"agentbox could not run it", agentResult{Status: "failure", QAResult: &qaResult{Status: tasks.QAStatusCouldNotRun, StatusReason: "the preview never loaded"}}, nil, tasks.QAStatusCouldNotRun, "the preview never loaded"},
		{"an agent-reported skip", agentResult{Status: "success", QAResult: &qaResult{Status: tasks.QAStatusSkipped, StatusReason: "nothing to do"}}, nil, tasks.QAStatusCouldNotRun, "nothing to do"},
		{"no qa_result", agentResult{Status: "success"}, nil, tasks.QAStatusCouldNotRun, "the QA run produced no qa_result"},
		{"the spawn failed", agentResult{}, errors.New("error creating container: no space"), tasks.QAStatusCouldNotRun, "error creating container: no space"},
		{"an older image", agentResult{Status: "failure", Error: `invalid AGENT_MODE "qa": must be implement or review`}, errors.New("agent exited 1"), tasks.QAStatusCouldNotRun, qaOlderAgentboxReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stage.roundOutput(1, tc.result, tc.err)
			if got.Status != tc.wantStatus || got.StatusReason != tc.wantReason {
				t.Errorf("round = %s %q, want %s %q", got.Status, got.StatusReason, tc.wantStatus, tc.wantReason)
			}
		})
	}
	got := stage.roundOutput(1, qaCompleted(signUpFailed, pricingPassed), nil)
	if !reflect.DeepEqual(got.Criteria, []qaCriterion{signUpFailed, pricingPassed}) {
		t.Errorf("criteria = %+v", got.Criteria)
	}
}

func TestReadAgentResultReadsQAResult(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, agentboxResultDirRel, agentboxResultFile), `{"status":"success","qa_result":{"status":"completed","criteria":[{"criterion":"c","state":"failed","reason":"r"}],"findings":[{"parameter":"spec conformance","severity":"high","what":"w","stage":"qa","pass":"criteria"}],"observations":["slow"]}}`)
	result, err := readAgentResult(dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.QAResult == nil || result.QAResult.Criteria[0].State != "failed" || result.QAResult.Findings[0].Stage != "qa" || result.QAResult.Observations[0] != "slow" {
		t.Errorf("qa_result = %+v", result.QAResult)
	}
}

// --- the fix loop ----------------------------------------------------------------

// fixLoopEvents records the order of the loop's runs.
type fixLoopEvents struct{ events []string }

func (e *fixLoopEvents) add(s string) { e.events = append(e.events, s) }

func wireFixLoop(t *testing.T, stage *qaStage, workDir string, ev *fixLoopEvents, rounds ...agentResult) {
	t.Helper()
	run, _ := qaRounds(rounds...)
	stage.runQA = func(round int) (agentResult, error) {
		ev.add(fmt.Sprintf("qa %d", round))
		return run(round)
	}
	stage.runFix = func(prompt string) (agentResult, error) {
		ev.add("fix")
		writeFile(t, filepath.Join(workDir, "0-acme-site", "index.html"), "<button>Sign up</button>\n")
		return agentResult{Status: "success", ChangesSummary: "added the button", TokenUsage: tokenUsage{InputTokens: 1000}}, nil
	}
	stage.reviewer = func(rs *reviewStage) {
		rs.copyTree = testCopyTree
		rs.runReview = func(round int) (agentResult, error) {
			ev.add(fmt.Sprintf("review %d", round))
			return agentResult{Status: "success", Turns: 3, TokenUsage: tokenUsage{InputTokens: 50},
				ReviewResult: &reviewResult{Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}}}}, nil
		}
	}
}

func withReviewRecorded(t *testing.T, stage *qaStage, participation int64) {
	t.Helper()
	jobs.SetParameterValue[int64](stage.parameters, parameters_enums.ReviewParticipation, participation)
	if participation == participationOff {
		return
	}
	if err := mergeReviewIntoJobOutput(stage.parameters, &reviewOutput{
		Participation: participationName(participation),
		Rounds: []reviewRoundOutput{{Round: 1, Completed: true, Findings: []reviewFindingOutput{{
			Key: "sec-1", Parameter: "security", Severity: "high", What: "open", SentBack: true, MustFix: true,
		}}}},
		MustFixOpen:       true,
		FinalTreeReviewed: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQAFixLoopFailedCriterionFixReviewFinalRound(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	withReviewRecorded(t, stage, participationOn)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed, pricingPassed), qaCompleted(signUpPassed, pricingPassed))
	var prompt string
	fix := stage.runFix
	stage.runFix = func(p string) (agentResult, error) { prompt = p; return fix(p) }
	var openFindings string
	reviewer := stage.reviewer
	stage.reviewer = func(rs *reviewStage) {
		reviewer(rs)
		review := rs.runReview
		rs.runReview = func(round int) (agentResult, error) {
			openFindings, _ = rs.openSentBackEnvValue(round)
			if rs.roundFixDiffs == "" {
				t.Error("the review round after the QA fix has no fix diff")
			}
			return review(round)
		}
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1", "fix", "review 2", "qa 2"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	for _, want := range []string{"implement the export endpoint", "[Acceptance criteria that failed QA on a live preview]", "The home page shows a Sign up button", "QA: no button on /", "without reading the source", "change nothing for it and say why"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the fix prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "pricing page") {
		t.Error("the fix prompt carries a criterion that passed")
	}
	if !strings.Contains(openFindings, "sec-1") {
		t.Errorf("REVIEW_OPEN_FINDINGS = %q, want the review's still-open finding", openFindings)
	}
	qa := readQAFromJobOutput(stage.parameters)
	if qa == nil || len(qa.Rounds) != 2 || qa.Status != tasks.QAStatusCompleted {
		t.Fatalf("qa = %+v", qa)
	}
	if !reflect.DeepEqual(qa.FixedByQA, []string{"The home page shows a Sign up button"}) {
		t.Errorf("fixed by QA = %v", qa.FixedByQA)
	}
	review := readReviewFromJobOutput(stage.parameters)
	if review == nil || len(review.Rounds) != 2 || review.Rounds[1].Round != 2 || !review.FinalTreeReviewed {
		t.Fatalf("review = %+v", review)
	}
	// The reviewer did not answer for sec-1, so it is held: still open, still
	// holding.
	if !review.MustFixOpen {
		t.Error("a must-fix finding the review round held stopped holding")
	}
	// Usage: QA rounds in qa, the fix in implement, the review round in review.
	data := jobOutputFor(t, stage.parameters)
	if data.Usage == nil || data.Usage.QA == nil || data.Usage.QA.TokenUsage.InputTokens != 200 {
		t.Errorf("usage.qa = %+v", data.Usage)
	}
	if data.Usage.Implement == nil || data.Usage.Implement.TokenUsage.InputTokens != 1000 {
		t.Errorf("usage.implement = %+v", data.Usage.Implement)
	}
	if data.Usage.Review == nil || data.Usage.Review.TokenUsage.InputTokens != 50 {
		t.Errorf("usage.review = %+v", data.Usage.Review)
	}
	if dto := stage.qaResultDto(); dto.TokenUsage.InputTokens != 200 || dto.CostUSD != 0.5 || len(dto.Criteria) != 2 {
		t.Errorf("reported QA result = %+v", dto)
	}
}

func TestQAFixLoopWithReviewOffRunsNoReviewRound(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	withReviewRecorded(t, stage, participationOff)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed), qaCompleted(signUpPassed))
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1", "fix", "qa 2"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	if readReviewFromJobOutput(stage.parameters) != nil {
		t.Error("a review was recorded for a Task whose review is off")
	}
}

func TestQAFixLoopDeclinedFixStops(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	withReviewRecorded(t, stage, participationOn)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed))
	stage.runFix = func(string) (agentResult, error) {
		ev.add("fix")
		return agentResult{Status: "success", ChangesSummary: "The button is already there; QA looked at /old."}, nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1", "fix"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	qa := readQAFromJobOutput(stage.parameters)
	if !qa.StoppedNoChange || qa.DeclinedExplanation != "The button is already there; QA looked at /old." {
		t.Errorf("qa = %+v", qa)
	}
	if summary := readAgentSummaryFromJobOutput(stage.parameters); strings.Contains(summary, "already there") {
		t.Error("a declined fix's summary replaced the description")
	}
}

func TestQAFixLoopFailedFixIsRestored(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	withReviewRecorded(t, stage, participationOn)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed))
	before, err := os.ReadFile(filepath.Join(workDir, "0-acme-site", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	stage.runFix = func(string) (agentResult, error) {
		ev.add("fix")
		writeFile(t, filepath.Join(workDir, "0-acme-site", "index.html"), "half a fix\n")
		return agentResult{Status: "failure", Error: "max turns reached"}, nil
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(workDir, "0-acme-site", "index.html"))
	if string(after) != string(before) {
		t.Errorf("the tree after a failed fix = %q, want it restored to %q", after, before)
	}
	if want := []string{"qa 1", "fix"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	if qa := readQAFromJobOutput(stage.parameters); qa.FixError == "" {
		t.Errorf("qa = %+v, want the fix error recorded", qa)
	}
}

func TestQASmokeCheckIsNeverSentBack(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	var ev fixLoopEvents
	smoke := agentResult{Status: "success", QAResult: &qaResult{Status: tasks.QAStatusCompleted, NoCriteria: true,
		Findings: []reviewFinding{{Parameter: "correctness", Severity: "high", What: "console error"}}}}
	wireFixLoop(t, stage, workDir, &ev, smoke)
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	if qa := readQAFromJobOutput(stage.parameters); !qa.NoCriteria || len(qa.Rounds[0].Findings) != 1 {
		t.Errorf("qa = %+v", qa)
	}
}

func TestQANoBudgetNoFix(t *testing.T) {
	var logs strings.Builder
	stage, workDir := changedQAStage(t, &logs)
	stage.deadline = time.Now().Add(30 * time.Minute)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed))
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	if !strings.Contains(logs.String(), "QA: no time left for a fix round") {
		t.Errorf("the log does not say why no fix ran:\n%s", logs.String())
	}
}

// A fix run that overruns its timeout (vendoring a missing cache, say) leaves
// no time for what follows it: neither the review round nor the final QA round
// starts, and the review records that the final tree was not reviewed.
func TestQAFixOverrunStartsNoFurtherRound(t *testing.T) {
	var logs strings.Builder
	stage, workDir := changedQAStage(t, &logs)
	withReviewRecorded(t, stage, participationOn)
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed), qaCompleted(signUpPassed))
	fix := stage.runFix
	stage.runFix = func(p string) (agentResult, error) {
		late := time.Now().Add(time.Minute)
		stage.deadline = late
		stage.fixStage.deadline = late
		return fix(p)
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1", "fix"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	for _, want := range []string{"QA: no time left for a review round on the fix", "QA: no time left for the final QA round"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs.String())
		}
	}
	if review := readReviewFromJobOutput(stage.parameters); review == nil || review.FinalTreeReviewed || len(review.Rounds) != 1 {
		t.Errorf("review = %+v, want round 1 only and the final tree not reviewed", review)
	}
	qa := readQAFromJobOutput(stage.parameters)
	if qa == nil || len(qa.Rounds) != 2 {
		t.Fatalf("qa = %+v, want round 1 and a final round that could not run", qa)
	}
	if qa.Status != tasks.QAStatusCouldNotRun || qa.StatusReason != qaNoTimeForFinalRoundReason {
		t.Errorf("qa status = %q %q, want couldnt_run %q", qa.Status, qa.StatusReason, qaNoTimeForFinalRoundReason)
	}
	if final := qa.finalRound(); final == nil || len(final.Criteria) != 0 {
		t.Errorf("final round = %+v, want no criteria carried over from round 1", final)
	}
}

// A review round that did not complete asserts nothing: the review round on
// the QA fix is sent what the last COMPLETED round left open.
func TestQAFixReviewRoundIsSentTheLastCompletedRoundsOpenFindings(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	jobs.SetParameterValue[int64](stage.parameters, parameters_enums.ReviewParticipation, participationOn)
	if err := mergeReviewIntoJobOutput(stage.parameters, &reviewOutput{
		Participation: participationName(participationOn),
		Rounds: []reviewRoundOutput{
			{Round: 1, Completed: true, Findings: []reviewFindingOutput{{
				Key: "sec-1", Parameter: "security", Severity: "high", What: "open", SentBack: true, MustFix: true,
			}}},
			{Round: 2, Completed: false},
		},
		MustFixOpen: true,
	}); err != nil {
		t.Fatal(err)
	}
	var ev fixLoopEvents
	wireFixLoop(t, stage, workDir, &ev, qaCompleted(signUpFailed), qaCompleted(signUpPassed))
	var openFindings string
	reviewer := stage.reviewer
	stage.reviewer = func(rs *reviewStage) {
		reviewer(rs)
		review := rs.runReview
		rs.runReview = func(round int) (agentResult, error) {
			openFindings, _ = rs.openSentBackEnvValue(round)
			return review(round)
		}
	}
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"qa 1", "fix", "review 3", "qa 2"}; !reflect.DeepEqual(ev.events, want) {
		t.Errorf("events = %v, want %v", ev.events, want)
	}
	if !strings.Contains(openFindings, "sec-1") {
		t.Errorf("REVIEW_OPEN_FINDINGS = %q, want round 1's still-open finding", openFindings)
	}
	if review := readReviewFromJobOutput(stage.parameters); review == nil || !review.MustFixOpen {
		t.Errorf("review = %+v, want sec-1 still holding", review)
	}
}

func TestQAUserStopIsReturned(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	stage.runQA = func(int) (agentResult, error) { return agentResult{}, types.ErrJobStoppedByUser }
	if _, err := stage.run(); !errors.Is(err, types.ErrJobStoppedByUser) {
		t.Errorf("run = %v, want the user stop", err)
	}
}

func TestQAOutputAndUsageSurviveALaterMerge(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	run, _ := qaRounds(qaCompleted(signUpPassed))
	stage.runQA = run
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	if err := mergeAgentResultIntoJobOutput(stage.parameters, agentResult{Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := mergeReviewIntoJobOutput(stage.parameters, &reviewOutput{}); err != nil {
		t.Fatal(err)
	}
	data := jobOutputFor(t, stage.parameters)
	if data.QA == nil || data.QA.Status != tasks.QAStatusCompleted {
		t.Errorf("the qa block did not survive: %+v", data.QA)
	}
	if data.Usage == nil || data.Usage.QA == nil || data.Usage.QA.TokenUsage.InputTokens != 100 {
		t.Errorf("usage.qa did not survive: %+v", data.Usage)
	}
}

func TestQALogSummary(t *testing.T) {
	var logs strings.Builder
	stage, workDir := changedQAStage(t, &logs)
	var ev fixLoopEvents
	round1 := qaCompleted(signUpFailed, pricingPassed)
	round1.QAResult.Observations = []string{"the page took 4s to load"}
	wireFixLoop(t, stage, workDir, &ev, round1, qaCompleted(signUpPassed, pricingPassed))
	if _, err := stage.run(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--- QA stage ---",
		"Repositories QA'd: 0-acme-site",
		"QA round 1: completed — 1 passed, 1 failed, 0 not checked",
		"[failed] The home page shows a Sign up button — no button on /",
		"[observation] the page took 4s to load",
		"QA round 2: completed — 2 passed, 0 failed, 0 not checked",
		"[fixed by QA] The home page shows a Sign up button",
		"QA: advisory — does not hold this pull request",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q", want)
		}
	}
}

// A QA run stopped at its timeout is couldnt_run, but the qa_result agentbox
// wrote beside the failure keeps its criteria, findings and observations.
func TestQARoundKeepsQAResultBesideAFailure(t *testing.T) {
	stage := &qaStage{parameters: map[string]interface{}{}}
	result := agentResult{QAResult: &qaResult{
		Status:       tasks.QAStatusCouldNotRun,
		Criteria:     []qaCriterion{{Criterion: "Sign up works", State: tasks.QACriterionFailed, Reason: "500"}},
		Findings:     []reviewFinding{{Parameter: "correctness", What: "error page"}},
		Observations: []string{"slow load"},
	}}
	out := stage.roundOutput(1, result, errors.New("agentbox timed out after 20m0s"))
	if out.Status != tasks.QAStatusCouldNotRun || out.StatusReason != "agentbox timed out after 20m0s" {
		t.Errorf("status = %q %q", out.Status, out.StatusReason)
	}
	if len(out.Criteria) != 1 || len(out.Findings) != 1 || len(out.Observations) != 1 {
		t.Errorf("round = %+v, want the qa_result's criteria, findings and observations kept", out)
	}
}
