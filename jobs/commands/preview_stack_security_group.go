package commands

// preview_stack_security_group.go is the security group of a Task's backend preview
// stack, pvsg-<taskID>: the preview database runs in it alone, the API preview in it
// and the VPC's default group, and it admits the database's port only from itself —
// so only that Task's preview reaches its database. The deletes of the stack's
// services remove it once nothing uses it.

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
	DeleteSecurityGroup(ctx context.Context, params *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// previewStack is a Task's preview stack in a VPC, and the port its database
// listens on.
type previewStack struct {
	vpcID  string
	taskID string
	port   int64
}

// ensurePreviewStackSecurityGroup finds the Task's stack group in the VPC (by name
// and VPC), creating it when missing — a concurrent create (DuplicateGroup) is
// looked up again — and makes sure it admits the database's port from itself. Its
// id, and whether this call created it. A group created here that can't get its
// rule is removed again: nothing would ever find it to delete it.
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
	if hasSelfIngressRule(group, groupID, port) {
		return groupID, created, nil
	}
	_, err = ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(groupID),
		IpPermissions: []ec2Types.IpPermission{{
			IpProtocol:       aws.String("tcp"),
			FromPort:         aws.Int32(int32(port)),
			ToPort:           aws.Int32(int32(port)),
			UserIdGroupPairs: []ec2Types.UserIdGroupPair{{GroupId: aws.String(groupID), Description: aws.String("the preview stack")}},
		}},
	})
	if err != nil && !isDuplicateSecurityGroupRuleError(err) {
		if created {
			removeUnusedPreviewStackSecurityGroup(ec2Client, groupID, logsWriter)
		}
		return "", false, fmt.Errorf("allow port %d within security group %s: %w", port, groupID, err)
	}
	return groupID, created, nil
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
