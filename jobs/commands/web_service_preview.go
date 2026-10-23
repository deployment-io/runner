package commands

// web_service_preview.go is the deploy behind the agent's deploy_web_service_preview
// tool (agenttools): a Task's backend preview, built and deployed by the SAME commands
// a production web-service deploy runs — BuildDockerImage, UploadDockerImageToEcr and
// DeployAwsWebService's steps (without waiting for the ECS service to settle) — on a
// parameters map of its own. agenttools then puts CloudFront in front of the load
// balancer this returns.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Backend previews share one Service Connect namespace per org and region:
// getNamespaceName makes "previews-<last 4 of the org ID>" from these.
const webPreviewEnvironmentName = "previews"

// webPreviewDeploymentNameMax bounds a preview's DeploymentName, so its Service
// Connect alias (the name, "-" and 4 characters of the preview's id) stays a short
// DNS label.
const webPreviewDeploymentNameMax = 40

// webPreviewParametersInput is what a backend preview's parameters map is built from.
type webPreviewParametersInput struct {
	// Copied from the Step Job's parameters.
	OrganizationIDNamespace string
	OrganizationIdFromJob   string
	Region                  int64 // the runner region's region_enums.Type
	Now                     time.Time
	Deploy                  agenttools.WebServicePreviewDeployInput
}

// buildWebPreviewParameters is the parameters map a backend preview deploys with — a
// new map each time, never a Job's: the commands write into it, and previews deploy
// concurrently. A preview configuration variable the task definition can't carry, or
// a namespace that isn't the one deployment-server expects, is refused.
func buildWebPreviewParameters(in webPreviewParametersInput) (map[string]interface{}, error) {
	d := in.Deploy
	if len(d.PreviewID) < 4 || len(in.OrganizationIdFromJob) < 4 {
		return nil, errors.New("the backend preview has no usable preview or organization id")
	}
	env, err := webPreviewEnvironmentVariables(d.Settings.Variables)
	if err != nil {
		return nil, err
	}
	p := map[string]interface{}{}
	jobs.SetParameterValue(p, parameters_enums.OrganizationIDNamespace, in.OrganizationIDNamespace)
	jobs.SetParameterValue(p, parameters_enums.OrganizationIdFromJob, in.OrganizationIdFromJob)
	jobs.SetParameterValue(p, parameters_enums.DeploymentID, d.PreviewID)
	jobs.SetParameterValue(p, parameters_enums.DeploymentName, webPreviewDeploymentName(d.ServiceKey))
	jobs.SetParameterValue(p, parameters_enums.EnvironmentName, webPreviewEnvironmentName)
	jobs.SetParameterValue(p, parameters_enums.EnvironmentID, in.OrganizationIdFromJob)
	jobs.SetParameterValue(p, parameters_enums.BuildID, filepath.Base(d.BuildDir))
	jobs.SetParameterValue(p, parameters_enums.RepoDirectoryPath, filepath.Join(d.BuildDir, d.RootDirectory))
	jobs.SetParameterValue(p, parameters_enums.CommitHash, fmt.Sprintf("preview-%d", in.Now.Unix()))
	jobs.SetParameterValue(p, parameters_enums.DockerFilePath, webPreviewDockerFile(d.Service.DockerFilePath))
	if buildArgs, _ := webPreviewBuildArgs(d.Service.DockerBuildArgs, d.Settings.Variables); len(buildArgs) > 0 {
		jobs.SetParameterValue(p, parameters_enums.DockerBuildArgs, stringsToPrimitiveA(buildArgs))
	}
	jobs.SetParameterValue(p, parameters_enums.Region, in.Region)
	jobs.SetParameterValue(p, parameters_enums.Port, d.Service.Port)
	jobs.SetParameterValue(p, parameters_enums.HealthCheckPath, d.Service.HealthCheckPath)
	jobs.SetParameterValue(p, parameters_enums.Cpu, d.Service.Cpu)
	jobs.SetParameterValue(p, parameters_enums.Memory, d.Service.Memory)
	network := d.Settings.Network
	jobs.SetParameterValue(p, parameters_enums.VpcID, network.VpcID)
	jobs.SetParameterValue(p, parameters_enums.VpcCidr, network.VpcCidr)
	jobs.SetParameterValue(p, parameters_enums.PublicSubnets, stringsToPrimitiveA(network.PublicSubnets))
	jobs.SetParameterValue(p, parameters_enums.PrivateSubnets, stringsToPrimitiveA(network.PrivateSubnets))
	jobs.SetParameterValue(p, parameters_enums.EcsClusterArn, network.EcsClusterArn)
	jobs.SetParameterValue(p, parameters_enums.EcsTaskExecutionRoleArn, network.EcsTaskExecutionRoleArn)
	jobs.SetParameterValue(p, parameters_enums.EnvironmentVariables, env)
	setWebPreviewHandles(p, d.Existing)

	namespace, err := getNamespaceName(p)
	if err != nil {
		return nil, err
	}
	if namespace != d.Settings.NamespaceName {
		return nil, fmt.Errorf("The preview namespace doesn't match the server's (%s vs %s).", namespace, d.Settings.NamespaceName)
	}
	return p, nil
}

// setWebPreviewHandles puts the handles a prior deploy persisted into p, each only
// when set, so the commands reuse those resources instead of creating new ones.
func setWebPreviewHandles(p map[string]interface{}, existing agenttools.PreviewState) {
	if existing.EcrRepositoryUri != "" {
		jobs.SetParameterValue(p, parameters_enums.EcrRepositoryUri, existing.EcrRepositoryUri)
	}
	if existing.AlbSecurityGroupId != "" {
		jobs.SetParameterValue(p, parameters_enums.AlbSecurityGroupId, existing.AlbSecurityGroupId)
	}
	// createAlbIfNeeded reuses them only as a pair.
	if existing.LoadBalancerArn != "" && existing.TargetGroupArn != "" {
		jobs.SetParameterValue(p, parameters_enums.LoadBalancerArn, existing.LoadBalancerArn)
		jobs.SetParameterValue(p, parameters_enums.TargetGroupArn, existing.TargetGroupArn)
	}
	if existing.EcsServiceArn != "" {
		jobs.SetParameterValue(p, parameters_enums.EcsServiceArn, existing.EcsServiceArn)
	}
}

// webPreviewDeploymentName is a preview's DeploymentName: "pv-" and its service key,
// lower-case, every character outside [a-z0-9-] replaced by "-", at most
// webPreviewDeploymentNameMax characters.
func webPreviewDeploymentName(serviceKey string) string {
	name := []byte("pv-" + strings.ToLower(serviceKey))
	for i, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			name[i] = '-'
		}
	}
	if len(name) > webPreviewDeploymentNameMax {
		name = name[:webPreviewDeploymentNameMax]
	}
	return string(name)
}

// webPreviewDockerFile is the service's Dockerfile, "Dockerfile" when unset.
func webPreviewDockerFile(path string) string {
	if path == "" {
		return "Dockerfile"
	}
	return path
}

// webPreviewBuildArgs is a preview's Docker build args: the NAMES of the production
// service's build args, each with the preview configuration variable of that name as
// its value. Production's values never reach a preview build: the agent writes the
// Dockerfile, so it could serve whatever the image is built with. A name with no such
// variable, or whose variable's value has an "=" (GetDockerBuildArgs reads "K=V" with
// exactly one "=", as production does), is left out so the Dockerfile's own ARG
// default applies; left lists those names, never a value.
func webPreviewBuildArgs(productionArgs []string, variables map[string]string) (args, left []string) {
	seen := map[string]bool{}
	for _, arg := range productionArgs {
		name, _, _ := strings.Cut(arg, "=")
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		value, ok := variables[name]
		if !ok || strings.Contains(value, "=") {
			left = append(left, name)
			continue
		}
		args = append(args, name+"="+value)
	}
	return args, left
}

// webPreviewEnvironmentVariables is the preview configuration's variables as the task
// definition reads them, "K=V\n" lines sorted by key. A value with a line break can't
// be carried that way and is refused, naming the key and never the value.
func webPreviewEnvironmentVariables(variables map[string]string) (string, error) {
	keys := make([]string, 0, len(variables))
	for k := range variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if strings.Contains(variables[k], "\n") {
			return "", fmt.Errorf("Preview configuration variable %s contains a line break; backend previews can't pass it.", k)
		}
		b.WriteString(k + "=" + variables[k] + "\n")
	}
	return b.String(), nil
}

func stringsToPrimitiveA(values []string) primitive.A {
	a := make(primitive.A, 0, len(values))
	for _, v := range values {
		a = append(a, v)
	}
	return a
}

// checkWebPreviewBuildDir refuses parameters whose repository directory — what
// MarkDeploymentDone removes when a step finishes — isn't buildDir, the build copy.
func checkWebPreviewBuildDir(parameters map[string]interface{}, buildDir string) error {
	dir, err := commandUtils.GetRepositoryDirectoryPath(parameters)
	if err != nil {
		return err
	}
	if filepath.Clean(dir) != filepath.Clean(buildDir) {
		return fmt.Errorf("the backend preview's build directory %s isn't where its deploy would clean up (%s), so it isn't deployed", buildDir, dir)
	}
	return nil
}

// webPreviewStep is one command's Run.
type webPreviewStep func(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error)

// webPreviewSteps are the commands a backend preview deploy runs, in order. Tests
// replace them to deploy nowhere.
type webPreviewSteps struct {
	build  webPreviewStep
	upload webPreviewStep
	deploy webPreviewStep
	// loadBalancerDNS reads the DNS name of the load balancer the deploy created or
	// reused.
	loadBalancerDNS func(parameters map[string]interface{}) (string, error)
}

// productionWebPreviewSteps are production's own commands.
func productionWebPreviewSteps() webPreviewSteps {
	return webPreviewSteps{
		build:  (&BuildDockerImage{}).Run,
		upload: newUploadDockerImageToEcr().Run,
		deploy: deployAwsWebServicePreview,
		loadBalancerDNS: func(parameters map[string]interface{}) (string, error) {
			elbClient, err := cloud_api_clients.GetElbClient(parameters)
			if err != nil {
				return "", err
			}
			return describeLoadBalancerDNS(elbClient, parameters)
		},
	}
}

// webPreviewDeployer deploys backend previews for one Step Job.
type webPreviewDeployer struct {
	organizationIDNamespace string
	organizationIdFromJob   string
	region                  int64
	steps                   webPreviewSteps
}

// deploy is DeployWebServicePreviewDeps.Deploy: the parameters map and its checks,
// the secret files, then the image build, its upload and the web-service deploy, and
// the load balancer's DNS name.
func (d webPreviewDeployer) deploy(ctx context.Context, in agenttools.WebServicePreviewDeployInput) (agenttools.WebServicePreviewDeployOutput, error) {
	parameters, err := buildWebPreviewParameters(webPreviewParametersInput{
		OrganizationIDNamespace: d.organizationIDNamespace,
		OrganizationIdFromJob:   d.organizationIdFromJob,
		Region:                  d.region,
		Now:                     time.Now(),
		Deploy:                  in,
	})
	if err != nil {
		return agenttools.WebServicePreviewDeployOutput{}, err
	}
	// Before anything else: every step's failure, and the deploy's success, removes
	// this directory.
	if err := checkWebPreviewBuildDir(parameters, in.BuildDir); err != nil {
		return agenttools.WebServicePreviewDeployOutput{}, err
	}
	repoDir, _ := jobs.GetParameterValue[string](parameters, parameters_enums.RepoDirectoryPath)
	dockerFile, _ := jobs.GetParameterValue[string](parameters, parameters_enums.DockerFilePath)
	dockerFilePath := filepath.Join(repoDir, dockerFile)
	if info, err := os.Stat(dockerFilePath); err != nil || info.IsDir() || agenttools.CheckInside(repoDir, dockerFilePath) != nil {
		return agenttools.WebServicePreviewDeployOutput{}, fmt.Errorf("%s's Dockerfile %s isn't in its root directory %q, so its backend preview can't be built", in.Service.DeploymentName, dockerFile, in.RootDirectory)
	}
	// The Step log only (BuildLogs never reaches the agent), and names only.
	if _, left := webPreviewBuildArgs(in.Service.DockerBuildArgs, in.Settings.Variables); len(left) > 0 && in.BuildLogs != nil {
		io.WriteString(in.BuildLogs, fmt.Sprintf("Backend preview: build args left out, with no usable Preview configuration variable of the same name: %s\n", strings.Join(left, ", ")))
	}
	if in.Settings.ConfigurationSet {
		if err := agenttools.WritePreviewFiles(repoDir, in.Settings.Files); err != nil {
			return agenttools.WebServicePreviewDeployOutput{}, err
		}
	}
	return d.runSteps(ctx, parameters, in)
}

// runSteps runs the deploy's commands on parameters, stopping between them once ctx
// is cancelled (a command itself runs to its end).
func (d webPreviewDeployer) runSteps(ctx context.Context, parameters map[string]interface{}, in agenttools.WebServicePreviewDeployInput) (agenttools.WebServicePreviewDeployOutput, error) {
	for _, step := range []struct {
		run  webPreviewStep
		logs io.Writer
	}{{d.steps.build, in.BuildLogs}, {d.steps.upload, in.Logs}, {d.steps.deploy, in.Logs}} {
		if err := ctx.Err(); err != nil {
			return agenttools.WebServicePreviewDeployOutput{}, errors.New("the backend preview's deploy was cancelled")
		}
		next, err := step.run(parameters, step.logs)
		if err != nil {
			return agenttools.WebServicePreviewDeployOutput{}, agenttools.WebPreviewDeployFailed(err)
		}
		if next != nil {
			parameters = next
		}
	}
	dns, err := d.steps.loadBalancerDNS(parameters)
	if err != nil {
		return agenttools.WebServicePreviewDeployOutput{}, agenttools.WebPreviewDeployFailed(err)
	}
	return agenttools.WebServicePreviewDeployOutput{LoadBalancerDNS: dns}, nil
}

// elbLoadBalancersAPI is the part of the ELB client describeLoadBalancerDNS uses.
type elbLoadBalancersAPI interface {
	DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error)
}

// describeLoadBalancerDNS reads the DNS name of the deploy's load balancer by its name
// (getAlbName). createAlbIfNeeded never puts it in the parameters map, and returns
// without it when it reuses the load balancer, so the map can't say.
func describeLoadBalancerDNS(elbClient elbLoadBalancersAPI, parameters map[string]interface{}) (string, error) {
	name, err := getAlbName(parameters)
	if err != nil {
		return "", err
	}
	out, err := elbClient.DescribeLoadBalancers(context.TODO(), &elasticloadbalancingv2.DescribeLoadBalancersInput{
		Names: []string{name},
	})
	if err != nil {
		return "", fmt.Errorf("describe load balancer %s: %w", name, err)
	}
	if out == nil || len(out.LoadBalancers) == 0 || aws.ToString(out.LoadBalancers[0].DNSName) == "" {
		return "", fmt.Errorf("load balancer %s has no DNS name", name)
	}
	return aws.ToString(out.LoadBalancers[0].DNSName), nil
}
