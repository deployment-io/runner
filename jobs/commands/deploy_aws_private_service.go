package commands

import (
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
)

type DeployAwsPrivateService struct {
}

func (d *DeployAwsPrivateService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	return runAwsPrivateServiceDeploy(parameters, logsWriter, privateServiceDeployOptions{addDefaultVpcIngressRule: true})
}

// deployAwsPrivateServicePreview is DeployAwsPrivateService's steps for a Task's
// preview database: the same deploy — the VPC's DNS check, the DNS name, waiting
// for the ECS service to settle — without the ingress rule on the VPC's default
// security group, which a preview never edits (its own group admits its port),
// and updating an existing service's Service Connect port with its task
// definition (a changed engine has another port).
func deployAwsPrivateServicePreview(parameters map[string]interface{}, logsWriter io.Writer, engine string) (map[string]interface{}, error) {
	return runAwsPrivateServiceDeploy(parameters, logsWriter, previewDatabaseDeployOptions(engine))
}

// previewDatabaseDeployOptions are the preview database's private-service deploy
// options: MongoDB runs as a single-node replica set.
func previewDatabaseDeployOptions(engine string) privateServiceDeployOptions {
	return privateServiceDeployOptions{
		updateServiceConnect: true,
		mongoReplicaSet:      engine == task_previews.PreviewDatabaseEngineMongoDB,
	}
}

// privateServiceDeployOptions changes how a private service deploys.
type privateServiceDeployOptions struct {
	// addDefaultVpcIngressRule admits the port into the VPC's default security
	// group from the VPC. Production deploys set it.
	addDefaultVpcIngressRule bool
	// updateServiceConnect is ecsServiceDeployOptions.updateServiceConnect. The
	// preview database sets it.
	updateServiceConnect bool
	// mongoReplicaSet runs the container as a single-node MongoDB replica set
	// (mongoReplicaSetContainer). A MongoDB preview database sets it.
	mongoReplicaSet bool
}

// runAwsPrivateServiceDeploy is deployAwsPrivateService; tests replace it to see
// the options each entry point deploys with.
var runAwsPrivateServiceDeploy = deployAwsPrivateService

// deployAwsPrivateService runs the private-service deploy's steps with opts.
func deployAwsPrivateService(parameters map[string]interface{}, logsWriter io.Writer, opts privateServiceDeployOptions) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkDeploymentDone(parameters, err)
		}
	}()

	//check and add policy for AWS web service deployment
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return parameters, err
	}
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsWebServiceDeployment,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
	if err != nil {
		return parameters, err
	}
	ec2Client, err := cloud_api_clients.GetEC2Client(parameters)
	if err != nil {
		return parameters, err
	}
	if opts.addDefaultVpcIngressRule {
		err = addIngressRuleToDefaultVpcSecurityGroupForPortIfNeeded(parameters, ec2Client)
		if err != nil {
			return parameters, err
		}
	}

	ecsClient, err := cloud_api_clients.GetEcsClient(parameters)
	if err != nil {
		return parameters, err
	}
	var customize []func(*ecsTypes.ContainerDefinition)
	if opts.mongoReplicaSet {
		customize = append(customize, mongoReplicaSetContainer)
	}
	taskDefinitionArn, err := registerTaskDefinition(parameters, ecsClient, customize...)
	if err != nil {
		return parameters, err
	}
	ecsClusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return parameters, err
	}
	logsClient, err := cloud_api_clients.GetCloudwatchLogsClient(parameters)
	if err != nil {
		return parameters, err
	}
	// Its DNS name (registered below) resolves only once the VPC's DNS
	// attributes are on.
	ensurePrivateServiceVpcDns(parameters, ec2Client, logsWriter)
	err = deployEcsServiceAfterPreDeploy(parameters, ecsClient, logsClient, ecsClusterArn, "", taskDefinitionArn, logsWriter,
		ecsServiceDeployOptions{registerDnsName: true, updateServiceConnect: opts.updateServiceConnect})
	if err != nil {
		return parameters, err
	}

	buildID, err := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if err != nil {
		return parameters, err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}
	if !isPreview(parameters) {
		commandUtils.UpdateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
			ID:                buildID,
			TaskDefinitionArn: taskDefinitionArn,
		})
	} else {
		//build id is preview id
		previewID := buildID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:                previewID,
			TaskDefinitionArn: taskDefinitionArn,
		})
	}

	//mark build done successfully
	<-MarkDeploymentDone(parameters, nil)

	return parameters, nil
}

// mongoReplicaSetContainer makes the official MongoDB image a single-node replica
// set: started with --replSet, and initiated by its own health check, which runs
// inside the container (nothing outside can reach it) and is idempotent — it
// initiates only while rs.status() fails. ECS counts the task healthy, and the
// deploy settled, once the replica set is up.
func mongoReplicaSetContainer(container *ecsTypes.ContainerDefinition) {
	container.Command = []string{"--replSet", mongoReplicaSetName, "--bind_ip_all"}
	container.HealthCheck = &ecsTypes.HealthCheck{
		Command: []string{"CMD-SHELL",
			`mongosh --quiet --eval "try { rs.status().ok } catch (e) { rs.initiate().ok }" | grep -q 1`},
		Interval:    aws.Int32(10),
		Timeout:     aws.Int32(10),
		Retries:     aws.Int32(6),
		StartPeriod: aws.Int32(30),
	}
}
