package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdsTypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/deployment_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/rds_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/utils"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	runnerUtils "github.com/deployment-io/deployment-runner/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"time"
)

type DeployAwsRdsDatabase struct {
}

func getDBSubnetGroupName(parameters map[string]interface{}) (string, error) {
	//db-subnet-group-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("db-subnet-group-%s", organizationID), nil
}

func getRdsDBInstanceIdentifier(parameters map[string]interface{}) (string, error) {
	//rds-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("rds-%s", deploymentID), nil
}

func createDBSubnetGroupIfNeeded(parameters map[string]interface{}, rdsClient *rds.Client, logsWriter io.Writer) (string, error) {

	subnetGroupName, err := getDBSubnetGroupName(parameters)
	if err != nil {
		return "", err
	}

	describeDBSubnetGroupsOutput, err := rdsClient.DescribeDBSubnetGroups(context.TODO(), &rds.DescribeDBSubnetGroupsInput{
		DBSubnetGroupName: aws.String(subnetGroupName),
	})

	if describeDBSubnetGroupsOutput == nil || describeDBSubnetGroupsOutput.DBSubnetGroups == nil {
		io.WriteString(logsWriter, fmt.Sprintf("Creating DB subnet group for RDS: %s\n", subnetGroupName))
		privateSubnets, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.PrivateSubnets)
		if err != nil {
			return "", err
		}
		privateSubnetsSlice, err := commandUtils.ConvertPrimitiveAToStringSlice(privateSubnets)
		if err != nil {
			return "", err
		}
		_, err = rdsClient.CreateDBSubnetGroup(context.TODO(), &rds.CreateDBSubnetGroupInput{
			DBSubnetGroupDescription: aws.String(fmt.Sprintf("subnet group %s", subnetGroupName)),
			DBSubnetGroupName:        aws.String(subnetGroupName),
			SubnetIds:                privateSubnetsSlice,
			Tags: []rdsTypes.Tag{
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
		})

		if err != nil {
			return "", err
		}
	}

	return subnetGroupName, nil
}

func waitTillRdsAvailable(rdsClient *rds.Client, rdsDatabaseArn, rdsInstanceIdentifier string, useMultiAZ bool,
	logsWriter io.Writer) error {
	waiter := rds.NewDBInstanceAvailableWaiter(rdsClient)

	io.WriteString(logsWriter, fmt.Sprintf("Waiting for RDS database to be available: %s\n", rdsDatabaseArn))

	maxWaitDuration := time.Minute * 20

	if useMultiAZ {
		maxWaitDuration = time.Minute * 30
	}

	err := waiter.Wait(context.TODO(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
	}, maxWaitDuration)

	if err != nil {
		return err
	}
	return nil
}

func syncRds(parameters map[string]interface{}, rdsClient *rds.Client, rdsInstanceIdentifier,
	masterUserPassword, masterUserName string, logsWriter io.Writer, syncOnError bool) error {
	describeDBInstancesOutput, err := rdsClient.DescribeDBInstances(context.TODO(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
	})

	if err != nil {
		return err
	}

	if len(describeDBInstancesOutput.DBInstances) == 0 {
		err = fmt.Errorf("DB instance not available")
		return err
	}

	port := aws.ToInt32(describeDBInstancesOutput.DBInstances[0].Endpoint.Port)

	jobs.SetParameterValue[int64](parameters, parameters_enums.Port, int64(port))

	ec2Client, err := cloud_api_clients.GetEC2Client(parameters)
	if err != nil {
		return err
	}
	err = addIngressRuleToDefaultVpcSecurityGroupForPortIfNeeded(parameters, ec2Client)
	if err != nil {
		return err
	}

	endpointAddress := aws.ToString(describeDBInstancesOutput.DBInstances[0].Endpoint.Address)
	endpointPort := port
	engineVersion := aws.ToString(describeDBInstancesOutput.DBInstances[0].EngineVersion)

	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return err
	}

	rdsDatabaseArn := aws.ToString(describeDBInstancesOutput.DBInstances[0].DBInstanceArn)

	allocatedStorage := aws.ToInt32(describeDBInstancesOutput.DBInstances[0].AllocatedStorage)
	maxAllocatedStorage := aws.ToInt32(describeDBInstancesOutput.DBInstances[0].MaxAllocatedStorage)
	dbInstanceClass := aws.ToString(describeDBInstancesOutput.DBInstances[0].DBInstanceClass)

	updateDeploymentDtoV1 := deployments.UpdateDeploymentDtoV1{
		ID:               deploymentID,
		DnsName:          endpointAddress,
		Port:             endpointPort,
		RdsDatabaseArn:   rdsDatabaseArn,
		RdsEngineVersion: engineVersion,
	}

	if len(masterUserPassword) > 0 && len(masterUserName) > 0 {
		updateDeploymentDtoV1.RdsUserPassword = masterUserPassword
		updateDeploymentDtoV1.RdsUserName = masterUserName
	}

	if syncOnError {
		//sync these only on error
		updateDeploymentDtoV1.AllocatedStorage = allocatedStorage
		updateDeploymentDtoV1.MaxAllocatedStorage = maxAllocatedStorage
		updateDeploymentDtoV1.DBInstanceClass = dbInstanceClass
	}

	updateDeploymentsPipeline.Add(updateDeploymentsKey, updateDeploymentDtoV1)

	if !syncOnError {
		io.WriteString(logsWriter, fmt.Sprintf("RDS database is available at: %s:%d\n", endpointAddress, endpointPort))
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("There was an error modifying your RDS database but it's available"+
			" at: %s:%d\n", endpointAddress, endpointPort))
	}

	return nil
}

func (d *DeployAwsRdsDatabase) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	defer func() {
		//err can be nil in this case as well
		<-MarkDeploymentDone(parameters, err)
	}()
	rdsEngine, err := jobs.GetParameterValue[int64](parameters, parameters_enums.RdsEngine)
	if err != nil {
		return parameters, err
	}
	engine := rds_enums.Engine(rdsEngine)

	io.WriteString(logsWriter, fmt.Sprintf("Deploying RDS Database for %s\n", engine.Name()))

	//check and add policy for AWS RDS deployment
	runnerData := runnerUtils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return parameters, err
	}
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsRdsDeployment,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
	if err != nil {
		return parameters, err
	}

	rdsClient, err := cloud_api_clients.GetRdsClient(parameters)
	if err != nil {
		return parameters, err
	}

	rdsInstanceIdentifier, err := getRdsDBInstanceIdentifier(parameters)
	if err != nil {
		return parameters, err
	}

	rdsInstanceType, err := jobs.GetParameterValue[int64](parameters, parameters_enums.CpuMemoryRDSInstance)
	if err != nil {
		return parameters, err
	}

	rdsInstance := deployment_enums.CpuMemoryRDSInstance(rdsInstanceType)

	allocatedStorage, err := jobs.GetParameterValue[int64](parameters, parameters_enums.AllocatedStorage)
	if err != nil {
		return parameters, err
	}

	maxAllocatedStorage, err := jobs.GetParameterValue[int64](parameters, parameters_enums.MaxAllocatedStorage)
	if err != nil {
		return parameters, err
	}

	useDeletionProtection, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.UseDeletionProtection)

	dbInstancesOutput, _ := rdsClient.DescribeDBInstances(context.TODO(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
	})

	if dbInstancesOutput != nil && len(dbInstancesOutput.DBInstances) > 0 {
		applyRdsChangesImmediately, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.ApplyRdsChangesImmediately)
		var userName, userPassword string
		shouldUpdatePassword, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.ShouldUpdatePassword)
		if shouldUpdatePassword {
			userName = aws.ToString(dbInstancesOutput.DBInstances[0].MasterUsername)
			userPassword, err = utils.GenerateRandomString(15)
			if err != nil {
				return parameters, err
			}
		}
		//modify
		var modifyDBInstanceOutput *rds.ModifyDBInstanceOutput
		modifyDBInstanceInput := &rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(rdsInstanceIdentifier),
			AllocatedStorage:     aws.Int32(int32(allocatedStorage)),
			ApplyImmediately:     aws.Bool(applyRdsChangesImmediately),
			DBInstanceClass:      aws.String(rdsInstance.Instance()),
			MaxAllocatedStorage:  aws.Int32(int32(maxAllocatedStorage)),
			DeletionProtection:   aws.Bool(useDeletionProtection),
		}
		if len(userPassword) > 0 {
			modifyDBInstanceInput.MasterUserPassword = aws.String(userPassword)
		}
		modifyDBInstanceOutput, err = rdsClient.ModifyDBInstance(context.TODO(), modifyDBInstanceInput)
		if err != nil {
			_ = syncRds(parameters, rdsClient, rdsInstanceIdentifier, "", "", logsWriter,
				true)
			return parameters, err
		}
		if modifyDBInstanceOutput.DBInstance != nil {
			rdsDBArn := aws.ToString(modifyDBInstanceOutput.DBInstance.DBInstanceArn)
			if len(rdsDBArn) > 0 {
				err = waitTillRdsAvailable(rdsClient, rdsDBArn, rdsInstanceIdentifier, false, logsWriter)
				if err != nil {
					return parameters, err
				}
				err = syncRds(parameters, rdsClient, rdsInstanceIdentifier, userPassword, userName,
					logsWriter, false)
				if err != nil {
					return parameters, err
				}
			} else {
				err = fmt.Errorf("RDS DB Instance %s is not available", rdsInstanceIdentifier)
				return parameters, err
			}
		} else {
			err = fmt.Errorf("RDS DB Instance %s is not available", rdsInstanceIdentifier)
			return parameters, err
		}

		return parameters, nil
	}

	dbSubnetGroupName, err := createDBSubnetGroupIfNeeded(parameters, rdsClient, logsWriter)
	if err != nil {
		return parameters, err
	}

	useMultiAZ, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.UseMultiAz)

	masterUserPassword, err := utils.GenerateRandomString(15)
	if err != nil {
		return parameters, err
	}

	masterUserName, err := utils.GenerateRandomString(7)
	if err != nil {
		return parameters, err
	}
	masterUserName = "p" + masterUserName

	createDBInstanceOutput, err := rdsClient.CreateDBInstance(context.TODO(), &rds.CreateDBInstanceInput{
		DBInstanceClass:           aws.String(rdsInstance.Instance()),
		DBInstanceIdentifier:      aws.String(rdsInstanceIdentifier), //get from name
		DBSubnetGroupName:         aws.String(dbSubnetGroupName),
		EnablePerformanceInsights: aws.Bool(engine.EnablePerformanceInsights()),
		Engine:                    aws.String(engine.String()),
		AllocatedStorage:          aws.Int32(int32(allocatedStorage)),
		AutoMinorVersionUpgrade:   aws.Bool(true),
		CopyTagsToSnapshot:        aws.Bool(true),
		DeletionProtection:        aws.Bool(useDeletionProtection),
		EngineLifecycleSupport:    engine.GetEngineLifecycleSupport(),
		LicenseModel:              engine.GetLicenseModel(),
		ManageMasterUserPassword:  aws.Bool(false),
		MasterUserPassword:        aws.String(masterUserPassword),
		MasterUsername:            aws.String(masterUserName),
		MaxAllocatedStorage:       aws.Int32(int32(maxAllocatedStorage)),
		MultiAZ:                   aws.Bool(useMultiAZ),
		PubliclyAccessible:        aws.Bool(false),
		StorageEncrypted:          aws.Bool(rdsInstance.SupportsEncryption()),
		StorageType:               aws.String("gp3"),
		Tags: []rdsTypes.Tag{
			{
				Key:   aws.String("created by"),
				Value: aws.String("deployment.io"),
			},
		},

		//EnableCloudwatchLogsExports:        nil,
		//EngineVersion:                      nil, // for now we keep it default
		//Iops:                               nil,
		//MasterUserSecretKmsKeyId:           nil,
		//MonitoringInterval:                 nil,
		//MonitoringRoleArn:                  nil,
		//NetworkType:                        nil,
		//OptionGroupName:                    nil,
		//PerformanceInsightsKMSKeyId:        nil,
		//PerformanceInsightsRetentionPeriod: nil,
		//Port:                       nil,
		//PreferredBackupWindow:      nil,
		//PreferredMaintenanceWindow: nil,
		//ProcessorFeatures:        nil,
		//TdeCredentialArn:      nil,
		//TdeCredentialPassword: nil,
		//Timezone: nil,
		//VpcSecurityGroupIds: nil,
		//Iops:                      aws.Int32(3000),
		//For allocated storage between 20-399 GiB, a baseline of 3,000 IOPS is included in General Purpose SSD (gp3) storage volumes.
		//When allocated storage is 400 GiB or greater, a baseline of 12,000 IOPS is included
		//StorageThroughput:        aws.Int32(125),
		//For allocated storage between 20-399 GiB, a baseline storage throughput of 125 MiBps is included in General Purpose SSD (gp3) storage volumes.
		//When allocated storage is 400 GiB or greater, a baseline storage throughput of 500 MiBps is included.
	})

	if err != nil {
		return parameters, err
	}

	rdsDatabaseArn := aws.ToString(createDBInstanceOutput.DBInstance.DBInstanceArn)

	err = waitTillRdsAvailable(rdsClient, rdsDatabaseArn, rdsInstanceIdentifier, useMultiAZ, logsWriter)
	if err != nil {
		return parameters, err
	}

	err = syncRds(parameters, rdsClient, rdsInstanceIdentifier, masterUserPassword, masterUserName, logsWriter,
		false)
	if err != nil {
		return parameters, err
	}

	return parameters, nil
}
