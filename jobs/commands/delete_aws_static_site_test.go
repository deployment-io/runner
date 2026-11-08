package commands

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

type fakeStaticDistribution struct {
	comment string
	oacID   string
}

// fakeStaticSiteAWS stands in for the CloudFront and S3 clients deleting a static
// site uses. It records each call, in order, as "<operation> <argument>".
// ListDistributions and ListOriginAccessControls return one item per page.
type fakeStaticSiteAWS struct {
	calls []string

	distributions     map[string]fakeStaticDistribution // by id; order is distributionOrder
	distributionOrder []string
	oacs              map[string]string // name → id
	oacOrder          []string
	noBucket          bool

	listOACsErr  error // returned by ListOriginAccessControls
	deleteOACErr error // returned by DeleteOriginAccessControl
	oacGone      bool  // GetOriginAccessControl finds no OAC

	deletedOACs []string // OACs DeleteOriginAccessControl deleted
}

func (f *fakeStaticSiteAWS) call(operation, argument string) {
	f.calls = append(f.calls, operation+" "+argument)
}

// markerIndex is the index of the first item on the page marker starts.
func markerIndex(marker *string) int {
	i, _ := strconv.Atoi(aws.ToString(marker))
	return i
}

func (f *fakeStaticSiteAWS) ListDistributions(_ context.Context, in *cloudfront.ListDistributionsInput, _ ...func(*cloudfront.Options)) (*cloudfront.ListDistributionsOutput, error) {
	f.call("ListDistributions", aws.ToString(in.Marker))
	list := &cloudfrontTypes.DistributionList{IsTruncated: aws.Bool(false)}
	for i, id := range f.distributionOrder {
		if i < markerIndex(in.Marker) {
			continue
		}
		list.Items = []cloudfrontTypes.DistributionSummary{{Id: aws.String(id), Comment: aws.String(f.distributions[id].comment)}}
		if i+1 < len(f.distributionOrder) {
			list.IsTruncated, list.NextMarker = aws.Bool(true), aws.String(strconv.Itoa(i+1))
		}
		break
	}
	return &cloudfront.ListDistributionsOutput{DistributionList: list}, nil
}

func (f *fakeStaticSiteAWS) GetDistributionConfig(_ context.Context, in *cloudfront.GetDistributionConfigInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	id := aws.ToString(in.Id)
	f.call("GetDistributionConfig", id)
	d, ok := f.distributions[id]
	if !ok {
		return nil, &cloudfrontTypes.NoSuchDistribution{Message: aws.String("gone")}
	}
	return &cloudfront.GetDistributionConfigOutput{
		DistributionConfig: &cloudfrontTypes.DistributionConfig{
			Enabled:              aws.Bool(true),
			DefaultCacheBehavior: &cloudfrontTypes.DefaultCacheBehavior{CachePolicyId: aws.String("cp-" + id)},
			Origins: &cloudfrontTypes.Origins{Quantity: aws.Int32(1), Items: []cloudfrontTypes.Origin{{
				OriginAccessControlId: aws.String(d.oacID),
			}}},
		},
		ETag: aws.String("etag-config"),
	}, nil
}

func (f *fakeStaticSiteAWS) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	f.call("UpdateDistribution", aws.ToString(in.Id))
	return &cloudfront.UpdateDistributionOutput{ETag: aws.String("etag-updated")}, nil
}

func (f *fakeStaticSiteAWS) GetDistribution(_ context.Context, in *cloudfront.GetDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	f.call("GetDistribution", aws.ToString(in.Id))
	return &cloudfront.GetDistributionOutput{Distribution: &cloudfrontTypes.Distribution{Status: aws.String("Deployed")}}, nil
}

func (f *fakeStaticSiteAWS) DeleteDistribution(_ context.Context, in *cloudfront.DeleteDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error) {
	f.call("DeleteDistribution", aws.ToString(in.Id))
	delete(f.distributions, aws.ToString(in.Id))
	return &cloudfront.DeleteDistributionOutput{}, nil
}

func (f *fakeStaticSiteAWS) GetCachePolicy(_ context.Context, in *cloudfront.GetCachePolicyInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetCachePolicyOutput, error) {
	f.call("GetCachePolicy", aws.ToString(in.Id))
	return &cloudfront.GetCachePolicyOutput{ETag: aws.String("etag-policy")}, nil
}

func (f *fakeStaticSiteAWS) DeleteCachePolicy(_ context.Context, in *cloudfront.DeleteCachePolicyInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteCachePolicyOutput, error) {
	f.call("DeleteCachePolicy", aws.ToString(in.Id))
	return &cloudfront.DeleteCachePolicyOutput{}, nil
}

func (f *fakeStaticSiteAWS) ListOriginAccessControls(_ context.Context, in *cloudfront.ListOriginAccessControlsInput, _ ...func(*cloudfront.Options)) (*cloudfront.ListOriginAccessControlsOutput, error) {
	f.call("ListOriginAccessControls", aws.ToString(in.Marker))
	if f.listOACsErr != nil {
		return nil, f.listOACsErr
	}
	list := &cloudfrontTypes.OriginAccessControlList{IsTruncated: aws.Bool(false)}
	for i, name := range f.oacOrder {
		if i < markerIndex(in.Marker) {
			continue
		}
		list.Items = []cloudfrontTypes.OriginAccessControlSummary{{Id: aws.String(f.oacs[name]), Name: aws.String(name)}}
		if i+1 < len(f.oacOrder) {
			list.IsTruncated, list.NextMarker = aws.Bool(true), aws.String(strconv.Itoa(i+1))
		}
		break
	}
	return &cloudfront.ListOriginAccessControlsOutput{OriginAccessControlList: list}, nil
}

func (f *fakeStaticSiteAWS) GetOriginAccessControl(_ context.Context, in *cloudfront.GetOriginAccessControlInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetOriginAccessControlOutput, error) {
	f.call("GetOriginAccessControl", aws.ToString(in.Id))
	if f.oacGone {
		return nil, &cloudfrontTypes.NoSuchOriginAccessControl{Message: aws.String("gone")}
	}
	return &cloudfront.GetOriginAccessControlOutput{ETag: aws.String("etag-oac")}, nil
}

func (f *fakeStaticSiteAWS) DeleteOriginAccessControl(_ context.Context, in *cloudfront.DeleteOriginAccessControlInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteOriginAccessControlOutput, error) {
	f.call("DeleteOriginAccessControl", aws.ToString(in.Id))
	if f.deleteOACErr != nil {
		return nil, f.deleteOACErr
	}
	for _, d := range f.distributions {
		if d.oacID == aws.ToString(in.Id) {
			return nil, &cloudfrontTypes.OriginAccessControlInUse{Message: aws.String("in use")}
		}
	}
	f.deletedOACs = append(f.deletedOACs, aws.ToString(in.Id))
	return &cloudfront.DeleteOriginAccessControlOutput{}, nil
}

func (f *fakeStaticSiteAWS) DescribeFunction(_ context.Context, in *cloudfront.DescribeFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error) {
	f.call("DescribeFunction", aws.ToString(in.Name))
	return nil, &cloudfrontTypes.NoSuchFunctionExists{Message: aws.String("none")}
}

func (f *fakeStaticSiteAWS) DeleteFunction(_ context.Context, in *cloudfront.DeleteFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteFunctionOutput, error) {
	f.call("DeleteFunction", aws.ToString(in.Name))
	return &cloudfront.DeleteFunctionOutput{}, nil
}

func (f *fakeStaticSiteAWS) noSuchBucket() error {
	return &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "The specified bucket does not exist"}
}

func (f *fakeStaticSiteAWS) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.call("ListObjectsV2", aws.ToString(in.Bucket))
	if f.noBucket {
		return nil, f.noSuchBucket()
	}
	return &s3.ListObjectsV2Output{Contents: []s3Types.Object{{Key: aws.String("index.html")}}}, nil
}

func (f *fakeStaticSiteAWS) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.call("DeleteObjects", aws.ToString(in.Bucket))
	return &s3.DeleteObjectsOutput{}, nil
}

func (f *fakeStaticSiteAWS) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	f.call("DeleteBucket", aws.ToString(in.Bucket))
	if f.noBucket {
		return nil, f.noSuchBucket()
	}
	return &s3.DeleteBucketOutput{}, nil
}

func staticSiteDeleteParameters(cloudfrontID string) map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIdFromJob, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "d1")
	jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, cloudfrontID)
	return parameters
}

// runStaticSiteDelete runs DeleteAwsStaticSite over fake and returns the
// Deployment updates it made.
func runStaticSiteDelete(t *testing.T, fake *fakeStaticSiteAWS, cloudfrontID string) (string, []deployments.UpdateDeploymentDtoV1, error) {
	t.Helper()
	savedClients, savedPipeline := buildDeleteStaticSiteClients, commandUtils.UpdateDeploymentsPipeline
	buildDeleteStaticSiteClients = func(map[string]interface{}) (deleteStaticSiteClients, error) {
		return deleteStaticSiteClients{cloudfront: fake, s3: fake}, nil
	}
	var mu sync.Mutex
	var updates []deployments.UpdateDeploymentDtoV1
	commandUtils.UpdateDeploymentsPipeline, _ = goPipeline.NewPipeline(5, time.Hour, func(_ string, u []deployments.UpdateDeploymentDtoV1) {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, u...)
	})
	var logs bytes.Buffer
	_, err := (&DeleteAwsStaticSite{}).Run(staticSiteDeleteParameters(cloudfrontID), &logs)
	commandUtils.UpdateDeploymentsPipeline.Shutdown()
	buildDeleteStaticSiteClients, commandUtils.UpdateDeploymentsPipeline = savedClients, savedPipeline
	return logs.String(), updates, err
}

func markedDeleted(updates []deployments.UpdateDeploymentDtoV1) bool {
	return len(updates) == 2 && updates[0].DeletionState == deployment_enums.DeletionInProcess &&
		updates[1].ID == "d1" && updates[1].DeletionState == deployment_enums.DeletionDone
}

func distributionDeleteCalls(id string) []string {
	return []string{"GetDistributionConfig " + id, "UpdateDistribution " + id, "GetDistribution " + id, "DeleteDistribution " + id,
		"GetCachePolicy cp-" + id, "DeleteCachePolicy cp-" + id}
}

var staticSiteFunctionCalls = []string{"DescribeFunction response-headers-d1", "DescribeFunction viewer-request-d1"}

var staticSiteBucketCalls = []string{"ListObjectsV2 org-1-d1", "DeleteObjects org-1-d1", "DeleteBucket org-1-d1"}

func joinCalls(groups ...[]string) []string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

func checkStaticSiteDelete(t *testing.T, fake *fakeStaticSiteAWS, cloudfrontID string, want []string) string {
	t.Helper()
	logs, updates, err := runStaticSiteDelete(t, fake, cloudfrontID)
	if err != nil {
		t.Fatalf("Run: %v\nlogs:\n%s", err, logs)
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
	if !markedDeleted(updates) {
		t.Errorf("deployment updates = %+v, want in process then done", updates)
	}
	return logs
}

// A saved ID that exists is deleted exactly as before: distribution, its cache
// policy, the functions, its OAC, the bucket.
func TestDeleteStaticSiteSavedID(t *testing.T) {
	fake := &fakeStaticSiteAWS{distributions: map[string]fakeStaticDistribution{"E1": {comment: "anything", oacID: "oac-1"}}}
	checkStaticSiteDelete(t, fake, "E1", joinCalls([]string{"ListDistributions "}, distributionDeleteCalls("E1"), staticSiteFunctionCalls,
		[]string{"GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"}, staticSiteBucketCalls))
}

// No saved ID: the one distribution whose Comment names the deployment is found
// (on a later page) and deleted with its OAC; the one naming another id isn't.
func TestDeleteStaticSiteNoIDOneMatchingDistribution(t *testing.T) {
	fake := &fakeStaticSiteAWS{
		distributions: map[string]fakeStaticDistribution{
			"E0": {comment: "Preview distribution for d10", oacID: "oac-0"},
			"E1": {comment: "Preview distribution for d1", oacID: "oac-1"},
		},
		distributionOrder: []string{"E0", "E1"},
	}
	checkStaticSiteDelete(t, fake, "", joinCalls([]string{"ListDistributions ", "ListDistributions 1"}, distributionDeleteCalls("E1"),
		staticSiteFunctionCalls, []string{"GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"}, staticSiteBucketCalls))
	if _, ok := fake.distributions["E0"]; !ok {
		t.Error("deleted another deployment's distribution")
	}
}

func TestDeleteStaticSiteNoIDTwoMatchingDistributions(t *testing.T) {
	fake := &fakeStaticSiteAWS{
		distributions: map[string]fakeStaticDistribution{
			"E1": {comment: "Preview distribution for d1", oacID: "oac-1"},
			"E2": {comment: "Preview distribution for d1", oacID: "oac-1"},
		},
		distributionOrder: []string{"E1", "E2"},
	}
	checkStaticSiteDelete(t, fake, "", joinCalls([]string{"ListDistributions ", "ListDistributions 1"},
		distributionDeleteCalls("E1"), distributionDeleteCalls("E2"), staticSiteFunctionCalls,
		[]string{"GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"}, staticSiteBucketCalls))
}

// A saved ID beside a distribution an earlier deploy created and never saved, both
// on the preview's OAC: the Comment lookup runs anyway, both distributions go, and
// then the shared OAC, once.
func TestDeleteStaticSiteSavedIDBesideUnsavedDistribution(t *testing.T) {
	fake := &fakeStaticSiteAWS{
		distributions: map[string]fakeStaticDistribution{
			"E1": {comment: "Preview distribution for d1", oacID: "oac-1"},
			"E2": {comment: "Preview distribution for d1", oacID: "oac-1"},
		},
		distributionOrder: []string{"E1", "E2"},
	}
	checkStaticSiteDelete(t, fake, "E2", joinCalls([]string{"ListDistributions ", "ListDistributions 1"},
		distributionDeleteCalls("E1"), distributionDeleteCalls("E2"), staticSiteFunctionCalls,
		[]string{"GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"}, staticSiteBucketCalls))
	if len(fake.distributions) != 0 || !reflect.DeepEqual(fake.deletedOACs, []string{"oac-1"}) {
		t.Errorf("left distributions %v, deleted OACs %v", fake.distributions, fake.deletedOACs)
	}
}

// A saved ID the Comment lookup doesn't find (a production site, or a preview
// distribution under another Comment) is still deleted, first.
func TestDeleteStaticSiteSavedIDNotListed(t *testing.T) {
	fake := &fakeStaticSiteAWS{
		distributions: map[string]fakeStaticDistribution{
			"E1": {comment: "Preview distribution for d1", oacID: "oac-1"},
			"E9": {comment: "anything", oacID: "oac-9"},
		},
		distributionOrder: []string{"E1"},
	}
	checkStaticSiteDelete(t, fake, "E9", joinCalls([]string{"ListDistributions "},
		distributionDeleteCalls("E9"), distributionDeleteCalls("E1"), staticSiteFunctionCalls,
		[]string{"GetOriginAccessControl oac-9", "DeleteOriginAccessControl oac-9", "GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"},
		staticSiteBucketCalls))
}

// No saved ID and no distribution: the OAC named preview-<id> (found on a later
// page) and the bucket are deleted.
func TestDeleteStaticSiteNoIDNoDistribution(t *testing.T) {
	fake := &fakeStaticSiteAWS{
		oacs:     map[string]string{"preview-d10": "oac-0", "preview-d1": "oac-1"},
		oacOrder: []string{"preview-d10", "preview-d1"},
	}
	checkStaticSiteDelete(t, fake, "", joinCalls([]string{"ListDistributions "}, staticSiteFunctionCalls,
		[]string{"ListOriginAccessControls ", "ListOriginAccessControls 1", "GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"},
		staticSiteBucketCalls))
}

// Nothing was ever created (or all of it is gone): the delete still finishes and
// marks the Deployment deleted.
func TestDeleteStaticSiteNothingLeft(t *testing.T) {
	fake := &fakeStaticSiteAWS{noBucket: true}
	logs := checkStaticSiteDelete(t, fake, "", joinCalls([]string{"ListDistributions "}, staticSiteFunctionCalls,
		[]string{"ListOriginAccessControls ", "ListObjectsV2 org-1-d1"}))
	if !strings.Contains(logs, "S3 bucket org-1-d1 was already gone") {
		t.Errorf("logs = %q", logs)
	}
}

// A saved ID whose distribution is gone: its cache policy and OAC steps are
// skipped, the OAC is deleted by name and the bucket is deleted.
func TestDeleteStaticSiteSavedIDGone(t *testing.T) {
	fake := &fakeStaticSiteAWS{oacs: map[string]string{"preview-d1": "oac-1"}, oacOrder: []string{"preview-d1"}}
	logs := checkStaticSiteDelete(t, fake, "E1", joinCalls([]string{"ListDistributions ", "GetDistributionConfig E1"}, staticSiteFunctionCalls,
		[]string{"ListOriginAccessControls ", "GetOriginAccessControl oac-1", "DeleteOriginAccessControl oac-1"}, staticSiteBucketCalls))
	if !strings.Contains(logs, "CloudFront distribution E1 was already gone") {
		t.Errorf("logs = %q", logs)
	}
}

// A bucket deleted between emptying it and deleting it counts as gone.
func TestDeleteStaticSiteBucketGoneOnDelete(t *testing.T) {
	fake := &fakeStaticSiteAWS{distributions: map[string]fakeStaticDistribution{"E1": {oacID: "oac-1"}}}
	clients := deleteStaticSiteClients{cloudfront: fake, s3: bucketGoneOnDelete{fake}}
	var logs bytes.Buffer
	if err := deleteStaticSiteResources(staticSiteDeleteParameters("E1"), clients, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "S3 bucket org-1-d1 was already gone") {
		t.Errorf("logs = %q", logs.String())
	}
}

type bucketGoneOnDelete struct{ *fakeStaticSiteAWS }

func (b bucketGoneOnDelete) DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	return nil, b.noSuchBucket()
}

// Another S3 error still fails the delete.
func TestDeleteStaticSiteBucketOtherErrorFails(t *testing.T) {
	fake := &fakeStaticSiteAWS{}
	denied := errors.New("access denied")
	clients := deleteStaticSiteClients{cloudfront: fake, s3: bucketDeleteFails{fake, denied}}
	if err := deleteStaticSiteResources(staticSiteDeleteParameters(""), clients, &bytes.Buffer{}); !errors.Is(err, denied) {
		t.Errorf("err = %v, want %v", err, denied)
	}
}

type bucketDeleteFails struct {
	*fakeStaticSiteAWS
	err error
}

func (b bucketDeleteFails) DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	return nil, b.err
}

// The OAC deleted by name is the only thing that would ever delete it, so failing
// to list or delete it fails the job (the Deployment isn't marked deleted), after
// the bucket is deleted anyway.
func TestDeleteStaticSiteNamedOACFailureFails(t *testing.T) {
	failed := errors.New("throttled")
	for name, fake := range map[string]*fakeStaticSiteAWS{
		"list":   {listOACsErr: failed},
		"delete": {oacs: map[string]string{"preview-d1": "oac-1"}, oacOrder: []string{"preview-d1"}, deleteOACErr: failed},
	} {
		t.Run(name, func(t *testing.T) {
			logs, updates, err := runStaticSiteDelete(t, fake, "")
			if !errors.Is(err, failed) {
				t.Fatalf("err = %v, want %v\nlogs:\n%s", err, failed, logs)
			}
			if !strings.HasSuffix(strings.Join(fake.calls, "\n"), strings.Join(staticSiteBucketCalls, "\n")) {
				t.Errorf("bucket not deleted; calls:\n%s", strings.Join(fake.calls, "\n"))
			}
			for _, u := range updates {
				if u.DeletionState == deployment_enums.DeletionDone {
					t.Errorf("deployment marked deleted: %+v", updates)
				}
			}
		})
	}
}

// An OAC that goes between listing and deleting it counts as deleted.
func TestDeleteStaticSiteNamedOACGoneOnDelete(t *testing.T) {
	fake := &fakeStaticSiteAWS{oacs: map[string]string{"preview-d1": "oac-1"}, oacOrder: []string{"preview-d1"}, oacGone: true}
	logs := checkStaticSiteDelete(t, fake, "", joinCalls([]string{"ListDistributions "}, staticSiteFunctionCalls,
		[]string{"ListOriginAccessControls ", "GetOriginAccessControl oac-1"}, staticSiteBucketCalls))
	if !strings.Contains(logs, "origin access control oac-1 was already gone") {
		t.Errorf("logs = %q", logs)
	}
}
