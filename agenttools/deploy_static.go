// Package agenttools holds the implementations behind the agent-invoked MCP tools
// (deploy_static_site_preview, verify_preview_reachable, …). These are NOT job Commands — they don't
// implement Run and aren't dispatched sequentially by the job engine; they're
// called in-process by the runner's per-task agent_mcp tool handlers. Keeping them
// out of jobs/commands avoids implying they're part of the job command chain, and
// lets them depend on the leaf aws_utils package without an import cycle.
package agenttools

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
)

// cachingDisabledPolicyID is CloudFront's AWS-managed "CachingDisabled" cache
// policy. Because it's AWS-managed (not account-created), this ID is a GLOBAL AWS
// constant — identical in every commercial-partition account and region — which is
// exactly what makes it safe to hardcode in the BYO multi-account model (AWS's own
// CDK hardcodes the same literal as CachePolicy.CACHING_DISABLED). GovCloud/China
// are separate partitions where CloudFront is limited/absent and this may not apply.
//
// Previews use it instead of a custom policy: a preview iterates constantly (the
// opposite of a production site), so we want ZERO CDN caching — the freshly
// re-uploaded content is served immediately with no invalidation, and we don't
// create a per-preview custom policy that would burn one of the account's ~20
// cache-policy slots.
const cachingDisabledPolicyID = "4135ea2d-6df8-44a3-9df3-4b5a84be39ad"

// StaticPreviewDeployInput is the request for DeployStaticSitePreview.
type StaticPreviewDeployInput struct {
	OrgID         string // organization id (for resource naming)
	PreviewID     string // the ephemeral preview Deployment's id — bucket + resource naming
	DistDirectory string // host path to the built static site (agent's /work publish dir)
	Region        string // AWS region string, e.g. region_enums.Type(r).String()
	IsSPA         bool   // rewrite 403/404 -> /index.html so client-side routes resolve

	// SkipDeployWait returns as soon as the distribution is created/invalidated
	// (content uploaded, config applied) WITHOUT blocking on CloudFront's global
	// propagation waiter (up to 20 min). The preview tool sets this so an agent's
	// synchronous tool call returns promptly; the URL goes live as the CDN
	// propagates (typically 1–5 min). Verification (C3) polls the URL until live.
	SkipDeployWait bool

	// ExistingDistID is the CloudFront distribution id from a prior iteration.
	// Empty on the first preview of a task (create the distribution); set on
	// reuse (just re-upload — the CachingDisabled policy serves it immediately).
	ExistingDistID string

	S3Client         *s3.Client
	CloudfrontClient *cloudfront.Client
}

// StaticPreviewDeployResult carries what the caller persists on the preview
// Deployment for the next iteration + the URL.
type StaticPreviewDeployResult struct {
	DistributionID  string
	DistributionArn string
	DomainName      string // CloudFront domain (e.g. d123.cloudfront.net); the preview URL host
}

// DeployStaticSitePreview deploys an already-built static site into an ephemeral,
// task-scoped preview: its own S3 bucket + CloudFront distribution, created on the
// first call and reused across iterations (via ExistingDistID). It is served at a
// ROOT (the CloudFront default domain), so a normal static site — including an SPA
// with absolute asset paths + client routing — works with zero path-prefix corner
// cases.
//
// Self-contained + blocking: invoked in-process by the deploy_static_site_preview tool handler,
// deliberately NOT deploy_aws_static_site.Run (which is coupled to the Job params
// map, deployment-server RPCs, and MarkDeploymentDone). It reuses that command's
// arg-based AWS primitives, now shared via the aws_utils package.
//
// Known edge (C1 walking-skeleton scope): a first deploy that fails after creating
// the OAC but before the distribution would collide on that account-global name when
// retried — harden later (describe-or-create). The cache policy is the AWS-managed
// CachingDisabled one, so it's no longer a per-preview resource that can orphan.
func DeployStaticSitePreview(in StaticPreviewDeployInput, logsWriter io.Writer) (StaticPreviewDeployResult, error) {
	bucketName := in.OrgID + "-" + in.PreviewID // matches deploy_aws_static_site's <org>-<deploymentID> scheme

	if _, err := os.Stat(in.DistDirectory + "/index.html"); err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("no index.html in build dir %s: %w", in.DistDirectory, err)
	}

	bucketLocation, _, err := aws_utils.CreateS3BucketIfNeeded(in.S3Client, bucketName, in.Region)
	if err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("ensure preview bucket: %w", err)
	}

	// Re-upload the freshly built site (clear stale objects first).
	if err = aws_utils.DeleteAllS3Files(in.S3Client, bucketName); err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("clear preview bucket: %w", err)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Uploading preview to S3 bucket: %s\n", bucketName))
	if err = aws_utils.UploadToS3(in.DistDirectory, in.Region, bucketName, in.S3Client, logsWriter); err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("upload preview: %w", err)
	}

	cf := in.CloudfrontClient

	// Reuse path: the distribution already serves this bucket. It uses the managed
	// CachingDisabled policy, so the re-uploaded content is served immediately — no
	// invalidation needed.
	if in.ExistingDistID != "" {
		io.WriteString(logsWriter, fmt.Sprintf("Redeployed preview to existing distribution: %s\n", in.ExistingDistID))
		return StaticPreviewDeployResult{DistributionID: in.ExistingDistID}, nil
	}

	// First deploy: stand up the distribution. Uses the managed CachingDisabled
	// policy (cachingDisabledPolicyID) rather than creating a per-preview one.
	oacID, err := aws_utils.CreateOriginAccessControl("preview-"+in.PreviewID, cf)
	if err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("create OAC: %w", err)
	}
	behavior := aws_utils.CreateDefaultCacheBehavior(bucketLocation, aws.String(cachingDisabledPolicyID))
	s3Domain := bucketName + ".s3." + in.Region + ".amazonaws.com"
	distConfig := buildPreviewDistributionConfig(bucketLocation, oacID, in.PreviewID, s3Domain, behavior, in.IsSPA)

	io.WriteString(logsWriter, "Creating preview CloudFront distribution\n")
	out, err := cf.CreateDistribution(context.TODO(), &cloudfront.CreateDistributionInput{DistributionConfig: distConfig})
	if err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("create preview distribution: %w", err)
	}
	dist := out.Distribution

	if err = aws_utils.AttachPolicyToS3Bucket(dist.ARN, bucketName,
		"AllowCloudFrontServicePrincipal-"+in.PreviewID, "PolicyForCloudFrontPrivateContent-"+in.PreviewID, in.S3Client); err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("attach bucket policy: %w", err)
	}

	if in.SkipDeployWait {
		io.WriteString(logsWriter, fmt.Sprintf("Preview distribution created: %s (propagating)\n", aws.ToString(dist.Id)))
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Waiting for preview distribution to deploy: %s\n", aws.ToString(dist.Id)))
		if err = cloudfront.NewDistributionDeployedWaiter(cf).Wait(context.TODO(),
			&cloudfront.GetDistributionInput{Id: dist.Id}, 20*time.Minute); err != nil {
			return StaticPreviewDeployResult{}, fmt.Errorf("wait preview distribution: %w", err)
		}
	}

	return StaticPreviewDeployResult{
		DistributionID:  aws.ToString(dist.Id),
		DistributionArn: aws.ToString(dist.ARN),
		DomainName:      aws.ToString(dist.DomainName),
	}, nil
}

// buildPreviewDistributionConfig is a param-free distribution config for a preview
// (cf. createDistributionConfigForNewCloudfront, which reads error pages from the
// Job params map). For an SPA, 403/404 are rewritten to /index.html (200) so
// client-side routes resolve; a plain static site keeps real 404s.
func buildPreviewDistributionConfig(bucketLocation, oacID *string, previewID, s3Domain string,
	behavior *cloudfrontTypes.DefaultCacheBehavior, isSPA bool) *cloudfrontTypes.DistributionConfig {
	origins := &cloudfrontTypes.Origins{
		Quantity: aws.Int32(1),
		Items: []cloudfrontTypes.Origin{{
			Id:                    bucketLocation,
			DomainName:            aws.String(s3Domain),
			OriginAccessControlId: oacID,
			S3OriginConfig:        &cloudfrontTypes.S3OriginConfig{OriginAccessIdentity: aws.String("")},
		}},
	}

	errorResponses := &cloudfrontTypes.CustomErrorResponses{Quantity: aws.Int32(0)}
	if isSPA {
		// ErrorCachingMinTTL 0 so a redeploy's changes show immediately (the default
		// is 300s), matching the no-cache CachingDisabled policy on the behavior.
		items := []cloudfrontTypes.CustomErrorResponse{
			{ErrorCode: aws.Int32(403), ResponsePagePath: aws.String("/index.html"), ResponseCode: aws.String("200"), ErrorCachingMinTTL: aws.Int64(0)},
			{ErrorCode: aws.Int32(404), ResponsePagePath: aws.String("/index.html"), ResponseCode: aws.String("200"), ErrorCachingMinTTL: aws.Int64(0)},
		}
		errorResponses = &cloudfrontTypes.CustomErrorResponses{Quantity: aws.Int32(int32(len(items))), Items: items}
	}

	return &cloudfrontTypes.DistributionConfig{
		CallerReference:      aws.String(fmt.Sprintf("preview-%s-%d", previewID, time.Now().Unix())),
		Comment:              aws.String("Preview distribution for " + previewID),
		DefaultCacheBehavior: behavior,
		Enabled:              aws.Bool(true),
		Origins:              origins,
		CustomErrorResponses: errorResponses,
		DefaultRootObject:    aws.String("index.html"),
		// PriceClassAll: doesn't gate access — it picks which edge locations serve the
		// content. A BYO customer's viewers + origin can be in ANY region, and with
		// CachingDisabled every request hits the origin, so edge locality directly costs
		// latency. The cost delta vs 100/200 is negligible for low-traffic previews, so
		// a local edge everywhere is the right default.
		PriceClass: cloudfrontTypes.PriceClassPriceClassAll,
	}
}
