package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	acm_types "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/deployment-io/deployment-runner-kit/certificates"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner/utils"
	"io"
	"time"
)

type VerifyAcmCertificate struct {
}

func (v *VerifyAcmCertificate) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	//check and add policy for AWS ACM certificate verification
	runnerData := utils.RunnerData.Get()
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
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
	certificateArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.AcmCertificateArn)
	if err != nil {
		return parameters, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Verifying and validating certificate from ACM: %s\n", certificateArn))
	certificateID, err := jobs.GetParameterValue[string](parameters, parameters_enums.CertificateID)
	if err != nil {
		return parameters, err
	}
	//get status of certificate
	describeCertificateOutput, err := acmClient.DescribeCertificate(context.TODO(), &acm.DescribeCertificateInput{CertificateArn: aws.String(certificateArn)})
	if err != nil {
		return parameters, err
	}
	if describeCertificateOutput.Certificate != nil {
		io.WriteString(logsWriter, fmt.Sprintf("Current status of certificate: %s\n", describeCertificateOutput.Certificate.Status))
		if describeCertificateOutput.Certificate.Status == acm_types.CertificateStatusIssued {
			//certificate is already issued
			//sync verified status
			updateCertificatesPipeline.Add(updateCertificatesKey, certificates.UpdateCertificateDtoV1{
				ID:       certificateID,
				Verified: types.True,
			})
			io.WriteString(logsWriter, fmt.Sprintf("Certificate verified from ACM: %s\n", certificateArn))
			return parameters, err
		}
		if describeCertificateOutput.Certificate.Status == acm_types.CertificateStatusFailed || describeCertificateOutput.Certificate.Status == acm_types.CertificateStatusValidationTimedOut {
			//TODO handle - If a certificate shows status FAILED or VALIDATION_TIMED_OUT, delete the certificate
			//This can happen if the user doesn't validate certificate DNS in 72 hours
		}
	}

	io.WriteString(logsWriter, fmt.Sprintf("Waiting for certificate to be validated.....Please wait.\n"))
	newCertificateValidatedWaiter := acm.NewCertificateValidatedWaiter(acmClient)
	err = newCertificateValidatedWaiter.Wait(context.TODO(), &acm.DescribeCertificateInput{CertificateArn: aws.String(certificateArn)},
		20*time.Minute)
	if err != nil {
		return parameters, err
	}
	//sync verified status
	updateCertificatesPipeline.Add(updateCertificatesKey, certificates.UpdateCertificateDtoV1{
		ID:       certificateID,
		Verified: types.True,
	})

	//wait for a minute after the certificate is verified or AWS gives an error
	time.Sleep(1 * time.Minute)

	io.WriteString(logsWriter, fmt.Sprintf("Certificate verified from ACM: %s\n", certificateArn))

	return parameters, err
}
