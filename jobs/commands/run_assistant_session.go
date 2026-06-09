package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/sessions"
	"github.com/deployment-io/deployment-runner-kit/types"
	runnerclient "github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/moby/moby/client"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// RunAssistantSession is the runner command for an interactive Assistant
// session. Unlike RunAgentStep (one-shot, batch result), it keeps an agentbox
// container alive in interactive mode for the whole conversation and bridges it
// to the browser: it forwards the agent's streamed output to deployment-server
// (which appends to the thread's message_stream → SSE), and pulls the user's
// turns back to feed the live agent. The container is read-only — sessions plan,
// they don't modify code. Sits after CheckoutRepo, which populated /work.
type RunAssistantSession struct {
	// stopSignal closes when deployment-server reports the Job moved to
	// Stopping (user ended the session, idle/wall-clock/budget cron). For a
	// session, a stop is the normal end, not a failure.
	stopSignal <-chan struct{}
}

// SetStopSignal satisfies jobs.StoppableCommand.
func (rs *RunAssistantSession) SetStopSignal(stop <-chan struct{}) {
	rs.stopSignal = stop
}

const (
	sessionPromptInContainer = agentboxWorkDirInContainer + "/.agentbox-input/system-prompt.txt"
	sessionPollInterval      = 750 * time.Millisecond
)

// planModePrompt instructs the agent for a read-only, plan-mode session. Kept
// in sync with agentbox/cmd/interactive-harness planModePrompt; production
// injects it via APPEND_SYSTEM_PROMPT_FILE at spawn. Built with string
// concatenation because the task-spec fence uses backticks.
const planModePrompt = `You are in plan mode for a code repository. Investigate with read-only tools only — you cannot modify, build, run, or test the code (the sandbox is read-only at the OS level). Do not attempt builds or tests; verification happens later when the task is executed — note what should be verified in the spec's acceptance criteria instead.

Each turn, judge what the user is doing:
- Just asking a question, exploring, or discussing — answer normally and DO NOT emit a task-spec block.
- Working toward a concrete code change to dispatch — maintain a task-spec block at the END of your message, refining it as the task firms up.

Only emit a task-spec once the user has expressed intent to change the code; never fabricate a task from a pure question. Block format:

` + "```task-spec" + `
{"title":"...","goal":"...","context":"...","acceptance_criteria":["..."],"assumptions":["..."],"out_of_scope":["..."],"complexity":"low|medium|high","readiness":"vague|partial|ready","readiness_notes":"..."}
` + "```" + `

Set readiness to "ready" only when the goal, acceptance criteria, and file scope are concrete. Set complexity to the model tier the EXECUTION task needs: "low" = trivial/one-file change, "medium" = a few files with some logic, "high" = multi-file work, refactors, tests, or tricky logic. It's a hint for choosing the execution model; the user can override.`

func (rs *RunAssistantSession) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	orgID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, fmt.Errorf("organization id missing: %s", err)
	}
	jobID, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	if err != nil {
		return parameters, fmt.Errorf("job id missing: %s", err)
	}
	imageRef, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return parameters, fmt.Errorf("agentbox image missing: %s", err)
	}
	if err := pullAgentboxImage(imageRef); err != nil {
		return parameters, fmt.Errorf("error pulling agentbox image: %s", err)
	}
	workDirHost := commandUtils.GetSessionRepositoriesBaseDir(orgID, jobID)
	if err := prepareSessionDirs(workDirHost); err != nil {
		return parameters, fmt.Errorf("error preparing session dirs: %s", err)
	}
	envVars, err := buildSessionSpawnEnvVars(parameters)
	if err != nil {
		return parameters, err
	}
	return parameters, rs.runSession(orgID, jobID, imageRef, workDirHost, envVars, logsWriter)
}

// prepareSessionDirs creates the bind-mounted input/output message dirs and the
// plan-mode prompt file, then chowns them to the agentbox `agent` user so the
// UID-1000 container can read input and write output through the bind mount.
func prepareSessionDirs(workDirHost string) error {
	outDir := filepath.Join(workDirHost, ".agentbox-output", "messages")
	inDir := filepath.Join(workDirHost, ".agentbox-input", "messages")
	for _, d := range []string{outDir, inDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	promptPath := filepath.Join(workDirHost, ".agentbox-input", "system-prompt.txt")
	if err := os.WriteFile(promptPath, []byte(planModePrompt), 0644); err != nil {
		return err
	}
	for _, p := range []string{
		filepath.Join(workDirHost, ".agentbox-output"),
		filepath.Join(workDirHost, ".agentbox-input"),
	} {
		if err := chownTreeToAgentbox(p); err != nil {
			return err
		}
	}
	return nil
}

// buildSessionSpawnEnvVars assembles the interactive agentbox env: AGENT_MODE
// and READ_ONLY pin the long-lived read-only session, SESSION_ID gives the
// agent a stable conversation id, and the prompt file carries plan mode. The
// rest mirrors buildAgentSpawnEnvVars (creds, agent type, model, versions,
// allowlist) minus the one-shot STEP_PROMPT.
func buildSessionSpawnEnvVars(parameters map[string]interface{}) ([]string, error) {
	env := map[string]string{
		"AGENT_MODE":                "interactive",
		"READ_ONLY":                 "1",
		"WORK_DIR":                  agentboxWorkDirInContainer,
		"RESULT_PATH":               agentboxResultPathInCtr,
		"APPEND_SYSTEM_PROMPT_FILE": sessionPromptInContainer,
	}
	if creds, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.AgentEnvVars); err == nil {
		for k, v := range creds {
			env[k] = v
		}
	} else {
		return nil, fmt.Errorf("agent env vars missing — deployment-server should have injected at pickup: %s", err)
	}
	for _, kv := range []struct {
		envKey string
		param  parameters_enums.Key
	}{
		{"SESSION_ID", parameters_enums.SessionUUID},
		{"AGENT_TYPE", parameters_enums.AgentType},
		{"MODEL", parameters_enums.Model},
		{"CLAUDE_CODE_VERSION", parameters_enums.ClaudeCodeVersion},
		{"CODEX_VERSION", parameters_enums.CodexVersion},
	} {
		if v, err := jobs.GetParameterValue[string](parameters, kv.param); err == nil && v != "" {
			env[kv.envKey] = v
		}
	}
	if allowed := mergeAdditionalAllowedHosts(parameters); allowed != "" {
		env["ADDITIONAL_ALLOWED_HOSTS"] = allowed
	}
	return mapToEnvSlice(env), nil
}

// runSession spawns the interactive container, runs the output-forward and
// input-pump bridge loops alongside it, and blocks until the container exits or
// the session is stopped (the normal end). A stop is not a failure.
func (rs *RunAssistantSession) runSession(orgID, jobID, imageRef, workDirHost string, envVars []string, logsWriter io.Writer) error {
	dockerCtx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	containerID, err := createAgentboxContainer(dockerCtx, cli, agentboxSpawnSpec{imageRef: imageRef, workDirHost: workDirHost, env: envVars})
	if err != nil {
		return err
	}
	var logsWg sync.WaitGroup
	defer logsWg.Wait()
	defer func() { _ = removeContainer(dockerCtx, cli, containerID) }()
	if err := cli.ContainerStart(dockerCtx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("error starting container: %s", err)
	}
	logsWg.Add(1)
	go func() {
		defer logsWg.Done()
		streamContainerLogs(dockerCtx, cli, containerID, logsWriter)
	}()

	mf := &messageForwarder{
		dir:   filepath.Join(workDirHost, ".agentbox-output", "messages"),
		orgID: orgID, jobID: jobID, logsWriter: logsWriter, seen: map[string]bool{},
	}
	ip := &inputPump{
		dir:   filepath.Join(workDirHost, ".agentbox-input", "messages"),
		orgID: orgID, jobID: jobID, logsWriter: logsWriter,
	}
	stopBridge := make(chan struct{})
	var bridgeWg sync.WaitGroup
	bridgeWg.Add(2)
	go func() { defer bridgeWg.Done(); runSessionTicker(stopBridge, mf.tick) }()
	go func() { defer bridgeWg.Done(); runSessionTicker(stopBridge, ip.tick) }()

	waitCtx, cancelWait := context.WithTimeout(dockerCtx, defaultWallClockTimeout)
	defer cancelWait()
	_, waitErr := waitForContainerExit(waitCtx, cli, containerID, rs.stopSignal)
	close(stopBridge)
	bridgeWg.Wait()
	mf.tick() // final drain of any buffered output
	logSessionOutcome(workDirHost, logsWriter)
	if waitErr != nil && !errors.Is(waitErr, types.ErrJobStoppedByUser) {
		return waitErr
	}
	return nil
}

// runSessionTicker calls fn every sessionPollInterval until stop closes.
func runSessionTicker(stop <-chan struct{}, fn func()) {
	t := time.NewTicker(sessionPollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			fn()
		}
	}
}

// messageForwarder reads agentbox's streamed output files in order and forwards
// them to deployment-server as assistant-message deltas. agentbox emits a run
// of "chunk" records (deltas) then a "final" per assistant message; a new
// MessageID is minted at the first chunk after each final so the UI groups
// deltas correctly.
type messageForwarder struct {
	dir          string
	orgID, jobID string
	logsWriter   io.Writer
	seen         map[string]bool
	currentMsgID string
}

func (mf *messageForwarder) tick() {
	entries, _ := os.ReadDir(mf.dir)
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !mf.seen[e.Name()] {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var batch []sessions.AppendMessageDtoV1
	for _, n := range names {
		mf.seen[n] = true
		b, err := os.ReadFile(filepath.Join(mf.dir, n))
		if err != nil {
			continue
		}
		var rec struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		switch rec.Type {
		case "chunk":
			if mf.currentMsgID == "" {
				mf.currentMsgID = primitive.NewObjectID().Hex()
			}
			batch = append(batch, sessions.AppendMessageDtoV1{JobID: mf.jobID, MessageID: mf.currentMsgID, Content: rec.Text})
		case "final":
			if mf.currentMsgID != "" {
				batch = append(batch, sessions.AppendMessageDtoV1{JobID: mf.jobID, MessageID: mf.currentMsgID, IsDone: true})
				mf.currentMsgID = ""
			}
		}
	}
	if len(batch) == 0 {
		return
	}
	if err := runnerclient.Get().UpdateSessionMessages(batch, mf.orgID); err != nil {
		io.WriteString(mf.logsWriter, fmt.Sprintf("session: error forwarding messages: %s\n", err))
		// keep the names marked seen — a failed batch is dropped rather than
		// replayed, matching the live-stream "prefer a gap over a dup" stance.
	}
}

// inputPump pulls the user's new turns from deployment-server and writes them
// into agentbox's input dir (atomic temp+rename) for the live agent to consume.
type inputPump struct {
	dir          string
	orgID, jobID string
	logsWriter   io.Writer
	afterTs      int64
	seq          int
}

func (ip *inputPump) tick() {
	msgs, err := runnerclient.Get().GetSessionInput(ip.jobID, ip.afterTs, ip.orgID)
	if err != nil {
		io.WriteString(ip.logsWriter, fmt.Sprintf("session: error pulling input: %s\n", err))
		return
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Ts < msgs[j].Ts })
	for _, m := range msgs {
		ip.seq++
		ip.write(m)
		if m.Ts > ip.afterTs {
			ip.afterTs = m.Ts
		}
	}
}

func (ip *inputPump) write(m sessions.UserMessageDtoV1) {
	rec := map[string]any{"id": m.ID, "content": m.Content, "ts": m.Ts}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	name := fmt.Sprintf("%010d.json", ip.seq)
	tmp := filepath.Join(ip.dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(ip.dir, name))
}

// logSessionOutcome best-effort surfaces the final agent result and whether a
// task spec was produced. Spec persistence to the Session record is wired in a
// later phase (app-server convert flow).
func logSessionOutcome(workDirHost string, logsWriter io.Writer) {
	if result, err := readAgentResult(workDirHost); err == nil {
		io.WriteString(logsWriter, fmt.Sprintf("session ended: status=%s\n", result.Status))
	}
	specPath := filepath.Join(workDirHost, ".agentbox-output", "task-spec.json")
	if b, err := os.ReadFile(specPath); err == nil && len(b) > 0 {
		io.WriteString(logsWriter, "session produced a task spec\n")
	}
}
