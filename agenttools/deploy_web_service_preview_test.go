package agenttools

import (
	"io"
	"strings"
	"testing"
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
