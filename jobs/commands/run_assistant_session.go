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
	// sessionWallClockHardCap is the runner-side backstop on a session
	// container's lifetime. The REAL wall-clock enforcement is the
	// deployment-server idle/wall-clock cron (default 4h, honors the session's
	// WallClockMaxHours), which MarkStopping's the Job → a clean, graceful end.
	// This cap is deliberately well above that so the cron always wins; it only
	// fires if the server never stops the session (server unreachable / broken
	// heartbeat) — a genuinely orphaned container, where reporting failed is
	// acceptable. Equal caps would race and mislabel a normal time-limit end as
	// Failed, so keep this strictly larger than the server's max.
	sessionWallClockHardCap = 8 * time.Hour
)

// planModePrompt instructs the agent for a read-only, plan-mode session. Kept
// in sync with agentbox/cmd/interactive-harness planModePrompt; production
// injects it via APPEND_SYSTEM_PROMPT_FILE at spawn. Built with string
// concatenation because the task-spec fence uses backticks.
const planModePrompt = `You are in plan mode for one or more code repositories, each checked out as a subdirectory of your working directory. Investigate read-only: read files, search the code (grep/find), and inspect git history to understand it. Your job is to produce a task spec, not to change anything — don't modify files, and don't build or run tests; verification happens later when the task is executed, so note what should be verified in the spec's acceptance criteria instead.

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
	// The work dir is keyed by OrganizationIDNamespace — the same param
	// CheckoutRepo's runForSession used to clone the repo. That differs from
	// OrganizationIdFromJob (the real org used for the message-stream bridge)
	// under saas-runner mode, where the namespace is rewritten to the global org.
	workDirOrg, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return parameters, fmt.Errorf("organization id namespace missing: %s", err)
	}
	imageRef, err := jobs.GetParameterValue[string](parameters, parameters_enums.AgentboxImage)
	if err != nil {
		return parameters, fmt.Errorf("agentbox image missing: %s", err)
	}
	if err := pullAgentboxImage(imageRef); err != nil {
		return parameters, fmt.Errorf("error pulling agentbox image: %s", err)
	}
	workDirHost := commandUtils.GetSessionRepositoriesBaseDir(workDirOrg, jobID)
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
		// Disable agentbox's stdout no-activity watchdog for sessions: an
		// interactive agent is idle by design between user turns, so the
		// watchdog (built for batch — detect a hung subprocess) would kill a
		// session whenever the user pauses longer than its timeout to think.
		// The server-side idle cron (user-message-based) is the idle-killer.
		"NO_ACTIVITY_TIMEOUT": "0",
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
		orgID: orgID, jobID: jobID, logsWriter: logsWriter, seen: map[string]bool{},
	}
	sf := &specForwarder{
		path:  filepath.Join(workDirHost, ".agentbox-output", "task-spec.json"),
		orgID: orgID, jobID: jobID, logsWriter: logsWriter,
	}
	stopBridge := make(chan struct{})
	var bridgeWg sync.WaitGroup
	bridgeWg.Add(3)
	go func() { defer bridgeWg.Done(); runSessionTicker(stopBridge, mf.tick) }()
	go func() { defer bridgeWg.Done(); runSessionTicker(stopBridge, ip.tick) }()
	go func() { defer bridgeWg.Done(); runSessionTicker(stopBridge, sf.tick) }()

	waitCtx, cancelWait := context.WithTimeout(dockerCtx, sessionWallClockHardCap)
	defer cancelWait()
	exitCode, waitErr := waitForContainerExit(waitCtx, cli, containerID, rs.stopSignal)
	close(stopBridge)
	bridgeWg.Wait()
	mf.tick() // final drain of any buffered output
	sf.tick() // final spec snapshot
	logSessionOutcome(workDirHost, logsWriter)
	if errors.Is(waitErr, types.ErrJobStoppedByUser) {
		return nil // user / cron / convert stop — the normal session end
	}
	if waitErr != nil {
		return waitErr // wall-clock cap or a wait error
	}
	// Natural container exit. A healthy interactive session agent doesn't exit
	// on its own — a non-zero code means it failed (API credits/auth exhausted,
	// a fatal rate-limit, or a crash). Surface the reason to the chat so the
	// user sees why it stopped, and return an error so the Job is reported
	// failed (deployment-server then marks the session Failed).
	if exitCode != 0 {
		failMsg := sessionAgentFailureMessage(workDirHost)
		rs.forwardSessionFailure(orgID, jobID, failMsg, logsWriter)
		return fmt.Errorf("session agent exited with code %d: %s", exitCode, failMsg)
	}
	return nil
}

// sessionAgentFailureMessage builds a user-facing reason for a failed session
// agent, preferring agentbox's classified error from result.json (auth / credit
// / rate-limit context) when present.
func sessionAgentFailureMessage(workDirHost string) string {
	if result, err := readAgentResult(workDirHost); err == nil && strings.TrimSpace(result.Error) != "" {
		return result.Error
	}
	return "the agent stopped unexpectedly — it may have run out of API credits, hit an auth or rate-limit error, or crashed"
}

// forwardSessionFailure posts a final assistant message to the session thread so
// the chat shows why the agent stopped instead of going silent. A turn-end
// follows it: a dead agent never sends its own boundary, and without one the
// UI's composer (gated on the turn boundary) would stay locked until the
// session goes terminal.
func (rs *RunAssistantSession) forwardSessionFailure(orgID, jobID, msg string, logsWriter io.Writer) {
	err := runnerclient.Get().UpdateSessionMessages([]sessions.AppendMessageDtoV1{{
		JobID:     jobID,
		MessageID: primitive.NewObjectID().Hex(),
		Content:   "⚠️ " + msg,
		IsDone:    true,
	}, {
		JobID:     jobID,
		MessageID: primitive.NewObjectID().Hex(),
		IsDone:    true,
		TurnEnd:   true,
	}}, orgID)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("session: error forwarding failure message: %s\n", err))
	}
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
	if len(names) == 0 {
		return
	}
	// Read unseen files in order. A readable file is consumed even if its JSON
	// is corrupt (agentbox writes atomically, so a parse failure is permanent —
	// don't re-read it). Stop on a transient read error to preserve order.
	var records []outputRec
	var consumed []string
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(mf.dir, n))
		if err != nil {
			break // retry from n next tick
		}
		var rec outputRec
		if json.Unmarshal(b, &rec) == nil {
			records = append(records, rec)
		}
		consumed = append(consumed, n)
	}
	// Rotate over a COPY of the cursor; commit neither mf.seen nor
	// mf.currentMsgID until the forward succeeds, so a failed forward is retried
	// verbatim next tick (at-least-once) rather than dropped. Dropping a batch
	// that held the "final" would lose the whole assistant message. The server
	// persists idempotently, so a redelivered final is a no-op.
	batch, endMsgID := rotateOutputBatch(records, mf.jobID, mf.currentMsgID)
	if len(batch) == 0 {
		// Only unknown/corrupt files this pass; still advance seen so we don't
		// re-scan them every tick.
		for _, n := range consumed {
			mf.seen[n] = true
		}
		return
	}
	if err := runnerclient.Get().UpdateSessionMessages(batch, mf.orgID); err != nil {
		io.WriteString(mf.logsWriter, fmt.Sprintf("session: error forwarding messages: %s\n", err))
		return // do NOT advance seen or currentMsgID — retry the whole batch next tick
	}
	for _, n := range consumed {
		mf.seen[n] = true
	}
	mf.currentMsgID = endMsgID
}

// outputRec is one parsed agentbox output record (.agentbox-output/messages).
type outputRec struct {
	Type string `json:"type"` // "chunk" | "final" | "turn_end"
	Text string `json:"text"`
}

// rotateOutputBatch turns a run of parsed output records into assistant-message
// deltas: consecutive "chunk"s share one MessageID, "final" closes it, and
// "turn_end" forwards the agent's turn boundary as its own control update.
// startMsgID continues an in-flight message across ticks ("" mints a new id at
// the first chunk); endMsgID is the cursor to carry to the next call. Pure (no
// fs / client) so the chunk/final grouping is unit-testable.
func rotateOutputBatch(records []outputRec, jobID, startMsgID string) (batch []sessions.AppendMessageDtoV1, endMsgID string) {
	msgID := startMsgID
	for _, rec := range records {
		switch rec.Type {
		case "chunk":
			if msgID == "" {
				msgID = primitive.NewObjectID().Hex()
			}
			batch = append(batch, sessions.AppendMessageDtoV1{JobID: jobID, MessageID: msgID, Content: rec.Text})
		case "final":
			if msgID != "" {
				batch = append(batch, sessions.AppendMessageDtoV1{JobID: jobID, MessageID: msgID, IsDone: true})
				msgID = ""
			}
		case "turn_end":
			if msgID != "" {
				// Defensive: agentbox always finalizes a message before the
				// boundary, but never let one ride past it half-open.
				batch = append(batch, sessions.AppendMessageDtoV1{JobID: jobID, MessageID: msgID, IsDone: true})
				msgID = ""
			}
			batch = append(batch, sessions.AppendMessageDtoV1{JobID: jobID, MessageID: primitive.NewObjectID().Hex(), IsDone: true, TurnEnd: true})
		}
	}
	return batch, msgID
}

// inputPump pulls the user's new turns from deployment-server and writes them
// into agentbox's input dir (atomic temp+rename) for the live agent to consume.
type inputPump struct {
	dir          string
	orgID, jobID string
	logsWriter   io.Writer
	afterTs      int64
	seq          int
	seen         map[string]bool // delivered message ids — dedup the inclusive ($gte) AfterTs boundary
}

func (ip *inputPump) tick() {
	msgs, err := runnerclient.Get().GetSessionInput(ip.jobID, ip.afterTs, ip.orgID)
	if err != nil {
		io.WriteString(ip.logsWriter, fmt.Sprintf("session: error pulling input: %s\n", err))
		return
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Ts < msgs[j].Ts })
	for _, m := range filterUndelivered(msgs, ip.seen) {
		ip.seq++
		ip.write(m)
		ip.seen[m.ID] = true
		if m.Ts > ip.afterTs {
			ip.afterTs = m.Ts
		}
	}
}

// filterUndelivered drops messages already delivered (by id), so the server's
// inclusive ($gte AfterTs) input query can re-return same-second turns without
// the runner replaying them to the agent. Pure.
func filterUndelivered(msgs []sessions.UserMessageDtoV1, seen map[string]bool) []sessions.UserMessageDtoV1 {
	out := make([]sessions.UserMessageDtoV1, 0, len(msgs))
	for _, m := range msgs {
		if !seen[m.ID] {
			out = append(out, m)
		}
	}
	return out
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

// specForwarder reads agentbox's task-spec.json each tick and forwards it to
// deployment-server (which persists it to Session.StructuredSpec) whenever the
// content changes — the planning agent refines the spec across turns. The
// convert flow reads the persisted spec. Best-effort: a failed forward leaves
// lastContent unchanged so the next tick retries.
type specForwarder struct {
	path         string
	orgID, jobID string
	logsWriter   io.Writer
	lastContent  string
}

func (sf *specForwarder) tick() {
	b, err := os.ReadFile(sf.path)
	if err != nil {
		return // not written yet
	}
	if string(b) == sf.lastContent {
		return // unchanged since the last successful forward
	}
	var rec struct {
		Title       string   `json:"title"`
		Goal        string   `json:"goal"`
		Context     string   `json:"context"`
		Acceptance  []string `json:"acceptance_criteria"`
		Assumptions []string `json:"assumptions"`
		OutOfScope  []string `json:"out_of_scope"`
		Complexity  string   `json:"complexity"`
		Readiness   string   `json:"readiness"`
		Notes       string   `json:"readiness_notes"`
	}
	if json.Unmarshal(b, &rec) != nil {
		return
	}
	if err := runnerclient.Get().UpdateSessionSpec(sessions.UpdateSpecDtoV1{
		JobID:       sf.jobID,
		Title:       rec.Title,
		Goal:        rec.Goal,
		Context:     rec.Context,
		Acceptance:  rec.Acceptance,
		Assumptions: rec.Assumptions,
		OutOfScope:  rec.OutOfScope,
		Complexity:  rec.Complexity,
		Readiness:   rec.Readiness,
		Notes:       rec.Notes,
	}, sf.orgID); err != nil {
		io.WriteString(sf.logsWriter, fmt.Sprintf("session: error forwarding spec: %s\n", err))
		return // keep lastContent unchanged → retry next tick
	}
	sf.lastContent = string(b)
}
