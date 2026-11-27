package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/internal/gittest"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/moby/moby/api/types/container"
)

// startUpHarness is the bump harness's host (acme/kit, acme/app, acme/priv)
// with a Task whose Step 1 checks out acme/app (and maybe more), its branch
// pushed by Step 0, driven through CheckoutRepository.runForTask.
type startUpHarness struct {
	*bumpHarness
	org     string
	entries []tasks.RepositoryEntry
	// stepTips is each entry's Task branch on the remote before the run.
	stepTips []string
	params   map[string]interface{}
}

const (
	startUpStepPrompt = "Add the feature."
	startUpTaskTitle  = "Use the new kit"
	startUpKitTitle   = "Change the kit"
)

func newStartUpHarness(t *testing.T) *startUpHarness {
	t.Helper()
	h := &startUpHarness{bumpHarness: newBumpHarness(t), org: "org-" + filepath.Base(filepath.Dir(t.TempDir()))}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/tmp", h.org)) })
	h.entries = []tasks.RepositoryEntry{h.entry("app")}
	fakeRefresh(t, "checkout-token", "push-token")
	recordTaskEvents(t)
	t.Cleanup(func() { commandUtils.CloseTaskEvents(bumpJobID, 5*time.Second) })
	original := commandUtils.UpdateTasksPipeline
	commandUtils.UpdateTasksPipeline, _ = goPipeline.NewPipeline(5, time.Hour, func(string, []tasks.UpdateTaskStepRunningDtoV1) {})
	t.Cleanup(func() {
		commandUtils.UpdateTasksPipeline.Shutdown()
		commandUtils.UpdateTasksPipeline = original
	})
	return h
}

// kitSource is the upstream Task's merged kit.
func (h *startUpHarness) kitSource() tasks.StartUpBumpSourceV1 {
	return tasks.StartUpBumpSourceV1{TaskID: "task-a", TaskTitle: startUpKitTitle, Repository: h.entry("kit"), Commit: h.kitCommit,
		Provides: []tasks.StartUpBumpProvidedV1{{Ecosystem: "fake", Name: h.name("kit")}}}
}

func (h *startUpHarness) command() *CheckoutRepository {
	bump := h.bumpHarness.command()
	return &CheckoutRepository{bumpWorkRoot: h.workRoot, bumpToken: bump.token, bumpDocker: bump.docker}
}

// run pushes each entry's Task branch as Step 0 left it, then runs Step 1's
// checkout with sources (no parameter when sources is nil), and checks what
// every run must: no work directory left, every volume and container
// removed, no token in the log, nothing pushed.
func (h *startUpHarness) run(sources []tasks.StartUpBumpSourceV1) error {
	h.t.Helper()
	h.stepTips = nil
	for _, e := range h.entries {
		repo := strings.TrimPrefix(e.CloneURL, h.host.URL+"/")
		h.stepTips = append(h.stepTips, h.host.Commit(repo, bumpBranch, map[string]string{"feature.txt": "step 0\n"}))
	}
	h.params = map[string]interface{}{}
	for k, v := range map[parameters_enums.Key]string{
		parameters_enums.OrganizationIDNamespace: h.org,
		parameters_enums.TaskID:                  "task-1",
		parameters_enums.TaskTitle:               startUpTaskTitle,
		parameters_enums.TaskBranchName:          bumpBranch,
		parameters_enums.Repositories:            mustJSON(h.t, h.entries),
		parameters_enums.JobID:                   bumpJobID,
		parameters_enums.StepPrompt:              startUpStepPrompt,
		parameters_enums.DashboardURL:            "https://app.deployment.io/",
	} {
		_ = jobs.SetParameterValue(h.params, k, v)
	}
	_ = jobs.SetParameterValue(h.params, parameters_enums.StepIndex, int64(1))
	if sources != nil {
		_ = jobs.SetParameterValue(h.params, parameters_enums.StartUpBumpSources, mustJSON(h.t, sources))
	}
	_, err := h.command().runForTask(h.params, &h.log)
	if entries, _ := os.ReadDir(h.workRoot); len(entries) != 0 {
		h.t.Errorf("work directories left: %v", entries)
	}
	if !equalStrings(h.docker.created, h.docker.dropped) || len(h.docker.removed) != len(h.docker.configs) {
		h.t.Errorf("volumes created %v, removed %v; %d containers, %d removed", h.docker.created, h.docker.dropped, len(h.docker.configs), len(h.docker.removed))
	}
	if log := h.log.String(); strings.Contains(log, "tok-") || strings.Contains(log, "-token") {
		h.t.Errorf("the log holds a token:\n%s", log)
	}
	for i, e := range h.entries {
		if tip := h.host.Git(strings.TrimPrefix(e.CloneURL, h.host.URL+"/"), "rev-parse", bumpBranch); tip != h.stepTips[i] {
			h.t.Errorf("%s's branch was pushed: %s, was %s", e.Name, tip, h.stepTips[i])
		}
	}
	return err
}

func (h *startUpHarness) repoDir(idx int) string {
	return commandUtils.GetTaskRepositoryDir(h.org, "task-1", idx, h.entries[idx].Name)
}

// head is the Step's checkout of entry idx: its HEAD and its Task branch,
// which must be the same commit.
func (h *startUpHarness) head(idx int) string {
	h.t.Helper()
	repository, err := git.PlainOpen(h.repoDir(idx))
	if err != nil {
		h.t.Fatal(err)
	}
	head, err := repository.Head()
	if err != nil || head.Name().Short() != bumpBranch {
		h.t.Fatalf("HEAD %v: %v", head, err)
	}
	return head.Hash().String()
}

// gitIn runs git in the Step's checkout of entry idx.
func (h *startUpHarness) gitIn(idx int, args ...string) string {
	h.t.Helper()
	return gitOutput(h.t, h.repoDir(idx), args...)
}

func (h *startUpHarness) jobOutput() jobOutputData {
	h.t.Helper()
	var data jobOutputData
	raw, _ := jobs.GetParameterValue[string](h.params, parameters_enums.JobOutput)
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		h.t.Fatalf("JobOutput %q: %v", raw, err)
	}
	return data
}

func (h *startUpHarness) prompt() string {
	p, _ := jobs.GetParameterValue[string](h.params, parameters_enums.StepPrompt)
	return p
}

// wantPrompt checks the StepPrompt is the description and then the block of
// lines, or the description alone.
func (h *startUpHarness) wantPrompt(lines ...string) {
	h.t.Helper()
	want := startUpStepPrompt
	if len(lines) > 0 {
		want += "\n\n" + startUpBumpPromptHeader + "\n" + strings.Join(lines, "\n") + "\n"
	}
	if got := h.prompt(); got != want {
		h.t.Errorf("StepPrompt =\n%s\nwant\n%s", got, want)
	}
}

// wantUnmoved checks the Step's checkout of entry idx is the pushed branch,
// clean.
func (h *startUpHarness) wantUnmoved(idx int) {
	h.t.Helper()
	if head := h.head(idx); head != h.stepTips[idx] {
		h.t.Errorf("%s's checkout moved to %s, want %s", h.entries[idx].Name, head, h.stepTips[idx])
	}
	if status := h.gitIn(idx, "status", "--porcelain"); status != "" {
		h.t.Errorf("%s's checkout isn't clean:\n%s", h.entries[idx].Name, status)
	}
}

func (h *startUpHarness) wantBump(got startUpBumpOutput, want startUpBumpOutput) {
	h.t.Helper()
	if got.Error != "" && want.Error != "" && strings.Contains(got.Error, want.Error) {
		want.Error = got.Error
	}
	if got.BuildError != "" && want.BuildError != "" && strings.Contains(got.BuildError, want.BuildError) {
		want.BuildError = got.BuildError
	}
	if got != want {
		h.t.Errorf("start-up bump = %+v\nwant %+v\nlog:\n%s", got, want, h.log.String())
	}
}

func (h *startUpHarness) kitBump(idx int, state string) startUpBumpOutput {
	return startUpBumpOutput{Repository: idx, Ecosystem: "fake", Name: h.name("kit"), SourceTaskID: "task-a", SourceRepository: "acme/kit", Commit: h.kitCommit, State: state, FromVersion: "v0.9.0"}
}

func (h *startUpHarness) bumpedLine(repo string) string {
	return fmt.Sprintf("- %s: bumped the fake package %s from v0.9.0 to %s, the merged commit %s of acme/kit from the Task %q. It is already committed on the branch.",
		repo, h.name("kit"), h.bumpedVersion(), h.kitCommit[:12], startUpKitTitle)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// --- the tests ---------------------------------------------------------------------

// A repository requiring the source's library is bumped before the agent
// runs: one commit on the Task branch on top of the checkout, recorded as the
// base commit, chowned, in the prompt and in the JobOutput, and not pushed.
func TestStartUpBumpBumpsTheCheckout(t *testing.T) {
	h := newStartUpHarness(t)
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	head := h.head(0)
	if head == h.stepTips[0] || h.gitIn(0, "rev-parse", head+"^") != h.stepTips[0] {
		t.Fatalf("HEAD %s isn't one commit on the checkout %s\nlog:\n%s", head, h.stepTips[0], h.log.String())
	}
	if status := h.gitIn(0, "status", "--porcelain"); status != "" {
		t.Errorf("the checkout isn't clean:\n%s", status)
	}
	data, _ := os.ReadFile(filepath.Join(h.repoDir(0), "deps.txt"))
	if !strings.Contains(string(data), "requires "+h.name("kit")+" "+h.bumpedVersion()+"\n") {
		t.Errorf("deps.txt = %q", data)
	}
	message := h.gitIn(0, "log", "-1", "--format=%s%n%an%n%b", head)
	want := "Bump the fake package " + h.name("kit") + " to " + h.kitCommit[:12] + "\ndeployment.io\n" + commitTrailerTag +
		"\nTask: " + startUpTaskTitle + "\nTask-URL: https://app.deployment.io/tasks/task-1"
	if message != want {
		t.Errorf("commit =\n%s\nwant\n%s", message, want)
	}
	out := h.jobOutput()
	if len(out.BaseCommits) != 1 || out.BaseCommits[0].CommitSHA != head {
		t.Errorf("base commits %+v, want %s", out.BaseCommits, head)
	}
	if len(out.StartUpBumps) != 1 {
		t.Fatalf("start-up bumps %+v", out.StartUpBumps)
	}
	bumped := h.kitBump(0, tasks.DependencyBumpStateBumped)
	bumped.ToVersion, bumped.CommitSHA = h.bumpedVersion(), head
	h.wantBump(out.StartUpBumps[0], bumped)
	h.wantPrompt(h.bumpedLine("acme/app"))
	for _, word := range []string{"push", "pull request", "commit it", "open a"} {
		if strings.Contains(strings.TrimPrefix(h.prompt(), startUpStepPrompt), word) {
			t.Errorf("the prompt's block says %q", word)
		}
	}
	info, err := os.Lstat(filepath.Join(h.repoDir(0), "deps.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != commandUtils.AgentboxUID || st.Gid != commandUtils.AgentboxGID {
		t.Errorf("deps.txt is owned by %d:%d", st.Uid, st.Gid)
	}
	if !equalStrings(h.docker.created, []string{"dependency-bump-cache-" + bumpJobID + "-0"}) {
		t.Errorf("volumes %v", h.docker.created)
	}
	if !strings.Contains(h.log.String(), "Dependency bump: acme/app: the fake package "+h.name("kit")+" v0.9.0 → "+h.bumpedVersion()) {
		t.Errorf("log:\n%s", h.log.String())
	}
}

// A pin that already contains the merged commit is already current: nothing
// is committed and the agent isn't told.
func TestStartUpBumpPinAlreadyContainsTheMergedCommit(t *testing.T) {
	h := newStartUpHarness(t)
	pinned := "c-" + h.laterKitCommit()[:12]
	h.pinKit(pinned)
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	h.wantUnmoved(0)
	h.wantPrompt()
	current := h.kitBump(0, tasks.DependencyBumpStateAlreadyCurrent)
	current.FromVersion, current.ToVersion = pinned, pinned
	out := h.jobOutput()
	if len(out.StartUpBumps) != 1 {
		t.Fatalf("start-up bumps %+v", out.StartUpBumps)
	}
	h.wantBump(out.StartUpBumps[0], current)
	if len(h.docker.configs) != 0 {
		t.Errorf("containers ran: %v", h.docker.commands)
	}
	if out.BaseCommits[0].CommitSHA != h.stepTips[0] {
		t.Errorf("base commit %s, want %s", out.BaseCommits[0].CommitSHA, h.stepTips[0])
	}
}

// A repository requiring nothing a source provides is left alone.
func TestStartUpBumpLeavesARepositoryRequiringNothing(t *testing.T) {
	h := newStartUpHarness(t)
	priv := h.kitSource()
	priv.Repository, priv.Provides = h.entry("priv"), []tasks.StartUpBumpProvidedV1{{Ecosystem: "fake", Name: h.name("priv")}}
	if err := h.run([]tasks.StartUpBumpSourceV1{priv}); err != nil {
		t.Fatalf("err = %v", err)
	}
	h.wantUnmoved(0)
	h.wantPrompt()
	if out := h.jobOutput(); len(out.StartUpBumps) != 0 {
		t.Errorf("start-up bumps %+v", out.StartUpBumps)
	}
	if len(h.docker.configs) != 0 || len(h.docker.created) != 0 {
		t.Errorf("containers ran: %v", h.docker.commands)
	}
}

// A bump failure commits nothing, leaves the checkout as it was, tells the
// agent, and the Step goes on.
func TestStartUpBumpBumpFailure(t *testing.T) {
	h := newStartUpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-bump" {
			return 2, "can't reach the module\n"
		}
		return 0, ""
	}
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v", err)
	}
	h.wantUnmoved(0)
	failed := h.kitBump(0, tasks.DependencyBumpStateFailed)
	failed.Error = "the fake bump failed: fake-bump"
	out := h.jobOutput()
	if len(out.StartUpBumps) != 1 {
		t.Fatalf("start-up bumps %+v", out.StartUpBumps)
	}
	h.wantBump(out.StartUpBumps[0], failed)
	h.wantPrompt(fmt.Sprintf("- acme/app: couldn't bump the fake package %s to acme/kit's merged commit %s: the fake bump failed: fake-bump %s exited with code 2\n    can't reach the module. It still pins v0.9.0.",
		h.name("kit"), h.kitCommit[:12], h.name("kit")+"@"+h.kitCommit))
	if out.BaseCommits[0].CommitSHA != h.stepTips[0] {
		t.Errorf("base commit %s, want %s", out.BaseCommits[0].CommitSHA, h.stepTips[0])
	}
}

// A verify failure keeps the bump, committed, and tells the agent the build's
// last lines.
func TestStartUpBumpVerifyFailure(t *testing.T) {
	h := newStartUpHarness(t)
	h.docker.run = func(src string, cmd []string) (int, string) {
		if cmd[0] == "fake-verify" {
			return 1, "compile error: undefined: kit.New\n"
		}
		return applyFakeBump(src, cmd)
	}
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v", err)
	}
	head := h.head(0)
	if h.gitIn(0, "rev-parse", head+"^") != h.stepTips[0] {
		t.Fatalf("the bump isn't committed on the checkout\nlog:\n%s", h.log.String())
	}
	bumped := h.kitBump(0, tasks.DependencyBumpStateBumped)
	bumped.ToVersion, bumped.CommitSHA = h.bumpedVersion(), head
	bumped.BuildError = "acme/app doesn't build after the bump: fake-verify exited with code 1\ncompile error: undefined: kit.New"
	out := h.jobOutput()
	if len(out.StartUpBumps) != 1 {
		t.Fatalf("start-up bumps %+v", out.StartUpBumps)
	}
	h.wantBump(out.StartUpBumps[0], bumped)
	h.wantPrompt(fmt.Sprintf("- acme/app: bumped the fake package %s from v0.9.0 to %s, the merged commit %s of acme/kit from the Task %q, but acme/app doesn't build after it:\n"+
		"    acme/app doesn't build after the bump: fake-verify exited with code 1\n    compile error: undefined: kit.New",
		h.name("kit"), h.bumpedVersion(), h.kitCommit[:12], startUpKitTitle))
}

// A source under another owner, with a malformed commit or of an ecosystem
// this runner doesn't support fails on its own; the others are bumped.
func TestStartUpBumpFailsUnbumpableSourcesAlone(t *testing.T) {
	h := newStartUpHarness(t)
	otherOwner := h.kitSource()
	otherOwner.TaskID, otherOwner.Repository = "task-b", tasks.RepositoryEntry{Name: "other/kit", CloneURL: h.host.URL + "/other/kit", BaseBranch: "main", InstallationID: "inst-2"}
	malformed := h.kitSource()
	malformed.TaskID, malformed.Commit = "task-c", strings.ToUpper(h.kitCommit)
	unknown := h.kitSource()
	unknown.TaskID, unknown.Provides = "task-d", []tasks.StartUpBumpProvidedV1{{Ecosystem: "npm", Name: h.name("kit")}}
	if err := h.run([]tasks.StartUpBumpSourceV1{otherOwner, malformed, unknown, h.kitSource()}); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := h.jobOutput()
	if len(out.StartUpBumps) != 4 {
		t.Fatalf("start-up bumps %+v\nlog:\n%s", out.StartUpBumps, h.log.String())
	}
	head := h.head(0)
	for i, want := range []struct{ task, repo, commit, eco, err string }{
		{"task-b", "other/kit", h.kitCommit, "fake", "other/kit isn't under the same owner as acme/app; bumps across owners aren't supported yet"},
		{"task-c", "acme/kit", strings.ToUpper(h.kitCommit), "fake", "the merged commit isn't a commit id"},
		{"task-d", "acme/kit", h.kitCommit, "npm", "this runner doesn't support the npm ecosystem"},
	} {
		failed := startUpBumpOutput{Repository: 0, Ecosystem: want.eco, Name: h.name("kit"), SourceTaskID: want.task, SourceRepository: want.repo, Commit: want.commit, State: tasks.DependencyBumpStateFailed, FromVersion: "v0.9.0", Error: want.err}
		h.wantBump(out.StartUpBumps[i], failed)
	}
	bumped := h.kitBump(0, tasks.DependencyBumpStateBumped)
	bumped.ToVersion, bumped.CommitSHA = h.bumpedVersion(), head
	h.wantBump(out.StartUpBumps[3], bumped)
	kit := "the fake package " + h.name("kit")
	h.wantPrompt(
		"- acme/app: couldn't bump "+kit+" to other/kit's merged commit "+h.kitCommit[:12]+": other/kit isn't under the same owner as acme/app; bumps across owners aren't supported yet. It still pins v0.9.0.",
		"- acme/app: couldn't bump "+kit+" to acme/kit's merged commit "+strings.ToUpper(h.kitCommit)[:12]+": the merged commit isn't a commit id. It still pins v0.9.0.",
		"- acme/app: couldn't bump "+h.name("kit")+" to acme/kit's merged commit "+h.kitCommit[:12]+": this runner doesn't support the npm ecosystem. It still pins v0.9.0.",
		h.bumpedLine("acme/app"),
	)
}

// Two repositories bumped in one Step have their own work directories,
// volumes and commits.
func TestStartUpBumpTwoRepositories(t *testing.T) {
	h := newStartUpHarness(t)
	h.host.AddRepo("acme/web")
	h.host.Commit("acme/web", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " v0.8.0\n"})
	h.entries = append(h.entries, h.entry("web"))
	var srcs []string
	h.docker.atBump = func(host *container.HostConfig) { srcs = append(srcs, mountSource(host, "/src")) }
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	out := h.jobOutput()
	if len(out.StartUpBumps) != 2 {
		t.Fatalf("start-up bumps %+v", out.StartUpBumps)
	}
	for idx := range h.entries {
		head := h.head(idx)
		if h.gitIn(idx, "rev-parse", head+"^") != h.stepTips[idx] {
			t.Errorf("%s isn't bumped", h.entries[idx].Name)
		}
		if out.StartUpBumps[idx].Repository != idx || out.StartUpBumps[idx].CommitSHA != head || out.BaseCommits[idx].CommitSHA != head {
			t.Errorf("repository %d: bump %+v, base %+v, head %s", idx, out.StartUpBumps[idx], out.BaseCommits[idx], head)
		}
	}
	if h.head(0) == h.head(1) {
		t.Error("the two repositories share a commit")
	}
	wantVolumes := []string{"dependency-bump-cache-" + bumpJobID + "-0", "dependency-bump-cache-" + bumpJobID + "-1"}
	if !equalStrings(h.docker.created, wantVolumes) {
		t.Errorf("volumes %v, want %v", h.docker.created, wantVolumes)
	}
	wantSrcs := []string{filepath.Join(h.workRoot, "dependency-bump-"+bumpJobID+"-0", "repo"), filepath.Join(h.workRoot, "dependency-bump-"+bumpJobID+"-1", "repo")}
	if !equalStrings(srcs, wantSrcs) {
		t.Errorf("bumped in %v, want %v", srcs, wantSrcs)
	}
	web := fmt.Sprintf("- acme/web: bumped the fake package %s from v0.8.0 to %s, the merged commit %s of acme/kit from the Task %q. It is already committed on the branch.",
		h.name("kit"), h.bumpedVersion(), h.kitCommit[:12], startUpKitTitle)
	h.wantPrompt(h.bumpedLine("acme/app"), web)
}

// Without the parameter the checkout is what it always was.
func TestStartUpBumpWithoutTheParameter(t *testing.T) {
	for name, sources := range map[string][]tasks.StartUpBumpSourceV1{"absent": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			h := newStartUpHarness(t)
			if err := h.run(sources); err != nil {
				t.Fatalf("err = %v", err)
			}
			h.wantUnmoved(0)
			h.wantPrompt()
			raw, _ := jobs.GetParameterValue[string](h.params, parameters_enums.JobOutput)
			if strings.Contains(raw, "start_up_bumps") {
				t.Errorf("JobOutput %s", raw)
			}
			if len(h.eco.read) != 0 || len(h.docker.configs) != 0 || strings.Contains(h.log.String(), "Dependency bump") {
				t.Errorf("read %v, ran %v\nlog:\n%s", h.eco.read, h.docker.commands, h.log.String())
			}
		})
	}
}

// CommitAndPush with the bump commit and no change of the agent's pushes the
// bump.
func TestStartUpBumpIsPushedByCommitAndPush(t *testing.T) {
	h := newStartUpHarness(t)
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v", err)
	}
	head := h.head(0)
	ctx, err := commandUtils.ParseTaskJobContext(h.params)
	if err != nil {
		t.Fatal(err)
	}
	tcp := &taskCommitPush{ctx: ctx, tokenCache: map[string]string{}, logsWriter: &h.log}
	out, err := tcp.commitAndPushOne(h.repoDir(0), h.entries[0], 0)
	if err != nil {
		t.Fatalf("commitAndPushOne: %v\nlog:\n%s", err, h.log.String())
	}
	if !out.HasChanges || out.CommitSHA != head || h.branchTip() != head {
		t.Errorf("output %+v, remote tip %s, want %s", out, h.branchTip(), head)
	}
}

// A repository with a submodule is bumped in a clone whose submodule is
// checked out as the Step's is, without the network.
func TestStartUpBumpChecksOutSubmodulesInTheClone(t *testing.T) {
	h := newStartUpHarness(t)
	lib := h.host.AddRepo("acme/lib")
	h.host.AddRepo("acme/web", gittest.Submodule{Path: "lib", URL: "../lib", Commit: lib})
	h.host.Commit("acme/web", "main", map[string]string{"deps.txt": "requires " + h.name("kit") + " v0.8.0\n"})
	h.entries = []tasks.RepositoryEntry{h.entry("web")}
	var seen []string
	h.docker.run = func(src string, cmd []string) (int, string) {
		if data, err := os.ReadFile(filepath.Join(src, "lib", "README")); err == nil {
			seen = append(seen, cmd[0]+": "+strings.TrimSpace(string(data)))
		}
		return applyFakeBump(src, cmd)
	}
	if err := h.run([]tasks.StartUpBumpSourceV1{h.kitSource()}); err != nil {
		t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
	}
	if want := []string{"fake-bump: acme/lib", "fake-verify: acme/lib"}; !equalStrings(seen, want) {
		t.Errorf("the containers saw the submodule as %v, want %v\nlog:\n%s", seen, want, h.log.String())
	}
	head := h.head(0)
	if h.gitIn(0, "rev-parse", head+"^") != h.stepTips[0] {
		t.Fatalf("the bump isn't committed on the checkout\nlog:\n%s", h.log.String())
	}
	if files := h.gitIn(0, "diff", "--name-only", head+"^", head); files != "deps.txt" {
		t.Errorf("the bump commit changed %q", files)
	}
	if got := h.gitIn(0, "rev-parse", head+":lib"); got != lib {
		t.Errorf("the bump commit's gitlink is %s, want %s", got, lib)
	}
}

// Two upstream Tasks that merged the same library at a commit and at a
// descendant of it: the descendant is bumped whichever source comes first,
// and the other target is already current through it, with no prompt line.
func TestStartUpBumpPicksTheDescendantOfTwoSources(t *testing.T) {
	for _, laterFirst := range []bool{false, true} {
		h := newStartUpHarness(t)
		earlier := h.kitSource()
		later := h.kitSource()
		later.TaskID, later.TaskTitle, later.Commit = "task-b", "Change the kit again", h.laterKitCommit()
		sources := []tasks.StartUpBumpSourceV1{earlier, later}
		if laterFirst {
			sources = []tasks.StartUpBumpSourceV1{later, earlier}
		}
		if err := h.run(sources); err != nil {
			t.Fatalf("err = %v\nlog:\n%s", err, h.log.String())
		}
		head := h.head(0)
		if h.gitIn(0, "rev-parse", head+"^") != h.stepTips[0] {
			t.Fatalf("HEAD %s isn't one commit on the checkout\nlog:\n%s", head, h.log.String())
		}
		to := "c-" + later.Commit[:12]
		data, _ := os.ReadFile(filepath.Join(h.repoDir(0), "deps.txt"))
		if !strings.Contains(string(data), "requires "+h.name("kit")+" "+to+"\n") {
			t.Errorf("deps.txt = %q", data)
		}
		out := h.jobOutput()
		if len(out.StartUpBumps) != 2 {
			t.Fatalf("start-up bumps %+v", out.StartUpBumps)
		}
		for _, got := range out.StartUpBumps {
			want := h.kitBump(0, tasks.DependencyBumpStateAlreadyCurrent)
			want.ToVersion = to
			if got.Commit == later.Commit {
				want.SourceTaskID, want.Commit, want.State, want.CommitSHA = "task-b", later.Commit, tasks.DependencyBumpStateBumped, head
			}
			h.wantBump(got, want)
		}
		line := fmt.Sprintf("- acme/app: bumped the fake package %s from v0.9.0 to %s, the merged commit %s of acme/kit from the Task %q. It is already committed on the branch.",
			h.name("kit"), to, later.Commit[:12], later.TaskTitle)
		h.wantPrompt(line)
	}
}

// A build's error is bounded keeping its first line and its last lines.
func TestTruncateKeepingTail(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("acme/app doesn't build after the bump: go exited with code 1\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "./x.go:%d: undefined: kit.Old\n", i)
	}
	sb.WriteString("./main.go:9: the last error é")
	got := truncateKeepingTail(sb.String(), 1024, "\n[...]\n")
	if len(got) > 1024 || !strings.HasPrefix(got, "acme/app doesn't build after the bump: go exited with code 1\n[...]\n") || !strings.HasSuffix(got, "./main.go:9: the last error é") {
		t.Errorf("truncated to %d bytes:\n%s", len(got), got)
	}
	if got := truncateKeepingTail("short", 1024, "[...]"); got != "short" {
		t.Errorf("a short error became %q", got)
	}
}

// The prompt's block stays bounded however many targets carry a build's
// output, and keeps every line's sentence and its last lines.
func TestBoundPromptLines(t *testing.T) {
	build := strings.Repeat("    ./x.go:1: undefined: kit.Old\n", 130) + "    ./main.go:9: the last error"
	var lines []string
	for i := 0; i < 64; i++ {
		lines = append(lines, fmt.Sprintf("- acme/app-%d: bumped kit from v1 to v2, but acme/app-%d doesn't build after it:\n%s", i, i, build))
	}
	lines = append(lines, "- acme/app: couldn't bump kit: no.")
	bounded := boundPromptLines(lines, startUpBumpPromptMaxBytes)
	total := 0
	for i, l := range bounded {
		total += len(l) + 1
		if i < 64 && (!strings.HasPrefix(l, fmt.Sprintf("- acme/app-%d: bumped kit", i)) || !strings.HasSuffix(l, "the last error")) {
			t.Errorf("line %d lost its sentence or its end:\n%s", i, l)
		}
	}
	if total > startUpBumpPromptMaxBytes {
		t.Errorf("the block is %d bytes, want at most %d", total, startUpBumpPromptMaxBytes)
	}
	if bounded[64] != lines[64] {
		t.Errorf("a short line was shortened: %q", bounded[64])
	}
	if got := boundPromptLines(lines[64:], startUpBumpPromptMaxBytes); got[0] != lines[64] {
		t.Errorf("a block that fits changed: %v", got)
	}
}

// A shortened line keeps its whole first line, the target's sentence, when
// it fits in the line's share, and the build's last lines get the rest.
func TestBoundPromptLinesKeepsTheSentence(t *testing.T) {
	sentence := "- acme/app-server: bumped the Go module github.com/acme/kit from v0.0.0-20261120101010-0123456789ab to v0.0.0-20261126101010-abcdef012345, the merged commit abcdef012345 of acme/kit from the Task \"Change the kit\", but acme/app-server doesn't build after it:"
	build := strings.Repeat("    ./x.go:1: undefined: kit.Old\n", 130) + "    ./main.go:9: the last error"
	var lines []string
	for i := 0; i < 64; i++ {
		lines = append(lines, sentence+"\n"+build)
	}
	for i, l := range boundPromptLines(lines, startUpBumpPromptMaxBytes) {
		if !strings.HasPrefix(l, sentence+"\n") || !strings.HasSuffix(l, "the last error") {
			t.Fatalf("line %d lost its sentence or its end:\n%s", i, l)
		}
	}
}
