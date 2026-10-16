package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
)

// RunProductionQA is the only command of a production QA Job: once a Deployed
// Task's deliveries have all ended, ONE report-only agentbox QA run checks the
// Task's acceptance criteria against the deployed repositories' production
// URLs.
//
// It is blind like the QA stage — no checkout, no diff, no context, no deploy
// and no fix round — and production is real: the run's tool socket serves only
// verify_preview_reachable and verify_preview_rendered, every rendered check is
// read-only, and an image too old to honour that is refused before anything
// starts.
//
// Every outcome is a completed Job whose output's "qa" says what happened —
// app-server reads it into Task.ProductionQA, and deployment-server persists a
// Job's output only when it completes successfully. Run returns an error only
// when it cannot write that output. It has no Step: it never reports a stage
// or a QA result to one.
type RunProductionQA struct {
	stopSignal <-chan struct{}

	// spawn runs the QA container; nil is the real thing. Tests only.
	spawn func(spec agentboxSpawnSpec, logsWriter io.Writer) (agentResult, error)
}

func (rp *RunProductionQA) SetStopSignal(stop <-chan struct{}) { rp.stopSignal = stop }

// productionQATarget is one entry of the ProductionQATargets parameter (kit's
// job_creation_utils.ProductionQATarget).
type productionQATarget struct {
	Repository string   `json:"repository"`
	URL        string   `json:"url"`
	Hosts      []string `json:"hosts,omitempty"`
}

// productionQADirName is appended to the Task's base dir for the run's /work:
// a sibling, never inside it, holding only agentbox's output directories.
const productionQADirName = "-production-qa"

const (
	productionQANoTargetsReason = "no production URL to check"
	productionQAStoppedReason   = "the production check was stopped"
)

func (rp *RunProductionQA) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	out := rp.check(parameters, logsWriter)
	io.WriteString(logsWriter, fmt.Sprintf("Production QA: %s", out.Status))
	if out.StatusReason != "" {
		io.WriteString(logsWriter, " — "+out.StatusReason)
	}
	io.WriteString(logsWriter, "\n")
	if err := mergeQAIntoJobOutput(parameters, out); err != nil {
		return parameters, fmt.Errorf("could not record the production check on the job output: %w", err)
	}
	return parameters, nil
}

// check runs the production check and returns its "qa" record: one round when
// the QA run was started, none when it was refused before.
func (rp *RunProductionQA) check(parameters map[string]interface{}, logsWriter io.Writer) *qaOutput {
	targets, err := parseProductionQATargets(parameters)
	out := &qaOutput{Repositories: productionQARepositories(targets)}
	couldntRun := func(reason string) *qaOutput {
		out.Status, out.StatusReason = tasks.QAStatusCouldNotRun, reason
		return out
	}
	if err != nil {
		return couldntRun(err.Error())
	}
	orgID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	taskID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.TaskID)
	if orgID == "" || taskID == "" {
		return couldntRun("the production check has no organization or task id")
	}
	qaParams, qaFailure := qaParameters(parameters)
	if qaFailure != "" {
		return couldntRun(qaFailure)
	}
	if reason := productionQAImageRefusal(parameters); reason != "" {
		return couldntRun(reason)
	}
	release, reason := rp.holdImage(parameters)
	defer release()
	if reason != "" {
		return couldntRun(reason)
	}
	stage := &qaStage{
		parameters:   parameters,
		workDirHost:  commandUtils.GetTaskRepositoriesBaseDir(orgID, taskID),
		logsWriter:   logsWriter,
		qaParams:     qaParams,
		repositories: out.Repositories,
	}
	io.WriteString(logsWriter, fmt.Sprintf("Production QA: checking the acceptance criteria on %s\n", strings.Join(productionQAURLs(targets), ", ")))
	run := rp.runQA(stage, targets)
	stage.accumulateQARun(1, run.result)
	round := stage.roundOutput(1, run.result, run.err)
	switch {
	case run.aborted != "":
		round.Status, round.StatusReason = tasks.QAStatusCouldNotRun, run.aborted
	case errors.Is(run.err, types.ErrJobStoppedByUser):
		round.Status, round.StatusReason = tasks.QAStatusCouldNotRun, productionQAStoppedReason
	}
	out.Rounds = []qaRoundOutput{round}
	out.Status, out.StatusReason, out.NoCriteria = round.Status, round.StatusReason, round.NoCriteria
	return out
}

// productionQAImageRefusal names why this run can't use its images, or "":
// the verify-browser image must honour read_only and the agentbox image must
// have the production QA mode. An image whose tag isn't a version is refused.
func productionQAImageRefusal(parameters map[string]interface{}) string {
	jobBrowser, _ := jobs.GetParameterValue[string](parameters, parameters_enums.VerifyBrowserImage)
	if ref := verifyBrowserImageRef(jobBrowser); !imageVersionAtLeast(ref, minReadOnlyVerifyBrowserVersion) {
		return fmt.Sprintf("the QA browser image %s can't check production read-only (needs verify-browser %s or later)", ref, minReadOnlyVerifyBrowserVersion)
	}
	agentbox, _ := jobs.GetParameterValue[string](parameters, parameters_enums.AgentboxImage)
	if !imageVersionAtLeast(agentbox, minProductionQAAgentboxVersion) {
		return fmt.Sprintf("the agentbox image %q predates production QA (needs agentbox %s or later)", agentbox, minProductionQAAgentboxVersion)
	}
	return ""
}

// holdImage holds the agentbox image for the run, as the QA stage does, and
// returns its release and, when the image is unavailable, why.
func (rp *RunProductionQA) holdImage(parameters map[string]interface{}) (func(), string) {
	if rp.spawn != nil {
		return func() {}, ""
	}
	imageRef, _ := jobs.GetParameterValue[string](parameters, parameters_enums.AgentboxImage)
	release := reclaim.MarkImageInUse(imageRef)
	if err := ensureAgentboxImage(imageRef); err != nil {
		return release, fmt.Sprintf("error pulling the agentbox image: %s", err)
	}
	return release, ""
}

// productionQARun is how the one QA container ended: what it reported, its
// error, and — when it was stopped because the browser ignored read_only — why.
type productionQARun struct {
	result  agentResult
	err     error
	aborted string
}

// runQA runs the one QA container in a fresh directory.
func (rp *RunProductionQA) runQA(stage *qaStage, targets []productionQATarget) productionQARun {
	imageRef, _ := jobs.GetParameterValue[string](stage.parameters, parameters_enums.AgentboxImage)
	env, err := buildAgentSpawnEnvVars(stage.qaView(), stage.logsWriter)
	if err != nil {
		return productionQARun{err: err}
	}
	spec, _ := jobs.GetParameterValue[string](stage.parameters, parameters_enums.ReviewSpec)
	env = applyProductionQAEnv(env, spec, targets)
	dir := strings.TrimRight(stage.workDirHost, "/") + productionQADirName
	_ = os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return productionQARun{err: fmt.Errorf("error preparing the QA directory: %s", err)}
	}
	if rp.spawn == nil {
		if err := prepareAgentboxHostDirs(dir); err != nil {
			return productionQARun{err: fmt.Errorf("error preparing the QA directory: %s", err)}
		}
	}
	abort := newRunAbort(rp.stopSignal)
	defer abort.release()
	spawnSpec := agentboxSpawnSpec{
		imageRef:      imageRef,
		workDirHost:   dir,
		env:           env,
		mcpSocketHost: agentMCPSocketHostPath(dir),
		waitTimeout:   qaRunTimeout,
		productionTools: &productionQATools{
			record:                productionQARecord(targets),
			verifyBrowser:         verifyBrowserForJob(runnerVerifyBrowser.run, stage.parameters),
			logsWriter:            stage.logsWriter,
			onReadOnlyUnsupported: abort.abort,
		},
	}
	io.WriteString(stage.logsWriter, fmt.Sprintf("Production QA: turn cap %s model responses\n", envValue(env, "MAX_TURNS")))
	var result agentResult
	if rp.spawn != nil {
		result, err = rp.spawn(spawnSpec, stage.logsWriter)
	} else {
		impl := &RunAgentStep{stopSignal: abort.stop}
		result, err = impl.spawnAgentboxAndWait(spawnSpec, stage.logsWriter)
	}
	logRoundOutput(filepath.Join(dir, agentboxResultDirRel), "Production QA", stage.logsWriter)
	return productionQARun{result: result, err: err, aborted: abort.reason()}
}

// applyProductionQAEnv is applyQAEnv's QA environment — blind, with
// QA_REPOSITORIES the targets' repositories — pointed at production:
// QA_TARGET=production and QA_URLS, "<repository>=<url>" per repository.
func applyProductionQAEnv(env []string, spec string, targets []productionQATarget) []string {
	env = applyQAEnv(env, spec, productionQARepositories(targets))
	urls := make([]string, 0, len(targets))
	for _, t := range targets {
		urls = append(urls, t.Repository+"="+t.URL)
	}
	return append(env, "QA_TARGET=production", "QA_URLS="+strings.Join(urls, ","))
}

// productionQARecord is the run's preview record: no lister, seeded with
// exactly the target URLs and https://<host>/ for each target's custom
// domains, so a redirect to a custom domain loads and anything else is
// blocked.
func productionQARecord(targets []productionQATarget) *agenttools.PreviewRecord {
	record := agenttools.NewPreviewRecord(nil, nil)
	for _, t := range targets {
		record.AddURL(t.URL)
		for _, host := range t.Hosts {
			record.AddURL("https://" + host + "/")
		}
	}
	return record
}

// parseProductionQATargets reads and checks the ProductionQATargets parameter:
// at least one target, each naming a repository and an https URL. A custom
// domain must be a bare host name.
func parseProductionQATargets(parameters map[string]interface{}) ([]productionQATarget, error) {
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.ProductionQATargets)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, errors.New(productionQANoTargetsReason)
	}
	var targets []productionQATarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return nil, fmt.Errorf("the production targets are not valid JSON: %s", err)
	}
	if len(targets) == 0 {
		return nil, errors.New(productionQANoTargetsReason)
	}
	for _, t := range targets {
		if strings.TrimSpace(t.Repository) == "" || strings.ContainsAny(t.Repository, ",=") {
			return targets, fmt.Errorf("invalid production target repository %q", t.Repository)
		}
		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || strings.Contains(t.URL, ",") {
			return targets, fmt.Errorf("the production URL %q of %s is not an https URL", t.URL, t.Repository)
		}
		for _, host := range t.Hosts {
			if host == "" || strings.ContainsAny(host, "/:@?# ") {
				return targets, fmt.Errorf("invalid custom domain %q of %s", host, t.Repository)
			}
		}
	}
	return targets, nil
}

func productionQARepositories(targets []productionQATarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Repository)
	}
	return out
}

func productionQAURLs(targets []productionQATarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.URL)
	}
	return out
}

// runAbort stops a run from inside it: stop closes when the outer stop signal
// fires or abort is called, and reason says why abort was called.
type runAbort struct {
	stop    chan struct{}
	aborted chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	why     string
}

func newRunAbort(outer <-chan struct{}) *runAbort {
	a := &runAbort{stop: make(chan struct{}), aborted: make(chan struct{}), done: make(chan struct{})}
	go func() {
		select {
		case <-outer:
		case <-a.aborted:
		case <-a.done:
			return
		}
		close(a.stop)
	}()
	return a
}

func (a *runAbort) abort(reason string) {
	a.once.Do(func() {
		a.mu.Lock()
		a.why = reason
		a.mu.Unlock()
		close(a.aborted)
	})
}

func (a *runAbort) reason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.why
}

func (a *runAbort) release() { close(a.done) }
