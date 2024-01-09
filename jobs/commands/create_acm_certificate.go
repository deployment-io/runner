package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	acm_types "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/deployment-io/deployment-runner-kit/certificates"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"io"
	"time"
)

type CreateAcmCertificate struct {
}

func (c *CreateAcmCertificate) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {
	acmClient, err := getAcmClient(parameters)
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
		KeyAlgorithm:     acm_types.KeyAlgorithmRsa2048,
		Options:          &acm_types.CertificateOptions{CertificateTransparencyLoggingPreference: acm_types.CertificateTransparencyLoggingPreferenceEnabled},
		ValidationMethod: acm_types.ValidationMethodDns,
		SubjectAlternativeNames: []string{
			parentDomain,
		},
	})
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
					////save certificate cname info
					//
					//fmt.Println(aws.ToString(describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Name))
					//fmt.Println(aws.ToString(describeCertificateOutput.Certificate.DomainValidationOptions[0].ResourceRecord.Value))

					updateCertificatesPipeline.Add(updateCertificatesKey, certificates.UpdateCertificateDtoV1{
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
	updateCertificatesPipeline.Add(updateCertificatesKey, certificates.UpdateCertificateDtoV1{
		ID:             certificateID,
		CertificateArn: aws.ToString(certificateArn),
	})
	jobs.SetParameterValue[string](parameters, parameters_enums.AcmCertificateArn, aws.ToString(certificateArn))
	return parameters, err
}
