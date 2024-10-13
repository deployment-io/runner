package commands

import (
	"bytes"
	"context"
	"fmt"
	md "github.com/ankit-arora/markdown"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatch_types "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"io"
	"time"
)

// ListCloudWatchMetricsAwsEcsWebService sends only CPU and memory utilization for now
type ListCloudWatchMetricsAwsEcsWebService struct {
}

func (l *ListCloudWatchMetricsAwsEcsWebService) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	io.WriteString(logsWriter, fmt.Sprintf("Listing cloudwatch metrics for web service\n"))
	cloudwatchClient, err := cloud_api_clients.GetCloudwatchClient(parameters)
	if err != nil {
		return parameters, err
	}
	ecsClusterName, err := jobs.GetParameterValue[string](parameters, parameters_enums.EcsClusterName)
	if err != nil {
		return parameters, nil
	}
	ecsServiceName, err := getEcsServiceName(parameters)
	if err != nil {
		return parameters, err
	}
	now := time.Now()
	before := now.Add(-10 * time.Minute)
	getMetricDataOutput, err := cloudwatchClient.GetMetricData(context.TODO(), &cloudwatch.GetMetricDataInput{
		EndTime: &now,
		MetricDataQueries: []cloudwatch_types.MetricDataQuery{
			{
				Id: aws.String("cpu1"),
				MetricStat: &cloudwatch_types.MetricStat{
					Metric: &cloudwatch_types.Metric{
						Dimensions: []cloudwatch_types.Dimension{
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
					Unit:   cloudwatch_types.StandardUnitPercent,
				},
				ReturnData: aws.Bool(true),
			},
			{
				Id: aws.String("mem1"),
				MetricStat: &cloudwatch_types.MetricStat{
					Metric: &cloudwatch_types.Metric{
						Dimensions: []cloudwatch_types.Dimension{
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
					Unit:   cloudwatch_types.StandardUnitPercent,
				},
				ReturnData: aws.Bool(true),
			},
		},
		StartTime: &before,
	})

	if err != nil {
		return parameters, err
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

	shouldSendChatOutput, err := jobs.GetParameterValue[bool](parameters, parameters_enums.ShouldSendChatOutput)
	if err != nil {
		return parameters, err
	}

	if shouldSendChatOutput {
		//send the results back
		var jobID string
		jobID, err = jobs.GetParameterValue[string](parameters, parameters_enums.JobID)
		if err != nil {
			return parameters, err
		}
		buf := new(bytes.Buffer)
		err = md.NewMarkdown(buf).Table(md.TableSet{
			Header: []string{"Cpu", "Memory"},
			Rows: [][]string{
				{fmt.Sprintf("%f%%", latestCpuValue), fmt.Sprintf("%f%%", latestMemoryValue)},
			},
		}).Build()
		if err != nil {
			return parameters, err
		}
		var organizationIdFromJob string
		organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
		if err != nil {
			return parameters, err
		}
		updateJobOutputPipeline.Add(organizationIdFromJob, jobs.UpdateJobOutputDtoV1{
			ID:     jobID,
			Output: buf.String(),
		})
	}

	return parameters, err
}
