package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// graphTask is a Task whose repositories are checked out where the Step's
// are (commandUtils.GetTaskRepositoryDir), each a git repository holding
// files, committed on the Task branch.
func graphTask(t *testing.T, repos ...map[string]string) commandUtils.TaskJobContext {
	t.Helper()
	ctx := commandUtils.TaskJobContext{
		OrganizationID: fmt.Sprintf("org-graph-%d", time.Now().UnixNano()),
		TaskID:         "task-graph",
		TaskTitle:      "Graph",
		BranchName:     "deployment-io/task-graph",
	}
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(commandUtils.GetTaskRepositoriesBaseDir(ctx.OrganizationID, ctx.TaskID)))
	})
	for idx, files := range repos {
		entry := tasks.RepositoryEntry{Name: fmt.Sprintf("repo%d", idx), CloneURL: "https://github.com/acme/repo", BaseBranch: "main"}
		ctx.Entries = append(ctx.Entries, entry)
		dir := commandUtils.GetTaskRepositoryDir(ctx.OrganizationID, ctx.TaskID, idx, entry.Name)
		repository, err := git.PlainInit(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		worktree, _ := repository.Worktree()
		_ = worktree.AddGlob(".")
		if _, err := worktree.Commit("initial", &git.CommitOptions{AllowEmptyCommits: true, Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}}); err != nil {
			t.Fatal(err)
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Create: true, Branch: plumbing.NewBranchReferenceName(ctx.BranchName)}); err != nil {
			t.Fatal(err)
		}
	}
	return ctx
}

// recordGraph records ctx's graph, and returns what was sent and the log.
func recordGraph(t *testing.T, ctx commandUtils.TaskJobContext) (tasks.TaskDependenciesDtoV1, string) {
	t.Helper()
	var sent []tasks.TaskDependenciesDtoV1
	swapRecordTaskDependencies(t, func(org string, dto tasks.TaskDependenciesDtoV1) error {
		if org != ctx.OrganizationID {
			t.Errorf("organization %q", org)
		}
		sent = append(sent, dto)
		return nil
	})
	params := map[string]interface{}{}
	_ = jobs.SetParameterValue(params, parameters_enums.JobID, "step-job-1")
	var log strings.Builder
	recordTaskDependencies(params, ctx, &log)
	if len(sent) != 1 {
		t.Fatalf("sent %d graphs", len(sent))
	}
	if sent[0].TaskID != ctx.TaskID || sent[0].JobID != "step-job-1" {
		t.Errorf("graph ids %q %q", sent[0].TaskID, sent[0].JobID)
	}
	return sent[0], log.String()
}

func swapRecordTaskDependencies(t *testing.T, rpc func(string, tasks.TaskDependenciesDtoV1) error) {
	origRPC, origWait := recordTaskDependenciesRPC, recordTaskDependenciesRetryWait
	recordTaskDependenciesRPC, recordTaskDependenciesRetryWait = rpc, 0
	t.Cleanup(func() { recordTaskDependenciesRPC, recordTaskDependenciesRetryWait = origRPC, origWait })
}

const graphKitGoMod = "module github.com/acme/kit\n\ngo 1.27\n"

func TestRecordTaskDependenciesBetweenTwoRepositories(t *testing.T) {
	ctx := graphTask(t,
		map[string]string{"go.mod": graphKitGoMod},
		map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27\n\nrequire (\n\tgithub.com/acme/kit v1.4.0\n\tgithub.com/aws/aws-sdk-go-v2 v1.30.0\n)\n"},
	)
	graph, log := recordGraph(t, ctx)
	wantProvided := []tasks.DependencyProvidedV1{{Repository: 0, Ecosystem: "go", Name: "github.com/acme/kit"}, {Repository: 1, Ecosystem: "go", Name: "github.com/acme/app"}}
	if !reflect.DeepEqual(graph.Provided, wantProvided) {
		t.Errorf("provided = %+v", graph.Provided)
	}
	// The public require isn't sent: no Task repository provides it.
	wantRequired := []tasks.DependencyRequiredV1{{Repository: 1, Ecosystem: "go", Name: "github.com/acme/kit", Description: "the Go module github.com/acme/kit", ProvidedBy: 0, Version: "v1.4.0"}}
	if !reflect.DeepEqual(graph.Required, wantRequired) {
		t.Errorf("required = %+v", graph.Required)
	}
	if !strings.Contains(log, "Dependencies: repo1 requires the Go module github.com/acme/kit from repo0\n") {
		t.Errorf("log:\n%s", log)
	}
}

func TestRecordTaskDependenciesBetweenThreeRepositories(t *testing.T) {
	ctx := graphTask(t,
		map[string]string{"go.mod": graphKitGoMod},
		map[string]string{"go.mod": "module github.com/acme/lib\n\ngo 1.27\n\nrequire github.com/acme/kit v1.0.0\n"},
		map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27\n\nrequire github.com/acme/kit v1.1.0\nrequire github.com/acme/lib v0.2.0 // indirect\n"},
	)
	graph, _ := recordGraph(t, ctx)
	want := []tasks.DependencyRequiredV1{
		{Repository: 1, Ecosystem: "go", Name: "github.com/acme/kit", Description: "the Go module github.com/acme/kit", ProvidedBy: 0, Version: "v1.0.0"},
		{Repository: 2, Ecosystem: "go", Name: "github.com/acme/kit", Description: "the Go module github.com/acme/kit", ProvidedBy: 0, Version: "v1.1.0"},
		{Repository: 2, Ecosystem: "go", Name: "github.com/acme/lib", Description: "the Go module github.com/acme/lib", ProvidedBy: 1, Version: "v0.2.0"},
	}
	if !reflect.DeepEqual(graph.Required, want) {
		t.Errorf("required = %+v", graph.Required)
	}
	if len(graph.Provided) != 3 {
		t.Errorf("provided = %+v", graph.Provided)
	}
}

// A name two other repositories provide has no right provider: it isn't sent,
// and the log says so. Both are still sent as provided.
func TestRecordTaskDependenciesNameProvidedTwice(t *testing.T) {
	ctx := graphTask(t,
		map[string]string{"go.mod": graphKitGoMod},
		map[string]string{"go.mod": graphKitGoMod},
		map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27\n\nrequire github.com/acme/kit v1.4.0\n"},
	)
	graph, log := recordGraph(t, ctx)
	if len(graph.Required) != 0 {
		t.Errorf("required = %+v", graph.Required)
	}
	if len(graph.Provided) != 3 {
		t.Errorf("provided = %+v", graph.Provided)
	}
	if !strings.Contains(log, "Dependencies: github.com/acme/kit is provided by more than one repository; not tracked\n") || !strings.Contains(log, "Dependencies: none between this Task's repositories\n") {
		t.Errorf("log:\n%s", log)
	}
}

// A module the dependent replaces isn't a dependency to bump.
func TestRecordTaskDependenciesReplacedModule(t *testing.T) {
	ctx := graphTask(t,
		map[string]string{"go.mod": graphKitGoMod},
		map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27\n\nrequire github.com/acme/kit v1.4.0\n\nreplace github.com/acme/kit => ../kit\n"},
	)
	graph, _ := recordGraph(t, ctx)
	if len(graph.Required) != 0 || len(graph.Provided) != 2 {
		t.Errorf("graph = %+v", graph)
	}
}

// A repository without a go.mod provides and requires nothing; a repository
// whose go.mod can't be read is skipped and logged.
func TestRecordTaskDependenciesWithoutAGoMod(t *testing.T) {
	ctx := graphTask(t,
		map[string]string{"README.md": "docs\n"},
		map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27\n"},
		map[string]string{},
	)
	broken := filepath.Join(commandUtils.GetTaskRepositoryDir(ctx.OrganizationID, ctx.TaskID, 2, "repo2"), "go.mod")
	if err := os.Symlink("/etc/hostname", broken); err != nil {
		t.Fatal(err)
	}
	graph, log := recordGraph(t, ctx)
	if want := []tasks.DependencyProvidedV1{{Repository: 1, Ecosystem: "go", Name: "github.com/acme/app"}}; !reflect.DeepEqual(graph.Provided, want) || len(graph.Required) != 0 {
		t.Errorf("graph = %+v", graph)
	}
	if !strings.Contains(log, "Dependencies: couldn't read repo2's manifest") {
		t.Errorf("log:\n%s", log)
	}
}

// CommitAndPush records the graph at its end, with the Step Job's id, and a
// graph that can't be sent never fails the Step.
func TestCommitAndPushRecordsTheGraphBestEffort(t *testing.T) {
	ctx := graphTask(t,
		map[string]string{"go.mod": graphKitGoMod},
		map[string]string{"go.mod": "module github.com/acme/app\n\ngo 1.27\n\nrequire github.com/acme/kit v1.4.0\n"},
	)
	var sent []tasks.TaskDependenciesDtoV1
	swapRecordTaskDependencies(t, func(org string, dto tasks.TaskDependenciesDtoV1) error {
		sent = append(sent, dto)
		return errors.New("deployment.io is away")
	})
	params := map[string]interface{}{}
	_ = jobs.SetParameterValue(params, parameters_enums.OrganizationIDNamespace, ctx.OrganizationID)
	_ = jobs.SetParameterValue(params, parameters_enums.TaskID, ctx.TaskID)
	_ = jobs.SetParameterValue(params, parameters_enums.TaskTitle, ctx.TaskTitle)
	_ = jobs.SetParameterValue(params, parameters_enums.StepIndex, int64(0))
	_ = jobs.SetParameterValue(params, parameters_enums.TaskBranchName, ctx.BranchName)
	_ = jobs.SetParameterValue(params, parameters_enums.Repositories, mustJSON(t, ctx.Entries))
	_ = jobs.SetParameterValue(params, parameters_enums.JobID, "step-job-2")
	var log strings.Builder
	if _, err := (&CommitAndPush{}).Run(params, &log); err != nil {
		t.Fatalf("CommitAndPush failed: %v\n%s", err, log.String())
	}
	if len(sent) != recordTaskDependenciesAttempts {
		t.Errorf("sent %d times, want %d", len(sent), recordTaskDependenciesAttempts)
	}
	if len(sent) > 0 && (sent[0].JobID != "step-job-2" || len(sent[0].Required) != 1) {
		t.Errorf("graph = %+v", sent[0])
	}
	if !strings.Contains(log.String(), "Dependencies: couldn't record this Task's dependencies: deployment.io is away") {
		t.Errorf("log:\n%s", log.String())
	}
}

// At most tasks.MaxTaskDependencies of each are sent; the rest are logged.
func TestRecordTaskDependenciesCapsTheGraph(t *testing.T) {
	var app strings.Builder
	repos := []map[string]string{}
	app.WriteString("module github.com/acme/app\n\ngo 1.27\n\nrequire (\n")
	for i := 0; i < tasks.MaxTaskDependencies+1; i++ {
		repos = append(repos, map[string]string{"go.mod": fmt.Sprintf("module github.com/acme/m%d\n\ngo 1.27\n", i)})
		fmt.Fprintf(&app, "\tgithub.com/acme/m%d v1.0.0\n", i)
	}
	app.WriteString(")\n")
	repos = append(repos, map[string]string{"go.mod": app.String()})
	graph, log := recordGraph(t, graphTask(t, repos...))
	if len(graph.Provided) != tasks.MaxTaskDependencies || len(graph.Required) != tasks.MaxTaskDependencies {
		t.Errorf("%d provided, %d required", len(graph.Provided), len(graph.Required))
	}
	if !strings.Contains(log, "provided modules or packages over the limit") || !strings.Contains(log, "dependencies over the limit") {
		t.Errorf("log:\n%s", log)
	}
}
