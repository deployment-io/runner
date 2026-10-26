package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
)

type fakePreviewTargetGroup struct {
	inputs []*elasticloadbalancingv2.ModifyTargetGroupAttributesInput
	err    error
}

func (f *fakePreviewTargetGroup) ModifyTargetGroupAttributes(_ context.Context, in *elasticloadbalancingv2.ModifyTargetGroupAttributesInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.ModifyTargetGroupAttributesOutput, error) {
	f.inputs = append(f.inputs, in)
	return &elasticloadbalancingv2.ModifyTargetGroupAttributesOutput{}, f.err
}

// A preview's target group drains a stopping task in 5 s, not AWS's 300 s: a
// stop-then-start rollout waits for the drain before starting the new task.
func TestSetPreviewTargetGroupDeregistrationDelay(t *testing.T) {
	elb := &fakePreviewTargetGroup{}
	if err := setPreviewTargetGroupDeregistrationDelay(elb, "arn:tg"); err != nil {
		t.Fatal(err)
	}
	in := elb.inputs[0]
	if aws.ToString(in.TargetGroupArn) != "arn:tg" || len(in.Attributes) != 1 ||
		aws.ToString(in.Attributes[0].Key) != "deregistration_delay.timeout_seconds" || aws.ToString(in.Attributes[0].Value) != "5" {
		t.Errorf("input %+v", in)
	}
	elb = &fakePreviewTargetGroup{err: errors.New("AccessDenied")}
	if err := setPreviewTargetGroupDeregistrationDelay(elb, "arn:tg"); err == nil {
		t.Error("an error must fail the deploy")
	}
}
