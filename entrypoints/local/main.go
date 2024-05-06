package main

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/entrypoints/common"
	"github.com/deployment-io/deployment-runner/utils"
	"log"
)

var clientCertPem, clientKeyPem, version, serviceFromBuild string

func main() {
	userId, organizationId, token, service, targetCloud, err := getEnvironmentForLocal()
	if err != nil {
		log.Println(err)
		return
	}
	var cloudAccountID string
	var region string
	switch targetCloud {
	case runner_enums.AwsCloud:
		log.Println("Reading local AWS configuration")
		cfg, err := config.LoadDefaultConfig(context.TODO())
		if err != nil {
			log.Println(err)
			return
		}
		region = cfg.Region
		if len(region) == 0 {
			log.Println("error getting AWS account info. It seems that AWS API credentials are not configured. " +
				"For more information about installing the runner locally, see https://deployment.io/docs/runner-installation/local-setup/")
			return
		}
		stsClient, err := cloud_api_clients.GetStsClient(region)
		if err != nil {
			log.Println(err)
		}

		getCallerIdentityOutput, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
		if err != nil {
			log.Println(err)
			log.Println("error getting AWS account info. It seems that AWS API credentials are not configured correctly. " +
				"For more information about installing the runner locally, see https://deployment.io/docs/runner-installation/local-setup/")
			return
		}
		cloudAccountID = aws.ToString(getCallerIdentityOutput.Account)
		log.Println("Deploying to AWS cloud account:", cloudAccountID)
		log.Println("Default region:", region)

	default:
		log.Println("unsupported target cloud")
		return
	}
	runnerMode := runner_enums.LOCAL
	client.Connect(client.Options{
		Service:               service,
		OrganizationID:        organizationId,
		Token:                 token,
		ClientCertPem:         clientCertPem,
		ClientKeyPem:          clientKeyPem,
		DockerImage:           version,
		Region:                region,
		CloudAccountID:        cloudAccountID,
		BlockTillFirstConnect: false,
		RunnerMode:            runnerMode,
		TargetCloud:           targetCloud,
		UserID:                userId,
	})
	common.Init()
	archEnum, osType := common.GetRuntimeEnvironment()
	utils.RunnerData.Set(region, cloudAccountID, archEnum, osType, runnerMode, targetCloud)
	c := client.Get()
	common.GetAndRunJobs(c, runnerMode)
}
