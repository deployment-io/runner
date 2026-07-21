package aws_utils

// web_service.go holds the arg-based AWS primitives that back the in-process
// web-service preview deploy (agenttools.DeployWebServicePreview). It is the
// web-service twin of static_site.go: every helper takes explicit arguments +
// an already-built AWS client, with NO coupling to the job `parameters` map, so
// the preview deploy core can reuse them the way the static preview reuses the
// S3/CloudFront helpers.
//
// These are written FRESH against the AWS SDK (referencing, not refactoring, the
// production monolith jobs/commands/deploy_aws_web_service.go). The production
// path is per-service (one ALB per deployment, private subnets + NAT + Service
// Connect); the preview is deliberately leaner and self-contained:
//
//   - Fargate tasks run in the account's DEFAULT VPC public subnets with a public
//     IP (AssignPublicIp=ENABLED) so they pull the ECR image + reach the internet
//     with NO NAT gateway (a NAT's idle charge is exactly what a cost-optimized
//     preview avoids).
//   - One shared per-org task security group (ingress from the VPC CIDR) rather
//     than a per-service SG + default-SG mutation.
//   - No Cloud Map / Service Connect (the preview is reached through the shared
//     ALB + CloudFront, not service-to-service internal DNS).
//
// Cw1a covers the ECS half (ECR, cluster, execution role, VPC/subnets, task SG,
// task definition, target group, service). Cw1b appends the ingress half (shared
// ALB + header-routed listener rule).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamTypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
)

// awsCreatedByTag is the marker every production resource carries; previews reuse
// it so an operator (and the GC sweep) can recognize deployment.io-owned infra.
const awsCreatedByTag = "deployment.io"

// apiErrCodeIs reports whether err is (or wraps) an AWS API error with one of the
// given codes — used to make create/authorize calls idempotent (a concurrent or
// retried first-deploy hits "already exists" instead of a real failure).
func apiErrCodeIs(err error, codes ...string) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.ErrorCode() == c {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// ECR
// ---------------------------------------------------------------------------

// EnsureEcrRepository find-or-creates an ECR repository by name and returns its
// URI. Mirrors the monolith's createEcrRepositoryIfNeeded but arg-based and
// without the control-plane persistence side effects.
func EnsureEcrRepository(ecrClient *ecr.Client, repositoryName string, logsWriter io.Writer) (string, error) {
	describeOut, _ := ecrClient.DescribeRepositories(context.TODO(), &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{repositoryName},
	})
	if describeOut != nil {
		for _, repo := range describeOut.Repositories {
			if aws.ToString(repo.RepositoryName) == repositoryName {
				return aws.ToString(repo.RepositoryUri), nil
			}
		}
	}

	createOut, err := ecrClient.CreateRepository(context.TODO(), &ecr.CreateRepositoryInput{
		RepositoryName:     aws.String(repositoryName),
		ImageTagMutability: ecrTypes.ImageTagMutabilityMutable,
		Tags: []ecrTypes.Tag{
			{Key: aws.String("Name"), Value: aws.String(repositoryName)},
			{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)},
		},
	})
	if err != nil {
		// A concurrent first-deploy may have won the race — re-describe.
		if apiErrCodeIs(err, "RepositoryAlreadyExistsException") {
			describeOut, derr := ecrClient.DescribeRepositories(context.TODO(), &ecr.DescribeRepositoriesInput{
				RepositoryNames: []string{repositoryName},
			})
			if derr == nil && describeOut != nil && len(describeOut.Repositories) > 0 {
				return aws.ToString(describeOut.Repositories[0].RepositoryUri), nil
			}
		}
		return "", err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Created ECR repository: %s\n", aws.ToString(createOut.Repository.RepositoryUri)))
	return aws.ToString(createOut.Repository.RepositoryUri), nil
}

// EcrDockerAuth returns a base64-encoded docker registry auth string for the
// account's ECR registry (the form docker's ImagePush RegistryAuth expects).
// Mirrors the monolith's GetAuthorizationToken -> "AWS:<token>" decoding.
func EcrDockerAuth(ecrClient *ecr.Client) (string, error) {
	out, err := ecrClient.GetAuthorizationToken(context.TODO(), &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", err
	}
	if len(out.AuthorizationData) < 1 {
		return "", fmt.Errorf("no auth token from ECR")
	}
	decoded, err := base64.StdEncoding.DecodeString(aws.ToString(out.AuthorizationData[0].AuthorizationToken))
	if err != nil {
		return "", err
	}
	// The decoded token is "AWS:<password>".
	full := string(decoded)
	pw := full
	for i := 0; i < len(full); i++ {
		if full[i] == ':' {
			pw = full[i+1:]
			break
		}
	}
	authJSON, err := json.Marshal(map[string]string{"username": "AWS", "password": pw})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(authJSON), nil
}

// ---------------------------------------------------------------------------
// ECS cluster + task execution role
// ---------------------------------------------------------------------------

// EnsureEcsCluster find-or-creates a FARGATE ECS cluster by name and returns its
// ARN. Mirrors create_ecs_cluster.go's createEcsClusterIfNeeded (an INACTIVE
// cluster of the same name is treated as absent and recreated).
func EnsureEcsCluster(ecsClient *ecs.Client, clusterName string) (string, error) {
	describeOut, err := ecsClient.DescribeClusters(context.TODO(), &ecs.DescribeClustersInput{
		Clusters: []string{clusterName},
	})
	if err == nil && describeOut != nil {
		for _, c := range describeOut.Clusters {
			if aws.ToString(c.ClusterName) == clusterName && aws.ToString(c.Status) != "INACTIVE" {
				return aws.ToString(c.ClusterArn), nil
			}
		}
	}

	createOut, err := ecsClient.CreateCluster(context.TODO(), &ecs.CreateClusterInput{
		ClusterName:       aws.String(clusterName),
		CapacityProviders: []string{"FARGATE"},
		Tags:              []ecsTypes.Tag{{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)}},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(createOut.Cluster.ClusterArn), nil
}

// ecsTaskExecutionAssumeRolePolicy lets ECS tasks assume the execution role.
const ecsTaskExecutionAssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Principal": {"Service": "ecs-tasks.amazonaws.com"}, "Action": "sts:AssumeRole"}]
}`

// EnsureEcsTaskExecutionRole find-or-creates an ECS task execution role and
// returns its ARN. The role lets the task pull from ECR and ship logs to
// CloudWatch; CloudWatchFullAccess is attached in addition to the managed ECS
// execution policy because the awslogs driver is configured with
// awslogs-create-group=true (which needs logs:CreateLogGroup). IAM is global, so
// a per-org role is reused across regions/previews.
func EnsureEcsTaskExecutionRole(iamClient *iam.Client, roleName string) (string, error) {
	getOut, err := iamClient.GetRole(context.TODO(), &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err == nil && getOut.Role != nil {
		return aws.ToString(getOut.Role.Arn), nil
	}
	if err != nil && !apiErrCodeIs(err, "NoSuchEntity", "NoSuchEntityException") {
		return "", err
	}

	createOut, err := iamClient.CreateRole(context.TODO(), &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(ecsTaskExecutionAssumeRolePolicy),
		Description:              aws.String("ECS task execution role for deployment.io previews"),
		Tags:                     []iamTypes.Tag{{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)}},
	})
	if err != nil {
		// Lost the create race — fetch the existing role.
		if apiErrCodeIs(err, "EntityAlreadyExists", "EntityAlreadyExistsException") {
			getOut, gerr := iamClient.GetRole(context.TODO(), &iam.GetRoleInput{RoleName: aws.String(roleName)})
			if gerr == nil && getOut.Role != nil {
				return aws.ToString(getOut.Role.Arn), nil
			}
		}
		return "", err
	}

	for _, policyArn := range []string{
		"arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy",
		"arn:aws:iam::aws:policy/CloudWatchFullAccess",
	} {
		if _, err := iamClient.AttachRolePolicy(context.TODO(), &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(policyArn),
		}); err != nil {
			return "", fmt.Errorf("attach %s: %w", policyArn, err)
		}
	}
	return aws.ToString(createOut.Role.Arn), nil
}

// ---------------------------------------------------------------------------
// VPC / subnets
// ---------------------------------------------------------------------------

// VpcNetwork is the resolved VPC a preview deploys into: the VPC id, its CIDR
// (for intra-VPC security-group rules), and public subnets across distinct AZs
// (Fargate tasks + the shared ALB both live here).
type VpcNetwork struct {
	VpcID     string
	VpcCidr   string
	SubnetIDs []string
}

// DiscoverDefaultVpcNetwork resolves the account's default VPC in the client's
// region and its public subnets. The default VPC's subnets have an
// internet-gateway route, so tasks with a public IP reach ECR/the internet
// without a NAT. Callers that have deleted their default VPC should pass an
// explicit VpcID (see DescribeVpcNetwork).
func DiscoverDefaultVpcNetwork(ec2Client *ec2.Client) (VpcNetwork, error) {
	out, err := ec2Client.DescribeVpcs(context.TODO(), &ec2.DescribeVpcsInput{
		Filters: []ec2Types.Filter{{Name: aws.String("isDefault"), Values: []string{"true"}}},
	})
	if err != nil {
		return VpcNetwork{}, err
	}
	if len(out.Vpcs) == 0 {
		return VpcNetwork{}, fmt.Errorf("no default VPC in this region — supply an explicit VpcID for the preview")
	}
	return DescribeVpcNetwork(ec2Client, aws.ToString(out.Vpcs[0].VpcId))
}

// DescribeVpcNetwork resolves an explicit VPC's CIDR + public subnets (one per
// AZ, preferring auto-assign-public-IP subnets). Used when the caller supplies a
// VpcID instead of relying on the default VPC.
func DescribeVpcNetwork(ec2Client *ec2.Client, vpcID string) (VpcNetwork, error) {
	vpcsOut, err := ec2Client.DescribeVpcs(context.TODO(), &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil {
		return VpcNetwork{}, err
	}
	if len(vpcsOut.Vpcs) == 0 {
		return VpcNetwork{}, fmt.Errorf("VPC %s not found", vpcID)
	}
	cidr := aws.ToString(vpcsOut.Vpcs[0].CidrBlock)

	subnetsOut, err := ec2Client.DescribeSubnets(context.TODO(), &ec2.DescribeSubnetsInput{
		Filters: []ec2Types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return VpcNetwork{}, err
	}

	// One subnet per AZ (an ALB needs >=2 AZs), preferring public (auto-assign
	// public IP) subnets so a NAT-less task can still egress.
	perAZ := map[string]string{}     // az -> subnet id (a public one if seen)
	perAZPublic := map[string]bool{} // az -> whether the chosen subnet is public
	for _, sn := range subnetsOut.Subnets {
		az := aws.ToString(sn.AvailabilityZone)
		id := aws.ToString(sn.SubnetId)
		isPublic := aws.ToBool(sn.MapPublicIpOnLaunch)
		if _, ok := perAZ[az]; !ok || (isPublic && !perAZPublic[az]) {
			perAZ[az] = id
			perAZPublic[az] = isPublic
		}
	}
	if len(perAZ) == 0 {
		return VpcNetwork{}, fmt.Errorf("no subnets found in VPC %s", vpcID)
	}
	azs := make([]string, 0, len(perAZ))
	for az := range perAZ {
		azs = append(azs, az)
	}
	sort.Strings(azs) // deterministic subnet ordering
	subnetIDs := make([]string, 0, len(azs))
	for _, az := range azs {
		subnetIDs = append(subnetIDs, perAZ[az])
	}
	return VpcNetwork{VpcID: vpcID, VpcCidr: cidr, SubnetIDs: subnetIDs}, nil
}

// ---------------------------------------------------------------------------
// Security groups
// ---------------------------------------------------------------------------

// EnsureSecurityGroup find-or-creates a security group by name within a VPC and
// returns its id. New groups carry the default allow-all egress rule, which is
// all a preview needs (tasks egress to ECR/internet; the ALB egresses to tasks).
func EnsureSecurityGroup(ec2Client *ec2.Client, name, description, vpcID string) (string, error) {
	describeOut, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2Types.Filter{
			{Name: aws.String("group-name"), Values: []string{name}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err == nil && describeOut != nil && len(describeOut.SecurityGroups) > 0 {
		return aws.ToString(describeOut.SecurityGroups[0].GroupId), nil
	}

	createOut, err := ec2Client.CreateSecurityGroup(context.TODO(), &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String(description),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []ec2Types.TagSpecification{{
			ResourceType: ec2Types.ResourceTypeSecurityGroup,
			Tags: []ec2Types.Tag{
				{Key: aws.String("Name"), Value: aws.String(name)},
				{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)},
			},
		}},
	})
	if err != nil {
		// Lost the create race — re-describe.
		if apiErrCodeIs(err, "InvalidGroup.Duplicate") {
			describeOut, derr := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{
				Filters: []ec2Types.Filter{
					{Name: aws.String("group-name"), Values: []string{name}},
					{Name: aws.String("vpc-id"), Values: []string{vpcID}},
				},
			})
			if derr == nil && describeOut != nil && len(describeOut.SecurityGroups) > 0 {
				return aws.ToString(describeOut.SecurityGroups[0].GroupId), nil
			}
		}
		return "", err
	}
	return aws.ToString(createOut.GroupId), nil
}

// AuthorizeTCPIngressFromCIDR adds a TCP ingress rule (idempotent) allowing the
// given CIDR to reach [fromPort,toPort] on the security group. A duplicate rule
// (redeploy / concurrent) is treated as success.
func AuthorizeTCPIngressFromCIDR(ec2Client *ec2.Client, sgID, cidr string, fromPort, toPort int32) error {
	_, err := ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2Types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(fromPort),
			ToPort:     aws.Int32(toPort),
			IpRanges:   []ec2Types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String("deployment.io preview")}},
		}},
	})
	if err != nil && !apiErrCodeIs(err, "InvalidPermission.Duplicate") {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task definition
// ---------------------------------------------------------------------------

// TaskDefinitionInput describes the Fargate task definition for a preview.
type TaskDefinitionInput struct {
	Family           string
	ContainerName    string
	ImageURI         string // ECR URI including the tag
	Port             int32
	Cpu              string // Fargate CPU units, e.g. "256"
	Memory           string // Fargate memory (MiB), e.g. "512"
	ExecutionRoleArn string
	EnvVars          map[string]string
	LogGroup         string
	LogRegion        string
	LogStreamPrefix  string
	CpuArchitecture  ecsTypes.CPUArchitecture // default X86_64
	OSFamily         ecsTypes.OSFamily        // default LINUX
}

// envMapToKeyValuePairs converts an env map to ECS KeyValuePairs, sorted by key
// so a redeploy of the same env produces a byte-identical container definition
// (and the output is deterministic for tests).
func envMapToKeyValuePairs(env map[string]string) []ecsTypes.KeyValuePair {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]ecsTypes.KeyValuePair, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, ecsTypes.KeyValuePair{Name: aws.String(k), Value: aws.String(env[k])})
	}
	return pairs
}

// RegisterWebServiceTaskDefinition registers a new Fargate task-definition
// revision and returns its ARN. Env vars are injected as plaintext KeyValuePairs
// exactly as the production monolith does (they arrive already resolved from the
// runner, which is why the agent never sees them — the runner, not agentbox,
// performs the injection).
func RegisterWebServiceTaskDefinition(ecsClient *ecs.Client, in TaskDefinitionInput) (string, error) {
	cpuArch := in.CpuArchitecture
	if cpuArch == "" {
		cpuArch = ecsTypes.CPUArchitectureX8664
	}
	osFamily := in.OSFamily
	if osFamily == "" {
		osFamily = ecsTypes.OSFamilyLinux
	}

	container := ecsTypes.ContainerDefinition{
		Name:         aws.String(in.ContainerName),
		Image:        aws.String(in.ImageURI),
		Essential:    aws.Bool(true),
		Environment:  envMapToKeyValuePairs(in.EnvVars),
		PortMappings: []ecsTypes.PortMapping{{ContainerPort: aws.Int32(in.Port), Protocol: ecsTypes.TransportProtocolTcp}},
		LogConfiguration: &ecsTypes.LogConfiguration{
			LogDriver: ecsTypes.LogDriverAwslogs,
			Options: map[string]string{
				"awslogs-create-group":  "true",
				"awslogs-group":         in.LogGroup,
				"awslogs-region":        in.LogRegion,
				"awslogs-stream-prefix": in.LogStreamPrefix,
			},
		},
	}

	out, err := ecsClient.RegisterTaskDefinition(context.TODO(), &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(in.Family),
		ContainerDefinitions:    []ecsTypes.ContainerDefinition{container},
		Cpu:                     aws.String(in.Cpu),
		Memory:                  aws.String(in.Memory),
		ExecutionRoleArn:        aws.String(in.ExecutionRoleArn),
		NetworkMode:             ecsTypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecsTypes.Compatibility{ecsTypes.CompatibilityFargate},
		RuntimePlatform: &ecsTypes.RuntimePlatform{
			CpuArchitecture:       cpuArch,
			OperatingSystemFamily: osFamily,
		},
		Tags: []ecsTypes.Tag{{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)}},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.TaskDefinition.TaskDefinitionArn), nil
}

// ---------------------------------------------------------------------------
// Target group
// ---------------------------------------------------------------------------

// TargetGroupInput describes an ip-target-type target group for a preview
// service.
type TargetGroupInput struct {
	Name            string
	Port            int32
	VpcID           string
	HealthCheckPath string // default "/"
}

// EnsureTargetGroup find-or-creates an ip target group and returns its ARN.
// Target type is ip (required for Fargate/awsvpc); ECS registers the task's ENI
// IP into it via the service's LoadBalancers block.
func EnsureTargetGroup(elbClient *elb.Client, in TargetGroupInput) (string, error) {
	describeOut, err := elbClient.DescribeTargetGroups(context.TODO(), &elb.DescribeTargetGroupsInput{
		Names: []string{in.Name},
	})
	if err == nil && describeOut != nil && len(describeOut.TargetGroups) > 0 {
		return aws.ToString(describeOut.TargetGroups[0].TargetGroupArn), nil
	}
	if err != nil && !apiErrCodeIs(err, "TargetGroupNotFound") {
		return "", err
	}

	healthPath := in.HealthCheckPath
	if healthPath == "" {
		healthPath = "/"
	}
	createOut, err := elbClient.CreateTargetGroup(context.TODO(), &elb.CreateTargetGroupInput{
		Name:                       aws.String(in.Name),
		Port:                       aws.Int32(in.Port),
		Protocol:                   elbTypes.ProtocolEnumHttp,
		TargetType:                 elbTypes.TargetTypeEnumIp,
		VpcId:                      aws.String(in.VpcID),
		HealthCheckEnabled:         aws.Bool(true),
		HealthCheckProtocol:        elbTypes.ProtocolEnumHttp,
		HealthCheckPath:            aws.String(healthPath),
		HealthCheckIntervalSeconds: aws.Int32(30),
		HealthCheckTimeoutSeconds:  aws.Int32(10),
		HealthyThresholdCount:      aws.Int32(2),
		UnhealthyThresholdCount:    aws.Int32(5),
		Matcher:                    &elbTypes.Matcher{HttpCode: aws.String("200-399")},
		Tags: []elbTypes.Tag{
			{Key: aws.String("Name"), Value: aws.String(in.Name)},
			{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(createOut.TargetGroups[0].TargetGroupArn), nil
}

// ---------------------------------------------------------------------------
// ECS service
// ---------------------------------------------------------------------------

// EcsServiceInput describes the Fargate service for a preview.
type EcsServiceInput struct {
	ClusterArn        string
	ServiceName       string
	TaskDefinitionArn string
	ContainerName     string
	Port              int32
	TargetGroupArn    string // wires container:port -> target group
	SubnetIDs         []string
	SecurityGroupIDs  []string
	AssignPublicIP    bool
	// WaitForStable blocks on the service reaching a stable state. It only
	// succeeds once the target group is health-checked by a load balancer, so
	// Cw1a (no ALB yet) leaves it false; Cw1b sets it once the listener rule
	// attaches the target group to the shared ALB.
	WaitForStable bool
}

// CreateOrUpdateEcsService creates the service if absent, else updates it to the
// new task definition (forcing a fresh deployment). Returns the service ARN.
func CreateOrUpdateEcsService(ecsClient *ecs.Client, in EcsServiceInput, logsWriter io.Writer) (string, error) {
	describeOut, err := ecsClient.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
		Cluster:  aws.String(in.ClusterArn),
		Services: []string{in.ServiceName},
	})
	if err != nil {
		return "", err
	}
	var existingArn string
	for _, svc := range describeOut.Services {
		if aws.ToString(svc.ServiceName) == in.ServiceName {
			status := aws.ToString(svc.Status)
			if status == "ACTIVE" || status == "DRAINING" {
				existingArn = aws.ToString(svc.ServiceArn)
			}
		}
	}

	if existingArn != "" {
		io.WriteString(logsWriter, fmt.Sprintf("Updating preview service: %s\n", in.ServiceName))
		if _, err := ecsClient.UpdateService(context.TODO(), &ecs.UpdateServiceInput{
			Cluster:            aws.String(in.ClusterArn),
			Service:            aws.String(in.ServiceName),
			TaskDefinition:     aws.String(in.TaskDefinitionArn),
			DesiredCount:       aws.Int32(1),
			ForceNewDeployment: true,
			PropagateTags:      ecsTypes.PropagateTagsTaskDefinition,
		}); err != nil {
			return "", err
		}
		if err := waitEcsServiceStable(ecsClient, in, logsWriter); err != nil {
			return "", err
		}
		return existingArn, nil
	}

	var loadBalancers []ecsTypes.LoadBalancer
	var gracePeriod *int32
	if in.TargetGroupArn != "" {
		loadBalancers = []ecsTypes.LoadBalancer{{
			ContainerName:  aws.String(in.ContainerName),
			ContainerPort:  aws.Int32(in.Port),
			TargetGroupArn: aws.String(in.TargetGroupArn),
		}}
		gracePeriod = aws.Int32(60)
	}
	assignPublicIP := ecsTypes.AssignPublicIpDisabled
	if in.AssignPublicIP {
		assignPublicIP = ecsTypes.AssignPublicIpEnabled
	}

	io.WriteString(logsWriter, fmt.Sprintf("Creating preview service: %s\n", in.ServiceName))
	createOut, err := ecsClient.CreateService(context.TODO(), &ecs.CreateServiceInput{
		ServiceName:                   aws.String(in.ServiceName),
		Cluster:                       aws.String(in.ClusterArn),
		TaskDefinition:                aws.String(in.TaskDefinitionArn),
		DesiredCount:                  aws.Int32(1),
		LaunchType:                    ecsTypes.LaunchTypeFargate,
		SchedulingStrategy:            ecsTypes.SchedulingStrategyReplica,
		LoadBalancers:                 loadBalancers,
		HealthCheckGracePeriodSeconds: gracePeriod,
		PropagateTags:                 ecsTypes.PropagateTagsTaskDefinition,
		NetworkConfiguration: &ecsTypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{
				Subnets:        in.SubnetIDs,
				SecurityGroups: in.SecurityGroupIDs,
				AssignPublicIp: assignPublicIP,
			},
		},
		Tags: []ecsTypes.Tag{{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)}},
	})
	if err != nil {
		return "", err
	}
	if err := waitEcsServiceStable(ecsClient, in, logsWriter); err != nil {
		return "", err
	}
	return aws.ToString(createOut.Service.ServiceArn), nil
}

// waitEcsServiceStable optionally blocks until the service is stable.
func waitEcsServiceStable(ecsClient *ecs.Client, in EcsServiceInput, logsWriter io.Writer) error {
	if !in.WaitForStable {
		return nil
	}
	io.WriteString(logsWriter, fmt.Sprintf("Waiting for preview service to stabilize: %s\n", in.ServiceName))
	waiter := ecs.NewServicesStableWaiter(ecsClient)
	return waiter.Wait(context.TODO(), &ecs.DescribeServicesInput{
		Cluster:  aws.String(in.ClusterArn),
		Services: []string{in.ServiceName},
	}, 15*time.Minute)
}

// WaitForRunningTask polls the service until it reports at least one RUNNING
// task or the deadline elapses. Cw1a uses this in place of the stable waiter
// (which can't succeed before an ALB health-checks the target group) so the
// deploy core can confirm the task actually launched.
func WaitForRunningTask(ecsClient *ecs.Client, clusterArn, serviceName string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		out, err := ecsClient.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
			Cluster:  aws.String(clusterArn),
			Services: []string{serviceName},
		})
		if err != nil {
			return false, err
		}
		for _, svc := range out.Services {
			if aws.ToString(svc.ServiceName) == serviceName && svc.RunningCount >= 1 {
				return true, nil
			}
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(10 * time.Second)
	}
}

// ---------------------------------------------------------------------------
// Shared ALB + header-routed listener rule (ingress half)
// ---------------------------------------------------------------------------

// SharedAlbInput describes the one-per-org+region ALB that fronts every
// web-service preview.
type SharedAlbInput struct {
	AlbName   string
	AlbSgName string
	VpcID     string
	SubnetIDs []string
}

// SharedAlbResult carries the shared ALB's resources.
type SharedAlbResult struct {
	LoadBalancerArn string
	LoadBalancerDns string
	ListenerArn     string // the HTTP:80 listener
	SecurityGroupID string
}

// EnsureSharedAlb find-or-creates the shared, internet-facing ALB + its security
// group (inbound 80 from anywhere — CloudFront reaches the origin over HTTP) + an
// HTTP:80 listener whose DEFAULT action is a fixed 404 (a request without a
// matching X-Preview-Target header belongs to no preview). Per-preview target
// groups attach to this one listener via header-routed rules
// (EnsurePreviewListenerRule). One ALB serves every preview in the org+region,
// which is why it's found-or-created by a stable name rather than per preview.
func EnsureSharedAlb(elbClient *elb.Client, ec2Client *ec2.Client, in SharedAlbInput) (SharedAlbResult, error) {
	var res SharedAlbResult

	albSgID, err := EnsureSecurityGroup(ec2Client, in.AlbSgName, "deployment.io preview shared ALB", in.VpcID)
	if err != nil {
		return res, fmt.Errorf("ensure ALB security group: %w", err)
	}
	if err := AuthorizeTCPIngressFromCIDR(ec2Client, albSgID, "0.0.0.0/0", 80, 80); err != nil {
		return res, fmt.Errorf("authorize ALB ingress: %w", err)
	}
	res.SecurityGroupID = albSgID

	// Find-or-create the ALB.
	descOut, err := elbClient.DescribeLoadBalancers(context.TODO(), &elb.DescribeLoadBalancersInput{Names: []string{in.AlbName}})
	if err == nil && descOut != nil && len(descOut.LoadBalancers) > 0 {
		res.LoadBalancerArn = aws.ToString(descOut.LoadBalancers[0].LoadBalancerArn)
		res.LoadBalancerDns = aws.ToString(descOut.LoadBalancers[0].DNSName)
	} else if err != nil && !apiErrCodeIs(err, "LoadBalancerNotFound") {
		return res, err
	}
	if res.LoadBalancerArn == "" {
		createOut, err := elbClient.CreateLoadBalancer(context.TODO(), &elb.CreateLoadBalancerInput{
			Name:           aws.String(in.AlbName),
			Type:           elbTypes.LoadBalancerTypeEnumApplication,
			Scheme:         elbTypes.LoadBalancerSchemeEnumInternetFacing,
			IpAddressType:  elbTypes.IpAddressTypeIpv4,
			SecurityGroups: []string{albSgID},
			Subnets:        in.SubnetIDs,
			Tags: []elbTypes.Tag{
				{Key: aws.String("Name"), Value: aws.String(in.AlbName)},
				{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)},
			},
		})
		if err != nil {
			return res, err
		}
		res.LoadBalancerArn = aws.ToString(createOut.LoadBalancers[0].LoadBalancerArn)
		res.LoadBalancerDns = aws.ToString(createOut.LoadBalancers[0].DNSName)
		if err := elb.NewLoadBalancerAvailableWaiter(elbClient).Wait(context.TODO(),
			&elb.DescribeLoadBalancersInput{LoadBalancerArns: []string{res.LoadBalancerArn}}, 5*time.Minute); err != nil {
			return res, fmt.Errorf("wait ALB available: %w", err)
		}
	}

	// Find-or-create the HTTP:80 listener with a default 404.
	listenersOut, err := elbClient.DescribeListeners(context.TODO(), &elb.DescribeListenersInput{LoadBalancerArn: aws.String(res.LoadBalancerArn)})
	if err != nil {
		return res, err
	}
	for _, l := range listenersOut.Listeners {
		if aws.ToInt32(l.Port) == 80 {
			res.ListenerArn = aws.ToString(l.ListenerArn)
			break
		}
	}
	if res.ListenerArn == "" {
		createListenerOut, err := elbClient.CreateListener(context.TODO(), &elb.CreateListenerInput{
			LoadBalancerArn: aws.String(res.LoadBalancerArn),
			Port:            aws.Int32(80),
			Protocol:        elbTypes.ProtocolEnumHttp,
			DefaultActions: []elbTypes.Action{{
				Type: elbTypes.ActionTypeEnumFixedResponse,
				FixedResponseConfig: &elbTypes.FixedResponseActionConfig{
					StatusCode:  aws.String("404"),
					ContentType: aws.String("text/plain"),
					MessageBody: aws.String("No matching preview"),
				},
			}},
		})
		if err != nil {
			return res, err
		}
		res.ListenerArn = aws.ToString(createListenerOut.Listeners[0].ListenerArn)
	}
	return res, nil
}

// EnsurePreviewListenerRule find-or-creates the header-routed rule that forwards
// requests carrying `<headerName>: <previewID>` to the preview's target group,
// returning the rule ARN. Idempotent: an existing rule for this previewID is
// reused; otherwise the lowest free priority is taken (retrying if a concurrent
// deploy grabs it first). The X-Preview-Target header is injected by the
// preview's CloudFront distribution as an origin custom header, so only that
// preview's own CloudFront can reach its target group through the shared ALB.
func EnsurePreviewListenerRule(elbClient *elb.Client, listenerArn, headerName, previewID, targetGroupArn string) (string, error) {
	rulesOut, err := elbClient.DescribeRules(context.TODO(), &elb.DescribeRulesInput{ListenerArn: aws.String(listenerArn)})
	if err != nil {
		return "", err
	}
	used := map[int]bool{}
	for _, r := range rulesOut.Rules {
		for _, c := range r.Conditions {
			if aws.ToString(c.Field) == "http-header" && c.HttpHeaderConfig != nil &&
				aws.ToString(c.HttpHeaderConfig.HttpHeaderName) == headerName {
				for _, v := range c.HttpHeaderConfig.Values {
					if v == previewID {
						return aws.ToString(r.RuleArn), nil
					}
				}
			}
		}
		if p, perr := strconv.Atoi(aws.ToString(r.Priority)); perr == nil {
			used[p] = true
		}
	}

	condition := elbTypes.RuleCondition{
		Field: aws.String("http-header"),
		HttpHeaderConfig: &elbTypes.HttpHeaderConditionConfig{
			HttpHeaderName: aws.String(headerName),
			Values:         []string{previewID},
		},
	}
	action := elbTypes.Action{Type: elbTypes.ActionTypeEnumForward, TargetGroupArn: aws.String(targetGroupArn)}

	priority := 1
	for attempt := 0; attempt < 50; attempt++ {
		for used[priority] {
			priority++
		}
		if priority > 50000 { // ALB max rule priority
			return "", fmt.Errorf("no free ALB listener-rule priority available")
		}
		out, err := elbClient.CreateRule(context.TODO(), &elb.CreateRuleInput{
			ListenerArn: aws.String(listenerArn),
			Priority:    aws.Int32(int32(priority)),
			Conditions:  []elbTypes.RuleCondition{condition},
			Actions:     []elbTypes.Action{action},
			Tags: []elbTypes.Tag{
				{Key: aws.String("created by"), Value: aws.String(awsCreatedByTag)},
				{Key: aws.String("preview"), Value: aws.String(previewID)},
			},
		})
		if err == nil {
			return aws.ToString(out.Rules[0].RuleArn), nil
		}
		if apiErrCodeIs(err, "PriorityInUse") {
			used[priority] = true
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("could not allocate a listener-rule priority after retries")
}
