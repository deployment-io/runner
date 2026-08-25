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
	"net/url"

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
// Returns the object keys written, for MarkStaleS3Files.
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

// staleObjectTagKey / staleObjectTagValue is the tag MarkStaleS3Files writes and
// the lifecycle rule expires on. It is the whole contract between the two: the
// marking pass and the rule never talk to each other otherwise.
const (
	staleObjectTagKey   = "deployment-io-stale"
	staleObjectTagValue = "true"
)

// staleObjectExpiryDays is how long a superseded build's files stay served
// before S3 expires them — the grace period a browser tab loaded against the
// previous build gets to keep fetching its chunks.
const staleObjectExpiryDays = 7

// MarkStaleS3Files tags the objects the build just uploaded did not produce, so
// an S3 lifecycle rule expires them after staleObjectExpiryDays instead of this
// deploy deleting them.
//
// REPLACES delete-then-upload, and then replaces the delete itself. The first
// two failure modes only appeared once sites started code-splitting:
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
// Reordering to upload -> invalidate -> delete fixed the first and only
// narrowed the second: a tab open at the moment of the deploy still lost its
// chunks, just a few seconds later. Vercel and Netlify keep superseded assets
// around instead, and that is what this does — bounded, because "keep forever"
// grows a bucket without limit. Old tabs work for a week; storage settles at
// roughly the current build plus whatever the last week's builds superseded.
//
// Ordering still matters and is still upload -> invalidate -> mark: until the
// edge stops serving the previous index.html, the files it names must not be
// on an expiry clock any earlier than they have to be.
//
// keep is the upload's own key set rather than a re-walk of the build
// directory. The uploader names objects with strings.TrimPrefix(path, dir+"/"),
// and a second implementation of that would usually agree — the failure mode
// when it did not would be putting the live site on an expiry clock.
//
// ⚠️ ASSUMES THE BUCKET IS DEDICATED to this site — one bucket per deployment,
// one per preview, which is how both callers provision them. That assumption is
// what makes "mark everything this build did not produce" a safe sentence.
// If a bucket is ever shared with anything else — another site, logs, user
// uploads — this function schedules it for deletion, and no guard here can tell
// the difference. Widen the keep set before widening the bucket.
func MarkStaleS3Files(s3Client *s3.Client, bucketName string, keep map[string]bool, logsWriter io.Writer) error {
	// An empty keep set would mean "mark everything", which is never a
	// legitimate outcome here: the caller has just uploaded a build. Refusing
	// is what makes a set-difference safe to run against a live bucket. It runs
	// before anything touches S3, so a nil client never gets that far.
	if len(keep) == 0 {
		return fmt.Errorf("refusing to mark stale files in %s: the uploaded key set is empty", bucketName)
	}
	// Not fatal. Tags are inert until the rule exists: a later deploy lands it
	// and everything already tagged becomes eligible then. The failure mode of
	// giving up here — refusing to mark, so the previous build's files live
	// forever — is strictly worse than marking a week early.
	if err := ensureStaleLifecycleRule(s3Client, bucketName); err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Could not ensure the stale-file expiry rule on %s (marking anyway): %s\n", bucketName, err))
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
	marked := markObjectsStale(s3Client, bucketName, stale, logsWriter)
	io.WriteString(logsWriter, fmt.Sprintf("Marked %d file(s) from the previous build as stale; S3 removes them in about %d days.\n",
		marked, staleObjectExpiryDays))
	return nil
}

// markObjectsStale tags each object with staleObjectTagKey, returning how many
// it managed to tag.
//
// ⚠️ SELF-COPY, NOT PutObjectTagging, AND THAT IS THE POINT. Lifecycle
// expiration counts its days from the object's LastModified — for a stale file
// that is the *previous* deploy, not now. Plain tagging would therefore grant
// "7 days minus however long ago the last deploy was", which is fine for a site
// deployed daily and collapses to hours for one deployed after a month's pause
// — exactly the tab-breaking behaviour this whole change exists to remove.
// Copying an object onto itself rewrites it, resetting LastModified so the
// window is measured from when the file actually became stale, and carries the
// tag in the same call. (S3 rejects a self-copy that changes nothing; the
// tagging directive is the change that makes it legal.)
//
// These objects are still being served for the next week, so the copy must not
// disturb them: MetadataDirective defaults to COPY, which carries Content-Type
// and the rest across unchanged. Only the tags are replaced.
//
// A per-object failure is logged and skipped rather than returned: the new
// build is already live, and one un-tagged leftover file costs a little storage
// until the next deploy marks it. Failing the deploy over that would report a
// successful rollout as broken.
func markObjectsStale(s3Client *s3.Client, bucketName string, stale []s3Types.ObjectIdentifier, logsWriter io.Writer) int {
	var marked int
	for _, object := range stale {
		key := aws.ToString(object.Key)
		// CopySource is bucket/key and the SDK sends it as a header verbatim,
		// so keys with spaces or other header-hostile characters have to be
		// escaped here. EscapedPath leaves the "/" separators intact.
		source := (&url.URL{Path: bucketName + "/" + key}).EscapedPath()
		_, err := s3Client.CopyObject(context.TODO(), &s3.CopyObjectInput{
			Bucket:           aws.String(bucketName),
			Key:              object.Key,
			CopySource:       aws.String(source),
			Tagging:          aws.String(staleObjectTagKey + "=" + staleObjectTagValue),
			TaggingDirective: s3Types.TaggingDirectiveReplace,
		})
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Could not mark %s as stale (it will be marked by a later deploy): %s\n", key, err))
			continue
		}
		marked++
	}
	return marked
}

// staleS3Objects is the set difference MarkStaleS3Files tags: every listed
// object whose key the current build did not produce. Split out because this is
// the one computation whose bug puts a live file on an expiry clock, so it is
// the part that must be testable without an S3 client.
//
// ⚠️ LOAD-BEARING PROPERTY: nothing here ever removes the stale tag, and
// nothing needs to. A rollback re-deploys a build whose files may already be
// tagged, and an S3 overwrite with no explicit tagging replaces the object
// outright — empty tag set, fresh LastModified — so a re-uploaded file stops
// being expiry-eligible by the act of being uploaded. That works only because
// the deploy uploads before it marks: by the time this difference is computed,
// a re-included file is already back in place untagged and lands in keep, so it
// is not stale here either. Reorder the deploy to mark before uploading and
// rollbacks start deleting themselves a week later.
func staleS3Objects(objects []s3Types.Object, keep map[string]bool) []s3Types.ObjectIdentifier {
	var stale []s3Types.ObjectIdentifier
	for _, object := range objects {
		if !keep[aws.ToString(object.Key)] {
			stale = append(stale, s3Types.ObjectIdentifier{Key: object.Key})
		}
	}
	return stale
}

// staleLifecycleRuleID identifies the rule this package owns. Rules are matched
// by ID on every deploy, so it must never change — a new ID means a second copy
// of the rule appended alongside the first, forever.
const staleLifecycleRuleID = "deployment-io-expire-stale-build-files"

// ensureStaleLifecycleRule makes sure the bucket has the rule that expires
// tagged objects. Idempotent: run on every deploy, appends at most once.
func ensureStaleLifecycleRule(s3Client *s3.Client, bucketName string) error {
	var existing []s3Types.LifecycleRule
	getOutput, err := s3Client.GetBucketLifecycleConfiguration(context.TODO(), &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		// A bucket with no lifecycle configuration is an error, not an empty
		// response — every bucket starts here, so this is the normal first-deploy
		// path and not an exceptional one.
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchLifecycleConfiguration" {
			return fmt.Errorf("error reading lifecycle configuration of bucket %s : %s", bucketName, err)
		}
	} else {
		existing = getOutput.Rules
	}
	rules, changed := withStaleLifecycleRule(existing)
	if !changed {
		return nil
	}
	_, err = s3Client.PutBucketLifecycleConfiguration(context.TODO(), &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 aws.String(bucketName),
		LifecycleConfiguration: &s3Types.BucketLifecycleConfiguration{Rules: rules},
	})
	if err != nil {
		return fmt.Errorf("error putting lifecycle configuration on bucket %s : %s", bucketName, err)
	}
	return nil
}

// withStaleLifecycleRule appends the expiry rule to a bucket's existing rules
// unless it is already there, reporting whether anything changed.
//
// READ-MODIFY-WRITE, NEVER A BLIND PUT.
// PutBucketLifecycleConfiguration replaces a bucket's entire configuration —
// there is no "add one rule" API — so the existing rules have to be carried
// through or a deploy silently deletes whatever else the bucket was configured
// to do.
//
// ⚠️ THE FILTER MUST BE THE TAG, NOT AN AGE. A plain "expire everything older
// than 7 days" rule on the bucket looks equivalent and is not: it depends on
// deploys to keep the current build's LastModified fresh. Every deploy re-uploads
// every file, so an actively deployed site refreshes itself and never notices.
// A site deployed once and then left running refreshes nothing, and on day 7 the
// rule deletes the live site out from under it. Tagging is what keeps a current
// build categorically ineligible: only objects a later build superseded ever
// carry the tag.
func withStaleLifecycleRule(existing []s3Types.LifecycleRule) ([]s3Types.LifecycleRule, bool) {
	for _, rule := range existing {
		if aws.ToString(rule.ID) == staleLifecycleRuleID {
			return existing, false
		}
	}
	staleRule := s3Types.LifecycleRule{
		ID:     aws.String(staleLifecycleRuleID),
		Status: s3Types.ExpirationStatusEnabled,
		Filter: &s3Types.LifecycleRuleFilterMemberTag{
			Value: s3Types.Tag{
				Key:   aws.String(staleObjectTagKey),
				Value: aws.String(staleObjectTagValue),
			},
		},
		Expiration: &s3Types.LifecycleExpiration{Days: aws.Int32(staleObjectExpiryDays)},
	}
	return append(append([]s3Types.LifecycleRule{}, existing...), staleRule), true
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
// limit. Only teardown deletes now (DeleteAllS3Files) — the deploy path marks
// instead, see MarkStaleS3Files.
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
// Still used by teardown (delete_aws_static_site), where emptying is the point
// and nothing is left to keep serving. The DEPLOY path no longer deletes at all
// — see MarkStaleS3Files.
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
