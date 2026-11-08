// Package agenttools holds the implementations behind the agent-invoked MCP tools
// (deploy_static_site_preview, verify_preview_reachable, …). These are NOT job Commands — they don't
// implement Run and aren't dispatched sequentially by the job engine; they're
// called in-process by the runner's per-task agent_mcp tool handlers. Keeping them
// out of jobs/commands avoids implying they're part of the job command chain, and
// lets them depend on the leaf aws_utils package without an import cycle.
package agenttools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	IsSPA         bool   // SPA: rewrite 403/404 -> /index.html; else serve <page>/index.html at <page>/

	// SkipDeployWait returns as soon as the distribution is created/invalidated
	// (content uploaded, config applied) WITHOUT blocking on CloudFront's global
	// propagation waiter (up to 20 min). The preview tool sets this so an agent's
	// synchronous tool call returns promptly; the URL goes live as the CDN
	// propagates (typically 1–5 min). Verification (C3) polls the URL until live.
	SkipDeployWait bool

	// ExistingDistID is the CloudFront distribution id from a prior iteration.
	// Empty on the first preview of a task (create the distribution); set on
	// reuse (re-upload — the CachingDisabled policy serves it immediately — and
	// bring the distribution's routing in line with IsSPA if it differs).
	ExistingDistID string

	// ExistingDistArn is ExistingDistID's ARN. On reuse the bucket policy is
	// attached again for it: a first deploy saves its distribution before
	// attaching the policy, so that attach may never have happened. Empty means
	// it is looked up from CloudFront (the preview record carries only the id).
	ExistingDistArn string

	// OnDistributionCreated, when set, is called with a new distribution's id,
	// ARN and domain as soon as CloudFront creates it, before anything else can
	// fail, so the caller saves the id even when the deploy fails afterwards (a
	// preview whose id was never saved can't be redeployed or deleted by id).
	// It is also called for a distribution adopted by adoptPreviewDistribution,
	// and not when the deploy reuses ExistingDistID.
	OnDistributionCreated func(id, arn, domain string)

	S3Client         *s3.Client
	CloudfrontClient *cloudfront.Client
}

// StaticPreviewDeployResult carries what the caller persists on the preview
// Deployment for the next iteration + the URL.
type StaticPreviewDeployResult struct {
	DistributionID  string
	DistributionArn string
	DomainName      string // CloudFront domain (e.g. d123.cloudfront.net); the preview URL host
	// UploadedKeys are the S3 keys this deploy uploaded (paths relative to the
	// upload root, "/"-separated), sorted.
	UploadedKeys []string
}

// sortedUploadedKeys returns the keys of uploaded, sorted.
func sortedUploadedKeys(uploaded map[string]bool) []string {
	keys := make([]string, 0, len(uploaded))
	for k := range uploaded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pickProbeKey picks the file verify_preview_reachable waits for after a new
// distribution's front page answers: the lexically first of keys (S3 keys from an
// upload) that isn't an HTML page and has no path segment starting with ".". It
// returns "" when there is none.
func pickProbeKey(keys []string) string {
	best := ""
	for _, k := range keys {
		if k == "" || strings.HasSuffix(strings.ToLower(k), ".html") {
			continue
		}
		hidden := false
		for _, seg := range strings.Split(k, "/") {
			if strings.HasPrefix(seg, ".") {
				hidden = true
				break
			}
		}
		if hidden {
			continue
		}
		if best == "" || k < best {
			best = k
		}
	}
	return best
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
// A first deploy that fails after creating the OAC but before saving the
// distribution's id leaves the OAC behind, and maybe the distribution: the next
// first deploy adopts that distribution (adoptPreviewDistribution), or else
// reuses the OAC by name (findPreviewOriginAccessControl). The cache policy is
// the AWS-managed CachingDisabled one, so it's no longer a per-preview resource
// that can orphan.
func DeployStaticSitePreview(in StaticPreviewDeployInput, logsWriter io.Writer) (StaticPreviewDeployResult, error) {
	bucketName := previewBucketName(in)

	if _, err := os.Stat(in.DistDirectory + "/index.html"); err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("no index.html in build dir %s: %w", in.DistDirectory, err)
	}

	bucketLocation, _, err := aws_utils.CreateS3BucketIfNeeded(in.S3Client, bucketName, in.Region)
	if err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("ensure preview bucket: %w", err)
	}

	// Upload over the previous build, then mark what it left behind for expiry
	// a week out — never empty the bucket first, and never delete outright.
	// Hashed filenames cannot collide, so the upload is additive, and the origin
	// is never missing files mid-deploy. Same reason as the deployment path: a
	// code-split SPA fetches chunks on navigation, so a session running the
	// previous build asks for files that clearing would already have deleted,
	// and a preview tab is exactly the kind of thing someone leaves open while
	// a new build lands.
	//
	// Marking must follow the upload: it is the upload that clears the stale tag
	// from any file a previous deploy marked and this build brought back.
	//
	// No invalidation to sequence around here — this distribution uses the
	// managed CachingDisabled policy, so the edge always reads through.
	io.WriteString(logsWriter, fmt.Sprintf("Uploading preview to S3 bucket: %s\n", bucketName))
	uploadedKeys, err := aws_utils.UploadToS3(in.DistDirectory, in.Region, bucketName, in.S3Client, logsWriter)
	if err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("upload preview: %w", err)
	}
	adoptedDomain, err := adoptPreviewDistribution(in.CloudfrontClient, &in, logsWriter)
	if err != nil {
		return StaticPreviewDeployResult{}, err
	}
	// Not fatal: the preview is already serving the new build, and leftover
	// files from the previous one do not break it — they just sit there until a
	// later deploy marks them.
	if err = aws_utils.MarkStaleS3Files(in.S3Client, bucketName, uploadedKeys, logsWriter); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not mark previous preview files as stale (the preview is live): %s\n", err))
	}

	dist, err := deployPreviewDistribution(in.CloudfrontClient, in, bucketLocation, logsWriter)
	if err != nil {
		return StaticPreviewDeployResult{}, err
	}
	if dist == nil {
		arn, err := previewDistributionArn(in.CloudfrontClient, in)
		if err != nil {
			return StaticPreviewDeployResult{}, err
		}
		if err = attachPreviewBucketPolicy(in, &cloudfrontTypes.Distribution{ARN: aws.String(arn)}); err != nil {
			return StaticPreviewDeployResult{}, err
		}
		// An adopted distribution's domain goes back with it, for the caller to save;
		// a reused one's is already on the preview record.
		return StaticPreviewDeployResult{DistributionID: in.ExistingDistID, DistributionArn: arn, DomainName: adoptedDomain, UploadedKeys: sortedUploadedKeys(uploadedKeys)}, nil
	}
	cf := in.CloudfrontClient

	if err = attachPreviewBucketPolicy(in, dist); err != nil {
		return StaticPreviewDeployResult{}, err
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
		UploadedKeys:    sortedUploadedKeys(uploadedKeys),
	}, nil
}

// ReservePreviewDistribution is DeployStaticSitePreview's first deploy without the
// upload: the bucket, the distribution in front of it (routed for IsSPA) and the
// bucket policy that lets it read the bucket, so the preview's address is known
// before its site is built. A later DeployStaticSitePreview with ExistingDistID set
// to the returned distribution uploads the site. It doesn't wait for the
// distribution to deploy.
func ReservePreviewDistribution(in StaticPreviewDeployInput, logsWriter io.Writer) (StaticPreviewDeployResult, error) {
	bucketLocation, _, err := aws_utils.CreateS3BucketIfNeeded(in.S3Client, previewBucketName(in), in.Region)
	if err != nil {
		return StaticPreviewDeployResult{}, fmt.Errorf("ensure preview bucket: %w", err)
	}
	in.ExistingDistID = ""
	adoptedDomain, err := adoptPreviewDistribution(in.CloudfrontClient, &in, logsWriter)
	if err != nil {
		return StaticPreviewDeployResult{}, err
	}
	if in.ExistingDistID != "" {
		return reserveAdoptedPreviewDistribution(in, bucketLocation, adoptedDomain, logsWriter)
	}
	dist, err := deployPreviewDistribution(in.CloudfrontClient, in, bucketLocation, logsWriter)
	if err != nil {
		return StaticPreviewDeployResult{}, err
	}
	if err = attachPreviewBucketPolicy(in, dist); err != nil {
		return StaticPreviewDeployResult{}, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Preview distribution reserved: %s (propagating)\n", aws.ToString(dist.Id)))
	return StaticPreviewDeployResult{
		DistributionID:  aws.ToString(dist.Id),
		DistributionArn: aws.ToString(dist.ARN),
		DomainName:      aws.ToString(dist.DomainName),
	}, nil
}

// reserveAdoptedPreviewDistribution is ReservePreviewDistribution for a distribution
// adoptPreviewDistribution found: its routing is brought in line with IsSPA and the
// bucket policy attached for it, as for a reused one.
func reserveAdoptedPreviewDistribution(in StaticPreviewDeployInput, bucketLocation *string, domain string, logsWriter io.Writer) (StaticPreviewDeployResult, error) {
	if _, err := deployPreviewDistribution(in.CloudfrontClient, in, bucketLocation, logsWriter); err != nil {
		return StaticPreviewDeployResult{}, err
	}
	arn, err := previewDistributionArn(in.CloudfrontClient, in)
	if err != nil {
		return StaticPreviewDeployResult{}, err
	}
	if err = attachPreviewBucketPolicy(in, &cloudfrontTypes.Distribution{ARN: aws.String(arn)}); err != nil {
		return StaticPreviewDeployResult{}, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Preview distribution reserved: %s (propagating)\n", in.ExistingDistID))
	return StaticPreviewDeployResult{DistributionID: in.ExistingDistID, DistributionArn: arn, DomainName: domain}, nil
}

// previewDistributionComment is the Comment every preview distribution carries
// (buildPreviewDistributionConfig). A deploy adopts, and the static-site delete
// finds, a preview's distributions by it.
func previewDistributionComment(previewID string) string {
	return "Preview distribution for " + previewID
}

// adoptPreviewDistribution makes a preview with no saved distribution keep the one
// an earlier deploy created and never saved (the save failed, or the runner
// restarted before it was sent), so a preview has one distribution, not one per
// failed first deploy. When in has no ExistingDistID it looks for a distribution
// with the preview's Comment; when there is one it sets in's ExistingDistID and
// ExistingDistArn to it, hands it to OnDistributionCreated (the caller saves it at
// once) and returns its domain. It returns "" when in already has a distribution
// or none was found.
func adoptPreviewDistribution(cf previewDistributions, in *StaticPreviewDeployInput, logsWriter io.Writer) (string, error) {
	if in.ExistingDistID != "" {
		return "", nil
	}
	found, err := findPreviewDistribution(cf, previewDistributionComment(in.PreviewID))
	if err != nil {
		return "", fmt.Errorf("look for the preview's distribution: %w", err)
	}
	if found == nil {
		return "", nil
	}
	id, arn, domain := aws.ToString(found.Id), aws.ToString(found.ARN), aws.ToString(found.DomainName)
	io.WriteString(logsWriter, fmt.Sprintf("Reusing the preview's CloudFront distribution %s (an earlier deploy created it; its id wasn't saved)\n", id))
	in.ExistingDistID, in.ExistingDistArn = id, arn
	if in.OnDistributionCreated != nil {
		in.OnDistributionCreated(id, arn, domain)
	}
	return domain, nil
}

// findPreviewDistribution returns the first distribution whose Comment is exactly
// comment, or nil when there is none.
func findPreviewDistribution(cf previewDistributions, comment string) (*cloudfrontTypes.DistributionSummary, error) {
	var marker *string
	for {
		out, err := cf.ListDistributions(context.TODO(), &cloudfront.ListDistributionsInput{Marker: marker})
		if err != nil {
			return nil, err
		}
		if out == nil || out.DistributionList == nil {
			return nil, nil
		}
		list := out.DistributionList
		for i := range list.Items {
			if aws.ToString(list.Items[i].Comment) == comment {
				return &list.Items[i], nil
			}
		}
		if !aws.ToBool(list.IsTruncated) || aws.ToString(list.NextMarker) == "" {
			return nil, nil
		}
		marker = list.NextMarker
	}
}

// attachPreviewBucketPolicy lets a preview distribution read its bucket. It
// replaces the bucket's policy with the same one each time.
func attachPreviewBucketPolicy(in StaticPreviewDeployInput, dist *cloudfrontTypes.Distribution) error {
	if err := aws_utils.AttachPolicyToS3Bucket(dist.ARN, previewBucketName(in),
		"AllowCloudFrontServicePrincipal-"+in.PreviewID, "PolicyForCloudFrontPrivateContent-"+in.PreviewID, in.S3Client); err != nil {
		return fmt.Errorf("attach bucket policy: %w", err)
	}
	return nil
}

// previewDistributionArn is the ARN of the reused distribution in.ExistingDistID:
// in.ExistingDistArn, or CloudFront's when that is empty.
func previewDistributionArn(cf previewDistributions, in StaticPreviewDeployInput) (string, error) {
	if in.ExistingDistArn != "" {
		return in.ExistingDistArn, nil
	}
	out, err := cf.GetDistribution(context.TODO(), &cloudfront.GetDistributionInput{Id: aws.String(in.ExistingDistID)})
	if err != nil {
		return "", fmt.Errorf("get preview distribution %s: %w", in.ExistingDistID, err)
	}
	if out.Distribution == nil || aws.ToString(out.Distribution.ARN) == "" {
		return "", fmt.Errorf("get preview distribution %s: no ARN", in.ExistingDistID)
	}
	return aws.ToString(out.Distribution.ARN), nil
}

// previewBucketName matches deploy_aws_static_site's <org>-<deploymentID> scheme.
func previewBucketName(in StaticPreviewDeployInput) string {
	return in.OrgID + "-" + in.PreviewID
}

// deployPreviewDistribution is the CloudFront half of DeployStaticSitePreview.
// On reuse (ExistingDistID set) it brings the existing distribution's routing in
// line with IsSPA and returns a nil distribution; on a first deploy it creates
// the OAC and a distribution already configured that way.
func deployPreviewDistribution(cf previewCloudfront, in StaticPreviewDeployInput, bucketLocation *string,
	logsWriter io.Writer) (*cloudfrontTypes.Distribution, error) {
	// A plain static site writes <page>/index.html; S3 only serves that at
	// <page>/index.html, so a viewer-request function maps <page>/ onto it, as
	// production does. An SPA instead relies on the 403/404 → /index.html rewrite.
	routingARN := ""
	if !in.IsSPA {
		arn, err := ensurePreviewRoutingFunction(cf)
		if err != nil {
			return nil, err
		}
		routingARN = arn
	}

	// Reuse path: the distribution already serves this bucket. It uses the managed
	// CachingDisabled policy, so the re-uploaded content is served immediately — no
	// invalidation needed.
	if in.ExistingDistID != "" {
		return nil, redeployPreviewDistribution(cf, in, routingARN, logsWriter)
	}

	// First deploy: stand up the distribution. Uses the managed CachingDisabled
	// policy (cachingDisabledPolicyID) rather than creating a per-preview one.
	oacID, err := ensurePreviewOriginAccessControl(cf, "preview-"+in.PreviewID, logsWriter)
	if err != nil {
		return nil, err
	}
	behavior := aws_utils.CreateDefaultCacheBehavior(bucketLocation, aws.String(cachingDisabledPolicyID))
	s3Domain := previewBucketName(in) + ".s3." + in.Region + ".amazonaws.com"
	distConfig := buildPreviewDistributionConfig(bucketLocation, oacID, in.PreviewID, s3Domain, behavior, in.IsSPA)
	reconcilePreviewRouting(distConfig, in.IsSPA, routingARN)

	io.WriteString(logsWriter, "Creating preview CloudFront distribution\n")
	out, err := cf.CreateDistribution(context.TODO(), &cloudfront.CreateDistributionInput{DistributionConfig: distConfig})
	if err != nil {
		return nil, fmt.Errorf("create preview distribution: %w", err)
	}
	if in.OnDistributionCreated != nil && out.Distribution != nil {
		in.OnDistributionCreated(aws.ToString(out.Distribution.Id), aws.ToString(out.Distribution.ARN), aws.ToString(out.Distribution.DomainName))
	}
	return out.Distribution, nil
}

// ensurePreviewOriginAccessControl creates the preview's OAC, or returns the id of
// the one named name when it already exists: an earlier deploy of this preview
// created it and failed before its distribution's id was saved. OAC names are
// unique per account.
func ensurePreviewOriginAccessControl(cf previewCloudfront, name string, logsWriter io.Writer) (*string, error) {
	oacID, err := aws_utils.CreateOriginAccessControl(name, cf)
	if err == nil {
		return oacID, nil
	}
	var alreadyExists *cloudfrontTypes.OriginAccessControlAlreadyExists
	if !errors.As(err, &alreadyExists) {
		return nil, fmt.Errorf("create OAC: %w", err)
	}
	existingID, err := findPreviewOriginAccessControl(cf, name)
	if err != nil {
		return nil, fmt.Errorf("find existing OAC %s: %w", name, err)
	}
	if existingID == "" {
		return nil, fmt.Errorf("create OAC: %s already exists but wasn't found", name)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Reusing origin access control %s: %s\n", name, existingID))
	return aws.String(existingID), nil
}

// findPreviewOriginAccessControl returns the id of the OAC named exactly name, or
// "" when there is none.
func findPreviewOriginAccessControl(cf previewOriginAccessControls, name string) (string, error) {
	var marker *string
	for {
		out, err := cf.ListOriginAccessControls(context.TODO(), &cloudfront.ListOriginAccessControlsInput{Marker: marker})
		if err != nil {
			return "", err
		}
		if out == nil || out.OriginAccessControlList == nil {
			return "", nil
		}
		list := out.OriginAccessControlList
		for _, oac := range list.Items {
			if aws.ToString(oac.Name) == name {
				return aws.ToString(oac.Id), nil
			}
		}
		if !aws.ToBool(list.IsTruncated) || aws.ToString(list.NextMarker) == "" {
			return "", nil
		}
		marker = list.NextMarker
	}
}

// redeployPreviewDistribution updates an existing preview distribution's routing
// to match IsSPA when it differs.
func redeployPreviewDistribution(cf previewDistributions, in StaticPreviewDeployInput, routingARN string, logsWriter io.Writer) error {
	io.WriteString(logsWriter, fmt.Sprintf("Redeployed preview to existing distribution: %s\n", in.ExistingDistID))
	changed, err := reconcileExistingPreviewDistribution(cf, in.ExistingDistID, in.IsSPA, routingARN)
	if err != nil || !changed {
		return err
	}
	if in.IsSPA {
		io.WriteString(logsWriter, fmt.Sprintf("Updated preview distribution %s to serve /index.html for client-side routes (takes a few minutes to propagate)\n", in.ExistingDistID))
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Updated preview distribution %s to serve <page>/index.html at <page>/ (takes a few minutes to propagate)\n", in.ExistingDistID))
	}
	return nil
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
		items := spaErrorResponses()
		errorResponses = &cloudfrontTypes.CustomErrorResponses{Quantity: aws.Int32(int32(len(items))), Items: items}
	}

	return &cloudfrontTypes.DistributionConfig{
		CallerReference:      aws.String(fmt.Sprintf("preview-%s-%d", previewID, time.Now().Unix())),
		Comment:              aws.String(previewDistributionComment(previewID)),
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
