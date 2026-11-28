package commands

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/sessions"
)

// planSink records what the forwarder sent for the spec and the plan, in
// order, and can be made to fail or refuse.
type planSink struct {
	calls    []string // "spec:<title>" / "plan:<first key>"
	plans    []sessions.UpdatePlanDtoV1
	orgs     []string
	failPlan bool
	refuse   string
}

func (s *planSink) sendSpec(dto sessions.UpdateSpecDtoV1, orgID string) error {
	s.calls = append(s.calls, "spec:"+dto.Title)
	s.orgs = append(s.orgs, orgID)
	return nil
}

func (s *planSink) sendPlan(dto sessions.UpdatePlanDtoV1, orgID string) (sessions.UpdatePlanReplyV1, error) {
	if s.failPlan {
		return sessions.UpdatePlanReplyV1{}, fmt.Errorf("rpc down")
	}
	s.calls = append(s.calls, "plan:"+dto.Tasks[0].Key)
	s.plans = append(s.plans, dto)
	s.orgs = append(s.orgs, orgID)
	if s.refuse != "" {
		return sessions.UpdatePlanReplyV1{Refused: s.refuse}, nil
	}
	return sessions.UpdatePlanReplyV1{Done: true}, nil
}

// newPlanForwarder wires a forwarder of <root>/.agentbox-output (returned as
// dir) with an injected sink.
func newPlanForwarder(t *testing.T) (*specForwarder, *planSink, *bytes.Buffer, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".agentbox-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sink, logs := &planSink{}, &bytes.Buffer{}
	return &specForwarder{
		path: filepath.Join(dir, "task-spec.json"), planPath: filepath.Join(dir, "task-plan.json"),
		files: newSessionFiles(root, logs), orgID: "org-1", jobID: "job-1", logsWriter: logs,
		sendSpec: sink.sendSpec, sendPlan: sink.sendPlan,
	}, sink, logs, dir
}

const onePlan = `{"tasks":[` +
	`{"key":"api","title":"Add it","goal":"g","context":"c","acceptance_criteria":["a"],"assumptions":["s"],` +
	`"out_of_scope":["o"],"complexity":"medium","readiness":"ready","readiness_notes":"n","repositories":["acme/api"]},` +
	`{"key":"web","title":"Use it","repositories":["acme/web"]}],` +
	`"edges":[{"from":"api","to":"web","when":"merge","reason":"web calls it"}]}`

// writeOnly writes name and removes the other output file, as agentbox does.
func writeOnly(t *testing.T, dir, name, body string) {
	t.Helper()
	other := map[string]string{"task-spec.json": "task-plan.json", "task-plan.json": "task-spec.json"}[name]
	if err := os.Remove(filepath.Join(dir, other)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSpawnEnvSetsMaxPlanTasks(t *testing.T) {
	env, err := buildSessionSpawnEnvVars(implementerJobParameters(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := envMap(env)["AGENTBOX_MAX_PLAN_TASKS"], strconv.Itoa(sessions.MaxPlanTasks); got != want {
		t.Errorf("AGENTBOX_MAX_PLAN_TASKS = %q, want %q", got, want)
	}
}

func TestPlanForwarder_ForwardsOnce(t *testing.T) {
	sf, sink, _, dir := newPlanForwarder(t)
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick()
	sf.tick()
	if len(sink.plans) != 1 || len(sink.calls) != 1 {
		t.Fatalf("calls = %q, want the plan once", sink.calls)
	}
	p := sink.plans[0]
	a := p.Tasks[0]
	if p.JobID != "job-1" || sink.orgs[0] != "org-1" || len(p.Tasks) != 2 ||
		a.Key != "api" || a.Title != "Add it" || a.Goal != "g" || a.Context != "c" || a.Acceptance[0] != "a" ||
		a.Assumptions[0] != "s" || a.OutOfScope[0] != "o" || a.Complexity != "medium" || a.Readiness != "ready" ||
		a.Notes != "n" || a.Repositories[0] != "acme/api" {
		t.Errorf("plan = %+v", p)
	}
	if e := p.Edges[0]; e.From != "api" || e.To != "web" || e.When != "merge" || e.Reason != "web calls it" {
		t.Errorf("edge = %+v", e)
	}
}

func TestPlanForwarder_RefusedLoggedOnceNotResent(t *testing.T) {
	sf, sink, logs, dir := newPlanForwarder(t)
	sink.refuse = "Task api: acme/api is not a repository of the session"
	writeOnly(t, dir, "task-plan.json", onePlan)
	for i := 0; i < 3; i++ {
		sf.tick()
	}
	if len(sink.plans) != 1 {
		t.Errorf("plan sent %d times, want once", len(sink.plans))
	}
	if want := "session: plan refused: " + sink.refuse + "\n"; logs.String() != want {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
}

func TestPlanForwarder_RPCErrorRetried(t *testing.T) {
	sf, sink, logs, dir := newPlanForwarder(t)
	sink.failPlan = true
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick()
	sf.tick()
	if len(sink.plans) != 0 || strings.Count(logs.String(), "session: error forwarding plan: rpc down") != 2 {
		t.Fatalf("plans = %d, logs = %q", len(sink.plans), logs.String())
	}
	sink.failPlan = false
	sf.tick()
	sf.tick()
	if len(sink.plans) != 1 {
		t.Errorf("plan sent %d times after recovery, want once", len(sink.plans))
	}
}

// spec, then a plan, then the same spec again: the spec is forwarded twice,
// since the plan replaced it in between.
func TestPlanForwarder_SpecPlanSpec(t *testing.T) {
	sf, sink, _, dir := newPlanForwarder(t)
	spec := `{"title":"T","goal":"G"}`
	writeOnly(t, dir, "task-spec.json", spec)
	sf.tick()
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick()
	writeOnly(t, dir, "task-spec.json", spec)
	sf.tick()
	sf.tick()
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick()
	want := []string{"spec:T", "plan:api", "spec:T", "plan:api"}
	if strings.Join(sink.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %q, want %q", sink.calls, want)
	}
}

// The tick after the session ended forwards a plan written just before it.
func TestPlanForwarder_FinalTick(t *testing.T) {
	sf, sink, _, dir := newPlanForwarder(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); runSessionTicker(stop, sf.tick) }()
	close(stop)
	<-done
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick() // the final tick, as runSession does after the container exits
	if len(sink.plans) != 1 {
		t.Errorf("plans = %d, want the final tick to forward one", len(sink.plans))
	}
	logs := &bytes.Buffer{}
	logSessionOutcome(newSessionFiles(filepath.Dir(dir), logs), logs)
	if !strings.Contains(logs.String(), "session produced a task plan\n") {
		t.Errorf("logs = %q, want the plan reported", logs.String())
	}
}

// A spec, a refused plan, the same spec, then the same plan (say after the
// missing repository was added): each switch is forwarded.
func TestPlanForwarder_RefusedPlanThenSpecThenPlanAgain(t *testing.T) {
	sf, sink, _, dir := newPlanForwarder(t)
	const spec = `{"title":"one"}`
	writeOnly(t, dir, "task-spec.json", spec)
	sf.tick()
	sink.refuse = "Task api: acme/api is not a repository of the session"
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick()
	sink.refuse = ""
	writeOnly(t, dir, "task-spec.json", spec)
	sf.tick()
	writeOnly(t, dir, "task-plan.json", onePlan)
	sf.tick()
	if got, want := strings.Join(sink.calls, ","), "spec:one,plan:api,spec:one,plan:api"; got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
}
