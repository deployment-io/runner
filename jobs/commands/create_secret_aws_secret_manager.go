package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	secretsmanager_types "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils"
	"io"
)

type CreateSecretAwsSecretManager struct {
}

func (c *CreateSecretAwsSecretManager) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		if err != nil {
			<-MarkBuildDone(parameters, err)
		}
	}()
	//add iam policy for secret manager
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return parameters, err
	}
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsSecretsManager,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
	if err != nil {
		return parameters, err
	}

	secretName, err := jobs.GetParameterValue[string](parameters, parameters_enums.SecretName)
	if err != nil {
		return parameters, err
	}

	secretValue, err := jobs.GetParameterValue[string](parameters, parameters_enums.SecretValue)
	if err != nil {
		return parameters, err
	}

	secretsManagerClient, err := cloud_api_clients.GetSecretsManagerClient(parameters)

	//check if secret exists
	getSecretValueOutput, err := secretsManagerClient.GetSecretValue(context.TODO(), &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})

	if err == nil && getSecretValueOutput != nil {
		//secret already saved with that name, return
		return parameters, err
	}

	io.WriteString(logsWriter, fmt.Sprintf("Creating secret for registry credential.\n"))

	_, err = secretsManagerClient.CreateSecret(context.TODO(), &secretsmanager.CreateSecretInput{
		Name: aws.String(secretName),
		//AddReplicaRegions:           nil,
		//ClientRequestToken:          nil,
		//Description:                 nil,
		//ForceOverwriteReplicaSecret: false,
		//KmsKeyId:                    nil,
		//SecretBinary:                nil,
		SecretString: aws.String(secretValue),
		Tags: []secretsmanager_types.Tag{
			{
				Key:   aws.String("created by"),
				Value: aws.String("deployment.io"),
			},
		},
	})

	if err != nil {
		return parameters, err
	}

	return parameters, err
}
