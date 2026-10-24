package commands

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sd_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	testNamespaceName  = "prod-0001"
	testNamespaceID    = "ns-prod0001"
	testNamespaceArn   = "arn:aws:servicediscovery:eu-west-1:123456789012:namespace/" + testNamespaceID
	testAlias          = "api-nt-1"
	testDnsServiceArn  = "arn:aws:servicediscovery:eu-west-1:123456789012:service/srv-api"
	testNewServiceArn  = "arn:aws:servicediscovery:eu-west-1:123456789012:service/srv-new"
	testEcsServiceArn  = "arn:aws:ecs:eu-west-1:123456789012:service/c/es-deployment-1"
	testNoDnsNameLog   = "Couldn't give " + testAlias + " a DNS name: "
	testStillReachable = "It's still reachable from other services over Service Connect.\n"
)

// fakeCloudMap stands in for Cloud Map. It records each call as
// "<operation> <argument>"; errs makes an operation fail.
type fakeCloudMap struct {
	calls         []string
	errs          map[string]error
	namespaceType sd_types.NamespaceType
	// emptyNamespacePages are the empty filtered pages ListNamespaces returns
	// before the matching namespace.
	emptyNamespacePages int
	// services are the namespace's existing services, one page each.
	services     [][]sd_types.ServiceSummary
	createInputs []*servicediscovery.CreateServiceInput
	deleteErrs   []error // one per DeleteService call; nil after they run out
}

func newFakeCloudMap() *fakeCloudMap {
	return &fakeCloudMap{namespaceType: sd_types.NamespaceTypeDnsPrivate}
}

func (f *fakeCloudMap) call(operation, argument string) error {
	f.calls = append(f.calls, operation+" "+argument)
	return f.errs[operation]
}

func (f *fakeCloudMap) ListNamespaces(_ context.Context, in *servicediscovery.ListNamespacesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.ListNamespacesOutput, error) {
	filter := in.Filters[0]
	if err := f.call("ListNamespaces", string(filter.Name)+"="+strings.Join(filter.Values, ",")); err != nil {
		return nil, err
	}
	if filter.Values[0] != testNamespaceName {
		return &servicediscovery.ListNamespacesOutput{}, nil
	}
	page := 0
	if in.NextToken != nil {
		page = int(aws.ToString(in.NextToken)[0] - '0')
	}
	if page < f.emptyNamespacePages {
		return &servicediscovery.ListNamespacesOutput{NextToken: aws.String(string(rune('0' + page + 1)))}, nil
	}
	return &servicediscovery.ListNamespacesOutput{Namespaces: []sd_types.NamespaceSummary{{
		Id: aws.String(testNamespaceID), Name: aws.String(testNamespaceName), Arn: aws.String(testNamespaceArn), Type: f.namespaceType,
	}}}, nil
}

func (f *fakeCloudMap) CreatePrivateDnsNamespace(_ context.Context, in *servicediscovery.CreatePrivateDnsNamespaceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.CreatePrivateDnsNamespaceOutput, error) {
	return &servicediscovery.CreatePrivateDnsNamespaceOutput{}, f.call("CreatePrivateDnsNamespace", aws.ToString(in.Name))
}

func (f *fakeCloudMap) GetNamespace(_ context.Context, in *servicediscovery.GetNamespaceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.GetNamespaceOutput, error) {
	if err := f.call("GetNamespace", aws.ToString(in.Id)); err != nil {
		return nil, err
	}
	return &servicediscovery.GetNamespaceOutput{Namespace: &sd_types.Namespace{Id: in.Id, Type: f.namespaceType}}, nil
}

func (f *fakeCloudMap) ListServices(_ context.Context, in *servicediscovery.ListServicesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.ListServicesOutput, error) {
	filter := in.Filters[0]
	if err := f.call("ListServices", string(filter.Name)+"="+strings.Join(filter.Values, ",")); err != nil {
		return nil, err
	}
	page := 0
	if in.NextToken != nil {
		page = int(aws.ToString(in.NextToken)[0] - '0')
	}
	output := &servicediscovery.ListServicesOutput{}
	if page < len(f.services) {
		output.Services = f.services[page]
	}
	if page+1 < len(f.services) {
		output.NextToken = aws.String(string(rune('0' + page + 1)))
	}
	return output, nil
}

func (f *fakeCloudMap) CreateService(_ context.Context, in *servicediscovery.CreateServiceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.CreateServiceOutput, error) {
	f.createInputs = append(f.createInputs, in)
	if err := f.call("CreateService", aws.ToString(in.Name)+" in "+aws.ToString(in.NamespaceId)); err != nil {
		return nil, err
	}
	return &servicediscovery.CreateServiceOutput{Service: &sd_types.Service{Arn: aws.String(testNewServiceArn)}}, nil
}

func (f *fakeCloudMap) DeleteService(_ context.Context, in *servicediscovery.DeleteServiceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.DeleteServiceOutput, error) {
	f.calls = append(f.calls, "DeleteService "+aws.ToString(in.Id))
	if len(f.deleteErrs) == 0 {
		return &servicediscovery.DeleteServiceOutput{}, nil
	}
	err := f.deleteErrs[0]
	f.deleteErrs = f.deleteErrs[1:]
	return nil, err
}

// registryCalls are the calls that register a DNS name, as opposed to the
// namespace lookup every create makes.
func (f *fakeCloudMap) registryCalls() []string {
	var calls []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "ListNamespaces") && !strings.HasPrefix(c, "CreatePrivateDnsNamespace") {
			calls = append(calls, c)
		}
	}
	return calls
}

// fakeEcsService stands in for ECS creating and updating a service. service is
// what DescribeServices returns; nil is a service that doesn't exist yet.
type fakeEcsService struct {
	calls        []string
	service      *ecsTypes.Service
	describeErr  error
	createInputs []*ecs.CreateServiceInput
	updateInputs []*ecs.UpdateServiceInput
}

func (f *fakeEcsService) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	f.calls = append(f.calls, "DescribeServices "+strings.Join(in.Services, ","))
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.service == nil {
		return &ecs.DescribeServicesOutput{}, nil
	}
	return &ecs.DescribeServicesOutput{Services: []ecsTypes.Service{*f.service}}, nil
}

func (f *fakeEcsService) CreateService(_ context.Context, in *ecs.CreateServiceInput, _ ...func(*ecs.Options)) (*ecs.CreateServiceOutput, error) {
	f.calls = append(f.calls, "CreateService "+aws.ToString(in.ServiceName))
	f.createInputs = append(f.createInputs, in)
	return &ecs.CreateServiceOutput{Service: &ecsTypes.Service{ServiceArn: aws.String(testEcsServiceArn)}}, nil
}

func (f *fakeEcsService) UpdateService(_ context.Context, in *ecs.UpdateServiceInput, _ ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	f.calls = append(f.calls, "UpdateService "+aws.ToString(in.Service))
	f.updateInputs = append(f.updateInputs, in)
	// The describe after the update finds the service.
	f.describeErr = nil
	if f.service == nil {
		f.service = &ecsTypes.Service{}
	}
	f.service.ServiceArn = aws.String(testEcsServiceArn)
	return &ecs.UpdateServiceOutput{}, nil
}

// liveService is a running service with Service Connect at testAlias in
// namespace, as ECS describes it.
func liveService(namespace string, registries ...string) *ecsTypes.Service {
	service := &ecsTypes.Service{
		ServiceArn: aws.String(testEcsServiceArn),
		Deployments: []ecsTypes.Deployment{{
			ServiceConnectConfiguration: &ecsTypes.ServiceConnectConfiguration{
				Enabled:   true,
				Namespace: aws.String(namespace),
				Services: []ecsTypes.ServiceConnectService{{
					PortName:      aws.String("port-mapping-deployment-1-8080"),
					ClientAliases: []ecsTypes.ServiceConnectClientAlias{{Port: aws.Int32(8080), DnsName: aws.String(testAlias)}},
				}},
			},
		}},
	}
	for _, arn := range registries {
		service.ServiceRegistries = append(service.ServiceRegistries, ecsTypes.ServiceRegistry{RegistryArn: aws.String(arn)})
	}
	return service
}

func privateServiceDeployParameters() map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIDNamespace, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIdFromJob, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "deployment-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentName, "API")
	jobs.SetParameterValue(parameters, parameters_enums.EnvironmentID, "env-0001")
	jobs.SetParameterValue(parameters, parameters_enums.EnvironmentName, "Prod")
	jobs.SetParameterValue(parameters, parameters_enums.VpcID, "vpc-1")
	jobs.SetParameterValue(parameters, parameters_enums.PrivateSubnets, primitive.A{"subnet-a"})
	jobs.SetParameterValue(parameters, parameters_enums.Port, int64(8080))
	return parameters
}

// stubServiceAWS points the service steps at the fakes and makes them not
// wait for the service to settle or queue updates for the server.
func stubServiceAWS(t *testing.T, ecsService *fakeEcsService, cloudMap *fakeCloudMap) {
	t.Helper()
	originalEcs, originalCloudMap, originalWait := ecsServiceClient, getServiceDiscoveryClient, waitForEcsServiceStable
	originalDeployments, originalPreviews := commandUtils.UpdateDeploymentsPipeline, commandUtils.UpdatePreviewsPipeline
	commandUtils.UpdateDeploymentsPipeline, _ = goPipeline.NewPipeline(5, time.Hour, func(string, []deployments.UpdateDeploymentDtoV1) {})
	commandUtils.UpdatePreviewsPipeline, _ = goPipeline.NewPipeline(5, time.Hour, func(string, []previews.UpdatePreviewDtoV1) {})
	t.Cleanup(func() {
		commandUtils.UpdateDeploymentsPipeline.Shutdown()
		commandUtils.UpdatePreviewsPipeline.Shutdown()
		ecsServiceClient, getServiceDiscoveryClient, waitForEcsServiceStable = originalEcs, originalCloudMap, originalWait
		commandUtils.UpdateDeploymentsPipeline, commandUtils.UpdatePreviewsPipeline = originalDeployments, originalPreviews
	})
	ecsServiceClient = func(*ecs.Client) ecsServiceAPI { return ecsService }
	getServiceDiscoveryClient = func(map[string]interface{}) (serviceDiscoveryAPI, error) { return cloudMap, nil }
	waitForEcsServiceStable = func(*ecs.Client, *ecs.DescribeServicesInput) error { return nil }
}

func registryArns(registries []ecsTypes.ServiceRegistry) []string {
	var arns []string
	for _, r := range registries {
		arns = append(arns, aws.ToString(r.RegistryArn))
		if r.ContainerName != nil || r.ContainerPort != nil || r.Port != nil {
			arns = append(arns, "unexpected container name or port")
		}
	}
	return arns
}

func countLines(logs, line string) int {
	return strings.Count(logs, line)
}

func TestCreatePrivateServicePassesTheCreatedRegistry(t *testing.T) {
	ecsService, cloudMap := &fakeEcsService{}, newFakeCloudMap()
	stubServiceAWS(t, ecsService, cloudMap)
	var logs bytes.Buffer
	_, shouldUpdate, err := createEcsServiceIfNeeded(privateServiceDeployParameters(), nil, testClusterArn, "", testTaskDefinitionArn,
		&logs, ecsServiceDeployOptions{registerDnsName: true})
	if err != nil || shouldUpdate {
		t.Fatalf("create: %v, shouldUpdate %v", err, shouldUpdate)
	}
	if len(ecsService.createInputs) != 1 {
		t.Fatalf("CreateService called %d times; want 1", len(ecsService.createInputs))
	}
	in := ecsService.createInputs[0]
	if got := registryArns(in.ServiceRegistries); !reflect.DeepEqual(got, []string{testNewServiceArn}) {
		t.Errorf("ServiceRegistries = %v; want the created %s", got, testNewServiceArn)
	}
	if alias := aws.ToString(in.ServiceConnectConfiguration.Services[0].ClientAliases[0].DnsName); alias != testAlias {
		t.Errorf("Service Connect alias = %q; want %q", alias, testAlias)
	}
	wantCalls := []string{
		"GetNamespace " + testNamespaceID,
		"ListServices NAMESPACE_ID=" + testNamespaceID,
		"CreateService " + testAlias + " in " + testNamespaceID,
	}
	if got := cloudMap.registryCalls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("Cloud Map calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}
	created := cloudMap.createInputs[0]
	if created.DnsConfig.RoutingPolicy != sd_types.RoutingPolicyMultivalue || len(created.DnsConfig.DnsRecords) != 1 ||
		created.DnsConfig.DnsRecords[0].Type != sd_types.RecordTypeA || aws.ToInt64(created.DnsConfig.DnsRecords[0].TTL) != 10 {
		t.Errorf("DnsConfig = %+v; want MULTIVALUE with one A record, TTL 10", created.DnsConfig)
	}
	if aws.ToInt32(created.HealthCheckCustomConfig.FailureThreshold) != 1 {
		t.Errorf("HealthCheckCustomConfig = %+v", created.HealthCheckCustomConfig)
	}
	tags := map[string]string{}
	for _, tag := range created.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if !reflect.DeepEqual(tags, map[string]string{"created by": "deployment.io", "deployment id": "deployment-1"}) {
		t.Errorf("tags = %v", tags)
	}
	if id := aws.ToString(created.CreatorRequestId); !strings.HasPrefix(id, "dns-deployment-1-") {
		t.Errorf("CreatorRequestId = %q; want dns-deployment-1-<unix time>", id)
	}
}

// A retried job finds the Cloud Map service its first attempt created.
func TestExistingDnsServiceIsReused(t *testing.T) {
	ecsService, cloudMap := &fakeEcsService{}, newFakeCloudMap()
	cloudMap.services = [][]sd_types.ServiceSummary{
		{{Name: aws.String("other-0002"), Arn: aws.String("arn:other")}},
		{{Name: aws.String(testAlias), Arn: aws.String(testDnsServiceArn)}},
	}
	stubServiceAWS(t, ecsService, cloudMap)
	_, _, err := createEcsServiceIfNeeded(privateServiceDeployParameters(), nil, testClusterArn, "", testTaskDefinitionArn,
		&bytes.Buffer{}, ecsServiceDeployOptions{registerDnsName: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cloudMap.createInputs) != 0 {
		t.Errorf("created a second Cloud Map service: %v", cloudMap.calls)
	}
	if got := registryArns(ecsService.createInputs[0].ServiceRegistries); !reflect.DeepEqual(got, []string{testDnsServiceArn}) {
		t.Errorf("ServiceRegistries = %v; want the existing %s", got, testDnsServiceArn)
	}
}

func TestUpdatePrivateServiceAddsTheRegistryInTheSameCall(t *testing.T) {
	for name, namespace := range map[string]string{"namespace name": testNamespaceName, "namespace ARN": testNamespaceArn} {
		t.Run(name, func(t *testing.T) {
			ecsService, cloudMap := &fakeEcsService{service: liveService(namespace)}, newFakeCloudMap()
			stubServiceAWS(t, ecsService, cloudMap)
			if err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn,
				&bytes.Buffer{}, ecsServiceDeployOptions{registerDnsName: true}); err != nil {
				t.Fatal(err)
			}
			if len(ecsService.updateInputs) != 1 {
				t.Fatalf("UpdateService called %d times; want 1 (one ECS deployment)", len(ecsService.updateInputs))
			}
			in := ecsService.updateInputs[0]
			if aws.ToString(in.TaskDefinition) != testTaskDefinitionArn {
				t.Errorf("TaskDefinition = %q", aws.ToString(in.TaskDefinition))
			}
			if got := registryArns(in.ServiceRegistries); !reflect.DeepEqual(got, []string{testNewServiceArn}) {
				t.Errorf("ServiceRegistries = %v; want %s", got, testNewServiceArn)
			}
			if len(cloudMap.createInputs) != 1 || aws.ToString(cloudMap.createInputs[0].Name) != testAlias ||
				aws.ToString(cloudMap.createInputs[0].NamespaceId) != testNamespaceID {
				t.Errorf("Cloud Map calls %v; want %s created in %s", cloudMap.calls, testAlias, testNamespaceID)
			}
			if ecsService.calls[0] != "DescribeServices es-deployment-1" || ecsService.calls[1] != "UpdateService es-deployment-1" {
				t.Errorf("ECS calls %v; want the live service described before the update", ecsService.calls)
			}
		})
	}
}

func TestNamespaceLookupFollowsEmptyPages(t *testing.T) {
	cloudMap := newFakeCloudMap()
	cloudMap.emptyNamespacePages = 2
	namespaceID, err := getDnsNamespaceID(cloudMap, testNamespaceName)
	if err != nil {
		t.Fatal(err)
	}
	if namespaceID != testNamespaceID {
		t.Errorf("namespace ID = %q; want %q", namespaceID, testNamespaceID)
	}
	if _, err := getDnsNamespaceID(cloudMap, "missing-0003"); err == nil {
		t.Error("found a namespace that doesn't exist")
	}
}

func TestUpdateLeavesAnExistingRegistryAlone(t *testing.T) {
	ecsService, cloudMap := &fakeEcsService{service: liveService(testNamespaceArn, testDnsServiceArn)}, newFakeCloudMap()
	stubServiceAWS(t, ecsService, cloudMap)
	var logs bytes.Buffer
	if err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn,
		&logs, ecsServiceDeployOptions{registerDnsName: true}); err != nil {
		t.Fatal(err)
	}
	if in := ecsService.updateInputs[0]; in.ServiceRegistries != nil {
		t.Errorf("ServiceRegistries = %v; want none passed", registryArns(in.ServiceRegistries))
	}
	if len(cloudMap.calls) != 0 {
		t.Errorf("Cloud Map calls %v; want none", cloudMap.calls)
	}
	if strings.Contains(logs.String(), "DNS name") {
		t.Errorf("logs:\n%s", logs.String())
	}
}

// Nothing about the DNS name fails the deploy: it goes on without a registry
// and the deploy logs say so once.
func TestDnsNameFailuresDontFailTheDeploy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cloudMap  func(*fakeCloudMap)
		wantInLog string
	}{
		{name: "namespace isn't private DNS", cloudMap: func(f *fakeCloudMap) { f.namespaceType = sd_types.NamespaceTypeHttp },
			wantInLog: "isn't a private DNS namespace"},
		{name: "ListServices fails", cloudMap: func(f *fakeCloudMap) {
			f.errs = map[string]error{"ListServices": errors.New("AccessDeniedException")}
		}, wantInLog: "AccessDeniedException"},
		{name: "CreateService fails", cloudMap: func(f *fakeCloudMap) {
			f.errs = map[string]error{"CreateService": &sd_types.InvalidInput{Message: aws.String("name too long")}}
		}, wantInLog: "name too long"},
		{name: "GetNamespace fails", cloudMap: func(f *fakeCloudMap) {
			f.errs = map[string]error{"GetNamespace": &sd_types.NamespaceNotFound{Message: aws.String("gone")}}
		}, wantInLog: "gone"},
	} {
		t.Run(tt.name+"/create", func(t *testing.T) {
			ecsService, cloudMap := &fakeEcsService{}, newFakeCloudMap()
			tt.cloudMap(cloudMap)
			stubServiceAWS(t, ecsService, cloudMap)
			var logs bytes.Buffer
			_, _, err := createEcsServiceIfNeeded(privateServiceDeployParameters(), nil, testClusterArn, "", testTaskDefinitionArn,
				&logs, ecsServiceDeployOptions{registerDnsName: true})
			assertDeployedWithoutDnsName(t, err, len(ecsService.createInputs), ecsService.createInputs[0].ServiceRegistries, logs.String(), tt.wantInLog)
		})
		t.Run(tt.name+"/update", func(t *testing.T) {
			ecsService, cloudMap := &fakeEcsService{service: liveService(testNamespaceArn)}, newFakeCloudMap()
			tt.cloudMap(cloudMap)
			stubServiceAWS(t, ecsService, cloudMap)
			var logs bytes.Buffer
			err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn,
				&logs, ecsServiceDeployOptions{registerDnsName: true})
			assertDeployedWithoutDnsName(t, err, len(ecsService.updateInputs), ecsService.updateInputs[0].ServiceRegistries, logs.String(), tt.wantInLog)
		})
	}
	t.Run("describing the live service fails", func(t *testing.T) {
		ecsService := &fakeEcsService{service: liveService(testNamespaceArn), describeErr: errors.New("ThrottlingException")}
		stubServiceAWS(t, ecsService, newFakeCloudMap())
		var logs bytes.Buffer
		err := updateEcsService(privateServiceDeployParameters(), nil, testClusterArn, testTaskDefinitionArn,
			&logs, ecsServiceDeployOptions{registerDnsName: true})
		if err != nil || len(ecsService.updateInputs) != 1 || ecsService.updateInputs[0].ServiceRegistries != nil {
			t.Fatalf("err %v, updates %d; want the update without a registry", err, len(ecsService.updateInputs))
		}
		if !strings.Contains(logs.String(), "ThrottlingException. "+testStillReachable) {
			t.Errorf("logs:\n%s", logs.String())
		}
	})
}

func assertDeployedWithoutDnsName(t *testing.T, err error, calls int, registries []ecsTypes.ServiceRegistry, logs, wantInLog string) {
	t.Helper()
	if err != nil {
		t.Fatalf("the deploy failed: %v", err)
	}
	if calls != 1 || registries != nil {
		t.Errorf("%d calls with ServiceRegistries %v; want one without", calls, registryArns(registries))
	}
	if countLines(logs, testNoDnsNameLog) != 1 || countLines(logs, testStillReachable) != 1 || !strings.Contains(logs, wantInLog) {
		t.Errorf("logs:\n%s\nwant one %q line with %q", logs, testNoDnsNameLog+"…"+testStillReachable, wantInLog)
	}
}

// Web services (and the zero options every web-service deploy passes) make no
// Cloud Map registry call and pass no ServiceRegistries.
func TestWebServiceDeploysRegisterNoDnsName(t *testing.T) {
	ecsService, cloudMap := &fakeEcsService{}, newFakeCloudMap()
	stubServiceAWS(t, ecsService, cloudMap)
	parameters := privateServiceDeployParameters()
	if _, _, err := createEcsServiceIfNeeded(parameters, nil, testClusterArn, "arn:tg", testTaskDefinitionArn,
		&bytes.Buffer{}, ecsServiceDeployOptions{}); err != nil {
		t.Fatal(err)
	}
	ecsService.service = liveService(testNamespaceArn)
	if err := updateEcsService(parameters, nil, testClusterArn, testTaskDefinitionArn, &bytes.Buffer{}, ecsServiceDeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := cloudMap.registryCalls(); len(got) != 0 {
		t.Errorf("Cloud Map calls %v; want none", got)
	}
	if ecsService.createInputs[0].ServiceRegistries != nil || ecsService.updateInputs[0].ServiceRegistries != nil {
		t.Errorf("a web service was given a registry")
	}
	// The update is still UpdateService then DescribeServices, as before.
	want := []string{"DescribeServices es-deployment-1", "CreateService es-deployment-1", "UpdateService es-deployment-1", "DescribeServices es-deployment-1"}
	if !reflect.DeepEqual(ecsService.calls, want) {
		t.Errorf("ECS calls %v; want %v", ecsService.calls, want)
	}
}

// DeployAwsPrivateService's Run (live AWS) checks the VPC's DNS attributes
// before deploying with registerDnsName; no web-service deploy does either.
func TestOnlyPrivateServiceDeploysTouchTheVpcAndDnsName(t *testing.T) {
	calls := func(t *testing.T, file string) (map[string]token.Pos, map[string]bool) {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		position, keys := map[string]token.Pos{}, map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if ident, ok := n.Fun.(*ast.Ident); ok {
					if _, seen := position[ident.Name]; !seen {
						position[ident.Name] = n.Pos()
					}
				}
			case *ast.KeyValueExpr:
				if ident, ok := n.Key.(*ast.Ident); ok && ident.Name == "registerDnsName" {
					keys[ident.Name] = true
				}
			}
			return true
		})
		return position, keys
	}
	position, keys := calls(t, "deploy_aws_private_service.go")
	vpc, okVpc := position["ensurePrivateServiceVpcDns"]
	deploy, okDeploy := position["deployEcsServiceAfterPreDeploy"]
	if !okVpc || !okDeploy || vpc >= deploy || !keys["registerDnsName"] {
		t.Errorf("DeployAwsPrivateService must check the VPC, then deploy with registerDnsName")
	}
	for _, file := range []string{"deploy_aws_web_service.go", "deploy_aws_static_site.go"} {
		position, keys := calls(t, file)
		for _, name := range []string{"ensurePrivateServiceVpcDns", "ensureVpcDnsAttributes"} {
			if _, ok := position[name]; ok {
				t.Errorf("%s calls %s", file, name)
			}
		}
		if keys["registerDnsName"] {
			t.Errorf("%s sets registerDnsName", file)
		}
	}
}

// fakeVpcAttributes stands in for EC2's VPC attribute calls.
type fakeVpcAttributes struct {
	on           map[ec2Types.VpcAttributeName]bool
	describeErr  map[ec2Types.VpcAttributeName]error
	modifyErr    error
	modifyInputs []*ec2.ModifyVpcAttributeInput
}

func (f *fakeVpcAttributes) DescribeVpcAttribute(_ context.Context, in *ec2.DescribeVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcAttributeOutput, error) {
	if err := f.describeErr[in.Attribute]; err != nil {
		return nil, err
	}
	value := &ec2Types.AttributeBooleanValue{Value: aws.Bool(f.on[in.Attribute])}
	output := &ec2.DescribeVpcAttributeOutput{VpcId: in.VpcId}
	if in.Attribute == ec2Types.VpcAttributeNameEnableDnsHostnames {
		output.EnableDnsHostnames = value
	} else {
		output.EnableDnsSupport = value
	}
	return output, nil
}

func (f *fakeVpcAttributes) ModifyVpcAttribute(_ context.Context, in *ec2.ModifyVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error) {
	f.modifyInputs = append(f.modifyInputs, in)
	return &ec2.ModifyVpcAttributeOutput{}, f.modifyErr
}

func modifiedAttributes(inputs []*ec2.ModifyVpcAttributeInput) []string {
	var modified []string
	for _, in := range inputs {
		if in.EnableDnsSupport != nil && in.EnableDnsHostnames != nil {
			modified = append(modified, "both in one call")
		}
		if in.EnableDnsSupport != nil && aws.ToBool(in.EnableDnsSupport.Value) {
			modified = append(modified, "enableDnsSupport "+aws.ToString(in.VpcId))
		}
		if in.EnableDnsHostnames != nil && aws.ToBool(in.EnableDnsHostnames.Value) {
			modified = append(modified, "enableDnsHostnames "+aws.ToString(in.VpcId))
		}
	}
	return modified
}

func TestVpcDnsAttributesModifiedOnlyWhenOff(t *testing.T) {
	support, hostnames := ec2Types.VpcAttributeNameEnableDnsSupport, ec2Types.VpcAttributeNameEnableDnsHostnames
	for _, tt := range []struct {
		name string
		on   map[ec2Types.VpcAttributeName]bool
		want []string
	}{
		{name: "both on", on: map[ec2Types.VpcAttributeName]bool{support: true, hostnames: true}},
		{name: "hostnames off (a runner-created VPC)", on: map[ec2Types.VpcAttributeName]bool{support: true},
			want: []string{"enableDnsHostnames vpc-1"}},
		{name: "both off", on: map[ec2Types.VpcAttributeName]bool{},
			want: []string{"enableDnsSupport vpc-1", "enableDnsHostnames vpc-1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ec2Client := &fakeVpcAttributes{on: tt.on}
			var logs bytes.Buffer
			ensurePrivateServiceVpcDns(privateServiceDeployParameters(), ec2Client, &logs)
			if got := modifiedAttributes(ec2Client.modifyInputs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("modified %v; want %v", got, tt.want)
			}
			if n := countLines(logs.String(), "Turned on "); n != len(tt.want) {
				t.Errorf("logs:\n%s\nwant %d change lines", logs.String(), len(tt.want))
			}
			if len(tt.want) > 0 && !strings.Contains(logs.String(),
				"Turned on enableDnsHostnames for VPC vpc-1, so private services resolve by name inside it (takes a few minutes to apply).\n") {
				t.Errorf("logs:\n%s", logs.String())
			}
		})
	}
}

// ensurePrivateServiceVpcDns returns nothing, so it can't fail the deploy;
// what goes wrong is logged and the other attribute is still checked.
func TestVpcDnsAttributeErrorsAreLogged(t *testing.T) {
	ec2Client := &fakeVpcAttributes{
		on:          map[ec2Types.VpcAttributeName]bool{},
		describeErr: map[ec2Types.VpcAttributeName]error{ec2Types.VpcAttributeNameEnableDnsSupport: errors.New("UnauthorizedOperation")},
		modifyErr:   errors.New("InvalidVpcID.NotFound"),
	}
	var logs bytes.Buffer
	ensurePrivateServiceVpcDns(privateServiceDeployParameters(), ec2Client, &logs)
	if !strings.Contains(logs.String(), "UnauthorizedOperation") || !strings.Contains(logs.String(), "InvalidVpcID.NotFound") ||
		strings.Contains(logs.String(), "Turned on") {
		t.Errorf("logs:\n%s", logs.String())
	}
	if got := modifiedAttributes(ec2Client.modifyInputs); !reflect.DeepEqual(got, []string{"enableDnsHostnames vpc-1"}) {
		t.Errorf("modified %v", got)
	}

	// No VPC on the deployment: logged, nothing called.
	parameters := privateServiceDeployParameters()
	delete(parameters, mustKey(t, parameters_enums.VpcID))
	ec2Client = &fakeVpcAttributes{}
	logs.Reset()
	ensurePrivateServiceVpcDns(parameters, ec2Client, &logs)
	if len(ec2Client.modifyInputs) != 0 || !strings.Contains(logs.String(), "no VPC") {
		t.Errorf("logs:\n%s", logs.String())
	}
}

// fakePrivateServiceDelete stands in for ECS deleting a private service: the
// service is ACTIVE with registries until DeleteService, INACTIVE after.
type fakePrivateServiceDelete struct {
	calls       []string
	registries  []string
	describeErr error
	notFound    bool
	deleteErr   error
	deleted     bool
}

func (f *fakePrivateServiceDelete) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	f.calls = append(f.calls, "ecs DescribeServices "+strings.Join(in.Services, ","))
	if f.deleted {
		return &ecs.DescribeServicesOutput{Services: []ecsTypes.Service{{Status: aws.String("INACTIVE")}}}, nil
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.notFound {
		return &ecs.DescribeServicesOutput{Failures: []ecsTypes.Failure{{Reason: aws.String("MISSING")}}}, nil
	}
	service := liveService(testNamespaceArn, f.registries...)
	service.Status = aws.String("ACTIVE")
	return &ecs.DescribeServicesOutput{Services: []ecsTypes.Service{*service}}, nil
}

func (f *fakePrivateServiceDelete) DeleteService(_ context.Context, in *ecs.DeleteServiceInput, _ ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error) {
	f.calls = append(f.calls, "ecs DeleteService "+aws.ToString(in.Service))
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = true
	return &ecs.DeleteServiceOutput{}, nil
}

func runPrivateServiceDelete(t *testing.T, ecsClient *fakePrivateServiceDelete, cloudMap *fakeCloudMap) (string, []string, error) {
	t.Helper()
	originalCloudMap, originalSleep := getServiceDiscoveryClient, deleteDnsServiceSleep
	t.Cleanup(func() { getServiceDiscoveryClient, deleteDnsServiceSleep = originalCloudMap, originalSleep })
	var calls []string
	getServiceDiscoveryClient = func(map[string]interface{}) (serviceDiscoveryAPI, error) { return cloudMap, nil }
	deleteDnsServiceSleep = func(d time.Duration) { calls = append(calls, "sleep "+d.String()) }
	var logs bytes.Buffer
	err := deletePrivateServiceEcsService(privateServiceDeployParameters(), ecsClient, testClusterArn, &logs)
	// The ECS calls, then Cloud Map's, then the pauses between retries.
	all := append(append([]string{}, ecsClient.calls...), cloudMap.calls...)
	return logs.String(), append(all, calls...), err
}

func TestDeletePrivateServiceDeletesItsDnsServiceAfterTheServiceIsInactive(t *testing.T) {
	ecsClient := &fakePrivateServiceDelete{registries: []string{testDnsServiceArn}}
	cloudMap := newFakeCloudMap()
	var order []string
	ecsWrapped := &orderRecorder{fakePrivateServiceDelete: ecsClient, order: &order}
	cloudMapWrapped := &orderedCloudMap{fakeCloudMap: cloudMap, order: &order}
	originalCloudMap := getServiceDiscoveryClient
	t.Cleanup(func() { getServiceDiscoveryClient = originalCloudMap })
	getServiceDiscoveryClient = func(map[string]interface{}) (serviceDiscoveryAPI, error) { return cloudMapWrapped, nil }
	if err := deletePrivateServiceEcsService(privateServiceDeployParameters(), ecsWrapped, testClusterArn, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ecs DescribeServices es-deployment-1 ACTIVE",
		"ecs DeleteService es-deployment-1",
		"ecs DescribeServices es-deployment-1 INACTIVE",
		"cloudmap DeleteService srv-api",
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
}

// orderRecorder and orderedCloudMap record ECS and Cloud Map calls in one list.
type orderRecorder struct {
	*fakePrivateServiceDelete
	order *[]string
}

func (o *orderRecorder) DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	out, err := o.fakePrivateServiceDelete.DescribeServices(ctx, in, optFns...)
	status := ""
	if err == nil && len(out.Services) > 0 {
		status = " " + aws.ToString(out.Services[0].Status)
	}
	*o.order = append(*o.order, "ecs DescribeServices "+strings.Join(in.Services, ",")+status)
	return out, err
}

func (o *orderRecorder) DeleteService(ctx context.Context, in *ecs.DeleteServiceInput, optFns ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error) {
	*o.order = append(*o.order, "ecs DeleteService "+aws.ToString(in.Service))
	return o.fakePrivateServiceDelete.DeleteService(ctx, in, optFns...)
}

type orderedCloudMap struct {
	*fakeCloudMap
	order *[]string
}

func (o *orderedCloudMap) DeleteService(ctx context.Context, in *servicediscovery.DeleteServiceInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.DeleteServiceOutput, error) {
	*o.order = append(*o.order, "cloudmap DeleteService "+aws.ToString(in.Id))
	return o.fakeCloudMap.DeleteService(ctx, in, optFns...)
}

func TestDeletePrivateServiceDnsCleanup(t *testing.T) {
	inUse := &sd_types.ResourceInUse{Message: aws.String("instances still registered")}
	for _, tt := range []struct {
		name        string
		deleteErrs  []error
		wantDeletes int
		wantSleeps  int
		wantInLog   string
	}{
		{name: "deleted at once", wantDeletes: 1},
		{name: "in use, then deleted", deleteErrs: []error{inUse, inUse}, wantDeletes: 3, wantSleeps: 2},
		{name: "already gone", deleteErrs: []error{&sd_types.ServiceNotFound{Message: aws.String("gone")}}, wantDeletes: 1,
			wantInLog: "was already gone"},
		{name: "still in use after 2 minutes", deleteErrs: []error{inUse, inUse, inUse, inUse, inUse, inUse, inUse, inUse, inUse,
			inUse, inUse, inUse, inUse, inUse}, wantDeletes: 13, wantSleeps: 12, wantInLog: "instances still registered"},
		{name: "other error", deleteErrs: []error{errors.New("AccessDeniedException")}, wantDeletes: 1, wantInLog: "AccessDeniedException"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cloudMap := newFakeCloudMap()
			cloudMap.deleteErrs = tt.deleteErrs
			logs, calls, err := runPrivateServiceDelete(t, &fakePrivateServiceDelete{registries: []string{testDnsServiceArn}}, cloudMap)
			if err != nil {
				t.Fatalf("the delete failed: %v", err)
			}
			deletes, sleeps := 0, 0
			for _, c := range calls {
				if c == "DeleteService srv-api" {
					deletes++
				}
				if c == "sleep 10s" {
					sleeps++
				}
			}
			if deletes != tt.wantDeletes || sleeps != tt.wantSleeps {
				t.Errorf("%d deletes, %d sleeps; want %d, %d: %v", deletes, sleeps, tt.wantDeletes, tt.wantSleeps, calls)
			}
			if !strings.Contains(logs, tt.wantInLog) {
				t.Errorf("logs:\n%s\nwant %q", logs, tt.wantInLog)
			}
		})
	}
}

// A service without a registry, or one that can't be described, is deleted
// exactly as before: no Cloud Map call.
func TestDeletePrivateServiceWithoutRegistry(t *testing.T) {
	for name, ecsClient := range map[string]*fakePrivateServiceDelete{
		"no registry":       {},
		"describe fails":    {registries: []string{testDnsServiceArn}, describeErr: errors.New("ThrottlingException")},
		"service not found": {registries: []string{testDnsServiceArn}, notFound: true},
	} {
		t.Run(name, func(t *testing.T) {
			cloudMap := newFakeCloudMap()
			_, _, err := runPrivateServiceDelete(t, ecsClient, cloudMap)
			if err != nil || len(cloudMap.calls) != 0 || !ecsClient.deleted {
				t.Errorf("err %v, Cloud Map calls %v, deleted %v", err, cloudMap.calls, ecsClient.deleted)
			}
		})
	}
	// ECS's own DeleteService error still fails the delete, before any Cloud Map call.
	ecsClient := &fakePrivateServiceDelete{registries: []string{testDnsServiceArn}, deleteErr: errors.New("ClusterNotFoundException")}
	cloudMap := newFakeCloudMap()
	if _, _, err := runPrivateServiceDelete(t, ecsClient, cloudMap); err == nil || len(cloudMap.calls) != 0 {
		t.Errorf("err %v, Cloud Map calls %v", err, cloudMap.calls)
	}
}
