package main

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/entrypoints/common"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers/cloudwatch"
	"log"
)

var clientCertPem, clientKeyPem string

func main() {
	service, organizationId, token, region, dockerImage, _, _, _, awsAccountID := getEnvironmentForAws()
	stsClient, err := cloud_api_clients.GetStsClient(region)
	if err != nil {
		log.Fatal(err)
	}
	if len(awsAccountID) > 0 {
		//aws case - check account validity
		getCallerIdentityOutput, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
		if err != nil {
			log.Fatal(err)
		}
		if awsAccountID != aws.ToString(getCallerIdentityOutput.Account) {
			log.Fatalf("invalid AWS account ID")
		}
	} else {
		log.Fatal("error getting AWS account ID from AWS environment")
	}
	runnerMode := runner_enums.AwsEcs
	targetCloud := runner_enums.AwsCloud
	client.Connect(client.Options{
		Service: service,
		//OrganizationID:        organizationId,
		Token:                 token,
		ClientCertPem:         clientCertPem,
		ClientKeyPem:          clientKeyPem,
		DockerImage:           dockerImage,
		Region:                region,
		CloudAccountID:        awsAccountID,
		BlockTillFirstConnect: false,
		RunnerMode:            runnerMode,
		TargetCloud:           targetCloud,
	}, organizationId)
	common.Init()
	archEnum, osType := common.GetRuntimeEnvironment()
	utils.RunnerData.Set(region, awsAccountID, archEnum, osType, runnerMode, targetCloud)
	if len(awsAccountID) > 0 {
		//add permissions for sending logs to cloudwatch
		err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsLogs, osType.String(),
			archEnum.String(), organizationId, region, runnerMode, targetCloud)
		if err != nil {
			log.Fatal(err)
		}
	}
	go setMissingLogGroupRetention(organizationId, region)
	c := client.Get()
	common.GetAndRunJobs(c, runnerMode, organizationId)
}

// setMissingLogGroupRetention gives the organization's existing log groups without a retention one.
// Errors are logged; the sweep runs again at the next runner start.
func setMissingLogGroupRetention(organizationId, region string) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Printf("log group retention: error loading config for cloudwatch client: %s", err)
		return
	}
	cloudwatchClient := cloudwatchlogs.NewFromConfig(cfg, func(options *cloudwatchlogs.Options) {
		options.Region = region
	})
	cloudwatch.SetMissingRetention(context.TODO(), cloudwatchClient, organizationId+"/")
}
