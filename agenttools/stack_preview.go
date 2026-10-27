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
	"strings"
)

// StaticSitePreviewPlan is a planned runner-built static-site preview: what
// deploy_static_site_preview would build for a repository (no root_directory).
type StaticSitePreviewPlan struct {
	Repository string // "<idx>-<name>"
	// DeploymentName is the static-site deployment the preview is built like; ""
	// for a site detected from the repository, which has no deployment.
	DeploymentName string
	plan           runnerBuiltPreview
}

// PlanStaticSitePreview plans repository's static-site preview as
// deploy_static_site_preview does, building nothing. A repository the runner can't
// build a preview of is an error.
func PlanStaticSitePreview(deps DeployStaticSitePreviewDeps, repository string) (StaticSitePreviewPlan, error) {
	plan, ok, err := planRunnerBuiltPreview(deps, repository, nil)
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
	name := plan.site.DeploymentName
	if plan.detected != nil {
		name = ""
	}
	return StaticSitePreviewPlan{Repository: plan.repository, DeploymentName: name, plan: plan}, nil
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
	plan           webServicePreviewPlan
}

// PlanWebServicePreview plans repository's backend preview as
// deploy_web_service_preview does, deploying nothing.
func PlanWebServicePreview(deps DeployWebServicePreviewDeps, repository string) (WebServicePreviewPlan, error) {
	plan, err := planWebServicePreview(deps, repository, nil)
	if err != nil {
		return WebServicePreviewPlan{}, err
	}
	return WebServicePreviewPlan{Repository: plan.repository, DeploymentName: plan.service.DeploymentName, plan: plan}, nil
}

// DeployWebServicePreviewWith deploys the backend preview of p as DeployWebServicePreview
// does, with variables replacing the preview configuration's of the same name (the
// preview database's still win over them).
func DeployWebServicePreviewWith(ctx context.Context, deps DeployWebServicePreviewDeps, p WebServicePreviewPlan, variables map[string]string) (WebServicePreviewDeployed, error) {
	plan := p.plan
	plan.wired = variables
	return runWebServicePreview(ctx, deps, plan)
}
