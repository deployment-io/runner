package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner/client"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
	awsS3Uploads "github.com/deployment-io/deployment-runner/utils/uploads/aws-s3"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type DeployAwsStaticSite struct {
}

func createCachePolicy(cachePolicyName string, cloudFrontClient *cloudfront.Client) (*string, error) {
	//can be used to forward any other cloudfront specific headers
	cachePolicyConfig := &cloudfrontTypes.CachePolicyConfig{
		MinTTL: aws.Int64(31536000),
		Name:   aws.String(cachePolicyName),
		ParametersInCacheKeyAndForwardedToOrigin: &cloudfrontTypes.ParametersInCacheKeyAndForwardedToOrigin{
			CookiesConfig: &cloudfrontTypes.CachePolicyCookiesConfig{
				CookieBehavior: cloudfrontTypes.CachePolicyCookieBehaviorNone,
			},
			EnableAcceptEncodingGzip: aws.Bool(true),
			HeadersConfig: &cloudfrontTypes.CachePolicyHeadersConfig{
				HeaderBehavior: cloudfrontTypes.CachePolicyHeaderBehaviorWhitelist,
				Headers: &cloudfrontTypes.Headers{
					Quantity: aws.Int32(1),
					Items: []string{
						"CloudFront-Forwarded-Proto",
					},
				},
			},
			QueryStringsConfig: &cloudfrontTypes.CachePolicyQueryStringsConfig{
				QueryStringBehavior: cloudfrontTypes.CachePolicyQueryStringBehaviorNone,
			},
			EnableAcceptEncodingBrotli: aws.Bool(true),
		},
	}

	cachePolicyOutput, err := cloudFrontClient.CreateCachePolicy(context.TODO(), &cloudfront.CreateCachePolicyInput{CachePolicyConfig: cachePolicyConfig})

	if err != nil {
		return nil, err
	}

	return cachePolicyOutput.CachePolicy.Id, nil
}

func createOriginAccessControl(name string, cloudFrontClient *cloudfront.Client) (*string, error) {
	originAccessControlConfig := &cloudfrontTypes.OriginAccessControlConfig{
		Name:                          aws.String(name),
		OriginAccessControlOriginType: cloudfrontTypes.OriginAccessControlOriginTypesS3,
		SigningBehavior:               cloudfrontTypes.OriginAccessControlSigningBehaviorsAlways,
		SigningProtocol:               cloudfrontTypes.OriginAccessControlSigningProtocolsSigv4,
		Description:                   aws.String("access control config for " + name),
	}

	originAccessControl, err := cloudFrontClient.CreateOriginAccessControl(context.TODO(), &cloudfront.CreateOriginAccessControlInput{
		OriginAccessControlConfig: originAccessControlConfig,
	})

	if err != nil {
		return nil, err
	}

	originAccessControlId := originAccessControl.OriginAccessControl.Id

	return originAccessControlId, nil
}

func getBucketName(parameters map[string]interface{}) (string, error) {
	//bucket name = <organizationID>-<deploymentID>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", organizationID, deploymentID), nil
}

func getDistDirectory(parameters map[string]interface{}) (string, error) {
	repoDirectory, err := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	if err != nil {
		return "", err
	}
	publishDirectory, err := jobs.GetParameterValue[string](parameters, parameters_enums.PublishDirectory)
	if err != nil {
		return "", err
	}
	//remove . and /
	publishDirectory = strings.TrimPrefix(publishDirectory, ".")
	publishDirectory = strings.TrimPrefix(publishDirectory, "/")
	publishDirectory = strings.TrimSuffix(publishDirectory, "/")
	if len(publishDirectory) == 0 {
		return "", fmt.Errorf("publish directory path is same as the root directory")
	}
	return fmt.Sprintf("%s/%s", repoDirectory, publishDirectory), nil
}

func uploadToS3(directory, s3Region, s3Bucket string, s3Client *s3.Client, logsWriter io.Writer) error {
	uploader, err := awsS3Uploads.NewUploader(s3Region, s3Bucket, s3Client)
	if err != nil {
		return err
	}
	err = uploader.UploadDirectory(directory, logsWriter)
	if err != nil {
		return err
	}
	return nil
}

func bucketExists(s3Client *s3.Client, s3Bucket string) bool {
	_, err := s3Client.HeadBucket(context.TODO(), &s3.HeadBucketInput{
		Bucket: aws.String(s3Bucket),
	})

	if err != nil {
		return false
	}

	return true
}

func createS3BucketIfNeeded(s3Client *s3.Client, s3Bucket, s3Region string) (*string, bool, error) {
	exists := bucketExists(s3Client, s3Bucket)

	if exists {
		return aws.String(fmt.Sprintf("/%s", s3Bucket)), false, nil
	}

	// Create S3 bucket
	createBucketInput := &s3.CreateBucketInput{
		Bucket: aws.String(s3Bucket),
	}
	if s3Region != "us-east-1" {
		//weird AWS gives error with location constraint for us-east-1
		createBucketConfiguration := &s3Types.CreateBucketConfiguration{
			LocationConstraint: s3Types.BucketLocationConstraint(s3Region),
		}
		createBucketInput.CreateBucketConfiguration = createBucketConfiguration
	}

	response, err := s3Client.CreateBucket(context.TODO(), createBucketInput)
	if err != nil {
		//var bne *types.BucketAlreadyExists
		var ae smithy.APIError
		if errors.As(err, &ae) {
			log.Printf("code: %s, message: %s, fault: %s", ae.ErrorCode(), ae.ErrorMessage(), ae.ErrorFault().String())
		}
		return nil, false, err
	}
	bucketLocation := response.Location
	//log.Println("created bucket info")
	//log.Println(aws.ToString(bucketLocation))
	//log.Println(response.ResultMetadata)
	//log.Println("------------------------")
	return bucketLocation, true, nil
}

func getCommentForCloudfront(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Cloudfront distribution for %s-%s", organizationID, deploymentID), nil
}

func getCallerReference(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%d", organizationID, deploymentID, time.Now().Unix()), nil
}

func getCachePolicyName(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", organizationID, deploymentID), nil
}

func getOriginAccessName(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", organizationID, deploymentID), nil
}

func createDefaultCacheBehavior(bucketLocation, cachePolicyId *string) *cloudfrontTypes.DefaultCacheBehavior {
	allowedMethods := &cloudfrontTypes.AllowedMethods{
		Items: []cloudfrontTypes.Method{
			cloudfrontTypes.MethodGet,
			cloudfrontTypes.MethodHead,
		},
		Quantity: aws.Int32(2),
	}

	defaultCacheBehavior := &cloudfrontTypes.DefaultCacheBehavior{
		TargetOriginId:       bucketLocation,
		ViewerProtocolPolicy: cloudfrontTypes.ViewerProtocolPolicyAllowAll,
		AllowedMethods:       allowedMethods,
		CachePolicyId:        cachePolicyId,
	}
	return defaultCacheBehavior
}

func createDistributionConfigForNewCloudfront(parameters map[string]interface{}, bucketLocation, originAccessControlId *string, callerReference, comment,
	domainName string,
	defaultCacheBehavior *cloudfrontTypes.DefaultCacheBehavior) (*cloudfrontTypes.DistributionConfig, error) {
	origin := cloudfrontTypes.Origin{
		Id:                    bucketLocation,
		DomainName:            aws.String(domainName),
		OriginAccessControlId: originAccessControlId,
		S3OriginConfig: &cloudfrontTypes.S3OriginConfig{
			OriginAccessIdentity: aws.String(""),
		},
	}

	origins := &cloudfrontTypes.Origins{
		Items: []cloudfrontTypes.Origin{
			origin,
		},
		Quantity: aws.Int32(1),
	}

	errorPagesA, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.ErrorPages)
	if err != nil {
		return nil, err
	}
	errorPages, err := commandUtils.ConvertPrimitiveAToTwoDStringSlice(errorPagesA)
	if err != nil {
		return nil, err
	}

	var customErrorResponses []cloudfrontTypes.CustomErrorResponse
	var q int32 = 0
	for _, errorPageRow := range errorPages {
		if len(errorPageRow) == 3 {
			i, err := strconv.ParseInt(errorPageRow[0], 10, 64)
			if err == nil {
				customErrorResponse := cloudfrontTypes.CustomErrorResponse{
					ErrorCode:        aws.Int32(int32(i)),
					ResponsePagePath: aws.String(errorPageRow[1]),
					ResponseCode:     aws.String(errorPageRow[2]),
				}
				customErrorResponses = append(customErrorResponses, customErrorResponse)
				q++
			}
		}
	}

	distributionConfig := &cloudfrontTypes.DistributionConfig{
		CallerReference:      aws.String(callerReference),
		Comment:              aws.String(comment),
		DefaultCacheBehavior: defaultCacheBehavior,
		Enabled:              aws.Bool(true),
		Origins:              origins,
		CustomErrorResponses: &cloudfrontTypes.CustomErrorResponses{
			Quantity: aws.Int32(q),
			Items:    customErrorResponses,
		},
		DefaultRootObject: aws.String("index.html"),
		PriceClass:        cloudfrontTypes.PriceClassPriceClassAll,
		ViewerCertificate: nil,
		WebACLId:          nil,
	}

	return distributionConfig, nil
}

type bucketPolicyStatement struct {
	Sid       string `json:"Sid"`
	Effect    string `json:"Effect"`
	Principal struct {
		Service string `json:"Service"`
	} `json:"Principal"`
	Action    string `json:"Action"`
	Resource  string `json:"Resource"`
	Condition struct {
		StringEquals struct {
			AWSSourceArn string `json:"AWS:SourceArn"`
		} `json:"StringEquals"`
	} `json:"Condition"`
}

type bucketPolicyDto struct {
	Version   string                  `json:"Version"`
	Id        string                  `json:"Id"`
	Statement []bucketPolicyStatement `json:"Statement"`
}

func getBucketPolicySid(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("AllowCloudFrontServicePrincipal-%s-%s", organizationID, deploymentID), nil
}

func getBucketPolicyId(parameters map[string]interface{}) (string, error) {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("PolicyForCloudFrontPrivateContent-%s-%s", organizationID, deploymentID), nil
}

func attachPolicyToS3Bucket(distributionArn *string, s3BucketName, policySid, policyId string, s3Client *s3.Client) error {
	policyStatement := bucketPolicyStatement{
		Sid:    policySid,
		Effect: "Allow",
		Principal: struct {
			Service string `json:"Service"`
		}{
			Service: "cloudfront.amazonaws.com",
		},
		Action:   "s3:GetObject",
		Resource: "arn:aws:s3:::" + s3BucketName + "/*",
		Condition: struct {
			StringEquals struct {
				AWSSourceArn string `json:"AWS:SourceArn"`
			} `json:"StringEquals"`
		}{
			StringEquals: struct {
				AWSSourceArn string `json:"AWS:SourceArn"`
			}{
				AWSSourceArn: aws.ToString(distributionArn),
			},
		},
	}

	policyDto := bucketPolicyDto{
		Version: "2008-10-17",
		Id:      policyId,
		Statement: []bucketPolicyStatement{
			policyStatement,
		},
	}

	policyInJsonBytes, err := json.Marshal(policyDto)
	if err != nil {
		return err
	}

	bucketPolicyInput := &s3.PutBucketPolicyInput{
		Bucket: aws.String(s3BucketName),
		Policy: aws.String(string(policyInJsonBytes)),
	}

	_, err = s3Client.PutBucketPolicy(context.TODO(), bucketPolicyInput)

	if err != nil {
		return err
	}
	return nil
}

func (d *DeployAwsStaticSite) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()

	region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return parameters, err
	}

	//check and add policy for AWS static site deployment
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return parameters, err
	}
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsStaticSiteDeployment,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
	if err != nil {
		return parameters, err
	}

	bucketName, err := getBucketName(parameters)
	if err != nil {
		return parameters, err
	}
	distDirectory, err := getDistDirectory(parameters)
	if err != nil {
		return parameters, err
	}

	//checking if index.html file exists
	if _, err = os.Stat(distDirectory + "/index.html"); err != nil {
		if os.IsNotExist(err) {
			io.WriteString(logsWriter, fmt.Sprintf("index.html file doesn't exists in build directory\n"))
			return parameters, err
		} else {
			return parameters, err
		}
	}

	s3Client, err := cloud_api_clients.GetS3Client(parameters)
	if err != nil {
		return parameters, err
	}

	bucketLocation, isNewBucketCreated, err := createS3BucketIfNeeded(s3Client, bucketName, region_enums.Type(region).String())
	if err != nil {
		return parameters, err
	}

	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}

	var cloudfrontID string
	ignoreErrorsTillCF := false
	if !isNewBucketCreated {
		cloudfrontID, err = jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID)
		if err != nil || len(cloudfrontID) == 0 {
			//get cloudfront id if not there
			var deploymentID string
			deploymentID, err = jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
			if err != nil {
				return parameters, err
			}
			c := client.Get()
			e := false
			//TODO put this in a function and add support for preview
			if !isPreview(parameters) {
				var deploymentData []deployments.GetDeploymentDtoV1
				for !e {
					deploymentData, err = c.GetDeploymentData([]string{deploymentID}, organizationIdFromJob)
					if err == client.ErrConnection {
						time.Sleep(time.Second * 20)
						continue
					}
					if err != nil {
						return parameters, err
					}
					if len(deploymentData) < 1 {
						return parameters, fmt.Errorf("error getting deployment data for %s", deploymentID)
					}
					e = true
				}
				cloudfrontID = deploymentData[0].CloudfrontDistributionID
			} else {
				var previewData []previews.GetPreviewDtoV1
				//preview id is deployment id in case of preview
				previewID := deploymentID
				for !e {
					previewData, err = c.GetPreviewData([]string{previewID}, organizationIdFromJob)
					if err == client.ErrConnection {
						time.Sleep(time.Second * 20)
						continue
					}
					if err != nil {
						return parameters, err
					}
					if len(previewData) < 1 {
						return parameters, fmt.Errorf("error getting preview data for %s", previewID)
					}
					e = true
				}
				cloudfrontID = previewData[0].CloudfrontDistributionID
			}

			if len(cloudfrontID) == 0 {
				isNewBucketCreated = true
				ignoreErrorsTillCF = true
			}
		}
	}

	err = deleteAllS3Files(s3Client, bucketName)
	if err != nil {
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Uploading site to S3 bucket: %s\n", bucketName))

	err = uploadToS3(distDirectory, region_enums.Type(region).String(), bucketName, s3Client, logsWriter)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Error uploading site to S3 bucket: %s\n", bucketName))
		return parameters, err
	}

	// Create an Amazon Cloudfront service client
	cloudfrontClient, err := cloud_api_clients.GetCloudfrontClient(parameters, cloudfrontRegion)
	if err != nil {
		return parameters, err
	}

	if isNewBucketCreated {
		//new deployment
		// Create Origin Access Control
		var originAccessControlId *string
		var originAccessName string
		originAccessName, err = getOriginAccessName(parameters)
		if !ignoreErrorsTillCF && err != nil {
			return parameters, err
		}
		originAccessControlId, err = createOriginAccessControl(originAccessName, cloudfrontClient)
		if !ignoreErrorsTillCF && err != nil {
			return parameters, err
		}
		var cachePolicyName string
		cachePolicyName, err = getCachePolicyName(parameters)
		if !ignoreErrorsTillCF && err != nil {
			return parameters, err
		}

		// Create cache policy
		var cachePolicyId *string
		cachePolicyId, err = createCachePolicy(cachePolicyName, cloudfrontClient)
		if !ignoreErrorsTillCF && err != nil {
			return parameters, err
		}

		// Create default cache behavior
		defaultCacheBehavior := createDefaultCacheBehavior(bucketLocation, cachePolicyId)

		// Create distribution config
		domainName := bucketName + ".s3." + region_enums.Type(region).String() + ".amazonaws.com"
		var callerReference string
		callerReference, err = getCallerReference(parameters)
		if !ignoreErrorsTillCF && err != nil {
			return parameters, err
		}

		var comment string
		comment, err = getCommentForCloudfront(parameters)
		if !ignoreErrorsTillCF && err != nil {
			return parameters, err
		}

		var distributionConfig *cloudfrontTypes.DistributionConfig
		distributionConfig, err = createDistributionConfigForNewCloudfront(parameters, bucketLocation, originAccessControlId,
			callerReference, comment, domainName, defaultCacheBehavior)

		if err != nil {
			return parameters, err
		}

		io.WriteString(logsWriter, fmt.Sprintf("Creating cloudfront distribution\n"))

		// Create cloudfront distribution
		var createDistributionOutput *cloudfront.CreateDistributionOutput
		createDistributionOutput, err = cloudfrontClient.CreateDistribution(context.TODO(), &cloudfront.CreateDistributionInput{
			DistributionConfig: distributionConfig,
		})
		if err != nil {
			return parameters, err
		}

		distributionId := createDistributionOutput.Distribution.Id

		io.WriteString(logsWriter, fmt.Sprintf("Created cloudfront distribution: %s\n", aws.ToString(distributionId)))

		// Attach bucket policy
		var bucketPolicySid string
		bucketPolicySid, err = getBucketPolicySid(parameters)
		if err != nil {
			return parameters, err
		}
		var bucketPolicyId string
		bucketPolicyId, err = getBucketPolicyId(parameters)
		if err != nil {
			return parameters, err
		}
		err = attachPolicyToS3Bucket(createDistributionOutput.Distribution.ARN, bucketName, bucketPolicySid, bucketPolicyId, s3Client)
		if err != nil {
			return parameters, err
		}

		// Check if cloudfront distribution is deployed
		getDistributionInput := &cloudfront.GetDistributionInput{Id: distributionId}

		distributionDeployedWaiter := cloudfront.NewDistributionDeployedWaiter(cloudfrontClient)

		io.WriteString(logsWriter, fmt.Sprintf("Waiting for cloudfront distribution to be deployed: %s\n", aws.ToString(distributionId)))

		var deploymentID string
		deploymentID, err = jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
		if err != nil {
			return parameters, err
		}
		if !isPreview(parameters) {
			//send data back to save for deployment
			commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
				ID:                               deploymentID,
				CloudfrontDistributionID:         aws.ToString(createDistributionOutput.Distribution.Id),
				CloudfrontDistributionArn:        aws.ToString(createDistributionOutput.Distribution.ARN),
				CloudfrontDistributionDomainName: aws.ToString(createDistributionOutput.Distribution.DomainName),
			})
		} else {
			//deployment id is preview id in case of preview
			previewID := deploymentID
			//send data back to save for preview
			commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
				ID:                               previewID,
				CloudfrontDistributionID:         aws.ToString(createDistributionOutput.Distribution.Id),
				CloudfrontDistributionArn:        aws.ToString(createDistributionOutput.Distribution.ARN),
				CloudfrontDistributionDomainName: aws.ToString(createDistributionOutput.Distribution.DomainName),
			})
		}

		err = distributionDeployedWaiter.Wait(context.TODO(), getDistributionInput, 20*time.Minute)
		if err != nil {
			return parameters, err
		}

		jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, aws.ToString(createDistributionOutput.Distribution.Id))
	} else {
		//new build
		//Invalidate cloudfront
		var callerReference string
		callerReference, err = getCallerReference(parameters)
		if err != nil {
			return parameters, err
		}
		var createInvalidationOutput *cloudfront.CreateInvalidationOutput
		createInvalidationOutput, err = cloudfrontClient.CreateInvalidation(context.TODO(), &cloudfront.CreateInvalidationInput{
			DistributionId: aws.String(cloudfrontID),
			InvalidationBatch: &cloudfrontTypes.InvalidationBatch{
				CallerReference: aws.String(callerReference),
				Paths: &cloudfrontTypes.Paths{
					Quantity: aws.Int32(1),
					Items: []string{
						"/*",
					},
				},
			},
		})

		if err != nil {
			return parameters, err
		}

		//Wait for invalidation to get done
		invalidationWaiter := cloudfront.NewInvalidationCompletedWaiter(cloudfrontClient)

		io.WriteString(logsWriter, fmt.Sprintf("Waiting for cloudfront distribution to be invalidated: %s\n", cloudfrontID))

		err = invalidationWaiter.Wait(context.TODO(), &cloudfront.GetInvalidationInput{
			DistributionId: aws.String(cloudfrontID),
			Id:             createInvalidationOutput.Invalidation.Id,
		}, 20*time.Minute)

		if err != nil {
			return parameters, err
		}

		jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, cloudfrontID)
	}

	//mark build done successfully
	<-MarkDeploymentDone(parameters, nil)

	return parameters, nil
}
