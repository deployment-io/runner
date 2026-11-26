package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/jobs/commands/dependencies"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/go-git/go-git/v5"
)

// RunDependencyBump is the only command of a dependency bump Job: it bumps one
// Task repository (the dependent) to libraries the Task merged, verifies the
// result offline, pushes it onto the Task branch, opens the pull request when
// there is none, and reports every target's result
// (Jobs.UpdateTaskDependencyBumpV1).
//
// It never names an ecosystem: everything ecosystem-specific is behind
// dependencies.Lookup and the dependencies.Ecosystem interface. Every git
// operation that needs the installation token runs here, in the runner
// process, with go-git and http.BasicAuth; the toolchain containers get no
// credential. The org's private modules resolve from bare mirrors this command
// fetches, through a container-only git url.insteadOf.
//
// It isn't a Task Step (common.go never calls MarkStepDone for it): its work
// directory is its own, under reclaim.DependencyBumpWorkRoot, and is removed
// on every return.
type RunDependencyBump struct {
	// Tests only; nil (or "") is the real thing.
	workRoot   string
	getPR      func(organizationID, installationID, repoName, headBranch string) (oauth.GetOpenPullRequestForBranchDtoV1, error)
	openPR     func(organizationID string, args oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error)
	sendReport func(organizationID string, dto tasks.UpdateTaskDependencyBumpDtoV1) error
	token      func(ctx context.Context, installationID, organizationID, rejectedToken string) (string, error)
	docker     *dependencyBumpDocker
	sleep      func(d time.Duration)
}

const (
	// dependencyBumpMaxMirrors bounds the private repositories one bump
	// mirrors.
	dependencyBumpMaxMirrors = 20
	// dependencyBumpReportAttempts and dependencyBumpReportRetryWait are how
	// the report is sent: as the production watch's final report.
	dependencyBumpReportAttempts  = 3
	dependencyBumpReportRetryWait = 10 * time.Second
	// dependencyBumpOutputLines is how many of a container's last output
	// lines a failure carries.
	dependencyBumpOutputLines = 20
	// dependencyBumpShortCommit is how many characters of a commit id
	// messages show.
	dependencyBumpShortCommit = 12
)

// dependencyCommitRe is a full commit id: SHA-1 or SHA-256, lower-case hex.
var dependencyCommitRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func (rd *RunDependencyBump) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	in, err := parseDependencyBumpInputs(parameters)
	if err != nil {
		// Nothing can be reported for such a request: deployment-server would
		// reject the report. The claim goes stale and is released.
		io.WriteString(logsWriter, fmt.Sprintf("Dependency bump: the Job's inputs don't parse (%s); nothing is reported\n", err))
		return parameters, fmt.Errorf("the dependency bump's inputs don't parse: %w", err)
	}
	b := newDependencyBump(rd, in, logsWriter)
	b.run()
	return parameters, b.finish()
}

// dependencyBumpInputs are the Job's parameters.
type dependencyBumpInputs struct {
	orgID, taskID, branch, jobID string
	// taskTitle and dashboardURL are optional: they only fill the commit's
	// trailer.
	taskTitle, dashboardURL string
	repos                   []tasks.RepositoryEntry
	request                 tasks.DependencyBumpRequestV1
}

// parseDependencyBumpInputs reads and checks the Job's parameters. An error
// means no report could ever be stored for the request.
func parseDependencyBumpInputs(parameters map[string]interface{}) (dependencyBumpInputs, error) {
	var in dependencyBumpInputs
	var reposJSON, requestJSON string
	for _, p := range []struct {
		key  parameters_enums.Key
		dst  *string
		name string
	}{
		{parameters_enums.OrganizationIDNamespace, &in.orgID, "organization id"},
		{parameters_enums.TaskID, &in.taskID, "task id"},
		{parameters_enums.TaskBranchName, &in.branch, "task branch name"},
		{parameters_enums.Repositories, &reposJSON, "repositories"},
		{parameters_enums.DependencyBumpRequest, &requestJSON, "dependency bump request"},
		{parameters_enums.JobID, &in.jobID, "job id"},
	} {
		v, err := jobs.GetParameterValue[string](parameters, p.key)
		if err != nil || v == "" {
			return in, fmt.Errorf("the %s is missing", p.name)
		}
		*p.dst = v
	}
	in.taskTitle, _ = jobs.GetParameterValue[string](parameters, parameters_enums.TaskTitle)
	in.dashboardURL, _ = jobs.GetParameterValue[string](parameters, parameters_enums.DashboardURL)
	if err := json.Unmarshal([]byte(reposJSON), &in.repos); err != nil {
		return in, errors.New("the repositories aren't valid JSON")
	}
	if err := json.Unmarshal([]byte(requestJSON), &in.request); err != nil {
		return in, errors.New("the dependency bump request isn't valid JSON")
	}
	return in, in.check()
}

// check is the request's rules: those deployment-server's report checks
// imply.
func (in dependencyBumpInputs) check() error {
	r := in.request
	switch {
	case len(r.Dependencies) == 0:
		return errors.New("the request has no dependencies")
	case len(r.Dependencies) > tasks.MaxDependencyBumpsPerJob:
		return fmt.Errorf("the request has more than %d dependencies", tasks.MaxDependencyBumpsPerJob)
	case r.Repository < 0 || r.Repository >= len(in.repos):
		return fmt.Errorf("repository %d isn't one of the Task's %d repositories", r.Repository, len(in.repos))
	}
	for i, d := range r.Dependencies {
		switch {
		case d.Source < 0 || d.Source >= len(in.repos):
			return fmt.Errorf("dependency %d's source %d isn't one of the Task's %d repositories", i, d.Source, len(in.repos))
		case d.Source == r.Repository:
			return fmt.Errorf("dependency %d's source is the repository being bumped", i)
		case !tasks.ValidDependencyEcosystem(d.Ecosystem):
			return fmt.Errorf("dependency %d's ecosystem isn't a valid ecosystem name", i)
		case d.Name == "":
			return fmt.Errorf("dependency %d has no name", i)
		}
	}
	return nil
}

// bumpTarget is one dependency of the request and what became of it so far.
type bumpTarget struct {
	tasks.DependencyBumpTargetV1
	ecosystem dependencies.Ecosystem
	result    tasks.DependencyBumpResultDtoV1
}

// open reports whether the target is still in play: not yet resolved, or
// bumped but not yet pushed.
func (t *bumpTarget) open() bool {
	return t.result.State == "" || t.result.State == tasks.DependencyBumpStateBumped
}

// orgScope is the host and owner of the dependent's clone URL: the
// repositories the mirrors and the git rule cover.
type orgScope struct {
	host, owner string
}

func (s orgScope) contains(ref dependencies.RepositoryRef) bool {
	return strings.EqualFold(ref.Host, s.host) && strings.EqualFold(ref.Owner, s.owner)
}

// prefix is "<host>/<owner>": the module-name prefix the mirrors serve.
func (s orgScope) prefix() string { return s.host + "/" + s.owner }

// cloneURLRef is a clone URL's host, owner and repository name
// (https://github.com/acme/kit.git → github.com, acme, kit).
func cloneURLRef(cloneURL string) (dependencies.RepositoryRef, bool) {
	u, err := url.Parse(cloneURL)
	if err != nil || u.Host == "" {
		return dependencies.RepositoryRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" {
		return dependencies.RepositoryRef{}, false
	}
	name := strings.TrimSuffix(parts[len(parts)-1], ".git")
	if name == "" {
		return dependencies.RepositoryRef{}, false
	}
	return dependencies.RepositoryRef{Host: strings.ToLower(u.Host), Owner: parts[0], Name: name}, true
}

// dependencyBump is one run of the command.
type dependencyBump struct {
	cmd     *RunDependencyBump
	in      dependencyBumpInputs
	log     io.Writer
	entry   tasks.RepositoryEntry
	targets []*bumpTarget
	scope   orgScope
	workDir string
	volume  string
	// volumeCreated: the cache volume exists and must be removed.
	volumeCreated bool
	// token is the dependent's installation token; tokens every token used,
	// scrubbed from every message.
	token  string
	tokens []string

	// What one attempt (steps 2 to 10) found; reset when it starts over.
	attemptState dependencyBumpAttempt
}

// dependencyBumpAttempt is what one attempt from step 2 found and did.
type dependencyBumpAttempt struct {
	repository *git.Repository
	worktree   *git.Worktree
	// prOpen: the dependent's pull request was found open (in step 2, or just
	// before the push).
	prOpen bool
	before map[string]dependencies.Manifest
	// ran are the ecosystems whose bump container ran.
	ran       []dependencies.Ecosystem
	mirrors   map[string]*bumpMirror
	mirrorOrd []string
	// sources is each target's library mirror, as fetchMirrors fetched it.
	sources   map[*bumpTarget]*bumpMirror
	commitSHA string
	subject   string
	prURL     string
	prNumber  int
}

func newDependencyBump(rd *RunDependencyBump, in dependencyBumpInputs, logsWriter io.Writer) *dependencyBump {
	root := rd.workRoot
	if root == "" {
		root = reclaim.DependencyBumpWorkRoot
	}
	b := &dependencyBump{
		cmd:     rd,
		in:      in,
		log:     logsWriter,
		entry:   in.repos[in.request.Repository],
		workDir: filepath.Join(root, "dependency-bump-"+in.jobID),
		volume:  "dependency-bump-cache-" + in.jobID,
	}
	for _, d := range in.request.Dependencies {
		b.targets = append(b.targets, &bumpTarget{
			DependencyBumpTargetV1: d,
			result:                 tasks.DependencyBumpResultDtoV1{Ecosystem: d.Ecosystem, Name: d.Name},
		})
	}
	return b
}

func (b *dependencyBump) logf(format string, args ...interface{}) {
	io.WriteString(b.log, "Dependency bump: "+b.scrub(fmt.Sprintf(format, args...))+"\n")
}

// repoDir is where the dependent is checked out, mounted at /src.
func (b *dependencyBump) repoDir() string { return filepath.Join(b.workDir, "repo") }

// run is steps 1 to 10. Whatever happens, every target ends with a result.
func (b *dependencyBump) run() {
	b.checkTargets()
	if !b.anyOpen() {
		return
	}
	defer os.RemoveAll(b.workDir)
	defer b.removeVolume()
	initial := b.snapshot()
	for attempt := 1; ; attempt++ {
		if err := b.resetWorkDir(); err != nil {
			b.failOpen(fmt.Sprintf("couldn't make the work directory: %s", err))
			return
		}
		b.attemptState = dependencyBumpAttempt{before: map[string]dependencies.Manifest{}, mirrors: map[string]*bumpMirror{}}
		if !b.attempt() {
			return
		}
		if attempt == 2 {
			b.logf("%s's branch %s moved again while bumping", b.entry.Name, b.in.branch)
			b.failOpen("the branch moved while bumping")
			return
		}
		b.logf("%s's branch %s moved while bumping; starting over", b.entry.Name, b.in.branch)
		b.restore(initial)
	}
}

// attempt is steps 2 to 10, with the pull request looked up again before
// the push; moved is true when the push found the Task
// branch moved, and the bump starts over.
func (b *dependencyBump) attempt() (moved bool) {
	steps := []func() bool{b.checkout, b.readBefore, b.fetchMirrors, b.skipContained, b.runBumps, b.checkChanges, b.readAfter, b.runVerify, b.recheckPR}
	for _, step := range steps {
		if !step() || !b.anyOpen() {
			return false
		}
	}
	pushed, moved := b.commitAndPush()
	if pushed && !b.attemptState.prOpen {
		b.openPullRequest()
	}
	return moved
}

// snapshot is every target's result, which restore puts back.
func (b *dependencyBump) snapshot() []tasks.DependencyBumpResultDtoV1 {
	results := make([]tasks.DependencyBumpResultDtoV1, len(b.targets))
	for i, t := range b.targets {
		results[i] = t.result
	}
	return results
}

func (b *dependencyBump) restore(results []tasks.DependencyBumpResultDtoV1) {
	for i, t := range b.targets {
		t.result = results[i]
	}
}

// resetWorkDir makes an empty work directory holding mirrors/ and sources/
// (repo/ is the clone's).
func (b *dependencyBump) resetWorkDir() error {
	if err := os.RemoveAll(b.workDir); err != nil {
		return err
	}
	for _, dir := range []string{"mirrors", "sources"} {
		if err := os.MkdirAll(filepath.Join(b.workDir, dir), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (b *dependencyBump) anyOpen() bool {
	for _, t := range b.targets {
		if t.open() {
			return true
		}
	}
	return false
}

// fail resolves t as failed with message.
func (b *dependencyBump) fail(t *bumpTarget, message string) {
	if message == "" {
		message = "the bump failed"
	}
	t.result.State = tasks.DependencyBumpStateFailed
	t.result.ToVersion = ""
	t.result.Error = truncateUTF8(b.scrub(message), tasks.DependencyErrorMaxBytes)
}

// failOpen fails every target still in play with message.
func (b *dependencyBump) failOpen(message string) {
	for _, t := range b.targets {
		if t.open() {
			b.fail(t, message)
		}
	}
}

// truncateUTF8 bounds s to max bytes, on a UTF-8 boundary.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// scrub removes every token the bump used from s.
func (b *dependencyBump) scrub(s string) string {
	for _, token := range b.tokens {
		if token != "" {
			s = strings.ReplaceAll(s, token, "***")
		}
	}
	return s
}

// dependencyBumpGroup is the open targets of one ecosystem.
type dependencyBumpGroup struct {
	ecosystem dependencies.Ecosystem
	targets   []*bumpTarget
}

// groups is the open targets by ecosystem, in the order each ecosystem first
// appears.
func (b *dependencyBump) groups() []dependencyBumpGroup {
	var groups []dependencyBumpGroup
	index := map[string]int{}
	for _, t := range b.targets {
		if !t.open() {
			continue
		}
		name := t.ecosystem.Name()
		i, ok := index[name]
		if !ok {
			i = len(groups)
			index[name] = i
			groups = append(groups, dependencyBumpGroup{ecosystem: t.ecosystem})
		}
		groups[i].targets = append(groups[i].targets, t)
	}
	return groups
}

// checkTargets is step 1: a target this runner can't bump fails on its own;
// the rest go on.
func (b *dependencyBump) checkTargets() {
	dependent, dependentOK := cloneURLRef(b.entry.CloneURL)
	if dependentOK {
		b.scope = orgScope{host: dependent.Host, owner: dependent.Owner}
	}
	for _, t := range b.targets {
		eco, ok := dependencies.Lookup(t.Ecosystem)
		if !ok {
			b.fail(t, fmt.Sprintf("this runner doesn't support the %s ecosystem", t.Ecosystem))
			continue
		}
		t.ecosystem = eco
		if !dependencyCommitRe.MatchString(t.Commit) {
			b.fail(t, "the merged commit isn't a commit id")
			continue
		}
		source := b.in.repos[t.Source]
		ref, ok := cloneURLRef(source.CloneURL)
		if !dependentOK || !ok || !b.scope.contains(ref) {
			b.fail(t, fmt.Sprintf("%s isn't under the same owner as %s; bumps across owners aren't supported yet", source.Name, b.entry.Name))
		}
	}
	for _, t := range b.targets {
		if !t.open() {
			b.logf("%s: %s", t.Name, t.result.Error)
		}
	}
}

// readBefore is step 3: what the dependent pins now. A target it doesn't
// require is not_required.
func (b *dependencyBump) readBefore() bool {
	for _, g := range b.groups() {
		m, ok, err := g.ecosystem.ReadManifest(b.repoDir())
		if err != nil {
			for _, t := range g.targets {
				b.fail(t, fmt.Sprintf("couldn't read %s's manifest: %s", b.entry.Name, err))
			}
			continue
		}
		if !ok {
			m = dependencies.Manifest{}
		}
		b.attemptState.before[g.ecosystem.Name()] = m
		for _, t := range g.targets {
			version, required := m.Requires[t.Name]
			if !required {
				b.logf("%s doesn't require %s", b.entry.Name, g.ecosystem.Describe(t.Name))
				t.result.State = tasks.DependencyBumpStateNotRequired
				continue
			}
			t.result.FromVersion = version
			b.logf("%s requires %s at %s", b.entry.Name, g.ecosystem.Describe(t.Name), version)
		}
	}
	return true
}

// readAfter is step 7: what the dependent pins after the bump. It goes on only
// when a target is bumped.
func (b *dependencyBump) readAfter() bool {
	bumped := false
	for _, g := range b.groups() {
		m, ok, err := g.ecosystem.ReadManifest(b.repoDir())
		if err != nil {
			for _, t := range g.targets {
				b.fail(t, fmt.Sprintf("couldn't read %s's manifest after the bump: %s", b.entry.Name, err))
			}
			continue
		}
		if !ok {
			m = dependencies.Manifest{}
		}
		for _, t := range g.targets {
			version, required := m.Requires[t.Name]
			switch {
			case !required:
				b.logf("%s no longer requires %s after the bump", b.entry.Name, g.ecosystem.Describe(t.Name))
				t.result.State = tasks.DependencyBumpStateNotRequired
			case version == t.result.FromVersion:
				b.logf("%s already pins %s at %s", b.entry.Name, g.ecosystem.Describe(t.Name), version)
				t.result.State = tasks.DependencyBumpStateAlreadyCurrent
				t.result.ToVersion = version
			case version == "":
				b.fail(t, fmt.Sprintf("the bump left %s without a version", g.ecosystem.Describe(t.Name)))
			default:
				b.logf("%s: %s %s → %s", b.entry.Name, g.ecosystem.Describe(t.Name), t.result.FromVersion, version)
				t.result.State = tasks.DependencyBumpStateBumped
				t.result.ToVersion = version
				bumped = true
			}
		}
	}
	if !bumped {
		b.logf("nothing to push for %s", b.entry.Name)
	}
	return bumped
}

// bumped is the targets bumped so far, in request order.
func (b *dependencyBump) bumped() []*bumpTarget {
	var out []*bumpTarget
	for _, t := range b.targets {
		if t.result.State == tasks.DependencyBumpStateBumped {
			out = append(out, t)
		}
	}
	return out
}

// lookupPR, openPullRequestRPC, sendReportRPC and sleepFor are the command's
// seams, or the real calls.
func (b *dependencyBump) lookupPR() func(string, string, string, string) (oauth.GetOpenPullRequestForBranchDtoV1, error) {
	if b.cmd.getPR != nil {
		return b.cmd.getPR
	}
	return client.Get().GetOpenPullRequestForBranch
}

func (b *dependencyBump) openPullRequestRPC() func(string, oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error) {
	if b.cmd.openPR != nil {
		return b.cmd.openPR
	}
	return client.Get().OpenPullRequest
}

func (b *dependencyBump) sendReportRPC() func(string, tasks.UpdateTaskDependencyBumpDtoV1) error {
	if b.cmd.sendReport != nil {
		return b.cmd.sendReport
	}
	return client.Get().UpdateTaskDependencyBump
}

func (b *dependencyBump) sleepFor(d time.Duration) {
	if b.cmd.sleep != nil {
		b.cmd.sleep(d)
		return
	}
	time.Sleep(d)
}

// openPullRequest is step 10, when step 2 found no pull request: it opens it.
// A failure leaves the push in place; the PR tracker finds the branch's pull
// request later if one appears.
func (b *dependencyBump) openPullRequest() {
	var body strings.Builder
	for _, t := range b.targets {
		fmt.Fprintf(&body, "- %s\n", b.prBodyLine(t))
	}
	body.WriteString("\nOpened by deployment.io after the library merged.\n")
	reply, err := b.openPullRequestRPC()(b.in.orgID, oauth.OpenPullRequestArgsV1{
		InstallationID: b.entry.InstallationID,
		RepoName:       b.entry.Name,
		BaseBranch:     b.entry.BaseBranch,
		HeadBranch:     b.in.branch,
		Title:          b.attemptState.subject,
		Body:           body.String(),
		Draft:          false,
	})
	if err != nil {
		b.logf("couldn't open %s's pull request (%s); the push stays on %s", b.entry.Name, err, b.in.branch)
		return
	}
	if reply.AlreadyExisted {
		b.logf("%s's pull request #%d already existed", b.entry.Name, reply.Number)
	} else {
		b.logf("opened %s's pull request #%d", b.entry.Name, reply.Number)
	}
	if strings.HasPrefix(reply.URL, "https://") && reply.Number > 0 {
		b.attemptState.prURL = reply.URL
		b.attemptState.prNumber = reply.Number
	}
}

// prBodyLine is t's line in the pull request's body: its versions, or why it
// wasn't bumped.
func (b *dependencyBump) prBodyLine(t *bumpTarget) string {
	describe := t.Name
	if t.ecosystem != nil {
		describe = t.ecosystem.Describe(t.Name)
	}
	source := b.in.repos[t.Source].Name
	switch t.result.State {
	case tasks.DependencyBumpStateBumped, tasks.DependencyBumpStateAlreadyCurrent:
		return fmt.Sprintf("%s: %s → %s, because %s merged", describe, t.result.FromVersion, t.result.ToVersion, source)
	case tasks.DependencyBumpStateNotRequired:
		return fmt.Sprintf("%s: not required, though %s merged", describe, source)
	default:
		return fmt.Sprintf("%s: not bumped (%s), though %s merged", describe, t.result.Error, source)
	}
}

// finish is step 11: it reports every target's result, and is the command's
// error when any target failed or the report couldn't be sent.
func (b *dependencyBump) finish() error {
	dto := tasks.UpdateTaskDependencyBumpDtoV1{
		TaskID:     b.in.taskID,
		JobID:      b.in.jobID,
		Repository: b.in.request.Repository,
		PRURL:      b.attemptState.prURL,
		PRNumber:   b.attemptState.prNumber,
	}
	if len(b.bumped()) > 0 {
		dto.CommitSHA = b.attemptState.commitSHA
	}
	failed := 0
	for _, t := range b.targets {
		dto.Results = append(dto.Results, t.result)
		if t.result.State == tasks.DependencyBumpStateFailed {
			failed++
		}
		b.logf("%s %s: %s", t.Ecosystem, t.Name, strings.ReplaceAll(t.result.State, "_", " "))
	}
	var sendErr error
	for attempt := 1; attempt <= dependencyBumpReportAttempts; attempt++ {
		if sendErr = b.sendReportRPC()(b.in.orgID, dto); sendErr == nil {
			break
		}
		if attempt < dependencyBumpReportAttempts {
			b.sleepFor(dependencyBumpReportRetryWait)
		}
	}
	if sendErr != nil {
		b.logf("couldn't send the report: %s", sendErr)
		return fmt.Errorf("couldn't send the dependency bump's report: %w", sendErr)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d dependencies couldn't be bumped", failed, len(b.targets))
	}
	return nil
}
