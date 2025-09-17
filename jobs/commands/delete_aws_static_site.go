package commands

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"

	"io"
	"time"
)

type DeleteAwsStaticSite struct {
}

func (d *DeleteAwsStaticSite) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	//For static site
	io.WriteString(logsWriter, fmt.Sprintf("Deleting static site on AWS\n"))
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return parameters, err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}
	if !isPreview(parameters) {
		//update deployment to deleted and delete domain
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:            deploymentID,
			DeletionState: deployment_enums.DeletionInProcess,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:            previewID,
			DeletionState: deployment_enums.DeletionInProcess,
		})
	}
	//delete cloudfront distribution
	cloudfrontDistributionId, err := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID)
	if err != nil {
		return parameters, err
	}
	cloudfrontClient, err := cloud_api_clients.GetCloudfrontClient(parameters, cloudfrontRegion)
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
	io.WriteString(logsWriter, fmt.Sprintf("Disabling cloudfront distribution: %s\n", cloudfrontDistributionId))
	updateDistributionOutput, err := cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
		DistributionConfig: distributionConfig,
		Id:                 aws.String(cloudfrontDistributionId),
		IfMatch:            distributionConfigOutput.ETag,
	})
	if err != nil {
		return parameters, err
	}
	waiter := cloudfront.NewDistributionDeployedWaiter(cloudfrontClient)
	err = waiter.Wait(context.TODO(), &cloudfront.GetDistributionInput{Id: aws.String(cloudfrontDistributionId)}, 20*time.Minute)
	if err != nil {
		return parameters, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting cloudfront distribution: %s\n", cloudfrontDistributionId))
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
	io.WriteString(logsWriter, fmt.Sprintf("Deleting cache policy: %s\n", aws.ToString(cachePolicyId)))
	_, _ = cloudfrontClient.DeleteCachePolicy(context.TODO(), &cloudfront.DeleteCachePolicyInput{
		Id:      cachePolicyId,
		IfMatch: getCachePolicyOutput.ETag,
	})

	//delete cf functions if needed
	err = deleteCloudfrontFunctions(parameters, cloudfrontClient, logsWriter)
	if err != nil {
		return parameters, err
	}

	//delete origin access for each origin if needed
	if distributionConfig.Origins != nil {
		for _, origin := range distributionConfig.Origins.Items {
			if origin.OriginAccessControlId != nil {
				_ = deleteOriginAccessControl(origin.OriginAccessControlId, cloudfrontClient, logsWriter)
			}
		}
	}

	//delete S3 bucket
	s3Client, err := cloud_api_clients.GetS3Client(parameters)
	if err != nil {
		return parameters, err
	}
	bucketName, err := getBucketName(parameters)
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Deleting all files in S3 bucket: %s\n", bucketName))
	err = deleteAllS3Files(s3Client, bucketName)
	if err != nil {
		return parameters, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting S3 bucket: %s\n", bucketName))
	_, err = s3Client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return parameters, err
	}

	if !isPreview(parameters) {
		//update deployment to deleted and delete domain
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:            deploymentID,
			DeletionState: deployment_enums.DeletionDone,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:            previewID,
			DeletionState: deployment_enums.DeletionDone,
		})
	}

	return parameters, err
}

func deleteOriginAccessControl(originAccessControlId *string, cloudFrontClient *cloudfront.Client, logsWriter io.Writer) error {
	io.WriteString(logsWriter, fmt.Sprintf("Deleting origin access control: %s\n", aws.ToString(originAccessControlId)))
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

func deleteCloudfrontFunctions(parameters map[string]interface{}, cloudfrontClient *cloudfront.Client, logsWriter io.Writer) error {
	viewerResponseFunctionName, err := getViewerResponseFunctionName(parameters)
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting cloudfront function: %s\n", viewerResponseFunctionName))
	err = deleteCloudfrontFunction(cloudfrontClient, viewerResponseFunctionName)
	if err != nil {
		return err
	}
	viewerRequestFunctionName, err := getViewerRequestFunctionName(parameters)
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting cloudfront function: %s\n", viewerRequestFunctionName))
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
