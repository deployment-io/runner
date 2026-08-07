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
// The reconcile derives its ports from ipPermissions rather than restating
// them, so adding a port to that list cannot silently leave it unreconciled.
// This pins the properties that derivation depends on; the AWS calls need live
// EC2 and are covered by a real deploy.
func TestIngressPermissionCarriesWhatTheReconcileNeeds(t *testing.T) {
	for _, port := range []int64{443, 80} {
		perm := getIngressIpPermissionFromInternetToPort(port)

		// One port per permission. Two ports in a single permission would make
		// the per-permission retry ambiguous — and is what fails when either
		// already exists.
		if aws.ToInt32(perm.FromPort) != int32(port) || aws.ToInt32(perm.ToPort) != int32(port) {
			t.Errorf("port %d: permission spans %d-%d, so a per-permission retry cannot name one port",
				port, aws.ToInt32(perm.FromPort), aws.ToInt32(perm.ToPort))
		}

		// The reconcile reads the cidr off the permission so its matcher cannot
		// drift from what was authorized. An empty IpRanges would leave it
		// matching on "" and never finding the existing rule — which means the
		// tags are never restored and every deploy re-enters this path.
		if len(perm.IpRanges) == 0 || aws.ToString(perm.IpRanges[0].CidrIp) == "" {
			t.Errorf("port %d: no cidr on the permission for the reconcile to match on", port)
		}

		// recoverAndTagAlbSecurityGroupRule matches on tcp.
		if got := aws.ToString(perm.IpProtocol); got != "tcp" {
			t.Errorf("port %d: protocol = %q, want tcp", port, got)
		}
	}
}
