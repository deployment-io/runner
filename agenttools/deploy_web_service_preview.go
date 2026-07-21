package agenttools

// deploy_web_service_preview.go is the arg-based, self-contained web-service preview
// deploy core — the twin of deploy_static.go's DeployStaticSitePreview. Where the
// static preview stands up S3 + CloudFront, a web-service preview builds/pushes a
// container image, runs it as a Fargate service behind a target group, and (Cw1b)
// fronts it with a shared ALB + a per-preview CloudFront distribution.
//
// Like the static core it is invoked in-process by the (future, Cw2)
// deploy_web_service_preview MCP tool handler and is deliberately NOT the
// production DeployAwsWebService command (a 1348-line monolith welded to the job
// `parameters` map with a per-service ALB). It references that monolith's SDK
// calls but is written fresh so the preview can use a shared, cost-optimized
// ingress. The AWS primitives live in the leaf utils/aws_utils package.
//
// Cw1a (this slice) covers the ECS half: image build+push, cluster, execution
// role, VPC/subnets, task security group, task definition, target group, and the
// Fargate service. Cw1b appends the ingress half (shared ALB + header-routed
// listener rule + CloudFront) and populates the URL.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
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
// Cw1a scope: this returns after the ECS service is created/updated and (best
// effort) a task reaches RUNNING. The shared ALB + listener rule + CloudFront
// that make the service publicly reachable — and populate the URL — arrive in
// Cw1b.
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

	// 8. Create/update the Fargate service, wired to the target group.
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

	// Cw1a confirmation: without an ALB the stable waiter can't run, so poll for a
	// RUNNING task (best effort — a slow first image pull just returns false).
	if in.SkipServiceStableWait {
		running, werr := aws_utils.WaitForRunningTask(in.EcsClient, clusterArn, ecsServiceName(in.PreviewID), 5*time.Minute)
		if werr != nil {
			return res, fmt.Errorf("await running task: %w", werr)
		}
		res.TaskRunning = running
		if running {
			io.WriteString(logsWriter, "Preview task is running and registered with the target group\n")
		} else {
			io.WriteString(logsWriter, "Preview service created; task not yet running (still pulling/starting)\n")
		}
	}

	return res, nil
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
