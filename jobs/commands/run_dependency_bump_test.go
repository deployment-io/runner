package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/oauth"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/internal/gittest"
	"github.com/deployment-io/deployment-runner/jobs/commands/dependencies"
	"github.com/deployment-io/deployment-runner/utils/reclaim"
	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// --- a made-up ecosystem ------------------------------------------------------

// fakeEcosystem is an ecosystem the runner doesn't know: its manifest is
// deps.txt, with "provides <name>" and "requires <name> <version>" lines, and
// a name's repository is its first three path elements. A test registers it
// to prove the command needs no ecosystem-specific code.
type fakeEcosystem struct {
	// read is every directory ReadManifest read.
	read []string
}

func (f *fakeEcosystem) Name() string                { return "fake" }
func (f *fakeEcosystem) Release() string             { return tasks.DependencyReleaseCommit }
func (f *fakeEcosystem) Describe(name string) string { return "the fake package " + name }

func (f *fakeEcosystem) ReadManifest(dir string) (dependencies.Manifest, bool, error) {
	f.read = append(f.read, dir)
	data, err := os.ReadFile(filepath.Join(dir, "deps.txt"))
	if errors.Is(err, fs.ErrNotExist) {
		return dependencies.Manifest{}, false, nil
	}
	if err != nil {
		return dependencies.Manifest{}, false, err
	}
	m := dependencies.Manifest{Requires: map[string]string{}}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "provides":
			m.Provides = append(m.Provides, fields[1])
		case len(fields) == 3 && fields[0] == "requires":
			m.Requires[fields[1]] = fields[2]
		}
	}
	return m, true, nil
}

func (f *fakeEcosystem) Source(name string) (dependencies.RepositoryRef, bool) {
	parts := strings.Split(name, "/")
	if len(parts) < 3 {
		return dependencies.RepositoryRef{}, false
	}
	return dependencies.RepositoryRef{Host: parts[0], Owner: parts[1], Name: parts[2]}, true
}

// fakePinnedRe is a fake pin naming a commit, as fake-bump writes it.
var fakePinnedRe = regexp.MustCompile(`^c-([0-9a-f]{12})$`)

func (f *fakeEcosystem) PinnedCommit(_, version string) (string, bool) {
	m := fakePinnedRe.FindStringSubmatch(version)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func (f *fakeEcosystem) Bump(_ string, bumps []dependencies.Bump, private []string) (dependencies.Commands, error) {
	cmd := []string{"fake-bump"}
	for _, b := range bumps {
		cmd = append(cmd, b.Name+"@"+b.Commit)
	}
	return dependencies.Commands{Image: "fake:1", Commands: [][]string{cmd}, Env: []string{"FAKE_PRIVATE=" + strings.Join(private, ",")}, Summary: "fake-bump"}, nil
}

func (f *fakeEcosystem) Verify(_ string, private []string) (dependencies.Commands, error) {
	return dependencies.Commands{Image: "fake:1", Commands: [][]string{{"fake-verify"}}, Env: []string{"FAKE_PRIVATE=" + strings.Join(private, ",")}, Summary: "fake-verify"}, nil
}

func (f *fakeEcosystem) Allowed(path string) bool {
	return path == "deps.txt" || strings.HasPrefix(path, "vendor/")
}

// --- a fake docker ---------------------------------------------------------------

// fakeBumpDocker records the containers it's asked for and runs their
// commands against the host directory mounted at /src: by default a
// fake-bump pins each name@commit at c-<commit[:12]> and a go get at a Go
// pseudo-version; everything else succeeds doing nothing.
type fakeBumpDocker struct {
	configs  []*container.Config
	hosts    []*container.HostConfig
	commands []string
	removed  []string
	created  []string
	dropped  []string
	// run, when set, answers a command instead of the default.
	run func(src string, cmd []string) (int, string)
	// atBump runs once per bump container, before its first command.
	atBump func(host *container.HostConfig)
}

func (f *fakeBumpDocker) docker() *dependencyBumpDocker {
	byID := map[string]*container.HostConfig{}
	return &dependencyBumpDocker{
		containers: &crossRepoDocker{
			pull: func(context.Context, string) error { return nil },
			create: func(_ context.Context, cfg *container.Config, host *container.HostConfig) (string, error) {
				f.configs = append(f.configs, cfg)
				f.hosts = append(f.hosts, host)
				id := fmt.Sprintf("c%d", len(f.configs))
				byID[id] = host
				return id, nil
			},
			start: func(context.Context, string) error { return nil },
			exec: func(_ context.Context, id string, cmd, _ []string, out io.Writer) (int, error) {
				host := byID[id]
				f.commands = append(f.commands, strings.Join(cmd, " "))
				if host.NetworkMode == "bridge" && f.atBump != nil {
					f.atBump(host)
				}
				run := f.run
				if run == nil {
					run = applyFakeBump
				}
				code, output := run(mountSource(host, "/src"), cmd)
				io.WriteString(out, output)
				return code, nil
			},
			remove:  func(id string) { f.removed = append(f.removed, id) },
			timeout: time.Minute,
		},
		createVolume: func(name string) error { f.created = append(f.created, name); return nil },
		removeVolume: func(name string) { f.dropped = append(f.dropped, name) },
	}
}

func mountSource(host *container.HostConfig, target string) string {
	for _, m := range host.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

func findBumpMount(host *container.HostConfig, target string) (mount.Mount, bool) {
	for _, m := range host.Mounts {
		if m.Target == target {
			return m, true
		}
	}
	return mount.Mount{}, false
}

// applyFakeBump is the default container: fake-bump and go get pin each
// name@commit; anything else does nothing.
func applyFakeBump(src string, cmd []string) (int, string) {
	switch {
	case cmd[0] == "fake-bump":
		for _, arg := range cmd[1:] {
			name, commit, _ := strings.Cut(arg, "@")
			rewriteVersion(src, "deps.txt", `(?m)^requires `+regexp.QuoteMeta(name)+` \S+$`, "requires "+name+" c-"+commit[:12])
		}
		return 0, "bumped\n"
	case len(cmd) > 2 && cmd[0] == "go" && cmd[1] == "get":
		for _, arg := range cmd[2:] {
			name, commit, _ := strings.Cut(arg, "@")
			rewriteVersion(src, "go.mod", `(?m)^require `+regexp.QuoteMeta(name)+` \S+$`, "require "+name+" v0.0.0-20261126000000-"+commit[:12])
		}
		return 0, "go: upgraded\n"
	}
	return 0, ""
}

func rewriteVersion(src, file, pattern, replacement string) {
	path := filepath.Join(src, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, regexp.MustCompile(pattern).ReplaceAll(data, []byte(replacement)), 0o644)
}

// --- the harness -----------------------------------------------------------------

const (
	bumpJobID  = "0123456789abcdef01234567"
	bumpBranch = "deployment-io/task-bump"
)

// bumpHarness is a library (acme/kit, index 0), a dependent (acme/app, index
// 1) and a private module the library requires (acme/priv), under one owner
// of an https git host, with the fake ecosystem registered and every RPC
// stubbed.
type bumpHarness struct {
	t         *testing.T
	host      *gittest.Host
	hostName  string
	repos     []tasks.RepositoryEntry
	kitCommit string
	workRoot  string
	eco       *fakeEcosystem
	docker    *fakeBumpDocker
	log       strings.Builder

	pr         oauth.GetOpenPullRequestForBranchDtoV1
	prErr      error
	prCalls    int
	opened     []oauth.OpenPullRequestArgsV1
	openReply  oauth.OpenPullRequestDtoV1
	openErr    error
	reports    []tasks.UpdateTaskDependencyBumpDtoV1
	reportErrs int
	tokens     []string
	rejected   []string
	slept      []time.Duration
}

func newBumpHarness(t *testing.T) *bumpHarness {
	t.Helper()
	h := &bumpHarness{t: t, host: gittest.NewHost(t), eco: &fakeEcosystem{}, docker: &fakeBumpDocker{}, workRoot: t.TempDir()}
	h.hostName = strings.TrimPrefix(h.host.URL, "https://")
	restore := dependencies.SetRegistryForTest(append(dependencies.Registered(), h.eco))
	t.Cleanup(restore)
	for _, name := range []string{"acme/kit", "acme/app", "acme/priv"} {
		h.host.AddRepo(name)
	}
	h.kitCommit = h.host.Commit("acme/kit", "main", map[string]string{"deps.txt": "provides " + h.name("kit") + "\nrequires " + h.name("priv") + " v1.0.0\n"})
	h.host.Git("acme/kit", "tag", "v1.0.0", h.kitCommit)
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "provides " + h.name("app") + "\nrequires " + h.name("kit") + " v0.9.0\nrequires example.com/pub v1\n"})
	h.repos = []tasks.RepositoryEntry{h.entry("kit"), h.entry("app")}
	h.openReply = oauth.OpenPullRequestDtoV1{URL: "https://github.com/acme/app/pull/7", Number: 7}
	return h
}

// name is the fake ecosystem's name of the acme repository repo.
func (h *bumpHarness) name(repo string) string { return h.hostName + "/acme/" + repo }

func (h *bumpHarness) entry(repo string) tasks.RepositoryEntry {
	return tasks.RepositoryEntry{Name: "acme/" + repo, CloneURL: h.host.URL + "/acme/" + repo, BaseBranch: "main", Provider: "github", InstallationID: "inst-1"}
}

// kitTarget is the bump of the library to its merged commit.
func (h *bumpHarness) kitTarget() tasks.DependencyBumpTargetV1 {
	return tasks.DependencyBumpTargetV1{Ecosystem: "fake", Name: h.name("kit"), Description: "the fake package " + h.name("kit"), Source: 0, Commit: h.kitCommit, Release: tasks.DependencyReleaseCommit}
}

func (h *bumpHarness) command() *RunDependencyBump {
	return &RunDependencyBump{
		workRoot: h.workRoot,
		getPR: func(org, installationID, repoName, branch string) (oauth.GetOpenPullRequestForBranchDtoV1, error) {
			h.prCalls++
			if org != "org-1" || installationID != "inst-1" || repoName != "acme/app" || branch != bumpBranch {
				h.t.Errorf("GetOpenPullRequestForBranch(%q, %q, %q, %q)", org, installationID, repoName, branch)
			}
			return h.pr, h.prErr
		},
		openPR: func(org string, args oauth.OpenPullRequestArgsV1) (oauth.OpenPullRequestDtoV1, error) {
			h.opened = append(h.opened, args)
			return h.openReply, h.openErr
		},
		sendReport: func(org string, dto tasks.UpdateTaskDependencyBumpDtoV1) error {
			h.reports = append(h.reports, dto)
			if len(h.reports) <= h.reportErrs {
				return errors.New("deployment.io is away")
			}
			return nil
		},
		token: func(_ context.Context, installationID, org, rejected string) (string, error) {
			if rejected != "" {
				h.rejected = append(h.rejected, rejected)
			}
			token := fmt.Sprintf("tok-%d", len(h.tokens)+1)
			h.tokens = append(h.tokens, token)
			return token, nil
		},
		docker: h.docker.docker(),
		sleep:  func(d time.Duration) { h.slept = append(h.slept, d) },
	}
}

func (h *bumpHarness) params(targets ...tasks.DependencyBumpTargetV1) map[string]interface{} {
	request := tasks.DependencyBumpRequestV1{Repository: 1, Dependencies: targets}
	return bumpParameters(h.t, h.repos, mustJSON(h.t, request))
}

func bumpParameters(t *testing.T, repos []tasks.RepositoryEntry, request string) map[string]interface{} {
	p := map[string]interface{}{}
	_ = jobs.SetParameterValue(p, parameters_enums.OrganizationIDNamespace, "org-1")
	_ = jobs.SetParameterValue(p, parameters_enums.TaskID, "task-1")
	_ = jobs.SetParameterValue(p, parameters_enums.TaskBranchName, bumpBranch)
	_ = jobs.SetParameterValue(p, parameters_enums.Repositories, mustJSON(t, repos))
	_ = jobs.SetParameterValue(p, parameters_enums.DependencyBumpRequest, request)
	_ = jobs.SetParameterValue(p, parameters_enums.JobID, bumpJobID)
	return p
}

// run runs the command and checks what every run must: one report (when it
// was sent first time) that passes deployment-server's rules and has every
// target, the work directory and cache volume gone, every container removed,
// and no token in the job log.
func (h *bumpHarness) run(targets ...tasks.DependencyBumpTargetV1) (tasks.UpdateTaskDependencyBumpDtoV1, error) {
	h.t.Helper()
	_, err := h.command().Run(h.params(targets...), &h.log)
	if len(h.reports) != h.reportErrs+1 {
		h.t.Fatalf("%d reports sent, want %d\nlog:\n%s", len(h.reports), h.reportErrs+1, h.log.String())
	}
	report := h.reports[len(h.reports)-1]
	checkDependencyBumpReport(h.t, report)
	if len(report.Results) != len(targets) {
		h.t.Errorf("%d results for %d targets", len(report.Results), len(targets))
	}
	for i, r := range report.Results {
		if r.Ecosystem != targets[i].Ecosystem || r.Name != targets[i].Name {
			h.t.Errorf("result %d is %s %s, want %s %s", i, r.Ecosystem, r.Name, targets[i].Ecosystem, targets[i].Name)
		}
	}
	if report.TaskID != "task-1" || report.JobID != bumpJobID || report.Repository != 1 {
		h.t.Errorf("report = %+v", report)
	}
	if _, statErr := os.Stat(filepath.Join(h.workRoot, "dependency-bump-"+bumpJobID)); !errors.Is(statErr, fs.ErrNotExist) {
		h.t.Errorf("the work directory is still there: %v", statErr)
	}
	if !equalStrings(h.docker.created, h.docker.dropped) {
		h.t.Errorf("volumes created %v, removed %v", h.docker.created, h.docker.dropped)
	}
	if len(h.docker.removed) != len(h.docker.configs) {
		h.t.Errorf("%d containers created, %d removed", len(h.docker.configs), len(h.docker.removed))
	}
	if strings.Contains(h.log.String(), "tok-") {
		h.t.Errorf("the job log holds a token:\n%s", h.log.String())
	}
	return report, err
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

// checkDependencyBumpReport is a copy of deployment-server's rules for a
// report (UpdateTaskDependencyBumpV1, invalidDependencyBumpUpdateField).
func checkDependencyBumpReport(t *testing.T, dto tasks.UpdateTaskDependencyBumpDtoV1) {
	t.Helper()
	if len(dto.Results) == 0 || len(dto.Results) > tasks.MaxDependencyBumpsPerJob {
		t.Errorf("report has %d results", len(dto.Results))
	}
	anyBumped := false
	for _, r := range dto.Results {
		if !tasks.ValidDependencyEcosystem(r.Ecosystem) || r.Name == "" {
			t.Errorf("result %+v: invalid ecosystem or empty name", r)
		}
		valid := false
		for _, s := range tasks.DependencyBumpResultStates {
			valid = valid || r.State == s
		}
		if !valid {
			t.Errorf("result %+v: state %q isn't a result state", r, r.State)
		}
		if r.State == tasks.DependencyBumpStateFailed && r.Error == "" {
			t.Errorf("result %+v: failed without an error", r)
		}
		if r.State == tasks.DependencyBumpStateBumped {
			anyBumped = true
			if r.ToVersion == "" {
				t.Errorf("result %+v: bumped without a version", r)
			}
		}
		if len(r.Error) > tasks.DependencyErrorMaxBytes {
			t.Errorf("result %+v: error too long", r)
		}
	}
	if anyBumped && !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(dto.CommitSHA) {
		t.Errorf("CommitSHA %q isn't a full commit id", dto.CommitSHA)
	}
	if dto.PRURL != "" && (!strings.HasPrefix(dto.PRURL, "https://") || dto.PRNumber <= 0) {
		t.Errorf("PRURL %q, PRNumber %d", dto.PRURL, dto.PRNumber)
	}
	if dto.PRURL == "" && dto.PRNumber != 0 {
		t.Errorf("PRNumber %d without a PRURL", dto.PRNumber)
	}
}

// branchTip is the dependent's Task branch on the remote, or "".
func (h *bumpHarness) branchTip() string {
	return h.host.Git("acme/app", "for-each-ref", "--format=%(objectname)", "refs/heads/"+bumpBranch)
}

// parentOf is commit's first parent on the remote.
func (h *bumpHarness) parentOf(commit string) string {
	return h.host.Git("acme/app", "rev-parse", commit+"^")
}

// remoteFile is path at the Task branch's tip on the remote.
func (h *bumpHarness) remoteFile(path string) string {
	return h.host.Git("acme/app", "show", bumpBranch+":"+path)
}

func (h *bumpHarness) wantResult(r tasks.DependencyBumpResultDtoV1, state, from, to, errText string) {
	h.t.Helper()
	if r.State != state || r.FromVersion != from || r.ToVersion != to || !strings.Contains(r.Error, errText) || (errText == "" && r.Error != "") {
		h.t.Errorf("result = %+v, want %s %q → %q error %q\nlog:\n%s", r, state, from, to, errText, h.log.String())
	}
}

func (h *bumpHarness) bumpedVersion() string { return "c-" + h.kitCommit[:12] }

// --- the tests ---------------------------------------------------------------------

// With the dependent's pull request open, one commit goes onto its branch,
// fast-forward, no pull request is opened, and the report has the versions
// and the full commit id but no pull request.
func TestDependencyBumpOntoAnOpenPullRequest(t *testing.T) {
	h := newBumpHarness(t)
	prTip := h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "the Task's change\n"})
	h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 3, URL: "https://github.com/acme/app/pull/3"}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	tip := h.branchTip()
	if report.CommitSHA != tip || len(tip) != 40 {
		t.Errorf("CommitSHA %q, branch tip %q", report.CommitSHA, tip)
	}
	if parent := h.parentOf(tip); parent != prTip {
		t.Errorf("the bump's parent is %s, want the pull request's tip %s", parent, prTip)
	}
	if !strings.Contains(h.remoteFile("deps.txt"), "requires "+h.name("kit")+" "+h.bumpedVersion()) || h.remoteFile("feature.txt") != "the Task's change" {
		t.Errorf("deps.txt = %q", h.remoteFile("deps.txt"))
	}
	subject := h.host.Git("acme/app", "log", "-1", "--format=%s%n%an <%ae>%n%b", tip)
	if want := "Bump the fake package " + h.name("kit") + " to " + h.kitCommit[:12] + "\ndeployment.io <noreply@deployment.io>\n" + commitTrailerTag; subject != want {
		t.Errorf("commit = %q, want %q", subject, want)
	}
	if len(h.opened) != 0 || report.PRURL != "" || report.PRNumber != 0 {
		t.Errorf("opened %v, report PR %q %d", h.opened, report.PRURL, report.PRNumber)
	}
}

// With no pull request, the Task branch is created from the base branch,
// pushed, and the pull request opened with the commit's subject.
func TestDependencyBumpOpensThePullRequest(t *testing.T) {
	h := newBumpHarness(t)
	mainTip := h.host.Git("acme/app", "rev-parse", "main")
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	tip := h.branchTip()
	if tip == "" || h.parentOf(tip) != mainTip || report.CommitSHA != tip {
		t.Errorf("tip %q (parent of it should be main %s), CommitSHA %q", tip, mainTip, report.CommitSHA)
	}
	if len(h.opened) != 1 {
		t.Fatalf("opened %d pull requests", len(h.opened))
	}
	args := h.opened[0]
	wantTitle := "Bump the fake package " + h.name("kit") + " to " + h.kitCommit[:12]
	if args.InstallationID != "inst-1" || args.RepoName != "acme/app" || args.BaseBranch != "main" || args.HeadBranch != bumpBranch || args.Title != wantTitle || args.Draft {
		t.Errorf("OpenPullRequest args = %+v", args)
	}
	wantBody := "- the fake package " + h.name("kit") + ": v0.9.0 → " + h.bumpedVersion() + ", because acme/kit merged\n\nOpened by deployment.io after the library merged.\n"
	if args.Body != wantBody {
		t.Errorf("body = %q, want %q", args.Body, wantBody)
	}
	if report.PRURL != "https://github.com/acme/app/pull/7" || report.PRNumber != 7 {
		t.Errorf("report PR %q %d", report.PRURL, report.PRNumber)
	}
}

// With no pull request but the Task branch already pushed, the bump goes on
// top of the branch.
func TestDependencyBumpOntoAPushedBranchWithoutAPullRequest(t *testing.T) {
	h := newBumpHarness(t)
	pushed := h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "pushed earlier\n"})
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	if tip := h.branchTip(); h.parentOf(tip) != pushed || report.CommitSHA != tip {
		t.Errorf("tip %s's parent isn't the pushed %s", tip, pushed)
	}
	if len(h.opened) != 1 || report.PRNumber != 7 {
		t.Errorf("opened %v, report %+v", h.opened, report)
	}
}

// A pull request that already existed is reported with its URL and number.
func TestDependencyBumpReportsAnAlreadyExistingPullRequest(t *testing.T) {
	h := newBumpHarness(t)
	h.openReply = oauth.OpenPullRequestDtoV1{URL: "https://github.com/acme/app/pull/9", Number: 9, AlreadyExisted: true}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	if report.PRURL != "https://github.com/acme/app/pull/9" || report.PRNumber != 9 {
		t.Errorf("report PR %q %d", report.PRURL, report.PRNumber)
	}
}

// A failed OpenPullRequest leaves the push, and the report has no pull
// request.
func TestDependencyBumpKeepsThePushWhenThePullRequestCantOpen(t *testing.T) {
	h := newBumpHarness(t)
	h.openErr = errors.New("provider down")
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	if report.PRURL != "" || report.PRNumber != 0 || report.CommitSHA != h.branchTip() || h.branchTip() == "" {
		t.Errorf("report %+v", report)
	}
}

// A merged or closed pull request, or one that couldn't be looked up, fails
// every target and pushes nothing.
func TestDependencyBumpStopsAtTheDependentsPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pr    oauth.GetOpenPullRequestForBranchDtoV1
		prErr error
		want  string
	}{
		{"merged", oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "closed", Merged: true, PRNumber: 3}, nil, "acme/app's pull request has merged; bump it in a new Task"},
		{"closed", oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "closed", PRNumber: 3}, nil, "acme/app's pull request was closed"},
		{"lookup error", oauth.GetOpenPullRequestForBranchDtoV1{}, errors.New("rpc down"), "couldn't look up acme/app's pull request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBumpHarness(t)
			h.pr, h.prErr = tc.pr, tc.prErr
			report, err := h.run(h.kitTarget(), h.kitTarget())
			if err == nil {
				t.Error("err = nil, want an error")
			}
			for _, r := range report.Results {
				if r.State != tasks.DependencyBumpStateFailed || r.Error != tc.want {
					t.Errorf("result = %+v, want failed with %q", r, tc.want)
				}
			}
			if h.branchTip() != "" || len(h.opened) != 0 || len(h.docker.configs) != 0 {
				t.Errorf("pushed %q, opened %v, containers %d", h.branchTip(), h.opened, len(h.docker.configs))
			}
		})
	}
}

// A target the dependent doesn't require is not_required; the others go on.
func TestDependencyBumpTargetNotRequired(t *testing.T) {
	h := newBumpHarness(t)
	other := h.kitTarget()
	other.Name = h.name("kit") + "/other"
	report, err := h.run(other, h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateNotRequired, "", "", "")
	h.wantResult(report.Results[1], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
}

// A target nothing requires at all: nothing runs, nothing is pushed.
func TestDependencyBumpOnlyTargetNotRequired(t *testing.T) {
	h := newBumpHarness(t)
	other := h.kitTarget()
	other.Name = h.name("kit") + "/other"
	report, err := h.run(other)
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateNotRequired, "", "", "")
	if h.branchTip() != "" || len(h.docker.configs) != 0 || len(h.opened) != 0 {
		t.Error("something ran or was pushed")
	}
}

// A change the ecosystem doesn't allow fails every target, and nothing is
// pushed.
func TestDependencyBumpRefusesADisallowedChange(t *testing.T) {
	h := newBumpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			_ = os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644)
			_ = os.MkdirAll(filepath.Join(src, "vendor", "x"), 0o755)
			_ = os.WriteFile(filepath.Join(src, "vendor", "x", "x.go"), []byte("package x\n"), 0o644)
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget())
	if err == nil {
		t.Error("err = nil")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "the bump changed main.go, which a fake bump may not change")
	if h.branchTip() != "" || len(h.opened) != 0 {
		t.Error("pushed or opened a pull request")
	}
	if len(h.docker.configs) != 1 {
		t.Errorf("%d containers, want only the bump's", len(h.docker.configs))
	}
}

// A bump that changes nothing leaves every target already current.
func TestDependencyBumpWithNoChange(t *testing.T) {
	h := newBumpHarness(t)
	h.docker.run = func(string, []string) (int, string) { return 0, "" }
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateAlreadyCurrent, "v0.9.0", "v0.9.0", "")
	if h.branchTip() != "" || report.CommitSHA != "" || len(h.opened) != 0 {
		t.Error("pushed something")
	}
}

// A target tidy dropped is not_required, with its version before kept.
func TestDependencyBumpTargetDroppedByTheBump(t *testing.T) {
	h := newBumpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			rewriteVersion(src, "deps.txt", `(?m)^requires `+regexp.QuoteMeta(h.name("kit"))+` \S+\n`, "")
		}
		return 0, ""
	}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateNotRequired, "v0.9.0", "", "")
	if h.branchTip() != "" {
		t.Error("pushed")
	}
}

// One target bumped while another is already current: both reported, the
// bump pushed.
func TestDependencyBumpOneBumpedOneCurrent(t *testing.T) {
	h := newBumpHarness(t)
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " v0.9.0\nrequires " + h.name("kit") + "/v2 v2.0.0\n"})
	v2 := h.kitTarget()
	v2.Name = h.name("kit") + "/v2"
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			return applyFakeBump(src, []string{"fake-bump", h.name("kit") + "@" + h.kitCommit})
		}
		return 0, ""
	}
	report, err := h.run(h.kitTarget(), v2)
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	h.wantResult(report.Results[1], tasks.DependencyBumpStateAlreadyCurrent, "v2.0.0", "v2.0.0", "")
	if report.CommitSHA != h.branchTip() {
		t.Errorf("CommitSHA %q", report.CommitSHA)
	}
}

// Two bumped targets make one commit "Bump 2 dependencies" with a line each.
func TestDependencyBumpTwoBumped(t *testing.T) {
	h := newBumpHarness(t)
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " v0.9.0\nrequires " + h.name("kit") + "/v2 v2.0.0\n"})
	v2 := h.kitTarget()
	v2.Name = h.name("kit") + "/v2"
	report, err := h.run(h.kitTarget(), v2)
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[1], tasks.DependencyBumpStateBumped, "v2.0.0", h.bumpedVersion(), "")
	if want := "fake-bump " + h.name("kit") + "@" + h.kitCommit + " " + h.name("kit") + "/v2@" + h.kitCommit; h.docker.commands[0] != want {
		t.Errorf("bump command = %q, want %q", h.docker.commands[0], want)
	}
	message := h.host.Git("acme/app", "log", "-1", "--format=%B", h.branchTip())
	if !strings.HasPrefix(message, "Bump 2 dependencies\n\nBump the fake package "+h.name("kit")+" to ") || strings.Count(message, "\nBump the fake package") != 2 {
		t.Errorf("message = %q", message)
	}
	if h.opened[0].Title != "Bump 2 dependencies" || strings.Count(h.opened[0].Body, ", because acme/kit merged") != 2 {
		t.Errorf("pull request %+v", h.opened[0])
	}
}

// A verify failure fails every target with its last lines, and nothing is
// pushed.
func TestDependencyBumpVerifyFailure(t *testing.T) {
	h := newBumpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-verify" {
			return 1, "compile error: undefined: kit.New\n"
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget())
	if err == nil {
		t.Error("err = nil")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "fake-verify exited with code 1\ncompile error: undefined: kit.New")
	if h.branchTip() != "" || report.CommitSHA != "" {
		t.Error("pushed")
	}
}

// A bump failure fails every target with its last lines.
func TestDependencyBumpBumpFailure(t *testing.T) {
	h := newBumpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			return 2, "can't reach the module\n"
		}
		return 0, ""
	}
	report, err := h.run(h.kitTarget())
	if err == nil {
		t.Error("err = nil")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "can't reach the module")
	if len(h.docker.configs) != 1 || h.branchTip() != "" {
		t.Error("verified or pushed after a failed bump")
	}
}

// A 401 on push refreshes the token once, and the push goes through.
func TestDependencyBumpRefreshesTheTokenOnPush(t *testing.T) {
	h := newBumpHarness(t)
	h.host.RejectPushOnce("acme/app")
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	if report.CommitSHA != h.branchTip() || h.branchTip() == "" {
		t.Errorf("not pushed: %+v", report)
	}
	if !equalStrings(h.rejected, []string{"tok-1"}) {
		t.Errorf("rejected tokens %v", h.rejected)
	}
	if !strings.Contains(h.host.Requests("acme/app")[len(h.host.Requests("acme/app"))-1].Authorization, "Basic ") {
		t.Error("the push carried no basic auth")
	}
}

// The branch moving after the clone starts the bump over once; the second
// bump goes on top of the moved branch.
func TestDependencyBumpStartsOverWhenTheBranchMoves(t *testing.T) {
	h := newBumpHarness(t)
	h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 3}
	h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "one\n"})
	var moved string
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" && moved == "" {
			moved = h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "two\n"})
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	if tip := h.branchTip(); h.parentOf(tip) != moved || report.CommitSHA != tip {
		t.Errorf("tip %s's parent isn't the moved %s", tip, moved)
	}
	// Each try looks it up at the checkout and again before the push.
	if h.prCalls != 4 {
		t.Errorf("looked up the pull request %d times, want 4", h.prCalls)
	}
}

// A branch that moves again fails every target.
func TestDependencyBumpGivesUpWhenTheBranchKeepsMoving(t *testing.T) {
	h := newBumpHarness(t)
	h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 3}
	h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "one\n"})
	n := 0
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			n++
			h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": fmt.Sprintf("moved %d\n", n)})
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget())
	if err == nil {
		t.Error("err = nil")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "the branch moved while bumping")
	if h.remoteFile("feature.txt") != "moved 2" || strings.Contains(h.remoteFile("deps.txt"), "c-") {
		t.Error("the bump was pushed")
	}
	if report.CommitSHA != "" {
		t.Errorf("CommitSHA %q", report.CommitSHA)
	}
}

// A target this runner can't bump fails on its own; the others are bumped.
func TestDependencyBumpFailsUnbumpableTargetsAlone(t *testing.T) {
	h := newBumpHarness(t)
	h.repos = append(h.repos, tasks.RepositoryEntry{Name: "other/kit", CloneURL: h.host.URL + "/other/kit", BaseBranch: "main", InstallationID: "inst-2"})
	unknown := h.kitTarget()
	unknown.Ecosystem = "npm"
	malformed := h.kitTarget()
	malformed.Commit = strings.ToUpper(h.kitCommit)
	otherOwner := h.kitTarget()
	otherOwner.Source = 2
	report, err := h.run(unknown, malformed, otherOwner, h.kitTarget())
	if err == nil {
		t.Error("err = nil, want an error for the failed targets")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "", "", "this runner doesn't support the npm ecosystem")
	h.wantResult(report.Results[1], tasks.DependencyBumpStateFailed, "", "", "the merged commit isn't a commit id")
	h.wantResult(report.Results[2], tasks.DependencyBumpStateFailed, "", "", "other/kit isn't under the same owner as acme/app; bumps across owners aren't supported yet")
	h.wantResult(report.Results[3], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	// The pull request's body lists every target, the failed ones too.
	kit := "the fake package " + h.name("kit")
	wantBody := "- " + h.name("kit") + ": not bumped (this runner doesn't support the npm ecosystem), though acme/kit merged\n" +
		"- " + kit + ": not bumped (the merged commit isn't a commit id), though acme/kit merged\n" +
		"- " + kit + ": not bumped (other/kit isn't under the same owner as acme/app; bumps across owners aren't supported yet), though other/kit merged\n" +
		"- " + kit + ": v0.9.0 → " + h.bumpedVersion() + ", because acme/kit merged\n" +
		"\nOpened by deployment.io after the library merged.\n"
	if len(h.opened) != 1 || h.opened[0].Body != wantBody {
		t.Errorf("opened %+v, want the body %q", h.opened, wantBody)
	}
}

// A target already current or not required is listed in the pull request's
// body too.
func TestDependencyBumpPullRequestBodyListsEveryTarget(t *testing.T) {
	h := newBumpHarness(t)
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " v0.9.0\nrequires " + h.name("kit") + "/v2 v2.0.0\n"})
	v2 := h.kitTarget()
	v2.Name = h.name("kit") + "/v2"
	other := h.kitTarget()
	other.Name = h.name("kit") + "/other"
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			return applyFakeBump(src, []string{"fake-bump", h.name("kit") + "@" + h.kitCommit})
		}
		return 0, ""
	}
	if _, err := h.run(h.kitTarget(), v2, other); err != nil {
		t.Fatal(err)
	}
	kit := "the fake package " + h.name("kit")
	wantBody := "- " + kit + ": v0.9.0 → " + h.bumpedVersion() + ", because acme/kit merged\n" +
		"- " + kit + "/v2: v2.0.0 → v2.0.0, because acme/kit merged\n" +
		"- " + kit + "/other: not required, though acme/kit merged\n" +
		"\nOpened by deployment.io after the library merged.\n"
	if len(h.opened) != 1 || h.opened[0].Body != wantBody {
		t.Errorf("opened %+v, want the body %q", h.opened, wantBody)
	}
}

// The dependent's pull request merging or closing while bumping fails every
// target, and nothing is pushed: merging needn't move the branch.
func TestDependencyBumpStopsWhenThePullRequestMergesWhileBumping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pr    oauth.GetOpenPullRequestForBranchDtoV1
		prErr error
		want  string
	}{
		{"merged", oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "closed", Merged: true, PRNumber: 3}, nil, "acme/app's pull request has merged; bump it in a new Task"},
		{"closed", oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "closed", PRNumber: 3}, nil, "acme/app's pull request was closed"},
		{"lookup error", oauth.GetOpenPullRequestForBranchDtoV1{}, errors.New("rpc down"), "couldn't look up acme/app's pull request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBumpHarness(t)
			h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 3}
			h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "one\n"})
			before := h.branchTip()
			h.docker.run = func(src string, cmd []string) (int, string) {
				if cmd[0] == "fake-bump" {
					h.pr, h.prErr = tc.pr, tc.prErr
				}
				return applyFakeBump(src, cmd)
			}
			report, err := h.run(h.kitTarget())
			if err == nil {
				t.Error("err = nil, want an error")
			}
			h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", tc.want)
			if h.branchTip() != before || len(h.opened) != 0 || report.CommitSHA != "" {
				t.Errorf("pushed %q (was %q), opened %v, CommitSHA %q", h.branchTip(), before, h.opened, report.CommitSHA)
			}
		})
	}
}

// A pull request opened while bumping is used, not opened again.
func TestDependencyBumpUsesAPullRequestOpenedWhileBumping(t *testing.T) {
	h := newBumpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 9}
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	if report.CommitSHA != h.branchTip() || len(h.opened) != 0 || report.PRURL != "" {
		t.Errorf("CommitSHA %q, opened %v, PRURL %q", report.CommitSHA, h.opened, report.PRURL)
	}
}

// A merged commit that isn't in the library's repository fails that target.
func TestDependencyBumpCommitNotInTheLibrary(t *testing.T) {
	h := newBumpHarness(t)
	missing := h.kitTarget()
	missing.Commit = strings.Repeat("ab", 20)
	report, err := h.run(missing)
	if err == nil {
		t.Error("err = nil")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "the merged commit abababababab isn't in acme/kit")
	if len(h.docker.configs) != 0 {
		t.Error("a container ran")
	}
}

// A mirror that can't be fetched fails every target, with no token in the
// message.
func TestDependencyBumpMirrorFetchFailure(t *testing.T) {
	h := newBumpHarness(t)
	h.host.Unauthorized("acme/priv")
	report, err := h.run(h.kitTarget())
	if err == nil {
		t.Error("err = nil")
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "couldn't fetch "+h.hostName+"/acme/priv: ")
	if strings.Contains(report.Results[0].Error, "tok-") {
		t.Errorf("error holds a token: %q", report.Results[0].Error)
	}
}

// More than 20 private repositories fail every target.
func TestDependencyBumpTooManyMirrors(t *testing.T) {
	h := newBumpHarness(t)
	var deps strings.Builder
	deps.WriteString("requires " + h.name("kit") + " v0.9.0\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&deps, "requires %s v1.0.0\n", h.name(fmt.Sprintf("m%d", i)))
	}
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": deps.String()})
	report, _ := h.run(h.kitTarget())
	h.wantResult(report.Results[0], tasks.DependencyBumpStateFailed, "v0.9.0", "", "the bump needs more than 20 private repositories")
}

// A module path that spells a repository in another case, or with ".git",
// is the same mirror, reachable at that spelling.
func TestDependencyBumpMirrorAliases(t *testing.T) {
	h := newBumpHarness(t)
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " v0.9.0\nrequires " + h.name("Kit") + "/sub v0.9.0\nrequires " + h.name("priv.git") + " v1.0.0\n"})
	links := map[string]string{}
	var mirrors []string
	h.docker.atBump = func(host *container.HostConfig) {
		dir := mountSource(host, "/mirrors")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				links[e.Name()], _ = os.Readlink(filepath.Join(dir, e.Name()))
			} else {
				mirrors = append(mirrors, e.Name())
			}
		}
	}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	sort.Strings(mirrors)
	if !equalStrings(mirrors, []string{"kit", "priv"}) {
		t.Errorf("mirrors = %v, want kit and priv once each", mirrors)
	}
	want := map[string]string{"kit.git": "kit", "Kit": "kit", "Kit.git": "kit", "priv.git": "priv"}
	if caseInsensitiveDir(t, t.TempDir()) {
		// "Kit" already resolves to "kit" there (macOS), so linkMirror
		// makes no link for another case.
		delete(want, "Kit")
		delete(want, "Kit.git")
	}
	for link, target := range want {
		if links[link] != target {
			t.Errorf("mirror alias %s -> %q, want %q (aliases %v)", link, links[link], target, links)
		}
	}
	if len(links) != len(want) {
		t.Errorf("mirror aliases = %v, want %v", links, want)
	}
}

// The mirrors are bare, have every tag and hold no token; the library's
// manifest is read through the adapter from a local clone of its mirror; the
// containers are configured as decided.
func TestDependencyBumpMirrorsAndContainers(t *testing.T) {
	h := newBumpHarness(t)
	work := filepath.Join(h.workRoot, "dependency-bump-"+bumpJobID)
	var mirrors, aliases []string
	h.docker.atBump = func(host *container.HostConfig) {
		dir := mountSource(host, "/mirrors")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				if target, _ := os.Readlink(filepath.Join(dir, e.Name())); !strings.EqualFold(strings.TrimSuffix(e.Name(), ".git"), target) {
					t.Errorf("mirror alias %s points at %q", e.Name(), target)
				}
				aliases = append(aliases, e.Name())
				continue
			}
			mirrors = append(mirrors, e.Name())
			repo, err := git.PlainOpen(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Errorf("mirror %s: %v", e.Name(), err)
				continue
			}
			cfg, _ := repo.Config()
			if !cfg.Core.IsBare {
				t.Errorf("mirror %s isn't bare", e.Name())
			}
			raw, _ := os.ReadFile(filepath.Join(dir, e.Name(), "config"))
			if strings.Contains(string(raw), "tok-") || strings.Contains(string(raw), "@") {
				t.Errorf("mirror %s's config holds a credential:\n%s", e.Name(), raw)
			}
			if e.Name() == "kit" {
				if _, err := repo.Tag("v1.0.0"); err != nil {
					t.Errorf("the kit mirror has no tag v1.0.0: %v", err)
				}
			}
		}
	}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatal(err)
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	sort.Strings(mirrors)
	if !equalStrings(mirrors, []string{"kit", "priv"}) {
		t.Errorf("mirrors = %v, want kit (the library) and priv (what it requires)", mirrors)
	}
	// GitLab advertises its repositories' URLs with ".git".
	sort.Strings(aliases)
	if !equalStrings(aliases, []string{"kit.git", "priv.git"}) {
		t.Errorf("mirror aliases = %v, want kit.git and priv.git", aliases)
	}
	readSource := false
	for _, dir := range h.eco.read {
		readSource = readSource || dir == filepath.Join(work, "sources", "kit")
	}
	if !readSource {
		t.Errorf("the library's manifest wasn't read from its mirror's clone: %v", h.eco.read)
	}
	if len(h.docker.configs) != 2 {
		t.Fatalf("%d containers, want the bump's and the verify's", len(h.docker.configs))
	}
	volume := "dependency-bump-cache-" + bumpJobID
	if !equalStrings(h.docker.created, []string{volume}) {
		t.Errorf("volumes %v", h.docker.created)
	}
	bump, bumpHost := h.docker.configs[0], h.docker.hosts[0]
	verify, verifyHost := h.docker.configs[1], h.docker.hosts[1]
	if bumpHost.NetworkMode != "bridge" || verifyHost.NetworkMode != "none" {
		t.Errorf("networks %q and %q", bumpHost.NetworkMode, verifyHost.NetworkMode)
	}
	wantMounts := func(name string, host *container.HostConfig, want map[string]mount.Mount) {
		if len(host.Mounts) != len(want) {
			t.Errorf("%s: mounts %+v", name, host.Mounts)
		}
		for target, w := range want {
			if m, ok := findBumpMount(host, target); !ok || m.Source != w.Source || m.ReadOnly != w.ReadOnly || m.Type != w.Type {
				t.Errorf("%s: mount %s = %+v, want %+v", name, target, m, w)
			}
		}
	}
	wantMounts("bump", bumpHost, map[string]mount.Mount{
		"/src":     {Type: mount.TypeBind, Source: filepath.Join(work, "repo")},
		"/mirrors": {Type: mount.TypeBind, Source: filepath.Join(work, "mirrors"), ReadOnly: true},
		"/cache":   {Type: mount.TypeVolume, Source: volume},
	})
	wantMounts("verify", verifyHost, map[string]mount.Mount{
		"/src":   {Type: mount.TypeBind, Source: filepath.Join(work, "repo"), ReadOnly: true},
		"/cache": {Type: mount.TypeVolume, Source: volume, ReadOnly: true},
	})
	env := strings.Join(bump.Env, "\n")
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.file:///mirrors/.insteadOf", "GIT_CONFIG_VALUE_0=https://" + h.hostName + "/acme/",
		"GIT_CONFIG_KEY_1=safe.directory", "GIT_CONFIG_VALUE_1=*", "FAKE_PRIVATE=" + h.hostName + "/acme"} {
		if !strings.Contains(env, want) {
			t.Errorf("bump env lacks %s:\n%s", want, env)
		}
	}
	for i, cfg := range h.docker.configs {
		host := h.docker.hosts[i]
		if strings.Contains(strings.Join(cfg.Env, " "), "tok-") || len(cfg.Env) > 12 {
			t.Errorf("container %d env %v", i, cfg.Env)
		}
		if cfg.Labels[reclaim.PurposeLabelKey] != reclaim.PurposeDependencyBump || cfg.User != "root" || cfg.WorkingDir != "/src" || cfg.Image != "fake:1" {
			t.Errorf("container %d config %+v", i, cfg)
		}
		if !equalStrings(host.ExtraHosts, cloudMetadataExtraHosts()) || host.Resources.PidsLimit == nil || *host.Resources.PidsLimit != 1024 {
			t.Errorf("container %d host config %+v", i, host)
		}
	}
	if strings.Contains(strings.Join(verify.Env, " "), "insteadOf") {
		t.Errorf("verify env %v", verify.Env)
	}
	if !equalStrings(h.docker.commands, []string{"fake-bump " + h.name("kit") + "@" + h.kitCommit, "fake-verify"}) {
		t.Errorf("commands %v", h.docker.commands)
	}
}

// The report is retried, 10 s apart, until it's sent.
func TestDependencyBumpRetriesTheReport(t *testing.T) {
	h := newBumpHarness(t)
	h.reportErrs = 2
	if _, err := h.run(h.kitTarget()); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) != 2 || h.slept[0] != 10*time.Second {
		t.Errorf("slept %v", h.slept)
	}
	if !equalStrings([]string{fmt.Sprint(h.reports[0].Results)}, []string{fmt.Sprint(h.reports[2].Results)}) {
		t.Error("the retried report differs")
	}
}

// A report that can't be sent is the command's error.
func TestDependencyBumpReportNeverSent(t *testing.T) {
	h := newBumpHarness(t)
	cmd := h.command()
	cmd.sendReport = func(string, tasks.UpdateTaskDependencyBumpDtoV1) error {
		h.reports = append(h.reports, tasks.UpdateTaskDependencyBumpDtoV1{})
		return errors.New("away")
	}
	_, err := cmd.Run(h.params(h.kitTarget()), &h.log)
	if err == nil || !strings.Contains(err.Error(), "couldn't send") || len(h.reports) != 3 {
		t.Errorf("err = %v after %d sends", err, len(h.reports))
	}
}

// The real Go adapter through the whole command: go get, go mod tidy, then
// go build and go vet, offline.
func TestDependencyBumpWithTheGoAdapter(t *testing.T) {
	h := newBumpHarness(t)
	kit := h.host.Commit("acme/kit", "main", map[string]string{"go.mod": "module github.com/acme/kit\n\ngo 1.27\n"})
	h.host.Commit("acme/app", "main", map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27.3\n\nrequire github.com/acme/kit v1.4.0\n", "go.sum": ""})
	target := tasks.DependencyBumpTargetV1{Ecosystem: "go", Name: "github.com/acme/kit", Source: 0, Commit: kit, Release: tasks.DependencyReleaseCommit}
	report, err := h.run(target)
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v1.4.0", "v0.0.0-20261126000000-"+kit[:12], "")
	want := []string{"go get github.com/acme/kit@" + kit, "go mod tidy", "go build -o /dev/null ./...", "go vet ./..."}
	if !equalStrings(h.docker.commands, want) {
		t.Errorf("commands %v, want %v", h.docker.commands, want)
	}
	if h.docker.configs[0].Image != "golang:1.27.3" || !strings.Contains(strings.Join(h.docker.configs[0].Env, " "), "GOPRIVATE="+h.hostName+"/acme") {
		t.Errorf("bump container %+v", h.docker.configs[0])
	}
	if h.opened[0].Title != "Bump the Go module github.com/acme/kit to "+kit[:12] {
		t.Errorf("title %q", h.opened[0].Title)
	}
}

// Inputs that don't parse report nothing and are the command's error.
func TestDependencyBumpInputsThatDontParse(t *testing.T) {
	repos := []tasks.RepositoryEntry{{Name: "acme/kit", CloneURL: "https://github.com/acme/kit"}, {Name: "acme/app", CloneURL: "https://github.com/acme/app"}}
	target := tasks.DependencyBumpTargetV1{Ecosystem: "go", Name: "github.com/acme/kit", Source: 0, Commit: strings.Repeat("a", 40)}
	request := func(repository int, targets ...tasks.DependencyBumpTargetV1) string {
		return mustJSON(t, tasks.DependencyBumpRequestV1{Repository: repository, Dependencies: targets})
	}
	with := func(f func(*tasks.DependencyBumpTargetV1)) tasks.DependencyBumpTargetV1 {
		d := target
		f(&d)
		return d
	}
	many := make([]tasks.DependencyBumpTargetV1, tasks.MaxDependencyBumpsPerJob+1)
	for i := range many {
		many[i] = target
	}
	cases := map[string]map[string]interface{}{
		"bad request JSON":        bumpParameters(t, repos, "{"),
		"no targets":              bumpParameters(t, repos, request(1)),
		"17 targets":              bumpParameters(t, repos, request(1, many...)),
		"repository out of range": bumpParameters(t, repos, request(2, target)),
		"negative repository":     bumpParameters(t, repos, request(-1, target)),
		"source out of range":     bumpParameters(t, repos, request(1, with(func(d *tasks.DependencyBumpTargetV1) { d.Source = 5 }))),
		"source is repository":    bumpParameters(t, repos, request(1, with(func(d *tasks.DependencyBumpTargetV1) { d.Source = 1 }))),
		"invalid ecosystem":       bumpParameters(t, repos, request(1, with(func(d *tasks.DependencyBumpTargetV1) { d.Ecosystem = "Go!" }))),
		"empty name":              bumpParameters(t, repos, request(1, with(func(d *tasks.DependencyBumpTargetV1) { d.Name = "" }))),
	}
	missing := bumpParameters(t, repos, request(1, target))
	key, _ := parameters_enums.DependencyBumpRequest.Key()
	delete(missing, key)
	cases["missing parameter"] = missing
	badRepos := bumpParameters(t, repos, request(1, target))
	_ = jobs.SetParameterValue(badRepos, parameters_enums.Repositories, "[")
	cases["bad repositories JSON"] = badRepos
	for name, params := range cases {
		sent := 0
		cmd := &RunDependencyBump{
			workRoot:   t.TempDir(),
			sendReport: func(string, tasks.UpdateTaskDependencyBumpDtoV1) error { sent++; return nil },
			getPR: func(string, string, string, string) (oauth.GetOpenPullRequestForBranchDtoV1, error) {
				t.Errorf("%s: looked up a pull request", name)
				return oauth.GetOpenPullRequestForBranchDtoV1{}, nil
			},
		}
		var log strings.Builder
		if _, err := cmd.Run(params, &log); err == nil {
			t.Errorf("%s: err = nil", name)
		}
		if sent != 0 {
			t.Errorf("%s: %d reports sent", name, sent)
		}
		if !strings.Contains(log.String(), "nothing is reported") {
			t.Errorf("%s: log %q", name, log.String())
		}
	}
}

func TestGetReturnsRunDependencyBump(t *testing.T) {
	cmd, err := Get(commands_enums.RunDependencyBump)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cmd.(*RunDependencyBump); !ok {
		t.Errorf("Get = %T", cmd)
	}
}

// caseInsensitiveDir reports whether dir's filesystem ignores case in names.
func caseInsensitiveDir(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "case"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASE"))
	return err == nil
}

// --- a pin that already contains the merged commit ---------------------------------

// pinKit makes the dependent's main pin kit at version.
func (h *bumpHarness) pinKit(version string) {
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "provides " + h.name("app") + "\nrequires " + h.name("kit") + " " + version + "\n"})
}

// laterKitCommit is a kit commit on main after the merged commit.
func (h *bumpHarness) laterKitCommit() string {
	return h.host.Commit("acme/kit", "main", map[string]string{"later.txt": "after the merge\n"})
}

// wantNothingRan checks no container ran, nothing was pushed and no pull
// request opened.
func (h *bumpHarness) wantNothingRan(tipBefore string, report tasks.UpdateTaskDependencyBumpDtoV1) {
	h.t.Helper()
	if len(h.docker.commands) != 0 || len(h.docker.configs) != 0 {
		h.t.Errorf("containers ran: %v", h.docker.commands)
	}
	if tip := h.branchTip(); tip != tipBefore || report.CommitSHA != "" || len(h.opened) != 0 || report.PRURL != "" {
		h.t.Errorf("branch tip %q (was %q), CommitSHA %q, opened %v", tip, tipBefore, report.CommitSHA, h.opened)
	}
}

// A pin of a kit commit after the merged commit is already current: nothing
// runs and nothing is pushed.
func TestDependencyBumpPinAlreadyContainsTheMergedCommit(t *testing.T) {
	h := newBumpHarness(t)
	pinned := "c-" + h.laterKitCommit()[:12]
	h.pinKit(pinned)
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateAlreadyCurrent, pinned, pinned, "")
	h.wantNothingRan("", report)
	want := "acme/app already pins the fake package " + h.name("kit") + " at " + pinned + ", which contains acme/kit's merged commit " + h.kitCommit[:12]
	if !strings.Contains(h.log.String(), want) {
		t.Errorf("log doesn't say %q:\n%s", want, h.log.String())
	}
}

// A pin of the merged commit itself is already current.
func TestDependencyBumpPinIsTheMergedCommit(t *testing.T) {
	h := newBumpHarness(t)
	h.pinKit(h.bumpedVersion())
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateAlreadyCurrent, h.bumpedVersion(), h.bumpedVersion(), "")
	h.wantNothingRan("", report)
}

// A pin of an older kit commit is bumped as before.
func TestDependencyBumpPinOlderThanTheMergedCommit(t *testing.T) {
	h := newBumpHarness(t)
	older := "c-" + h.host.Git("acme/kit", "rev-parse", h.kitCommit+"^")[:12]
	h.pinKit(older)
	prTip := h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "the Task's change\n"})
	h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 3, URL: "https://github.com/acme/app/pull/3"}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantBumpedOnto(report, older, prTip)
}

// wantBumpedOnto checks kit was bumped from from, in one commit on top of
// parent, with no pull request opened.
func (h *bumpHarness) wantBumpedOnto(report tasks.UpdateTaskDependencyBumpDtoV1, from, parent string) {
	h.t.Helper()
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, from, h.bumpedVersion(), "")
	tip := h.branchTip()
	if report.CommitSHA != tip || len(tip) != 40 || h.parentOf(tip) != parent {
		h.t.Errorf("CommitSHA %q, branch tip %q, want on top of %s", report.CommitSHA, tip, parent)
	}
	if !strings.Contains(h.remoteFile("deps.txt"), "requires "+h.name("kit")+" "+h.bumpedVersion()) {
		h.t.Errorf("deps.txt = %q", h.remoteFile("deps.txt"))
	}
	if len(h.opened) != 0 || report.PRURL != "" {
		h.t.Errorf("opened %v, report PR %q", h.opened, report.PRURL)
	}
}

// A pin on a branch that diverged from the merged commit is bumped.
func TestDependencyBumpPinOnADivergedBranch(t *testing.T) {
	h := newBumpHarness(t)
	side := h.host.Commit("acme/kit", "side", map[string]string{"side.txt": "the PR head\n"})
	h.kitCommit = h.laterKitCommit()
	pinned := "c-" + side[:12]
	h.pinKit(pinned)
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, pinned, h.bumpedVersion(), "")
	if h.branchTip() == "" || report.CommitSHA != h.branchTip() {
		t.Errorf("not pushed: %+v", report)
	}
}

// A pin naming no commit (the harness's v0.9.0) is bumped.
func TestDependencyBumpPinNamesNoCommit(t *testing.T) {
	h := newBumpHarness(t)
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "v0.9.0", h.bumpedVersion(), "")
	if strings.Contains(h.log.String(), "couldn't check") {
		t.Errorf("log:\n%s", h.log.String())
	}
}

// A pin of a commit the mirror doesn't have is bumped, and that isn't an
// error.
func TestDependencyBumpPinNotInTheMirror(t *testing.T) {
	h := newBumpHarness(t)
	h.pinKit("c-0123456789ab")
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateBumped, "c-0123456789ab", h.bumpedVersion(), "")
	if h.branchTip() == "" || report.CommitSHA != h.branchTip() {
		t.Errorf("not pushed: %+v", report)
	}
	if strings.Contains(h.log.String(), "couldn't check") {
		t.Errorf("log:\n%s", h.log.String())
	}
}

// Of two targets, the one already current takes no part in the bump.
func TestDependencyBumpOneContainedOneBumped(t *testing.T) {
	h := newBumpHarness(t)
	pinned := "c-" + h.laterKitCommit()[:12]
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " " + pinned + "\nrequires " + h.name("kit") + "/v2 v2.0.0\n"})
	v2 := h.kitTarget()
	v2.Name = h.name("kit") + "/v2"
	report, err := h.run(h.kitTarget(), v2)
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateAlreadyCurrent, pinned, pinned, "")
	h.wantResult(report.Results[1], tasks.DependencyBumpStateBumped, "v2.0.0", h.bumpedVersion(), "")
	if len(h.docker.commands) == 0 {
		t.Fatal("no bump ran")
	}
	if want := "fake-bump " + h.name("kit") + "/v2@" + h.kitCommit; h.docker.commands[0] != want {
		t.Errorf("bump command = %q, want %q", h.docker.commands[0], want)
	}
	if h.branchTip() == "" || report.CommitSHA != h.branchTip() {
		t.Errorf("not pushed: %+v", report)
	}
	if deps := h.remoteFile("deps.txt"); !strings.Contains(deps, "requires "+h.name("kit")+" "+pinned+"\n") || !strings.Contains(deps, "requires "+h.name("kit")+"/v2 "+h.bumpedVersion()) {
		t.Errorf("deps.txt = %q", deps)
	}
}

// The branch moving, while bumping, to a commit that pins a later kit commit:
// the restart finds it already current and pushes nothing.
func TestDependencyBumpStartsOverOntoAPinThatContainsTheMergedCommit(t *testing.T) {
	h := newBumpHarness(t)
	h.pr = oauth.GetOpenPullRequestForBranchDtoV1{Found: true, State: "open", PRNumber: 3}
	h.host.Commit("acme/app", bumpBranch, map[string]string{"feature.txt": "one\n"})
	pinned := "c-" + h.laterKitCommit()[:12]
	var moved string
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" && moved == "" {
			moved = h.host.Commit("acme/app", bumpBranch, map[string]string{"deps.txt": "requires " + h.name("kit") + " " + pinned + "\n"})
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget())
	if err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateAlreadyCurrent, pinned, pinned, "")
	if tip := h.branchTip(); moved == "" || tip != moved || report.CommitSHA != "" || len(h.opened) != 0 {
		t.Errorf("branch tip %s, moved to %s, CommitSHA %q, opened %v", tip, moved, report.CommitSHA, h.opened)
	}
	if n := strings.Count(strings.Join(h.docker.commands, "\n"), "fake-bump"); n != 1 {
		t.Errorf("%d bumps ran, want only the first try's: %v", n, h.docker.commands)
	}
}

// Bumping one target moves the pin of a target left alone because it already
// contains the merged commit: the bump fails and nothing is pushed.
func TestDependencyBumpFailsWhenTheBumpMovesASkippedPin(t *testing.T) {
	h := newBumpHarness(t)
	pinned := "c-" + h.laterKitCommit()[:12]
	h.host.Commit("acme/app", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " " + pinned + "\nrequires " + h.name("kit") + "/v2 v2.0.0\n"})
	v2 := h.kitTarget()
	v2.Name = h.name("kit") + "/v2"
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			// As go get lowering a module the bumped one requires.
			rewriteVersion(src, "deps.txt", `(?m)^requires `+regexp.QuoteMeta(h.name("kit"))+` \S+$`, "requires "+h.name("kit")+" v0.8.0")
		}
		return applyFakeBump(src, cmd)
	}
	report, err := h.run(h.kitTarget(), v2)
	if err == nil {
		t.Fatalf("err = nil, want the failed target\nlog:\n%s", h.log.String())
	}
	h.wantResult(report.Results[0], tasks.DependencyBumpStateAlreadyCurrent, pinned, pinned, "")
	h.wantResult(report.Results[1], tasks.DependencyBumpStateFailed, "v2.0.0", "", "nothing was pushed")
	if h.branchTip() != "" || report.CommitSHA != "" || len(h.opened) != 0 {
		t.Errorf("pushed: tip %q, report %+v", h.branchTip(), report)
	}
}
