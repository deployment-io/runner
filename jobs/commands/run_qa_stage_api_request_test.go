package commands

import (
	"errors"
	"io"
	"strings"
	"testing"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// A stack-path round serves verify_api_request, and sets QA_API_REQUEST_TOOL=1,
// only when a backend preview deployed.
func TestQAStackRoundServesTheAPIRequestTool(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sites    []string
		backends []string
		f        *fakeStackOps
		want     bool
	}{
		{name: "a deployed backend", sites: []string{"0-web"}, backends: []string{"1-api"},
			f: &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"}}, want: true},
		{name: "only a backend", backends: []string{"1-api"},
			f: &fakeStackOps{backends: map[string]string{"1-api": "api"}}, want: true},
		{name: "only site previews", sites: []string{"0-web"},
			f: &fakeStackOps{sites: map[string]string{"0-web": "web"}}},
		{name: "only failed backends", sites: []string{"0-web"}, backends: []string{"1-api"},
			f: &fakeStackOps{sites: map[string]string{"0-web": "web"}, backends: map[string]string{"1-api": "api"},
				deployErr: map[string]error{"backend:1-api": errors.New("the service didn't start")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, _, specs := stackStage(t, []string{"web", "api"}, tc.sites, tc.backends, tc.f)
			if _, err := runStackRound(t, stage, 1); err != nil {
				t.Fatal(err)
			}
			spec := (*specs)[0]
			env := envMap(spec.env)
			_, set := env["QA_API_REQUEST_TOOL"]
			tools := toolNames(spec)
			if tc.want {
				if !spec.apiRequestTool || env["QA_API_REQUEST_TOOL"] != "1" || tools != "ping,verify_preview_reachable,verify_preview_rendered,verify_api_request" {
					t.Errorf("tools = %s, QA_API_REQUEST_TOOL = %q", tools, env["QA_API_REQUEST_TOOL"])
				}
				return
			}
			if spec.apiRequestTool || set || tools != "ping,verify_preview_reachable,verify_preview_rendered" {
				t.Errorf("tools = %s, QA_API_REQUEST_TOOL set = %v", tools, set)
			}
		})
	}
}

// Production QA, implement runs and the older QA_BACKENDS path never serve it.
func TestOnlyAStackRoundServesTheAPIRequestTool(t *testing.T) {
	workDir := t.TempDir()
	tools := buildImplementPreviewTools(commandUtils.TaskJobContext{OrganizationID: "o", TaskID: "t"}, webPreviewJobParameters(t), workDir, io.Discard)
	implement := implementSpawnSpec("img", workDir, "cache", nil, tools)
	production := agentboxSpawnSpec{productionTools: &productionQATools{record: productionQARecord(productionTestTargets)}}
	stage, qaWorkDir := changedQAStage(t, nil)
	backendsPath := stage.qaSpawnSpec("img", qaRoundDirPath(qaWorkDir, 1), nil, buildStaticSitePreviewDeps(stage.ctx, stage.parameters, qaWorkDir, io.Discard))
	for name, spec := range map[string]agentboxSpawnSpec{"implement": implement, "production QA": production, "QA_BACKENDS round": backendsPath} {
		if got := toolNames(spec); strings.Contains(got, "verify_api_request") {
			t.Errorf("%s tools = %s", name, got)
		}
		if _, ok := envMap(spec.env)["QA_API_REQUEST_TOOL"]; ok {
			t.Errorf("%s sets QA_API_REQUEST_TOOL", name)
		}
	}
	// The flag is only read beside verifyToolsOnly.
	implement.apiRequestTool = true
	if got := toolNames(implement); strings.Contains(got, "verify_api_request") {
		t.Errorf("an implement run with the flag serves %s", got)
	}
}
