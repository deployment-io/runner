package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	acmTypes "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/deployment-io/deployment-runner-kit/certificates"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"github.com/deployment-io/deployment-runner/utils"
)

type CreateAcmCertificate struct {
}

func (c *CreateAcmCertificate) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {

	//check and add policy for AWS ACM certificate creation
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return parameters, err
	}
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsCertificateManager,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
	if err != nil {
		return parameters, err
	}

	acmClient, err := cloud_api_clients.GetAcmClient(parameters)
	if err != nil {
		return parameters, err
	}
	certificateDomain, err := jobs.GetParameterValue[string](parameters, parameters_enums.CertificateDomain)
	if err != nil {
		return parameters, err
	}
	parentDomain, err := jobs.GetParameterValue[string](parameters, parameters_enums.ParentDomain)
	if err != nil {
		return parameters, err
	}
	certificateID, err := jobs.GetParameterValue[string](parameters, parameters_enums.CertificateID)
	if err != nil {
		return parameters, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Requesting certificate from ACM for domain: %s\n", certificateDomain))
	requestCertificateOutput, err := acmClient.RequestCertificate(context.TODO(), &acm.RequestCertificateInput{
		DomainName:       aws.String(certificateDomain),
		IdempotencyToken: aws.String(certificateID), //will be unique for each creation request
		KeyAlgorithm:     acmTypes.KeyAlgorithmRsa2048,
		Options:          &acmTypes.CertificateOptions{CertificateTransparencyLoggingPreference: acmTypes.CertificateTransparencyLoggingPreferenceEnabled},
		ValidationMethod: acmTypes.ValidationMethodDns,
		SubjectAlternativeNames: []string{
			parentDomain,
		},
		Tags: []acmTypes.Tag{
			{
				Key:   aws.String("created by"),
				Value: aws.String("deployment.io"),
			},
		},
	})
	if err != nil {
		return parameters, err
	}
	var organizationIdFromJob string
	organizationIdFromJob, err = jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIdFromJob)
	if err != nil {
		return parameters, err
	}
	certificateArn := requestCertificateOutput.CertificateArn
	go func() {
		//wait till verification cnames are ready and send them back
		maxRetries := 200
		retryCount := 0
		for retryCount < maxRetries {
			describeCertificateOutput, err := acmClient.DescribeCertificate(context.TODO(), &acm.DescribeCertificateInput{CertificateArn: certificateArn})
			if err == nil {
				if describeCertificateOutput != nil && describeCertificateOutput.Certificate != nil &&
					len(describeCertificateOutput.Certificate.DomainValidationOptions) > 0 &&
					describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord != nil &&
					describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Name != nil &&
					describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Value != nil &&
					len(aws.ToString(describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Name)) > 0 &&
					len(aws.ToString(describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Value)) > 0 {

					commandUtils.UpdateCertificatesPipeline.Add(organizationIdFromJob, certificates.UpdateCertificateDtoV1{
						ID:         certificateID,
						CnameName:  aws.ToString(describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Name),
						CnameValue: aws.ToString(describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Value),
					})

					return
				}
			}
			retryCount++
			time.Sleep(5 * time.Second)
		}
	}()

	io.WriteString(logsWriter, fmt.Sprintf("Got certificate from ACM: %s\n", aws.ToString(certificateArn)))
	commandUtils.UpdateCertificatesPipeline.Add(organizationIdFromJob, certificates.UpdateCertificateDtoV1{
		ID:             certificateID,
		CertificateArn: aws.ToString(certificateArn),
	})
	jobs.SetParameterValue[string](parameters, parameters_enums.AcmCertificateArn, aws.ToString(certificateArn))
	return parameters, err
}
