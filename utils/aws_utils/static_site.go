package aws_utils

// static_site.go holds the shared S3 + CloudFront primitives used to deploy a
// static site. Extracted from the deploy_aws_static_site command so both that
// command and the in-process preview deploy can reuse them without an import
// cycle — aws_utils is a leaf package (commands already import it for the ECS
// helpers).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	awsS3Uploads "github.com/deployment-io/deployment-runner/utils/uploads/aws-s3"
)

// CreateCachePolicy creates a CloudFront cache policy (forwards the
// CloudFront-Forwarded-Proto header; long min TTL) and returns its id.
func CreateCachePolicy(cachePolicyName string, cloudFrontClient *cloudfront.Client) (*string, error) {
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

// CreateOriginAccessControl creates a CloudFront OAC (sigv4, S3 origin) and
// returns its id.
func CreateOriginAccessControl(name string, cloudFrontClient *cloudfront.Client) (*string, error) {
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

// UploadToS3 uploads a local directory tree to an S3 bucket.
func UploadToS3(directory, s3Region, s3Bucket string, s3Client *s3.Client, logsWriter io.Writer) error {
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

// CreateS3BucketIfNeeded ensures the bucket exists (idempotent) and returns its
// location, whether it was newly created, and any error.
func CreateS3BucketIfNeeded(s3Client *s3.Client, s3Bucket, s3Region string) (*string, bool, error) {
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
		var ae smithy.APIError
		if errors.As(err, &ae) {
			log.Printf("code: %s, message: %s, fault: %s", ae.ErrorCode(), ae.ErrorMessage(), ae.ErrorFault().String())
		}
		return nil, false, err
	}
	bucketLocation := response.Location
	return bucketLocation, true, nil
}

// CreateDefaultCacheBehavior builds the default cache behavior for a static-site
// distribution (GET/HEAD, allow-all viewer protocol, the given cache policy).
func CreateDefaultCacheBehavior(bucketLocation, cachePolicyId *string) *cloudfrontTypes.DefaultCacheBehavior {
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

func listAllS3Objects(s3Client *s3.Client, bucketName string) ([]s3Types.Object, error) {
	params := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	}

	listObjectsPaginator := s3.NewListObjectsV2Paginator(s3Client, params)

	var i int
	var objects []s3Types.Object
	for listObjectsPaginator.HasMorePages() {
		i++
		page, err := listObjectsPaginator.NextPage(context.TODO())
		if err != nil {
			return nil, fmt.Errorf("failed to get page %v, %v", i, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

// DeleteAllS3Files empties an S3 bucket (batched deletes, 9000 at a time).
func DeleteAllS3Files(s3Client *s3.Client, bucketName string) error {
	allS3Objects, err := listAllS3Objects(s3Client, bucketName)
	if err != nil {
		return err
	}
	var objectIds []s3Types.ObjectIdentifier
	for _, object := range allS3Objects {
		objectIds = append(objectIds, s3Types.ObjectIdentifier{Key: object.Key})
		if len(objectIds) == 9000 {
			//delete 9000 at a time. Limit is 10000
			_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
				Bucket: aws.String(bucketName),
				Delete: &s3Types.Delete{Objects: objectIds},
			})
			if err != nil {
				return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
			}
			objectIds = nil
		}
	}
	if len(objectIds) > 0 {
		_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &s3Types.Delete{Objects: objectIds},
		})
		if err != nil {
			return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
		}
	}
	return nil
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

// AttachPolicyToS3Bucket grants the CloudFront distribution s3:GetObject on the
// bucket (OAC bucket policy).
func AttachPolicyToS3Bucket(distributionArn *string, s3BucketName, policySid, policyId string, s3Client *s3.Client) error {
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
