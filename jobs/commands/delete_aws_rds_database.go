package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"io"
)

type DeleteAwsRdsDatabase struct {
}

func (d *DeleteAwsRdsDatabase) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	io.WriteString(logsWriter, fmt.Sprintf("Deleting RDS Database\n"))

	rdsClient, err := cloud_api_clients.GetRdsClient(parameters)
	if err != nil {
		return parameters, err
	}

	rdsInstanceIdentifier, err := getRdsDBInstanceIdentifier(parameters)
	if err != nil {
		return parameters, err
	}

	describeDBInstances, err := rdsClient.DescribeDBInstances(context.TODO(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
	})

	if err != nil {
		return parameters, err
	}

	if len(describeDBInstances.DBInstances) == 0 {
		return parameters, fmt.Errorf("RDS instance doesn't exists")
	}

	if describeDBInstances.DBInstances[0].DeletionProtection != nil && *describeDBInstances.DBInstances[0].DeletionProtection {
		_, err = rdsClient.ModifyDBInstance(context.TODO(), &rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
			DeletionProtection:   aws.Bool(false),
		})

		if err != nil {
			return parameters, err
		}
	}

	_, err = rdsClient.DeleteDBInstance(context.TODO(), &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
		SkipFinalSnapshot:    aws.Bool(true),
	})

	if err != nil {
		return parameters, err
	}

	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return parameters, err
	}

	//update deployment to deleted
	updateDeploymentsPipeline.Add(updateDeploymentsKey, deployments.UpdateDeploymentDtoV1{
		ID:            deploymentID,
		DeletionState: deployment_enums.DeletionDone,
	})

	return parameters, nil
}
