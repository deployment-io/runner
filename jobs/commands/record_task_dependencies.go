package commands

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/tasks"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/jobs/commands/dependencies"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// recordTaskDependenciesRPC and recordTaskDependenciesRetryWait are how the
// graph is sent; tests replace them.
var (
	recordTaskDependenciesRPC = func(organizationID string, dto tasks.TaskDependenciesDtoV1) error {
		return client.Get().RecordTaskDependencies(organizationID, dto)
	}
	recordTaskDependenciesRetryWait = 5 * time.Second
)

const recordTaskDependenciesAttempts = 3

// taskRepositoryManifest is one Task repository's manifest in one ecosystem.
type taskRepositoryManifest struct {
	repository int
	ecosystem  dependencies.Ecosystem
	manifest   dependencies.Manifest
}

// recordTaskDependencies reads the dependency graph between the Task's
// repositories after a Step (every registered ecosystem's manifest of each
// repository) and sends it to deployment-server, which keeps it only when
// this Job is the Task's latest run. Best-effort: nothing here fails the Step.
func recordTaskDependencies(parameters map[string]interface{}, ctx commandUtils.TaskJobContext, logsWriter io.Writer) {
	jobID, _ := jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
	manifests := readTaskManifests(ctx, logsWriter)
	dto := tasks.TaskDependenciesDtoV1{TaskID: ctx.TaskID, JobID: jobID}
	dto.Provided = taskDependenciesProvided(manifests)
	dto.Required = taskDependenciesRequired(manifests, logsWriter)
	if n := len(dto.Provided) - tasks.MaxTaskDependencies; n > 0 {
		io.WriteString(logsWriter, fmt.Sprintf("Dependencies: %d provided modules or packages over the limit of %d not tracked\n", n, tasks.MaxTaskDependencies))
		dto.Provided = dto.Provided[:tasks.MaxTaskDependencies]
	}
	if n := len(dto.Required) - tasks.MaxTaskDependencies; n > 0 {
		io.WriteString(logsWriter, fmt.Sprintf("Dependencies: %d dependencies over the limit of %d not tracked\n", n, tasks.MaxTaskDependencies))
		dto.Required = dto.Required[:tasks.MaxTaskDependencies]
	}
	if len(dto.Required) == 0 {
		io.WriteString(logsWriter, "Dependencies: none between this Task's repositories\n")
	}
	for _, r := range dto.Required {
		io.WriteString(logsWriter, fmt.Sprintf("Dependencies: %s requires %s from %s\n", ctx.Entries[r.Repository].Name, r.Description, ctx.Entries[r.ProvidedBy].Name))
	}
	var err error
	for attempt := 1; attempt <= recordTaskDependenciesAttempts; attempt++ {
		if err = recordTaskDependenciesRPC(ctx.OrganizationID, dto); err == nil {
			return
		}
		if attempt < recordTaskDependenciesAttempts {
			time.Sleep(recordTaskDependenciesRetryWait)
		}
	}
	io.WriteString(logsWriter, fmt.Sprintf("Dependencies: couldn't record this Task's dependencies: %s\n", err))
}

// readTaskManifests is every Task repository's manifest in every registered
// ecosystem it is in. A repository whose manifest can't be read is skipped
// and logged.
func readTaskManifests(ctx commandUtils.TaskJobContext, logsWriter io.Writer) []taskRepositoryManifest {
	var out []taskRepositoryManifest
	for idx, entry := range ctx.Entries {
		dir := commandUtils.GetTaskRepositoryDir(ctx.OrganizationID, ctx.TaskID, idx, entry.Name)
		var found []taskRepositoryManifest
		var readErr error
		for _, eco := range dependencies.Registered() {
			m, ok, err := eco.ReadManifest(dir)
			if err != nil {
				readErr = err
				break
			}
			if ok {
				found = append(found, taskRepositoryManifest{repository: idx, ecosystem: eco, manifest: m})
			}
		}
		if readErr != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Dependencies: couldn't read %s's manifest (%s); not tracking it\n", entry.Name, readErr))
			continue
		}
		out = append(out, found...)
	}
	return out
}

// validDependencyName is a name deployment-server accepts.
func validDependencyName(name string) bool {
	return name != "" && len(name) <= tasks.DependencyNameMaxBytes
}

func taskDependenciesProvided(manifests []taskRepositoryManifest) []tasks.DependencyProvidedV1 {
	var out []tasks.DependencyProvidedV1
	for _, m := range manifests {
		for _, name := range m.manifest.Provides {
			if validDependencyName(name) {
				out = append(out, tasks.DependencyProvidedV1{Repository: m.repository, Ecosystem: m.ecosystem.Name(), Name: name})
			}
		}
	}
	return out
}

// taskDependenciesRequired is every require of a Task repository that exactly
// one other Task repository provides in the same ecosystem. A name more than
// one other repository provides has no right provider, and isn't tracked.
func taskDependenciesRequired(manifests []taskRepositoryManifest, logsWriter io.Writer) []tasks.DependencyRequiredV1 {
	providers := map[[2]string][]int{}
	for _, m := range manifests {
		for _, name := range m.manifest.Provides {
			key := [2]string{m.ecosystem.Name(), name}
			providers[key] = append(providers[key], m.repository)
		}
	}
	var out []tasks.DependencyRequiredV1
	ambiguous := map[[2]string]bool{}
	for _, m := range manifests {
		names := make([]string, 0, len(m.manifest.Requires))
		for name := range m.manifest.Requires {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			key := [2]string{m.ecosystem.Name(), name}
			var others []int
			for _, p := range providers[key] {
				if p != m.repository {
					others = append(others, p)
				}
			}
			version := m.manifest.Requires[name]
			switch {
			case len(others) == 0 || !validDependencyName(name) || len(version) > tasks.DependencyVersionMaxBytes:
				continue
			case len(others) > 1:
				if !ambiguous[key] {
					ambiguous[key] = true
					io.WriteString(logsWriter, fmt.Sprintf("Dependencies: %s is provided by more than one repository; not tracked\n", name))
				}
				continue
			}
			out = append(out, tasks.DependencyRequiredV1{
				Repository:  m.repository,
				Ecosystem:   m.ecosystem.Name(),
				Name:        name,
				Description: truncateUTF8(m.ecosystem.Describe(name), tasks.DependencyDescriptionMaxBytes),
				ProvidedBy:  others[0],
				Version:     version,
			})
		}
	}
	return out
}
