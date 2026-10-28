package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
	"github.com/deployment-io/deployment-runner/agenttools"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	productionTestBrowser  = "docker.io/deploymenthq/verify-browser:1.3.0"
	productionTestAgentbox = "deploymenthq/agentbox:1.9.34"
)

var productionTestTargets = []productionQATarget{
	{Repository: "0-acme/site", URL: "https://d111.cloudfront.net", Hosts: []string{"www.acme.com", "acme.com"}},
	{Repository: "2-acme/docs", URL: "https://d222.cloudfront.net/"},
}

// productionQAParameters is a production QA Job's parameters as kit stamps
// them and deployment-server injects the credentials.
func productionQAParameters(t *testing.T) map[string]interface{} {
	t.Helper()
	t.Setenv(verifyBrowserImageEnv, "")
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[map[string]string](parameters, parameters_enums.AgentEnvVars,
		map[string]string{"ANTHROPIC_API_KEY": "sk-ant-implementer"})
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentType, "claude-code")
	jobs.SetParameterValue[string](parameters, parameters_enums.Model, "claude-sonnet-4-6")
	jobs.SetParameterValue[string](parameters, parameters_enums.AgentboxImage, productionTestAgentbox)
	jobs.SetParameterValue[string](parameters, parameters_enums.VerifyBrowserImage, productionTestBrowser)
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewSpec, qaTestSpec)
	jobs.SetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace, "org-"+primitive.NewObjectID().Hex())
	jobs.SetParameterValue[string](parameters, parameters_enums.TaskID, primitive.NewObjectID().Hex())
	targets, err := json.Marshal(productionTestTargets)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.ProductionQATargets, string(targets))
	return parameters
}

// runProductionQA runs the command with spawn standing in for the container
// and returns the job output's qa block and the whole envelope.
func runProductionQA(t *testing.T, parameters map[string]interface{}, spawn func(agentboxSpawnSpec, io.Writer) (agentResult, error)) (*qaOutput, jobOutputData) {
	t.Helper()
	return runProductionQAWith(t, &RunProductionQA{spawn: spawn, prefetchBrowser: func(context.Context, string, io.Writer) {}}, parameters)
}

func runProductionQAWith(t *testing.T, cmd *RunProductionQA, parameters map[string]interface{}) (*qaOutput, jobOutputData) {
	t.Helper()
	out, err := cmd.Run(parameters, io.Discard)
	if err != nil {
		t.Fatalf("Run returned %v; every outcome must complete the Job", err)
	}
	raw, err := jobs.GetParameterValue[string](out, parameters_enums.JobOutput)
	if err != nil {
		t.Fatal(err)
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	if data.QA == nil {
		t.Fatal("no qa block on the job output")
	}
	return data.QA, data
}

func TestGetMapsRunProductionQA(t *testing.T) {
	cmd, err := Get(commands_enums.RunProductionQA)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cmd.(*RunProductionQA); !ok {
		t.Errorf("Get(RunProductionQA) = %T", cmd)
	}
	if _, ok := cmd.(jobs.StoppableCommand); !ok {
		t.Error("RunProductionQA must honour the stop signal")
	}
}

func TestProductionQAEnvIsBlindAndTargetsProduction(t *testing.T) {
	inherited := []string{
		"ANTHROPIC_API_KEY=sk", "STEP_PROMPT=do X", "PREVIOUS_STEPS_SUMMARY=did Y",
		"REVIEW_SPEC=x", "REVIEW_BASE_COMMITS={}", "QA_TARGET=preview", "AGENT_MODE=implement", "MAX_TURNS=40",
	}
	env := applyProductionQAEnv(inherited, qaTestSpec, productionTestTargets)
	got := envMap(env)
	for key, want := range map[string]string{
		"AGENT_MODE":          "qa",
		"QA_TARGET":           "production",
		"QA_REPOSITORIES":     "0-acme/site,2-acme/docs",
		"QA_URLS":             "0-acme/site=https://d111.cloudfront.net,2-acme/docs=https://d222.cloudfront.net/",
		"QA_SPEC":             qaTestSpec,
		"MAX_TURNS":           "80",
		"MCP_TOOL_RPC_SOCKET": "/run/agentbox/tool-rpc.sock",
		"ANTHROPIC_API_KEY":   "sk",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	seen := map[string]int{}
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		seen[key]++
		if key == "STEP_PROMPT" || key == "PREVIOUS_STEPS_SUMMARY" || strings.HasPrefix(key, "REVIEW_") {
			t.Errorf("the production QA spawn carries %s", key)
		}
	}
	if seen["QA_TARGET"] != 1 || seen["AGENT_MODE"] != 1 {
		t.Errorf("duplicated keys: %v", seen)
	}
}

func TestProductionQASocketServesOnlyTheTwoChecks(t *testing.T) {
	srv := agentmcp.New("test", "0")
	stop := registerAgentTools(srv, agentboxSpawnSpec{productionTools: &productionQATools{record: productionQARecord(productionTestTargets)}})
	defer stop()
	if got := strings.Join(srv.ToolNames(), ","); got != "verify_preview_reachable,verify_preview_rendered" {
		t.Errorf("tools = %s", got)
	}
	// Every other spawn is unchanged.
	srv = agentmcp.New("test", "0")
	registerAgentTools(srv, agentboxSpawnSpec{})()
	if got := strings.Join(srv.ToolNames(), ","); got != "ping" {
		t.Errorf("tools without preview deps = %s", got)
	}
}

func TestProductionQARecordIsExactlyTheTargets(t *testing.T) {
	record := productionQARecord(productionTestTargets)
	if got := strings.Join(record.Hosts(), ","); got != "acme.com,d111.cloudfront.net,d222.cloudfront.net,www.acme.com" {
		t.Errorf("hosts = %s", got)
	}
}

func TestProductionQASpawnSpec(t *testing.T) {
	parameters := productionQAParameters(t)
	var spec agentboxSpawnSpec
	runProductionQA(t, parameters, func(s agentboxSpawnSpec, _ io.Writer) (agentResult, error) {
		spec = s
		if _, err := os.Stat(s.workDirHost); err != nil {
			t.Errorf("the QA dir does not exist during the run: %v", err)
		}
		entries, _ := os.ReadDir(s.workDirHost)
		if len(entries) != 0 {
			t.Errorf("the QA dir holds %d entries; it must hold no repository or context", len(entries))
		}
		return qaCompleted(signUpPassed), nil
	})
	orgID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	taskID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
	if want := fmt.Sprintf("/tmp/%s/%s-production-qa", orgID, taskID); spec.workDirHost != want {
		t.Errorf("work dir = %s, want %s", spec.workDirHost, want)
	}
	if _, err := os.Stat(spec.workDirHost); !os.IsNotExist(err) {
		t.Errorf("the QA dir survived the run: %v", err)
	}
	if spec.previewDeps != nil || spec.cacheVolume != "" || spec.readOnlyContextDir != "" || len(spec.readOnlyRepoDirs) != 0 {
		t.Errorf("the spawn has preview deps, a cache or mounts: %+v", spec)
	}
	if spec.productionTools == nil || spec.productionTools.onReadOnlyUnsupported == nil {
		t.Fatal("the spawn does not serve the production tools")
	}
	if spec.waitTimeout != qaRunTimeout || spec.imageRef != productionTestAgentbox {
		t.Errorf("timeout %s, image %s", spec.waitTimeout, spec.imageRef)
	}
	if got := envMap(spec.env); got["QA_TARGET"] != "production" || got["MAX_TURNS"] != "80" {
		t.Errorf("env = %v", got)
	}
}

func TestImageVersionAtLeast(t *testing.T) {
	for ref, want := range map[string]bool{
		"docker.io/deploymenthq/verify-browser:1.3.0":      true,
		"docker.io/deploymenthq/verify-browser:1.10.0":     true,
		"docker.io/deploymenthq/verify-browser:2.0.0":      true,
		"docker.io/deploymenthq/verify-browser:v1.3.1":     true,
		"localhost:5000/verify-browser:1.3.0":              true,
		"docker.io/deploymenthq/verify-browser:1.2.9":      false,
		"docker.io/deploymenthq/verify-browser:1.0.0":      false,
		"docker.io/deploymenthq/verify-browser:latest":     false,
		"docker.io/deploymenthq/verify-browser:1.3":        false,
		"docker.io/deploymenthq/verify-browser:1.3.0-rc1":  false,
		"docker.io/deploymenthq/verify-browser":            false,
		"localhost:5000/verify-browser":                    false,
		"docker.io/deploymenthq/verify-browser@sha256:abc": false,
		"": false,
	} {
		if got := imageVersionAtLeast(ref, minReadOnlyVerifyBrowserVersion); got != want {
			t.Errorf("imageVersionAtLeast(%q) = %v, want %v", ref, got, want)
		}
	}
}

// A refused run starts nothing and completes as couldnt_run naming the
// version it needs.
func TestProductionQARefusesOldOrUnversionedImages(t *testing.T) {
	for _, tc := range []struct {
		name, browser, envBrowser, agentbox, want string
	}{
		{"old browser", "docker.io/deploymenthq/verify-browser:1.2.0", "", productionTestAgentbox, "needs verify-browser 1.3.0 or later"},
		{"unparseable browser tag", "docker.io/deploymenthq/verify-browser:latest", "", productionTestAgentbox, "needs verify-browser 1.3.0 or later"},
		{"old browser by the runner's override", productionTestBrowser, "docker.io/deploymenthq/verify-browser:1.0.0", productionTestAgentbox, "needs verify-browser 1.3.0 or later"},
		{"old agentbox", productionTestBrowser, "", "deploymenthq/agentbox:1.9.33", "needs agentbox 1.9.34 or later"},
		{"unparseable agentbox tag", productionTestBrowser, "", "deploymenthq/agentbox:dev", "needs agentbox 1.9.34 or later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := productionQAParameters(t)
			t.Setenv(verifyBrowserImageEnv, tc.envBrowser)
			jobs.SetParameterValue[string](parameters, parameters_enums.VerifyBrowserImage, tc.browser)
			jobs.SetParameterValue[string](parameters, parameters_enums.AgentboxImage, tc.agentbox)
			qa, _ := runProductionQA(t, parameters, func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
				t.Fatal("a refused run started a container")
				return agentResult{}, nil
			})
			if qa.Status != tasks.QAStatusCouldNotRun || !strings.Contains(qa.StatusReason, tc.want) {
				t.Errorf("qa = %s %q, want couldnt_run naming %q", qa.Status, qa.StatusReason, tc.want)
			}
			if len(qa.Rounds) != 0 {
				t.Errorf("a refused run recorded %d rounds", len(qa.Rounds))
			}
		})
	}
}

func TestProductionQACompleted(t *testing.T) {
	qa, data := runProductionQA(t, productionQAParameters(t), func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
		return qaCompleted(signUpPassed, pricingPassed), nil
	})
	if qa.Status != tasks.QAStatusCompleted || qa.StatusReason != "" || qa.NoCriteria {
		t.Errorf("qa = %+v", qa)
	}
	if strings.Join(qa.Repositories, ",") != "0-acme/site,2-acme/docs" {
		t.Errorf("repositories = %v", qa.Repositories)
	}
	if len(qa.Rounds) != 1 || qa.Rounds[0].Round != 1 || len(qa.Rounds[0].Criteria) != 2 ||
		qa.Rounds[0].AgentType != "claude-code" || qa.Rounds[0].Turns != 7 {
		t.Errorf("rounds = %+v", qa.Rounds)
	}
	if data.Usage == nil || data.Usage.QA == nil || data.Usage.QA.TokenUsage.InputTokens != 100 {
		t.Errorf("usage = %+v, want the run's usage under qa", data.Usage)
	}
	if data.Usage.Implement != nil || data.Usage.Review != nil {
		t.Errorf("usage has other stages: %+v", data.Usage)
	}
	if data.Cost == nil {
		t.Error("the run's cost was not added")
	}
}

func TestProductionQACouldntRunPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spawn func(agentboxSpawnSpec, io.Writer) (agentResult, error)
		want  string
	}{
		{"agent failure", func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			return agentResult{}, errors.New("error starting container: boom")
		}, "error starting container: boom"},
		{"no qa_result", func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			return agentResult{Status: "error", Error: "agent crashed"}, nil
		}, "the QA run produced no qa_result: agent crashed"},
		{"timeout", func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			return qaCompleted(signUpPassed), errors.New("agentbox timed out after 20m0s")
		}, "agentbox timed out after 20m0s"},
		{"agent reported couldnt_run", func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			return agentResult{QAResult: &qaResult{Status: tasks.QAStatusCouldNotRun, StatusReason: "the site is down"}}, nil
		}, "the site is down"},
		{"browser without read-only", func(spec agentboxSpawnSpec, _ io.Writer) (agentResult, error) {
			// The tool tells the run, which stops the container.
			spec.productionTools.onReadOnlyUnsupported(agenttools.VerifyBrowserReadOnlyUnsupportedReason)
			return agentResult{}, types.ErrJobStoppedByUser
		}, "the QA browser doesn't support read-only checks"},
		{"user stop", func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			return agentResult{}, types.ErrJobStoppedByUser
		}, productionQAStoppedReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qa, _ := runProductionQA(t, productionQAParameters(t), tc.spawn)
			if qa.Status != tasks.QAStatusCouldNotRun || qa.StatusReason != tc.want {
				t.Errorf("qa = %s %q, want couldnt_run %q", qa.Status, qa.StatusReason, tc.want)
			}
			if len(qa.Rounds) != 1 || qa.Rounds[0].Status != tasks.QAStatusCouldNotRun {
				t.Errorf("rounds = %+v", qa.Rounds)
			}
		})
	}
}

func TestProductionQACouldntRunBeforeSpawning(t *testing.T) {
	noSpawn := func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
		return agentResult{}, errors.New("must not run")
	}
	for _, tc := range []struct {
		name string
		edit func(map[string]interface{})
		want string
	}{
		{"no targets", func(p map[string]interface{}) {
			jobs.SetParameterValue[string](p, parameters_enums.ProductionQATargets, "[]")
		}, productionQANoTargetsReason},
		{"http target", func(p map[string]interface{}) {
			jobs.SetParameterValue[string](p, parameters_enums.ProductionQATargets, `[{"repository":"0-a/b","url":"http://x.example.com"}]`)
		}, "is not an https URL"},
		{"QA model without credentials", func(p map[string]interface{}) {
			jobs.SetParameterValue[string](p, parameters_enums.QAAgentType, "codex")
			jobs.SetParameterValue[string](p, parameters_enums.QAModel, "gpt-5.5")
		}, "no credentials for the QA model gpt-5.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := productionQAParameters(t)
			tc.edit(parameters)
			qa, _ := runProductionQA(t, parameters, noSpawn)
			if qa.Status != tasks.QAStatusCouldNotRun || !strings.Contains(qa.StatusReason, tc.want) || len(qa.Rounds) != 0 {
				t.Errorf("qa = %+v, want couldnt_run with %q and no round", qa, tc.want)
			}
		})
	}
}

// The QA browser's download starts once, for the Job's image, before the QA
// agent starts.
func TestProductionQAPrefetchesTheBrowserBeforeTheRun(t *testing.T) {
	var events []string
	cmd := &RunProductionQA{
		spawn: func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			events = append(events, "spawn")
			return qaCompleted(signUpPassed, pricingPassed), nil
		},
		prefetchBrowser: func(ctx context.Context, jobRef string, _ io.Writer) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("prefetch's context has no deadline")
			}
			events = append(events, "prefetch "+jobRef)
		},
	}
	qa, _ := runProductionQAWith(t, cmd, productionQAParameters(t))
	if qa.Status != tasks.QAStatusCompleted {
		t.Errorf("qa = %+v", qa)
	}
	if want := []string{"prefetch " + productionTestBrowser, "spawn"}; strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
}

func TestProductionQANoPrefetchWhenTheRunDoesNotGoAhead(t *testing.T) {
	noPrefetch := func(context.Context, string, io.Writer) { t.Error("prefetched for a run that did not go ahead") }
	t.Run("image refused", func(t *testing.T) {
		parameters := productionQAParameters(t)
		jobs.SetParameterValue[string](parameters, parameters_enums.VerifyBrowserImage, "docker.io/deploymenthq/verify-browser:1.2.0")
		cmd := &RunProductionQA{
			spawn: func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
				return agentResult{}, errors.New("must not run")
			},
			prefetchBrowser: noPrefetch,
		}
		if qa, _ := runProductionQAWith(t, cmd, parameters); qa.Status != tasks.QAStatusCouldNotRun {
			t.Errorf("qa = %+v, want couldnt_run", qa)
		}
	})
	t.Run("agentbox image unavailable", func(t *testing.T) {
		// No spawn seam: holdImage asks Docker, which isn't there.
		t.Setenv("DOCKER_HOST", "unix://"+t.TempDir()+"/no-docker.sock")
		cmd := &RunProductionQA{prefetchBrowser: noPrefetch}
		qa, _ := runProductionQAWith(t, cmd, productionQAParameters(t))
		if qa.Status != tasks.QAStatusCouldNotRun || !strings.Contains(qa.StatusReason, "agentbox image") {
			t.Errorf("qa = %s %q, want couldnt_run naming the agentbox image", qa.Status, qa.StatusReason)
		}
	})
}
