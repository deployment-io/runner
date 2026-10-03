package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/context_pack_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/jobs/commands/context_sources"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// fakeContextClient records the materialize and save calls.
type fakeContextClient struct {
	materializeCalls []bool // RefreshRepoCatalog of each call
	saves            []string
	saveErr          error
}

func (f *fakeContextClient) MaterializeContext(_ string, _ []context_pack.Scope, refreshRepoCatalog bool) ([]context_pack.ContextFileV1, error) {
	f.materializeCalls = append(f.materializeCalls, refreshRepoCatalog)
	return []context_pack.ContextFileV1{{Path: "index.md", Content: "# ctx\n"}}, nil
}

func (f *fakeContextClient) SaveInfraContext(_ string, packsJSON string, _ time.Duration) (int, error) {
	f.saves = append(f.saves, packsJSON)
	if f.saveErr != nil {
		return 0, f.saveErr
	}
	var packs []context_pack.ScopedPack
	_ = json.Unmarshal([]byte(packsJSON), &packs)
	return len(packs), nil
}

// withContextFakes swaps the client and the infrastructure scan for the test.
func withContextFakes(t *testing.T, c *fakeContextClient, scan func(context.Context, map[string]interface{}, io.Writer) ([]context_pack.ScopedPack, error)) {
	t.Helper()
	prevClient, prevScan := getContextClient, scanInfra
	t.Cleanup(func() { getContextClient, scanInfra = prevClient, prevScan })
	getContextClient = func() contextClient { return c }
	scanInfra = scan
}

// taskParameters returns a Task-mode Job's parameters and removes its context dir afterwards.
func taskParameters(t *testing.T) map[string]interface{} {
	t.Helper()
	orgID, taskID := primitive.NewObjectID().Hex(), primitive.NewObjectID().Hex()
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(commandUtils.GetTaskRepositoriesBaseDir(orgID, taskID))) })
	p := map[string]interface{}{}
	jobs.SetParameterValue[string](p, parameters_enums.OrganizationIDNamespace, orgID)
	jobs.SetParameterValue[string](p, parameters_enums.TaskID, taskID)
	return p
}

func oneClusterScan(context.Context, map[string]interface{}, io.Writer) ([]context_pack.ScopedPack, error) {
	return []context_pack.ScopedPack{{Scope: context_pack.Scope{Level: context_pack_enums.Cluster, ID: "arn:aws:ecs:eu-west-1:1:cluster/a"}}}, nil
}

func failIfScanned(t *testing.T) func(context.Context, map[string]interface{}, io.Writer) ([]context_pack.ScopedPack, error) {
	return func(context.Context, map[string]interface{}, io.Writer) ([]context_pack.ScopedPack, error) {
		t.Errorf("infrastructure scanned without RefreshInfraContext")
		return nil, nil
	}
}

func runMaterialize(t *testing.T, p map[string]interface{}) string {
	t.Helper()
	var logs bytes.Buffer
	if _, err := (&MaterializeContext{}).Run(p, &logs); err != nil {
		t.Fatalf("MaterializeContext: %v", err)
	}
	return logs.String()
}

func TestMaterializeContext_RefreshInfraSavesAndLogs(t *testing.T) {
	c := &fakeContextClient{}
	withContextFakes(t, c, oneClusterScan)
	p := taskParameters(t)
	jobs.SetParameterValue[bool](p, parameters_enums.RefreshInfraContext, true)

	logs := runMaterialize(t, p)
	if len(c.saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(c.saves))
	}
	for _, line := range []string{"Context refresh: rescanning this runner's infrastructure", "Context refresh: saved 1 scope(s)"} {
		if !strings.Contains(logs, line) {
			t.Errorf("logs missing %q:\n%s", line, logs)
		}
	}
	if len(c.materializeCalls) != 1 || c.materializeCalls[0] {
		t.Errorf("materialize calls = %v, want one without the catalog refresh", c.materializeCalls)
	}
}

func TestMaterializeContext_FailedScanStillMaterializes(t *testing.T) {
	c := &fakeContextClient{}
	withContextFakes(t, c, func(context.Context, map[string]interface{}, io.Writer) ([]context_pack.ScopedPack, error) {
		return nil, errors.New("redaction failed")
	})
	p := taskParameters(t)
	jobs.SetParameterValue[bool](p, parameters_enums.RefreshInfraContext, true)

	logs := runMaterialize(t, p)
	if !strings.Contains(logs, "Context refresh: infrastructure rescan failed (redaction failed) — using the stored context") {
		t.Errorf("logs missing the failure line:\n%s", logs)
	}
	if len(c.saves) != 0 || len(c.materializeCalls) != 1 {
		t.Errorf("saves=%d materialize=%d, want 0, 1", len(c.saves), len(c.materializeCalls))
	}
}

func TestMaterializeContext_FailedSaveStillMaterializes(t *testing.T) {
	c := &fakeContextClient{saveErr: errors.New("connection reset")}
	withContextFakes(t, c, oneClusterScan)
	p := taskParameters(t)
	jobs.SetParameterValue[bool](p, parameters_enums.RefreshInfraContext, true)

	logs := runMaterialize(t, p)
	if !strings.Contains(logs, "Context refresh: infrastructure rescan failed (connection reset) — using the stored context") {
		t.Errorf("logs missing the failure line:\n%s", logs)
	}
	if len(c.materializeCalls) != 1 {
		t.Errorf("materialize calls = %d, want 1", len(c.materializeCalls))
	}
}

func TestMaterializeContext_RefreshRepoCatalogIsPassedThrough(t *testing.T) {
	c := &fakeContextClient{}
	withContextFakes(t, c, failIfScanned(t))
	p := taskParameters(t)
	jobs.SetParameterValue[bool](p, parameters_enums.RefreshRepoCatalog, true)

	logs := runMaterialize(t, p)
	if !strings.Contains(logs, "Context refresh: asking for a fresh repo catalog") {
		t.Errorf("logs missing the catalog line:\n%s", logs)
	}
	if len(c.materializeCalls) != 1 || !c.materializeCalls[0] {
		t.Errorf("materialize calls = %v, want one with the catalog refresh", c.materializeCalls)
	}
}

func TestMaterializeContext_NoRefreshParametersIsOnePlainCall(t *testing.T) {
	c := &fakeContextClient{}
	withContextFakes(t, c, failIfScanned(t))

	logs := runMaterialize(t, taskParameters(t))
	if strings.Contains(logs, "Context refresh") {
		t.Errorf("logs mention a refresh without the parameters:\n%s", logs)
	}
	if len(c.materializeCalls) != 1 || c.materializeCalls[0] || len(c.saves) != 0 {
		t.Errorf("materialize=%v saves=%d, want one plain call and no save", c.materializeCalls, len(c.saves))
	}
}

// fakeSource is a context source that returns fixed results.
type fakeSource struct {
	name    string
	results []context_sources.Result
	err     error
}

func (s *fakeSource) Name() string { return s.name }
func (s *fakeSource) Build(context.Context, map[string]interface{}, io.Writer) ([]context_sources.Result, error) {
	return s.results, s.err
}

// BuildInfraContext still groups by scope, normalizes the Org scope to the org id, skips a failed
// source, and writes the []ScopedPack JSON to JobOutput.
func TestBuildInfraContext_Output(t *testing.T) {
	prev := infraSources
	t.Cleanup(func() { infraSources = prev })
	cluster := context_pack.Scope{Level: context_pack_enums.Cluster, ID: "arn:aws:ecs:eu-west-1:1:cluster/a"}
	infraSources = func() []context_sources.Source {
		return []context_sources.Source{
			&fakeSource{name: "broken", err: errors.New("boom")},
			&fakeSource{name: "ok", results: []context_sources.Result{
				{Scope: cluster, Artifacts: []context_pack.Artifact{{Name: "a.json", Data: []interface{}{"x"}}}, Entries: []context_pack.ManifestEntry{{Path: "a.json", Source: "ok"}}},
				{Scope: context_pack.Scope{Level: context_pack_enums.Org, ID: "ignored"}, Gaps: []string{"no access"}},
			}},
		}
	}
	p := map[string]interface{}{}
	jobs.SetParameterValue[string](p, parameters_enums.OrganizationIDNamespace, "org-1")
	var logs bytes.Buffer
	out, err := (&BuildInfraContext{}).Run(p, &logs)
	if err != nil {
		t.Fatalf("BuildInfraContext: %v", err)
	}
	raw, err := jobs.GetParameterValue[string](out, parameters_enums.JobOutput)
	if err != nil {
		t.Fatalf("JobOutput: %v", err)
	}
	var packs []context_pack.ScopedPack
	if err := json.Unmarshal([]byte(raw), &packs); err != nil {
		t.Fatalf("JobOutput is not []ScopedPack JSON: %v", err)
	}
	if len(packs) != 2 || packs[0].Scope != cluster || packs[1].Scope.ID != "org-1" {
		t.Fatalf("packs = %+v, want the cluster then the org-1 scope", packs)
	}
	if len(packs[0].Pack.Artifacts) != 1 || packs[0].Pack.Manifest.PackVersion != contextPackVersion || packs[1].Pack.Manifest.Gaps[0] != "no access" {
		t.Errorf("pack contents = %+v", packs)
	}
	for _, line := range []string{"Building infra context pack...", "Running 2 context source(s)...", "source broken failed", "Context pack built: 2 scope(s)"} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("logs missing %q:\n%s", line, logs.String())
		}
	}
}

// stalledSaveClient's save behaves like a save to a server that never answers: it returns the
// connection's deadline error once the timeout it was given passes.
type stalledSaveClient struct {
	fakeContextClient
	timeouts []time.Duration
}

func (s *stalledSaveClient) SaveInfraContext(_ string, _ string, timeout time.Duration) (int, error) {
	s.timeouts = append(s.timeouts, timeout)
	time.Sleep(timeout)
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

func shortenRefreshTimeouts(t *testing.T) {
	t.Helper()
	prevScan, prevSave := infraRefreshTimeout, infraSaveTimeout
	t.Cleanup(func() { infraRefreshTimeout, infraSaveTimeout = prevScan, prevSave })
	infraRefreshTimeout, infraSaveTimeout = 20*time.Millisecond, 20*time.Millisecond
}

func TestMaterializeContext_ScanIgnoringTheDeadlineIsAbandoned(t *testing.T) {
	shortenRefreshTimeouts(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := &fakeContextClient{}
	// A scan stuck in a call that doesn't take the deadline (the IAM self-grant).
	withContextFakes(t, c, func(_ context.Context, _ map[string]interface{}, logs io.Writer) ([]context_pack.ScopedPack, error) {
		<-release
		io.WriteString(logs, "late scan line\n")
		return nil, nil
	})
	p := taskParameters(t)
	jobs.SetParameterValue[bool](p, parameters_enums.RefreshInfraContext, true)

	start := time.Now()
	logs := runMaterialize(t, p)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("MaterializeContext took %s with a stuck scan, want ~infraRefreshTimeout", elapsed)
	}
	if !strings.Contains(logs, "Context refresh: infrastructure rescan failed (timed out after 20ms) — using the stored context") {
		t.Errorf("logs missing the timeout line:\n%s", logs)
	}
	if len(c.saves) != 0 || len(c.materializeCalls) != 1 {
		t.Errorf("saves=%d materialize=%d, want 0, 1", len(c.saves), len(c.materializeCalls))
	}
}

func TestMaterializeContext_StalledSaveTimesOut(t *testing.T) {
	shortenRefreshTimeouts(t)
	c := &stalledSaveClient{}
	withContextFakes(t, &c.fakeContextClient, oneClusterScan)
	getContextClient = func() contextClient { return c }
	p := taskParameters(t)
	jobs.SetParameterValue[bool](p, parameters_enums.RefreshInfraContext, true)

	logs := runMaterialize(t, p)
	if len(c.timeouts) != 1 || c.timeouts[0] != infraSaveTimeout {
		t.Errorf("save timeouts = %v, want [%s]", c.timeouts, infraSaveTimeout)
	}
	if !strings.Contains(logs, "Context refresh: infrastructure rescan failed (saving timed out after 20ms) — using the stored context") {
		t.Errorf("logs missing the timeout line:\n%s", logs)
	}
	if len(c.materializeCalls) != 1 {
		t.Errorf("materialize calls = %d, want 1", len(c.materializeCalls))
	}
}
