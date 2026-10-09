package aws_utils

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

// AssociateFunctionToCloudfrontDistribution adds a CloudFront Function association
// for eventType to the distribution config's DefaultCacheBehavior, unless that
// exact function is already associated for that event. It reports whether the
// config changed. Shared by the production function commands and the preview
// deploy (agenttools).
func AssociateFunctionToCloudfrontDistribution(distributionConfig *cloudfrontTypes.DistributionConfig,
	functionARN *string, eventType cloudfrontTypes.EventType) bool {
	functionAssociations := distributionConfig.DefaultCacheBehavior.FunctionAssociations
	items := functionAssociations.Items
	quantity := functionAssociations.Quantity
	associate := true
	for _, item := range items {
		//check if already associated
		associatedArn := aws.ToString(item.FunctionARN)
		newArn := aws.ToString(functionARN)
		if (associatedArn == newArn) && (item.EventType == eventType) {
			associate = false
		}
	}
	if !associate {
		return false
	}

	var q int32
	if quantity == nil {
		q = 0
	} else {
		q = aws.ToInt32(quantity)
	}
	q++
	quantity = aws.Int32(q)
	items = append(items, cloudfrontTypes.FunctionAssociation{
		EventType:   eventType,
		FunctionARN: functionARN,
	})
	functionAssociations = &cloudfrontTypes.FunctionAssociations{
		Quantity: quantity,
		Items:    items,
	}
	distributionConfig.DefaultCacheBehavior.FunctionAssociations = functionAssociations

	return true
}
