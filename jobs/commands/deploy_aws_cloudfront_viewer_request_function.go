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

type DeployAwsCloudfrontViewerRequestFunction struct {
}

func getViewerRequestFunctionName(parameters map[string]interface{}) (string, error) {
	//viewer-request-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("viewer-request-%s", deploymentID), nil
}

func getViewerRequestFunctionComment(parameters map[string]interface{}) (string, error) {
	//cloudfront function for viewer-request-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("cloudfront function for viewer-request-%s", deploymentID), nil
}

func getViewerRequestFunctionCode(parameters map[string]interface{}) (string, error) {
	redirectDomainsA, _ := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.RedirectDomain)
	var redirectDomains []string
	domainRedirectStatement := ""
	var err error
	if len(redirectDomainsA) > 0 {
		redirectDomains, err = commandUtils.ConvertPrimitiveAToStringSlice(redirectDomainsA)
		if err != nil {
			return "", err
		}
		if len(redirectDomains) == 2 {
			from := redirectDomains[0]
			to := redirectDomains[1]
			proto := "https"
			domainRedirectStatement = fmt.Sprintf(`if ((request.headers["host"] && (request.headers["host"].value.startsWith("%s")))) {
        var response = {
            statusCode: 301,
            statusDescription: 'Moved Permanently',
            headers: {
                'location': { value: '%s://%s'+event.request.uri }
            }
        };
        return response;
    }`, from, proto, to)
		}
	}

	domainsA, _ := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.Domains)
	//if err != nil {
	//	return "", err
	//}
	var domains []string
	domainHttpsStatement := ""
	if len(domainsA) > 0 {
		domains, err = commandUtils.ConvertPrimitiveAToStringSlice(domainsA)
		if err != nil {
			return "", err
		}
		if len(domains) >= 1 {
			to := domains[0]
			proto := "https"
			domainHttpsStatement = fmt.Sprintf(`if ((request.headers["cloudfront-forwarded-proto"] && (request.headers["cloudfront-forwarded-proto"].value == "http"))) {
       var response = {
           statusCode: 301,
           statusDescription: 'Moved Permanently',
           headers: {
               'location': { value: '%s://%s'+event.request.uri }
           }
       };
       return response;
   }`, proto, to)
		}

	}

	subdirectoryIndexStatement := `if (request.uri.endsWith('/')) {
        request.uri += 'index.html';
     } else if (!request.uri.includes('.')) {
        request.uri += '/index.html';
     }`

	cloudfrontFunction := fmt.Sprintf(`function handler(event) {
    var request = event.request;
    %s
    %s
    %s
    return request;
}`, domainRedirectStatement, domainHttpsStatement, subdirectoryIndexStatement)
	return cloudfrontFunction, nil
}

func describeViewerRequestFunction(parameters map[string]interface{}, cloudfrontClient *cloudfront.Client) (describeFunctionOutput *cloudfront.DescribeFunctionOutput,
	exists bool, err error) {
	domainsFunctionName, err := getViewerRequestFunctionName(parameters)
	if err != nil {
		return nil, false, err
	}
	describeFunctionOutput, _ = cloudfrontClient.DescribeFunction(context.TODO(), &cloudfront.DescribeFunctionInput{
		Name: aws.String(domainsFunctionName),
		//Stage: "",
	})
	if describeFunctionOutput == nil {
		return nil, false, nil
	}
	return describeFunctionOutput, true, nil
}

func associateFunctionToCloudfrontDistribution(distributionConfig *cloudfront_types.DistributionConfig,
	functionARN *string, eventType cloudfront_types.EventType) bool {
	functionAssociations := distributionConfig.DefaultCacheBehavior.FunctionAssociations
	items := functionAssociations.Items
	quantity := functionAssociations.Quantity
	associate := true
	for _, item := range items {
		//check if already associated
		associatedArn := aws.ToString(item.FunctionARN)
		newArn := aws.ToString(functionARN)
		if (associatedArn == newArn) && (item.EventType == eventType) {
			associate = false
		}
	}
	if !associate {
		return false
	}

	var q int32
	if quantity == nil {
		q = 0
	} else {
		q = aws.ToInt32(quantity)
	}
	q++
	quantity = aws.Int32(q)
	items = append(items, cloudfront_types.FunctionAssociation{
		EventType:   eventType,
		FunctionARN: functionARN,
	})
	functionAssociations = &cloudfront_types.FunctionAssociations{
		Quantity: quantity,
		Items:    items,
	}
	distributionConfig.DefaultCacheBehavior.FunctionAssociations = functionAssociations

	return true
}

func (d *DeployAwsCloudfrontViewerRequestFunction) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {

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
	distributionConfig := distributionConfigOutput.DistributionConfig
	describeFunctionOutput, functionExists, err := describeViewerRequestFunction(parameters, cloudfrontClient)
	if err != nil {
		return parameters, err
	}

	//add function for redirection
	var functionARN, etag *string
	var cloudfrontFunctionComment string
	cloudfrontFunctionComment, err = getViewerRequestFunctionComment(parameters)
	if err != nil {
		return parameters, err
	}
	var viewerRequestsFunctionName string
	viewerRequestsFunctionName, err = getViewerRequestFunctionName(parameters)
	if err != nil {
		return parameters, err
	}
	var cloudfrontFunction string
	cloudfrontFunction, err = getViewerRequestFunctionCode(parameters)
	if err != nil {
		return parameters, err
	}

	config := &cloudfront_types.FunctionConfig{
		Comment: aws.String(cloudfrontFunctionComment),
		Runtime: cloudfront_types.FunctionRuntimeCloudfrontJs10,
	}
	if functionExists {
		io.WriteString(logsWriter, fmt.Sprintf("Updating cloudfront function %s for adding redirects\n", viewerRequestsFunctionName))
		//if function exists update existing function
		var updateFunctionOutput *cloudfront.UpdateFunctionOutput
		updateFunctionOutput, err = cloudfrontClient.UpdateFunction(context.TODO(), &cloudfront.UpdateFunctionInput{
			FunctionCode:   []byte(cloudfrontFunction),
			FunctionConfig: config,
			IfMatch:        describeFunctionOutput.ETag,
			Name:           aws.String(viewerRequestsFunctionName),
		})
		if err != nil {
			return parameters, err
		}
		functionARN = updateFunctionOutput.FunctionSummary.FunctionMetadata.FunctionARN
		etag = updateFunctionOutput.ETag
	} else {
		io.WriteString(logsWriter, fmt.Sprintf("Creating cloudfront function %s for adding redirects\n", viewerRequestsFunctionName))
		//create function
		var createFunctionOutput *cloudfront.CreateFunctionOutput
		createFunctionOutput, err = cloudfrontClient.CreateFunction(context.TODO(), &cloudfront.CreateFunctionInput{
			FunctionCode:   []byte(cloudfrontFunction),
			FunctionConfig: config,
			Name:           aws.String(viewerRequestsFunctionName),
		})
		if err != nil {
			return parameters, err
		}
		functionARN = createFunctionOutput.FunctionSummary.FunctionMetadata.FunctionARN
		etag = createFunctionOutput.ETag
	}
	io.WriteString(logsWriter, fmt.Sprintf("Publishing cloudfront function: %s\n", viewerRequestsFunctionName))
	//publish function
	_, err = cloudfrontClient.PublishFunction(context.TODO(), &cloudfront.PublishFunctionInput{
		IfMatch: etag,
		Name:    aws.String(viewerRequestsFunctionName),
	})
	if err != nil {
		return parameters, err
	}
	//associate function to distribution config
	associate := associateFunctionToCloudfrontDistribution(distributionConfig, functionARN, cloudfront_types.EventTypeViewerRequest)
	if associate {
		io.WriteString(logsWriter, fmt.Sprintf("Associating cloudfront function %s to cloudfront distribution: %s\n",
			viewerRequestsFunctionName, cloudfrontDistributionId))
		_, err = cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
			DistributionConfig: distributionConfig,
			Id:                 aws.String(cloudfrontDistributionId),
			IfMatch:            distributionConfigOutput.ETag,
		})

		if err != nil {
			return parameters, err
		}
	}

	err = invalidateCloudfrontDistribution(parameters, cloudfrontClient, cloudfrontDistributionId, logsWriter)
	if err != nil {
		return parameters, err
	}

	return parameters, err
}
