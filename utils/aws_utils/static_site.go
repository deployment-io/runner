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
// Returns the object keys written, for PruneStaleS3Files.
func UploadToS3(directory, s3Region, s3Bucket string, s3Client *s3.Client, logsWriter io.Writer) (map[string]bool, error) {
	uploader, err := awsS3Uploads.NewUploader(s3Region, s3Bucket, s3Client)
	if err != nil {
		return nil, err
	}
	uploadedKeys, err := uploader.UploadDirectory(directory, logsWriter)
	if err != nil {
		return nil, err
	}
	return uploadedKeys, nil
}

// PruneStaleS3Files removes objects the build just uploaded did not produce.
//
// REPLACES delete-then-upload, which had two failure modes that only appeared
// once sites started code-splitting:
//
//   - Between the delete and the upload, the origin held NOTHING. CloudFront
//     masked it for cached objects, but a first-time visitor in that window got
//     a 404. On a build with hundreds of chunks the window is not brief.
//   - Deleting the previous build's hashed chunks breaks every session still
//     running it. A single-bundle app never noticed: it had already downloaded
//     everything it would ever need. A code-split app fetches chunks on
//     navigation, so it outlives its own build and then asks for a file that
//     was deleted — "Failed to fetch dynamically imported module".
//
// Ordering is the fix, and it must be upload -> invalidate -> prune. Invalidate
// BEFORE pruning: until the edge stops serving the previous index.html, the
// files it names must still exist.
//
// keep is the upload's own key set rather than a re-walk of the build
// directory. The uploader names objects with strings.TrimPrefix(path, dir+"/"),
// and a second implementation of that would usually agree — the failure mode
// when it did not would be deleting the live site.
//
// ⚠️ ASSUMES THE BUCKET IS DEDICATED to this site — one bucket per deployment,
// one per preview, which is how both callers provision them. That assumption is
// what makes "delete everything this build did not produce" a safe sentence.
// If a bucket is ever shared with anything else — another site, logs, user
// uploads — this function deletes it, and no guard here can tell the
// difference. Widen the keep set before widening the bucket.
func PruneStaleS3Files(s3Client *s3.Client, bucketName string, keep map[string]bool, logsWriter io.Writer) error {
	// An empty keep set would mean "delete everything", which is never a
	// legitimate outcome here: the caller has just uploaded a build. Refusing
	// is what makes a set-difference delete safe to run against a live bucket.
	if len(keep) == 0 {
		return fmt.Errorf("refusing to prune %s: the uploaded key set is empty", bucketName)
	}
	allS3Objects, err := listAllS3Objects(s3Client, bucketName)
	if err != nil {
		return err
	}
	stale := staleS3Objects(allS3Objects, keep)
	if len(stale) == 0 {
		// SAY SO. This branch was silent, and the first live deploy after the
		// prune shipped happened to be an unchanged redeploy — every hash
		// identical, nothing stale — so the log looked exactly like a runner
		// still on the old delete-then-upload code. "Ran, nothing to remove"
		// and "didn't run" must not read the same to the person checking.
		io.WriteString(logsWriter, "No files left over from the previous build.\n")
		return nil
	}
	io.WriteString(logsWriter, fmt.Sprintf("Removing %d file(s) left over from the previous build\n", len(stale)))
	return deleteS3ObjectsInBatches(s3Client, bucketName, stale)
}

// staleS3Objects is the set difference PruneStaleS3Files deletes: every listed
// object whose key the current build did not produce. Split out because this is
// the one computation whose bug deletes a live file, so it is the part that
// must be testable without an S3 client.
func staleS3Objects(objects []s3Types.Object, keep map[string]bool) []s3Types.ObjectIdentifier {
	var stale []s3Types.ObjectIdentifier
	for _, object := range objects {
		if !keep[aws.ToString(object.Key)] {
			stale = append(stale, s3Types.ObjectIdentifier{Key: object.Key})
		}
	}
	return stale
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

// s3DeleteObjectsLimit is the number of keys DeleteObjects accepts per request.
//
// ⚠️ 1000, NOT 10000. This batched at 9000 under a comment reading "Limit is
// 10000", which S3 rejects outright. It never fired because these buckets held
// a handful of files — a single-bundle SPA is a dozen objects. Code-splitting
// changes that: one dashboard build is already 241 objects, and a site past
// 1000 would have failed on both deploy and teardown.
const s3DeleteObjectsLimit = 1000

// deleteS3ObjectsInBatches deletes the given keys, respecting the per-request
// limit. Shared by DeleteAllS3Files and PruneStaleS3Files so the cap is stated
// once.
func deleteS3ObjectsInBatches(s3Client *s3.Client, bucketName string, objectIds []s3Types.ObjectIdentifier) error {
	for start := 0; start < len(objectIds); start += s3DeleteObjectsLimit {
		end := start + s3DeleteObjectsLimit
		if end > len(objectIds) {
			end = len(objectIds)
		}
		_, err := s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &s3Types.Delete{Objects: objectIds[start:end]},
		})
		if err != nil {
			return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
		}
	}
	return nil
}

// DeleteAllS3Files empties an S3 bucket.
//
// Still used by teardown (delete_aws_static_site), where emptying is the point.
// The DEPLOY path no longer calls this — see PruneStaleS3Files.
func DeleteAllS3Files(s3Client *s3.Client, bucketName string) error {
	allS3Objects, err := listAllS3Objects(s3Client, bucketName)
	if err != nil {
		return err
	}
	var objectIds []s3Types.ObjectIdentifier
	for _, object := range allS3Objects {
		objectIds = append(objectIds, s3Types.ObjectIdentifier{Key: object.Key})
	}
	if len(objectIds) > 0 {
		if err = deleteS3ObjectsInBatches(s3Client, bucketName, objectIds); err != nil {
			return err
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
