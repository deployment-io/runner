package commands

import (
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"testing"

	"github.com/aws/smithy-go"
)

func TestIsDuplicateSecurityGroupRuleError(t *testing.T) {
	// The real production error: EC2 rejects re-authorizing an existing rule.
	duplicateErr := &smithy.GenericAPIError{
		Code:    "InvalidPermission.Duplicate",
		Message: `the specified rule "peer: 192.168.0.0/16, TCP, from port: 8081, to port: 8081, ALLOW" already exists`,
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "exact duplicate api error",
			err:  duplicateErr,
			want: true,
		},
		{
			// The AWS SDK wraps the API error inside an "operation error EC2: ..."
			// chain, which is exactly how it reaches the deploy code. errors.As must
			// unwrap it for the deploy to stay idempotent.
			name: "wrapped duplicate api error",
			err:  fmt.Errorf("operation error EC2: AuthorizeSecurityGroupIngress, https response error StatusCode: 400: %w", duplicateErr),
			want: true,
		},
		{
			name: "different api error code is not swallowed",
			err:  &smithy.GenericAPIError{Code: "InvalidGroup.NotFound", Message: "group not found"},
			want: false,
		},
		{
			name: "plain non-api error is not swallowed",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDuplicateSecurityGroupRuleError(tt.err); got != tt.want {
				t.Errorf("isDuplicateSecurityGroupRuleError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// AuthorizeSecurityGroupIngress is ALL-OR-NOTHING across its IpPermissions: one
// duplicate rejects the whole request and the others are never added. The ALB
// ingress call asks for 443 and 80 together, so a group holding only 443 fails
// it — and treating that as "already configured" leaves 80 permanently missing.
//
// This pins the retry SHAPE, which is what makes the difference: one request
// per port, so an existing 443 cannot mask a missing 80. The AWS calls
// themselves need live EC2 and are covered by a real deploy.
func TestAlbIngressRetriesEachPortSeparately(t *testing.T) {
	// One IpPermission per request is the whole point. Two ports in one request
	// is what fails when either already exists.
	for _, port := range []int64{443, 80} {
		perm := getIngressIpPermissionFromInternetToPort(port)
		if aws.ToInt32(perm.FromPort) != int32(port) || aws.ToInt32(perm.ToPort) != int32(port) {
			t.Errorf("port %d: permission covers %d-%d", port, aws.ToInt32(perm.FromPort), aws.ToInt32(perm.ToPort))
		}
		if len(perm.IpRanges) != 1 || aws.ToString(perm.IpRanges[0].CidrIp) != "0.0.0.0/0" {
			t.Errorf("port %d: expected a single 0.0.0.0/0 range, got %v", port, perm.IpRanges)
		}
	}

	// The recovery matcher must agree with what was authorized, or a rule that
	// exists is reported missing and its tags are never restored — leaving
	// every future deploy to re-enter the duplicate path.
	if got := aws.ToString(getIngressIpPermissionFromInternetToPort(80).IpProtocol); got != "tcp" {
		t.Errorf("protocol = %q, want tcp — recoverAndTagAlbSecurityGroupRule matches on tcp", got)
	}
}
