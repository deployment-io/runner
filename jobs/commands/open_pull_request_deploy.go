package commands

import (
	"fmt"
	"net/url"
	"strings"
)

// prBodyDeployNameMaxRunes caps each environment, service and cluster name the
// "Before deploying" part prints. With maxDeployRequirements entries, it is
// what bounds the part.
const prBodyDeployNameMaxRunes = 100

// beforeDeployingSection lists the configuration the change reads that the
// review did not find in its environment — the Review stage's resolved deploy
// requirements. Empty when there are none.
//
// Every link is built HERE, from the resolved entry (whose environment id comes
// from the stage's own copy of services.json) and the Job's DashboardURL —
// never from model text. The names are printed in code spans with backticks
// and line breaks removed, so none can close its span or start a new line; a
// service name on a "not found" line is the reviewer's own text and is also
// neutralised like every other piece of model text on the pull request.
func beforeDeployingSection(review *reviewOutput, dashboardURL string) string {
	if review == nil || len(review.DeployRequirements) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("**Before deploying**\n\n")
	sb.WriteString("This change reads configuration that the review did not find in its environment. Make sure each is set before the change is deployed:\n")
	for i, r := range review.DeployRequirements {
		if i == maxDeployRequirements {
			break
		}
		if !deployVariableName.MatchString(r.Variable) {
			continue
		}
		sb.WriteString(deployRequirementLine(r, dashboardURL) + "\n")
	}
	return sb.String()
}

// deployRequirementLine is one requirement's line in "Before deploying".
func deployRequirementLine(r resolvedDeployRequirement, dashboardURL string) string {
	name := "`" + r.Variable + "`"
	switch {
	case r.Managed:
		line := fmt.Sprintf("- %s in environment %s (service %s)", name, deployCodeSpan(r.Environment), deployCodeSpan(r.Service))
		if base := strings.TrimRight(dashboardURL, "/"); base != "" {
			link := fmt.Sprintf("%s/environments/%s/edit?add=%s", base,
				url.PathEscape(deployName(r.EnvironmentID)), url.QueryEscape(r.Variable))
			line += fmt.Sprintf(" — [Add it](%s)", link)
		}
		return line
	case r.Found:
		line := fmt.Sprintf("- %s for ECS service %s", name, deployCodeSpan(r.Service))
		if cluster := deployName(r.Cluster); cluster != "" {
			line += " in cluster " + deployCodeSpan(cluster)
		}
		return line + " — set it in the service's task definition"
	}
	service := neutraliseModelText(deployName(r.Service))
	if service == "" {
		service = "-"
	}
	if r.ContextUnavailable {
		// The stage could not fetch the context, so nothing was checked — not
		// even whether the service is in it.
		return fmt.Sprintf("- %s for service `%s` — deployment.io's context could not be read, so this was not checked against the service's environment", name, service)
	}
	return fmt.Sprintf("- %s for service `%s` — this service is not in deployment.io's context", name, service)
}

// deployName removes backticks and line breaks from a name and caps it.
func deployName(s string) string {
	s = strings.NewReplacer("`", "", "\r", "", "\n", "").Replace(s)
	return capRunes(strings.TrimSpace(s), prBodyDeployNameMaxRunes)
}

// deployCodeSpan prints a name in a code span; an empty name shows as "-"
// rather than as an empty span, which Markdown does not render as one.
func deployCodeSpan(s string) string {
	if s = deployName(s); s == "" {
		s = "-"
	}
	return "`" + s + "`"
}
