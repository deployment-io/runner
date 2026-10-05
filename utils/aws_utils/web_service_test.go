package aws_utils

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
)

// TestEnvMapToKeyValuePairs covers deterministic (key-sorted) conversion and the
// empty -> nil case (so a no-env container definition stays byte-stable).
func TestEnvMapToKeyValuePairs(t *testing.T) {
	if got := envMapToKeyValuePairs(nil); got != nil {
		t.Fatalf("empty map -> %v, want nil", got)
	}
	pairs := envMapToKeyValuePairs(map[string]string{"ZED": "z", "ALPHA": "a", "MID": "m"})
	if len(pairs) != 3 {
		t.Fatalf("len = %d, want 3", len(pairs))
	}
	wantKeys := []string{"ALPHA", "MID", "ZED"} // sorted
	wantVals := []string{"a", "m", "z"}
	for i, p := range pairs {
		if aws.ToString(p.Name) != wantKeys[i] || aws.ToString(p.Value) != wantVals[i] {
			t.Errorf("pair[%d] = (%q,%q), want (%q,%q)", i,
				aws.ToString(p.Name), aws.ToString(p.Value), wantKeys[i], wantVals[i])
		}
	}
}

// TestApiErrCodeIs covers the idempotency helper matching an AWS error code.
func TestApiErrCodeIs(t *testing.T) {
	dup := &smithy.GenericAPIError{Code: "InvalidPermission.Duplicate", Message: "exists"}
	if !apiErrCodeIs(dup, "InvalidPermission.Duplicate") {
		t.Errorf("expected match on exact code")
	}
	if !apiErrCodeIs(dup, "Other", "InvalidPermission.Duplicate") {
		t.Errorf("expected match when code is among the list")
	}
	if apiErrCodeIs(dup, "InvalidGroup.Duplicate") {
		t.Errorf("unexpected match on a different code")
	}
	if apiErrCodeIs(nil, "InvalidPermission.Duplicate") {
		t.Errorf("nil error should not match")
	}
	if apiErrCodeIs(errors.New("plain error"), "InvalidPermission.Duplicate") {
		t.Errorf("non-API error should not match")
	}
}
