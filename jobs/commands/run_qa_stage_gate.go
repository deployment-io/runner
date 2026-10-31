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
	"syscall"

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
	// stack is the stack path: static and backends are every repository of the
	// Task with a site or a backend preview, unchanged ones included.
	stack bool
	// dropped is, in the stack path, every backend left out because it uses a
	// Go module of another changed repository: the round reports it as a
	// backend preview that couldn't be planned, and deploys nothing for it.
	dropped []droppedBackend
}

// gate decides whether QA runs, before anything is spawned. It classifies
// every CHANGED repository: a static site the runner can build a preview of,
// a backend the runner deploys as a web-service preview itself, or both. A
// backend that uses the Go module of another changed repository is left out
// (dropBackendsUsingChangedRepositories): its preview couldn't build the
// change. It returns:
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
	stack := s.stackPath()
	candidates := changed
	if stack {
		// The whole stack, so a new frontend is never checked against the old
		// backend, or the other way round.
		candidates = make([]string, 0, len(repos))
		for _, repo := range repos {
			candidates = append(candidates, repo.Dir)
		}
	}
	buildable, lookupErr := s.buildableRepositories(candidates)
	backends := s.backendRepositories(candidates)
	backends, dropped := s.dropBackendsUsingChangedRepositories(backends, changed)
	if len(buildable) == 0 && len(backends) == 0 {
		if lookupErr != "" {
			return qaGate{status: tasks.QAStatusCouldNotRun, reason: lookupErr}
		}
		if len(dropped) > 0 {
			return qaGate{status: tasks.QAStatusSkipped, reason: fmt.Sprintf(qaBackendUsesChangedRepositoryReason, dropped[0].backendDir, dropped[0].dependencyDir, dropped[0].dependencyDir)}
		}
		return qaGate{status: tasks.QAStatusSkipped, reason: qaNoStaticSiteOrBackendReason}
	}
	if stack {
		s.logUnchangedInStack(changed, buildable, backends)
	}
	g := qaGate{static: buildable, backends: backends, stack: stack}
	if stack {
		g.dropped = dropped
	}
	if s.qaFailure != "" {
		g.status, g.reason = tasks.QAStatusCouldNotRun, s.qaFailure
	}
	return g
}

// stackPath reports whether the agentbox image reads QA_PREVIEWS, so the runner
// deploys the change's whole stack itself (minPreviewsQAAgentboxVersion).
func (s *qaStage) stackPath() bool {
	imageRef, _ := jobs.GetParameterValue[string](s.parameters, parameters_enums.AgentboxImage)
	return imageVersionAtLeast(imageRef, minPreviewsQAAgentboxVersion)
}

// logUnchangedInStack logs each repository of the stack that didn't change.
func (s *qaStage) logUnchangedInStack(changed, static, backends []string) {
	seen := map[string]bool{}
	for _, dir := range changed {
		seen[dir] = true
	}
	for _, dir := range qaStackRepositories(static, backends) {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: deploying %s too: unchanged, but part of this change's stack\n", dir))
	}
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

// droppedBackend is a backend left out because it uses a Go module from
// dependencyDir, a repository this Task changes.
type droppedBackend struct {
	backendDir, dependencyDir, modulePath string
}

// previewError is the dropped backend's QA_PREVIEWS error.
func (d droppedBackend) previewError() string {
	return fmt.Sprintf("left out: it uses the Go module %s from %s, which this Task also changes, and a preview builds against the version in its go.mod", d.modulePath, d.dependencyDir)
}

// dropBackendsUsingChangedRepositories leaves out every backend whose go.mod
// requires the Go module of another CHANGED repository of the Task: its
// preview builds against the version its go.mod pins, so it would fail to
// build or not show the change. An unchanged repository's released version is
// what the build uses anyway, so depending on one is fine. It returns the
// backends kept and those left out, in work-dir order.
func (s *qaStage) dropBackendsUsingChangedRepositories(backends, changed []string) ([]string, []droppedBackend) {
	var kept []string
	var dropped []droppedBackend
	for _, dir := range backends {
		dependencyDir, modulePath, found := s.crossRepoGoDependency(dir, changed)
		if !found {
			kept = append(kept, dir)
			continue
		}
		io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: leaving %s's backend preview out — it uses the Go module %s from %s, which this Task also changes, and a preview builds against the version in its go.mod; QA can check it once that change is released\n", dir, modulePath, dependencyDir))
		dropped = append(dropped, droppedBackend{backendDir: dir, dependencyDir: dependencyDir, modulePath: modulePath})
	}
	return kept, dropped
}

// crossRepoGoDependency reports the first changed repository (other than
// backendDir) whose Go module backendDir's go.mod requires, with that module's
// path. Only go.mod files at the repositories' roots are read. A backend
// without a go.mod isn't a Go module and depends on nothing here; a changed
// repository without a go.mod or a module line is skipped. A read error other
// than "not found" is logged and never leaves the backend out.
func (s *qaStage) crossRepoGoDependency(backendDir string, changed []string) (dependencyDir, modulePath string, found bool) {
	_, required, ok := s.readGoMod(backendDir)
	if !ok || len(required) == 0 {
		return "", "", false
	}
	for _, dir := range changed {
		if dir == backendDir {
			continue
		}
		module, _, ok := s.readGoMod(dir)
		if !ok || module == "" {
			continue
		}
		if required[module] {
			return dir, module, true
		}
	}
	return "", "", false
}

// readGoMod reads dir's go.mod under the work dir: its module path and every
// module path it requires. ok is false when there is no go.mod or it can't be
// read (logged).
func (s *qaStage) readGoMod(dir string) (module string, required map[string]bool, ok bool) {
	read := s.readFile
	if read == nil {
		read = readGoModFile
	}
	data, err := read(filepath.Join(s.workDirHost, dir, "go.mod"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			io.WriteString(s.logsWriter, fmt.Sprintf("QA stage: could not read %s's go.mod (%s) — not checking it for modules this Task changes\n", dir, err))
		}
		return "", nil, false
	}
	module, required = parseGoMod(data)
	return module, required, true
}

// goModMaxBytes bounds a go.mod read host-side.
const goModMaxBytes = 4 << 20

// readGoModFile reads path when it is a regular file of at most goModMaxBytes
// (readBoundedRegularFile).
func readGoModFile(path string) ([]byte, error) {
	return readBoundedRegularFile(path, goModMaxBytes)
}

// readBoundedRegularFile reads path when it is a regular file of at most max bytes:
// never through a symlink, never a FIFO or device (opened non-blocking, so a
// FIFO can't hang it). A missing file is fs.ErrNotExist; anything else that
// isn't such a file is another error.
func readBoundedRegularFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Re-checked on the opened file: the path may have been swapped since Lstat.
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return data, nil
}

// parseGoMod is a small go.mod reader: the module path (module path or
// module ( path )) and the paths of every require, single-line or in a
// require ( … ) block, // indirect included. Every other directive's block is
// skipped whole.
func parseGoMod(data []byte) (module string, required map[string]bool) {
	required = map[string]bool{}
	// inOther is inside another directive's block (replace, exclude,
	// retract, …): its lines are never read as directives.
	inRequire, inModule, inOther := false, false, false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		// Module paths hold no parentheses: spacing them out makes
		// "require(" and "require ( )" ordinary fields.
		line = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(line)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if inModule {
			// module ( path ): the block holds the one module path.
			if fields[0] == ")" {
				inModule = false
			} else if module == "" {
				module = goModPath(fields[0])
			}
			continue
		}
		if inOther {
			if fields[0] == ")" {
				inOther = false
			}
			continue
		}
		if inRequire {
			if fields[0] == ")" {
				inRequire = false
				continue
			}
			required[goModPath(fields[0])] = true
			continue
		}
		switch fields[0] {
		case "module":
			switch {
			case len(fields) >= 3 && fields[1] == "(" && fields[2] != ")":
				// module ( path ) on one line.
				module = goModPath(fields[2])
				inModule = len(fields) < 4 || fields[3] != ")"
			case len(fields) >= 2 && fields[1] == "(":
				inModule = len(fields) < 3
			case len(fields) >= 2:
				module = goModPath(fields[1])
			}
		case "require":
			switch {
			case len(fields) >= 3 && fields[1] == "(" && fields[2] == ")":
				// An empty block on one line: require ( ).
			case len(fields) >= 2 && fields[1] == "(":
				inRequire = true
			case len(fields) >= 2:
				required[goModPath(fields[1])] = true
			}
		default:
			// Another directive's block, unless it closes on the same line.
			inOther = len(fields) >= 2 && fields[1] == "(" && fields[len(fields)-1] != ")"
		}
	}
	return module, required
}

// goModPath unquotes a quoted go.mod path.
func goModPath(field string) string {
	if unquoted, err := strconv.Unquote(field); err == nil {
		return unquoted
	}
	return field
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
