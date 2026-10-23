package agenttools

// deploy_web_preview_cloudfront.go is the CloudFront half of deploy_web_service_preview:
// a distribution in front of the preview's load balancer, so the backend preview has
// an https URL. Created on a preview's first deploy and left as it is afterwards (its
// origin, the load balancer named after the preview, doesn't change).

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

// allViewerOriginRequestPolicyID is CloudFront's AWS-managed "AllViewer" origin
// request policy: every viewer header, cookie and query string reaches the backend.
// AWS-managed, so a global constant like cachingDisabledPolicyID.
const allViewerOriginRequestPolicyID = "216adef6-5c7f-47e4-b989-5492eafa07d3"

// webPreviewOriginID is the id of the distribution's one origin, the load balancer.
const webPreviewOriginID = "preview-load-balancer"

// webPreviewDistribution is a created distribution's id, ARN and domain.
type webPreviewDistribution struct {
	id, arn, domain string
}

// createWebPreviewDistribution creates the backend preview's distribution in front of
// the load balancer at loadBalancerDNS, without waiting for it to deploy.
func createWebPreviewDistribution(ctx context.Context, cf WebPreviewCloudFront, previewID, loadBalancerDNS string, logs io.Writer) (webPreviewDistribution, error) {
	if loadBalancerDNS == "" {
		return webPreviewDistribution{}, fmt.Errorf("the preview's load balancer has no DNS name")
	}
	io.WriteString(logs, "Creating the backend preview's CloudFront distribution\n")
	out, err := cf.CreateDistribution(ctx, &cloudfront.CreateDistributionInput{
		DistributionConfig: buildWebPreviewDistributionConfig(previewID, loadBalancerDNS, time.Now()),
	})
	if err != nil {
		return webPreviewDistribution{}, fmt.Errorf("create the preview's CloudFront distribution: %w", err)
	}
	if out == nil || out.Distribution == nil {
		return webPreviewDistribution{}, fmt.Errorf("create the preview's CloudFront distribution: no distribution in the reply")
	}
	d := webPreviewDistribution{
		id:     aws.ToString(out.Distribution.Id),
		arn:    aws.ToString(out.Distribution.ARN),
		domain: aws.ToString(out.Distribution.DomainName),
	}
	io.WriteString(logs, fmt.Sprintf("Preview distribution created: %s (propagating)\n", d.id))
	return d, nil
}

// buildWebPreviewDistributionConfig is the backend preview's distribution: the load
// balancer as its one origin over http, every method forwarded with everything the
// viewer sent, nothing cached, viewers redirected to https.
func buildWebPreviewDistributionConfig(previewID, loadBalancerDNS string, now time.Time) *cloudfrontTypes.DistributionConfig {
	return &cloudfrontTypes.DistributionConfig{
		CallerReference: aws.String(fmt.Sprintf("preview-%s-%d", previewID, now.Unix())),
		Comment:         aws.String("deployment.io backend preview " + previewID),
		Enabled:         aws.Bool(true),
		Origins: &cloudfrontTypes.Origins{
			Quantity: aws.Int32(1),
			Items: []cloudfrontTypes.Origin{{
				Id:         aws.String(webPreviewOriginID),
				DomainName: aws.String(loadBalancerDNS),
				CustomOriginConfig: &cloudfrontTypes.CustomOriginConfig{
					HTTPPort:             aws.Int32(80),
					HTTPSPort:            aws.Int32(443),
					OriginProtocolPolicy: cloudfrontTypes.OriginProtocolPolicyHttpOnly,
				},
			}},
		},
		DefaultCacheBehavior: &cloudfrontTypes.DefaultCacheBehavior{
			TargetOriginId:       aws.String(webPreviewOriginID),
			ViewerProtocolPolicy: cloudfrontTypes.ViewerProtocolPolicyRedirectToHttps,
			AllowedMethods: &cloudfrontTypes.AllowedMethods{
				Quantity: aws.Int32(7),
				Items: []cloudfrontTypes.Method{
					cloudfrontTypes.MethodGet, cloudfrontTypes.MethodHead, cloudfrontTypes.MethodOptions,
					cloudfrontTypes.MethodPut, cloudfrontTypes.MethodPost, cloudfrontTypes.MethodPatch,
					cloudfrontTypes.MethodDelete,
				},
				CachedMethods: &cloudfrontTypes.CachedMethods{
					Quantity: aws.Int32(2),
					Items:    []cloudfrontTypes.Method{cloudfrontTypes.MethodGet, cloudfrontTypes.MethodHead},
				},
			},
			CachePolicyId:         aws.String(cachingDisabledPolicyID),
			OriginRequestPolicyId: aws.String(allViewerOriginRequestPolicyID),
			Compress:              aws.Bool(false),
		},
		// As buildPreviewDistributionConfig: a local edge everywhere, since every
		// request goes through to the origin.
		PriceClass: cloudfrontTypes.PriceClassPriceClassAll,
	}
}
