package commands

// preview_stack_security_group.go is the security group of a Task's backend preview
// stack, pvsg-<taskID>. Every backend preview runs in it alone — the API preview,
// its database and its one-off tasks — never in the VPC's default group, which a
// preview never edits either. It admits the database's port from itself and the
// API's port from the preview's own load balancer group, so only that Task's
// preview reaches its database and only through its load balancer reaches its API.
// Its outbound traffic is limited to public IPv4 addresses and the group itself:
// the VPC's default group, which production services run in, admits the whole
// VPC on each service's port, so a preview — code nobody has reviewed yet — must
// not be able to send to private addresses. It still reaches hosted services and
// APIs through the VPC's NAT gateway. The deletes of the stack's services remove it once nothing uses it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

// previewTaskTagKey tags a Task's preview stack group with the Task's id. A delete
// removes a group only when it carries this tag.
const previewTaskTagKey = "deployment.io preview task"

// previewStackSecurityGroupName is the stack group's name.
func previewStackSecurityGroupName(taskID string) string {
	return "pvsg-" + taskID
}

// previewStackSecurityGroupAPI is the part of *ec2.Client finding or creating the
// stack group uses.
type previewStackSecurityGroupAPI interface {
	DescribeSecurityGroups(ctx context.Context, params *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	CreateSecurityGroup(ctx context.Context, params *ec2.CreateSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, params *ec2.AuthorizeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	AuthorizeSecurityGroupEgress(ctx context.Context, params *ec2.AuthorizeSecurityGroupEgressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error)
	RevokeSecurityGroupEgress(ctx context.Context, params *ec2.RevokeSecurityGroupEgressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error)
	DeleteSecurityGroup(ctx context.Context, params *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// previewStack is a Task's preview stack in a VPC, and the port its database
// listens on (0: the stack has no database, so no rule for one).
type previewStack struct {
	vpcID  string
	taskID string
	port   int64
}

// ensurePreviewStackSecurityGroup finds the Task's stack group in the VPC (by name
// and VPC), creating it when missing — a concurrent create (DuplicateGroup) is
// looked up again — and makes sure it admits the database's port from itself and
// sends only to public addresses and itself (ensurePreviewStackEgress). Its id,
// and whether this call created it. A group created here that can't get its
// rules is removed again: nothing would ever find it to delete it.
func ensurePreviewStackSecurityGroup(ec2Client previewStackSecurityGroupAPI, stack previewStack, logsWriter io.Writer) (string, bool, error) {
	vpcID, taskID, port := stack.vpcID, stack.taskID, stack.port
	if taskID == "" || vpcID == "" {
		return "", false, errors.New("the preview's security group needs a Task and a VPC")
	}
	name := previewStackSecurityGroupName(taskID)
	group, found, err := findPreviewStackSecurityGroup(ec2Client, vpcID, name)
	if err != nil {
		return "", false, err
	}
	created := false
	if !found {
		createOutput, err := ec2Client.CreateSecurityGroup(context.TODO(), &ec2.CreateSecurityGroupInput{
			Description: aws.String(fmt.Sprintf("deployment.io preview stack of Task %s", taskID)),
			GroupName:   aws.String(name),
			VpcId:       aws.String(vpcID),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSecurityGroup,
				Tags: []ec2Types.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
					{Key: aws.String("created by"), Value: aws.String("deployment.io")},
					{Key: aws.String(previewTaskTagKey), Value: aws.String(taskID)},
				},
			}},
		})
		switch {
		case err == nil:
			group, created = ec2Types.SecurityGroup{GroupId: createOutput.GroupId}, true
			io.WriteString(logsWriter, fmt.Sprintf("Created the preview's security group %s (%s)\n", name, aws.ToString(createOutput.GroupId)))
		case isSecurityGroupErrorCode(err, "InvalidGroup.Duplicate"):
			group, found, err = findPreviewStackSecurityGroup(ec2Client, vpcID, name)
			if err != nil {
				return "", false, err
			}
			if !found {
				return "", false, fmt.Errorf("security group %s exists but couldn't be found", name)
			}
		default:
			return "", false, fmt.Errorf("create security group %s: %w", name, err)
		}
	}
	groupID := aws.ToString(group.GroupId)
	if port != 0 && !hasSelfIngressRule(group, groupID, port) {
		_, err = ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(groupID),
			IpPermissions: []ec2Types.IpPermission{{
				IpProtocol:       aws.String("tcp"),
				FromPort:         aws.Int32(int32(port)),
				ToPort:           aws.Int32(int32(port)),
				UserIdGroupPairs: []ec2Types.UserIdGroupPair{{GroupId: aws.String(groupID), Description: aws.String(previewStackSelfRuleDescription)}},
			}},
		})
		if err != nil && !isDuplicateSecurityGroupRuleError(err) {
			if created {
				removeUnusedPreviewStackSecurityGroup(ec2Client, groupID, logsWriter)
			}
			return "", false, fmt.Errorf("allow port %d within security group %s: %w", port, groupID, err)
		}
	}
	if err := ensurePreviewStackEgress(ec2Client, group, groupID, created, logsWriter); err != nil {
		if created {
			removeUnusedPreviewStackSecurityGroup(ec2Client, groupID, logsWriter)
		}
		return "", false, fmt.Errorf("limit outbound traffic of security group %s: %w", groupID, err)
	}
	return groupID, created, nil
}

// previewStackSelfRuleDescription describes the stack group's rules to itself.
const previewStackSelfRuleDescription = "the preview stack"

// previewStackInternetRuleDescription describes the stack group's outbound rules
// to previewStackInternetCIDRs.
const previewStackInternetRuleDescription = "the internet (not private addresses)"

// previewStackInternetCIDRs are every IPv4 address except 10.0.0.0/8,
// 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 and 169.254.0.0/16: the addresses
// a preview may send to besides its own stack.
var previewStackInternetCIDRs = []string{
	"0.0.0.0/5", "8.0.0.0/7", "11.0.0.0/8", "12.0.0.0/6", "16.0.0.0/4", "32.0.0.0/3", "64.0.0.0/3",
	"96.0.0.0/6", "100.0.0.0/10", "100.128.0.0/9", "101.0.0.0/8", "102.0.0.0/7", "104.0.0.0/5",
	"112.0.0.0/4", "128.0.0.0/3", "160.0.0.0/5", "168.0.0.0/8", "169.0.0.0/9", "169.128.0.0/10",
	"169.192.0.0/11", "169.224.0.0/12", "169.240.0.0/13", "169.248.0.0/14", "169.252.0.0/15",
	"169.255.0.0/16", "170.0.0.0/7", "172.0.0.0/12", "172.32.0.0/11", "172.64.0.0/10",
	"172.128.0.0/9", "173.0.0.0/8", "174.0.0.0/7", "176.0.0.0/4", "192.0.0.0/9", "192.128.0.0/11",
	"192.160.0.0/13", "192.169.0.0/16", "192.170.0.0/15", "192.172.0.0/14", "192.176.0.0/12",
	"192.192.0.0/10", "193.0.0.0/8", "194.0.0.0/7", "196.0.0.0/6", "200.0.0.0/5", "208.0.0.0/4",
	"224.0.0.0/3",
}

// ensurePreviewStackEgress makes group (described, or {GroupId} only when this
// call created it) send all traffic to previewStackInternetCIDRs and to itself,
// and nothing to 0.0.0.0/0 or ::/0. The new rules are added before the open ones
// are removed, so a running preview never loses internet access in between. No
// other rule, and no inbound rule, is touched.
func ensurePreviewStackEgress(ec2Client previewStackSecurityGroupAPI, group ec2Types.SecurityGroup, groupID string, created bool, logsWriter io.Writer) error {
	changed := false
	if missing := missingPreviewStackEgress(group, groupID); len(missing) > 0 {
		_, err := ec2Client.AuthorizeSecurityGroupEgress(context.TODO(), &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId: aws.String(groupID), IpPermissions: missing,
		})
		if isDuplicateSecurityGroupRuleError(err) {
			// Some of them were added meanwhile (a concurrent deploy): add what's still missing.
			out, describeErr := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{GroupIds: []string{groupID}})
			if describeErr != nil {
				return fmt.Errorf("describe security group %s: %w", groupID, describeErr)
			}
			if len(out.SecurityGroups) == 0 {
				return fmt.Errorf("security group %s couldn't be found", groupID)
			}
			err = nil
			if missing := missingPreviewStackEgress(out.SecurityGroups[0], groupID); len(missing) > 0 {
				_, err = ec2Client.AuthorizeSecurityGroupEgress(context.TODO(), &ec2.AuthorizeSecurityGroupEgressInput{
					GroupId: aws.String(groupID), IpPermissions: missing,
				})
			}
		}
		if err != nil {
			return err
		}
		changed = true
	}

	var open []ec2Types.IpPermission
	if created {
		// AWS's allow-all rule, which the created group isn't described with.
		open = append(open, ec2Types.IpPermission{IpProtocol: aws.String("-1"), IpRanges: []ec2Types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}})
	}
	for _, permission := range group.IpPermissionsEgress {
		for _, r := range permission.IpRanges {
			if aws.ToString(r.CidrIp) == "0.0.0.0/0" {
				open = append(open, ec2Types.IpPermission{IpProtocol: permission.IpProtocol, FromPort: permission.FromPort, ToPort: permission.ToPort,
					IpRanges: []ec2Types.IpRange{{CidrIp: r.CidrIp}}})
			}
		}
		for _, r := range permission.Ipv6Ranges {
			if aws.ToString(r.CidrIpv6) == "::/0" {
				open = append(open, ec2Types.IpPermission{IpProtocol: permission.IpProtocol, FromPort: permission.FromPort, ToPort: permission.ToPort,
					Ipv6Ranges: []ec2Types.Ipv6Range{{CidrIpv6: r.CidrIpv6}}})
			}
		}
	}
	for _, permission := range open {
		_, err := ec2Client.RevokeSecurityGroupEgress(context.TODO(), &ec2.RevokeSecurityGroupEgressInput{
			GroupId: aws.String(groupID), IpPermissions: []ec2Types.IpPermission{permission},
		})
		if err != nil && !isSecurityGroupErrorCode(err, "InvalidPermission.NotFound") {
			return err
		}
		changed = true
	}

	if changed {
		io.WriteString(logsWriter, fmt.Sprintf("Limited the preview's outbound traffic to the internet and its own stack (security group %s)\n", groupID))
	}
	return nil
}

// missingPreviewStackEgress are the outbound rules of ensurePreviewStackEgress
// that group lacks: a protocol "-1" rule for each of previewStackInternetCIDRs
// and one to the group itself.
func missingPreviewStackEgress(group ec2Types.SecurityGroup, groupID string) []ec2Types.IpPermission {
	cidrs, self := map[string]bool{}, false
	for _, permission := range group.IpPermissionsEgress {
		if aws.ToString(permission.IpProtocol) != "-1" {
			continue
		}
		for _, r := range permission.IpRanges {
			cidrs[aws.ToString(r.CidrIp)] = true
		}
		for _, pair := range permission.UserIdGroupPairs {
			if aws.ToString(pair.GroupId) == groupID {
				self = true
			}
		}
	}
	var ranges []ec2Types.IpRange
	for _, cidr := range previewStackInternetCIDRs {
		if !cidrs[cidr] {
			ranges = append(ranges, ec2Types.IpRange{CidrIp: aws.String(cidr), Description: aws.String(previewStackInternetRuleDescription)})
		}
	}
	var missing []ec2Types.IpPermission
	if len(ranges) > 0 {
		missing = append(missing, ec2Types.IpPermission{IpProtocol: aws.String("-1"), IpRanges: ranges})
	}
	if !self {
		missing = append(missing, ec2Types.IpPermission{IpProtocol: aws.String("-1"),
			UserIdGroupPairs: []ec2Types.UserIdGroupPair{{GroupId: aws.String(groupID), Description: aws.String(previewStackSelfRuleDescription)}}})
	}
	return missing
}

// removeUnusedPreviewStackSecurityGroup deletes a stack group a failed deploy
// created before any service ran in it: the deletes find the group only through
// the stack's services, so it would be left behind. One attempt: a group
// something already uses (DependencyViolation) is kept, and that service's
// delete removes it.
func removeUnusedPreviewStackSecurityGroup(ec2Client previewStackSecurityGroupAPI, groupID string, logsWriter io.Writer) {
	_, err := ec2Client.DeleteSecurityGroup(context.TODO(), &ec2.DeleteSecurityGroupInput{GroupId: aws.String(groupID)})
	switch {
	case err == nil:
		io.WriteString(logsWriter, fmt.Sprintf("Removed the preview's security group %s again: the deploy failed before anything used it\n", groupID))
	case isSecurityGroupNotFoundError(err):
	case isSecurityGroupErrorCode(err, "DependencyViolation"):
		io.WriteString(logsWriter, fmt.Sprintf("Kept the preview's security group %s: something already uses it\n", groupID))
	default:
		io.WriteString(logsWriter, fmt.Sprintf("Couldn't remove the preview's security group %s: %s\n", groupID, err))
	}
}

func findPreviewStackSecurityGroup(ec2Client previewStackSecurityGroupAPI, vpcID, name string) (ec2Types.SecurityGroup, bool, error) {
	out, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2Types.Filter{
			{Name: aws.String("group-name"), Values: []string{name}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return ec2Types.SecurityGroup{}, false, fmt.Errorf("find security group %s: %w", name, err)
	}
	for _, g := range out.SecurityGroups {
		if aws.ToString(g.GroupName) == name && aws.ToString(g.VpcId) == vpcID {
			return g, true, nil
		}
	}
	return ec2Types.SecurityGroup{}, false, nil
}

// hasSelfIngressRule reports whether group admits tcp port from itself.
func hasSelfIngressRule(group ec2Types.SecurityGroup, groupID string, port int64) bool {
	for _, p := range group.IpPermissions {
		if aws.ToString(p.IpProtocol) != "tcp" || int64(aws.ToInt32(p.FromPort)) > port || int64(aws.ToInt32(p.ToPort)) < port {
			continue
		}
		for _, pair := range p.UserIdGroupPairs {
			if aws.ToString(pair.GroupId) == groupID {
				return true
			}
		}
	}
	return false
}

func isSecurityGroupErrorCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}

// serviceSecurityGroupIDs are the awsvpc security groups of a described service.
func serviceSecurityGroupIDs(service *ecsTypes.Service) []string {
	if service == nil || service.NetworkConfiguration == nil || service.NetworkConfiguration.AwsvpcConfiguration == nil {
		return nil
	}
	return append([]string(nil), service.NetworkConfiguration.AwsvpcConfiguration.SecurityGroups...)
}

// securityGroupDeleteAPI is the part of *ec2.Client removing a stack group uses.
type securityGroupDeleteAPI interface {
	DescribeSecurityGroups(ctx context.Context, params *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, params *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// Swapped by tests so retrying runs without real time passing.
var (
	previewSecurityGroupDeletePause   = 15 * time.Second
	previewSecurityGroupDeleteTimeout = 3 * time.Minute
	previewSecurityGroupDeleteSleep   = time.Sleep
)

// deletePreviewStackSecurityGroups deletes those of groupIDs (a deleted service's
// groups) that carry the preview task tag — never any other group, so never the
// VPC's default one. A group still in use (the stack's other service, or network
// interfaces released later) is retried for a while and then left to the other
// service's delete. It never fails the delete.
func deletePreviewStackSecurityGroups(ec2Client securityGroupDeleteAPI, groupIDs []string, logsWriter io.Writer) {
	for _, groupID := range groupIDs {
		if groupID == "" {
			continue
		}
		out, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{GroupIds: []string{groupID}})
		if isSecurityGroupNotFoundError(err) {
			continue
		}
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Couldn't check security group %s: %s\n", groupID, err))
			continue
		}
		if len(out.SecurityGroups) == 0 || !isPreviewStackSecurityGroup(out.SecurityGroups[0]) {
			continue
		}
		deletePreviewStackSecurityGroup(ec2Client, groupID, logsWriter)
	}
}

// isPreviewStackSecurityGroup reports whether group is a Task's preview stack group.
func isPreviewStackSecurityGroup(group ec2Types.SecurityGroup) bool {
	if aws.ToString(group.GroupName) == "default" {
		return false
	}
	for _, tag := range group.Tags {
		if aws.ToString(tag.Key) == previewTaskTagKey && aws.ToString(tag.Value) != "" {
			return true
		}
	}
	return false
}

func deletePreviewStackSecurityGroup(ec2Client securityGroupDeleteAPI, groupID string, logsWriter io.Writer) {
	io.WriteString(logsWriter, fmt.Sprintf("Deleting the preview's security group: %s\n", groupID))
	retries := int(previewSecurityGroupDeleteTimeout / previewSecurityGroupDeletePause)
	for attempt := 0; ; attempt++ {
		_, err := ec2Client.DeleteSecurityGroup(context.TODO(), &ec2.DeleteSecurityGroupInput{GroupId: aws.String(groupID)})
		switch {
		case err == nil:
			return
		case isSecurityGroupNotFoundError(err):
			logAlreadyGone(logsWriter, "Security group", groupID)
			return
		case isSecurityGroupErrorCode(err, "DependencyViolation"):
			if attempt < retries {
				previewSecurityGroupDeleteSleep(previewSecurityGroupDeletePause)
				continue
			}
			io.WriteString(logsWriter, fmt.Sprintf("Security group %s is still in use; the preview's other service removes it\n", groupID))
			return
		default:
			io.WriteString(logsWriter, fmt.Sprintf("Couldn't delete security group %s: %s\n", groupID, err))
			return
		}
	}
}

// previewStackIngressAPI is the part of *ec2.Client admitting the API's port into
// the stack group uses.
type previewStackIngressAPI interface {
	AuthorizeSecurityGroupIngress(ctx context.Context, params *ec2.AuthorizeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
}

// allowPreviewStackPortFromLoadBalancer admits the preview's port into its stack
// group (TaskSecurityGroupIds, exactly one) from its load balancer's group: the
// load balancer reaches the API through the stack group, never the VPC's default
// one. A rule that already exists is fine.
func allowPreviewStackPortFromLoadBalancer(parameters map[string]interface{}, ec2Client previewStackIngressAPI, loadBalancerGroupID string) error {
	groups, err := getTaskSecurityGroupIds(parameters)
	if err != nil {
		return err
	}
	if len(groups) != 1 {
		return fmt.Errorf("a backend preview runs in exactly one security group, its stack's (got %d)", len(groups))
	}
	port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
	if err != nil {
		return err
	}
	_, err = ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(groups[0]),
		IpPermissions: []ec2Types.IpPermission{{
			IpProtocol:       aws.String("tcp"),
			FromPort:         aws.Int32(int32(port)),
			ToPort:           aws.Int32(int32(port)),
			UserIdGroupPairs: []ec2Types.UserIdGroupPair{{GroupId: aws.String(loadBalancerGroupID), Description: aws.String("preview load balancer")}},
		}},
	})
	if err != nil && !isDuplicateSecurityGroupRuleError(err) {
		return fmt.Errorf("allow port %d into security group %s from the load balancer: %w", port, groups[0], err)
	}
	return nil
}

// previewStackRevokeAPI is the part of *ec2.Client removing the stack group's rule
// for a load balancer's group uses.
type previewStackRevokeAPI interface {
	DescribeSecurityGroups(ctx context.Context, params *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, params *ec2.RevokeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
}

// revokePreviewStackIngressFrom removes, from those of groupIDs that are preview
// stack groups, every rule admitting traffic from sourceGroupID (a preview's load
// balancer group, about to be deleted: EC2 refuses to delete a group another
// group's rule references). It never fails the delete; a rule left behind makes
// that group's delete fail, which the job reports as before.
func revokePreviewStackIngressFrom(ec2Client previewStackRevokeAPI, groupIDs []string, sourceGroupID string, logsWriter io.Writer) {
	for _, groupID := range groupIDs {
		if groupID == "" || groupID == sourceGroupID {
			continue
		}
		out, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{GroupIds: []string{groupID}})
		if isSecurityGroupNotFoundError(err) {
			continue
		}
		if err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Couldn't check security group %s: %s\n", groupID, err))
			continue
		}
		if len(out.SecurityGroups) == 0 || !isPreviewStackSecurityGroup(out.SecurityGroups[0]) {
			continue
		}
		for _, permission := range out.SecurityGroups[0].IpPermissions {
			var pairs []ec2Types.UserIdGroupPair
			for _, pair := range permission.UserIdGroupPairs {
				if aws.ToString(pair.GroupId) == sourceGroupID {
					pairs = append(pairs, ec2Types.UserIdGroupPair{GroupId: pair.GroupId})
				}
			}
			if len(pairs) == 0 {
				continue
			}
			_, err := ec2Client.RevokeSecurityGroupIngress(context.TODO(), &ec2.RevokeSecurityGroupIngressInput{
				GroupId: aws.String(groupID),
				IpPermissions: []ec2Types.IpPermission{{
					IpProtocol:       permission.IpProtocol,
					FromPort:         permission.FromPort,
					ToPort:           permission.ToPort,
					UserIdGroupPairs: pairs,
				}},
			})
			if err != nil && !isSecurityGroupNotFoundError(err) {
				io.WriteString(logsWriter, fmt.Sprintf("Couldn't remove security group %s's rule for %s: %s\n", groupID, sourceGroupID, err))
			}
		}
	}
}
