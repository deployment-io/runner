package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/deployment-io/deployment-runner-kit/certificates"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
	"io"
	"time"
)

type VerifyAcmCertificate struct {
}

func (v *VerifyAcmCertificate) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	acmClient, err := getAcmClient(parameters)
	if err != nil {
		return parameters, err
	}
	certificateArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.AcmCertificateArn)
	if err != nil {
		return parameters, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Verifying and validating certificate from ACM: %s\n", certificateArn))
	//TODO handle - If a certificate shows status FAILED or VALIDATION_TIMED_OUT, delete the request
	//This can happen if the user doesn't validate certificate DNS in 72 hours
	newCertificateValidatedWaiter := acm.NewCertificateValidatedWaiter(acmClient)
	err = newCertificateValidatedWaiter.Wait(context.TODO(), &acm.DescribeCertificateInput{CertificateArn: aws.String(certificateArn)},
		10*time.Minute)
	if err != nil {
		return parameters, err
	}
	certificateID, err := jobs.GetParameterValue[string](parameters, parameters_enums.CertificateID)
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
