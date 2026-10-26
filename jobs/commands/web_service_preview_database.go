package commands

// web_service_preview_database.go deploys a backend preview's own throwaway
// database: an allowlisted official image run as the Task's "<service>-db" private
// service, with DeployAwsPrivateService's steps on a parameters map of its own, in
// the Task's preview stack security group. The API preview then runs against it by
// its DNS name, and the person-set preview migrate command, if any, with it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/task_previews"
	"github.com/deployment-io/deployment-runner/agenttools"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
)

// A preview database's fixed credentials. It is empty, has no storage and only its
// Task's preview can reach it, so there is no secret to keep: a generated password
// would have to be stored and handed back on every re-deploy for nothing.
const (
	previewDatabaseUser     = "preview"
	previewDatabasePassword = "preview"
	previewDatabaseName     = "preview"
)

// previewDatabaseInitVariables are the variables the engine's official image
// creates its user and database from.
func previewDatabaseInitVariables(engine string) map[string]string {
	switch engine {
	case task_previews.PreviewDatabaseEnginePostgres:
		return map[string]string{
			"POSTGRES_USER": previewDatabaseUser, "POSTGRES_PASSWORD": previewDatabasePassword, "POSTGRES_DB": previewDatabaseName,
		}
	case task_previews.PreviewDatabaseEngineMySQL:
		return map[string]string{
			"MYSQL_USER": previewDatabaseUser, "MYSQL_PASSWORD": previewDatabasePassword, "MYSQL_DATABASE": previewDatabaseName,
			"MYSQL_RANDOM_ROOT_PASSWORD": "yes",
		}
	case task_previews.PreviewDatabaseEngineMongoDB:
		// No user: MongoDB runs as a single-node replica set (transactions need
		// one, for every driver), and auth on a replica set needs a shared key
		// file. It is empty, throwaway and reachable only from its Task's stack.
		return map[string]string{}
	}
	return nil
}

// mongoReplicaSetName is the preview MongoDB's replica set.
const mongoReplicaSetName = "rs0"

// previewDatabaseURL is the database's address as the variable name holds it:
// a URL, except a MySQL *_DSN variable, which gets the Go MySQL driver's
// user:password@tcp(host:port)/database form. MongoDB is reached directly
// (directConnection): the driver must not look up the replica set's member,
// which is named after the container.
func previewDatabaseURL(engine, name, host string, port int64) string {
	credentials := previewDatabaseUser + ":" + previewDatabasePassword
	switch engine {
	case task_previews.PreviewDatabaseEngineMySQL:
		if strings.HasSuffix(strings.ToUpper(name), "DSN") {
			return fmt.Sprintf("%s@tcp(%s:%d)/%s", credentials, host, port, previewDatabaseName)
		}
		return fmt.Sprintf("mysql://%s@%s:%d/%s", credentials, host, port, previewDatabaseName)
	case task_previews.PreviewDatabaseEngineMongoDB:
		return fmt.Sprintf("mongodb://%s:%d/%s?directConnection=true", host, port, previewDatabaseName)
	}
	return fmt.Sprintf("postgresql://%s@%s:%d/%s", credentials, host, port, previewDatabaseName)
}

// previewDatabaseVariableValues are the values of the API's database variables: a
// URL variable gets the URL, a split variable its part. A name that is neither (a
// person's setting may name any) gets the URL.
func previewDatabaseVariableValues(engine, host string, port int64, names []string) map[string]string {
	user, password := previewDatabaseUser, previewDatabasePassword
	if engine == task_previews.PreviewDatabaseEngineMongoDB {
		// MongoDB has no user (see previewDatabaseInitVariables).
		user, password = "", ""
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		kind, part := task_previews.ClassifyDatabaseVariable(name)
		if kind != task_previews.DatabaseSplitVariable {
			values[name] = previewDatabaseURL(engine, name, host, port)
			continue
		}
		switch part {
		case task_previews.DatabasePartHost:
			values[name] = host
		case task_previews.DatabasePartPort:
			values[name] = strconv.FormatInt(port, 10)
		case task_previews.DatabasePartUser:
			values[name] = user
		case task_previews.DatabasePartPassword:
			values[name] = password
		case task_previews.DatabasePartName:
			values[name] = previewDatabaseName
		}
	}
	return values
}

// previewDatabaseParametersInput is what a preview database's parameters map is
// built from.
type previewDatabaseParametersInput struct {
	OrganizationIDNamespace string
	OrganizationIdFromJob   string
	Region                  int64
	Database                agenttools.PreviewDatabaseDeploy
	Settings                task_previews.WebServicePreviewSettingsReplyV1
	// BuildDir is the fresh empty directory under /tmp/<OrganizationIDNamespace>/
	// MarkDeploymentDone removes when the deploy ends.
	BuildDir         string
	SecurityGroupIDs []string
}

// buildPreviewDatabaseParameters is the parameters map a preview database deploys
// with — a new map each time. Only an allowlisted engine, version and image is
// accepted, and only in the namespace deployment-server expects.
func buildPreviewDatabaseParameters(in previewDatabaseParametersInput) (map[string]interface{}, error) {
	db := in.Database
	engine, ok := task_previews.LookupPreviewDatabaseEngine(db.Engine)
	if !ok {
		return nil, fmt.Errorf("%q isn't a preview database engine", db.Engine)
	}
	image, err := engine.Image(db.Version)
	if err != nil {
		return nil, err
	}
	if image != db.Image {
		return nil, fmt.Errorf("the preview database's image %s isn't the allowlisted %s", db.Image, image)
	}
	if len(db.PreviewID) < 4 || len(in.OrganizationIdFromJob) < 4 {
		return nil, errors.New("the preview database has no usable preview or organization id")
	}
	if len(in.SecurityGroupIDs) == 0 {
		return nil, errors.New("the preview database has no security group")
	}
	env, err := webPreviewEnvironmentVariables(previewDatabaseInitVariables(engine.Engine))
	if err != nil {
		return nil, err
	}
	p := map[string]interface{}{}
	jobs.SetParameterValue(p, parameters_enums.OrganizationIDNamespace, in.OrganizationIDNamespace)
	jobs.SetParameterValue(p, parameters_enums.OrganizationIdFromJob, in.OrganizationIdFromJob)
	jobs.SetParameterValue(p, parameters_enums.DeploymentID, db.PreviewID)
	jobs.SetParameterValue(p, parameters_enums.DeploymentName, webPreviewDeploymentName(db.ServiceKey))
	jobs.SetParameterValue(p, parameters_enums.EnvironmentName, webPreviewEnvironmentName)
	jobs.SetParameterValue(p, parameters_enums.EnvironmentID, in.OrganizationIdFromJob)
	jobs.SetParameterValue(p, parameters_enums.BuildID, filepath.Base(in.BuildDir))
	jobs.SetParameterValue(p, parameters_enums.Region, in.Region)
	jobs.SetParameterValue(p, parameters_enums.Port, engine.Port)
	jobs.SetParameterValue(p, parameters_enums.Cpu, engine.Cpu)
	jobs.SetParameterValue(p, parameters_enums.Memory, engine.Memory)
	network := in.Settings.Network
	jobs.SetParameterValue(p, parameters_enums.VpcID, network.VpcID)
	jobs.SetParameterValue(p, parameters_enums.VpcCidr, network.VpcCidr)
	jobs.SetParameterValue(p, parameters_enums.PrivateSubnets, stringsToPrimitiveA(network.PrivateSubnets))
	jobs.SetParameterValue(p, parameters_enums.EcsClusterArn, network.EcsClusterArn)
	jobs.SetParameterValue(p, parameters_enums.EcsTaskExecutionRoleArn, network.EcsTaskExecutionRoleArn)
	jobs.SetParameterValue(p, parameters_enums.EnvironmentVariables, env)
	jobs.SetParameterValue(p, parameters_enums.DockerRepositoryUriWithTag, image)
	jobs.SetParameterValue(p, parameters_enums.TaskSecurityGroupIds, stringsToPrimitiveA(in.SecurityGroupIDs))

	namespace, err := getNamespaceName(p)
	if err != nil {
		return nil, err
	}
	if namespace != in.Settings.NamespaceName {
		return nil, fmt.Errorf("The preview namespace doesn't match the server's (%s vs %s).", namespace, in.Settings.NamespaceName)
	}
	return p, nil
}

// previewDatabaseHost is the database's DNS name: its Service Connect alias in
// the preview namespace, which DeployAwsPrivateService registers.
func previewDatabaseHost(parameters map[string]interface{}, namespaceName string) (string, error) {
	alias, err := getInternalDnsName(parameters)
	if err != nil {
		return "", err
	}
	return alias + "." + namespaceName, nil
}

// webPreviewDatabase is a deployed preview database, as the API preview needs it.
type webPreviewDatabase struct {
	host string
}

// setWebPreviewDatabaseParameters points the API preview's parameters at its
// database: Preview configuration's variables plus the database's (a person's
// "use" setting wins over Preview configuration for its names), and the
// person-set preview migrate command, run against it first. Never the production
// pre-deploy command. (The stack group is set for every backend preview by the
// deployer.)
func setWebPreviewDatabaseParameters(p map[string]interface{}, in agenttools.WebServicePreviewDeployInput, db webPreviewDatabase) error {
	variables := make(map[string]string, len(in.Settings.Variables)+len(in.Database.Variables))
	for k, v := range in.Settings.Variables {
		variables[k] = v
	}
	for k, v := range previewDatabaseVariableValues(in.Database.Engine, db.host, in.Database.Port, in.Database.Variables) {
		variables[k] = v
	}
	env, err := webPreviewEnvironmentVariables(variables)
	if err != nil {
		return err
	}
	jobs.SetParameterValue(p, parameters_enums.EnvironmentVariables, env)
	if in.Database.MigrateCommand != "" {
		jobs.SetParameterValue(p, parameters_enums.PreDeployCommand, in.Database.MigrateCommand)
	}
	return nil
}

// previewDatabaseECSAPI is the part of *ecs.Client the re-deploy check uses.
type previewDatabaseECSAPI interface {
	DescribeServices(ctx context.Context, params *ecs.DescribeServicesInput, optFns ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	DescribeTaskDefinition(ctx context.Context, params *ecs.DescribeTaskDefinitionInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
}

// previewDatabaseUnchanged reports whether the database's ECS service exists, runs
// in the stack group alone, has its DNS registration and its task definition runs
// image: then it is kept, with its data and schema. One in another group (the
// stack group was removed and made again) or without its DNS registration (it
// failed when the service was created) is redeployed, which moves it onto the
// current group and registers its DNS name.
func previewDatabaseUnchanged(ecsClient previewDatabaseECSAPI, clusterArn, serviceName, image, securityGroupID string) (bool, error) {
	services, err := ecsClient.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterArn),
		Services: []string{serviceName},
	})
	if err != nil {
		return false, fmt.Errorf("describe the preview database's service: %w", err)
	}
	if len(services.Services) == 0 || aws.ToString(services.Services[0].Status) != "ACTIVE" {
		return false, nil
	}
	if groups := serviceSecurityGroupIDs(&services.Services[0]); len(groups) != 1 || groups[0] != securityGroupID {
		return false, nil
	}
	if len(services.Services[0].ServiceRegistries) == 0 {
		return false, nil
	}
	taskDefinition := aws.ToString(services.Services[0].TaskDefinition)
	if taskDefinition == "" {
		return false, nil
	}
	described, err := ecsClient.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(taskDefinition)})
	if err != nil {
		return false, fmt.Errorf("describe the preview database's task definition: %w", err)
	}
	if described.TaskDefinition == nil {
		return false, nil
	}
	for _, c := range described.TaskDefinition.ContainerDefinitions {
		if aws.ToString(c.Image) == image {
			return true, nil
		}
	}
	return false, nil
}

// deployWebPreviewDatabase is webPreviewSteps.database in production: the
// database in the Task's stack group (made by the deployer), deployed unless it
// already runs the same image, and its DNS name.
func deployWebPreviewDatabase(stackGroupID string, apiParameters map[string]interface{}, in agenttools.WebServicePreviewDeployInput) (webPreviewDatabase, error) {
	parameters, err := newPreviewDatabaseParameters(apiParameters, in, stackGroupID)
	if err != nil {
		return webPreviewDatabase{}, err
	}
	buildDir, _ := commandUtils.GetRepositoryDirectoryPath(parameters)
	// The deploy removes it when it ends; a kept database never deploys.
	defer os.RemoveAll(buildDir)
	if err = deployPreviewDatabaseIfChanged(parameters, in.Database.Engine, in.Database.Image, stackGroupID, in.Logs); err != nil {
		return webPreviewDatabase{}, err
	}
	host, err := previewDatabaseHost(parameters, in.Settings.NamespaceName)
	if err != nil {
		return webPreviewDatabase{}, err
	}
	return webPreviewDatabase{host: host}, nil
}

// ensureWebPreviewStack is webPreviewSteps.stack in production: the Task's stack
// group in the preview's VPC, admitting port (the database's; 0 without one) from
// itself. Its id, and whether this call created it.
func ensureWebPreviewStack(taskID string, apiParameters map[string]interface{}, in agenttools.WebServicePreviewDeployInput, port int64) (string, bool, error) {
	// The EC2 actions are the web-service deploy policy's.
	organizationID, _ := jobs.GetParameterValue[string](apiParameters, parameters_enums.OrganizationIDNamespace)
	runnerData := utils.RunnerData.Get()
	if err := iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsWebServiceDeployment,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud); err != nil {
		return "", false, err
	}
	ec2Client, err := cloud_api_clients.GetEC2Client(apiParameters)
	if err != nil {
		return "", false, err
	}
	stack := previewStack{vpcID: in.Settings.Network.VpcID, taskID: taskID, port: port}
	return ensurePreviewStackSecurityGroup(ec2Client, stack, in.Logs)
}

// removeWebPreviewStack is webPreviewSteps.removeStack in production: a stack
// group this deploy created, removed again when the deploy failed before anything
// used it (the deletes find the group only through the stack's services).
func removeWebPreviewStack(apiParameters map[string]interface{}, groupID string, logsWriter io.Writer) {
	ec2Client, err := cloud_api_clients.GetEC2Client(apiParameters)
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Couldn't remove the preview's security group %s: %s\n", groupID, err))
		return
	}
	removeUnusedPreviewStackSecurityGroup(ec2Client, groupID, logsWriter)
}

// newPreviewDatabaseParameters creates the database deploy's fresh build directory
// and its parameters map, and checks the map's repository directory is that
// directory (MarkDeploymentDone removes it) before any step.
func newPreviewDatabaseParameters(apiParameters map[string]interface{}, in agenttools.WebServicePreviewDeployInput, securityGroupID string) (map[string]interface{}, error) {
	organizationIDNamespace, err := jobs.GetParameterValue[string](apiParameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return nil, err
	}
	organizationIdFromJob, _ := jobs.GetParameterValue[string](apiParameters, parameters_enums.OrganizationIdFromJob)
	region, _ := jobs.GetParameterValue[int64](apiParameters, parameters_enums.Region)
	orgDir := filepath.Join("/tmp", organizationIDNamespace)
	if err := os.MkdirAll(orgDir, 0o755); err != nil {
		return nil, err
	}
	buildDir, err := os.MkdirTemp(orgDir, "preview-db-")
	if err != nil {
		return nil, err
	}
	parameters, err := buildPreviewDatabaseParameters(previewDatabaseParametersInput{
		OrganizationIDNamespace: organizationIDNamespace,
		OrganizationIdFromJob:   organizationIdFromJob,
		Region:                  region,
		Database:                *in.Database,
		Settings:                in.Settings,
		BuildDir:                buildDir,
		SecurityGroupIDs:        []string{securityGroupID},
	})
	if err == nil {
		err = checkWebPreviewBuildDir(parameters, buildDir)
	}
	if err != nil {
		os.RemoveAll(buildDir)
		return nil, err
	}
	return parameters, nil
}

// deployPreviewDatabaseIfChanged deploys the database unless its service already
// runs image (then its data and schema are kept; the preview migrate command, if
// set, applies new migrations). A first deploy, or a changed engine or version, deploys a
// fresh, empty database.
func deployPreviewDatabaseIfChanged(parameters map[string]interface{}, engine, image, securityGroupID string, logsWriter io.Writer) error {
	ecsClient, err := getPreviewDatabaseECSClient(parameters)
	if err != nil {
		return err
	}
	clusterArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterArn)
	if err != nil {
		return err
	}
	serviceName, err := aws_utils.GetEcsServiceName(parameters)
	if err != nil {
		return err
	}
	unchanged, err := previewDatabaseUnchanged(ecsClient, clusterArn, serviceName, image, securityGroupID)
	if err != nil {
		return err
	}
	if unchanged {
		io.WriteString(logsWriter, fmt.Sprintf("Preview database: keeping the running %s and its data\n", image))
		return nil
	}
	io.WriteString(logsWriter, fmt.Sprintf("Preview database: deploying %s\n", image))
	_, err = runPreviewDatabaseDeploy(parameters, logsWriter, engine)
	return err
}

// Swapped by tests so the re-deploy decision runs without AWS.
var (
	getPreviewDatabaseECSClient = func(parameters map[string]interface{}) (previewDatabaseECSAPI, error) {
		client, err := cloud_api_clients.GetEcsClient(parameters)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	runPreviewDatabaseDeploy = deployAwsPrivateServicePreview
)
