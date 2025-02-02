package get_cpu_memory_usage

import (
	"context"
	"fmt"
	"github.com/ankit-arora/langchaingo/callbacks"
	"github.com/ankit-arora/langchaingo/tools"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/automation_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils/aws_utils"
	"io"
	"time"
)

type Tool struct {
	Parameters       map[string]interface{}
	LogsWriter       io.Writer
	CallbacksHandler callbacks.Handler
	Entities         []automation_enums.Entity
}

func (t *Tool) entitiesString() string {
	entities := ""
	for index, entity := range t.Entities {
		entities += entity.String()
		if index < len(t.Entities)-1 {
			entities += ", "
		}
	}
	return entities
}

func (t *Tool) Name() string {
	return "getCpuAndMemoryUsage"
}

func (t *Tool) Description() string {
	entitiesString := t.entitiesString()
	return fmt.Sprintf("Gets cpu or memory usage metrics for %s. "+
		"This function doesn't require a name or any other input.", entitiesString)
}

func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	if t.CallbacksHandler != nil {
		info := fmt.Sprintf("Getting CPU and memory usage metrics")
		t.CallbacksHandler.HandleToolStart(ctx, info)
	}
	//TODO assume that services use ECS for now
	cloudwatchClient, err := cloud_api_clients.GetCloudwatchClient(t.Parameters)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting Cloudwatch client: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	ecsClusterName, err := jobs.GetParameterValue[string](t.Parameters, parameters_enums.EcsClusterName)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting ECS cluster name: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	ecsServiceName, err := aws_utils.GetEcsServiceName(t.Parameters)
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting ECS service name: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	now := time.Now()
	before := now.Add(-10 * time.Minute)
	getMetricDataOutput, err := cloudwatchClient.GetMetricData(context.TODO(), &cloudwatch.GetMetricDataInput{
		EndTime: &now,
		MetricDataQueries: []cloudwatchTypes.MetricDataQuery{
			{
				Id: aws.String("cpu1"),
				MetricStat: &cloudwatchTypes.MetricStat{
					Metric: &cloudwatchTypes.Metric{
						Dimensions: []cloudwatchTypes.Dimension{
							{
								Name:  aws.String("ServiceName"),
								Value: aws.String(ecsServiceName),
							},
							{
								Name:  aws.String("ClusterName"),
								Value: aws.String(ecsClusterName),
							},
						},
						MetricName: aws.String("CPUUtilization"),
						Namespace:  aws.String("AWS/ECS"),
					},
					Period: aws.Int32(60),
					Stat:   aws.String("Average"),
					Unit:   cloudwatchTypes.StandardUnitPercent,
				},
				ReturnData: aws.Bool(true),
			},
			{
				Id: aws.String("mem1"),
				MetricStat: &cloudwatchTypes.MetricStat{
					Metric: &cloudwatchTypes.Metric{
						Dimensions: []cloudwatchTypes.Dimension{
							{
								Name:  aws.String("ServiceName"),
								Value: aws.String(ecsServiceName),
							},
							{
								Name:  aws.String("ClusterName"),
								Value: aws.String(ecsClusterName),
							},
						},
						MetricName: aws.String("MemoryUtilization"),
						Namespace:  aws.String("AWS/ECS"),
					},
					Period: aws.Int32(60),
					Stat:   aws.String("Average"),
					Unit:   cloudwatchTypes.StandardUnitPercent,
				},
				ReturnData: aws.Bool(true),
			},
		},
		StartTime: &before,
	})
	if err != nil {
		if t.CallbacksHandler != nil {
			t.CallbacksHandler.HandleToolError(ctx, fmt.Errorf("error getting CPU and memory usage data: %s", err))
		}
		return "There was an error. We'll get back to you.", nil
	}
	var latestCpuValue float64
	var latestCpuTimestamp time.Time
	var latestMemoryValue float64
	var latestMemoryTimestamp time.Time
	for _, metricDataResult := range getMetricDataOutput.MetricDataResults {
		if *metricDataResult.Id == "cpu1" {
			for i, timestamp := range metricDataResult.Timestamps {
				if latestCpuTimestamp.IsZero() || timestamp.After(latestCpuTimestamp) {
					latestCpuTimestamp = timestamp
					latestCpuValue = metricDataResult.Values[i]
				}
			}
		}
		if *metricDataResult.Id == "mem1" {
			for i, timestamp := range metricDataResult.Timestamps {
				if latestMemoryTimestamp.IsZero() || timestamp.After(latestMemoryTimestamp) {
					latestMemoryTimestamp = timestamp
					latestMemoryValue = metricDataResult.Values[i]
				}
			}
		}
	}
	out := fmt.Sprintf("cpu usage is %f%% and memory usage is %f%%", latestCpuValue, latestMemoryValue)
	info := fmt.Sprintf("Exiting get cpu and memory usage tool with output: %s", out)
	if t.CallbacksHandler != nil {
		t.CallbacksHandler.HandleToolEnd(ctx, info)
	}
	return out, nil
}

var _ tools.Tool = &Tool{}
