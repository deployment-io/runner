package agenttools

import (
	"fmt"
	"strings"
)

// RunnerBuiltPreviewCheck is CheckRunnerBuiltPreview's answer for one repository.
type RunnerBuiltPreviewCheck struct {
	// OK is true when deploy_static_site_preview would build the repository's
	// preview itself (managed build settings or detected ones).
	OK bool
	// Reason is why not, when OK is false and LookupErr is nil.
	Reason string
	// LookupErr is set when deployment.io could not be asked how the repository is
	// deployed: the answer is unknown, not "no".
	LookupErr error
}

// CheckRunnerBuiltPreview makes deploy_static_site_preview's runner-built decision for
// repository — the build-settings lookup, then detection, then the managed site's
// validation — WITHOUT building or uploading anything. The QA stage runs it to find
// the repositories it can preview. The decision is the tool's own (planRunnerBuiltPreview
// with no root_directory), so the two cannot disagree.
func CheckRunnerBuiltPreview(deps DeployStaticSitePreviewDeps, repository string) RunnerBuiltPreviewCheck {
	repoDir, entry, err := resolveTaskRepository(deps.Repositories, repository)
	if err != nil {
		return RunnerBuiltPreviewCheck{Reason: err.Error()}
	}
	if deps.BuildSettings == nil {
		return RunnerBuiltPreviewCheck{LookupErr: fmt.Errorf("deployment.io can't look up how %s is deployed here", repoDir)}
	}
	settings, err := lookupBuildSettings(deps.BuildSettings, entry.CloneURL)
	if err != nil {
		return RunnerBuiltPreviewCheck{LookupErr: fmt.Errorf("look up how deployment.io deploys %s: %w", repoDir, err)}
	}
	plan, ok, err := planFromBuildSettings(deps, repoDir, nil, settings)
	if err != nil {
		return RunnerBuiltPreviewCheck{Reason: err.Error()}
	}
	if !ok {
		reason := strings.TrimSpace(plan.notDetected)
		if reason == "" {
			reason = "no static-site build settings could be detected"
		}
		return RunnerBuiltPreviewCheck{Reason: reason}
	}
	return RunnerBuiltPreviewCheck{OK: true}
}
