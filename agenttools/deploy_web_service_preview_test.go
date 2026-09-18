package agenttools

import (
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

// TestWebServiceResourceNames pins the naming scheme and, critically, guards the
// AWS 32-char limit on target-group names for a standard 24-hex-char preview id.
func TestWebServiceResourceNames(t *testing.T) {
	const org = "org123"
	const previewID = "64f0c2a1b3d4e5f6a7b8c9d0" // 24-char Mongo object id

	if got := ecrRepositoryName(org, previewID); got != "ecr-org123-64f0c2a1b3d4e5f6a7b8c9d0" {
		t.Errorf("ecrRepositoryName = %q", got)
	}
	if got := clusterName(org); got != "ecs-org123" {
		t.Errorf("clusterName = %q", got)
	}
	if got := ecsServiceName(previewID); got != "pv-es-"+previewID {
		t.Errorf("ecsServiceName = %q", got)
	}
	if got := localImageName(org, previewID, "1700000000"); got != "org123-64f0c2a1b3d4e5f6a7b8c9d0:1700000000" {
		t.Errorf("localImageName = %q", got)
	}

	// Target-group names must stay <= 32 chars (AWS limit) for a 24-char id.
	if got := targetGroupName(previewID); len(got) > 32 {
		t.Errorf("targetGroupName %q is %d chars, exceeds AWS 32-char limit", got, len(got))
	}

	// ALB names must also stay <= 32 chars; the org id is hashed to fixed width.
	if got := albName(org); len(got) > 32 {
		t.Errorf("albName %q is %d chars, exceeds AWS 32-char limit", got, len(got))
	}
	if albName("a") == albName("b") {
		t.Errorf("albName must differ per org")
	}
	if albName(org) != albName(org) {
		t.Errorf("albName must be deterministic")
	}
	if got := orgHash(org); len(got) != 16 {
		t.Errorf("orgHash = %q, want 16 hex chars", got)
	}
}

// TestBuildPreviewWebServiceDistributionConfig pins the CloudFront config that
// makes the shared-ALB routing work: an HTTP-only custom origin at the ALB DNS
// carrying the X-Preview-Target routing header, caching off, full request
// forwarded.
func TestBuildPreviewWebServiceDistributionConfig(t *testing.T) {
	const albDns = "pv-alb-abc.us-east-1.elb.amazonaws.com"
	const previewID = "64f0c2a1b3d4e5f6a7b8c9d0"
	cfg := buildPreviewWebServiceDistributionConfig(albDns, previewID)

	if !aws.ToBool(cfg.Enabled) {
		t.Errorf("distribution not enabled")
	}
	if aws.ToInt32(cfg.Origins.Quantity) != 1 || len(cfg.Origins.Items) != 1 {
		t.Fatalf("want exactly one origin, got %d", len(cfg.Origins.Items))
	}
	origin := cfg.Origins.Items[0]
	if aws.ToString(origin.DomainName) != albDns {
		t.Errorf("origin domain = %q, want the ALB DNS %q", aws.ToString(origin.DomainName), albDns)
	}
	if origin.CustomOriginConfig == nil {
		t.Fatalf("want a CustomOriginConfig (ALB origin), got nil")
	}
	if origin.CustomOriginConfig.OriginProtocolPolicy != cloudfrontTypes.OriginProtocolPolicyHttpOnly {
		t.Errorf("origin protocol = %v, want http-only", origin.CustomOriginConfig.OriginProtocolPolicy)
	}
	if origin.S3OriginConfig != nil {
		t.Errorf("web-service origin must not be an S3/OAC origin")
	}

	// The X-Preview-Target origin custom header is the ALB routing key.
	if origin.CustomHeaders == nil || aws.ToInt32(origin.CustomHeaders.Quantity) != 1 {
		t.Fatalf("want exactly one origin custom header")
	}
	h := origin.CustomHeaders.Items[0]
	if aws.ToString(h.HeaderName) != previewTargetHeader || aws.ToString(h.HeaderValue) != previewID {
		t.Errorf("custom header = %q:%q, want %q:%q",
			aws.ToString(h.HeaderName), aws.ToString(h.HeaderValue), previewTargetHeader, previewID)
	}

	b := cfg.DefaultCacheBehavior
	if aws.ToString(b.TargetOriginId) != aws.ToString(origin.Id) {
		t.Errorf("behavior target origin %q != origin id %q", aws.ToString(b.TargetOriginId), aws.ToString(origin.Id))
	}
	if aws.ToString(b.CachePolicyId) != cachingDisabledPolicyID {
		t.Errorf("cache policy = %q, want CachingDisabled", aws.ToString(b.CachePolicyId))
	}
	if aws.ToString(b.OriginRequestPolicyId) != allViewerOriginRequestPolicyID {
		t.Errorf("origin request policy = %q, want AllViewer", aws.ToString(b.OriginRequestPolicyId))
	}
	// A web service needs write methods, not just GET/HEAD.
	if aws.ToInt32(b.AllowedMethods.Quantity) != 7 {
		t.Errorf("allowed methods = %d, want all 7", aws.ToInt32(b.AllowedMethods.Quantity))
	}
}

// TestToBuildArgPointers covers the map[string]string -> map[string]*string
// adaptation docker expects (nil for empty, distinct pointers per value).
func TestToBuildArgPointers(t *testing.T) {
	if got := toBuildArgPointers(nil); got != nil {
		t.Fatalf("toBuildArgPointers(nil) = %v, want nil", got)
	}
	args := map[string]string{"FOO": "1", "BAR": "2"}
	got := toBuildArgPointers(args)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got["FOO"] == nil || *got["FOO"] != "1" || got["BAR"] == nil || *got["BAR"] != "2" {
		t.Fatalf("values not adapted correctly: %v", got)
	}
	// Each entry must point at its own value, not a shared loop variable.
	if got["FOO"] == got["BAR"] {
		t.Fatalf("build-arg pointers alias the same address")
	}
}

// TestDeployWebServicePreviewValidation covers the two guards that fire before
// any AWS client is touched, so they are safe to exercise with a zero-value input.
func TestDeployWebServicePreviewValidation(t *testing.T) {
	if _, err := DeployWebServicePreview(WebServicePreviewDeployInput{}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "port") {
		t.Fatalf("missing port: got err %v, want a port error", err)
	}

	_, err := DeployWebServicePreview(WebServicePreviewDeployInput{
		Port:            8080,
		BuildContextDir: "/nonexistent-preview-context-xyz",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile") {
		t.Fatalf("missing Dockerfile: got err %v, want a Dockerfile error", err)
	}
}

// TestStreamDockerJSONLogs covers surfacing a build/push error line and copying
// stream/status text through.
func TestStreamDockerJSONLogs(t *testing.T) {
	var buf strings.Builder
	ok := `{"stream":"Step 1/2\n"}
{"status":"Pushing"}`
	if err := streamDockerJSONLogs(strings.NewReader(ok), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Step 1/2") || !strings.Contains(buf.String(), "Pushing") {
		t.Fatalf("stream/status not copied: %q", buf.String())
	}

	fail := `{"stream":"Step 1/2\n"}
{"errorDetail":{"message":"boom"},"error":"boom"}`
	if err := streamDockerJSONLogs(strings.NewReader(fail), io.Discard); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("error line not surfaced: got %v", err)
	}
}
