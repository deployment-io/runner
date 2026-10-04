package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// The deploy readiness pass reads the org's deployment facts from
// /work/context/services.json. The implement run can write /work/context, so
// what it left there is not evidence the reviewer may rely on: the Review stage
// fetches the context again, writes it OUTSIDE the work dir and binds that copy
// read-only at /work/context in every review spawn. Fix runs never see the
// copy — they are implement runs and get the work dir as it is.

// reviewContextDirName is the directory under the work dir the copy is bound
// over; contextDirFor's last element.
const reviewContextDirName = "context"

// reviewContextCopyPattern names the copy's temporary directory: a sibling of
// the work dir, so it is never visible at /work and never part of a diff.
func reviewContextCopyPattern(workDirHost string) string {
	return filepath.Base(strings.TrimRight(workDirHost, "/")) + "-review-context-*"
}

// prepareContextCopy fetches the org's context and writes it into a fresh
// temporary directory beside the work dir, recording it in s.contextCopyDir.
// The returned function removes the copy; the caller defers it so the copy
// goes on every path out of the stage.
//
// Never fatal. Without a copy no extra mount is added, the deploy readiness
// pass reads whatever /work/context the Task left, and the deploy requirements
// resolve to "not found" — no link built from facts nobody fetched.
func (s *reviewStage) prepareContextCopy() func() {
	noop := func() {}
	unavailable := func(reason string) func() {
		io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: deployment context unavailable (%s) — the deploy readiness pass reads /work/context as the Task left it, and deploy requirements get no links\n", reason))
		return noop
	}
	if s.workDirHost == "" {
		return unavailable("no work directory")
	}
	// Never a catalog rebuild: the Step's own MaterializeContext already
	// asked for one when it was due.
	files, err := getContextClient().MaterializeContext(s.ctx.OrganizationID, nil, false)
	if err != nil {
		return unavailable(err.Error())
	}
	if len(files) == 0 {
		return unavailable("no context has been built")
	}
	base := strings.TrimRight(s.workDirHost, "/")
	dir, err := os.MkdirTemp(filepath.Dir(base), reviewContextCopyPattern(base))
	if err != nil {
		return unavailable(err.Error())
	}
	cleanup := func() {
		_ = os.RemoveAll(dir)
		s.contextCopyDir = ""
	}
	written, err := writeContextFiles(dir, files, s.logsWriter)
	if err != nil {
		cleanup()
		return unavailable(err.Error())
	}
	if written == 0 {
		cleanup()
		return unavailable("no context file could be written")
	}
	if err := ensureContextMountPoint(s.workDirHost, s.logsWriter); err != nil {
		cleanup()
		return unavailable(err.Error())
	}
	s.contextCopyDir = dir
	io.WriteString(s.logsWriter, fmt.Sprintf("Review stage: %d deployment context file(s) mounted read-only at /work/context for every review round\n", written))
	return cleanup
}

// ensureContextMountPoint makes <workDirHost>/context a real directory owned
// by the agentbox user, so Docker binds the copy onto it rather than creating
// the mount point as root. Something the implement run left in its place that
// is not a directory — a file, or a symlink that would carry the mount
// somewhere else — is removed first.
func ensureContextMountPoint(workDirHost string, logsWriter io.Writer) error {
	mountPoint := filepath.Join(workDirHost, reviewContextDirName)
	info, err := os.Lstat(mountPoint)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		if err := os.RemoveAll(mountPoint); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		return err
	}
	if err := os.Chown(mountPoint, commandUtils.AgentboxUID, commandUtils.AgentboxGID); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not chown context dir: %s\n", err))
	}
	return nil
}

// reviewDeployRequirement mirrors agentbox's review_result.deploy_requirements
// entry: a variable the deploy readiness pass found this change newly reads
// and the service's environment does not provide. NOT a finding — never sent
// back, never held, never counted, never an inline comment.
type reviewDeployRequirement struct {
	Variable    string `json:"variable"`
	Service     string `json:"service"`
	Environment string `json:"environment"`
	Location    string `json:"location"`
}

// resolvedDeployRequirement is a deploy requirement matched against the
// stage's own copy of services.json — what the pull request's "Before
// deploying" part renders. Every field but Variable, Service and Location
// comes from that copy, never from the reviewer.
type resolvedDeployRequirement struct {
	Variable      string `json:"variable"`
	Service       string `json:"service"`
	Environment   string `json:"environment,omitempty"`
	EnvironmentID string `json:"environment_id,omitempty"`
	Cluster       string `json:"cluster,omitempty"`
	// Managed is true when deployment.io deploys the service and the
	// environment is known, so the variable can be added on the dashboard.
	Managed bool `json:"managed,omitempty"`
	// Found is false when no row of services.json names the service.
	Found    bool   `json:"found"`
	Location string `json:"location,omitempty"`
}

// maxDeployRequirements bounds the resolved list, as agentbox bounds the raw
// one; it is also what bounds the pull request's "Before deploying" part.
const maxDeployRequirements = 20

// deployVariableName is a name a process environment can carry. agentbox
// already drops anything else; checked again here because the name goes into
// the pull request and into a link.
var deployVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// deployServiceRow is the part of a services.json row the resolution reads.
type deployServiceRow struct {
	Service       string   `json:"service"`
	Environment   string   `json:"environment"`
	EnvironmentID string   `json:"environmentId"`
	Cluster       string   `json:"cluster"`
	RepoSource    string   `json:"repoSource"`
	VariableNames []string `json:"variableNames"`
	VariablesFrom string   `json:"variablesFrom"`
}

// readDeployServiceRows reads the copy's services.json, one JSON object per
// line; a malformed line is skipped. No copy, or no file, is no rows.
func readDeployServiceRows(contextDir string) []deployServiceRow {
	if contextDir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(contextDir, "services.json"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []deployServiceRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row deployServiceRow
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// latestCompletedDeployRequirements is the deploy requirements of the latest
// round that completed. A failed round reported nothing, and an earlier
// round's list describes a tree a later round has already replaced.
func latestCompletedDeployRequirements(rounds []reviewRoundOutput) []reviewDeployRequirement {
	for i := len(rounds) - 1; i >= 0; i-- {
		if rounds[i].Completed {
			return rounds[i].DeployRequirements
		}
	}
	return nil
}

// resolveDeployRequirements resolves each requirement against the copy's
// services.json rows (see resolveDeployRequirement), drops duplicates of
// (variable, service, environment) and keeps at most maxDeployRequirements,
// logging one line per entry.
func resolveDeployRequirements(reqs []reviewDeployRequirement, rows []deployServiceRow, logsWriter io.Writer) []resolvedDeployRequirement {
	var out []resolvedDeployRequirement
	seen := map[[3]string]bool{}
	for _, req := range reqs {
		if !deployVariableName.MatchString(req.Variable) || strings.TrimSpace(req.Service) == "" {
			continue
		}
		for _, r := range resolveDeployRequirement(req, rows, logsWriter) {
			if len(out) == maxDeployRequirements {
				return out
			}
			key := [3]string{r.Variable, r.Service, r.Environment}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
			logDeployRequirement(logsWriter, r, deployRequirementOutcome(r))
		}
	}
	return out
}

// resolveDeployRequirement matches one requirement against the rows whose
// service equals its service exactly and, when it names one, whose environment
// equals its environment. A row whose environment already provides the
// variable — known only when variablesFrom says where the names came from —
// drops it for that row. A requirement no row matches is one "not found"
// entry carrying its own service and environment.
func resolveDeployRequirement(req reviewDeployRequirement, rows []deployServiceRow, logsWriter io.Writer) []resolvedDeployRequirement {
	var out []resolvedDeployRequirement
	matched := false
	for _, row := range rows {
		if row.Service != req.Service || (req.Environment != "" && row.Environment != req.Environment) {
			continue
		}
		matched = true
		entry := resolvedDeployRequirement{
			Variable:      req.Variable,
			Service:       req.Service,
			Environment:   row.Environment,
			EnvironmentID: row.EnvironmentID,
			Cluster:       row.Cluster,
			Managed:       row.RepoSource == "managed" && row.EnvironmentID != "",
			Found:         true,
			Location:      req.Location,
		}
		if row.VariablesFrom != "" && containsString(row.VariableNames, req.Variable) {
			logDeployRequirement(logsWriter, entry, "already set, dropped")
			continue
		}
		out = append(out, entry)
	}
	if !matched {
		out = append(out, resolvedDeployRequirement{
			Variable:    req.Variable,
			Service:     req.Service,
			Environment: req.Environment,
			Location:    req.Location,
		})
	}
	return out
}

func deployRequirementOutcome(r resolvedDeployRequirement) string {
	switch {
	case !r.Found:
		return "service not found"
	case r.Managed:
		return "managed"
	}
	return "unmanaged"
}

func deployRequirementsOf(result agentResult) []reviewDeployRequirement {
	if result.ReviewResult == nil {
		return nil
	}
	return result.ReviewResult.DeployRequirements
}

func logDeployRequirement(logsWriter io.Writer, r resolvedDeployRequirement, outcome string) {
	environment := r.Environment
	if environment == "" {
		environment = "-"
	}
	io.WriteString(logsWriter, fmt.Sprintf("Review stage: deploy requirement %s for %s in %s: %s\n", r.Variable, r.Service, environment, outcome))
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
