package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

const productionHealthTestSpec = `{"Title":"Pricing","Acceptance":["The pricing page lists three plans","The API answers /plans"]}`

// healthTestServer answers every health check, whatever its host, with the
// status statusFor gives its path, and counts the requests per path.
type healthTestServer struct {
	mu    sync.Mutex
	calls map[string]int
}

func (h *healthTestServer) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[path]
}

func startHealthTestServer(t *testing.T, statusFor func(path string) int) *healthTestServer {
	t.Helper()
	h := &healthTestServer{calls: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls[r.URL.Path]++
		h.mu.Unlock()
		status := statusFor(r.URL.Path)
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "/broken")
			if r.URL.Path == "/moved-badly" {
				w.Header().Set("Location", "https://example.com/%zz")
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, strings.Repeat("x", 200<<10))
	}))
	t.Cleanup(srv.Close)
	routeHealthChecksTo(t, srv.Listener.Addr().String())
	return h
}

// routeHealthChecksTo sends every health check to addr, and makes retries
// immediate, for the test.
func routeHealthChecksTo(t *testing.T, addr string) {
	t.Helper()
	transport, wait := productionQAHealthTransport, productionQAHealthRetryWait
	t.Cleanup(func() { productionQAHealthTransport, productionQAHealthRetryWait = transport, wait })
	productionQAHealthRetryWait = time.Millisecond
	productionQAHealthTransport = &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

func TestProductionQAHealthCheck(t *testing.T) {
	statuses := map[string]int{"/ok": 200, "/moved": 302, "/moved-badly": 302, "/bad-request": 400, "/unauthorized": 401, "/missing": 404, "/down": 503, "/broken": 500}
	h := startHealthTestServer(t, func(path string) int { return statuses[path] })
	for _, tc := range []struct {
		path     string
		state    string
		reason   string
		attempts int
	}{
		{"/ok", tasks.QACriterionPassed, "answered 200", 1},
		{"/moved", tasks.QACriterionPassed, "answered 302", 1},
		{"/moved-badly", tasks.QACriterionPassed, "answered 302", 1},
		{"/bad-request", tasks.QACriterionPassed, "answered 400", 1},
		{"/unauthorized", tasks.QACriterionFailed, "answered 401", 3},
		{"/missing", tasks.QACriterionFailed, "answered 404", 3},
		{"/down", tasks.QACriterionFailed, "answered 503", 3},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var logs strings.Builder
			c, stopped := (&RunProductionQA{}).healthCheck(productionQATarget{Repository: "1-acme/api", HealthURL: "http://api.acme.test" + tc.path}, &logs)
			if stopped {
				t.Fatal("stopped")
			}
			want := qaCriterion{Criterion: "1-acme/api: GET " + tc.path + " answers with a status from 200 to 400", State: tc.state, Reason: tc.reason}
			if c != want {
				t.Errorf("criterion = %+v, want %+v", c, want)
			}
			if n := h.count(tc.path); n != tc.attempts {
				t.Errorf("%d attempts, want %d", n, tc.attempts)
			}
			wantLog := "Production QA: health check of 1-acme/api GET http://api.acme.test" + tc.path + " → " + strings.TrimPrefix(tc.reason, "answered ") + "\n"
			if logs.String() != wantLog {
				t.Errorf("logs = %q, want %q", logs.String(), wantLog)
			}
		})
	}
	if n := h.count("/broken"); n != 0 {
		t.Errorf("the redirect was followed %d times", n)
	}
}

func TestProductionQAHealthCheckNoAnswer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	routeHealthChecksTo(t, addr)
	var logs strings.Builder
	c, _ := (&RunProductionQA{}).healthCheck(productionQATarget{Repository: "1-acme/api", HealthURL: "http://api.acme.test/ping"}, &logs)
	if c.State != tasks.QACriterionFailed || c.Reason != "no answer (connection refused)" {
		t.Errorf("criterion = %+v, want failed with no answer (connection refused)", c)
	}
	if strings.Contains(c.Reason, "acme.test") || strings.Contains(c.Reason, addr) {
		t.Errorf("the reason names the address: %q", c.Reason)
	}
	if !strings.HasSuffix(logs.String(), "GET http://api.acme.test/ping → no answer\n") {
		t.Errorf("logs = %q", logs.String())
	}
}

func setProductionTargets(t *testing.T, parameters map[string]interface{}, targets []productionQATarget) {
	t.Helper()
	raw, err := json.Marshal(targets)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.ProductionQATargets, string(raw))
}

// healthOnlyParameters is a production QA Job with only a health-checked
// target and no QA agent it could run: no images, no implementer, and a QA
// model without credentials.
func healthOnlyParameters(t *testing.T, spec string) map[string]interface{} {
	t.Helper()
	parameters := productionQAParameters(t)
	for _, k := range []parameters_enums.Key{parameters_enums.AgentEnvVars, parameters_enums.AgentType, parameters_enums.Model,
		parameters_enums.AgentboxImage, parameters_enums.VerifyBrowserImage} {
		key, err := k.Key()
		if err != nil {
			t.Fatal(err)
		}
		delete(parameters, key)
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.QAAgentType, "codex")
	jobs.SetParameterValue[string](parameters, parameters_enums.QAModel, "gpt-5.5")
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewSpec, spec)
	setProductionTargets(t, parameters, []productionQATarget{{Repository: "1-acme/api", HealthURL: "http://lb-1.elb.amazonaws.com/ping"}})
	return parameters
}

func noAgentRun(t *testing.T) *RunProductionQA {
	return &RunProductionQA{
		spawn: func(agentboxSpawnSpec, io.Writer) (agentResult, error) {
			t.Error("a health-only check spawned the QA agent")
			return agentResult{}, nil
		},
		prefetchBrowser: func(context.Context, string, io.Writer) { t.Error("a health-only check prefetched the browser") },
	}
}

func TestProductionQAHealthOnly(t *testing.T) {
	startHealthTestServer(t, func(string) int { return 200 })
	qa, data := runProductionQAWith(t, noAgentRun(t), healthOnlyParameters(t, productionHealthTestSpec))
	if qa.Status != tasks.QAStatusCompleted || qa.StatusReason != "" || qa.NoCriteria {
		t.Fatalf("qa = %+v, want completed with criteria", qa)
	}
	if len(qa.Rounds) != 1 || qa.Rounds[0].Status != tasks.QAStatusCompleted || qa.Rounds[0].NoCriteria {
		t.Fatalf("rounds = %+v", qa.Rounds)
	}
	notChecked := "Not checked: no https address to open it in a browser — add a custom domain to 1-acme/api's service."
	want := []qaCriterion{
		{Criterion: "1-acme/api: GET /ping answers with a status from 200 to 400", State: tasks.QACriterionPassed, Reason: "answered 200"},
		{Criterion: "The pricing page lists three plans", State: tasks.QACriterionNotChecked, Reason: notChecked},
		{Criterion: "The API answers /plans", State: tasks.QACriterionNotChecked, Reason: notChecked},
	}
	if got := qa.Rounds[0].Criteria; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("criteria = %+v, want %+v", got, want)
	}
	if data.Usage != nil && data.Usage.QA != nil {
		t.Errorf("a health-only check recorded agent usage: %+v", data.Usage)
	}
}

func TestProductionQAHealthOnlyFailedAndNoCriteria(t *testing.T) {
	startHealthTestServer(t, func(string) int { return 503 })
	qa, _ := runProductionQAWith(t, noAgentRun(t), healthOnlyParameters(t, `{"Title":"Pricing"}`))
	if qa.Status != tasks.QAStatusCompleted || !qa.NoCriteria || len(qa.Rounds) != 1 || !qa.Rounds[0].NoCriteria {
		t.Fatalf("qa = %+v, want completed with no criteria", qa)
	}
	got := qa.Rounds[0].Criteria
	if len(got) != 1 || got[0].State != tasks.QACriterionFailed || got[0].Reason != "answered 503" {
		t.Errorf("criteria = %+v, want only the failed health check", got)
	}
}

func TestProductionQAMixedTargets(t *testing.T) {
	startHealthTestServer(t, func(string) int { return 200 })
	parameters := productionQAParameters(t)
	setProductionTargets(t, parameters, []productionQATarget{
		{Repository: "0-acme/site", URL: "https://www.acme.com", Hosts: []string{"d111.cloudfront.net"}},
		{Repository: "1-acme/api", HealthURL: "http://lb-1.elb.amazonaws.com/ping"},
		{Repository: "2-acme/app", URL: "https://app.acme.com", HealthURL: "http://app.acme.com/healthz"}, // the test server speaks http
	})
	var spec agentboxSpawnSpec
	qa, _ := runProductionQA(t, parameters, func(s agentboxSpawnSpec, _ io.Writer) (agentResult, error) {
		spec = s
		return qaCompleted(signUpPassed, pricingPassed), nil
	})
	env := envMap(spec.env)
	if env["QA_URLS"] != "0-acme/site=https://www.acme.com,2-acme/app=https://app.acme.com" {
		t.Errorf("QA_URLS = %q", env["QA_URLS"])
	}
	if env["QA_REPOSITORIES"] != "0-acme/site,2-acme/app" {
		t.Errorf("QA_REPOSITORIES = %q", env["QA_REPOSITORIES"])
	}
	if got := strings.Join(spec.productionTools.record.Hosts(), ","); got != "app.acme.com,d111.cloudfront.net,www.acme.com" {
		t.Errorf("record hosts = %s", got)
	}
	if qa.Status != tasks.QAStatusCompleted || qa.NoCriteria || len(qa.Rounds) != 1 {
		t.Fatalf("qa = %+v", qa)
	}
	got := qa.Rounds[0].Criteria
	want := []qaCriterion{
		signUpPassed, pricingPassed,
		{Criterion: "1-acme/api: GET /ping answers with a status from 200 to 400", State: tasks.QACriterionPassed, Reason: "answered 200"},
		{Criterion: "2-acme/app: GET /healthz answers with a status from 200 to 400", State: tasks.QACriterionPassed, Reason: "answered 200"},
	}
	if len(got) != len(want) {
		t.Fatalf("criteria = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("criterion %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseProductionQATargetsHealthURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target productionQATarget
		ok     bool
	}{
		{"health URL only", productionQATarget{Repository: "1-a/api", HealthURL: "http://lb-1.elb.amazonaws.com/ping"}, true},
		{"https URL and health URL", productionQATarget{Repository: "1-a/api", URL: "https://api.acme.com", HealthURL: "https://api.acme.com/ping"}, true},
		{"the scheme's default port", productionQATarget{Repository: "1-a/api", HealthURL: "http://lb.example.com:80/"}, true},
		{"https on its default port", productionQATarget{Repository: "1-a/api", HealthURL: "https://api.acme.com:443/"}, true},
		{"neither", productionQATarget{Repository: "1-a/api"}, false},
		{"an http URL beside a health URL", productionQATarget{Repository: "1-a/api", URL: "http://lb.example.com", HealthURL: "http://lb.example.com/"}, false},
		{"user info", productionQATarget{Repository: "1-a/api", HealthURL: "http://user:pw@lb.example.com/"}, false},
		{"a non-default port", productionQATarget{Repository: "1-a/api", HealthURL: "http://lb.example.com:8080/"}, false},
		{"https on port 80", productionQATarget{Repository: "1-a/api", HealthURL: "https://lb.example.com:80/"}, false},
		{"an empty port", productionQATarget{Repository: "1-a/api", HealthURL: "http://lb.example.com:/"}, false},
		{"a comma", productionQATarget{Repository: "1-a/api", HealthURL: "http://lb.example.com/a,b"}, false},
		{"another scheme", productionQATarget{Repository: "1-a/api", HealthURL: "ftp://lb.example.com/"}, false},
		{"no host", productionQATarget{Repository: "1-a/api", HealthURL: "http:///ping"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := map[string]interface{}{}
			setProductionTargets(t, parameters, []productionQATarget{tc.target})
			_, err := parseProductionQATargets(parameters)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

// A bad health URL fails the run before anything starts.
func TestProductionQAInvalidHealthURLIsCouldntRun(t *testing.T) {
	parameters := productionQAParameters(t)
	setProductionTargets(t, parameters, []productionQATarget{{Repository: "1-a/api", HealthURL: "http://lb.example.com:8080/"}})
	qa, _ := runProductionQAWith(t, noAgentRun(t), parameters)
	if qa.Status != tasks.QAStatusCouldNotRun || !strings.Contains(qa.StatusReason, "health check URL") || len(qa.Rounds) != 0 {
		t.Errorf("qa = %+v, want couldnt_run naming the health check URL", qa)
	}
}

func TestProductionQAHealthCheckStops(t *testing.T) {
	startHealthTestServer(t, func(string) int { return 200 })
	stop := make(chan struct{})
	close(stop)
	var logs strings.Builder
	criteria, stopped := (&RunProductionQA{stopSignal: stop}).healthChecks([]productionQATarget{
		{Repository: "1-acme/api", HealthURL: "http://api.acme.test/ping"},
		{Repository: "2-acme/web", HealthURL: "http://web.acme.test/ping"},
	}, &logs)
	if !stopped || len(criteria) != 0 {
		t.Errorf("stopped = %v, criteria = %+v; want stopped with none", stopped, criteria)
	}
}

func TestProductionQAHealthCheckErrorNamesNoURL(t *testing.T) {
	_, err := url.Parse("https://example.com/%zz")
	for _, e := range []error{
		&url.Error{Op: "Get", URL: "http://api.acme.test/ping", Err: err},
		errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP status code "see http://evil.test/` + strings.Repeat("x", 500) + `"`),
	} {
		if got := healthCheckError(e); got != "request failed" {
			t.Errorf("healthCheckError(%v) = %q, want request failed", e, got)
		}
	}
}

func TestProductionQAHealthCriteriaAreNeverCut(t *testing.T) {
	// 50 acceptance results, the last one failed, and a passing health check:
	// every result is kept, so the failure is still counted.
	agent := make([]qaCriterion, 50)
	for i := range agent {
		agent[i] = qaCriterion{Criterion: fmt.Sprintf("criterion %d", i), State: tasks.QACriterionPassed}
	}
	agent[49].State = tasks.QACriterionFailed
	health := []qaCriterion{{Criterion: "1-acme/api: GET /ping", State: tasks.QACriterionPassed}}
	got := withHealthCriteria(agent, health)
	if len(got) != 51 || got[49] != agent[49] || got[50] != health[0] {
		t.Errorf("%d criteria, want the 50 acceptance results then the health check", len(got))
	}

	// A round with only health checks keeps every not_checked acceptance
	// criterion after them.
	var acceptance []string
	for i := 0; i < 50; i++ {
		acceptance = append(acceptance, fmt.Sprintf("criterion %d", i))
	}
	parameters := map[string]interface{}{}
	jobs.SetParameterValue[string](parameters, parameters_enums.ReviewSpec, taskSpecJSON(t, acceptance))
	round := healthOnlyRound(parameters, []productionQATarget{{Repository: "1-acme/api"}}, health)
	if len(round.Criteria) != 51 || round.Criteria[0] != health[0] ||
		round.Criteria[50].Criterion != "criterion 49" || round.Criteria[50].State != tasks.QACriterionNotChecked {
		t.Errorf("%d criteria, want the health check then 50 not_checked", len(round.Criteria))
	}
}
