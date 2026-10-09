package aws_utils

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

func configWithAssociations(items ...cloudfrontTypes.FunctionAssociation) *cloudfrontTypes.DistributionConfig {
	assoc := &cloudfrontTypes.FunctionAssociations{Items: items}
	if len(items) > 0 {
		assoc.Quantity = aws.Int32(int32(len(items)))
	}
	return &cloudfrontTypes.DistributionConfig{
		DefaultCacheBehavior: &cloudfrontTypes.DefaultCacheBehavior{FunctionAssociations: assoc},
	}
}

func TestAssociateFunctionToCloudfrontDistribution_AddsWhenAbsent(t *testing.T) {
	config := configWithAssociations()
	if !AssociateFunctionToCloudfrontDistribution(config, aws.String("arn:fn"), cloudfrontTypes.EventTypeViewerRequest) {
		t.Fatal("changed = false; want true for a new association")
	}
	got := config.DefaultCacheBehavior.FunctionAssociations
	if aws.ToInt32(got.Quantity) != 1 || len(got.Items) != 1 ||
		aws.ToString(got.Items[0].FunctionARN) != "arn:fn" || got.Items[0].EventType != cloudfrontTypes.EventTypeViewerRequest {
		t.Fatalf("associations = %+v", got)
	}
}

func TestAssociateFunctionToCloudfrontDistribution_NoOpWhenAlreadyAssociated(t *testing.T) {
	config := configWithAssociations(cloudfrontTypes.FunctionAssociation{
		FunctionARN: aws.String("arn:fn"), EventType: cloudfrontTypes.EventTypeViewerRequest,
	})
	before := config.DefaultCacheBehavior.FunctionAssociations
	if AssociateFunctionToCloudfrontDistribution(config, aws.String("arn:fn"), cloudfrontTypes.EventTypeViewerRequest) {
		t.Fatal("changed = true; want false for an existing association")
	}
	if config.DefaultCacheBehavior.FunctionAssociations != before {
		t.Fatal("associations replaced although nothing changed")
	}
}

func TestAssociateFunctionToCloudfrontDistribution_AppendsAlongsideOthers(t *testing.T) {
	config := configWithAssociations(
		cloudfrontTypes.FunctionAssociation{FunctionARN: aws.String("arn:other"), EventType: cloudfrontTypes.EventTypeViewerResponse},
		cloudfrontTypes.FunctionAssociation{FunctionARN: aws.String("arn:fn"), EventType: cloudfrontTypes.EventTypeViewerResponse},
	)
	if !AssociateFunctionToCloudfrontDistribution(config, aws.String("arn:fn"), cloudfrontTypes.EventTypeViewerRequest) {
		t.Fatal("changed = false; same ARN on another event type is a new association")
	}
	got := config.DefaultCacheBehavior.FunctionAssociations
	if aws.ToInt32(got.Quantity) != 3 || len(got.Items) != 3 ||
		aws.ToString(got.Items[0].FunctionARN) != "arn:other" || got.Items[2].EventType != cloudfrontTypes.EventTypeViewerRequest {
		t.Fatalf("associations = %+v", got)
	}
}
