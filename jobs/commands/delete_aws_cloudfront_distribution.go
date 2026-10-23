package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

// deleteCloudfrontDistributionAPI is the part of *cloudfront.Client deleting a
// distribution uses. GetDistribution is what the deployed waiter polls.
type deleteCloudfrontDistributionAPI interface {
	GetDistributionConfig(ctx context.Context, params *cloudfront.GetDistributionConfigInput, optFns ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error)
	UpdateDistribution(ctx context.Context, params *cloudfront.UpdateDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error)
	GetDistribution(ctx context.Context, params *cloudfront.GetDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error)
	DeleteDistribution(ctx context.Context, params *cloudfront.DeleteDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error)
}

// deleteCloudfrontDistribution disables the distribution (unless it already is),
// waits for the change to deploy and deletes it. It returns the distribution's
// config so the caller can clean up what the distribution used. A distribution
// that doesn't exist fails with *cloudfrontTypes.NoSuchDistribution.
func deleteCloudfrontDistribution(cloudfrontClient deleteCloudfrontDistributionAPI, distributionID string, logsWriter io.Writer) (*cloudfrontTypes.DistributionConfig, error) {
	distributionConfigOutput, err := cloudfrontClient.GetDistributionConfig(context.TODO(), &cloudfront.GetDistributionConfigInput{
		Id: aws.String(distributionID),
	})
	if err != nil {
		return nil, err
	}
	if distributionConfigOutput == nil || distributionConfigOutput.DistributionConfig == nil {
		return nil, fmt.Errorf("distribution doesn't exists")
	}
	distributionConfig := distributionConfigOutput.DistributionConfig
	eTag := distributionConfigOutput.ETag
	if aws.ToBool(distributionConfig.Enabled) {
		distributionConfig.Enabled = aws.Bool(false)
		//disable distribution
		io.WriteString(logsWriter, fmt.Sprintf("Disabling cloudfront distribution: %s\n", distributionID))
		updateDistributionOutput, err := cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
			DistributionConfig: distributionConfig,
			Id:                 aws.String(distributionID),
			IfMatch:            eTag,
		})
		if err != nil {
			return nil, err
		}
		eTag = updateDistributionOutput.ETag
	}
	waiter := cloudfront.NewDistributionDeployedWaiter(cloudfrontClient)
	err = waiter.Wait(context.TODO(), &cloudfront.GetDistributionInput{Id: aws.String(distributionID)}, 20*time.Minute,
		func(o *cloudfront.DistributionDeployedWaiterOptions) {
			// The default retries every GetDistribution error and times out, losing a
			// NoSuchDistribution (the distribution deleted meanwhile); return it as is.
			defaultRetryable := o.Retryable
			o.Retryable = func(ctx context.Context, in *cloudfront.GetDistributionInput, out *cloudfront.GetDistributionOutput, err error) (bool, error) {
				var noSuchDistribution *cloudfrontTypes.NoSuchDistribution
				if errors.As(err, &noSuchDistribution) {
					return false, err
				}
				return defaultRetryable(ctx, in, out, err)
			}
		})
	if err != nil {
		return nil, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting cloudfront distribution: %s\n", distributionID))
	_, err = cloudfrontClient.DeleteDistribution(context.TODO(), &cloudfront.DeleteDistributionInput{
		Id:      aws.String(distributionID),
		IfMatch: eTag,
	})
	if err != nil {
		return nil, err
	}
	return distributionConfig, nil
}
