package commands

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/types"
)

// stageContextClient serves the Review stage's context fetch.
type stageContextClient struct {
	files   []context_pack.ContextFileV1
	err     error
	refresh []bool
}

func (c *stageContextClient) MaterializeContext(_ string, _ []context_pack.Scope, refreshRepoCatalog bool) ([]context_pack.ContextFileV1, error) {
	c.refresh = append(c.refresh, refreshRepoCatalog)
	return c.files, c.err
}

func (c *stageContextClient) SaveInfraContext(string, string, time.Duration) (int, error) {
	return 0, errors.New("not used")
}

func withStageContextClient(t *testing.T, c *stageContextClient) {
	t.Helper()
	prev := getContextClient
	t.Cleanup(func() { getContextClient = prev })
	getContextClient = func() contextClient { return c }
}

const servicesFixture = `{"service":"api","repo":"acme/api","environment":"prod","environmentId":"env/1 x","repoSource":"managed","variablesFrom":"environment","variableNames":["DB_URL"]}
not json
{"service":"api","repo":"acme/api","environment":"staging","environmentId":"env-2","repoSource":"managed","variablesFrom":"environment","variableNames":["STRIPE_KEY"]}
{"service":"worker","repo":"acme/worker","cluster":"ecs-1","repoSource":"answered","variablesFrom":"task definition","variableNames":["QUEUE"]}
{"service":"legacy","repo":"acme/legacy","repoSource":"managed","variableNames":["STRIPE_KEY"]}
`

func deployReq(variable, service, environment string) reviewDeployRequirement {
	return reviewDeployRequirement{Variable: variable, Service: service, Environment: environment, Location: "0-acme/api/pay.go:3"}
}

func fixtureRows(t *testing.T) []deployServiceRow {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "services.json"), servicesFixture)
	rows := readDeployServiceRows(dir)
	if len(rows) != 4 {
		t.Fatalf("read %d rows, want 4 with the malformed line skipped", len(rows))
	}
	return rows
}

// --- the pass list ------------------------------------------------------------

func TestReviewPassesIncludeDeploy(t *testing.T) {
	env := applyReviewEnv(nil, reviewEnvInputs{passes: reviewPasses})
	if got := envMap(env)["REVIEW_PASSES"]; got != "security,correctness,spec,deploy" {
		t.Errorf("REVIEW_PASSES = %q, want the deploy pass included", got)
	}
}

// --- the read-only copy -----------------------------------------------------

func TestTheReviewSpawnMountsTheContextCopyReadOnlyAndAFixRunDoesNot(t *testing.T) {
	workDir := t.TempDir()
	copyDir := t.TempDir()
	stage := &reviewStage{workDirHost: workDir, contextCopyDir: copyDir}

	m := findMount(t, agentboxMounts(stage.reviewSpawnSpec("img", workDir, nil, nil)), "/work/context")
	if !m.ReadOnly || m.Source != copyDir {
		t.Errorf("/work/context = %+v, want a read-only bind of the copy %s", m, copyDir)
	}
	for _, m := range agentboxMounts(stage.fixSpawnSpec("img", workDir, nil, nil)) {
		if m.Target == "/work/context" {
			t.Errorf("a fix run mounts the review's context copy: %+v", m)
		}
	}
	stage.contextCopyDir = ""
	for _, m := range agentboxMounts(stage.reviewSpawnSpec("img", workDir, nil, nil)) {
		if m.Target == "/work/context" {
			t.Errorf("a review with no copy mounts one: %+v", m)
		}
	}
}

// The copy lives outside the work dir for the whole stage, is fetched without
// a catalog rebuild, and is gone when the stage returns — however it returns.
func TestTheContextCopyIsRemovedOnEveryPathOutOfTheStage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result agentResult
		err    error
	}{
		{"success", agentResult{Status: "success", ReviewResult: &reviewResult{Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}}}}, nil},
		{"failure", agentResult{Status: "failed"}, errors.New("the container died")},
		{"stop", agentResult{}, types.ErrJobStoppedByUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &stageContextClient{files: []context_pack.ContextFileV1{{Path: "services.json", Content: servicesFixture}, {Path: "../escape.md", Content: "x"}}}
			withStageContextClient(t, client)
			workDir := filepath.Join(t.TempDir(), "work")
			writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "package api\n")
			stage := fixLoopStage(workDir, io.Discard)
			var seenCopy string
			stage.runReview = func(int) (agentResult, error) {
				seenCopy = stage.contextCopyDir
				if seenCopy == "" {
					t.Fatal("no context copy during the round")
				}
				if strings.HasPrefix(seenCopy, workDir+string(os.PathSeparator)) {
					t.Errorf("the copy %s is inside the work dir", seenCopy)
				}
				if _, err := os.Stat(filepath.Join(seenCopy, "services.json")); err != nil {
					t.Errorf("services.json is not in the copy: %s", err)
				}
				if _, err := os.Stat(filepath.Join(seenCopy, "escape.md")); err != nil {
					t.Errorf("an escaping path was not anchored under the copy: %s", err)
				}
				if info, err := os.Stat(filepath.Join(workDir, "context")); err != nil || !info.IsDir() {
					t.Errorf("the mount point was not created: %v", err)
				}
				return tc.result, tc.err
			}

			_, err := stage.run()
			if tc.name == "stop" && !errors.Is(err, types.ErrJobStoppedByUser) {
				t.Errorf("run = %v, want the stop", err)
			}
			if _, err := os.Stat(seenCopy); !os.IsNotExist(err) {
				t.Errorf("the copy %s survived the stage: %v", seenCopy, err)
			}
			if len(client.refresh) != 1 || client.refresh[0] {
				t.Errorf("fetches = %v, want one without a catalog rebuild", client.refresh)
			}
		})
	}
}

// Something the implement run left at /work/context that is not a directory
// is replaced, so the bind lands where the reviewer looks.
func TestEnsureContextMountPointReplacesASymlink(t *testing.T) {
	workDir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(workDir, "context")); err != nil {
		t.Fatal(err)
	}
	if err := ensureContextMountPoint(workDir, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(workDir, "context"))
	if err != nil || !info.IsDir() {
		t.Errorf("context = %v (%v), want a real directory", info, err)
	}
}

func TestAFailedContextFetchLogsAndMountsNothing(t *testing.T) {
	for _, client := range []*stageContextClient{{err: errors.New("connection refused")}, {}} {
		withStageContextClient(t, client)
		workDir := t.TempDir()
		writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "package api\n")
		var logs bytes.Buffer
		stage := fixLoopStage(workDir, &logs)
		stage.runReview = func(int) (agentResult, error) {
			for _, m := range agentboxMounts(stage.reviewSpawnSpec("img", workDir, nil, nil)) {
				if m.Target == "/work/context" {
					t.Errorf("a failed fetch still mounted %+v", m)
				}
			}
			return agentResult{Status: "success", ReviewResult: &reviewResult{Coverage: []reviewCoverage{{Parameter: "security", State: "checked"}}}}, nil
		}
		if _, err := stage.run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "Review stage: deployment context unavailable (") ||
			!strings.Contains(logs.String(), ") — the deploy readiness pass reads /work/context as the Task left it, and deploy requirements get no links\n") {
			t.Errorf("the log does not say the context is unavailable:\n%s", logs.String())
		}
		if client.err != nil && !strings.Contains(logs.String(), "connection refused") {
			t.Errorf("the log does not carry the reason:\n%s", logs.String())
		}
	}
}

// --- resolution ---------------------------------------------------------------

func TestResolveDeployRequirements(t *testing.T) {
	rows := fixtureRows(t)
	var logs bytes.Buffer
	got := resolveDeployRequirements([]reviewDeployRequirement{
		deployReq("STRIPE_KEY", "api", ""),        // prod lacks it (managed); staging has it (dropped)
		deployReq("SENTRY_DSN", "api", "staging"), // environment filter: staging only
		deployReq("SENTRY_DSN", "API", ""),        // service match is exact: not found
		deployReq("DB_URL", "api", "prod"),        // already set: dropped, no entry
		deployReq("SMTP_HOST", "worker", ""),      // unmanaged
		deployReq("STRIPE_KEY", "legacy", ""),     // no variablesFrom: unknown, so kept
		deployReq("STRIPE_KEY", "api", "prod"),    // duplicate of the first entry
		deployReq("bad-name", "api", ""),          // invalid name
	}, rows, &logs)

	want := []resolvedDeployRequirement{
		{Variable: "STRIPE_KEY", Service: "api", Environment: "prod", EnvironmentID: "env/1 x", Managed: true, Found: true, Location: "0-acme/api/pay.go:3"},
		{Variable: "SENTRY_DSN", Service: "api", Environment: "staging", EnvironmentID: "env-2", Managed: true, Found: true, Location: "0-acme/api/pay.go:3"},
		{Variable: "SENTRY_DSN", Service: "API", Location: "0-acme/api/pay.go:3"},
		{Variable: "SMTP_HOST", Service: "worker", Cluster: "ecs-1", Found: true, Location: "0-acme/api/pay.go:3"},
		{Variable: "STRIPE_KEY", Service: "legacy", Found: true, Location: "0-acme/api/pay.go:3"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, line := range []string{
		"Review stage: deploy requirement STRIPE_KEY for api in prod: managed\n",
		"Review stage: deploy requirement STRIPE_KEY for api in staging: already set, dropped\n",
		"Review stage: deploy requirement SENTRY_DSN for API in -: service not found\n",
		"Review stage: deploy requirement DB_URL for api in prod: already set, dropped\n",
		"Review stage: deploy requirement SMTP_HOST for worker in -: unmanaged\n",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, logs.String())
		}
	}
}

func TestResolveDeployRequirementsWithoutACopyIsNotFound(t *testing.T) {
	got := resolveDeployRequirements([]reviewDeployRequirement{deployReq("STRIPE_KEY", "api", "prod")}, readDeployServiceRows(""), io.Discard)
	if len(got) != 1 || got[0].Found || got[0].Environment != "prod" || got[0].Service != "api" {
		t.Errorf("got %+v, want one not-found entry with the requirement's own names", got)
	}
}

func TestResolveDeployRequirementsCapsAtTwenty(t *testing.T) {
	var reqs []reviewDeployRequirement
	for i := 0; i < 30; i++ {
		reqs = append(reqs, deployReq(fmt.Sprintf("V%d", i), "api", ""))
	}
	got := resolveDeployRequirements(reqs, fixtureRows(t), io.Discard)
	if len(got) != maxDeployRequirements {
		t.Errorf("got %d entries, want %d", len(got), maxDeployRequirements)
	}
}

func TestOnlyTheLatestCompletedRoundsRequirementsCount(t *testing.T) {
	rounds := []reviewRoundOutput{
		{Round: 1, Completed: true, DeployRequirements: []reviewDeployRequirement{deployReq("OLD", "api", "")}},
		{Round: 2, Completed: true, DeployRequirements: []reviewDeployRequirement{deployReq("NEW", "api", "")}},
		{Round: 3, Completed: false},
	}
	got := latestCompletedDeployRequirements(rounds)
	if len(got) != 1 || got[0].Variable != "NEW" {
		t.Errorf("got %+v, want round 2's", got)
	}
}

// A deploy requirement is never a finding: nothing is sent back, nothing
// holds, the tally and the inline comments do not change.
func TestDeployRequirementsAreNeverFindings(t *testing.T) {
	withStageContextClient(t, &stageContextClient{files: []context_pack.ContextFileV1{{Path: "services.json", Content: servicesFixture}}})
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "0-acme/api", "handler.go"), "package api\n")
	stage := fixLoopStage(workDir, io.Discard)
	fixRuns := 0
	stage.runFix = func([]reviewFindingOutput) error { fixRuns++; return nil }
	stage.runReview = func(int) (agentResult, error) {
		return agentResult{Status: "success", ReviewResult: &reviewResult{
			Coverage:           []reviewCoverage{{Parameter: "deploy readiness", State: "checked"}},
			DeployRequirements: []reviewDeployRequirement{deployReq("STRIPE_KEY", "api", "prod")},
		}}, nil
	}
	out, err := stage.run()
	if err != nil {
		t.Fatal(err)
	}
	review := readReviewFromJobOutput(out)
	if review == nil || len(review.DeployRequirements) != 1 || !review.DeployRequirements[0].Managed {
		t.Fatalf("review = %+v, want the resolved requirement", review)
	}
	if review.MustFixOpen || fixRuns != 0 || len(review.Rounds[0].Findings) != 0 {
		t.Errorf("a deploy requirement acted as a finding: must fix %v, fix runs %d, findings %+v",
			review.MustFixOpen, fixRuns, review.Rounds[0].Findings)
	}

	without := *review
	without.DeployRequirements = nil
	without.Rounds = append([]reviewRoundOutput(nil), review.Rounds...)
	without.Rounds[0].DeployRequirements = nil
	if reviewTally(review) != reviewTally(&without) {
		t.Errorf("the tally changed: %q vs %q", reviewTally(review), reviewTally(&without))
	}
	pr, _ := commentTestPR(review, nil)
	if plan := commentsFor(t, pr, 0); len(plan.comments) != 0 {
		t.Errorf("a deploy requirement was posted inline: %+v", plan.comments)
	}
}

// --- the pull request -------------------------------------------------------

func deployReview(reqs ...resolvedDeployRequirement) *reviewOutput {
	review := completedReview(false, layoutFinding("off by one", false, false))
	review.DeployRequirements = reqs
	return review
}

func TestBeforeDeployingSitsAfterTheTallyAndBeforeTheSummary(t *testing.T) {
	opr := reviewTestOpener(deployReview(
		resolvedDeployRequirement{Variable: "STRIPE_KEY", Service: "api`\nx", Environment: "prod`", EnvironmentID: "env/1 x", Managed: true, Found: true},
		resolvedDeployRequirement{Variable: "A_B", Service: "api", Environment: "prod", EnvironmentID: "e", Managed: true, Found: true},
		resolvedDeployRequirement{Variable: "SMTP_HOST", Service: "worker", Cluster: "ecs-1", Found: true},
		resolvedDeployRequirement{Variable: "QUEUE", Service: "worker", Found: true},
		resolvedDeployRequirement{Variable: "SENTRY_DSN", Service: "[evil](https://x.example)`"},
	))
	opr.ctx.DashboardURL = "https://app.example.com/dashboard/"

	_, body := opr.buildPRTitleAndBody()

	requireInOrder(t, body,
		"**Review:** 1 found",
		"**Before deploying**\n\nThis change reads configuration that the review did not find in its environment. Make sure each is set before the change is deployed:\n",
		"- `STRIPE_KEY` in environment `prod` (service `apix`) — [Add it](https://app.example.com/dashboard/environments/env%2F1%20x/edit?add=STRIPE_KEY)\n",
		"- `A_B` in environment `prod` (service `api`) — [Add it](https://app.example.com/dashboard/environments/e/edit?add=A_B)\n",
		"- `SMTP_HOST` for ECS service `worker` in cluster `ecs-1` — set it in the service's task definition\n",
		"- `QUEUE` for ECS service `worker` — set it in the service's task definition\n",
		"- `SENTRY_DSN` for service `\\[evil\\](https:\u200b//x.example)` — this service is not in deployment.io's context\n",
		"Added the login endpoint.",
	)

	opr.ctx.DashboardURL = ""
	_, body = opr.buildPRTitleAndBody()
	if !strings.Contains(body, "- `A_B` in environment `prod` (service `api`)\n") || strings.Contains(body, "[Add it]") {
		t.Errorf("without a DashboardURL the managed line should carry no link:\n%s", body)
	}
}

func TestBeforeDeployingIsAbsentWithoutRequirements(t *testing.T) {
	for _, review := range []*reviewOutput{nil, deployReview()} {
		_, body := reviewTestOpener(review).buildPRTitleAndBody()
		if strings.Contains(body, "Before deploying") {
			t.Errorf("the body carries Before deploying with no requirements:\n%s", body)
		}
	}
}

func TestBeforeDeployingCapsNamesAtAHundredRunes(t *testing.T) {
	long := strings.Repeat("é", 300)
	section := beforeDeployingSection(deployReview(resolvedDeployRequirement{Variable: "A", Service: long, Cluster: long, Found: true}), "")
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "- `A`") && utf8.RuneCountInString(line) > 300 {
			t.Errorf("a line is %d runes; the names were not capped", utf8.RuneCountInString(line))
		}
	}
}

func TestPRBodyStaysUnderTheCapWithTwentyRequirements(t *testing.T) {
	var findings []reviewFindingOutput
	for i := 0; i < 40; i++ {
		findings = append(findings, reviewFindingOutput{
			Parameter: "correctness", Severity: "low",
			Location: strings.Repeat("l", 400), What: strings.Repeat("w", 900), Why: strings.Repeat("y", 900),
		})
	}
	review := completedReview(false, findings...)
	long := strings.Repeat("s", 200)
	for i := 0; i < maxDeployRequirements; i++ {
		review.DeployRequirements = append(review.DeployRequirements, resolvedDeployRequirement{
			Variable: fmt.Sprintf("V%d_%s", i, strings.Repeat("X", 120)), Service: long, Environment: long,
			EnvironmentID: strings.Repeat("i", 100), Managed: true, Found: true,
		})
	}
	opr := reviewTestOpener(review)
	opr.ctx.DashboardURL = "https://app.example.com/dashboard"
	opr.agentSummary = strings.Repeat("x", 100000)
	var criteria []string
	for i := 0; i < 25; i++ {
		criteria = append(criteria, strings.Repeat("c", 500))
	}
	opr.acceptance = parseAcceptanceCriteria(taskSpecJSON(t, criteria))
	var hosts []string
	for i := 0; i < 80; i++ {
		hosts = append(hosts, fmt.Sprintf("%s-%d.example.com", strings.Repeat("h", 200), i))
	}
	opr.deniedHosts = hosts

	_, body := opr.buildPRTitleAndBody()

	if got := utf8.RuneCountInString(body); got > prBodyMaxRunes {
		t.Errorf("body is %d runes, want at most %d", got, prBodyMaxRunes)
	}
	if !strings.Contains(body, "**Before deploying**") || !strings.Contains(body, prSummaryTruncatedNote) {
		t.Error("the summary should yield before Before deploying does")
	}
	if strings.Count(body, "[Add it](") != maxDeployRequirements {
		t.Errorf("%d links, want %d", strings.Count(body, "[Add it]("), maxDeployRequirements)
	}
}
