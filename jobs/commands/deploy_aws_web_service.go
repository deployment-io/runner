package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sd_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/vpc_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner-kit/vpcs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"regexp"
	"strings"
	"time"
)

type DeployAwsWebService struct {
}

func getAlbSecurityGroupName(parameters map[string]interface{}) (string, error) {
	//security group name = albsg-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("albsg-%s", deploymentID), nil
}

func getAlbSecurityGroupIngressRuleName(parameters map[string]interface{}) (string, error) {
	//sgin-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sgin-%s", deploymentID), nil
}

func getAlbSecurityGroupEgressRuleName(parameters map[string]interface{}) (string, error) {
	//sgeg-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sgeg-%s", deploymentID), nil
}

func getIngressIpPermissionFromInternetToPort(port int64) ec2Types.IpPermission {
	return ec2Types.IpPermission{
		FromPort:   aws.Int32(int32(port)),
		IpProtocol: aws.String("tcp"),
		IpRanges: []ec2Types.IpRange{{
			CidrIp:      aws.String("0.0.0.0/0"),
			Description: aws.String("from internet"),
		}},
		ToPort: aws.Int32(int32(port)),
	}
}

func getDefaultVpcSecurityGroupIngressRuleNameForPort(parameters map[string]interface{}) (string, error) {
	//sgin-<port>
	port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sgin-%d", port), nil
}

func addIngressRuleToDefaultVpcSecurityGroupForPortIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client) error {
	vpcId, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcID)
	if err != nil {
		return err
	}
	vpcCidr, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcCidr)
	if err != nil {
		return err
	}

	defaultSecurityGroupId, err := getDefaultSecurityGroupIdForVpc(parameters, ec2Client, vpcId)
	if err != nil {
		return err
	}
	defaultVpcSecurityGroupIngressRuleNameForPort, err := getDefaultVpcSecurityGroupIngressRuleNameForPort(parameters)
	if err != nil {
		return err
	}

	describeSecurityGroupRulesOutput, err := ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultVpcSecurityGroupIngressRuleNameForPort,
				},
			},
			{
				Name: aws.String("group-id"),
				Values: []string{
					defaultSecurityGroupId,
				},
			},
		},
	})

	if err != nil {
		return err
	}

	if len(describeSecurityGroupRulesOutput.SecurityGroupRules) == 0 {
		port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
		if err != nil {
			return err
		}
		authorizeSecurityGroupIngressInput := &ec2.AuthorizeSecurityGroupIngressInput{
			CidrIp:     aws.String(vpcCidr),
			DryRun:     aws.Bool(false),
			FromPort:   aws.Int32(int32(port)),
			GroupId:    aws.String(defaultSecurityGroupId),
			IpProtocol: aws.String("tcp"),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSecurityGroupRule,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(defaultVpcSecurityGroupIngressRuleNameForPort),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("vpc-default-security-group-id"),
						Value: aws.String(defaultSecurityGroupId),
					},
				},
			}},
			ToPort: aws.Int32(int32(port)),
		}
		_, err = ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), authorizeSecurityGroupIngressInput)
		if err != nil {
			return err
		}

		describeSecurityGroupRulesOutput, err = ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
			DryRun: aws.Bool(false),
			Filters: []ec2Types.Filter{
				{
					Name: aws.String("group-id"),
					Values: []string{
						defaultSecurityGroupId,
					},
				},
			},
		})

		if err != nil {
			return err
		}

		var ingressRules []vpcs.DefaultSecurityIngressRuleDtoV1
		for _, securityGroupRule := range describeSecurityGroupRulesOutput.SecurityGroupRules {
			if !*securityGroupRule.IsEgress {
				ingressRules = append(ingressRules, vpcs.DefaultSecurityIngressRuleDtoV1{
					ID: aws.ToString(securityGroupRule.SecurityGroupRuleId),
				})
			}
		}

		region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
		if err != nil {
			return err
		}

		var organizationIdFromJob string
		organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
		if err != nil {
			return err
		}

		upsertVpcsPipeline.Add(organizationIdFromJob, vpcs.UpsertVpcDtoV1{
			Type:                        vpc_enums.AwsVpc,
			Region:                      region_enums.Type(region),
			DefaultSecurityIngressRules: ingressRules,
			DefaultSecurityGroupId:      defaultSecurityGroupId,
		})
	}
	return nil
}

func createAlbSecurityGroupIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client) (albSecurityGroupId string, err error) {
	albSecurityGroupIDFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.AlbSecurityGroupId)
	if err == nil && len(albSecurityGroupIDFromParams) > 0 {
		return albSecurityGroupIDFromParams, nil
	}

	albSecurityGroupName, err := getAlbSecurityGroupName(parameters)
	if err != nil {
		return "", err
	}

	describeSecurityGroupsOutput, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					albSecurityGroupName,
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	if len(describeSecurityGroupsOutput.SecurityGroups) > 0 {
		albSecurityGroupId = aws.ToString(describeSecurityGroupsOutput.SecurityGroups[0].GroupId)
	} else {
		vpcId, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcID)
		if err != nil {
			return "", err
		}

		createSecurityGroupInput := &ec2.CreateSecurityGroupInput{
			Description: aws.String(fmt.Sprintf("security group %s for alb", albSecurityGroupName)),
			GroupName:   aws.String(albSecurityGroupName),
			DryRun:      aws.Bool(false),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSecurityGroup,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(albSecurityGroupName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
				},
			}},
			VpcId: aws.String(vpcId),
		}

		createSecurityGroupOutput, err := ec2Client.CreateSecurityGroup(context.TODO(), createSecurityGroupInput)
		if err != nil {
			return "", err
		}
		albSecurityGroupId = aws.ToString(createSecurityGroupOutput.GroupId)
	}

	albSecurityGroupIngressRuleName, err := getAlbSecurityGroupIngressRuleName(parameters)
	if err != nil {
		return "", err
	}

	describeSecurityGroupRulesOutput, err := ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					albSecurityGroupIngressRuleName,
				},
			},
			{
				Name: aws.String("group-id"),
				Values: []string{
					albSecurityGroupId,
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	var albSecurityGroupIngressRuleId string
	if len(describeSecurityGroupRulesOutput.SecurityGroupRules) == 0 {
		var ipPermissions []ec2Types.IpPermission
		ipPermissions = append(ipPermissions, getIngressIpPermissionFromInternetToPort(443))
		ipPermissions = append(ipPermissions, getIngressIpPermissionFromInternetToPort(80))
		authorizeSecurityGroupIngressInput := &ec2.AuthorizeSecurityGroupIngressInput{
			DryRun:        aws.Bool(false),
			IpPermissions: ipPermissions,
			GroupId:       aws.String(albSecurityGroupId),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSecurityGroupRule,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(albSecurityGroupIngressRuleName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("alb-security-group-id"),
						Value: aws.String(albSecurityGroupId),
					},
				},
			}},
		}
		authorizeSecurityGroupIngressOutput, err := ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), authorizeSecurityGroupIngressInput)
		if err != nil {
			return "", err
		}
		albSecurityGroupIngressRuleId = aws.ToString(authorizeSecurityGroupIngressOutput.SecurityGroupRules[0].SecurityGroupRuleId)
	}

	albSecurityGroupEgressRuleName, err := getAlbSecurityGroupEgressRuleName(parameters)
	if err != nil {
		return "", err
	}

	describeSecurityGroupRulesOutput, err = ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					albSecurityGroupEgressRuleName,
				},
			},
			{
				Name: aws.String("group-id"),
				Values: []string{
					albSecurityGroupId,
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	var albSecurityGroupEgressRuleId string
	if len(describeSecurityGroupRulesOutput.SecurityGroupRules) == 0 {
		port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
		if err != nil {
			return "", err
		}

		vpcCidr, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcCidr)
		if err != nil {
			return "", err
		}
		authorizeSecurityGroupEgressInput := &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId: aws.String(albSecurityGroupId),
			DryRun:  aws.Bool(false),
			IpPermissions: []ec2Types.IpPermission{{
				FromPort:   aws.Int32(int32(port)),
				IpProtocol: aws.String("tcp"),
				IpRanges: []ec2Types.IpRange{{
					CidrIp:      aws.String(vpcCidr),
					Description: aws.String(fmt.Sprintf("VPC cidr - %s", vpcCidr)),
				}},
				ToPort: aws.Int32(int32(port)),
			}},
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSecurityGroupRule,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(albSecurityGroupEgressRuleName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("alb-security-group-id"),
						Value: aws.String(albSecurityGroupId),
					},
				},
			}},
		}

		authorizeSecurityGroupEgressOutput, err := ec2Client.AuthorizeSecurityGroupEgress(context.TODO(), authorizeSecurityGroupEgressInput)
		if err != nil {
			return "", err
		}
		albSecurityGroupEgressRuleId = aws.ToString(authorizeSecurityGroupEgressOutput.SecurityGroupRules[0].SecurityGroupRuleId)
	}

	//TODO can sync both sg ingress ids later
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return "", err
	}
	if !isPreview(parameters) {
		updateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:                       deploymentID,
			AlbSecurityGroupId:       albSecurityGroupId,
			AlbSecurityIngressRuleId: albSecurityGroupIngressRuleId,
			AlbSecurityEgressRuleId:  albSecurityGroupEgressRuleId,
		})
	} else {
		previewID := deploymentID
		updatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:                       previewID,
			AlbSecurityGroupId:       albSecurityGroupId,
			AlbSecurityIngressRuleId: albSecurityGroupIngressRuleId,
			AlbSecurityEgressRuleId:  albSecurityGroupEgressRuleId,
		})
	}

	return albSecurityGroupId, nil
}

func getAlbTargetGroupName(parameters map[string]interface{}) (string, error) {
	//tg-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tg-%s", deploymentID), nil
}

func getAlbName(parameters map[string]interface{}) (string, error) {
	//alb-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("alb-%s", deploymentID), nil
}

func getAlbListenerName(parameters map[string]interface{}, port int32) (string, error) {
	//lstr-<port>-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("lstr-%d-%s", port, deploymentID), nil
}

func createAlbIfNeeded(parameters map[string]interface{},
	elbClient *elasticloadbalancingv2.Client, albSecurityGroupId string, logsWriter io.Writer) (loadBalancerArn string, targetGroupArn string, err error) {

	loadBalancerArnFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.LoadBalancerArn)
	if err == nil && len(loadBalancerArnFromParams) > 0 {
		targetGroupArnFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.TargetGroupArn)
		if err == nil && len(targetGroupArnFromParams) > 0 {
			return loadBalancerArnFromParams, targetGroupArnFromParams, nil
		}
	}

	targetGroupName, err := getAlbTargetGroupName(parameters)
	if err != nil {
		return "", "", err
	}

	//TODO check for 400 not found error
	describeTargetGroupsOutput, _ := elbClient.DescribeTargetGroups(context.TODO(), &elasticloadbalancingv2.DescribeTargetGroupsInput{
		Names: []string{
			targetGroupName,
		},
	})

	if describeTargetGroupsOutput != nil && len(describeTargetGroupsOutput.TargetGroups) > 0 {
		targetGroupArn = aws.ToString(describeTargetGroupsOutput.TargetGroups[0].TargetGroupArn)
	} else {
		port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
		if err != nil {
			return "", "", err
		}

		vpcId, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcID)
		if err != nil {
			return "", "", err
		}

		healthCheckPath, err := jobs.GetParameterValue[string](parameters, parameters_enums.HealthCheckPath)
		if err != nil {
			return "", "", err
		}
		createTargetGroupInput := &elasticloadbalancingv2.CreateTargetGroupInput{
			Name:                       aws.String(targetGroupName),
			HealthCheckEnabled:         aws.Bool(true),
			HealthCheckIntervalSeconds: aws.Int32(40),
			HealthCheckPath:            aws.String(healthCheckPath),
			HealthCheckTimeoutSeconds:  aws.Int32(30),
			HealthCheckProtocol:        elbTypes.ProtocolEnumHttp,
			Matcher: &elbTypes.Matcher{
				HttpCode: aws.String("200-400"),
			},
			Port:     aws.Int32(int32(port)),
			Protocol: elbTypes.ProtocolEnumHttp,
			Tags: []elbTypes.Tag{
				{
					Key:   aws.String("Name"),
					Value: aws.String(targetGroupName),
				},
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
			TargetType: elbTypes.TargetTypeEnumIp,
			VpcId:      aws.String(vpcId),
		}

		createTargetGroupOutput, err := elbClient.CreateTargetGroup(context.TODO(), createTargetGroupInput)
		if err != nil {
			return "", "", err
		}
		targetGroupArn = aws.ToString(createTargetGroupOutput.TargetGroups[0].TargetGroupArn)
	}

	albName, err := getAlbName(parameters)
	if err != nil {
		return "", "", err
	}

	describeLoadBalancersOutput, err := elbClient.DescribeLoadBalancers(context.TODO(), &elasticloadbalancingv2.DescribeLoadBalancersInput{
		Names: []string{
			albName,
		},
	})
	var loadBalancerDns string
	if describeLoadBalancersOutput != nil && len(describeLoadBalancersOutput.LoadBalancers) > 0 {
		loadBalancerArn = aws.ToString(describeLoadBalancersOutput.LoadBalancers[0].LoadBalancerArn)
		loadBalancerDns = aws.ToString(describeLoadBalancersOutput.LoadBalancers[0].DNSName)
	} else {
		publicSubnets, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.PublicSubnets)
		if err != nil {
			return "", "", err
		}

		publicSubnetsSlice, err := commandUtils.ConvertPrimitiveAToStringSlice(publicSubnets)
		if err != nil {
			return "", "", err
		}

		createLoadBalancerInput := &elasticloadbalancingv2.CreateLoadBalancerInput{
			Name:           aws.String(albName),
			IpAddressType:  elbTypes.IpAddressTypeIpv4,
			Scheme:         elbTypes.LoadBalancerSchemeEnumInternetFacing,
			SecurityGroups: []string{albSecurityGroupId},
			Subnets:        publicSubnetsSlice,
			Tags: []elbTypes.Tag{
				{
					Key:   aws.String("Name"),
					Value: aws.String(albName),
				},
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
			Type: elbTypes.LoadBalancerTypeEnumApplication,
		}

		createLoadBalancerOutput, err := elbClient.CreateLoadBalancer(context.TODO(), createLoadBalancerInput)
		if err != nil {
			return "", "", err
		}
		loadBalancerArn = aws.ToString(createLoadBalancerOutput.LoadBalancers[0].LoadBalancerArn)

		newLoadBalancerAvailableWaiter := elasticloadbalancingv2.NewLoadBalancerAvailableWaiter(elbClient)

		describeLoadBalancersInput := &elasticloadbalancingv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []string{loadBalancerArn},
		}

		io.WriteString(logsWriter, fmt.Sprintf("Waiting for load balancer to be available: %s\n", loadBalancerArn))

		err = newLoadBalancerAvailableWaiter.Wait(context.TODO(), describeLoadBalancersInput, time.Minute*10)
		if err != nil {
			return "", "", err
		}
		loadBalancerDns = aws.ToString(createLoadBalancerOutput.LoadBalancers[0].DNSName)
	}

	var listenerPort int32 = 80
	albListenerName, err := getAlbListenerName(parameters, listenerPort)
	if err != nil {
		return "", "", err
	}

	describeListenersOutput, err := elbClient.DescribeListeners(context.TODO(), &elasticloadbalancingv2.DescribeListenersInput{
		LoadBalancerArn: aws.String(loadBalancerArn),
	})

	//TODO https listener as part of first deployment/creation
	//support for certificate/https flow will be added as a command
	var listenerArn string
	if describeListenersOutput != nil && len(describeListenersOutput.Listeners) > 0 {
		listenerArn = aws.ToString(describeListenersOutput.Listeners[0].ListenerArn)
	} else {
		createListenerInput := &elasticloadbalancingv2.CreateListenerInput{
			DefaultActions: []elbTypes.Action{{
				Type:           elbTypes.ActionTypeEnumForward,
				Order:          aws.Int32(1),
				TargetGroupArn: aws.String(targetGroupArn),
			}},
			LoadBalancerArn: aws.String(loadBalancerArn),
			Port:            aws.Int32(listenerPort),
			Protocol:        elbTypes.ProtocolEnumHttp,
			Tags: []elbTypes.Tag{
				{
					Key:   aws.String("Name"),
					Value: aws.String(albListenerName),
				},
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
		}
		createListenerOutput, err := elbClient.CreateListener(context.TODO(), createListenerInput)
		if err != nil {
			return "", "", err
		}

		listenerArn = aws.ToString(createListenerOutput.Listeners[0].ListenerArn)
	}

	io.WriteString(logsWriter, fmt.Sprintf("Created load balancer: %s\n", loadBalancerArn))

	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", "", err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return "", "", err
	}
	if !isPreview(parameters) {
		updateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:                deploymentID,
			TargetGroupArn:    targetGroupArn,
			ListenerArnPort80: listenerArn,
			LoadBalancerArn:   loadBalancerArn,
			LoadBalancerDns:   loadBalancerDns,
		})
	} else {
		previewID := deploymentID
		updatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:                previewID,
			TargetGroupArn:    targetGroupArn,
			ListenerArnPort80: listenerArn,
			LoadBalancerArn:   loadBalancerArn,
			LoadBalancerDns:   loadBalancerDns,
		})
	}

	return
}

func decodeEnvironmentVariablesToKeyValueSlice(envVariables string) ([]ecsTypes.KeyValuePair, error) {
	var envVariablesSlice []ecsTypes.KeyValuePair
	envEntries := strings.Split(envVariables, "\n")
	for _, envEntry := range envEntries {
		if len(envEntry) == 0 {
			continue
		}
		keyValue := strings.Split(envEntry, "=")
		if len(keyValue) < 2 {
			return nil, fmt.Errorf("environment variables in incorrect format")
		}
		value := ""
		if len(keyValue) == 2 {
			value = keyValue[1]
		} else {
			for i, s := range keyValue {
				if i > 0 {
					value += s
					if i != (len(keyValue) - 1) {
						value += "="
					}
				}
			}
		}

		kv := ecsTypes.KeyValuePair{
			Name:  aws.String(keyValue[0]),
			Value: aws.String(value),
		}
		envVariablesSlice = append(envVariablesSlice, kv)
	}
	return envVariablesSlice, nil
}

func getTaskDefinitionFamilyName(parameters map[string]interface{}) (string, error) {
	//td-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("td-%s", deploymentID), nil
}

func getContainerName(parameters map[string]interface{}) (string, error) {
	//c-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("c-%s", deploymentID), nil
}

func getEcsServiceName(parameters map[string]interface{}) (string, error) {
	//es--<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("es-%s", deploymentID), nil
}

func getEcsServiceCreationClientToken(parameters map[string]interface{}) (string, error) {
	//es-<epoch>
	return fmt.Sprintf("es-%d", time.Now().Unix()), nil
}

func getPortMappingName(parameters map[string]interface{}) (string, error) {
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("port-mapping-%s-%d", deploymentID, port), nil
}

func registerTaskDefinition(parameters map[string]interface{}, ecsClient *ecs.Client) (taskDefinitionArn string, err error) {

	taskDefinitionArnFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.TaskDefinitionArn)
	if err == nil && len(taskDefinitionArnFromParams) > 0 {
		return taskDefinitionArnFromParams, nil
	}

	port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
	if err != nil {
		return "", err
	}

	portMappingName, err := getPortMappingName(parameters)
	if err != nil {
		return "", err
	}

	containerPortMapping := ecsTypes.PortMapping{
		ContainerPort: aws.Int32(int32(port)),
		Name:          aws.String(portMappingName),
		Protocol:      ecsTypes.TransportProtocolTcp,
	}

	ecrRepositoryUriWithTag, err := jobs.GetParameterValue[string](parameters, parameters_enums.DockerRepositoryUriWithTag)
	if err != nil {
		return "", err
	}

	envVariables, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentVariables)
	var envVariablesKeyValuePair []ecsTypes.KeyValuePair
	if err == nil && len(envVariables) > 0 {
		envVariablesKeyValuePair, err = decodeEnvironmentVariablesToKeyValueSlice(envVariables)
		if err != nil {
			return "", err
		}
	}

	containerName, err := getContainerName(parameters)
	if err != nil {
		return "", err
	}

	logsStreamPrefix, err := commandUtils.GetApplicationLogStreamPrefix(parameters)
	if err != nil {
		return "", err
	}

	logGroupName, err := commandUtils.GetLogGroupName(parameters)
	if err != nil {
		return "", err
	}

	region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}

	containerDefinition := ecsTypes.ContainerDefinition{
		DisableNetworking: aws.Bool(false),
		Environment:       envVariablesKeyValuePair,
		Essential:         aws.Bool(true),
		Image:             aws.String(ecrRepositoryUriWithTag),
		Interactive:       aws.Bool(false),
		LogConfiguration: &ecsTypes.LogConfiguration{
			LogDriver: ecsTypes.LogDriverAwslogs,
			Options: map[string]string{
				"awslogs-create-group":  "true",
				"awslogs-group":         logGroupName,
				"awslogs-region":        region_enums.Type(region).String(),
				"awslogs-stream-prefix": logsStreamPrefix,
			},
		},
		Name: aws.String(containerName),
		PortMappings: []ecsTypes.PortMapping{
			containerPortMapping,
		},
		Privileged:             aws.Bool(false),
		PseudoTerminal:         aws.Bool(false),
		ReadonlyRootFilesystem: aws.Bool(false),
	}

	isPrivateRepository, _ := jobs.GetParameterValue[bool](parameters, parameters_enums.IsPrivateRegistry)
	if isPrivateRepository {
		secretName, err := jobs.GetParameterValue[string](parameters, parameters_enums.SecretName)
		if err != nil {
			return "", err
		}
		containerDefinition.RepositoryCredentials = &ecsTypes.RepositoryCredentials{CredentialsParameter: aws.String(secretName)}
	}

	taskDefinitionFamilyName, err := getTaskDefinitionFamilyName(parameters)
	if err != nil {
		return "", err
	}

	cpu, err := jobs.GetParameterValue[string](parameters, parameters_enums.Cpu)
	if err != nil {
		return "", err
	}

	memory, err := jobs.GetParameterValue[string](parameters, parameters_enums.Memory)
	if err != nil {
		return "", err
	}

	ecsTaskExecutionRoleArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsTaskExecutionRoleArn)
	if err != nil {
		return "", err
	}
	runnerData := utils.RunnerData.Get()
	cpuArch := ecsTypes.CPUArchitectureX8664
	if runnerData.CpuArchEnum == cpu_architecture_enums.ARM {
		cpuArch = ecsTypes.CPUArchitectureArm64
	}

	osFamily := ecsTypes.OSFamilyLinux
	if runnerData.OsType == os_enums.WINDOWS {
		osFamily = ecsTypes.OSFamilyWindowsServer2022Core
	}

	registerTaskDefinitionInput := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: []ecsTypes.ContainerDefinition{
			containerDefinition,
		},
		Family:           aws.String(taskDefinitionFamilyName),
		Cpu:              aws.String(cpu),
		ExecutionRoleArn: aws.String(ecsTaskExecutionRoleArn),
		Memory:           aws.String(memory),
		NetworkMode:      ecsTypes.NetworkModeAwsvpc,
		RuntimePlatform: &ecsTypes.RuntimePlatform{
			CpuArchitecture:       cpuArch,
			OperatingSystemFamily: osFamily,
		},
		Tags: []ecsTypes.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(taskDefinitionFamilyName),
			},
			{
				Key:   aws.String("created by"),
				Value: aws.String("deployment.io"),
			},
		},
	}
	registerTaskDefinitionOutput, err := ecsClient.RegisterTaskDefinition(context.TODO(), registerTaskDefinitionInput)

	if err != nil {
		return "", err
	}

	taskDefinitionArn = aws.ToString(registerTaskDefinitionOutput.TaskDefinition.TaskDefinitionArn)

	return taskDefinitionArn, nil
}

func getNamespaceName(parameters map[string]interface{}) (string, error) {
	environmentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentID)
	if err != nil {
		return "", err
	}
	environmentName, err := jobs.GetParameterValue[string](parameters, parameters_enums.EnvironmentName)
	if err != nil {
		return "", err
	}

	namespaceName := fmt.Sprintf("%s-%s", environmentName, environmentID[len(environmentID)-4:])

	namespaceName = strings.ToLower(namespaceName)

	pattern := "[^a-z0-9-]"

	re := regexp.MustCompile(pattern)

	// Replace all unmatched characters with -
	sanitized := re.ReplaceAllString(namespaceName, "-")

	// Remove '-' prefix if it exists
	for len(sanitized) > 0 && sanitized[0] == '-' {
		sanitized = strings.TrimPrefix(sanitized, "-")
	}

	if len(sanitized) == 0 {
		return "", fmt.Errorf("empty namespace name")
	}
	return sanitized, nil
}

func createNamespaceIfNeeded(parameters map[string]interface{}, logsWriter io.Writer) (string, error) {
	namespaceName, err := getNamespaceName(parameters)
	if err != nil {
		return "", err
	}
	vpcID, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcID)
	if err != nil {
		return "", err
	}
	serviceDiscoveryClient, err := cloud_api_clients.GetServiceDiscoveryClient(parameters)
	if err != nil {
		return "", err
	}

	triedCreating := false
	maxAttempts := 30

	for i := 0; i < maxAttempts; i++ {
		listNamespacesOutput, err := serviceDiscoveryClient.ListNamespaces(context.TODO(), &servicediscovery.ListNamespacesInput{
			Filters: []sd_types.NamespaceFilter{{
				Name:      sd_types.NamespaceFilterNameName,
				Values:    []string{namespaceName},
				Condition: sd_types.FilterConditionEq,
			}},
		})

		if err == nil && listNamespacesOutput != nil && len(listNamespacesOutput.Namespaces) > 0 {
			//already exists
			return namespaceName, nil
		}

		if !triedCreating {
			//try creating
			io.WriteString(logsWriter, fmt.Sprintf("Creating new namespace for environment: %s\n", namespaceName))
			requestId := fmt.Sprintf("%d", time.Now().Unix())
			input := &servicediscovery.CreatePrivateDnsNamespaceInput{
				Name:             aws.String(namespaceName),
				CreatorRequestId: aws.String(requestId),
				Description:      aws.String(fmt.Sprintf("namespace for environment: %s", namespaceName)),
				Vpc:              aws.String(vpcID),
				Tags: []sd_types.Tag{{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				}},
			}

			_, err = serviceDiscoveryClient.CreatePrivateDnsNamespace(context.TODO(), input)
			if err != nil {
				return "", err
			}
			triedCreating = true
		}

		time.Sleep(5 * time.Second)
	}

	return "", fmt.Errorf("exceeded max attempts while creating namespace")
}

func getInternalDnsName(parameters map[string]interface{}) (string, error) {
	deploymentName, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentName)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}

	// Convert all upper case letters in string to lower case
	deploymentName = strings.ToLower(deploymentName)

	// Pattern excluding the characters you want to keep
	pattern := "[^a-z0-9_.-]"

	re := regexp.MustCompile(pattern)

	// Replace all unmatched characters with -
	dnsName := re.ReplaceAllString(deploymentName, "-")

	// Remove '-' prefix if it exists
	for len(dnsName) > 0 && dnsName[0] == '-' {
		dnsName = strings.TrimPrefix(dnsName, "-")
	}
	if len(dnsName) == 0 {
		return "", fmt.Errorf("empty dns name")
	}

	dnsName = fmt.Sprintf("%s-%s", dnsName, deploymentID[len(deploymentID)-4:])
	return dnsName, nil
}

func createEcsServiceIfNeeded(parameters map[string]interface{}, ecsClient *ecs.Client,
	ecsClusterArn, targetGroupArn, taskDefinitionArn string, logsWriter io.Writer) (ecsServiceArn string, shouldUpdateService bool, err error) {

	ecsServiceArnFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsServiceArn)
	if err == nil && len(ecsServiceArnFromParams) > 0 {
		return ecsServiceArnFromParams, true, nil
	}

	ecsServiceName, err := getEcsServiceName(parameters)
	if err != nil {
		return "", false, err
	}

	shouldUpdateService = false

	describeServicesInput := &ecs.DescribeServicesInput{
		Services: []string{
			ecsServiceName,
		},
		Cluster: aws.String(ecsClusterArn),
	}

	describeServicesOutput, err := ecsClient.DescribeServices(context.TODO(), describeServicesInput)
	if err != nil {
		return "", false, err
	}

	var dnsName, namespaceName string

	if len(describeServicesOutput.Services) > 0 {
		ecsServiceArn = aws.ToString(describeServicesOutput.Services[0].ServiceArn)
		shouldUpdateService = true
	} else {
		namespaceName, err = createNamespaceIfNeeded(parameters, logsWriter)
		if err != nil {
			return "", false, err
		}
		privateSubnets, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.PrivateSubnets)
		if err != nil {
			return "", false, err
		}
		privateSubnetsSlice, err := commandUtils.ConvertPrimitiveAToStringSlice(privateSubnets)
		if err != nil {
			return "", false, err
		}
		awsVpcConfiguration := &ecsTypes.AwsVpcConfiguration{
			Subnets: privateSubnetsSlice,
		}

		networkConfiguration := &ecsTypes.NetworkConfiguration{
			AwsvpcConfiguration: awsVpcConfiguration,
		}

		containerName, err := getContainerName(parameters)
		if err != nil {
			return "", false, err
		}

		port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
		if err != nil {
			return "", false, err
		}

		ecsServiceCreationClientToken, err := getEcsServiceCreationClientToken(parameters)
		if err != nil {
			return "", false, err
		}

		portMappingName, err := getPortMappingName(parameters)
		if err != nil {
			return "", false, err
		}

		dnsName, err = getInternalDnsName(parameters)
		if err != nil {
			return "", false, err
		}

		var loadBalancers []ecsTypes.LoadBalancer
		var healthCheckGracePeriod *int32
		if len(targetGroupArn) > 0 {
			//only needed for public web services
			loadBalancers = []ecsTypes.LoadBalancer{{
				ContainerName:  aws.String(containerName),
				ContainerPort:  aws.Int32(int32(port)),
				TargetGroupArn: aws.String(targetGroupArn),
			}}
			healthCheckGracePeriod = aws.Int32(30)
		}

		createServiceInput := &ecs.CreateServiceInput{
			ServiceName:                   aws.String(ecsServiceName),
			CapacityProviderStrategy:      nil, // launchtype should be present since this in nil
			ClientToken:                   aws.String(ecsServiceCreationClientToken),
			Cluster:                       aws.String(ecsClusterArn),
			DesiredCount:                  aws.Int32(1),
			EnableECSManagedTags:          false,
			EnableExecuteCommand:          false,
			HealthCheckGracePeriodSeconds: healthCheckGracePeriod,     //use the startPeriod in the task definition health check parameters if this is empty - 30 seconds for now
			LaunchType:                    ecsTypes.LaunchTypeFargate, //use capacity provider for fargate spot
			LoadBalancers:                 loadBalancers,
			NetworkConfiguration:          networkConfiguration,
			PropagateTags:                 ecsTypes.PropagateTagsTaskDefinition,
			SchedulingStrategy:            ecsTypes.SchedulingStrategyReplica,
			Tags: []ecsTypes.Tag{
				{
					Key:   aws.String("Name"),
					Value: aws.String(ecsServiceName),
				},
				{
					Key:   aws.String("created by"),
					Value: aws.String("deployment.io"),
				},
			},
			TaskDefinition: aws.String(taskDefinitionArn),
			ServiceConnectConfiguration: &ecsTypes.ServiceConnectConfiguration{
				Enabled:   true,
				Namespace: aws.String(namespaceName),
				Services: []ecsTypes.ServiceConnectService{{
					PortName: aws.String(portMappingName),
					ClientAliases: []ecsTypes.ServiceConnectClientAlias{{
						Port:    aws.Int32(int32(port)),
						DnsName: aws.String(dnsName),
					}},
				}},
			},
		}

		createServiceOutput, err := ecsClient.CreateService(context.TODO(), createServiceInput)
		if err != nil {
			return "", false, err
		}

		ecsServiceArn = aws.ToString(createServiceOutput.Service.ServiceArn)

		newServicesStableWaiter := ecs.NewServicesStableWaiter(ecsClient)

		io.WriteString(logsWriter, fmt.Sprintf("Waiting for ECS service to be stable: %s\n", ecsServiceArn))

		err = newServicesStableWaiter.Wait(context.TODO(), describeServicesInput, time.Minute*10)

		if err != nil {
			return "", false, err
		}
	}

	io.WriteString(logsWriter, fmt.Sprintf("Created ECS service: %s\n", ecsServiceArn))

	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", false, err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return "", false, err
	}
	if !isPreview(parameters) {
		updateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:            deploymentID,
			EcsServiceArn: ecsServiceArn,
			DnsName:       dnsName,
			NamespaceName: namespaceName,
		})
	} else {
		previewID := deploymentID
		updatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:            previewID,
			EcsServiceArn: ecsServiceArn,
			DnsName:       dnsName,
			NamespaceName: namespaceName,
		})
	}

	return
}

func updateEcsService(parameters map[string]interface{}, ecsClient *ecs.Client, ecsClusterArn string,
	taskDefinitionArn string, logsWriter io.Writer) error {
	//TODO desired count is 1 for now.
	ecsServiceName, err := getEcsServiceName(parameters)
	if err != nil {
		return err
	}
	updateServiceInput := &ecs.UpdateServiceInput{
		Service:        aws.String(ecsServiceName),
		Cluster:        aws.String(ecsClusterArn),
		DesiredCount:   aws.Int32(1),
		TaskDefinition: aws.String(taskDefinitionArn),
		PropagateTags:  ecsTypes.PropagateTagsTaskDefinition,
	}
	_, err = ecsClient.UpdateService(context.TODO(), updateServiceInput)

	if err != nil {
		return err
	}

	describeServicesInput := &ecs.DescribeServicesInput{
		Services: []string{
			ecsServiceName,
		},
		Cluster: aws.String(ecsClusterArn),
	}

	describeServicesOutput, err := ecsClient.DescribeServices(context.TODO(), describeServicesInput)
	if err != nil {
		return err
	}

	ecsServiceArn := aws.ToString(describeServicesOutput.Services[0].ServiceArn)

	newServicesStableWaiter := ecs.NewServicesStableWaiter(ecsClient)

	io.WriteString(logsWriter, fmt.Sprintf("Waiting for ECS service to be stable: %s\n", ecsServiceArn))

	err = newServicesStableWaiter.Wait(context.TODO(), describeServicesInput, time.Minute*10)

	if err != nil {
		return err
	}

	return nil
}

//TODO
//1. add another ingress security rule for ALB security group  - port 80

func (d *DeployAwsWebService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
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
	var securityGroupId string
	securityGroupId, err = createAlbSecurityGroupIfNeeded(parameters, ec2Client)
	if err != nil {
		return parameters, err
	}
	elbClient, err := cloud_api_clients.GetElbClient(parameters)
	if err != nil {
		return parameters, err
	}
	_, targetGroupArn, err := createAlbIfNeeded(parameters, elbClient, securityGroupId, logsWriter)
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
	_, shouldUpdateService, err := createEcsServiceIfNeeded(parameters, ecsClient, ecsClusterArn, targetGroupArn, taskDefinitionArn, logsWriter)
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
		updateBuildsPipeline.Add(organizationIdFromJob, builds.UpdateBuildDtoV1{
			ID:                buildID,
			TaskDefinitionArn: taskDefinitionArn,
		})
	} else {
		//build id is preview id
		previewID := buildID
		updatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:                previewID,
			TaskDefinitionArn: taskDefinitionArn,
		})
	}

	//mark build done successfully
	<-MarkDeploymentDone(parameters, nil)

	return parameters, nil
}
