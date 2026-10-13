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

// gate decides whether QA runs, before anything is spawned. It returns:
//
//	(repositories, "", "")       run QA on these repositories;
//	(nil, "", "")                no repository changed — no QA run, no result;
//	(repositories, status, why)  record status ("skipped" or "couldnt_run")
//	                             with why, and spawn nothing.
func (s *qaStage) gate() ([]string, string, string) {
	repos := readQARepositoriesFromJobOutput(s.parameters)
	if !anyBaseCommit(repos) {
		return nil, tasks.QAStatusCouldNotRun, "no start-of-run commit was recorded"
	}
	changed := s.changedRepositories(repos)
	if len(changed) == 0 {
		return nil, "", ""
	}
	buildable, lookupErr := s.buildableRepositories(changed)
	if len(buildable) == 0 {
		if lookupErr != "" {
			return nil, tasks.QAStatusCouldNotRun, lookupErr
		}
		return nil, tasks.QAStatusSkipped, qaNoStaticSiteReason
	}
	if s.qaFailure != "" {
		return buildable, tasks.QAStatusCouldNotRun, s.qaFailure
	}
	return buildable, "", ""
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
