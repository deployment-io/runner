package agenttools

// stack_preview.go is what the QA stage uses to deploy a change's previews as one
// stack, outside the tools: plan a site and a backend preview the way the tools do,
// reserve a site's distribution before anything is built (so its origin is known
// when its backend deploys), and deploy each with variables the stage sets on top of
// the preview configuration's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// StaticSitePreviewPlan is a planned runner-built static-site preview: what
// deploy_static_site_preview would build for a repository (no root_directory).
type StaticSitePreviewPlan struct {
	Repository string // "<idx>-<name>"
	// DeploymentName is the static-site deployment the preview is built like; ""
	// for a site detected from the repository, which has no deployment.
	DeploymentName string
	// EnvironmentName is that deployment's environment's name; "" for a detected
	// site.
	EnvironmentName string
	plan            runnerBuiltPreview
}

// PlanStaticSitePreview plans repository's static-site preview as
// deploy_static_site_preview does, building nothing. A repository the runner can't
// build a preview of is an error.
func PlanStaticSitePreview(deps DeployStaticSitePreviewDeps, repository string) (StaticSitePreviewPlan, error) {
	plan, ok, err := planRunnerBuiltPreview(context.Background(), deps, repository, nil)
	if err != nil {
		return StaticSitePreviewPlan{}, err
	}
	if !ok {
		reason := strings.TrimSpace(plan.notDetected)
		if reason == "" {
			reason = "no static-site build settings could be detected"
		}
		return StaticSitePreviewPlan{}, fmt.Errorf("deployment.io can't build a preview of %s: %s", repository, reason)
	}
	name, environment := plan.site.DeploymentName, plan.site.EnvironmentName
	if plan.detected != nil {
		name, environment = "", ""
	}
	return StaticSitePreviewPlan{Repository: plan.repository, DeploymentName: name, EnvironmentName: environment, plan: plan}, nil
}

// reservePreviewDist is ReservePreviewDistribution; tests replace it to create nothing.
var reservePreviewDist = ReservePreviewDistribution

// ReserveStaticSitePreview makes sure the preview of p has its distribution before
// the site is built, and returns its URL, https://<distribution domain>: the one on
// the preview record or in deps.Distributions, else a new one (bucket, distribution
// and bucket policy, no upload), saved on the record and remembered at once. The
// site's later deploy uploads into it. A distribution reserved here counts as
// created in this run for its probe (PreviewRecord.SetDeployProbe).
func ReserveStaticSitePreview(ctx context.Context, deps DeployStaticSitePreviewDeps, p StaticSitePreviewPlan) (string, error) {
	previewID, existing, err := ensurePreview(ctx, deps.Store, p.plan.serviceName)
	if err != nil {
		return "", fmt.Errorf("ensure preview record: %w", err)
	}
	existing = deps.Distributions.fill(previewID, existing)
	if existing.CloudFrontDistributionID != "" {
		if existing.CloudFrontDomainName == "" {
			return "", fmt.Errorf("the preview's distribution %s has no domain on record", existing.CloudFrontDistributionID)
		}
		previewURL := "https://" + existing.CloudFrontDomainName
		deps.Record.AddURL(previewURL)
		return previewURL, nil
	}
	s3Client, cfClient, err := previewClients(ctx, deps)
	if err != nil {
		return "", err
	}
	res, err := reservePreviewDist(StaticPreviewDeployInput{
		OrgID:            deps.OrgID,
		PreviewID:        previewID,
		Region:           deps.Region,
		IsSPA:            p.plan.site.IsSpa,
		SkipDeployWait:   true,
		S3Client:         s3Client,
		CloudfrontClient: cfClient,
		// Saved the moment it exists, so a failing bucket policy still leaves it saved.
		OnDistributionCreated: func(id, arn, domain string) {
			saved := PreviewState{CloudFrontDistributionID: id, CloudFrontDistributionArn: arn, CloudFrontDomainName: domain}
			deps.Distributions.remember(previewID, saved)
			deps.Store.SavePreview(previewID, saved)
		},
	}, deps.LogsWriter)
	if err != nil {
		return "", fmt.Errorf("reserve the preview's distribution: %w", err)
	}
	if res.DomainName == "" {
		return "", errors.New("reserve the preview's distribution: CloudFront returned no domain")
	}
	saved := PreviewState{
		CloudFrontDistributionID:  res.DistributionID,
		CloudFrontDistributionArn: res.DistributionArn,
		CloudFrontDomainName:      res.DomainName,
	}
	deps.Distributions.remember(previewID, saved)
	deps.Store.SavePreview(previewID, saved)
	previewURL := "https://" + res.DomainName
	deps.Record.AddURL(previewURL)
	deps.Record.SetDeployProbe(previewURL, true, "")
	return previewURL, nil
}

// DeployStaticSitePreviewWith builds and deploys the preview of p as
// deploy_static_site_preview does, waiting for the build, with variables added to the
// build's environment after the preview configuration's (replacing one of the same
// name, and with no configuration set too). It returns the preview's URL.
func DeployStaticSitePreviewWith(ctx context.Context, deps DeployStaticSitePreviewDeps, p StaticSitePreviewPlan, variables map[string]string) (string, error) {
	plan := p.plan
	plan.wired = variables
	out, err := buildRunnerBuiltPreview(ctx, deps, plan)
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// WebServicePreviewPlan is a planned backend preview: what deploy_web_service_preview
// would deploy for a repository (no root_directory).
type WebServicePreviewPlan struct {
	Repository string // "<idx>-<name>"
	// DeploymentName is the web-service deployment the preview is built like.
	DeploymentName string
	// ServiceName is the key of the preview it deploys (webPreviewServiceKey).
	ServiceName string
	plan        webServicePreviewPlan
}

// PlanWebServicePreview plans repository's backend preview as
// deploy_web_service_preview does, deploying nothing.
func PlanWebServicePreview(deps DeployWebServicePreviewDeps, repository string) (WebServicePreviewPlan, error) {
	plan, err := planWebServicePreview(context.Background(), deps, repository, nil)
	if err != nil {
		return WebServicePreviewPlan{}, err
	}
	return WebServicePreviewPlan{Repository: plan.repository, DeploymentName: plan.service.DeploymentName, ServiceName: plan.serviceName, plan: plan}, nil
}

// DeployWebServicePreviewWith deploys the backend preview of p as DeployWebServicePreview
// does, with variables replacing the preview configuration's of the same name (the
// preview database's still win over them).
func DeployWebServicePreviewWith(ctx context.Context, deps DeployWebServicePreviewDeps, p WebServicePreviewPlan, variables map[string]string) (WebServicePreviewDeployed, error) {
	plan := p.plan
	plan.wired = variables
	return runWebServicePreview(ctx, deps, plan)
}

// CompanionWebService is a web service outside the Task that the Task's site
// previews call (a "companion" backend): the QA stage deploys an unchanged preview
// of it from its production branch.
type CompanionWebService struct {
	Repository string // "companion-<i>-<name>": never a directory under the work dir
	// Entry is the web service's repository: Name, CloneURL, BaseBranch (the
	// production branch), Provider and InstallationID.
	Entry tasks.RepositoryEntry
	API   string // the production web-service deployment's name
	// Checkout writes the companion's source into dir, a new empty build directory.
	Checkout func(ctx context.Context, dir string) error
}

// PlanCompanionWebServicePreview plans the backend preview of the companion c as
// PlanWebServicePreview plans a Task repository's, deploying nothing: the service
// is the one named c.API deploying c.Entry.BaseBranch, chosen by region. Its
// deploy (DeployWebServicePreviewWith) builds from c.Checkout, never from the
// work dir.
func PlanCompanionWebServicePreview(deps DeployWebServicePreviewDeps, c CompanionWebService) (WebServicePreviewPlan, error) {
	if deps.Settings == nil || deps.Deploy == nil {
		return WebServicePreviewPlan{}, fmt.Errorf("deployment.io can't deploy backend previews here")
	}
	if c.Checkout == nil {
		return WebServicePreviewPlan{}, fmt.Errorf("%s has no source to build", c.Repository)
	}
	settings, err := lookupWithin(context.Background(), deps.Settings, c.Entry.CloneURL,
		"deployment.io didn't answer in time")
	if err != nil {
		return WebServicePreviewPlan{}, fmt.Errorf("look up how deployment.io deploys %s: %w", c.Repository, err)
	}
	var kept []task_previews.WebServicePreviewSettingsV1
	for _, s := range settings.Services {
		if s.DeploymentName == c.API && s.Branch == c.Entry.BaseBranch {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return WebServicePreviewPlan{}, fmt.Errorf("deployment.io no longer deploys %s from %s as a web service", c.API, c.Entry.BaseBranch)
	}
	settings.Services = kept
	service, err := chooseWebService(settings, c.Repository, nil, deps.Region, c.Entry.BaseBranch)
	if err != nil {
		return WebServicePreviewPlan{}, err
	}
	root, err := cleanRelativeDir(service.RootDirectory)
	if err != nil {
		return WebServicePreviewPlan{}, fmt.Errorf("%s's root directory: %w", service.DeploymentName, err)
	}
	serviceName, ok := webPreviewServiceKey(c.Repository, root)
	if !ok {
		return WebServicePreviewPlan{}, fmt.Errorf("could not derive a service name for %s", service.DeploymentName)
	}
	plan := webServicePreviewPlan{
		repository:    c.Repository,
		service:       service,
		rootDirectory: root,
		serviceName:   serviceName,
		settings:      settings,
		checkout:      c.Checkout,
	}
	return WebServicePreviewPlan{Repository: plan.repository, DeploymentName: service.DeploymentName, ServiceName: serviceName, plan: plan}, nil
}

// companionBuildSource is the build directory of a companion's preview: a new
// directory beside the work dir with the companion's source checked out into it,
// and the merge result's rules applied (no .git or node_modules, no symlink that
// leaves it). Returned also with an error, so the caller removes it.
func companionBuildSource(ctx context.Context, deps DeployWebServicePreviewDeps, p webServicePreviewPlan) (string, error) {
	logs := deps.LogsWriter
	if logs == nil {
		logs = io.Discard
	}
	dir, err := newBuildDir(deps.WorkDirHost)
	if err != nil {
		return "", err
	}
	if err := p.checkout(ctx, dir); err != nil {
		return dir, err
	}
	if err := removeLeftOutEntries(dir); err != nil {
		return dir, err
	}
	return dir, removeEscapingSymlinks(dir, logs)
}
