package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/agenttools"
)

// gitEmptyTree is git's empty tree: the base a repository with no recorded
// start commit (a new, empty repository) is compared against.
const gitEmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// qaGate is the gate's decision: the static sites and backends QA checks, and
// the status ("skipped" or "couldnt_run") and why when it spawns nothing.
type qaGate struct {
	static, backends []string
	status, reason   string
}

// gate decides whether QA runs, before anything is spawned. It classifies
// every CHANGED repository: a static site the runner can build a preview of,
// a backend the runner deploys as a web-service preview itself, or both. It
// returns:
//
//	{static, backends, "", ""}       run QA on these repositories;
//	{nil, nil, "", ""}               no repository changed — no QA run, no result;
//	{static, backends, status, why}  record status with why, and spawn nothing.
func (s *qaStage) gate() qaGate {
	repos := readQARepositoriesFromJobOutput(s.parameters)
	if !anyBaseCommit(repos) {
		return qaGate{status: tasks.QAStatusCouldNotRun, reason: "no start-of-run commit was recorded"}
	}
	changed := s.changedRepositories(repos)
	if len(changed) == 0 {
		return qaGate{}
	}
	buildable, lookupErr := s.buildableRepositories(changed)
	backends := s.backendRepositories(changed)
	if len(buildable) == 0 && len(backends) == 0 {
		if lookupErr != "" {
			return qaGate{status: tasks.QAStatusCouldNotRun, reason: lookupErr}
		}
		return qaGate{status: tasks.QAStatusSkipped, reason: qaNoStaticSiteOrBackendReason}
	}
	g := qaGate{static: buildable, backends: backends}
	if s.qaFailure != "" {
		g.status, g.reason = tasks.QAStatusCouldNotRun, s.qaFailure
	}
	return g
}

// changedRepositories is every repository directory whose working tree differs
// from its start-of-run commit, in work-dir order.
//
// The comparison is the WORKING TREE against the base commit, not against
// HEAD, so a commit the agent made itself counts, and so do untracked files.
// It runs host-side with the fix trees' throwaway GIT_DIR: the repository's
// own git config is never read. A repository that cannot be compared counts
// as changed — QA is the safer answer than silently skipping it.
func (s *qaStage) changedRepositories(repos []baseCommitOutput) []string {
	var changed []string
	for _, repo := range repos {
		same, err := s.treeMatchesBase(repo.Dir, repo.CommitSHA)
		if err != nil {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: could not tell whether %s changed (%s) — treating it as changed\n", repo.Dir, err))
			changed = append(changed, repo.Dir)
			continue
		}
		if !same {
			changed = append(changed, repo.Dir)
		}
	}
	return changed
}

// treeMatchesBase reports whether dir's working tree is exactly its base
// commit's tree (the empty tree when no base was recorded).
func (s *qaStage) treeMatchesBase(dir, base string) (bool, error) {
	t := &fixTrees{workDirHost: s.workDirHost, logsWriter: io.Discard, git: s.fixTreesGit}
	if t.git == "" {
		t.git = "git"
	}
	root, err := os.MkdirTemp(s.fixTreesTmpRoot, "qa-changed-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(root)
	tree, err := t.prepare(dir, root)
	if err != nil {
		return false, err
	}
	baseTree := gitEmptyTree
	if base != "" {
		out, err := t.run(dir, root, nil, "rev-parse", "--verify", "--quiet", base+"^{tree}")
		if err != nil {
			return false, fmt.Errorf("the start-of-run commit %s cannot be read: %s", base, err)
		}
		baseTree = strings.TrimSpace(out)
	}
	return tree == baseTree, nil
}

// buildableRepositories keeps the changed repositories the runner can build a
// preview of — deploy_static_site_preview's own decision, made without
// building anything. A repository whose build settings could not be looked up
// is left out and logged; the first such error is returned so a gate that left
// out EVERY repository for that reason says it could not run rather than that
// there was nothing to check.
func (s *qaStage) buildableRepositories(changed []string) ([]string, string) {
	check := s.checkPreview
	if check == nil {
		deps := buildStaticSitePreviewDeps(s.ctx, s.parameters, s.workDirHost, s.logsWriter)
		check = func(repository string) agenttools.RunnerBuiltPreviewCheck {
			return agenttools.CheckRunnerBuiltPreview(*deps, repository)
		}
	}
	var buildable []string
	firstLookupErr := ""
	lookupErrs := 0
	for _, dir := range changed {
		result := check(dir)
		switch {
		case result.LookupErr != nil:
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: leaving %s out — %s\n", dir, result.LookupErr))
			lookupErrs++
			if firstLookupErr == "" {
				firstLookupErr = result.LookupErr.Error()
			}
		case result.OK:
			buildable = append(buildable, dir)
		default:
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: %s has no preview the runner can build (%s)\n", dir, result.Reason))
		}
	}
	if lookupErrs < len(changed) {
		firstLookupErr = ""
	}
	return buildable, firstLookupErr
}

// backendRepositories keeps the changed repositories that have a backend
// preview — deploy_web_service_preview's own decision without root_directory
// (one managed web service built from a Dockerfile in the runner's region),
// made without deploying anything. A refused plan is logged with its reason.
// With an agentbox image that predates backend QA, every backend is left out
// (logged) and QA proceeds with the static sites alone, as before.
func (s *qaStage) backendRepositories(changed []string) []string {
	check := s.checkBackend
	if check == nil {
		deps := buildWebServicePreviewDeps(s.ctx, s.parameters, s.workDirHost, s.logsWriter, nil)
		check = func(repository string) error {
			return agenttools.CheckWebServicePreview(*deps, repository)
		}
	}
	var backends []string
	for _, dir := range changed {
		if err := check(dir); err != nil {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: %s has no backend preview (%s)\n", dir, err))
			continue
		}
		backends = append(backends, dir)
	}
	if len(backends) == 0 {
		return nil
	}
	imageRef, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.AgentboxImage)
	if !imageVersionAtLeast(imageRef, minBackendQAAgentboxVersion) {
		for _, dir := range backends {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: leaving %s's backend out — the agentbox image predates backend QA (needs %s or later)\n", dir, minBackendQAAgentboxVersion))
		}
		return nil
	}
	return backends
}

// readQARepositoriesFromJobOutput is every checked-out repository recorded in
// the Job output's base_commits, in work-dir order (Task.Repositories' order),
// with its start commit when one was recorded.
func readQARepositoriesFromJobOutput(parameters map[string]interface{}) []baseCommitOutput {
	existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput)
	if err != nil || len(existing) == 0 {
		return nil
	}
	var data jobOutputData
	if err := json.Unmarshal([]byte(existing), &data); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []baseCommitOutput
	for _, bc := range data.BaseCommits {
		if bc.Dir == "" || seen[bc.Dir] || !filepath.IsLocal(bc.Dir) {
			continue
		}
		seen[bc.Dir] = true
		out = append(out, bc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func anyBaseCommit(repos []baseCommitOutput) bool {
	for _, r := range repos {
		if r.CommitSHA != "" {
			return true
		}
	}
	return false
}
