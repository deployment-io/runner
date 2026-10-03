package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// MaterializeContext pulls the org's stored context from the control plane and writes it under
// /work/context for the agent to read. It runs BEFORE CheckoutRepo and creates its own dir, so
// it works even when no repos are checked out (the repo-less Assistant plan session, where the
// catalog is the input to repo discovery). Failures degrade gracefully — a missing or
// unavailable context never fails the job; the agent falls back to live discovery.
type MaterializeContext struct{}

// infraRefreshTimeout bounds the inline infrastructure rescan, which sits on the run's critical
// path (unlike the 15-minute BuildInfraContext Job); infraSaveTimeout bounds the save RPC after it.
// Variables so tests can shorten them.
var (
	infraRefreshTimeout = 90 * time.Second
	infraSaveTimeout    = 30 * time.Second
)

// contextClient is the slice of the deployment-server client MaterializeContext uses.
type contextClient interface {
	MaterializeContext(organizationID string, scopes []context_pack.Scope, refreshRepoCatalog bool) ([]context_pack.ContextFileV1, error)
	SaveInfraContext(organizationID string, packsJSON string, timeout time.Duration) (int, error)
}

// Seams for tests: the control-plane client and the infrastructure scan.
var (
	getContextClient = func() contextClient { return client.Get() }
	scanInfra        = buildInfraPacks
)

func (m *MaterializeContext) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	orgID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Skipping context materialization (organization id missing): %s\n", err))
		return parameters, nil
	}
	contextDir, ok := contextDirFor(orgID, parameters)
	if !ok {
		// Not a Task/Session run — there's no /work to materialize into.
		return parameters, nil
	}

	// Refresh stale context first (decided at Job creation), so this run reads the fresh copy.
	// Every refresh failure degrades to the stored context.
	if refreshInfra, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.RefreshInfraContext); refreshInfra {
		refreshInfraContext(orgID, parameters, logsWriter)
	}
	refreshRepoCatalog, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.RefreshRepoCatalog)
	if refreshRepoCatalog {
		io.WriteString(logsWriter, "Context refresh: asking for a fresh repo catalog\n")
	}

	// Materialize the org's whole context. We send no explicit scopes: the server resolves them
	// (the Org pack + every Cluster/Account pack the connectors built), because the runner can't
	// enumerate the cluster scopes — the connectors discover them live, server-side. Each file
	// arrives namespaced under its scope's path (context_pack.ScopePath), which the write loop below
	// honors transparently via filepath.Join + MkdirAll.
	files, err := getContextClient().MaterializeContext(orgID, nil, refreshRepoCatalog)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Context unavailable, continuing without it: %s\n", err))
		return parameters, nil
	}
	if len(files) == 0 {
		io.WriteString(logsWriter, "No context to materialize (none built yet); continuing.\n")
		return parameters, nil
	}

	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not create context dir, continuing without it: %s\n", err))
		return parameters, nil
	}
	written := 0
	for _, f := range files {
		// Anchor under contextDir; filepath.Clean("/"+Path) defends against a stray ".." in Path.
		dest := filepath.Join(contextDir, filepath.Clean("/"+f.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("  skipping %s: %s\n", f.Path, err))
			continue
		}
		if err := os.WriteFile(dest, []byte(f.Content), 0o644); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("  skipping %s: %s\n", f.Path, err))
			continue
		}
		written++
	}
	// Make the tree readable by the agentbox (UID 1000) through the bind mount.
	if err := chownTreeToAgentbox(contextDir); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not chown context dir: %s\n", err))
	}
	io.WriteString(logsWriter, fmt.Sprintf("Materialized %d context file(s) into /work/context\n", written))
	return parameters, nil
}

// contextDirFor returns the host path that bind-mounts to /work/context for a Task or Session
// run, and whether this is such a run.
func contextDirFor(orgID string, parameters map[string]interface{}) (string, bool) {
	var base string
	switch {
	case commandUtils.IsTasksMode(parameters):
		taskID, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
		if err != nil {
			return "", false
		}
		base = commandUtils.GetTaskRepositoriesBaseDir(orgID, taskID)
	case commandUtils.IsSessionMode(parameters):
		jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
		if err != nil {
			return "", false
		}
		base = commandUtils.GetSessionRepositoriesBaseDir(orgID, jobID)
	default:
		return "", false
	}
	return filepath.Join(base, "context"), true
}

// refreshInfraContext rescans this runner's own infrastructure (every context source, under
// infraRefreshTimeout) and saves the result through deployment-server before the context is
// fetched. Any failure is logged and the stored context is used.
func refreshInfraContext(orgID string, parameters map[string]interface{}, logsWriter io.Writer) {
	io.WriteString(logsWriter, "Context refresh: rescanning this runner's infrastructure\n")
	saved, err := rescanAndSaveInfra(orgID, parameters, logsWriter)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Context refresh: infrastructure rescan failed (%s) — using the stored context\n", err))
		return
	}
	io.WriteString(logsWriter, fmt.Sprintf("Context refresh: saved %d scope(s)\n", saved))
}

// rescanAndSaveInfra scans and saves, each step bounded. The scan runs under infraRefreshTimeout
// (every source call, the IAM self-grant included, takes that deadline) and, as a backstop for a
// call that ignores it, in its own goroutine that the run abandons once the deadline passes — it
// finishes in the background, its log lines dropped. The save runs over its own connection with an
// infraSaveTimeout deadline (see client.SaveInfraContext), so it can't hold up the materialize call.
func rescanAndSaveInfra(orgID string, parameters map[string]interface{}, logsWriter io.Writer) (int, error) {
	logs := &cutoffWriter{w: logsWriter}
	defer logs.cutOff()

	scan, contextClient := scanInfra, getContextClient()
	// The scan gets its own copy of the parameters: if abandoned, it must not read the Job's map
	// while later commands write it.
	scanParameters := make(map[string]interface{}, len(parameters))
	for k, v := range parameters {
		scanParameters[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), infraRefreshTimeout)
	defer cancel()
	packs, err := runBounded(ctx, func() ([]context_pack.ScopedPack, error) {
		return scan(ctx, scanParameters, logs)
	})
	if err == nil && ctx.Err() != nil {
		// A source that hit the deadline was skipped; don't save a scan we know is incomplete.
		err = ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, fmt.Errorf("timed out after %s", infraRefreshTimeout)
	}
	if err != nil {
		return 0, err
	}
	packsJSON, err := json.Marshal(packs)
	if err != nil {
		return 0, fmt.Errorf("failed to encode context packs: %w", err)
	}

	saved, err := contextClient.SaveInfraContext(orgID, string(packsJSON), infraSaveTimeout)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, fmt.Errorf("saving timed out after %s", infraSaveTimeout)
	}
	return saved, err
}

// runBounded runs fn in a goroutine and returns its result, or ctx's error once ctx is done; fn
// keeps running in the background in that case and its result is discarded.
func runBounded[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1) // buffered: an abandoned fn never blocks on a gone reader
	go func() {
		value, err := fn()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// cutoffWriter forwards writes until cutOff, then drops them, so a scan abandoned past its
// deadline can't write into the Job's logs after MaterializeContext has moved on.
type cutoffWriter struct {
	mu  sync.Mutex
	w   io.Writer
	off bool
}

func (c *cutoffWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off {
		return len(p), nil
	}
	return c.w.Write(p)
}

func (c *cutoffWriter) cutOff() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off = true
}
