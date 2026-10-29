package cloudwatch

import (
	"context"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

// RetentionClient is the part of the cloudwatchlogs client SetMissingRetention needs.
type RetentionClient interface {
	DescribeLogGroups(ctx context.Context, params *cloudwatchlogs.DescribeLogGroupsInput,
		optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error)
	PutRetentionPolicy(ctx context.Context, params *cloudwatchlogs.PutRetentionPolicyInput,
		optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutRetentionPolicyOutput, error)
}

// SetMissingRetention sets logGroupRetentionDays on every log group whose name starts with prefix and
// that has no retention. Groups that already have a retention are left alone. Errors are logged, never
// returned: a failed group doesn't stop the rest and a failed listing ends the sweep.
// It returns the number of groups updated and the number of groups listed.
//
// A prefix with nothing before its trailing "/" (an empty organization ID) is refused: "/" matches
// groups the runner never created, such as /aws/lambda/..., and would expire their logs.
func SetMissingRetention(ctx context.Context, client RetentionClient, prefix string) (updated, total int) {
	if strings.TrimSuffix(prefix, "/") == "" {
		log.Printf("log group retention: skipped, no organization ID in prefix %q", prefix)
		return 0, 0
	}
	paginator := cloudwatchlogs.NewDescribeLogGroupsPaginator(client, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("log group retention: error listing log groups under %s: %s", prefix, err)
			break
		}
		for _, logGroup := range page.LogGroups {
			total++
			if logGroup.RetentionInDays != nil {
				continue
			}
			_, err = client.PutRetentionPolicy(ctx, &cloudwatchlogs.PutRetentionPolicyInput{
				LogGroupName:    logGroup.LogGroupName,
				RetentionInDays: aws.Int32(logGroupRetentionDays),
			})
			if err != nil {
				log.Printf("log group retention: error setting retention on %s: %s",
					aws.ToString(logGroup.LogGroupName), err)
				continue
			}
			updated++
		}
	}
	log.Printf("log group retention: set %d days on %d of %d log groups under %s",
		logGroupRetentionDays, updated, total, prefix)
	return updated, total
}
