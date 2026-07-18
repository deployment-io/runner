package commands

import (
	"errors"
	"fmt"
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
