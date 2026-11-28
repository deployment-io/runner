package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/jobs/commands/dependencies"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// The start-up bump: when a Task's Step waits for upstream Tasks that merged
// a library its repositories require (the StartUpBumpSources parameter),
// CheckoutRepository bumps each repository to the merged commits right after
// its checkout, before the agent runs, as the first commit on the Task
// branch. It is the dependency bump Job's bump (dependencyBump) from
// readBefore to runVerify, on a local clone of the Step's checkout, with no
// pull request, push or report: CommitAndPush pushes the commit with the
// agent's. Nothing that goes wrong in it stops the Step, except moving the
// Step's checkout to the bump commit.

// startUpBumpOutput is one target in the JobOutput's start_up_bumps block.
type startUpBumpOutput struct {
	Repository       int    `json:"repository"`
	Ecosystem        string `json:"ecosystem"`
	Name             string `json:"name"`
	SourceTaskID     string `json:"sourceTaskId"`
	SourceRepository string `json:"sourceRepository"`
	Commit           string `json:"commit"`
	State            string `json:"state"`
	FromVersion      string `json:"fromVersion,omitempty"`
	ToVersion        string `json:"toVersion,omitempty"`
	CommitSHA        string `json:"commitSHA,omitempty"`
	Error            string `json:"error,omitempty"`
	BuildError       string `json:"buildError,omitempty"`
}

// startUpBumps is the Step's start-up bumps: the sources and every target's
// result so far.
type startUpBumps struct {
	cmd     *RunDependencyBump
	ctx     commandUtils.TaskJobContext
	jobID   string
	sources []tasks.StartUpBumpSourceV1
	log     io.Writer
	results []startUpBumpOutput
	// lines are the prompt's lines, in the order the targets ran.
	lines []string
}

// startUpBumpPromptHeader opens the block appended to the StepPrompt.
const startUpBumpPromptHeader = "[Library bumps before this run]"

// newStartUpBumps reads the StartUpBumpSources parameter; nil when it is
// absent, empty or unreadable, and then the checkout is what it always was.
func (cr *CheckoutRepository) newStartUpBumps(parameters map[string]interface{}, ctx commandUtils.TaskJobContext, jobID string, logsWriter io.Writer) *startUpBumps {
	raw, err := jobs.GetParameterValue[string](parameters, parameters_enums.StartUpBumpSources)
	if err != nil || raw == "" {
		return nil
	}
	var sources []tasks.StartUpBumpSourceV1
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		io.WriteString(logsWriter, "Dependency bump: the start-up bump sources aren't valid JSON; nothing is bumped\n")
		return nil
	}
	if len(sources) == 0 {
		return nil
	}
	if len(sources) > tasks.MaxStartUpBumpSources {
		io.WriteString(logsWriter, fmt.Sprintf("Dependency bump: %d start-up bump sources; only the first %d are bumped\n", len(sources), tasks.MaxStartUpBumpSources))
		sources = sources[:tasks.MaxStartUpBumpSources]
	}
	if jobID == "" {
		jobID = ctx.TaskID + "-step-" + strconv.FormatInt(ctx.StepIndex, 10)
	}
	return &startUpBumps{
		cmd:     &RunDependencyBump{workRoot: cr.bumpWorkRoot, token: cr.bumpToken, docker: cr.bumpDocker},
		ctx:     ctx,
		jobID:   jobID,
		sources: sources,
		log:     logsWriter,
	}
}

// bump bumps the Task's repository idx, checked out at repoDir, to every
// source it requires. The error is only that the Step's checkout couldn't be
// moved to the bump commit, or chowned after it.
func (su *startUpBumps) bump(idx int, repoDir string) error {
	if su == nil {
		return nil
	}
	targets, pins := su.targets(idx, repoDir)
	if len(targets) == 0 {
		return nil
	}
	b := newDependencyBump(su.cmd, su.inputs(idx, targets), su.log)
	b.stepDir = repoDir
	moveErr := b.runStartUp()
	su.record(b, pins)
	if moveErr != nil {
		return moveErr
	}
	return chownTaskCheckout(repoDir)
}

// targets is every source's Provides that the repository at repoDir requires,
// with Source indexing the sources after the Task's repositories, at most
// tasks.MaxDependencyBumpsPerJob, and the version each is pinned at now. A
// Provides of an ecosystem this runner doesn't support is a target when a
// manifest requires its name, so that it fails as A-2's does.
func (su *startUpBumps) targets(idx int, repoDir string) ([]tasks.DependencyBumpTargetV1, []string) {
	name := su.ctx.Entries[idx].Name
	required := su.requiredNames(name, repoDir)
	var targets []tasks.DependencyBumpTargetV1
	var pins []string
	for s, source := range su.sources {
		for _, p := range source.Provides {
			eco, known := dependencies.Lookup(p.Ecosystem)
			pin, ok := requires(required, p, known)
			if !ok {
				continue
			}
			if len(targets) == tasks.MaxDependencyBumpsPerJob {
				io.WriteString(su.log, fmt.Sprintf("Dependency bump: %s requires more than %d of the merged libraries; not bumping %s\n", name, tasks.MaxDependencyBumpsPerJob, p.Name))
				continue
			}
			t := tasks.DependencyBumpTargetV1{Ecosystem: p.Ecosystem, Name: p.Name, Description: p.Name, Source: len(su.ctx.Entries) + s, Commit: source.Commit, Release: tasks.DependencyReleaseCommit}
			if known {
				t.Description, t.Release = eco.Describe(p.Name), eco.Release()
			}
			targets = append(targets, t)
			pins = append(pins, pin)
		}
	}
	return targets, pins
}

// requires reports whether the repository's required names (ecosystem →
// name → pinned version) include p, and its pin: in p's ecosystem, or in any
// when p's isn't known.
func requires(required map[string]map[string]string, p tasks.StartUpBumpProvidedV1, known bool) (string, bool) {
	if known {
		pin, ok := required[p.Ecosystem][p.Name]
		return pin, ok
	}
	for _, names := range required {
		if pin, ok := names[p.Name]; ok {
			return pin, true
		}
	}
	return "", false
}

// requiredNames is every name the repository at repoDir requires and its
// pinned version, by ecosystem, in every registered ecosystem whose manifest
// it has.
func (su *startUpBumps) requiredNames(name, repoDir string) map[string]map[string]string {
	required := map[string]map[string]string{}
	for _, eco := range dependencies.Registered() {
		m, ok, err := eco.ReadManifest(repoDir)
		if err != nil {
			io.WriteString(su.log, fmt.Sprintf("Dependency bump: couldn't read %s's %s manifest; not bumping it in that ecosystem\n", name, eco.Name()))
			continue
		}
		if !ok {
			continue
		}
		required[eco.Name()] = m.Requires
	}
	return required
}

// inputs is the bump of repository idx: the dependent is its entry, and the
// sources' entries follow the Task's. The job id carries the index, so two
// repositories of one Step never share a work directory or a cache volume.
func (su *startUpBumps) inputs(idx int, targets []tasks.DependencyBumpTargetV1) dependencyBumpInputs {
	repos := append([]tasks.RepositoryEntry{}, su.ctx.Entries...)
	for _, s := range su.sources {
		repos = append(repos, s.Repository)
	}
	return dependencyBumpInputs{
		orgID:        su.ctx.OrganizationID,
		taskID:       su.ctx.TaskID,
		branch:       su.ctx.BranchName,
		jobID:        su.jobID + "-" + strconv.Itoa(idx),
		taskTitle:    su.ctx.TaskTitle,
		dashboardURL: su.ctx.DashboardURL,
		repos:        repos,
		request:      tasks.DependencyBumpRequestV1{Repository: idx, Dependencies: targets},
	}
}

// record keeps every target of b: its start_up_bumps entry and, when it was
// bumped or failed, its prompt line. pins are the targets' versions as the
// manifest read them, for a target failed before the bump read its own.
func (su *startUpBumps) record(b *dependencyBump, pins []string) {
	b.settleSuperseded()
	for i, t := range b.targets {
		source := su.sources[t.Source-len(su.ctx.Entries)]
		if t.result.FromVersion == "" && i < len(pins) {
			t.result.FromVersion = pins[i]
		}
		out := startUpBumpOutput{
			Repository:       b.in.request.Repository,
			Ecosystem:        t.Ecosystem,
			Name:             t.Name,
			SourceTaskID:     source.TaskID,
			SourceRepository: source.Repository.Name,
			Commit:           t.Commit,
			State:            t.result.State,
			FromVersion:      t.result.FromVersion,
			ToVersion:        t.result.ToVersion,
			Error:            t.result.Error,
			BuildError:       t.buildError,
		}
		if t.result.State == tasks.DependencyBumpStateBumped {
			out.CommitSHA = b.attemptState.commitSHA
		}
		b.logf("%s %s: %s", t.Ecosystem, t.Name, strings.ReplaceAll(t.result.State, "_", " "))
		su.results = append(su.results, out)
		if line := startUpBumpPromptLine(b.entry.Name, t, source); line != "" {
			su.lines = append(su.lines, line)
		}
	}
}

// startUpBumpPromptLine is t's line in the prompt's block, or "" when the
// agent needn't hear of it (already current, not required).
func startUpBumpPromptLine(repo string, t *bumpTarget, source tasks.StartUpBumpSourceV1) string {
	describe := t.Name
	if t.ecosystem != nil {
		describe = t.ecosystem.Describe(t.Name)
	}
	short := t.Commit
	if len(short) > dependencyBumpShortCommit {
		short = short[:dependencyBumpShortCommit]
	}
	switch t.result.State {
	case tasks.DependencyBumpStateBumped:
		line := fmt.Sprintf("- %s: bumped %s from %s to %s, the merged commit %s of %s from the Task %q", repo, describe, t.result.FromVersion, t.result.ToVersion, short, source.Repository.Name, source.TaskTitle)
		if t.buildError != "" {
			return line + fmt.Sprintf(", but %s doesn't build after it:\n%s", repo, indentLines(t.buildError, "    "))
		}
		return line + ". It is already committed on the branch."
	case tasks.DependencyBumpStateFailed:
		reason := strings.ReplaceAll(strings.TrimRight(t.result.Error, ".\n"), "\n", "\n    ")
		line := fmt.Sprintf("- %s: couldn't bump %s to %s's merged commit %s: %s.", repo, describe, source.Repository.Name, short, reason)
		if t.result.FromVersion != "" {
			line += fmt.Sprintf(" It still pins %s.", t.result.FromVersion)
		}
		return line
	}
	return ""
}

// startUpBumpPromptMaxBytes bounds the block appended to the StepPrompt,
// which reaches agentbox as one environment string: every target can carry
// a build's last lines, and Linux refuses a string over 128 KiB.
const startUpBumpPromptMaxBytes = 32 << 10

// promptLineMarker joins a shortened prompt line's first line and its end.
const promptLineMarker = "\n    [...]\n"

// boundPromptLines shortens the longest of lines, keeping each one's first
// line and its end, until they fit in max bytes with their newlines: each
// line gets an even share of what the shorter ones leave.
func boundPromptLines(lines []string, max int) []string {
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	if total <= max {
		return lines
	}
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(lines[order[a]]) < len(lines[order[b]]) })
	bounded := append([]string{}, lines...)
	remaining := max
	for n, i := range order {
		share := remaining/(len(order)-n) - 1
		if len(bounded[i]) > share {
			// The first line is the target's sentence: it is kept whole when
			// it fits, and the build's last lines get what it leaves.
			bounded[i] = truncateHeadTail(bounded[i], share, share-len(promptLineMarker), promptLineMarker)
		}
		remaining -= len(bounded[i]) + 1
	}
	return bounded
}

// indentLines is s with every line indented by indent.
func indentLines(s, indent string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}

// finish writes the start_up_bumps block onto the JobOutput envelope and,
// when a target was bumped or failed, appends the block to the StepPrompt.
func (su *startUpBumps) finish(parameters map[string]interface{}) {
	if su == nil || len(su.results) == 0 {
		return
	}
	if err := mergeStartUpBumpsIntoJobOutput(parameters, su.results); err != nil {
		io.WriteString(su.log, fmt.Sprintf("warning: could not record the start-up bumps: %s\n", err))
	}
	if len(su.lines) == 0 {
		return
	}
	prompt, _ := jobs.GetParameterValue[string](parameters, parameters_enums.StepPrompt)
	if len(prompt) > 0 {
		prompt += "\n\n"
	}
	block := startUpBumpPromptHeader + "\n" + strings.Join(boundPromptLines(su.lines, startUpBumpPromptMaxBytes-len(startUpBumpPromptHeader)-1), "\n") + "\n"
	jobs.SetParameterValue[string](parameters, parameters_enums.StepPrompt, prompt+block)
}

// mergeStartUpBumpsIntoJobOutput writes the start_up_bumps block onto the
// accumulated envelope, as mergeBaseCommitsIntoJobOutput writes base_commits.
func mergeStartUpBumpsIntoJobOutput(parameters map[string]interface{}, results []startUpBumpOutput) error {
	data := jobOutputData{}
	if existing, err := jobs.GetParameterValue[string](parameters, parameters_enums.JobOutput); err == nil && len(existing) > 0 {
		_ = json.Unmarshal([]byte(existing), &data)
	}
	data.SchemaVersion = jobOutputSchemaVersion
	data.StartUpBumps = results
	merged, err := json.Marshal(data)
	if err != nil {
		return err
	}
	jobs.SetParameterValue[string](parameters, parameters_enums.JobOutput, string(merged))
	return nil
}

// runStartUp is the start-up bump: step 1, a local clone of the Step's
// checkout instead of steps 2's, steps 3 to 8, and, when a target is bumped,
// the commit, which moves the Step's checkout. The error is only that the
// Step's checkout couldn't be moved; every other failure is a target's.
func (b *dependencyBump) runStartUp() error {
	b.checkTargets()
	if !b.anyOpen() {
		return nil
	}
	defer os.RemoveAll(b.workDir)
	defer b.removeVolume()
	if err := b.resetWorkDir(); err != nil {
		b.failOpen(fmt.Sprintf("couldn't make the work directory: %s", err))
		return nil
	}
	b.attemptState = dependencyBumpAttempt{before: map[string]dependencies.Manifest{}, mirrors: map[string]*bumpMirror{}}
	steps := []func() bool{b.cloneStepCheckout, b.readBefore, b.fetchMirrors, b.resolveDuplicates, b.skipContained, b.runBumps, b.checkChanges, b.readAfter, b.checkSkipped, b.runVerify}
	for _, step := range steps {
		if !step() || !b.anyOpen() {
			return nil
		}
	}
	if len(b.bumped()) == 0 {
		return nil
	}
	_, message := b.commitMessage()
	hash, err := b.commit(message)
	if err != nil {
		b.failOpen(fmt.Sprintf("couldn't commit the bump: %s", err))
		return nil
	}
	if err := b.moveStepCheckout(hash); err != nil {
		b.logf("couldn't move %s's checkout to the bump commit %s", b.entry.Name, hash[:7])
		b.failOpen("couldn't move the Step's checkout to the bump commit")
		return fmt.Errorf("couldn't move the checkout to the dependency bump commit: %s", err)
	}
	b.attemptState.commitSHA = hash
	b.logf("committed %s onto %s for %s", hash[:7], b.in.branch, b.entry.Name)
	return nil
}

// resolveDuplicates runs after fetchMirrors: when two upstream Tasks merged
// the same library, the repository can pin only one of their commits. The
// target whose merged commit contains every other's is bumped, whatever the
// sources' order, and the others take its result (settleSuperseded). When no
// commit contains the others (they diverged, or the check fails), the first
// target is bumped and the others fail.
func (b *dependencyBump) resolveDuplicates() bool {
	groups := map[string][]*bumpTarget{}
	var keys []string
	for _, t := range b.targets {
		if !t.open() {
			continue
		}
		key := t.Ecosystem + "\x00" + t.Name
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], t)
	}
	for _, key := range keys {
		group := groups[key]
		if len(group) < 2 {
			continue
		}
		winner := group[0]
		for _, t := range group[1:] {
			if b.containsMerged(t, winner) {
				winner = t
			}
		}
		for _, t := range group {
			if t == winner {
				continue
			}
			if b.containsMerged(winner, t) {
				if b.superseded == nil {
					b.superseded = map[*bumpTarget]*bumpTarget{}
				}
				b.superseded[t] = winner
				// Out of play until settleSuperseded gives it winner's result.
				t.result.State = tasks.DependencyBumpStateAlreadyCurrent
				b.logf("%s: %s's merged commit %s contains %s's merged commit %s", t.Name, b.in.repos[winner.Source].Name, winner.Commit[:dependencyBumpShortCommit], b.in.repos[t.Source].Name, t.Commit[:dependencyBumpShortCommit])
				continue
			}
			b.fail(t, fmt.Sprintf("%s is bumped to %s's merged commit instead", t.ecosystem.Describe(t.Name), b.in.repos[winner.Source].Name))
			b.logf("%s: %s", t.Name, t.result.Error)
		}
	}
	return true
}

// containsMerged reports whether a's merged commit is b's or a descendant of
// it, in their library's mirror; false when they don't share one or the
// check fails.
func (b *dependencyBump) containsMerged(a, c *bumpTarget) bool {
	m := b.attemptState.sources[a]
	if m == nil || m != b.attemptState.sources[c] {
		return false
	}
	repository, err := git.PlainOpen(b.mirrorDir(m.dir))
	if err != nil {
		return false
	}
	descendant, err := repository.CommitObject(plumbing.NewHash(a.Commit))
	if err != nil {
		return false
	}
	ancestor, err := repository.CommitObject(plumbing.NewHash(c.Commit))
	if err != nil {
		return false
	}
	contained, err := ancestor.IsAncestor(descendant)
	return err == nil && contained
}

// settleSuperseded gives every superseded target the result of the target
// that bumps its library: already current when that one was bumped or
// already current (its pin contains this merged commit), failed with it
// otherwise.
func (b *dependencyBump) settleSuperseded() {
	for _, t := range b.targets {
		w, ok := b.superseded[t]
		if !ok {
			continue
		}
		switch w.result.State {
		case tasks.DependencyBumpStateBumped, tasks.DependencyBumpStateAlreadyCurrent:
			t.result.State = tasks.DependencyBumpStateAlreadyCurrent
			t.result.ToVersion = w.result.ToVersion
			if t.result.FromVersion == "" {
				t.result.FromVersion = w.result.FromVersion
			}
		default:
			message := w.result.Error
			if message == "" {
				message = fmt.Sprintf("%s wasn't bumped", t.ecosystem.Describe(t.Name))
			}
			b.fail(t, message)
		}
	}
}

// cloneStepCheckout is the start-up mode's step 2: a local clone of the
// Step's checkout at its Task branch into repo/ (no network, no token).
func (b *dependencyBump) cloneStepCheckout() bool {
	branch := plumbing.NewBranchReferenceName(b.in.branch)
	repository, err := git.PlainClone(b.repoDir(), false, &git.CloneOptions{URL: b.stepDir, ReferenceName: branch, SingleBranch: true, Tags: git.NoTags})
	if err != nil {
		b.logf("couldn't clone %s's checkout: %s", b.entry.Name, err)
		b.failOpen(fmt.Sprintf("couldn't clone %s's checkout: %s", b.entry.Name, err))
		return false
	}
	if err := b.cloneStepSubmodules(repository); err != nil {
		b.logf("couldn't check out %s's submodules in its clone: %s", b.entry.Name, err)
		b.failOpen(fmt.Sprintf("couldn't check out %s's submodules in its clone: %s", b.entry.Name, err))
		return false
	}
	b.attemptState.repository = repository
	return true
}

// cloneStepSubmodules checks out the clone's submodules as the Step's
// checkout has them, with no network: the Step's submodule repositories
// (.git/modules, nested ones within) are copied into the clone, and each
// submodule is checked out at its gitlink from there.
func (b *dependencyBump) cloneStepSubmodules(repository *git.Repository) error {
	worktree, err := repository.Worktree()
	if err != nil {
		return err
	}
	submodules, err := worktree.Submodules()
	if err != nil || len(submodules) == 0 {
		return err
	}
	modules := filepath.Join(b.stepDir, git.GitDirName, "modules")
	if _, err := os.Stat(modules); err != nil {
		return errors.New("the Step's checkout has none checked out")
	}
	clone := filepath.Join(b.repoDir(), git.GitDirName, "modules")
	if err := copyTree(modules, clone); err != nil {
		return err
	}
	// Each index describes the Step's submodule worktree, which the clone's
	// doesn't have yet: without it, the checkout writes the files.
	err = filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "index" {
			return err
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "HEAD")); err != nil {
			return nil
		}
		return os.Remove(path)
	})
	if err != nil {
		return err
	}
	return submodules.Update(&git.SubmoduleUpdateOptions{Init: true, NoFetch: true, RecurseSubmodules: git.DefaultSubmoduleRecursionDepth})
}

// moveStepCheckout copies the bump commit from the clone into the Step's
// checkout and hard-resets its Task branch to it. The copy is in-process
// (copyBumpCommit), not a fetch: a fetch over go-git's in-process file
// transport (local_git_transport.go) fails with "object not found" when the
// checkout has commits the clone doesn't, and the host's git must not run on
// the checkout.
func (b *dependencyBump) moveStepCheckout(hash string) error {
	repository, err := git.PlainOpen(b.stepDir)
	if err != nil {
		return err
	}
	branch := plumbing.NewBranchReferenceName(b.in.branch)
	head, err := repository.Head()
	if err != nil || head.Name() != branch {
		return errors.New("the checkout isn't on the Task branch")
	}
	clone, err := git.PlainOpen(b.repoDir())
	if err != nil {
		return err
	}
	if err := copyBumpCommit(clone.Storer, repository.Storer, plumbing.NewHash(hash), head.Hash()); err != nil {
		return err
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return err
	}
	if err := worktree.Reset(&git.ResetOptions{Commit: plumbing.NewHash(hash), Mode: git.HardReset}); err != nil {
		return err
	}
	if tip, err := repository.Reference(branch, true); err != nil || tip.Hash().String() != hash {
		return errors.New("the Task branch isn't at the bump commit after the reset")
	}
	return nil
}
