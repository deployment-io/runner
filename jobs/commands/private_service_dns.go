package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sd_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// A private service is reached over Service Connect at its client alias, which
// resolves only inside ECS services that are part of Service Connect. So that
// one-off tasks (the pre-deploy command, RunTask) can reach it too, the alias
// is also registered as a Cloud Map DNS name in the environment's private DNS
// namespace: <alias>.<namespace> resolves through the VPC's DNS from any task.
// None of this ever fails a deploy: without the DNS name the service is still
// reachable over Service Connect.

// serviceDiscoveryAPI is the part of *servicediscovery.Client deploys and
// deletes use.
type serviceDiscoveryAPI interface {
	ListNamespaces(ctx context.Context, params *servicediscovery.ListNamespacesInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.ListNamespacesOutput, error)
	CreatePrivateDnsNamespace(ctx context.Context, params *servicediscovery.CreatePrivateDnsNamespaceInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.CreatePrivateDnsNamespaceOutput, error)
	GetNamespace(ctx context.Context, params *servicediscovery.GetNamespaceInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.GetNamespaceOutput, error)
	ListServices(ctx context.Context, params *servicediscovery.ListServicesInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.ListServicesOutput, error)
	CreateService(ctx context.Context, params *servicediscovery.CreateServiceInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.CreateServiceOutput, error)
	DeleteService(ctx context.Context, params *servicediscovery.DeleteServiceInput, optFns ...func(*servicediscovery.Options)) (*servicediscovery.DeleteServiceOutput, error)
}

// ecsServiceAPI is the part of *ecs.Client creating and updating a service uses.
type ecsServiceAPI interface {
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	CreateService(ctx context.Context, params *ecs.CreateServiceInput, optFns ...func(*ecs.Options)) (*ecs.CreateServiceOutput, error)
	UpdateService(ctx context.Context, params *ecs.UpdateServiceInput, optFns ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
}

// vpcDnsAttributesAPI is the part of *ec2.Client checking a VPC's DNS attributes uses.
type vpcDnsAttributesAPI interface {
	DescribeVpcAttribute(ctx context.Context, params *ec2.DescribeVpcAttributeInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVpcAttributeOutput, error)
	ModifyVpcAttribute(ctx context.Context, params *ec2.ModifyVpcAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error)
}

// Swapped by tests so the service and Cloud Map steps run without AWS.
var (
	getServiceDiscoveryClient = func(parameters map[string]interface{}) (serviceDiscoveryAPI, error) {
		client, err := cloud_api_clients.GetServiceDiscoveryClient(parameters)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	ecsServiceClient = func(ecsClient *ecs.Client) ecsServiceAPI { return ecsClient }
	// Deleting a Cloud Map service is refused while ECS is still
	// deregistering its instances: retried every pause, for up to the timeout.
	deleteDnsServicePause   = 10 * time.Second
	deleteDnsServiceTimeout = 2 * time.Minute
	deleteDnsServiceSleep   = time.Sleep
)

func logNoDnsName(logsWriter io.Writer, alias string, reason error) {
	io.WriteString(logsWriter, fmt.Sprintf("Couldn't give %s a DNS name: %s. It's still reachable from other services over Service Connect.\n",
		alias, strings.TrimSuffix(reason.Error(), ".")))
}

// lastArnSegment is the ID at the end of a Cloud Map ARN
// (arn:aws:servicediscovery:<region>:<account>:namespace/ns-…, …:service/srv-…).
func lastArnSegment(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

// dnsServiceRegistries is the ServiceRegistries for an ECS service whose
// Service Connect client alias is alias in namespace (a name or an ARN): the
// Cloud Map service of that name, found or created. On any error it writes
// one line to the deploy logs and returns nil, so the deploy goes on without.
func dnsServiceRegistries(parameters map[string]interface{}, namespace, alias string, logsWriter io.Writer) []ecsTypes.ServiceRegistry {
	registryArn, err := findOrCreateDnsService(parameters, namespace, alias)
	if err != nil {
		logNoDnsName(logsWriter, alias, err)
		return nil
	}
	io.WriteString(logsWriter, fmt.Sprintf("Registering DNS name %s in namespace %s\n", alias, namespace))
	return []ecsTypes.ServiceRegistry{{RegistryArn: aws.String(registryArn)}}
}

func findOrCreateDnsService(parameters map[string]interface{}, namespace, alias string) (string, error) {
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	client, err := getServiceDiscoveryClient(parameters)
	if err != nil {
		return "", err
	}
	namespaceID, err := getDnsNamespaceID(client, namespace)
	if err != nil {
		return "", err
	}
	// A retried job finds the service its earlier attempt created.
	registryArn, err := findDnsService(client, namespaceID, alias)
	if err != nil || len(registryArn) > 0 {
		return registryArn, err
	}
	return createDnsService(client, namespaceID, alias, deploymentID)
}

// findDnsService is the ARN of the namespace's Cloud Map service named alias,
// or "" if there is none.
func findDnsService(client serviceDiscoveryAPI, namespaceID, alias string) (string, error) {
	listServicesInput := &servicediscovery.ListServicesInput{
		Filters: []sd_types.ServiceFilter{{
			Name:      sd_types.ServiceFilterNameNamespaceId,
			Values:    []string{namespaceID},
			Condition: sd_types.FilterConditionEq,
		}},
	}
	for {
		listServicesOutput, err := client.ListServices(context.TODO(), listServicesInput)
		if err != nil {
			return "", err
		}
		for _, service := range listServicesOutput.Services {
			if aws.ToString(service.Name) == alias {
				return aws.ToString(service.Arn), nil
			}
		}
		if len(aws.ToString(listServicesOutput.NextToken)) == 0 {
			return "", nil
		}
		listServicesInput.NextToken = listServicesOutput.NextToken
	}
}

func createDnsService(client serviceDiscoveryAPI, namespaceID, alias, deploymentID string) (string, error) {
	// Never a fixed request ID: Cloud Map refuses a reused one after its
	// service was deleted.
	createServiceOutput, err := client.CreateService(context.TODO(), &servicediscovery.CreateServiceInput{
		Name:             aws.String(alias),
		NamespaceId:      aws.String(namespaceID),
		CreatorRequestId: aws.String(fmt.Sprintf("dns-%s-%d", deploymentID, time.Now().Unix())),
		DnsConfig: &sd_types.DnsConfig{
			RoutingPolicy: sd_types.RoutingPolicyMultivalue,
			DnsRecords: []sd_types.DnsRecord{{
				Type: sd_types.RecordTypeA,
				TTL:  aws.Int64(10),
			}},
		},
		HealthCheckCustomConfig: &sd_types.HealthCheckCustomConfig{FailureThreshold: aws.Int32(1)},
		Tags: []sd_types.Tag{
			{Key: aws.String("created by"), Value: aws.String("deployment.io")},
			{Key: aws.String("deployment id"), Value: aws.String(deploymentID)},
		},
	})
	if err != nil {
		return "", err
	}
	if createServiceOutput.Service == nil {
		return "", fmt.Errorf("Cloud Map created no service")
	}
	return aws.ToString(createServiceOutput.Service.Arn), nil
}

// getDnsNamespaceID is the ID of namespace, a name or an ARN, and fails unless
// it is a private DNS namespace.
func getDnsNamespaceID(client serviceDiscoveryAPI, namespace string) (string, error) {
	var namespaceID string
	if strings.HasPrefix(namespace, "arn:") {
		namespaceID = lastArnSegment(namespace)
	} else {
		listNamespacesInput := &servicediscovery.ListNamespacesInput{
			Filters: []sd_types.NamespaceFilter{{
				Name:      sd_types.NamespaceFilterNameName,
				Values:    []string{namespace},
				Condition: sd_types.FilterConditionEq,
			}},
		}
		// Cloud Map filters each page, so an empty page can come before the match.
		for len(namespaceID) == 0 {
			listNamespacesOutput, err := client.ListNamespaces(context.TODO(), listNamespacesInput)
			if err != nil {
				return "", err
			}
			if len(listNamespacesOutput.Namespaces) > 0 {
				namespaceID = aws.ToString(listNamespacesOutput.Namespaces[0].Id)
			} else if len(aws.ToString(listNamespacesOutput.NextToken)) == 0 {
				return "", fmt.Errorf("namespace %s wasn't found", namespace)
			} else {
				listNamespacesInput.NextToken = listNamespacesOutput.NextToken
			}
		}
	}
	getNamespaceOutput, err := client.GetNamespace(context.TODO(), &servicediscovery.GetNamespaceInput{Id: aws.String(namespaceID)})
	if err != nil {
		return "", err
	}
	if getNamespaceOutput.Namespace == nil || getNamespaceOutput.Namespace.Type != sd_types.NamespaceTypeDnsPrivate {
		return "", fmt.Errorf("namespace %s isn't a private DNS namespace", namespace)
	}
	return namespaceID, nil
}

// missingDnsServiceRegistries is what to add to an existing service's
// UpdateService: nil when it already has a registry, else the registry for its
// live Service Connect alias and namespace. Never an error: what goes wrong is
// a line in the deploy logs.
func missingDnsServiceRegistries(parameters map[string]interface{}, ecsClient ecsServiceAPI, describeServicesInput *ecs.DescribeServicesInput,
	logsWriter io.Writer) []ecsTypes.ServiceRegistry {
	ecsServiceName := describeServicesInput.Services[0]
	describeServicesOutput, err := ecsClient.DescribeServices(context.TODO(), describeServicesInput)
	if err != nil {
		logNoDnsName(logsWriter, ecsServiceName, err)
		return nil
	}
	if len(describeServicesOutput.Services) == 0 {
		logNoDnsName(logsWriter, ecsServiceName, fmt.Errorf("the ECS service wasn't found"))
		return nil
	}
	service := describeServicesOutput.Services[0]
	if len(service.ServiceRegistries) > 0 {
		return nil
	}
	namespace, alias := liveServiceConnectAlias(service)
	if len(namespace) == 0 || len(alias) == 0 {
		logNoDnsName(logsWriter, ecsServiceName, fmt.Errorf("its Service Connect alias wasn't found"))
		return nil
	}
	return dnsServiceRegistries(parameters, namespace, alias, logsWriter)
}

// liveServiceConnectAlias is the namespace (a name or an ARN) and first client
// alias of the service's current Service Connect configuration.
func liveServiceConnectAlias(service ecsTypes.Service) (namespace, alias string) {
	if len(service.Deployments) == 0 {
		return "", ""
	}
	config := service.Deployments[0].ServiceConnectConfiguration
	if config == nil || len(config.Services) == 0 || len(config.Services[0].ClientAliases) == 0 {
		return "", ""
	}
	return aws.ToString(config.Namespace), aws.ToString(config.Services[0].ClientAliases[0].DnsName)
}

// ensurePrivateServiceVpcDns turns on the DNS attributes of the deployment's
// VPC, without which its private DNS names don't resolve. It never fails the
// deploy.
func ensurePrivateServiceVpcDns(parameters map[string]interface{}, ec2Client vpcDnsAttributesAPI, logsWriter io.Writer) {
	vpcID, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcID)
	if err != nil || len(vpcID) == 0 {
		io.WriteString(logsWriter, "Couldn't check the VPC's DNS settings: the deployment has no VPC.\n")
		return
	}
	ensureVpcDnsAttributes(ec2Client, vpcID, logsWriter)
}

// ensureVpcDnsAttributes turns on enableDnsSupport and enableDnsHostnames for
// the VPC, each only if it is off. Errors are logged, not returned.
func ensureVpcDnsAttributes(ec2Client vpcDnsAttributesAPI, vpcID string, logsWriter io.Writer) {
	for _, attribute := range []ec2Types.VpcAttributeName{ec2Types.VpcAttributeNameEnableDnsSupport, ec2Types.VpcAttributeNameEnableDnsHostnames} {
		describeVpcAttributeOutput, err := ec2Client.DescribeVpcAttribute(context.TODO(), &ec2.DescribeVpcAttributeInput{
			Attribute: attribute,
			VpcId:     aws.String(vpcID),
		})
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Couldn't check %s for VPC %s: %s\n", attribute, vpcID, err))
			continue
		}
		value := describeVpcAttributeOutput.EnableDnsSupport
		if attribute == ec2Types.VpcAttributeNameEnableDnsHostnames {
			value = describeVpcAttributeOutput.EnableDnsHostnames
		}
		if value != nil && aws.ToBool(value.Value) {
			continue
		}
		// ModifyVpcAttribute takes one attribute per call.
		modifyVpcAttributeInput := &ec2.ModifyVpcAttributeInput{VpcId: aws.String(vpcID)}
		on := &ec2Types.AttributeBooleanValue{Value: aws.Bool(true)}
		if attribute == ec2Types.VpcAttributeNameEnableDnsHostnames {
			modifyVpcAttributeInput.EnableDnsHostnames = on
		} else {
			modifyVpcAttributeInput.EnableDnsSupport = on
		}
		if _, err = ec2Client.ModifyVpcAttribute(context.TODO(), modifyVpcAttributeInput); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Couldn't turn on %s for VPC %s: %s\n", attribute, vpcID, err))
			continue
		}
		io.WriteString(logsWriter, fmt.Sprintf("Turned on %s for VPC %s, so private services resolve by name inside it (takes a few minutes to apply).\n",
			attribute, vpcID))
	}
}

// liveServiceRegistryArns is the registries of the ECS service, or nil if it
// can't be described or isn't found.
func liveServiceRegistryArns(ecsClient ecs.DescribeServicesAPIClient, clusterArn, ecsServiceName string) []string {
	describeServicesOutput, err := ecsClient.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
		Services: []string{ecsServiceName},
		Cluster:  aws.String(clusterArn),
	})
	if err != nil || len(describeServicesOutput.Services) == 0 {
		return nil
	}
	var registryArns []string
	for _, registry := range describeServicesOutput.Services[0].ServiceRegistries {
		if arn := aws.ToString(registry.RegistryArn); len(arn) > 0 {
			registryArns = append(registryArns, arn)
		}
	}
	return registryArns
}

// deleteDnsServices deletes the Cloud Map services of a deleted ECS service.
// It runs once the ECS service is inactive: Cloud Map refuses to delete a
// service with registered instances, and ECS deregisters them as the service
// goes. It never fails the delete: a Cloud Map service left behind costs
// nothing and affects nothing.
func deleteDnsServices(parameters map[string]interface{}, registryArns []string, logsWriter io.Writer) {
	if len(registryArns) == 0 {
		return
	}
	client, err := getServiceDiscoveryClient(parameters)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Couldn't delete the service's DNS name: %s\n", err))
		return
	}
	for _, registryArn := range registryArns {
		deleteDnsService(client, lastArnSegment(registryArn), logsWriter)
	}
}

func deleteDnsService(client serviceDiscoveryAPI, serviceID string, logsWriter io.Writer) {
	io.WriteString(logsWriter, fmt.Sprintf("Deleting Cloud Map service: %s\n", serviceID))
	retries := int(deleteDnsServiceTimeout / deleteDnsServicePause)
	for attempt := 0; ; attempt++ {
		_, err := client.DeleteService(context.TODO(), &servicediscovery.DeleteServiceInput{Id: aws.String(serviceID)})
		if err == nil {
			return
		}
		var serviceNotFound *sd_types.ServiceNotFound
		if errors.As(err, &serviceNotFound) {
			logAlreadyGone(logsWriter, "Cloud Map service", serviceID)
			return
		}
		var resourceInUse *sd_types.ResourceInUse
		if errors.As(err, &resourceInUse) && attempt < retries {
			deleteDnsServiceSleep(deleteDnsServicePause)
			continue
		}
		io.WriteString(logsWriter, fmt.Sprintf("Couldn't delete Cloud Map service %s: %s. It's left behind and doesn't affect anything.\n",
			serviceID, err))
		return
	}
}
