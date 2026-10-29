package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sd_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/aws/smithy-go"
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
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

// isDuplicateSecurityGroupRuleError reports whether err is the EC2
// InvalidPermission.Duplicate API error. EC2 returns it from
// AuthorizeSecurityGroupIngress/AuthorizeSecurityGroupEgress when the rule we are
// trying to add already exists. For our deploys that means the desired rule is
// already in place, so the caller can treat it as success and continue.
func isDuplicateSecurityGroupRuleError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidPermission.Duplicate"
	}
	return false
}

// recoverAndTagAlbSecurityGroupRule finds the id of the existing rule in the ALB
// security group matching the given direction, tcp port and cidr, and re-applies our
// Name tags to it so future deploys find the rule with the tag-based describe again.
// Called after EC2 reports InvalidPermission.Duplicate for a rule the tag-based
// describe missed (rule created by an older runner or with its tags stripped).
// Recovery is best effort - the security group is already in the desired state at
// this point - so failures are logged and an empty id is returned instead of
// failing the deploy.
func recoverAndTagAlbSecurityGroupRule(ec2Client *ec2.Client, albSecurityGroupId, ruleName string, isEgress bool, port int32, cidr string, logsWriter io.Writer) string {
	describeOutput, err := ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{{
			Name:   aws.String("group-id"),
			Values: []string{albSecurityGroupId},
		}},
	})
	if err != nil {
		io.WriteString(logsWriter, fmt.Sprintf("ALB security group: could not describe rules while recovering %s: %s\n", ruleName, err))
		return ""
	}
	for _, rule := range describeOutput.SecurityGroupRules {
		if aws.ToBool(rule.IsEgress) != isEgress || aws.ToString(rule.IpProtocol) != "tcp" ||
			aws.ToInt32(rule.FromPort) != port || aws.ToInt32(rule.ToPort) != port ||
			aws.ToString(rule.CidrIpv4) != cidr {
			continue
		}
		ruleId := aws.ToString(rule.SecurityGroupRuleId)
		_, err = ec2Client.CreateTags(context.TODO(), &ec2.CreateTagsInput{
			Resources: []string{ruleId},
			Tags: []ec2Types.Tag{
				{
					Key:   aws.String("Name"),
					Value: aws.String(ruleName),
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
		})
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("ALB security group: could not re-tag rule %s (%s): %s — the next deploy will retry.\n", ruleName, ruleId, err))
		}
		return ruleId
	}
	io.WriteString(logsWriter, fmt.Sprintf("ALB security group: no existing rule matched %s on port %d.\n", ruleName, port))
	return ""
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
		//if the rule already exists EC2 returns InvalidPermission.Duplicate. The desired rule is
		//already present, so we treat it as success and continue (idempotent ingress setup).
		if err != nil && !isDuplicateSecurityGroupRuleError(err) {
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

		commandUtils.UpsertVpcsPipeline.Add(organizationIdFromJob, vpcs.UpsertVpcDtoV1{
			Type:                        vpc_enums.AwsVpc,
			Region:                      region_enums.Type(region),
			DefaultSecurityIngressRules: ingressRules,
			DefaultSecurityGroupId:      defaultSecurityGroupId,
		})
	}
	return nil
}

func createAlbSecurityGroupIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, logsWriter io.Writer) (albSecurityGroupId string, err error) {
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
			//InvalidPermission.Duplicate means the rule already exists - treat as success,
			//recover the existing rule id and re-tag the rule so future deploys find it by tag
			if !isDuplicateSecurityGroupRuleError(err) {
				return "", err
			}
			//AuthorizeSecurityGroupIngress is ALL-OR-NOTHING across its
			//IpPermissions: one duplicate rejects the whole request, and the
			//others are never added. So a group holding 443 but not 80 fails
			//here, and treating that as "already configured" would leave 80
			//permanently missing — the ALB's HTTP->HTTPS redirect listener
			//would have no ingress, and http:// would time out rather than
			//redirect. Worse, it self-seals: once 443 is re-tagged below, the
			//tag-based describe above finds it and every later deploy skips
			//this block entirely.
			//
			//So retry each port on its OWN request. A port that really is
			//present duplicates again and gets recovered; a port that is
			//missing is created. Only a genuinely different error fails the
			//deploy.
			io.WriteString(logsWriter, fmt.Sprintf("ALB security group: ingress rule %s already exists; reconciling each port.\n", albSecurityGroupIngressRuleName))
			//Retry exactly what THIS request asked for, one permission at a
			//time — derived from ipPermissions rather than restating the
			//ports. Restating them means adding a port above silently leaves
			//it out of the reconcile, which is this same bug again for the new
			//port.
			for _, permission := range ipPermissions {
				perPortInput := &ec2.AuthorizeSecurityGroupIngressInput{
					DryRun:            aws.Bool(false),
					IpPermissions:     []ec2Types.IpPermission{permission},
					GroupId:           aws.String(albSecurityGroupId),
					TagSpecifications: authorizeSecurityGroupIngressInput.TagSpecifications,
				}
				port := aws.ToInt32(permission.FromPort)
				perPortOutput, perPortErr := ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), perPortInput)
				var ruleId string
				switch {
				case perPortErr == nil:
					if len(perPortOutput.SecurityGroupRules) > 0 {
						ruleId = aws.ToString(perPortOutput.SecurityGroupRules[0].SecurityGroupRuleId)
					}
					io.WriteString(logsWriter, fmt.Sprintf("ALB security group: added missing ingress on port %d.\n", port))
				case isDuplicateSecurityGroupRuleError(perPortErr):
					//Genuinely present, but the tag-based describe missed it —
					//an older runner created it, or its tags were stripped.
					//Re-tag so the next deploy finds it and never reaches here.
					//The cidr comes from the permission too, so the matcher
					//cannot drift from what was authorized.
					cidr := ""
					if len(permission.IpRanges) > 0 {
						cidr = aws.ToString(permission.IpRanges[0].CidrIp)
					}
					ruleId = recoverAndTagAlbSecurityGroupRule(ec2Client, albSecurityGroupId, albSecurityGroupIngressRuleName, false, port, cidr, logsWriter)
				default:
					return "", perPortErr
				}
				if len(albSecurityGroupIngressRuleId) == 0 {
					albSecurityGroupIngressRuleId = ruleId
				}
			}
		} else {
			albSecurityGroupIngressRuleId = aws.ToString(authorizeSecurityGroupIngressOutput.SecurityGroupRules[0].SecurityGroupRuleId)
		}
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
			//InvalidPermission.Duplicate means the rule already exists - treat as success,
			//recover the existing rule id and re-tag the rule so future deploys find it by tag
			if !isDuplicateSecurityGroupRuleError(err) {
				return "", err
			}
			//Single IpPermission here, so a duplicate means exactly this rule
			//exists — no all-or-nothing hazard as on the ingress side above.
			io.WriteString(logsWriter, fmt.Sprintf("ALB security group: egress rule %s already exists; re-tagging it.\n", albSecurityGroupEgressRuleName))
			albSecurityGroupEgressRuleId = recoverAndTagAlbSecurityGroupRule(ec2Client, albSecurityGroupId, albSecurityGroupEgressRuleName, true, int32(port), vpcCidr, logsWriter)
		} else {
			albSecurityGroupEgressRuleId = aws.ToString(authorizeSecurityGroupEgressOutput.SecurityGroupRules[0].SecurityGroupRuleId)
		}
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
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:                       deploymentID,
			AlbSecurityGroupId:       albSecurityGroupId,
			AlbSecurityIngressRuleId: albSecurityGroupIngressRuleId,
			AlbSecurityEgressRuleId:  albSecurityGroupEgressRuleId,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
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
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:                deploymentID,
			TargetGroupArn:    targetGroupArn,
			ListenerArnPort80: listenerArn,
			LoadBalancerArn:   loadBalancerArn,
			LoadBalancerDns:   loadBalancerDns,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
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

// allowUnprivilegedPorts lets a non-root process in a Linux container bind any
// port, as it can under plain Docker (which sets ip_unprivileged_port_start to 0).
// Fargate's awsvpc task namespace keeps Linux's default of 1024, so images that drop
// root and listen on port 80 (e.g. Rails 8 + Thruster) fail with "bind: permission
// denied". Windows containers don't support systemControls.
func allowUnprivilegedPorts(def *ecsTypes.ContainerDefinition, osFamily ecsTypes.OSFamily) {
	if osFamily != ecsTypes.OSFamilyLinux {
		return
	}
	def.SystemControls = []ecsTypes.SystemControl{{
		Namespace: aws.String("net.ipv4.ip_unprivileged_port_start"),
		Value:     aws.String("0"),
	}}
}

// registerTaskDefinition registers the deployment's task definition; customize,
// when given, adjusts its container before it's registered (the preview MongoDB's
// replica set).
func registerTaskDefinition(parameters map[string]interface{}, ecsClient *ecs.Client, customize ...func(*ecsTypes.ContainerDefinition)) (taskDefinitionArn string, err error) {

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

	for _, c := range customize {
		c(&containerDefinition)
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
	allowUnprivilegedPorts(&containerDefinition, osFamily)

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
	serviceDiscoveryClient, err := getServiceDiscoveryClient(parameters)
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

// getEcsServiceNetworkConfiguration is the network a deployment's tasks run in:
// the private subnets, no public IP and no security groups of their own (so the
// VPC's default one) — or, for a Task's backend preview, exactly its
// TaskSecurityGroupIds. The service and the pre-deploy command's one-off task
// both use it, so the pre-deploy command reaches exactly what the service reaches.
func getEcsServiceNetworkConfiguration(parameters map[string]interface{}) (*ecsTypes.NetworkConfiguration, error) {
	privateSubnets, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.PrivateSubnets)
	if err != nil {
		return nil, err
	}
	privateSubnetsSlice, err := commandUtils.ConvertPrimitiveAToStringSlice(privateSubnets)
	if err != nil {
		return nil, err
	}
	securityGroups, err := getTaskSecurityGroupIds(parameters)
	if err != nil {
		return nil, err
	}
	return &ecsTypes.NetworkConfiguration{
		AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{
			Subnets:        privateSubnetsSlice,
			SecurityGroups: securityGroups,
		},
	}, nil
}

// getTaskSecurityGroupIds is the TaskSecurityGroupIds a Task's backend preview
// runs in; nil when unset (every production deploy).
func getTaskSecurityGroupIds(parameters map[string]interface{}) ([]string, error) {
	groups, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.TaskSecurityGroupIds)
	if err != nil || len(groups) == 0 {
		return nil, nil
	}
	return commandUtils.ConvertPrimitiveAToStringSlice(groups)
}

// getEcsServiceConnectConfiguration is the Service Connect configuration
// createEcsServiceIfNeeded creates a service with: its port mapping under its
// alias in its namespace.
func getEcsServiceConnectConfiguration(parameters map[string]interface{}) (*ecsTypes.ServiceConnectConfiguration, error) {
	namespaceName, err := getNamespaceName(parameters)
	if err != nil {
		return nil, err
	}
	port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
	if err != nil {
		return nil, err
	}
	portMappingName, err := getPortMappingName(parameters)
	if err != nil {
		return nil, err
	}
	dnsName, err := getInternalDnsName(parameters)
	if err != nil {
		return nil, err
	}
	return &ecsTypes.ServiceConnectConfiguration{
		Enabled:   true,
		Namespace: aws.String(namespaceName),
		Services: []ecsTypes.ServiceConnectService{{
			PortName: aws.String(portMappingName),
			ClientAliases: []ecsTypes.ServiceConnectClientAlias{{
				Port:    aws.Int32(int32(port)),
				DnsName: aws.String(dnsName),
			}},
		}},
	}, nil
}

// previewTargetGroupDeregistrationDelay is how long a backend preview's load
// balancer drains a stopping task (AWS's default is 300 s). A preview rolls out
// stop-then-start (previewServiceDeploymentConfiguration), so ECS waits for the
// drain before starting the new task: the default would add five idle minutes
// to every redeploy.
const previewTargetGroupDeregistrationDelay = "5"

// previewTargetGroupAPI is the part of the ELB client setting a preview's
// target group drain uses.
type previewTargetGroupAPI interface {
	ModifyTargetGroupAttributes(ctx context.Context, params *elasticloadbalancingv2.ModifyTargetGroupAttributesInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.ModifyTargetGroupAttributesOutput, error)
}

// setPreviewTargetGroupDeregistrationDelay sets a backend preview's target group
// drain to previewTargetGroupDeregistrationDelay, on every deploy (it's
// idempotent, and reaches previews created before it existed).
func setPreviewTargetGroupDeregistrationDelay(elbClient previewTargetGroupAPI, targetGroupArn string) error {
	_, err := elbClient.ModifyTargetGroupAttributes(context.TODO(), &elasticloadbalancingv2.ModifyTargetGroupAttributesInput{
		TargetGroupArn: aws.String(targetGroupArn),
		Attributes: []elbTypes.TargetGroupAttribute{{
			Key:   aws.String("deregistration_delay.timeout_seconds"),
			Value: aws.String(previewTargetGroupDeregistrationDelay),
		}},
	})
	if err != nil {
		return fmt.Errorf("set the preview target group's deregistration delay: %w", err)
	}
	return nil
}

// previewServiceDeploymentConfiguration is a backend preview's rollout: the old
// task is stopped (and drained from its load balancer) before the new one starts,
// never beside it. A redeploy is then never answered by the previous revision:
// its health URL fails until the new task serves, and keeps failing when the new
// task can't start — the deploy returns without waiting (skipServiceStableWait),
// and QA and the agent read readiness from that URL. A preview's brief downtime
// doesn't matter.
func previewServiceDeploymentConfiguration() *ecsTypes.DeploymentConfiguration {
	return &ecsTypes.DeploymentConfiguration{
		MinimumHealthyPercent: aws.Int32(0),
		MaximumPercent:        aws.Int32(100),
	}
}

// ecsServiceDeployOptions changes how a deploy creates or updates its ECS
// service. The zero value is what every production deploy does.
type ecsServiceDeployOptions struct {
	// previewStack deploys a Task's backend preview into its preview stack's
	// security group (TaskSecurityGroupIds, exactly one) instead of the VPC's
	// default group: the default group is never edited, and the stack group admits
	// the service's port from the preview's own load balancer group. Its service
	// rolls out stop-then-start (previewServiceDeploymentConfiguration).
	previewStack bool
	// skipServiceStableWait returns once the service is created or updated,
	// without waiting for it to settle. A Task's backend preview sets it: its
	// readiness is checked by verify_preview_reachable on its health URL.
	skipServiceStableWait bool
	// registerDnsName registers the service's Service Connect alias as a DNS
	// name in its namespace (private_service_dns.go). Only DeployAwsPrivateService
	// sets it.
	registerDnsName bool
	// updateServiceConnect also passes the Service Connect configuration when
	// it updates an existing service, so a new task definition whose port (and
	// so its port mapping's name) changed is accepted. Only a Task's preview
	// database sets it: changing its engine changes its port.
	updateServiceConnect bool
}

// waitForEcsServiceStable waits until the service settles. Swapped by tests
// so they see whether a deploy waits, without AWS.
var waitForEcsServiceStable = func(ecsClient *ecs.Client, describeServicesInput *ecs.DescribeServicesInput) error {
	return ecs.NewServicesStableWaiter(ecsClient).Wait(context.TODO(), describeServicesInput, time.Minute*20)
}

func createEcsServiceIfNeeded(parameters map[string]interface{}, ecsClient *ecs.Client,
	ecsClusterArn, targetGroupArn, taskDefinitionArn string, logsWriter io.Writer, opts ecsServiceDeployOptions) (ecsServiceArn string, shouldUpdateService bool, err error) {

	ecsServiceArnFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsServiceArn)
	if err == nil && len(ecsServiceArnFromParams) > 0 {
		return ecsServiceArnFromParams, true, nil
	}
	ecsServiceAPIClient := ecsServiceClient(ecsClient)

	ecsServiceName, err := aws_utils.GetEcsServiceName(parameters)
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

	describeServicesOutput, err := ecsServiceAPIClient.DescribeServices(context.TODO(), describeServicesInput)
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
		networkConfiguration, err := getEcsServiceNetworkConfiguration(parameters)
		if err != nil {
			return "", false, err
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

		if opts.registerDnsName {
			createServiceInput.ServiceRegistries = dnsServiceRegistries(parameters, namespaceName, dnsName, logsWriter)
		}
		if opts.previewStack {
			createServiceInput.DeploymentConfiguration = previewServiceDeploymentConfiguration()
		}

		createServiceOutput, err := ecsServiceAPIClient.CreateService(context.TODO(), createServiceInput)
		if err != nil {
			return "", false, err
		}

		ecsServiceArn = aws.ToString(createServiceOutput.Service.ServiceArn)

		if !opts.skipServiceStableWait {
			io.WriteString(logsWriter, fmt.Sprintf("Waiting for ECS service to be stable: %s\n", ecsServiceArn))

			err = waitForEcsServiceStable(ecsClient, describeServicesInput)

			if err != nil {
				return "", false, err
			}
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
		commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
			ID:            deploymentID,
			EcsServiceArn: ecsServiceArn,
			DnsName:       dnsName,
			NamespaceName: namespaceName,
		})
	} else {
		previewID := deploymentID
		commandUtils.UpdatePreviewsPipeline.Add(organizationIdFromJob, previews.UpdatePreviewDtoV1{
			ID:            previewID,
			EcsServiceArn: ecsServiceArn,
			DnsName:       dnsName,
			NamespaceName: namespaceName,
		})
	}

	return
}

func updateEcsService(parameters map[string]interface{}, ecsClient *ecs.Client, ecsClusterArn string,
	taskDefinitionArn string, logsWriter io.Writer, opts ecsServiceDeployOptions) error {
	//TODO desired count is 1 for now.
	ecsServiceName, err := aws_utils.GetEcsServiceName(parameters)
	if err != nil {
		return err
	}
	describeServicesInput := &ecs.DescribeServicesInput{
		Services: []string{
			ecsServiceName,
		},
		Cluster: aws.String(ecsClusterArn),
	}

	ecsServiceAPIClient := ecsServiceClient(ecsClient)
	updateServiceInput := &ecs.UpdateServiceInput{
		Service:        aws.String(ecsServiceName),
		Cluster:        aws.String(ecsClusterArn),
		DesiredCount:   aws.Int32(1),
		TaskDefinition: aws.String(taskDefinitionArn),
		PropagateTags:  ecsTypes.PropagateTagsTaskDefinition,
	}
	if opts.previewStack {
		updateServiceInput.DeploymentConfiguration = previewServiceDeploymentConfiguration()
	}
	if opts.registerDnsName {
		// In the same call as the new task definition, so it is one ECS
		// deployment. Nil leaves a registry the service already has as is.
		updateServiceInput.ServiceRegistries = missingDnsServiceRegistries(parameters, ecsServiceAPIClient, describeServicesInput, logsWriter)
	}
	if opts.updateServiceConnect {
		updateServiceInput.ServiceConnectConfiguration, err = getEcsServiceConnectConfiguration(parameters)
		if err != nil {
			return err
		}
	}
	if groups, _ := getTaskSecurityGroupIds(parameters); len(groups) > 0 {
		// A preview's existing service moves onto its stack's groups.
		updateServiceInput.NetworkConfiguration, err = getEcsServiceNetworkConfiguration(parameters)
		if err != nil {
			return err
		}
	}
	_, err = ecsServiceAPIClient.UpdateService(context.TODO(), updateServiceInput)

	if err != nil {
		return err
	}

	describeServicesOutput, err := ecsServiceAPIClient.DescribeServices(context.TODO(), describeServicesInput)
	if err != nil {
		return err
	}

	ecsServiceArn := aws.ToString(describeServicesOutput.Services[0].ServiceArn)

	if opts.skipServiceStableWait {
		io.WriteString(logsWriter, fmt.Sprintf("Updated ECS service: %s\n", ecsServiceArn))
		return nil
	}

	io.WriteString(logsWriter, fmt.Sprintf("Waiting for ECS service to be stable: %s\n", ecsServiceArn))

	err = waitForEcsServiceStable(ecsClient, describeServicesInput)

	if err != nil {
		return err
	}

	return nil
}

//TODO
//1. add another ingress security rule for ALB security group  - port 80

func (d *DeployAwsWebService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	return runAwsWebServiceDeploy(parameters, logsWriter, ecsServiceDeployOptions{})
}

// deployAwsWebServicePreview is DeployAwsWebService's steps for a Task's
// backend preview: the same deploy, without waiting for the ECS service to
// settle.
func deployAwsWebServicePreview(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	return runAwsWebServiceDeploy(parameters, logsWriter, ecsServiceDeployOptions{skipServiceStableWait: true, previewStack: true})
}

// runAwsWebServiceDeploy is deployAwsWebService; tests replace it to see the
// options each entry point deploys with.
var runAwsWebServiceDeploy = deployAwsWebService

// deployAwsWebService runs the web-service deploy's steps with opts.
func deployAwsWebService(parameters map[string]interface{}, logsWriter io.Writer, opts ecsServiceDeployOptions) (newParameters map[string]interface{}, err error) {
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
	if !opts.previewStack {
		err = addIngressRuleToDefaultVpcSecurityGroupForPortIfNeeded(parameters, ec2Client)
		if err != nil {
			return parameters, err
		}
	}
	var securityGroupId string
	securityGroupId, err = createAlbSecurityGroupIfNeeded(parameters, ec2Client, logsWriter)
	if err != nil {
		return parameters, err
	}
	if opts.previewStack {
		if err = allowPreviewStackPortFromLoadBalancer(parameters, ec2Client, securityGroupId); err != nil {
			return parameters, err
		}
	}
	elbClient, err := cloud_api_clients.GetElbClient(parameters)
	if err != nil {
		return parameters, err
	}
	_, targetGroupArn, err := createAlbIfNeeded(parameters, elbClient, securityGroupId, logsWriter)
	if err != nil {
		return parameters, err
	}
	if opts.previewStack {
		if err = setPreviewTargetGroupDeregistrationDelay(elbClient, targetGroupArn); err != nil {
			return parameters, err
		}
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
	logsClient, err := cloud_api_clients.GetCloudwatchLogsClient(parameters)
	if err != nil {
		return parameters, err
	}
	err = deployEcsServiceAfterPreDeploy(parameters, ecsClient, logsClient, ecsClusterArn, targetGroupArn, taskDefinitionArn, logsWriter, opts)
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
