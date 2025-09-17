package commands

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"

	"io"
)

type AddAwsWebServiceDomain struct {
}

func (a *AddAwsWebServiceDomain) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {

	elbClient, err := cloud_api_clients.GetElbClient(parameters)
	if err != nil {
		return parameters, err
	}

	var listenerPort int32 = 443
	albListenerName, err := getAlbListenerName(parameters, listenerPort)
	if err != nil {
		return parameters, err
	}

	loadBalancerArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.LoadBalancerArn)
	if err != nil {
		return parameters, err
	}

	targetGroupArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.TargetGroupArn)
	if err != nil {
		return parameters, err
	}

	certificateArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.AcmCertificateArn)
	if err != nil {
		return parameters, err
	}

	describeListenersOutput, err := elbClient.DescribeListeners(context.TODO(), &elasticloadbalancingv2.DescribeListenersInput{
		LoadBalancerArn: aws.String(loadBalancerArn),
	})
	var listenerArn string
	if describeListenersOutput != nil {
		for _, listener := range describeListenersOutput.Listeners {
			p := aws.ToInt32(listener.Port)
			if p == listenerPort {
				//if listener with port 443 already exists
				listenerArn = aws.ToString(listener.ListenerArn)
			}
		}
	}

	if len(listenerArn) > 0 {
		//update
		io.WriteString(logsWriter, fmt.Sprintf("Adding certificate %s for alb https listener: %s\n", certificateArn, listenerArn))
		_, err := elbClient.ModifyListener(context.TODO(), &elasticloadbalancingv2.ModifyListenerInput{
			ListenerArn: aws.String(listenerArn),
			Certificates: []elbTypes.Certificate{{
				CertificateArn: aws.String(certificateArn),
			}},
			SslPolicy: aws.String("ELBSecurityPolicy-TLS13-1-2-2021-06"),
		})
		if err != nil {
			return parameters, err
		}
	} else {
		//create
		io.WriteString(logsWriter, fmt.Sprintf("Creating new https alb listener with certificate: %s\n", certificateArn))
		createListenerInput := &elasticloadbalancingv2.CreateListenerInput{
			DefaultActions: []elbTypes.Action{{
				Type:           elbTypes.ActionTypeEnumForward,
				Order:          aws.Int32(1),
				TargetGroupArn: aws.String(targetGroupArn),
			}},
			LoadBalancerArn: aws.String(loadBalancerArn),
			Certificates: []elbTypes.Certificate{{
				CertificateArn: aws.String(certificateArn),
			}},
			SslPolicy: aws.String("ELBSecurityPolicy-TLS13-1-2-2021-06"),
			Port:      aws.Int32(listenerPort),
			Protocol:  elbTypes.ProtocolEnumHttps,
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
			return parameters, err
		}
		listenerArn = aws.ToString(createListenerOutput.Listeners[0].ListenerArn)
		io.WriteString(logsWriter, fmt.Sprintf("New alb https listener created: %s\n", listenerArn))
	}

	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return parameters, err
	}
	organizationIdFromJob, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}
	commandUtils.UpdateDeploymentsPipeline.Add(organizationIdFromJob, deployments.UpdateDeploymentDtoV1{
		ID:                 deploymentID,
		ListenerArnPort443: listenerArn,
	})

	return parameters, err

}
