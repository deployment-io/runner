package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
)

var evidenceJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'J', 'F', 'I', 'F'}

// fillScreenshots runs n checks through the round's verify_preview_rendered,
// as registerAgentTools wires it, so the store holds c1…cn — except the
// checks in without, whose browser returns no screenshot.
func fillScreenshots(t *testing.T, spec agentboxSpawnSpec, n int, without ...int) {
	t.Helper()
	call := 0
	deps := *spec.previewDeps
	deps.Record.AddURL("https://d111.cloudfront.net/")
	deps.VerifyBrowser = func(_ context.Context, req agenttools.VerifyBrowserRequest, _ io.Writer) (agenttools.VerifyBrowserOutcome, error) {
		call++
		if !req.Screenshot {
			t.Errorf("check %d asked for no screenshot", call)
		}
		out := agenttools.VerifyBrowserOutcome{Result: []byte(`{"schema_version":1,"status":"completed","verdict":"pass","http_status":200,"screenshot":{"width":1280,"height":720}}`)}
		for _, w := range without {
			if w == call {
				return out, nil
			}
		}
		out.Screenshot = append([]byte{}, evidenceJPEG...)
		out.Screenshot = append(out.Screenshot, byte(call))
		return out, nil
	}
	spec.previewDeps = &deps
	srv := agentmcp.New("test", "0")
	tools := registerAgentTools(srv, spec)
	defer tools.stop()
	tool, ok := srv.Lookup("verify_preview_rendered")
	if !ok {
		t.Fatal("no verify_preview_rendered")
	}
	for i := 1; i <= n; i++ {
		out, err := tool.Handler(context.Background(), json.RawMessage(`{"url":"https://d111.cloudfront.net/"}`))
		if err != nil || !strings.Contains(out, fmt.Sprintf(`"check_id":"c%d"`, i)) {
			t.Fatalf("check %d: %s, %v", i, out, err)
		}
	}
}

// evidenceStage is a stage whose rounds 1…len(rounds) each ran with a store
// of n checks (all with screenshots but those in without), and whose QA output
// is rounds; uploads are recorded and answered by answer.
func evidenceStage(t *testing.T, rounds []qaRoundOutput, n int, without ...int) (*qaStage, *[]tasks.UploadQAEvidenceDtoV1, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	stage, workDir := changedQAStage(t, logs)
	jobs.SetParameterValue[string](stage.parameters, parameters_enums.JobID, "job-1")
	stage.ctx.StepIndex = 2
	for _, r := range rounds {
		deps := buildStaticSitePreviewDeps(stage.ctx, stage.parameters, workDir, io.Discard)
		spec := stage.qaSpawnSpec(r.Round, "agentbox:1.9.32", qaRoundDirPath(workDir, r.Round), nil, deps)
		if spec.qaScreenshots == nil {
			t.Fatalf("round %d has no store", r.Round)
		}
		fillScreenshots(t, spec, n, without...)
	}
	stage.out = &qaOutput{Status: tasks.QAStatusCompleted, Rounds: rounds}
	var uploads []tasks.UploadQAEvidenceDtoV1
	stage.uploadEvidence = func(u tasks.UploadQAEvidenceDtoV1) (string, error) {
		uploads = append(uploads, u)
		return "id-" + u.CheckID, nil
	}
	return stage, &uploads, logs
}

func evidenceRound(round int, evidence ...[]string) qaRoundOutput {
	r := qaRoundOutput{Round: round, Status: tasks.QAStatusCompleted}
	for i, e := range evidence {
		r.Criteria = append(r.Criteria, qaCriterion{Criterion: fmt.Sprintf("criterion %d", i), State: tasks.QACriterionPassed, Evidence: e})
	}
	return r
}

func dtoEvidence(dto tasks.QAResultDtoV1) [][]string {
	var out [][]string
	for _, c := range dto.Criteria {
		out = append(out, c.EvidenceIDs)
	}
	return out
}

func TestQASpawnEnvAsksForEvidenceOnlyInPreviewQA(t *testing.T) {
	stage, _ := changedQAStage(t, nil)
	env, err := stage.qaSpawnEnv()
	if err != nil {
		t.Fatal(err)
	}
	if envMap(env)["QA_EVIDENCE"] != "1" {
		t.Errorf("QA_EVIDENCE = %q, want 1", envMap(env)["QA_EVIDENCE"])
	}
	production := applyProductionQAEnv([]string{"QA_EVIDENCE=1"}, qaTestSpec, productionTestTargets)
	if _, set := envMap(production)["QA_EVIDENCE"]; set {
		t.Error("production QA's environment has QA_EVIDENCE")
	}
}

// Each QA round's spec gets its own store beside the work dir, kept by round;
// cleanupQAStageSiblings removes them.
func TestQASpawnSpecGivesEachRoundAStore(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	deps := buildStaticSitePreviewDeps(stage.ctx, stage.parameters, workDir, io.Discard)
	one := stage.qaSpawnSpec(1, "img", qaRoundDirPath(workDir, 1), nil, deps)
	two := stage.qaSpawnSpec(2, "img", qaRoundDirPath(workDir, 2), nil, deps)
	if one.qaScreenshots == nil || two.qaScreenshots == nil || one.qaScreenshots == two.qaScreenshots {
		t.Fatalf("stores %p and %p", one.qaScreenshots, two.qaScreenshots)
	}
	if one.qaScreenshots.Dir() != workDir+"-qa-evidence-1" || two.qaScreenshots.Dir() != workDir+"-qa-evidence-2" {
		t.Errorf("dirs %s and %s", one.qaScreenshots.Dir(), two.qaScreenshots.Dir())
	}
	if stage.screenshots[1] != one.qaScreenshots || stage.screenshots[2] != two.qaScreenshots {
		t.Error("the stage doesn't keep each round's store")
	}
	info, err := os.Stat(one.qaScreenshots.Dir())
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("store dir: %v, %v", info, err)
	}
	cleanupQAStageSiblings(workDir)
	for _, dir := range []string{one.qaScreenshots.Dir(), two.qaScreenshots.Dir()} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", dir, err)
		}
	}
}

// The stack path's round spec has a store too, and its tool asks for
// screenshots.
func TestQAStackRoundHasAStore(t *testing.T) {
	f := &fakeStackOps{sites: map[string]string{"0-web": "web"}}
	stage, _, specs := stackStage(t, []string{"web"}, []string{"0-web"}, nil, f)
	if _, err := runStackRound(t, stage, 1); err != nil {
		t.Fatal(err)
	}
	spec := (*specs)[0]
	if spec.qaScreenshots == nil || stage.screenshots[1] != spec.qaScreenshots || envMap(spec.env)["QA_EVIDENCE"] != "1" {
		t.Fatalf("store %p, env %v", spec.qaScreenshots, envMap(spec.env)["QA_EVIDENCE"])
	}
	fillScreenshots(t, spec, 1)
	if !spec.qaScreenshots.Has("c1") {
		t.Error("the stack round's check kept no screenshot")
	}
}

// An implement or fix run's tool asks for no screenshot.
func TestImplementRunToolAsksForNoScreenshot(t *testing.T) {
	stage, workDir := changedQAStage(t, nil)
	deps := buildStaticSitePreviewDeps(stage.ctx, stage.parameters, workDir, io.Discard)
	deps.Record.AddURL("https://d111.cloudfront.net/")
	var input string
	deps.VerifyBrowser = func(_ context.Context, req agenttools.VerifyBrowserRequest, _ io.Writer) (agenttools.VerifyBrowserOutcome, error) {
		input = string(req.Input)
		return agenttools.VerifyBrowserOutcome{}, errors.New("stop")
	}
	srv := agentmcp.New("test", "0")
	tools := registerAgentTools(srv, agentboxSpawnSpec{previewDeps: deps})
	defer tools.stop()
	tool, _ := srv.Lookup("verify_preview_rendered")
	_, _ = tool.Handler(context.Background(), json.RawMessage(`{"url":"https://d111.cloudfront.net/"}`))
	if input == "" || strings.Contains(input, "screenshot") {
		t.Errorf("input.json = %q", input)
	}
}

// Only the final round's cited, stored screenshots are uploaded, each once in
// the order first cited, with every citing criterion's position; each
// criterion's ids follow its citation order, unknown and repeated ids dropped
// and at most 3.
func TestQAEvidenceUploadsTheFinalRoundsCitedScreenshots(t *testing.T) {
	rounds := []qaRoundOutput{
		evidenceRound(1, []string{"c5"}),
		evidenceRound(2,
			[]string{"c2", "c1", "c2", "c4", "c3", "c5"},
			[]string{"c1"},
			[]string{"c9"},
			nil,
		),
	}
	stage, uploads, logs := evidenceStage(t, rounds, 5, 4)
	if _, err := stage.finish(); err != nil {
		t.Fatal(err)
	}
	type sent struct {
		checkID  string
		criteria []int
		round    int
	}
	var got []sent
	for _, u := range *uploads {
		got = append(got, sent{u.CheckID, u.Criteria, u.Round})
		if u.TaskID != "task-1" || u.StepIndex != 2 || u.JobID != "job-1" || u.Width != 1280 || u.Height != 720 || len(u.Image) != len(evidenceJPEG)+1 {
			t.Errorf("upload %s = %+v", u.CheckID, u)
		}
	}
	want := []sent{{"c2", []int{0}, 2}, {"c1", []int{0, 1}, 2}, {"c3", []int{0}, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uploads = %+v, want %+v", got, want)
	}
	if ev := dtoEvidence(stage.qaResultDto()); !reflect.DeepEqual(ev, [][]string{{"id-c2", "id-c1", "id-c3"}, {"id-c1"}, nil, nil}) {
		t.Errorf("evidence ids = %v", ev)
	}
	if !strings.Contains(logs.String(), "QA: stored 3 screenshots for 2 criteria\n") {
		t.Errorf("logs = %q", logs.String())
	}
	// The Job output's rounds carry the cited check ids.
	if out := readQAFromJobOutput(stage.parameters); out == nil || !reflect.DeepEqual(out.Rounds[1].Criteria[1].Evidence, []string{"c1"}) {
		t.Errorf("job output = %+v", out)
	}
}

// A criterion at position 50 or later is left out of a screenshot's criteria,
// and a check cited only there isn't uploaded.
func TestQAEvidenceLeavesOutCriteriaPast50(t *testing.T) {
	evidence := make([][]string, 51)
	evidence[0] = []string{"c1"}
	evidence[50] = []string{"c1", "c2"}
	stage, uploads, _ := evidenceStage(t, []qaRoundOutput{evidenceRound(1, evidence...)}, 2)
	stage.uploadQAEvidence()
	if len(*uploads) != 1 || (*uploads)[0].CheckID != "c1" || !reflect.DeepEqual((*uploads)[0].Criteria, []int{0}) {
		t.Errorf("uploads = %+v", *uploads)
	}
}

// The first failed upload stops the rest: the criteria keep the ids stored
// before it, and the result is still reported.
func TestQAEvidenceFirstFailedUploadStops(t *testing.T) {
	for name, fail := range map[string]func() (string, error){
		"error":      func() (string, error) { return "", errors.New("image refused") },
		"empty id":   func() (string, error) { return "", nil },
		"old server": func() (string, error) { return "", errors.New("rpc: can't find method Jobs.UploadQAEvidenceV1") },
		"timeout":    func() (string, error) { return "", errors.New("deployment.io didn't answer within 30s") },
	} {
		t.Run(name, func(t *testing.T) {
			stage, _, logs := evidenceStage(t, []qaRoundOutput{evidenceRound(1, []string{"c1", "c2"}, []string{"c3", "c1"})}, 3)
			var calls []string
			stage.uploadEvidence = func(u tasks.UploadQAEvidenceDtoV1) (string, error) {
				calls = append(calls, u.CheckID)
				if len(calls) == 2 {
					return fail()
				}
				return "id-" + u.CheckID, nil
			}
			if _, err := stage.finish(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"c1", "c2"}) {
				t.Errorf("uploads = %v, want c1 then the failing c2", calls)
			}
			dto := stage.qaResultDto()
			if ev := dtoEvidence(dto); !reflect.DeepEqual(ev, [][]string{{"id-c1"}, {"id-c1"}}) {
				t.Errorf("evidence ids = %v", ev)
			}
			if dto.Status != tasks.QAStatusCompleted || len(dto.Criteria) != 2 {
				t.Errorf("result = %+v", dto)
			}
			if !strings.Contains(logs.String(), "QA: couldn't store a screenshot (") || !strings.Contains(logs.String(), "); QA's remaining screenshots were not stored\n") || strings.Contains(logs.String(), "QA: stored") {
				t.Errorf("logs = %q", logs.String())
			}
		})
	}
}

// No final round, or one without criteria, uploads nothing.
func TestQAEvidenceNothingToUpload(t *testing.T) {
	for name, rounds := range map[string][]qaRoundOutput{
		"no round":    nil,
		"no criteria": {{Round: 1, Status: tasks.QAStatusCompleted, NoCriteria: true}},
		"no evidence": {evidenceRound(1, nil, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			stage, uploads, logs := evidenceStage(t, rounds, 0)
			stage.uploadQAEvidence()
			if len(*uploads) != 0 || strings.Contains(logs.String(), "screenshot") {
				t.Errorf("uploads %+v, logs %q", *uploads, logs.String())
			}
		})
	}
}
