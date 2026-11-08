package agenttools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

const testRoutingARN = "arn:aws:cloudfront::123456789012:function/" + previewRoutingFunctionName

// fakeCloudfront models the shared function's DEVELOPMENT/LIVE stages and one
// distribution's config, recording the mutating calls.
type fakeCloudfront struct {
	devExists, liveExists bool

	createErr     error // returned by CreateFunction
	concurrentDev bool  // CreateFunction's error came from a concurrent creator: DEVELOPMENT now exists
	publishErr    error // returned by PublishFunction
	concurrentPub bool  // PublishFunction's error came from a concurrent publisher: LIVE now exists

	creates, publishes int
	publishedETag      string

	distConfig  *cloudfrontTypes.DistributionConfig
	distETag    string
	updates     []*cloudfront.UpdateDistributionInput
	createdDist *cloudfrontTypes.DistributionConfig
	getDists    []string // ids GetDistribution was called with
	getDistErr  error    // returned by GetDistribution

	createOACErr error                                          // returned by CreateOriginAccessControl
	oacPages     [][]cloudfrontTypes.OriginAccessControlSummary // what ListOriginAccessControls returns, page by page
	oacLists     int

	distPages [][]cloudfrontTypes.DistributionSummary // what ListDistributions returns, page by page
	listErr   error                                   // returned by ListDistributions
	distLists int
}

func (f *fakeCloudfront) summary() *cloudfrontTypes.FunctionSummary {
	return &cloudfrontTypes.FunctionSummary{FunctionMetadata: &cloudfrontTypes.FunctionMetadata{FunctionARN: aws.String(testRoutingARN)}}
}

func (f *fakeCloudfront) DescribeFunction(_ context.Context, in *cloudfront.DescribeFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DescribeFunctionOutput, error) {
	if aws.ToString(in.Name) != previewRoutingFunctionName {
		return nil, errors.New("unexpected function " + aws.ToString(in.Name))
	}
	exists := f.devExists
	if in.Stage == cloudfrontTypes.FunctionStageLive {
		exists = f.liveExists
	}
	if !exists {
		return nil, &cloudfrontTypes.NoSuchFunctionExists{}
	}
	return &cloudfront.DescribeFunctionOutput{ETag: aws.String("dev-etag"), FunctionSummary: f.summary()}, nil
}

func (f *fakeCloudfront) CreateFunction(_ context.Context, in *cloudfront.CreateFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateFunctionOutput, error) {
	f.creates++
	if f.createErr != nil {
		f.devExists = f.devExists || f.concurrentDev
		return nil, f.createErr
	}
	if string(in.FunctionCode) != previewRoutingFunctionCode || in.FunctionConfig.Runtime != cloudfrontTypes.FunctionRuntimeCloudfrontJs10 {
		return nil, errors.New("unexpected function code/runtime")
	}
	f.devExists = true
	return &cloudfront.CreateFunctionOutput{ETag: aws.String("dev-etag"), FunctionSummary: f.summary()}, nil
}

func (f *fakeCloudfront) PublishFunction(_ context.Context, in *cloudfront.PublishFunctionInput, _ ...func(*cloudfront.Options)) (*cloudfront.PublishFunctionOutput, error) {
	f.publishes++
	f.publishedETag = aws.ToString(in.IfMatch)
	if f.publishErr != nil {
		f.liveExists = f.liveExists || f.concurrentPub
		return nil, f.publishErr
	}
	f.liveExists = true
	return &cloudfront.PublishFunctionOutput{FunctionSummary: f.summary()}, nil
}

func (f *fakeCloudfront) CreateOriginAccessControl(context.Context, *cloudfront.CreateOriginAccessControlInput, ...func(*cloudfront.Options)) (*cloudfront.CreateOriginAccessControlOutput, error) {
	if f.createOACErr != nil {
		return nil, f.createOACErr
	}
	return &cloudfront.CreateOriginAccessControlOutput{OriginAccessControl: &cloudfrontTypes.OriginAccessControl{Id: aws.String("oac-1")}}, nil
}

func (f *fakeCloudfront) ListOriginAccessControls(_ context.Context, in *cloudfront.ListOriginAccessControlsInput, _ ...func(*cloudfront.Options)) (*cloudfront.ListOriginAccessControlsOutput, error) {
	f.oacLists++
	page := 0
	if in.Marker != nil {
		page, _ = strconv.Atoi(aws.ToString(in.Marker))
	}
	list := &cloudfrontTypes.OriginAccessControlList{IsTruncated: aws.Bool(false)}
	if page < len(f.oacPages) {
		list.Items = f.oacPages[page]
	}
	if page+1 < len(f.oacPages) {
		list.IsTruncated = aws.Bool(true)
		list.NextMarker = aws.String(strconv.Itoa(page + 1))
	}
	return &cloudfront.ListOriginAccessControlsOutput{OriginAccessControlList: list}, nil
}

func (f *fakeCloudfront) ListDistributions(_ context.Context, in *cloudfront.ListDistributionsInput, _ ...func(*cloudfront.Options)) (*cloudfront.ListDistributionsOutput, error) {
	f.distLists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	page := 0
	if in.Marker != nil {
		page, _ = strconv.Atoi(aws.ToString(in.Marker))
	}
	list := &cloudfrontTypes.DistributionList{IsTruncated: aws.Bool(false)}
	if page < len(f.distPages) {
		list.Items = f.distPages[page]
	}
	if page+1 < len(f.distPages) {
		list.IsTruncated = aws.Bool(true)
		list.NextMarker = aws.String(strconv.Itoa(page + 1))
	}
	return &cloudfront.ListDistributionsOutput{DistributionList: list}, nil
}

func (f *fakeCloudfront) CreateDistribution(_ context.Context, in *cloudfront.CreateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionOutput, error) {
	f.createdDist = in.DistributionConfig
	return &cloudfront.CreateDistributionOutput{Distribution: &cloudfrontTypes.Distribution{Id: aws.String("E1"), ARN: aws.String("arn:dist"), DomainName: aws.String("d1.cloudfront.net")}}, nil
}

func (f *fakeCloudfront) GetDistribution(_ context.Context, in *cloudfront.GetDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	f.getDists = append(f.getDists, aws.ToString(in.Id))
	if f.getDistErr != nil {
		return nil, f.getDistErr
	}
	return &cloudfront.GetDistributionOutput{Distribution: &cloudfrontTypes.Distribution{Id: in.Id, ARN: aws.String("arn:" + aws.ToString(in.Id))}}, nil
}

func (f *fakeCloudfront) GetDistributionConfig(context.Context, *cloudfront.GetDistributionConfigInput, ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	return &cloudfront.GetDistributionConfigOutput{DistributionConfig: f.distConfig, ETag: aws.String(f.distETag)}, nil
}

func (f *fakeCloudfront) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	f.updates = append(f.updates, in)
	return &cloudfront.UpdateDistributionOutput{}, nil
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestPreviewRoutingFunctionCode(t *testing.T) {
	code := squash(previewRoutingFunctionCode)
	// Production's non-SPA statement (getViewerRequestFunctionCode), with the
	// 301 response returned directly.
	for _, want := range []string{
		`function buildQS(querystring) {`,
		`parts.push(k + '=' + mv.value);`,
		`if (request.uri.endsWith('/')) { request.uri += 'index.html'; } else if (!request.uri.includes('.')) { var host = request.headers["host"].value; var location = 'https://' + host + request.uri + '/' + buildQS(request.querystring); return { statusCode: 301, statusDescription: 'Moved Permanently', headers: { 'location': { value: location } } }; }`,
		`return request; }`,
	} {
		if !strings.Contains(code, squash(want)) {
			t.Errorf("function code missing %q", want)
		}
	}
	// No domain redirects: those key off the host or the forwarded proto.
	for _, notWant := range []string{"startsWith", "cloudfront-forwarded-proto"} {
		if strings.Contains(code, notWant) {
			t.Errorf("function code contains a domain redirect (%q)", notWant)
		}
	}
}

// TestPreviewRoutingFunctionBehaviour runs the function code under node (when
// installed) to check the rewrite, the redirect and that a repeated query key
// keeps all its values.
func TestPreviewRoutingFunctionBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := previewRoutingFunctionCode + `
function req(uri, qs) { return { request: { uri: uri, querystring: qs, headers: { host: { value: 'p.example' } } } }; }
var out = {
	dir: handler(req('/docs/tasks/review/', {})).uri,
	file: handler(req('/a.js', {})).uri,
	redirect: handler(req('/docs/tasks/review', {
		tag: { value: 'first', multiValue: [{ value: 'first' }, { value: 'second' }] },
		q: { value: 'x' }
	})).headers.location.value,
	bare: handler(req('/docs', {})).headers.location.value
};
console.log(JSON.stringify(out));`
	got, err := exec.Command(node, "-e", script).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `{"dir":"/docs/tasks/review/index.html","file":"/a.js","redirect":"https://p.example/docs/tasks/review/?tag=first&tag=second&q=x","bare":"https://p.example/docs/"}`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestEnsurePreviewRoutingFunction(t *testing.T) {
	tests := []struct {
		name                    string
		fake                    *fakeCloudfront
		wantCreates, wantPublis int
	}{
		{"missing: create and publish", &fakeCloudfront{}, 1, 1},
		{"LIVE present: neither", &fakeCloudfront{devExists: true, liveExists: true}, 0, 0},
		{"DEVELOPMENT only (earlier failure): publish without create", &fakeCloudfront{devExists: true}, 0, 1},
		{"create races a concurrent creator: publish theirs",
			&fakeCloudfront{createErr: &cloudfrontTypes.FunctionAlreadyExists{}, concurrentDev: true}, 1, 1},
		{"publish fails but LIVE exists (concurrent publish): success",
			&fakeCloudfront{devExists: true, publishErr: &cloudfrontTypes.PreconditionFailed{}, concurrentPub: true}, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arn, err := ensurePreviewRoutingFunction(tt.fake)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if arn != testRoutingARN {
				t.Errorf("arn = %q", arn)
			}
			if tt.fake.creates != tt.wantCreates || tt.fake.publishes != tt.wantPublis {
				t.Errorf("creates=%d publishes=%d; want %d/%d", tt.fake.creates, tt.fake.publishes, tt.wantCreates, tt.wantPublis)
			}
			if tt.fake.publishes > 0 && tt.fake.publishedETag != "dev-etag" {
				t.Errorf("published with IfMatch %q; want the DEVELOPMENT ETag", tt.fake.publishedETag)
			}
		})
	}
}

func TestEnsurePreviewRoutingFunction_PublishFailsWithoutLive(t *testing.T) {
	fake := &fakeCloudfront{devExists: true, publishErr: errors.New("boom")}
	if _, err := ensurePreviewRoutingFunction(fake); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v; want the publish error", err)
	}
}

var unrelatedAssociation = cloudfrontTypes.FunctionAssociation{
	FunctionARN: aws.String("arn:aws:cloudfront::123456789012:function/something-else"),
	EventType:   cloudfrontTypes.EventTypeViewerResponse,
}

var routingAssociation = cloudfrontTypes.FunctionAssociation{
	FunctionARN: aws.String(testRoutingARN), EventType: cloudfrontTypes.EventTypeViewerRequest,
}

func distConfig(assocs []cloudfrontTypes.FunctionAssociation, responses []cloudfrontTypes.CustomErrorResponse) *cloudfrontTypes.DistributionConfig {
	return &cloudfrontTypes.DistributionConfig{
		DefaultCacheBehavior: &cloudfrontTypes.DefaultCacheBehavior{
			FunctionAssociations: &cloudfrontTypes.FunctionAssociations{Quantity: aws.Int32(int32(len(assocs))), Items: assocs},
		},
		CustomErrorResponses: &cloudfrontTypes.CustomErrorResponses{Quantity: aws.Int32(int32(len(responses))), Items: responses},
	}
}

func hasRouting(c *cloudfrontTypes.DistributionConfig) bool {
	for _, a := range c.DefaultCacheBehavior.FunctionAssociations.Items {
		if aws.ToString(a.FunctionARN) == testRoutingARN && a.EventType == cloudfrontTypes.EventTypeViewerRequest {
			return true
		}
	}
	return false
}

func hasUnrelated(c *cloudfrontTypes.DistributionConfig) bool {
	for _, a := range c.DefaultCacheBehavior.FunctionAssociations.Items {
		if aws.ToString(a.FunctionARN) == aws.ToString(unrelatedAssociation.FunctionARN) {
			return true
		}
	}
	return false
}

func hasSPAResponses(c *cloudfrontTypes.DistributionConfig) bool {
	n := 0
	for _, r := range c.CustomErrorResponses.Items {
		for _, w := range spaErrorResponses() {
			if sameSPAErrorResponse(r, w) {
				n++
			}
		}
	}
	return n == 2
}

func checkQuantities(t *testing.T, c *cloudfrontTypes.DistributionConfig) {
	t.Helper()
	if aws.ToInt32(c.DefaultCacheBehavior.FunctionAssociations.Quantity) != int32(len(c.DefaultCacheBehavior.FunctionAssociations.Items)) {
		t.Error("function association Quantity doesn't match Items")
	}
	if aws.ToInt32(c.CustomErrorResponses.Quantity) != int32(len(c.CustomErrorResponses.Items)) {
		t.Error("error response Quantity doesn't match Items")
	}
}

func TestReconcilePreviewRouting(t *testing.T) {
	t.Run("non-SPA without association: added, SPA responses removed", func(t *testing.T) {
		c := distConfig([]cloudfrontTypes.FunctionAssociation{unrelatedAssociation}, spaErrorResponses())
		if !reconcilePreviewRouting(c, false, testRoutingARN) {
			t.Fatal("changed = false")
		}
		if !hasRouting(c) || !hasUnrelated(c) || len(c.CustomErrorResponses.Items) != 0 {
			t.Fatalf("config = %+v / %+v", c.DefaultCacheBehavior.FunctionAssociations, c.CustomErrorResponses)
		}
		checkQuantities(t, c)
	})
	t.Run("non-SPA already correct: unchanged", func(t *testing.T) {
		c := distConfig([]cloudfrontTypes.FunctionAssociation{unrelatedAssociation, routingAssociation}, nil)
		if reconcilePreviewRouting(c, false, testRoutingARN) {
			t.Fatal("changed = true")
		}
	})
	t.Run("SPA with the shared association: removed, responses present", func(t *testing.T) {
		c := distConfig([]cloudfrontTypes.FunctionAssociation{unrelatedAssociation, routingAssociation}, nil)
		if !reconcilePreviewRouting(c, true, "") {
			t.Fatal("changed = false")
		}
		if hasRouting(c) || !hasUnrelated(c) || !hasSPAResponses(c) {
			t.Fatalf("config = %+v / %+v", c.DefaultCacheBehavior.FunctionAssociations, c.CustomErrorResponses)
		}
		checkQuantities(t, c)
	})
	t.Run("SPA already correct: unchanged", func(t *testing.T) {
		c := distConfig([]cloudfrontTypes.FunctionAssociation{unrelatedAssociation}, spaErrorResponses())
		if reconcilePreviewRouting(c, true, "") {
			t.Fatal("changed = true")
		}
	})
}

func TestDeployPreviewDistribution_Reuse(t *testing.T) {
	t.Run("config wrong: one UpdateDistribution with the ETag", func(t *testing.T) {
		fake := &fakeCloudfront{liveExists: true, devExists: true, distETag: "etag-7",
			distConfig: distConfig(nil, spaErrorResponses())}
		var logs bytes.Buffer
		dist, err := deployPreviewDistribution(fake, StaticPreviewDeployInput{PreviewID: "p1", ExistingDistID: "E1"}, nil, &logs)
		if err != nil || dist != nil {
			t.Fatalf("dist=%v err=%v", dist, err)
		}
		if len(fake.updates) != 1 || aws.ToString(fake.updates[0].IfMatch) != "etag-7" || aws.ToString(fake.updates[0].Id) != "E1" {
			t.Fatalf("updates = %+v", fake.updates)
		}
		if !hasRouting(fake.updates[0].DistributionConfig) {
			t.Error("update lacks the routing association")
		}
		if !strings.Contains(logs.String(), "Updated preview distribution E1 to serve <page>/index.html at <page>/ (takes a few minutes to propagate)") {
			t.Errorf("logs = %q", logs.String())
		}
		if fake.createdDist != nil {
			t.Error("reuse created a distribution")
		}
	})
	t.Run("config right: no UpdateDistribution", func(t *testing.T) {
		fake := &fakeCloudfront{liveExists: true, devExists: true, distETag: "etag-7",
			distConfig: distConfig([]cloudfrontTypes.FunctionAssociation{routingAssociation}, nil)}
		var logs bytes.Buffer
		if _, err := deployPreviewDistribution(fake, StaticPreviewDeployInput{ExistingDistID: "E1"}, nil, &logs); err != nil {
			t.Fatal(err)
		}
		if len(fake.updates) != 0 || strings.Contains(logs.String(), "Updated preview distribution") {
			t.Fatalf("updates = %d, logs = %q", len(fake.updates), logs.String())
		}
	})
	t.Run("SPA: shared association removed without ensuring the function", func(t *testing.T) {
		fake := &fakeCloudfront{distETag: "etag-7",
			distConfig: distConfig([]cloudfrontTypes.FunctionAssociation{routingAssociation}, nil)}
		var logs bytes.Buffer
		if _, err := deployPreviewDistribution(fake, StaticPreviewDeployInput{ExistingDistID: "E1", IsSPA: true}, nil, &logs); err != nil {
			t.Fatal(err)
		}
		if len(fake.updates) != 1 || hasRouting(fake.updates[0].DistributionConfig) || fake.creates != 0 {
			t.Fatalf("updates = %+v, creates = %d", fake.updates, fake.creates)
		}
	})
}

func TestDeployPreviewDistribution_FirstDeploy(t *testing.T) {
	for _, isSPA := range []bool{false, true} {
		fake := &fakeCloudfront{}
		var logs bytes.Buffer
		dist, err := deployPreviewDistribution(fake, StaticPreviewDeployInput{OrgID: "o", PreviewID: "p1", Region: "us-east-1", IsSPA: isSPA},
			aws.String("o-p1.s3.amazonaws.com"), &logs)
		if err != nil || dist == nil || aws.ToString(dist.Id) != "E1" {
			t.Fatalf("isSPA=%v: dist=%v err=%v", isSPA, dist, err)
		}
		c := fake.createdDist
		if hasRouting(c) == isSPA || hasSPAResponses(c) != isSPA {
			t.Errorf("isSPA=%v: associations=%+v responses=%+v", isSPA, c.DefaultCacheBehavior.FunctionAssociations, c.CustomErrorResponses)
		}
		checkQuantities(t, c)
		if len(fake.updates) != 0 {
			t.Errorf("isSPA=%v: first deploy updated the distribution", isSPA)
		}
	}
}

// A new distribution's id, ARN and domain are handed to OnDistributionCreated as
// soon as it is created; a reused one calls nothing.
func TestDeployPreviewDistribution_OnDistributionCreated(t *testing.T) {
	var got []string
	in := StaticPreviewDeployInput{OrgID: "o", PreviewID: "p1", Region: "us-east-1", IsSPA: true,
		OnDistributionCreated: func(id, arn, domain string) { got = append(got, id+" "+arn+" "+domain) }}
	if _, err := deployPreviewDistribution(&fakeCloudfront{}, in, aws.String("o-p1.s3.amazonaws.com"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "E1 arn:dist d1.cloudfront.net" {
		t.Errorf("OnDistributionCreated calls = %q", got)
	}

	got = nil
	in.ExistingDistID = "E1"
	fake := &fakeCloudfront{distETag: "etag-7", distConfig: distConfig(nil, spaErrorResponses())}
	if _, err := deployPreviewDistribution(fake, in, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("reuse called OnDistributionCreated: %q", got)
	}
}

// An OAC left by an earlier failed first deploy is found by its name (on a later
// page) and the distribution is created with it; any other error fails the deploy.
func TestDeployPreviewDistribution_ExistingOAC(t *testing.T) {
	fake := &fakeCloudfront{
		createOACErr: &cloudfrontTypes.OriginAccessControlAlreadyExists{Message: aws.String("exists")},
		oacPages: [][]cloudfrontTypes.OriginAccessControlSummary{
			{{Id: aws.String("oac-other"), Name: aws.String("preview-p10")}},
			{{Id: aws.String("oac-old"), Name: aws.String("preview-p1")}},
		},
	}
	var logs bytes.Buffer
	dist, err := deployPreviewDistribution(fake, StaticPreviewDeployInput{OrgID: "o", PreviewID: "p1", Region: "us-east-1", IsSPA: true},
		aws.String("o-p1.s3.amazonaws.com"), &logs)
	if err != nil || dist == nil {
		t.Fatalf("dist=%v err=%v", dist, err)
	}
	if oac := aws.ToString(fake.createdDist.Origins.Items[0].OriginAccessControlId); oac != "oac-old" {
		t.Errorf("distribution's OAC = %q, want oac-old", oac)
	}
	if fake.oacLists != 2 || !strings.Contains(logs.String(), "Reusing origin access control preview-p1: oac-old") {
		t.Errorf("lists = %d, logs = %q", fake.oacLists, logs.String())
	}

	fake = &fakeCloudfront{createOACErr: errors.New("access denied")}
	_, err = deployPreviewDistribution(fake, StaticPreviewDeployInput{OrgID: "o", PreviewID: "p1", Region: "us-east-1", IsSPA: true},
		aws.String("o-p1.s3.amazonaws.com"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "access denied") || fake.createdDist != nil || fake.oacLists != 0 {
		t.Errorf("err = %v, created = %v, lists = %d", err, fake.createdDist != nil, fake.oacLists)
	}
}

func TestPreviewDistributionArn(t *testing.T) {
	t.Run("saved ARN is used without a lookup", func(t *testing.T) {
		fake := &fakeCloudfront{}
		arn, err := previewDistributionArn(fake, StaticPreviewDeployInput{ExistingDistID: "E1", ExistingDistArn: "arn:saved"})
		if err != nil || arn != "arn:saved" {
			t.Fatalf("arn, err = %q, %v; want arn:saved, nil", arn, err)
		}
		if len(fake.getDists) != 0 {
			t.Errorf("GetDistribution called with %v, want no call", fake.getDists)
		}
	})
	t.Run("no saved ARN (the record carries only the id): looked up", func(t *testing.T) {
		fake := &fakeCloudfront{}
		arn, err := previewDistributionArn(fake, StaticPreviewDeployInput{ExistingDistID: "E1"})
		if err != nil || arn != "arn:E1" {
			t.Fatalf("arn, err = %q, %v; want arn:E1, nil", arn, err)
		}
		if len(fake.getDists) != 1 || fake.getDists[0] != "E1" {
			t.Errorf("GetDistribution called with %v, want [E1]", fake.getDists)
		}
	})
	t.Run("lookup error fails", func(t *testing.T) {
		fake := &fakeCloudfront{getDistErr: errors.New("boom")}
		if _, err := previewDistributionArn(fake, StaticPreviewDeployInput{ExistingDistID: "E1"}); err == nil {
			t.Fatal("err = nil, want the lookup error")
		}
	})
}

// A preview with no saved distribution adopts the one an earlier deploy created
// (found by its Comment, on a later page): in now reuses it, OnDistributionCreated
// saves it, and the deploy creates no second distribution and no OAC.
func TestAdoptPreviewDistribution(t *testing.T) {
	fake := &fakeCloudfront{distETag: "etag-7", distConfig: distConfig(nil, spaErrorResponses()),
		createOACErr: errors.New("no OAC may be created"),
		distPages: [][]cloudfrontTypes.DistributionSummary{
			{{Id: aws.String("E0"), Comment: aws.String("Preview distribution for p10")}},
			{{Id: aws.String("E7"), ARN: aws.String("arn:E7"), DomainName: aws.String("d7.cloudfront.net"), Comment: aws.String("Preview distribution for p1")}},
		}}
	var saved []string
	in := StaticPreviewDeployInput{OrgID: "o", PreviewID: "p1", Region: "us-east-1", IsSPA: true,
		OnDistributionCreated: func(id, arn, domain string) { saved = append(saved, id+" "+arn+" "+domain) }}
	var logs bytes.Buffer
	domain, err := adoptPreviewDistribution(fake, &in, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if domain != "d7.cloudfront.net" || in.ExistingDistID != "E7" || in.ExistingDistArn != "arn:E7" {
		t.Errorf("adopted domain %q, ExistingDistID %q, ExistingDistArn %q", domain, in.ExistingDistID, in.ExistingDistArn)
	}
	if len(saved) != 1 || saved[0] != "E7 arn:E7 d7.cloudfront.net" {
		t.Errorf("OnDistributionCreated calls = %q", saved)
	}
	if !strings.Contains(logs.String(), "Reusing the preview's CloudFront distribution E7") {
		t.Errorf("logs = %q", logs.String())
	}
	if _, err := deployPreviewDistribution(fake, in, aws.String("o-p1.s3.amazonaws.com"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if fake.createdDist != nil {
		t.Error("created a second distribution for the preview")
	}
}

// Nothing to adopt: a preview with a saved distribution doesn't look, and one
// whose Comment matches nothing is left as it was; a failed lookup fails.
func TestAdoptPreviewDistribution_NothingToAdopt(t *testing.T) {
	fake := &fakeCloudfront{distPages: [][]cloudfrontTypes.DistributionSummary{
		{{Id: aws.String("E7"), Comment: aws.String("Preview distribution for p1")}}}}
	in := StaticPreviewDeployInput{PreviewID: "p1", ExistingDistID: "E1"}
	if domain, err := adoptPreviewDistribution(fake, &in, io.Discard); err != nil || domain != "" || in.ExistingDistID != "E1" || fake.distLists != 0 {
		t.Errorf("saved distribution: domain %q, err %v, ExistingDistID %q, lists %d", domain, err, in.ExistingDistID, fake.distLists)
	}

	called := false
	in = StaticPreviewDeployInput{PreviewID: "p2", OnDistributionCreated: func(string, string, string) { called = true }}
	if domain, err := adoptPreviewDistribution(fake, &in, io.Discard); err != nil || domain != "" || in.ExistingDistID != "" || called {
		t.Errorf("no match: domain %q, err %v, ExistingDistID %q, saved %v", domain, err, in.ExistingDistID, called)
	}

	fake = &fakeCloudfront{listErr: errors.New("boom")}
	in = StaticPreviewDeployInput{PreviewID: "p1"}
	if _, err := adoptPreviewDistribution(fake, &in, io.Discard); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failed lookup: err %v", err)
	}
}
