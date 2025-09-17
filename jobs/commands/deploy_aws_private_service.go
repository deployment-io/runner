package commands

import (
	"io"

	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
)

type DeployAwsPrivateService struct {
}

func (d *DeployAwsPrivateService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
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
	err = addIngressRuleToDefaultVpcSecurityGroupForPortIfNeeded(parameters, ec2Client)
	if err != nil {
		return parameters, err
	}

	ecsClient, err := cloud_api_clients.GetEcsClient(parameters)
	if err != nil {
		return parameters, err
	}
	taskDefinitionArn, err := registerTaskDefinition(parameters, ecsClient)
	if err != nil {
		return parameters, err
	}
	ecsClusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return parameters, err
	}
	_, shouldUpdateService, err := createEcsServiceIfNeeded(parameters, ecsClient, ecsClusterArn, "", taskDefinitionArn, logsWriter)
	if err != nil {
		return parameters, err
	}
	if shouldUpdateService {
		err = updateEcsService(parameters, ecsClient, ecsClusterArn, taskDefinitionArn, logsWriter)
		if err != nil {
			return parameters, err
		}
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
