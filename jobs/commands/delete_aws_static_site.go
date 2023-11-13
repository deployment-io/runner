package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"io"
	"time"
)

type DeleteAwsStaticSite struct {
}

func (d *DeleteAwsStaticSite) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	//For static site

	//delete cloudfront distribution
	cloudfrontDistributionId, err := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID)
	if err != nil {
		return parameters, err
	}
	cloudfrontClient, err := getCloudfrontClient(parameters, cloudfrontRegion)
	if err != nil {
		return parameters, err
	}
	distributionConfigOutput, err := cloudfrontClient.GetDistributionConfig(context.TODO(), &cloudfront.GetDistributionConfigInput{
		Id: aws.String(cloudfrontDistributionId),
	})

	if err != nil {
		return parameters, err
	}
	if distributionConfigOutput == nil {
		return parameters, fmt.Errorf("distribution doesn't exists")
	}
	distributionConfig := distributionConfigOutput.DistributionConfig
	distributionConfig.Enabled = aws.Bool(false)
	//disable distribution
	updateDistributionOutput, err := cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
		DistributionConfig: distributionConfig,
		Id:                 aws.String(cloudfrontDistributionId),
		IfMatch:            distributionConfigOutput.ETag,
	})
	if err != nil {
		return parameters, err
	}
	waiter := cloudfront.NewDistributionDeployedWaiter(cloudfrontClient)
	err = waiter.Wait(context.TODO(), &cloudfront.GetDistributionInput{Id: aws.String(cloudfrontDistributionId)}, 10*time.Minute)
	if err != nil {
		return parameters, err
	}
	_, err = cloudfrontClient.DeleteDistribution(context.TODO(), &cloudfront.DeleteDistributionInput{
		Id:      aws.String(cloudfrontDistributionId),
		IfMatch: updateDistributionOutput.ETag,
	})
	if err != nil {
		return parameters, err
	}

	//delete cache policy
	cachePolicyId := distributionConfig.DefaultCacheBehavior.CachePolicyId
	getCachePolicyOutput, err := cloudfrontClient.GetCachePolicy(context.TODO(), &cloudfront.GetCachePolicyInput{Id: cachePolicyId})
	if err != nil {
		return parameters, err
	}
	_, _ = cloudfrontClient.DeleteCachePolicy(context.TODO(), &cloudfront.DeleteCachePolicyInput{
		Id:      cachePolicyId,
		IfMatch: getCachePolicyOutput.ETag,
	})

	//delete cf functions if needed
	err = deleteCloudfrontFunctions(parameters, cloudfrontClient)
	if err != nil {
		return parameters, err
	}

	//delete origin access for each origin if needed
	if distributionConfig.Origins != nil {
		for _, origin := range distributionConfig.Origins.Items {
			if origin.OriginAccessControlId != nil {
				_ = deleteOriginAccessControl(origin.OriginAccessControlId, cloudfrontClient)
			}
		}
	}

	//delete S3 bucket
	s3Client, err := getS3Client(parameters)
	if err != nil {
		return parameters, err
	}
	bucketName, err := getBucketName(parameters)
	if err != nil {
		return parameters, err
	}
	err = deleteAllS3Files(s3Client, bucketName)
	if err != nil {
		return parameters, err
	}
	_, err = s3Client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return parameters, err
	}

	//update deployment to deleted and delete domain
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return parameters, err
	}
	updateDeploymentsPipeline.Add(updateDeploymentsKey, deployments.UpdateDeploymentDtoV1{
		ID:            deploymentID,
		DeletionState: deployment_enums.DeletionDone,
	})
	return parameters, err
}

func deleteOriginAccessControl(originAccessControlId *string, cloudFrontClient *cloudfront.Client) error {
	getOriginAccessControlOutput, err := cloudFrontClient.GetOriginAccessControl(context.TODO(), &cloudfront.GetOriginAccessControlInput{Id: originAccessControlId})
	if err != nil {
		return err
	}
	_, err = cloudFrontClient.DeleteOriginAccessControl(context.TODO(), &cloudfront.DeleteOriginAccessControlInput{
		Id:      originAccessControlId,
		IfMatch: getOriginAccessControlOutput.ETag,
	})

	if err != nil {
		return err
	}

	return nil
}

func deleteCloudfrontFunctions(parameters map[string]interface{}, cloudfrontClient *cloudfront.Client) error {
	viewerResponseFunctionName, err := getViewerResponseFunctionName(parameters)
	if err != nil {
		return err
	}
	err = deleteCloudfrontFunction(cloudfrontClient, viewerResponseFunctionName)
	if err != nil {
		return err
	}
	viewerRequestFunctionName, err := getViewerRequestFunctionName(parameters)
	if err != nil {
		return err
	}
	err = deleteCloudfrontFunction(cloudfrontClient, viewerRequestFunctionName)
	if err != nil {
		return err
	}
	return nil
}

func deleteCloudfrontFunction(cloudfrontClient *cloudfront.Client, functionName string) error {
	describeFunctionOutput, _ := cloudfrontClient.DescribeFunction(context.TODO(), &cloudfront.DescribeFunctionInput{
		Name: aws.String(functionName),
	})
	if describeFunctionOutput != nil {
		_, err := cloudfrontClient.DeleteFunction(context.TODO(), &cloudfront.DeleteFunctionInput{
			IfMatch: describeFunctionOutput.ETag,
			Name:    aws.String(functionName),
		})
		if err != nil {
			return err
		}
	}
	return nil
}
