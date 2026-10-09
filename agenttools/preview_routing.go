package agenttools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
)

// previewRoutingFunctionName is the ONE CloudFront Function every non-SPA preview
// in an AWS account shares. CloudFront Functions are account-wide with a default
// quota of 100, and previews are many and short-lived, so a per-preview function
// would exhaust the quota. Its code is fixed by its name: it is never updated or
// deleted (preview teardown only deletes functions named after the deployment it
// removes). A code change ships under a new name (…-v2).
const previewRoutingFunctionName = "deployment-io-preview-routing-v1"

// previewRoutingFunctionCode serves <page>/index.html at <page>/ and redirects
// <page> to <page>/ — the same non-SPA statement production's viewer-request
// function (getViewerRequestFunctionCode) runs, minus the domain redirects.
// Unlike production's buildQS, this one keeps every value of a repeated key
// (?tag=a&tag=b), which CloudFront delivers in multiValue.
const previewRoutingFunctionCode = `function buildQS(querystring) {
    var parts = [];
    Object.keys(querystring).forEach(function(k) {
        var entry = querystring[k];
        if (entry.multiValue) {
            entry.multiValue.forEach(function(mv) {
                parts.push(k + '=' + mv.value);
            });
        } else {
            parts.push(k + '=' + entry.value);
        }
    });
    return parts.length ? '?' + parts.join('&') : '';
}

function handler(event) {
    var request = event.request;
    if (request.uri.endsWith('/')) {
        request.uri += 'index.html';
    } else if (!request.uri.includes('.')) {
        var host = request.headers["host"].value;
        var location = 'https://' + host + request.uri + '/' + buildQS(request.querystring);
        return { statusCode: 301, statusDescription: 'Moved Permanently', headers: { 'location': { value: location } } };
    }
    return request;
}`

// previewFunctions is the CloudFront Functions API ensurePreviewRoutingFunction uses.
type previewFunctions interface {
	DescribeFunction(ctx context.Context, params *cloudfront.DescribeFunctionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error)
	CreateFunction(ctx context.Context, params *cloudfront.CreateFunctionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.CreateFunctionOutput, error)
	PublishFunction(ctx context.Context, params *cloudfront.PublishFunctionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.PublishFunctionOutput, error)
}

// previewDistributions is the CloudFront distribution API the preview deploy uses.
type previewDistributions interface {
	CreateDistribution(ctx context.Context, params *cloudfront.CreateDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error)
	GetDistributionConfig(ctx context.Context, params *cloudfront.GetDistributionConfigInput, optFns ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error)
	UpdateDistribution(ctx context.Context, params *cloudfront.UpdateDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error)
}

// previewCloudfront is the subset of *cloudfront.Client the preview deploy uses,
// so tests can fake it.
type previewCloudfront interface {
	aws_utils.OriginAccessControlCreator
	previewFunctions
	previewDistributions
}

// describePreviewRoutingFunction returns the function at stage, or found=false
// when it doesn't exist there.
func describePreviewRoutingFunction(cf previewFunctions, stage cloudfrontTypes.FunctionStage) (out *cloudfront.DescribeFunctionOutput, found bool, err error) {
	out, err = cf.DescribeFunction(context.TODO(), &cloudfront.DescribeFunctionInput{
		Name:  aws.String(previewRoutingFunctionName),
		Stage: stage,
	})
	if err != nil {
		var notFound *cloudfrontTypes.NoSuchFunctionExists
		if errors.As(err, &notFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

func functionARN(summary *cloudfrontTypes.FunctionSummary) string {
	if summary == nil || summary.FunctionMetadata == nil {
		return ""
	}
	return aws.ToString(summary.FunctionMetadata.FunctionARN)
}

// ensurePreviewRoutingFunction makes sure the shared preview routing function is
// published and returns its LIVE ARN. It only ever creates and publishes — never
// updates or deletes. A function an earlier failure left created but unpublished
// is published here; a concurrent preview creating or publishing it at the same
// time is not an error.
func ensurePreviewRoutingFunction(cf previewFunctions) (string, error) {
	live, found, err := describePreviewRoutingFunction(cf, cloudfrontTypes.FunctionStageLive)
	if err != nil {
		return "", fmt.Errorf("describe preview routing function: %w", err)
	}
	if found {
		return functionARN(live.FunctionSummary), nil
	}

	arn, err := createAndPublishPreviewRoutingFunction(cf)
	if err == nil && arn != "" {
		return arn, nil
	}
	// Whatever went wrong, a LIVE stage that exists now (e.g. a concurrent preview
	// published it first) is all this needs.
	if live, found, descErr := describePreviewRoutingFunction(cf, cloudfrontTypes.FunctionStageLive); descErr == nil && found {
		return functionARN(live.FunctionSummary), nil
	}
	if err == nil {
		err = errors.New("published function has no ARN")
	}
	return "", fmt.Errorf("publish preview routing function %s: %w", previewRoutingFunctionName, err)
}

func createAndPublishPreviewRoutingFunction(cf previewFunctions) (string, error) {
	dev, found, err := describePreviewRoutingFunction(cf, cloudfrontTypes.FunctionStageDevelopment)
	if err != nil {
		return "", err
	}
	etag := ""
	if found {
		etag = aws.ToString(dev.ETag)
	} else {
		created, err := cf.CreateFunction(context.TODO(), &cloudfront.CreateFunctionInput{
			Name:         aws.String(previewRoutingFunctionName),
			FunctionCode: []byte(previewRoutingFunctionCode),
			FunctionConfig: &cloudfrontTypes.FunctionConfig{
				Comment: aws.String("deployment.io previews: serve <page>/index.html at <page>/"),
				Runtime: cloudfrontTypes.FunctionRuntimeCloudfrontJs10,
			},
		})
		var alreadyExists *cloudfrontTypes.FunctionAlreadyExists
		switch {
		case err == nil:
			etag = aws.ToString(created.ETag)
		case errors.As(err, &alreadyExists):
			// A concurrent preview created it first — publish theirs.
			dev, found, err = describePreviewRoutingFunction(cf, cloudfrontTypes.FunctionStageDevelopment)
			if err != nil {
				return "", err
			}
			if !found {
				return "", errors.New("function reported as existing but not found")
			}
			etag = aws.ToString(dev.ETag)
		default:
			return "", err
		}
	}

	published, err := cf.PublishFunction(context.TODO(), &cloudfront.PublishFunctionInput{
		Name:    aws.String(previewRoutingFunctionName),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		return "", err
	}
	return functionARN(published.FunctionSummary), nil
}

// isPreviewRoutingFunctionARN matches the shared function by name, so an SPA
// preview can drop the association without first ensuring the function exists.
func isPreviewRoutingFunctionARN(arn string) bool {
	return strings.HasSuffix(arn, ":function/"+previewRoutingFunctionName)
}

// spaErrorResponses rewrite 403/404 to /index.html (200) so an SPA's client-side
// routes resolve. ErrorCachingMinTTL 0 so a redeploy's changes show immediately
// (the default is 300s), matching the no-cache CachingDisabled policy.
func spaErrorResponses() []cloudfrontTypes.CustomErrorResponse {
	return []cloudfrontTypes.CustomErrorResponse{
		{ErrorCode: aws.Int32(403), ResponsePagePath: aws.String("/index.html"), ResponseCode: aws.String("200"), ErrorCachingMinTTL: aws.Int64(0)},
		{ErrorCode: aws.Int32(404), ResponsePagePath: aws.String("/index.html"), ResponseCode: aws.String("200"), ErrorCachingMinTTL: aws.Int64(0)},
	}
}

func isSPAErrorResponse(r cloudfrontTypes.CustomErrorResponse) bool {
	code := aws.ToInt32(r.ErrorCode)
	return (code == 403 || code == 404) && aws.ToString(r.ResponsePagePath) == "/index.html"
}

func sameSPAErrorResponse(a, b cloudfrontTypes.CustomErrorResponse) bool {
	return aws.ToInt32(a.ErrorCode) == aws.ToInt32(b.ErrorCode) &&
		aws.ToString(a.ResponsePagePath) == aws.ToString(b.ResponsePagePath) &&
		aws.ToString(a.ResponseCode) == aws.ToString(b.ResponseCode) &&
		aws.ToInt64(a.ErrorCachingMinTTL) == aws.ToInt64(b.ErrorCachingMinTTL)
}

// reconcilePreviewRouting brings a preview distribution's config in line with
// isSPA and reports whether it changed:
//   - not SPA: the shared routing function (routingARN) associated as
//     viewer-request on DefaultCacheBehavior; no 403/404 → /index.html responses.
//   - SPA: no association of the shared function; the 403/404 → /index.html
//     responses present.
//
// Any other function association or error response is left alone.
func reconcilePreviewRouting(config *cloudfrontTypes.DistributionConfig, isSPA bool, routingARN string) bool {
	associationChanged := reconcileRoutingAssociation(config, isSPA, routingARN)
	responsesChanged := reconcileSPAErrorResponses(config, isSPA)
	return associationChanged || responsesChanged
}

func reconcileRoutingAssociation(config *cloudfrontTypes.DistributionConfig, isSPA bool, routingARN string) bool {
	if config.DefaultCacheBehavior.FunctionAssociations == nil {
		config.DefaultCacheBehavior.FunctionAssociations = &cloudfrontTypes.FunctionAssociations{Quantity: aws.Int32(0)}
	}
	if !isSPA {
		return aws_utils.AssociateFunctionToCloudfrontDistribution(config, aws.String(routingARN), cloudfrontTypes.EventTypeViewerRequest)
	}
	var kept []cloudfrontTypes.FunctionAssociation
	for _, item := range config.DefaultCacheBehavior.FunctionAssociations.Items {
		if !isPreviewRoutingFunctionARN(aws.ToString(item.FunctionARN)) {
			kept = append(kept, item)
		}
	}
	if len(kept) == len(config.DefaultCacheBehavior.FunctionAssociations.Items) {
		return false
	}
	config.DefaultCacheBehavior.FunctionAssociations = &cloudfrontTypes.FunctionAssociations{
		Quantity: aws.Int32(int32(len(kept))), Items: kept,
	}
	return true
}

// reconcileSPAErrorResponses adds (SPA) or removes (not SPA) the 403/404 →
// /index.html rewrite, keeping every other error response.
func reconcileSPAErrorResponses(config *cloudfrontTypes.DistributionConfig, isSPA bool) bool {
	var responses []cloudfrontTypes.CustomErrorResponse
	if config.CustomErrorResponses != nil {
		responses = config.CustomErrorResponses.Items
	}
	var kept []cloudfrontTypes.CustomErrorResponse
	changed := false
	if isSPA {
		// CloudFront allows one response per error code: keep everything but
		// 403/404, and replace those unless both already are the SPA rewrite.
		want := spaErrorResponses()
		matched := 0
		for _, r := range responses {
			switch aws.ToInt32(r.ErrorCode) {
			case 403:
				if sameSPAErrorResponse(r, want[0]) {
					matched++
				}
			case 404:
				if sameSPAErrorResponse(r, want[1]) {
					matched++
				}
			default:
				kept = append(kept, r)
			}
		}
		if matched != len(want) {
			kept = append(kept, want...)
			changed = true
		}
	} else {
		for _, r := range responses {
			if isSPAErrorResponse(r) {
				changed = true
				continue
			}
			kept = append(kept, r)
		}
	}
	if changed || config.CustomErrorResponses == nil {
		config.CustomErrorResponses = &cloudfrontTypes.CustomErrorResponses{
			Quantity: aws.Int32(int32(len(kept))), Items: kept,
		}
	}
	return changed
}

// reconcileExistingPreviewDistribution applies reconcilePreviewRouting to a
// deployed preview distribution, updating it only when its config changed.
func reconcileExistingPreviewDistribution(cf previewDistributions, distID string, isSPA bool, routingARN string) (bool, error) {
	got, err := cf.GetDistributionConfig(context.TODO(), &cloudfront.GetDistributionConfigInput{Id: aws.String(distID)})
	if err != nil {
		return false, fmt.Errorf("get preview distribution config: %w", err)
	}
	if !reconcilePreviewRouting(got.DistributionConfig, isSPA, routingARN) {
		return false, nil
	}
	if _, err = cf.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
		Id:                 aws.String(distID),
		IfMatch:            got.ETag,
		DistributionConfig: got.DistributionConfig,
	}); err != nil {
		return false, fmt.Errorf("update preview distribution routing: %w", err)
	}
	return true, nil
}
