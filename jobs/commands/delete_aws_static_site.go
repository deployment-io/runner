package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"

	"io"
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
	clients, err := buildDeleteStaticSiteClients(parameters)
	if err != nil {
		return parameters, err
	}
	err = deleteStaticSiteResources(parameters, clients, logsWriter)
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

// deleteStaticSiteCloudfrontAPI is the part of *cloudfront.Client deleting a
// static site uses.
type deleteStaticSiteCloudfrontAPI interface {
	deleteCloudfrontDistributionAPI
	ListDistributions(ctx context.Context, params *cloudfront.ListDistributionsInput, optFns ...func(*cloudfront.Options)) (*cloudfront.ListDistributionsOutput, error)
	GetCachePolicy(ctx context.Context, params *cloudfront.GetCachePolicyInput, optFns ...func(*cloudfront.Options)) (*cloudfront.GetCachePolicyOutput, error)
	DeleteCachePolicy(ctx context.Context, params *cloudfront.DeleteCachePolicyInput, optFns ...func(*cloudfront.Options)) (*cloudfront.DeleteCachePolicyOutput, error)
	ListOriginAccessControls(ctx context.Context, params *cloudfront.ListOriginAccessControlsInput, optFns ...func(*cloudfront.Options)) (*cloudfront.ListOriginAccessControlsOutput, error)
	GetOriginAccessControl(ctx context.Context, params *cloudfront.GetOriginAccessControlInput, optFns ...func(*cloudfront.Options)) (*cloudfront.GetOriginAccessControlOutput, error)
	DeleteOriginAccessControl(ctx context.Context, params *cloudfront.DeleteOriginAccessControlInput, optFns ...func(*cloudfront.Options)) (*cloudfront.DeleteOriginAccessControlOutput, error)
	DescribeFunction(ctx context.Context, params *cloudfront.DescribeFunctionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error)
	DeleteFunction(ctx context.Context, params *cloudfront.DeleteFunctionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.DeleteFunctionOutput, error)
}

// deleteStaticSiteS3API is the part of *s3.Client deleting a static site uses.
type deleteStaticSiteS3API interface {
	aws_utils.S3FilesDeleter
	DeleteBucket(ctx context.Context, params *s3.DeleteBucketInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

type deleteStaticSiteClients struct {
	cloudfront deleteStaticSiteCloudfrontAPI
	s3         deleteStaticSiteS3API
}

// buildDeleteStaticSiteClients is newDeleteStaticSiteClients; tests replace it
// with fakes.
var buildDeleteStaticSiteClients = newDeleteStaticSiteClients

func newDeleteStaticSiteClients(parameters map[string]interface{}) (clients deleteStaticSiteClients, err error) {
	if clients.cloudfront, err = cloud_api_clients.GetCloudfrontClient(parameters, cloudfrontRegion); err != nil {
		return clients, err
	}
	if clients.s3, err = cloud_api_clients.GetS3Client(parameters); err != nil {
		return clients, err
	}
	return clients, nil
}

// previewDistributionComment is the Comment a preview deploy gives its
// distribution (agenttools buildPreviewDistributionConfig).
func previewDistributionComment(deploymentID string) string {
	return "Preview distribution for " + deploymentID
}

// previewOriginAccessControlName is the name a preview deploy gives its OAC
// (agenttools deployPreviewDistribution).
func previewOriginAccessControlName(deploymentID string) string {
	return "preview-" + deploymentID
}

// deleteStaticSiteResources deletes what a static site deploy creates: its
// CloudFront distribution, the distribution's cache policy, the CloudFront
// functions, the origin access controls and the S3 bucket. It deletes what
// exists: the saved CloudFront distribution and every distribution a preview
// deploy created for this deployment (found by their Comment — one whose id was
// never saved can sit beside the saved one, sharing its OAC), else the preview's
// OAC by name; a distribution or bucket that doesn't exist counts as deleted.
func deleteStaticSiteResources(parameters map[string]interface{}, clients deleteStaticSiteClients, logsWriter io.Writer) error {
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return err
	}
	cloudfrontClient := clients.cloudfront
	//delete cloudfront distribution
	cloudfrontDistributionId, _ := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID)
	// Always looked up, not only without a saved ID: a preview deploy whose id
	// was never saved left a distribution that a later deploy's saved one may sit
	// beside, and their shared OAC can't go while either does.
	distributionIDs, err := listDistributionsWithComment(cloudfrontClient, previewDistributionComment(deploymentID))
	if err != nil {
		return err
	}
	if len(cloudfrontDistributionId) > 0 && !slices.Contains(distributionIDs, cloudfrontDistributionId) {
		distributionIDs = append([]string{cloudfrontDistributionId}, distributionIDs...)
	}
	distributionConfigs, err := deleteStaticSiteDistributions(cloudfrontClient, distributionIDs, logsWriter)
	if err != nil {
		return err
	}

	//delete cf functions if needed
	err = deleteCloudfrontFunctions(parameters, cloudfrontClient, logsWriter)
	if err != nil {
		return err
	}

	//delete origin access for each origin if needed, once each: distributions
	//of one preview share its OAC
	deletedOACs := map[string]bool{}
	for _, distributionConfig := range distributionConfigs {
		if distributionConfig.Origins != nil {
			for _, origin := range distributionConfig.Origins.Items {
				oacID := aws.ToString(origin.OriginAccessControlId)
				if oacID == "" || deletedOACs[oacID] {
					continue
				}
				deletedOACs[oacID] = true
				_ = deleteOriginAccessControl(origin.OriginAccessControlId, cloudfrontClient, logsWriter)
			}
		}
	}
	var oacErr error
	if len(distributionConfigs) == 0 {
		// No distribution to read its OAC from: a preview deploy may have created
		// the OAC and failed before (or right after) creating the distribution.
		// Nothing else would delete it, so a failure fails the job, after the
		// bucket has been deleted too.
		oacErr = deletePreviewOriginAccessControl(cloudfrontClient, previewOriginAccessControlName(deploymentID), logsWriter)
	}

	return errors.Join(oacErr, deleteStaticSiteBucket(parameters, clients.s3, logsWriter))
}

// deleteStaticSiteDistributions deletes each distribution and its cache policy,
// returning the configs of those it deleted. A distribution that doesn't exist is
// skipped.
func deleteStaticSiteDistributions(cloudfrontClient deleteStaticSiteCloudfrontAPI, distributionIDs []string, logsWriter io.Writer) ([]*cloudfrontTypes.DistributionConfig, error) {
	var distributionConfigs []*cloudfrontTypes.DistributionConfig
	for _, distributionID := range distributionIDs {
		distributionConfig, err := deleteCloudfrontDistribution(cloudfrontClient, distributionID, logsWriter)
		var noSuchDistribution *cloudfrontTypes.NoSuchDistribution
		if errors.As(err, &noSuchDistribution) {
			logAlreadyGone(logsWriter, "CloudFront distribution", distributionID)
			continue
		}
		if err != nil {
			return nil, err
		}
		distributionConfigs = append(distributionConfigs, distributionConfig)

		//delete cache policy
		cachePolicyId := distributionConfig.DefaultCacheBehavior.CachePolicyId
		getCachePolicyOutput, err := cloudfrontClient.GetCachePolicy(context.TODO(), &cloudfront.GetCachePolicyInput{Id: cachePolicyId})
		if err != nil {
			return nil, err
		}
		io.WriteString(logsWriter, fmt.Sprintf("Deleting cache policy: %s\n", aws.ToString(cachePolicyId)))
		_, _ = cloudfrontClient.DeleteCachePolicy(context.TODO(), &cloudfront.DeleteCachePolicyInput{
			Id:      cachePolicyId,
			IfMatch: getCachePolicyOutput.ETag,
		})
	}
	return distributionConfigs, nil
}

// deleteStaticSiteBucket empties and deletes the site's bucket. A bucket that
// doesn't exist counts as deleted.
func deleteStaticSiteBucket(parameters map[string]interface{}, s3Client deleteStaticSiteS3API, logsWriter io.Writer) error {
	bucketName, err := getBucketName(parameters)
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting all files in S3 bucket: %s\n", bucketName))
	err = aws_utils.DeleteAllS3Files(s3Client, bucketName)
	if isNoSuchBucket(err) {
		logAlreadyGone(logsWriter, "S3 bucket", bucketName)
		return nil
	}
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting S3 bucket: %s\n", bucketName))
	_, err = s3Client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	if isNoSuchBucket(err) {
		logAlreadyGone(logsWriter, "S3 bucket", bucketName)
		return nil
	}
	return err
}

func isNoSuchBucket(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket"
}

// listDistributionsWithComment returns the IDs of every distribution whose
// Comment is exactly comment.
func listDistributionsWithComment(cloudfrontClient deleteStaticSiteCloudfrontAPI, comment string) ([]string, error) {
	var ids []string
	var marker *string
	for {
		out, err := cloudfrontClient.ListDistributions(context.TODO(), &cloudfront.ListDistributionsInput{Marker: marker})
		if err != nil {
			return nil, err
		}
		if out == nil || out.DistributionList == nil {
			return ids, nil
		}
		list := out.DistributionList
		for _, distribution := range list.Items {
			if aws.ToString(distribution.Comment) == comment {
				ids = append(ids, aws.ToString(distribution.Id))
			}
		}
		if !aws.ToBool(list.IsTruncated) || aws.ToString(list.NextMarker) == "" {
			return ids, nil
		}
		marker = list.NextMarker
	}
}

// deletePreviewOriginAccessControl deletes the OAC named exactly name, if there is
// one. One that doesn't exist, or is gone by the time it is deleted, counts as
// deleted.
func deletePreviewOriginAccessControl(cloudfrontClient deleteStaticSiteCloudfrontAPI, name string, logsWriter io.Writer) error {
	var marker *string
	for {
		out, err := cloudfrontClient.ListOriginAccessControls(context.TODO(), &cloudfront.ListOriginAccessControlsInput{Marker: marker})
		if err != nil {
			return fmt.Errorf("list origin access controls to delete %s: %w", name, err)
		}
		if out == nil || out.OriginAccessControlList == nil {
			return nil
		}
		list := out.OriginAccessControlList
		for _, oac := range list.Items {
			if aws.ToString(oac.Name) != name {
				continue
			}
			err = deleteOriginAccessControl(oac.Id, cloudfrontClient, logsWriter)
			var noSuchOAC *cloudfrontTypes.NoSuchOriginAccessControl
			if errors.As(err, &noSuchOAC) {
				logAlreadyGone(logsWriter, "origin access control", aws.ToString(oac.Id))
				return nil
			}
			if err != nil {
				return fmt.Errorf("delete origin access control %s: %w", name, err)
			}
			return nil
		}
		if !aws.ToBool(list.IsTruncated) || aws.ToString(list.NextMarker) == "" {
			return nil
		}
		marker = list.NextMarker
	}
}

func deleteOriginAccessControl(originAccessControlId *string, cloudFrontClient deleteStaticSiteCloudfrontAPI, logsWriter io.Writer) error {
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

func deleteCloudfrontFunctions(parameters map[string]interface{}, cloudfrontClient deleteStaticSiteCloudfrontAPI, logsWriter io.Writer) error {
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

func deleteCloudfrontFunction(cloudfrontClient deleteStaticSiteCloudfrontAPI, functionName string) error {
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
