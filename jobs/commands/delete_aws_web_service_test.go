package commands

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// fakeWebServiceAWS stands in for every AWS client deleting a web service uses.
// It records each call, in order, as "<operation> <argument>"; errs makes an
// operation fail and the found* fields are what the by-name lookups return.
type fakeWebServiceAWS struct {
	calls []string
	errs  map[string]error

	distributionEnabled bool

	foundLoadBalancers []elbTypes.LoadBalancer
	foundTargetGroups  []elbTypes.TargetGroup
	foundGroups        []ec2Types.SecurityGroup

	// serviceGroups are the security groups DescribeServices says the service
	// runs in; groups what DescribeSecurityGroups by id finds; deleteGroupErrs
	// the errors deleting a group returns, in turn.
	serviceGroups   []string
	groups          map[string]ec2Types.SecurityGroup
	deleteGroupErrs map[string][]error
}

func (f *fakeWebServiceAWS) call(operation, argument string) error {
	f.calls = append(f.calls, operation+" "+argument)
	return f.errs[operation]
}

func (f *fakeWebServiceAWS) GetDistributionConfig(_ context.Context, in *cloudfront.GetDistributionConfigInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	if err := f.call("GetDistributionConfig", aws.ToString(in.Id)); err != nil {
		return nil, err
	}
	return &cloudfront.GetDistributionConfigOutput{
		DistributionConfig: &cloudfrontTypes.DistributionConfig{Enabled: aws.Bool(f.distributionEnabled)},
		ETag:               aws.String("etag-config"),
	}, nil
}

func (f *fakeWebServiceAWS) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	if aws.ToBool(in.DistributionConfig.Enabled) {
		return nil, errors.New("update should disable the distribution")
	}
	if err := f.call("UpdateDistribution", aws.ToString(in.Id)+" "+aws.ToString(in.IfMatch)); err != nil {
		return nil, err
	}
	return &cloudfront.UpdateDistributionOutput{ETag: aws.String("etag-updated")}, nil
}

func (f *fakeWebServiceAWS) GetDistribution(_ context.Context, in *cloudfront.GetDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	if err := f.call("GetDistribution", aws.ToString(in.Id)); err != nil {
		return nil, err
	}
	return &cloudfront.GetDistributionOutput{Distribution: &cloudfrontTypes.Distribution{Status: aws.String("Deployed")}}, nil
}

func (f *fakeWebServiceAWS) DeleteDistribution(_ context.Context, in *cloudfront.DeleteDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error) {
	return &cloudfront.DeleteDistributionOutput{}, f.call("DeleteDistribution", aws.ToString(in.Id)+" "+aws.ToString(in.IfMatch))
}

func (f *fakeWebServiceAWS) DeleteService(_ context.Context, in *ecs.DeleteServiceInput, _ ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error) {
	return &ecs.DeleteServiceOutput{}, f.call("DeleteService", aws.ToString(in.Service))
}

func (f *fakeWebServiceAWS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if err := f.call("DescribeServices", strings.Join(in.Services, ",")); err != nil {
		return nil, err
	}
	service := ecsTypes.Service{Status: aws.String("INACTIVE")}
	if len(f.serviceGroups) > 0 {
		service.NetworkConfiguration = &ecsTypes.NetworkConfiguration{AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{
			Subnets: []string{"subnet-a"}, SecurityGroups: f.serviceGroups,
		}}
	}
	return &ecs.DescribeServicesOutput{Services: []ecsTypes.Service{service}}, nil
}

func (f *fakeWebServiceAWS) ListTaskDefinitions(_ context.Context, in *ecs.ListTaskDefinitionsInput, _ ...func(*ecs.Options)) (*ecs.ListTaskDefinitionsOutput, error) {
	return &ecs.ListTaskDefinitionsOutput{}, f.call("ListTaskDefinitions", aws.ToString(in.FamilyPrefix))
}

func (f *fakeWebServiceAWS) DeregisterTaskDefinition(_ context.Context, in *ecs.DeregisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error) {
	return &ecs.DeregisterTaskDefinitionOutput{}, f.call("DeregisterTaskDefinition", aws.ToString(in.TaskDefinition))
}

func (f *fakeWebServiceAWS) DeleteTaskDefinitions(_ context.Context, in *ecs.DeleteTaskDefinitionsInput, _ ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error) {
	return &ecs.DeleteTaskDefinitionsOutput{}, f.call("DeleteTaskDefinitions", strings.Join(in.TaskDefinitions, ","))
}

func (f *fakeWebServiceAWS) DeleteRepository(_ context.Context, in *ecr.DeleteRepositoryInput, _ ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error) {
	return &ecr.DeleteRepositoryOutput{}, f.call("DeleteRepository", aws.ToString(in.RepositoryName))
}

func (f *fakeWebServiceAWS) DescribeLoadBalancers(_ context.Context, in *elasticloadbalancingv2.DescribeLoadBalancersInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error) {
	if len(in.Names) > 0 {
		if err := f.call("DescribeLoadBalancers", "name="+strings.Join(in.Names, ",")); err != nil {
			return nil, err
		}
		return &elasticloadbalancingv2.DescribeLoadBalancersOutput{LoadBalancers: f.foundLoadBalancers}, nil
	}
	// The deleted waiter: the load balancer is gone.
	f.call("DescribeLoadBalancers", strings.Join(in.LoadBalancerArns, ","))
	return nil, &elbTypes.LoadBalancerNotFoundException{Message: aws.String("gone")}
}

func (f *fakeWebServiceAWS) DeleteLoadBalancer(_ context.Context, in *elasticloadbalancingv2.DeleteLoadBalancerInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error) {
	return &elasticloadbalancingv2.DeleteLoadBalancerOutput{}, f.call("DeleteLoadBalancer", aws.ToString(in.LoadBalancerArn))
}

func (f *fakeWebServiceAWS) DescribeTargetGroups(_ context.Context, in *elasticloadbalancingv2.DescribeTargetGroupsInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error) {
	if err := f.call("DescribeTargetGroups", "name="+strings.Join(in.Names, ",")); err != nil {
		return nil, err
	}
	return &elasticloadbalancingv2.DescribeTargetGroupsOutput{TargetGroups: f.foundTargetGroups}, nil
}

func (f *fakeWebServiceAWS) DeleteTargetGroup(_ context.Context, in *elasticloadbalancingv2.DeleteTargetGroupInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteTargetGroupOutput, error) {
	return &elasticloadbalancingv2.DeleteTargetGroupOutput{}, f.call("DeleteTargetGroup", aws.ToString(in.TargetGroupArn))
}

func (f *fakeWebServiceAWS) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if len(in.GroupIds) > 0 {
		f.calls = append(f.calls, "DescribeSecurityGroups id="+strings.Join(in.GroupIds, ","))
		group, ok := f.groups[in.GroupIds[0]]
		if !ok {
			return nil, &smithy.GenericAPIError{Code: "InvalidGroup.NotFound"}
		}
		return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2Types.SecurityGroup{group}}, nil
	}
	var filter string
	for _, fl := range in.Filters {
		filter += aws.ToString(fl.Name) + "=" + strings.Join(fl.Values, ",")
	}
	if err := f.call("DescribeSecurityGroups", filter); err != nil {
		return nil, err
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.foundGroups}, nil
}

func (f *fakeWebServiceAWS) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	id := aws.ToString(in.GroupId)
	if errs := f.deleteGroupErrs[id]; len(errs) > 0 {
		f.calls = append(f.calls, "DeleteSecurityGroup "+id)
		f.deleteGroupErrs[id] = errs[1:]
		return nil, errs[0]
	}
	return &ec2.DeleteSecurityGroupOutput{}, f.call("DeleteSecurityGroup", aws.ToString(in.GroupId))
}

func (f *fakeWebServiceAWS) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	var rules []string
	for _, p := range in.IpPermissions {
		for _, pair := range p.UserIdGroupPairs {
			rules = append(rules, aws.ToString(pair.GroupId)+":"+strconv.Itoa(int(aws.ToInt32(p.FromPort))))
		}
	}
	return &ec2.RevokeSecurityGroupIngressOutput{}, f.call("RevokeSecurityGroupIngress", aws.ToString(in.GroupId)+" "+strings.Join(rules, ","))
}

func (f *fakeWebServiceAWS) clients() deleteWebServiceClients {
	return deleteWebServiceClients{ecs: f, ecr: f, elb: f, ec2: f, cloudfront: f}
}

const (
	testLoadBalancerArn = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/alb-deployment-1/1"
	testTargetGroupArn  = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/tg-deployment-1/1"
)

// webServiceDeleteParameters are a web service delete job's parameters with
// every handle saved and no CloudFront distribution, as for production.
func webServiceDeleteParameters() map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "deployment-1")
	jobs.SetParameterValue(parameters, parameters_enums.EcsClusterArn, "cluster-1")
	jobs.SetParameterValue(parameters, parameters_enums.LoadBalancerArn, testLoadBalancerArn)
	jobs.SetParameterValue(parameters, parameters_enums.TargetGroupArn, testTargetGroupArn)
	jobs.SetParameterValue(parameters, parameters_enums.AlbSecurityGroupId, "sg-alb")
	jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, "")
	return parameters
}

// productionDeleteCalls is the sequence of AWS operations for a web service with
// every handle saved: the service is described first for its security groups.
var productionDeleteCalls = []string{
	"DescribeServices es-deployment-1",
	"DeleteService es-deployment-1",
	"DescribeServices es-deployment-1",
	"ListTaskDefinitions td-deployment-1",
	"ListTaskDefinitions td-deployment-1-pre-deploy",
	"DeleteRepository ecr-org-1-deployment-1",
	"DeleteLoadBalancer " + testLoadBalancerArn,
	"DescribeLoadBalancers " + testLoadBalancerArn,
	"DeleteTargetGroup " + testTargetGroupArn,
	"DeleteSecurityGroup sg-alb",
}

func runWebServiceDelete(t *testing.T, parameters map[string]interface{}, fake *fakeWebServiceAWS) (string, error) {
	t.Helper()
	savedPause := deleteWebServiceLoadBalancerPause
	savedGrant := grantPreviewDeployAccess
	t.Cleanup(func() {
		deleteWebServiceLoadBalancerPause = savedPause
		grantPreviewDeployAccess = savedGrant
	})
	deleteWebServiceLoadBalancerPause = 0
	grantPreviewDeployAccess = func(map[string]interface{}) error {
		fake.calls = append(fake.calls, "grantPreviewDeployAccess")
		return nil
	}
	var logs bytes.Buffer
	err := deleteWebServiceResources(parameters, fake.clients(), &logs)
	return logs.String(), err
}

// With every handle saved and no CloudFront distribution, the delete makes the
// same AWS calls in the same order as it always has, after describing the service.
func TestDeleteWebServiceAllHandlesSet(t *testing.T) {
	fake := &fakeWebServiceAWS{}
	if _, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !reflect.DeepEqual(fake.calls, productionDeleteCalls) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(productionDeleteCalls, "\n"))
	}
}

// A service deployed from an image has no repository to delete.
func TestDeleteWebServiceDeployedFromImageSkipsRepository(t *testing.T) {
	parameters := webServiceDeleteParameters()
	jobs.SetParameterValue(parameters, parameters_enums.DeployedFromImage, true)
	fake := &fakeWebServiceAWS{}
	if _, err := runWebServiceDelete(t, parameters, fake); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, c := range fake.calls {
		if strings.HasPrefix(c, "DeleteRepository") {
			t.Fatalf("deleted a repository for a service deployed from an image: %q", fake.calls)
		}
	}
}

// Every step treats a resource that doesn't exist as already deleted and the
// next step still runs.
func TestDeleteWebServiceAlreadyGone(t *testing.T) {
	for _, tc := range []struct {
		operation string
		err       error
		skipped   []string
		log       string
	}{
		{"DeleteService", &ecsTypes.ServiceNotFoundException{}, []string{"DescribeServices es-deployment-1"}, "ECS service es-deployment-1 was already gone"},
		{"DeleteRepository", &ecrTypes.RepositoryNotFoundException{}, nil, "ECR repository ecr-org-1-deployment-1 was already gone"},
		{"DeleteLoadBalancer", &elbTypes.LoadBalancerNotFoundException{}, []string{"DescribeLoadBalancers " + testLoadBalancerArn}, "Load balancer " + testLoadBalancerArn + " was already gone"},
		{"DeleteTargetGroup", &elbTypes.TargetGroupNotFoundException{}, nil, "Target group " + testTargetGroupArn + " was already gone"},
		{"DeleteSecurityGroup", &smithy.GenericAPIError{Code: "InvalidGroup.NotFound"}, nil, "Security group sg-alb was already gone"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			fake := &fakeWebServiceAWS{errs: map[string]error{tc.operation: tc.err}}
			logs, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			var want []string
			for i, c := range productionDeleteCalls {
				// The first call is the describe before the delete; it always runs.
				if i == 0 || !containsCall(tc.skipped, c) {
					want = append(want, c)
				}
			}
			if !reflect.DeepEqual(fake.calls, want) {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
			}
			if !strings.Contains(logs, tc.log) {
				t.Errorf("logs lack %q:\n%s", tc.log, logs)
			}
		})
	}
}

func containsCall(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// A purged service answers ServiceNotFoundException to the describe before the
// delete as well: the delete carries on as for any service already gone.
func TestDeleteWebServicePurgedService(t *testing.T) {
	notFound := &ecsTypes.ServiceNotFoundException{}
	fake := &fakeWebServiceAWS{errs: map[string]error{"DescribeServices": notFound, "DeleteService": notFound}}
	logs, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !containsCall(fake.calls, "DeleteRepository ecr-org-1-deployment-1") || !strings.Contains(logs, "was already gone") {
		t.Errorf("calls:\n%s\nlogs:\n%s", strings.Join(fake.calls, "\n"), logs)
	}
}

// An ECS service that is no longer active is still waited for.
func TestDeleteWebServiceNotActiveStillWaits(t *testing.T) {
	fake := &fakeWebServiceAWS{errs: map[string]error{"DeleteService": &ecsTypes.ServiceNotActiveException{}}}
	if _, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !reflect.DeepEqual(fake.calls, productionDeleteCalls) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(productionDeleteCalls, "\n"))
	}
}

// Any other error fails the job at that step.
func TestDeleteWebServiceOtherErrorFails(t *testing.T) {
	for i, operation := range []string{"DeleteService", "DeleteRepository", "DeleteLoadBalancer", "DeleteTargetGroup", "DeleteSecurityGroup"} {
		t.Run(operation, func(t *testing.T) {
			failure := &smithy.GenericAPIError{Code: "AccessDenied"}
			fake := &fakeWebServiceAWS{errs: map[string]error{operation: failure}}
			_, err := runWebServiceDelete(t, webServiceDeleteParameters(), fake)
			if !errors.Is(err, failure) {
				t.Fatalf("err = %v, want %v", err, failure)
			}
			last := fake.calls[len(fake.calls)-1]
			if !strings.HasPrefix(last, operation) {
				t.Errorf("carried on past the failed %s (step %d): %q", operation, i, fake.calls)
			}
		})
	}
}

// Handles that were never saved are looked up by the names the deploy gives them.
func TestDeleteWebServiceLooksUpMissingHandles(t *testing.T) {
	parameters := webServiceDeleteParameters()
	jobs.SetParameterValue(parameters, parameters_enums.LoadBalancerArn, "")
	jobs.SetParameterValue(parameters, parameters_enums.TargetGroupArn, "")
	key, _ := parameters_enums.AlbSecurityGroupId.Key()
	delete(parameters, key)
	fake := &fakeWebServiceAWS{
		foundLoadBalancers: []elbTypes.LoadBalancer{{LoadBalancerArn: aws.String(testLoadBalancerArn)}},
		foundTargetGroups:  []elbTypes.TargetGroup{{TargetGroupArn: aws.String(testTargetGroupArn)}},
		foundGroups:        []ec2Types.SecurityGroup{{GroupId: aws.String("sg-found")}},
	}
	if _, err := runWebServiceDelete(t, parameters, fake); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []string{
		"DescribeServices es-deployment-1",
		"DeleteService es-deployment-1",
		"DescribeServices es-deployment-1",
		"ListTaskDefinitions td-deployment-1",
		"ListTaskDefinitions td-deployment-1-pre-deploy",
		"DeleteRepository ecr-org-1-deployment-1",
		"DescribeLoadBalancers name=alb-deployment-1",
		"DeleteLoadBalancer " + testLoadBalancerArn,
		"DescribeLoadBalancers " + testLoadBalancerArn,
		"DescribeTargetGroups name=tg-deployment-1",
		"DeleteTargetGroup " + testTargetGroupArn,
		"DescribeSecurityGroups group-name=albsg-deployment-1",
		"DeleteSecurityGroup sg-found",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
}

// A lookup that finds nothing skips that step; the rest still run.
func TestDeleteWebServiceLookupFindsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs map[string]error
	}{
		{"empty results", nil},
		{"not found errors", map[string]error{
			"DescribeLoadBalancers": &elbTypes.LoadBalancerNotFoundException{},
			"DescribeTargetGroups":  &elbTypes.TargetGroupNotFoundException{},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := webServiceDeleteParameters()
			jobs.SetParameterValue(parameters, parameters_enums.LoadBalancerArn, "")
			jobs.SetParameterValue(parameters, parameters_enums.TargetGroupArn, "")
			jobs.SetParameterValue(parameters, parameters_enums.AlbSecurityGroupId, "")
			fake := &fakeWebServiceAWS{errs: tc.errs}
			logs, err := runWebServiceDelete(t, parameters, fake)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			want := []string{
				"DescribeServices es-deployment-1",
				"DeleteService es-deployment-1",
				"DescribeServices es-deployment-1",
				"ListTaskDefinitions td-deployment-1",
				"ListTaskDefinitions td-deployment-1-pre-deploy",
				"DeleteRepository ecr-org-1-deployment-1",
				"DescribeLoadBalancers name=alb-deployment-1",
				"DescribeTargetGroups name=tg-deployment-1",
				"DescribeSecurityGroups group-name=albsg-deployment-1",
			}
			if !reflect.DeepEqual(fake.calls, want) {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
			}
			for _, line := range []string{
				"Load balancer alb-deployment-1 was already gone",
				"Target group tg-deployment-1 was already gone",
				"Security group albsg-deployment-1 was already gone",
			} {
				if !strings.Contains(logs, line) {
					t.Errorf("logs lack %q:\n%s", line, logs)
				}
			}
		})
	}
}

// A backend preview's distribution is disabled, waited for and deleted before
// anything else, after the runner grants itself the CloudFront actions.
func TestDeleteWebServiceCloudfrontFirst(t *testing.T) {
	parameters := webServiceDeleteParameters()
	jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, "E123")
	fake := &fakeWebServiceAWS{distributionEnabled: true}
	if _, err := runWebServiceDelete(t, parameters, fake); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := append([]string{
		"grantPreviewDeployAccess",
		"GetDistributionConfig E123",
		"UpdateDistribution E123 etag-config",
		"GetDistribution E123",
		"DeleteDistribution E123 etag-updated",
	}, productionDeleteCalls...)
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
}

// An already disabled distribution isn't updated again.
func TestDeleteWebServiceCloudfrontAlreadyDisabled(t *testing.T) {
	parameters := webServiceDeleteParameters()
	jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, "E123")
	fake := &fakeWebServiceAWS{distributionEnabled: false}
	if _, err := runWebServiceDelete(t, parameters, fake); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := append([]string{
		"grantPreviewDeployAccess",
		"GetDistributionConfig E123",
		"GetDistribution E123",
		"DeleteDistribution E123 etag-config",
	}, productionDeleteCalls...)
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
}

// A distribution that doesn't exist is already deleted; anything else fails.
func TestDeleteWebServiceCloudfrontErrors(t *testing.T) {
	parameters := webServiceDeleteParameters()
	jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, "E123")
	fake := &fakeWebServiceAWS{errs: map[string]error{"GetDistributionConfig": &cloudfrontTypes.NoSuchDistribution{}}}
	logs, err := runWebServiceDelete(t, parameters, fake)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := append([]string{"grantPreviewDeployAccess", "GetDistributionConfig E123"}, productionDeleteCalls...)
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(logs, "CloudFront distribution E123 was already gone") {
		t.Errorf("logs lack the already gone line:\n%s", logs)
	}

	failure := &cloudfrontTypes.PreconditionFailed{}
	fake = &fakeWebServiceAWS{distributionEnabled: true, errs: map[string]error{"UpdateDistribution": failure}}
	if _, err := runWebServiceDelete(t, parameters, fake); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want %v", err, failure)
	}
	if containsCall(fake.calls, "DeleteService es-deployment-1") {
		t.Errorf("deleted the ECS service after CloudFront failed: %q", fake.calls)
	}
}

// The static site delete still fails when its distribution doesn't exist: it
// needs the distribution's config to clean up after it.
func TestDeleteCloudfrontDistributionMissingFails(t *testing.T) {
	fake := &fakeWebServiceAWS{errs: map[string]error{"GetDistributionConfig": &cloudfrontTypes.NoSuchDistribution{}}}
	var logs bytes.Buffer
	if _, err := deleteCloudfrontDistribution(fake, "E123", &logs); err == nil {
		t.Fatal("want an error for a distribution that doesn't exist")
	}
}

// A distribution deleted after its config was read is already gone too, whether
// the deployed waiter or the delete finds it missing.
func TestDeleteWebServiceCloudfrontGoneMidway(t *testing.T) {
	for _, operation := range []string{"GetDistribution", "DeleteDistribution"} {
		t.Run(operation, func(t *testing.T) {
			parameters := webServiceDeleteParameters()
			jobs.SetParameterValue(parameters, parameters_enums.CloudfrontID, "E123")
			fake := &fakeWebServiceAWS{errs: map[string]error{operation: &cloudfrontTypes.NoSuchDistribution{}}}
			logs, err := runWebServiceDelete(t, parameters, fake)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			want := []string{"grantPreviewDeployAccess", "GetDistributionConfig E123", "GetDistribution E123"}
			if operation == "DeleteDistribution" {
				want = append(want, "DeleteDistribution E123 etag-config")
			}
			want = append(want, productionDeleteCalls...)
			if !reflect.DeepEqual(fake.calls, want) {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
			}
			if !strings.Contains(logs, "CloudFront distribution E123 was already gone") {
				t.Errorf("logs lack the already gone line:\n%s", logs)
			}
		})
	}
}
