package agenttools

// deploy_web_service_preview.go is the arg-based, self-contained web-service preview
// deploy core — the twin of deploy_static.go's DeployStaticSitePreview. Where the
// static preview stands up S3 + CloudFront, a web-service preview builds/pushes a
// container image, runs it as a Fargate service behind a target group, and fronts
// it with a shared ALB + a per-preview CloudFront distribution.
//
// Like the static core it is invoked in-process by the (future, Cw2)
// deploy_web_service_preview MCP tool handler and is deliberately NOT the
// production DeployAwsWebService command (a 1348-line monolith welded to the job
// `parameters` map with a per-service ALB). It references that monolith's SDK
// calls but is written fresh so the preview can use a shared, cost-optimized
// ingress. The AWS primitives live in the leaf utils/aws_utils package.
//
// The core was built in two slices: Cw1a (the ECS half — image build+push,
// cluster, execution role, VPC/subnets, task security group, task definition,
// target group, and the Fargate service) and Cw1b (the ingress half — the shared
// ALB + header-routed listener rule + per-preview CloudFront, which populate the
// public URL).

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfrontTypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/moby/moby/client"
	"github.com/moby/moby/pkg/archive"
)

// previewTargetHeader is the origin custom header the preview's CloudFront
// distribution injects and the shared ALB's listener rule matches — the routing
// key that fans one shared ALB out to many previews' target groups.
const previewTargetHeader = "X-Preview-Target"

// allViewerOriginRequestPolicyID is CloudFront's AWS-managed "AllViewer" origin
// request policy: it forwards all viewer headers, cookies, and query strings to
// the origin. A web service (unlike a static site) needs the full request, and
// like cachingDisabledPolicyID this is a global AWS-managed id, safe to hardcode
// in the BYO multi-account model.
const allViewerOriginRequestPolicyID = "216adef6-5c7f-47e4-b989-5492eafa07d3"

// Fargate defaults for a preview: the smallest valid CPU/memory combo (0.25 vCPU
// / 0.5 GB) — cheapest for a short-lived, low-traffic preview.
const (
	defaultPreviewCPU    = "256"
	defaultPreviewMemory = "512"
)

// WebServicePreviewDeployInput is the request for DeployWebServicePreview.
type WebServicePreviewDeployInput struct {
	OrgID     string // organization id (resource naming)
	PreviewID string // the ephemeral preview Deployment's id (per-preview resource naming)
	Region    string // AWS region string, e.g. "us-east-1" (log config)

	// --- Image build (runner-side; the agent built the source into /work) ---
	BuildContextDir string            // host path to the build context (the repo dir with the Dockerfile)
	DockerfilePath  string            // Dockerfile path relative to the context; default "Dockerfile"
	ImageTag        string            // image tag; default a unix-timestamp tag (unique per redeploy)
	BuildArgs       map[string]string // docker build args

	// --- Runtime ---
	Port            int32             // container port the service listens on
	HealthCheckPath string            // target-group health check path; default "/"
	Cpu             string            // Fargate CPU units; default "256"
	Memory          string            // Fargate memory MiB; default "512"
	EnvVars         map[string]string // runtime env, already resolved by the runner (agent-blind)
	CpuArchitecture ecsTypes.CPUArchitecture
	OSFamily        ecsTypes.OSFamily

	// --- Networking (optional; discover the default VPC when VpcID is empty) ---
	VpcID     string
	SubnetIDs []string

	// --- Reuse across iterations (empty on the first deploy of a task) ---
	ExistingClusterArn       string
	ExistingExecutionRoleArn string
	ExistingTargetGroupArn   string
	ExistingEcsServiceArn    string
	// ExistingDistID is the per-preview CloudFront distribution from a prior
	// iteration. The distribution fronts the (stable) shared ALB, so on reuse
	// there's nothing to recreate — the same URL keeps serving. The shared ALB +
	// header rule are found-or-created by identity, so they need no reuse handle.
	ExistingDistID string

	// --- Clients (built by the caller from the runner's IAM role + region) ---
	EcrClient        *ecr.Client
	EcsClient        *ecs.Client
	Ec2Client        *ec2.Client
	ElbClient        *elb.Client
	IamClient        *iam.Client
	CloudfrontClient *cloudfront.Client // used by the Cw1b ingress half

	// SkipServiceStableWait leaves the ECS service create/update without blocking
	// on the stable waiter. Cw1a sets this (the target group can't go healthy
	// before an ALB health-checks it); Cw1b clears it once the listener rule
	// attaches the target group to the shared ALB.
	SkipServiceStableWait bool
}

// WebServicePreviewDeployResult carries every resource the deploy created or
// reused, so the (Cw2) tool can persist it via SavePreview for the next
// iteration. Field names mirror the union deployments.UpdateDeploymentDtoV1 so
// the mapping in the commands layer is one-to-one.
type WebServicePreviewDeployResult struct {
	EcrRepositoryUri  string
	ImageUri          string // ECR URI including the pushed tag
	ClusterArn        string
	ExecutionRoleArn  string
	TaskDefinitionArn string
	TargetGroupArn    string
	EcsServiceArn     string
	Port              int32
	VpcID             string
	TaskRunning       bool // Cw1a signal: at least one task reached RUNNING

	// --- Ingress half (populated by Cw1b) ---
	LoadBalancerArn           string
	LoadBalancerDns           string
	ListenerArn               string
	ListenerRuleArn           string
	CloudFrontDistributionID  string
	CloudFrontDistributionArn string
	CloudFrontDomainName      string
	URL                       string
}

// DeployWebServicePreview builds + pushes the agent's container image and runs it
// as an ephemeral Fargate service wired to a target group, on the shared
// per-org ECS cluster. It is idempotent across a task's iterations: reused
// resource ids are passed back via the Existing* fields and a redeploy registers
// a fresh task-definition revision + updates the service.
//
// Full flow: build+push image -> shared cluster -> execution role -> VPC/subnets
// -> task security group -> task definition -> target group -> shared ALB +
// header-routed listener rule -> Fargate service -> per-preview CloudFront
// distribution. Returns the public https://<x>.cloudfront.net URL. Like the
// static preview it does not block on CloudFront propagation (SkipServiceStableWait
// also skips the ECS stable wait) — the agent's verify step polls the URL.
func DeployWebServicePreview(in WebServicePreviewDeployInput, logsWriter io.Writer) (WebServicePreviewDeployResult, error) {
	// Defaults.
	dockerfile := in.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	imageTag := in.ImageTag
	if imageTag == "" {
		imageTag = fmt.Sprintf("%d", time.Now().Unix())
	}
	cpu := in.Cpu
	if cpu == "" {
		cpu = defaultPreviewCPU
	}
	memory := in.Memory
	if memory == "" {
		memory = defaultPreviewMemory
	}

	if in.Port <= 0 {
		return WebServicePreviewDeployResult{}, fmt.Errorf("port is required for a web-service preview")
	}
	if _, err := os.Stat(filepath.Join(in.BuildContextDir, dockerfile)); err != nil {
		return WebServicePreviewDeployResult{}, fmt.Errorf("no %s in build context %s: %w", dockerfile, in.BuildContextDir, err)
	}

	res := WebServicePreviewDeployResult{Port: in.Port}

	// 1. Build + push the image to a per-preview ECR repo.
	repoURI, err := aws_utils.EnsureEcrRepository(in.EcrClient, ecrRepositoryName(in.OrgID, in.PreviewID), logsWriter)
	if err != nil {
		return res, fmt.Errorf("ensure ECR repository: %w", err)
	}
	res.EcrRepositoryUri = repoURI
	imageURI := repoURI + ":" + imageTag
	localTag := localImageName(in.OrgID, in.PreviewID, imageTag)
	if err := buildAndPushImage(in.EcrClient, in.BuildContextDir, dockerfile, localTag, imageURI, in.BuildArgs, logsWriter); err != nil {
		return res, fmt.Errorf("build and push image: %w", err)
	}
	res.ImageUri = imageURI

	// 2. Shared per-org Fargate cluster.
	clusterArn := in.ExistingClusterArn
	if clusterArn == "" {
		clusterArn, err = aws_utils.EnsureEcsCluster(in.EcsClient, clusterName(in.OrgID))
		if err != nil {
			return res, fmt.Errorf("ensure ECS cluster: %w", err)
		}
	}
	res.ClusterArn = clusterArn

	// 3. Task execution role (ECR pull + CloudWatch logs).
	executionRoleArn := in.ExistingExecutionRoleArn
	if executionRoleArn == "" {
		executionRoleArn, err = aws_utils.EnsureEcsTaskExecutionRole(in.IamClient, executionRoleName(in.OrgID))
		if err != nil {
			return res, fmt.Errorf("ensure task execution role: %w", err)
		}
	}
	res.ExecutionRoleArn = executionRoleArn

	// 4. Resolve the VPC + public subnets the task and (Cw1b) ALB live in.
	var network aws_utils.VpcNetwork
	if in.VpcID != "" {
		network, err = aws_utils.DescribeVpcNetwork(in.Ec2Client, in.VpcID)
	} else {
		network, err = aws_utils.DiscoverDefaultVpcNetwork(in.Ec2Client)
	}
	if err != nil {
		return res, fmt.Errorf("resolve VPC network: %w", err)
	}
	if len(in.SubnetIDs) > 0 {
		network.SubnetIDs = in.SubnetIDs
	}
	res.VpcID = network.VpcID

	// 5. Shared per-org task security group: reachable from within the VPC (the
	// ALB fronts it) on all TCP; default egress-all lets it pull ECR / reach out.
	taskSgID, err := aws_utils.EnsureSecurityGroup(in.Ec2Client, taskSecurityGroupName(in.OrgID),
		"deployment.io preview task security group", network.VpcID)
	if err != nil {
		return res, fmt.Errorf("ensure task security group: %w", err)
	}
	if err := aws_utils.AuthorizeTCPIngressFromCIDR(in.Ec2Client, taskSgID, network.VpcCidr, 0, 65535); err != nil {
		return res, fmt.Errorf("authorize task security group ingress: %w", err)
	}

	// 6. Task definition (new revision each deploy so a redeploy re-pulls).
	taskDefArn, err := aws_utils.RegisterWebServiceTaskDefinition(in.EcsClient, aws_utils.TaskDefinitionInput{
		Family:           taskDefinitionFamily(in.PreviewID),
		ContainerName:    containerName(in.PreviewID),
		ImageURI:         imageURI,
		Port:             in.Port,
		Cpu:              cpu,
		Memory:           memory,
		ExecutionRoleArn: executionRoleArn,
		EnvVars:          in.EnvVars,
		LogGroup:         logGroupName(in.OrgID, in.PreviewID),
		LogRegion:        in.Region,
		LogStreamPrefix:  "application",
		CpuArchitecture:  in.CpuArchitecture,
		OSFamily:         in.OSFamily,
	})
	if err != nil {
		return res, fmt.Errorf("register task definition: %w", err)
	}
	res.TaskDefinitionArn = taskDefArn

	// 7. Target group (ECS registers the task ENI IP into it).
	targetGroupArn := in.ExistingTargetGroupArn
	if targetGroupArn == "" {
		targetGroupArn, err = aws_utils.EnsureTargetGroup(in.ElbClient, aws_utils.TargetGroupInput{
			Name:            targetGroupName(in.PreviewID),
			Port:            in.Port,
			VpcID:           network.VpcID,
			HealthCheckPath: in.HealthCheckPath,
		})
		if err != nil {
			return res, fmt.Errorf("ensure target group: %w", err)
		}
	}
	res.TargetGroupArn = targetGroupArn

	// 8. Shared ingress: the one-per-org ALB + a header-routed listener rule that
	// forwards this preview's X-Preview-Target header to its target group.
	// Creating the rule associates the target group with the ALB, so the
	// service's targets start getting health-checked (and the service can reach a
	// stable state below).
	alb, err := aws_utils.EnsureSharedAlb(in.ElbClient, in.Ec2Client, aws_utils.SharedAlbInput{
		AlbName:   albName(in.OrgID),
		AlbSgName: albSecurityGroupName(in.OrgID),
		VpcID:     network.VpcID,
		SubnetIDs: network.SubnetIDs,
	})
	if err != nil {
		return res, fmt.Errorf("ensure shared ALB: %w", err)
	}
	res.LoadBalancerArn = alb.LoadBalancerArn
	res.LoadBalancerDns = alb.LoadBalancerDns
	res.ListenerArn = alb.ListenerArn

	ruleArn, err := aws_utils.EnsurePreviewListenerRule(in.ElbClient, alb.ListenerArn, previewTargetHeader, in.PreviewID, targetGroupArn)
	if err != nil {
		return res, fmt.Errorf("ensure listener rule: %w", err)
	}
	res.ListenerRuleArn = ruleArn

	// 9. Create/update the Fargate service, wired to the target group.
	serviceArn, err := aws_utils.CreateOrUpdateEcsService(in.EcsClient, aws_utils.EcsServiceInput{
		ClusterArn:        clusterArn,
		ServiceName:       ecsServiceName(in.PreviewID),
		TaskDefinitionArn: taskDefArn,
		ContainerName:     containerName(in.PreviewID),
		Port:              in.Port,
		TargetGroupArn:    targetGroupArn,
		SubnetIDs:         network.SubnetIDs,
		SecurityGroupIDs:  []string{taskSgID},
		AssignPublicIP:    true, // public subnets, no NAT
		WaitForStable:     !in.SkipServiceStableWait,
	}, logsWriter)
	if err != nil {
		return res, fmt.Errorf("create/update ECS service: %w", err)
	}
	res.EcsServiceArn = serviceArn

	if in.SkipServiceStableWait {
		// Returned promptly (the agent's verify step polls the URL until live); a
		// best-effort check still reports whether a task already launched.
		running, werr := aws_utils.WaitForRunningTask(in.EcsClient, clusterArn, ecsServiceName(in.PreviewID), 5*time.Minute)
		if werr != nil {
			return res, fmt.Errorf("await running task: %w", werr)
		}
		res.TaskRunning = running
	} else {
		res.TaskRunning = true // the stable waiter already confirmed healthy targets
	}

	// 10. Per-preview CloudFront distribution fronting the shared ALB — the public
	// https://<x>.cloudfront.net surface (free managed cert to the viewer; reaches
	// the ALB over HTTP with the X-Preview-Target origin header). Reused across
	// iterations via ExistingDistID.
	distID, distArn, domain, err := ensurePreviewDistribution(in.CloudfrontClient, alb.LoadBalancerDns, in.PreviewID, in.ExistingDistID, logsWriter)
	if err != nil {
		return res, fmt.Errorf("ensure preview distribution: %w", err)
	}
	res.CloudFrontDistributionID = distID
	res.CloudFrontDistributionArn = distArn
	res.CloudFrontDomainName = domain
	if domain != "" {
		res.URL = "https://" + domain
	}

	return res, nil
}

// ensurePreviewDistribution creates (or, on reuse, re-reads) the per-preview
// CloudFront distribution that fronts the shared ALB, returning its id, ARN, and
// domain. Like the static preview it does NOT block on CloudFront propagation
// (the agent's verify step polls the URL until it serves).
func ensurePreviewDistribution(cf *cloudfront.Client, albDns, previewID, existingDistID string, logsWriter io.Writer) (id, arn, domain string, err error) {
	if existingDistID != "" {
		// Reuse: the distribution already fronts the (stable) shared ALB. Re-read it
		// so the result still carries a usable URL.
		out, gerr := cf.GetDistribution(context.TODO(), &cloudfront.GetDistributionInput{Id: aws.String(existingDistID)})
		if gerr != nil {
			return "", "", "", gerr
		}
		io.WriteString(logsWriter, fmt.Sprintf("Reusing preview distribution: %s\n", existingDistID))
		return aws.ToString(out.Distribution.Id), aws.ToString(out.Distribution.ARN), aws.ToString(out.Distribution.DomainName), nil
	}
	io.WriteString(logsWriter, "Creating preview CloudFront distribution (ALB origin)\n")
	out, cerr := cf.CreateDistribution(context.TODO(), &cloudfront.CreateDistributionInput{
		DistributionConfig: buildPreviewWebServiceDistributionConfig(albDns, previewID),
	})
	if cerr != nil {
		return "", "", "", cerr
	}
	d := out.Distribution
	return aws.ToString(d.Id), aws.ToString(d.ARN), aws.ToString(d.DomainName), nil
}

// buildPreviewWebServiceDistributionConfig is the CloudFront config for a
// web-service preview: a CustomOrigin pointing at the shared ALB's DNS name over
// HTTP-only, with the X-Preview-Target origin custom header that routes this
// preview through the ALB's listener rule. Caching is disabled (a preview
// iterates constantly) and the AllViewer origin-request policy forwards the full
// request so the app sees real headers/cookies/query. cf. the static twin
// buildPreviewDistributionConfig (S3-OAC origin, GET/HEAD only).
func buildPreviewWebServiceDistributionConfig(albDns, previewID string) *cloudfrontTypes.DistributionConfig {
	const originID = "alb-origin"
	origins := &cloudfrontTypes.Origins{
		Quantity: aws.Int32(1),
		Items: []cloudfrontTypes.Origin{{
			Id:         aws.String(originID),
			DomainName: aws.String(albDns),
			CustomOriginConfig: &cloudfrontTypes.CustomOriginConfig{
				HTTPPort:             aws.Int32(80),
				HTTPSPort:            aws.Int32(443),
				OriginProtocolPolicy: cloudfrontTypes.OriginProtocolPolicyHttpOnly,
				OriginSslProtocols: &cloudfrontTypes.OriginSslProtocols{
					Quantity: aws.Int32(1),
					Items:    []cloudfrontTypes.SslProtocol{cloudfrontTypes.SslProtocolTLSv12},
				},
			},
			CustomHeaders: &cloudfrontTypes.CustomHeaders{
				Quantity: aws.Int32(1),
				Items: []cloudfrontTypes.OriginCustomHeader{{
					HeaderName:  aws.String(previewTargetHeader),
					HeaderValue: aws.String(previewID),
				}},
			},
		}},
	}

	allMethods := []cloudfrontTypes.Method{
		cloudfrontTypes.MethodGet, cloudfrontTypes.MethodHead, cloudfrontTypes.MethodOptions,
		cloudfrontTypes.MethodPut, cloudfrontTypes.MethodPost, cloudfrontTypes.MethodPatch,
		cloudfrontTypes.MethodDelete,
	}
	behavior := &cloudfrontTypes.DefaultCacheBehavior{
		TargetOriginId:       aws.String(originID),
		ViewerProtocolPolicy: cloudfrontTypes.ViewerProtocolPolicyAllowAll,
		AllowedMethods: &cloudfrontTypes.AllowedMethods{
			Quantity: aws.Int32(int32(len(allMethods))),
			Items:    allMethods,
			CachedMethods: &cloudfrontTypes.CachedMethods{
				Quantity: aws.Int32(2),
				Items:    []cloudfrontTypes.Method{cloudfrontTypes.MethodGet, cloudfrontTypes.MethodHead},
			},
		},
		CachePolicyId:         aws.String(cachingDisabledPolicyID),
		OriginRequestPolicyId: aws.String(allViewerOriginRequestPolicyID),
	}

	return &cloudfrontTypes.DistributionConfig{
		CallerReference:      aws.String(fmt.Sprintf("preview-ws-%s-%d", previewID, time.Now().Unix())),
		Comment:              aws.String("Preview distribution for " + previewID),
		DefaultCacheBehavior: behavior,
		Enabled:              aws.Bool(true),
		Origins:              origins,
		PriceClass:           cloudfrontTypes.PriceClassPriceClassAll,
	}
}

// buildAndPushImage builds the image from the agent's build context and pushes it
// to ECR — the runner (which holds the Docker daemon + cloud creds), not
// agentbox, performs both, mirroring the production build_docker_image /
// upload_docker_image_to_ecr commands.
func buildAndPushImage(ecrClient *ecr.Client, buildContextDir, dockerfile, localTag, imageURI string, buildArgs map[string]string, logsWriter io.Writer) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	tar, err := archive.TarWithOptions(buildContextDir, &archive.TarOptions{})
	if err != nil {
		return fmt.Errorf("tar build context: %w", err)
	}
	io.WriteString(logsWriter, "Building preview image\n")
	buildRes, err := cli.ImageBuild(ctx, tar, types.ImageBuildOptions{
		Dockerfile: dockerfile,
		Tags:       []string{localTag},
		Remove:     true,
		BuildArgs:  toBuildArgPointers(buildArgs),
	})
	if err != nil {
		return err
	}
	defer buildRes.Body.Close()
	if err := streamDockerJSONLogs(buildRes.Body, logsWriter); err != nil {
		return fmt.Errorf("image build: %w", err)
	}

	if err := cli.ImageTag(ctx, localTag, imageURI); err != nil {
		return fmt.Errorf("tag image: %w", err)
	}

	auth, err := aws_utils.EcrDockerAuth(ecrClient)
	if err != nil {
		return fmt.Errorf("ecr auth: %w", err)
	}
	io.WriteString(logsWriter, fmt.Sprintf("Pushing preview image to ECR: %s\n", imageURI))
	pushRes, err := cli.ImagePush(ctx, imageURI, image.PushOptions{RegistryAuth: auth})
	if err != nil {
		return err
	}
	defer pushRes.Close()
	if err := streamDockerJSONLogs(pushRes, logsWriter); err != nil {
		return fmt.Errorf("image push: %w", err)
	}
	return nil
}

// toBuildArgPointers adapts a plain build-arg map to docker's map[string]*string.
func toBuildArgPointers(args map[string]string) map[string]*string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]*string, len(args))
	for k, v := range args {
		v := v
		out[k] = &v
	}
	return out
}

// streamDockerJSONLogs copies a docker build/push JSON-lines stream to the log
// writer and returns an error if any line reports one. Both the build and push
// streams surface failures as a trailing {"error":...} / {"errorDetail":...}
// line rather than a non-nil call error.
func streamDockerJSONLogs(r io.Reader, logsWriter io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var streamErr string
	for scanner.Scan() {
		line := scanner.Text()
		var msg struct {
			Stream      string `json:"stream"`
			Status      string `json:"status"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if json.Unmarshal([]byte(line), &msg) == nil {
			switch {
			case msg.Stream != "":
				io.WriteString(logsWriter, msg.Stream)
			case msg.Status != "":
				io.WriteString(logsWriter, msg.Status+"\n")
			}
			if msg.Error != "" {
				streamErr = msg.Error
			} else if msg.ErrorDetail.Message != "" {
				streamErr = msg.ErrorDetail.Message
			}
		} else {
			io.WriteString(logsWriter, line+"\n")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if streamErr != "" {
		return fmt.Errorf("%s", streamErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Resource naming. Per-preview names key on the previewID (a 24-char deployment
// id) with short prefixes so they stay under AWS's 32-char ALB/target-group
// limit; shared names key on the org.
// ---------------------------------------------------------------------------

func ecrRepositoryName(orgID, previewID string) string { return "ecr-" + orgID + "-" + previewID }
func localImageName(orgID, previewID, tag string) string {
	return orgID + "-" + previewID + ":" + tag
}
func clusterName(orgID string) string              { return "ecs-" + orgID }
func executionRoleName(orgID string) string        { return "pv-ecs-exec-" + orgID }
func taskSecurityGroupName(orgID string) string    { return "pv-tasksg-" + orgID }
func taskDefinitionFamily(previewID string) string { return "pv-td-" + previewID }
func containerName(previewID string) string        { return "pv-c-" + previewID }
func targetGroupName(previewID string) string      { return "pv-tg-" + previewID } // <=32 chars
func ecsServiceName(previewID string) string       { return "pv-es-" + previewID }
func logGroupName(orgID, previewID string) string  { return "pv/" + orgID + "/" + previewID }

// albName is the shared, one-per-org+region ALB name. It must be <=32 chars and
// alphanumeric/hyphen, so the (variable-length) org id is hashed to fixed width.
func albName(orgID string) string              { return "pv-alb-" + orgHash(orgID) } // 7+16 = 23 chars
func albSecurityGroupName(orgID string) string { return "pv-albsg-" + orgID }

func orgHash(orgID string) string {
	sum := sha256.Sum256([]byte(orgID))
	return hex.EncodeToString(sum[:])[:16]
}
