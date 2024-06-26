package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfront_types "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
)

type UpdateAwsStaticSiteDomains struct {
}

func deleteDomainsFromCloudfront(cloudfrontClient *cloudfront.Client, distributionConfigOutput *cloudfront.GetDistributionConfigOutput,
	cloudfrontDistributionId string, logsWriter io.Writer) error {
	distributionConfig := distributionConfigOutput.DistributionConfig

	//distributionConfig.ViewerCertificate = &cloudfront_types.ViewerCertificate{
	//	ACMCertificateArn:            nil,
	//	CloudFrontDefaultCertificate: nil,
	//}

	distributionConfig.Aliases = &cloudfront_types.Aliases{
		Quantity: aws.Int32(0),
		Items:    []string{},
	}

	_, err := cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
		DistributionConfig: distributionConfig,
		Id:                 aws.String(cloudfrontDistributionId),
		IfMatch:            distributionConfigOutput.ETag,
	})
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleted alias from cloudfront distribution: %s\n", cloudfrontDistributionId))
	return nil
}

func (u *UpdateAwsStaticSiteDomains) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {

	cloudfrontClient, err := cloud_api_clients.GetCloudfrontClient(parameters, cloudfrontRegion)
	if err != nil {
		return parameters, err
	}

	cloudfrontDistributionId, err := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID)
	if err != nil {
		return parameters, err
	}

	distributionConfigOutput, err := cloudfrontClient.GetDistributionConfig(context.TODO(), &cloudfront.GetDistributionConfigInput{
		Id: aws.String(cloudfrontDistributionId),
	})

	if err != nil {
		return parameters, err
	}

	domainsA, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.Domains)
	if err != nil {
		return parameters, err
	}

	var domains []string
	if len(domainsA) == 0 {
		//delete domains from deployment
		io.WriteString(logsWriter, fmt.Sprintf("Deleting domains for static site\n"))
		err = deleteDomainsFromCloudfront(cloudfrontClient, distributionConfigOutput, cloudfrontDistributionId, logsWriter)
		if err != nil {
			return parameters, err
		}
		err = invalidateCloudfrontDistribution(parameters, cloudfrontClient, cloudfrontDistributionId, logsWriter)
		if err != nil {
			return parameters, err
		}
		return parameters, nil
	}

	io.WriteString(logsWriter, fmt.Sprintf("Updating domains for static site\n"))

	certificateArn, err := jobs.GetParameterValue[string](parameters, parameters_enums.AcmCertificateArn)
	if err != nil {
		return parameters, err
	}

	domains, err = commandUtils.ConvertPrimitiveAToStringSlice(domainsA)
	if err != nil {
		return parameters, err
	}

	distributionConfig := distributionConfigOutput.DistributionConfig

	distributionConfig.ViewerCertificate = &cloudfront_types.ViewerCertificate{
		ACMCertificateArn:            aws.String(certificateArn),
		CloudFrontDefaultCertificate: aws.Bool(false),
		MinimumProtocolVersion:       cloudfront_types.MinimumProtocolVersionTLSv122021,
		SSLSupportMethod:             cloudfront_types.SSLSupportMethodSniOnly,
	}

	distributionConfig.Aliases = &cloudfront_types.Aliases{
		Quantity: aws.Int32(int32(len(domains))),
		Items:    domains,
	}

	io.WriteString(logsWriter, fmt.Sprintf("Updating alias and certificate for cloudfront distribution: %s\n", cloudfrontDistributionId))

	_, err = cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
		DistributionConfig: distributionConfig,
		Id:                 aws.String(cloudfrontDistributionId),
		IfMatch:            distributionConfigOutput.ETag,
	})

	if err != nil {
		return parameters, err
	}

	err = invalidateCloudfrontDistribution(parameters, cloudfrontClient, cloudfrontDistributionId, logsWriter)
	if err != nil {
		return parameters, err
	}

	return parameters, err
}
