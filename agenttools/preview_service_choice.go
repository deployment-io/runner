package agenttools

import "github.com/deployment-io/deployment-runner-kit/task_previews"

// previewCandidate is a service a Task's preview may be built like: a web service
// or a static site, as deployment-server describes it.
type previewCandidate interface {
	task_previews.WebServicePreviewSettingsV1 | task_previews.StaticSiteBuildSettingsV1
}

// candidateBranch and candidateProduction read a candidate's Branch and
// ProductionEnvironment, whichever kind it is.
func candidateBranch[T previewCandidate](c T) string {
	switch c := any(c).(type) {
	case task_previews.WebServicePreviewSettingsV1:
		return c.Branch
	case task_previews.StaticSiteBuildSettingsV1:
		return c.Branch
	}
	return ""
}

func candidateProduction[T previewCandidate](c T) bool {
	switch c := any(c).(type) {
	case task_previews.WebServicePreviewSettingsV1:
		return c.ProductionEnvironment
	case task_previews.StaticSiteBuildSettingsV1:
		return c.ProductionEnvironment
	}
	return false
}

// preferTaskBranchServices narrows the services a preview chooses between of
// the same repository — Staging and Production deploying it from the same root
// directory — by the rule Deploy after merge picks a Task's production target
// with (kit task_models MatchProductionStaticSite): first the ones that deploy
// the Task's base branch, if any does (and baseBranch isn't ""); then, of
// those, the ones in a Production environment, if any is. It never empties the
// list and keeps the order of what it keeps. A deployment-server built before
// Branch and ProductionEnvironment sends "" and false: nothing is narrowed.
func preferTaskBranchServices[T previewCandidate](candidates []T, baseBranch string) []T {
	if baseBranch != "" {
		candidates = keepIfAny(candidates, func(c T) bool { return candidateBranch(c) == baseBranch })
	}
	return keepIfAny(candidates, candidateProduction[T])
}

// keepIfAny is the candidates keep accepts, in order — or all of them when it
// accepts none.
func keepIfAny[T any](candidates []T, keep func(T) bool) []T {
	var kept []T
	for _, c := range candidates {
		if keep(c) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return candidates
	}
	return kept
}
