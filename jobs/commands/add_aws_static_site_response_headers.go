package commands

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cloudfront_types "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"strings"
	"time"
)

type AddAwsStaticSiteResponseHeaders struct {
}

func getViewerResponseFunctionName(parameters map[string]interface{}) (string, error) {
	//response-headers-<deploymentID>
	//TODO change name to viewer-response-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("response-headers-%s", deploymentID), nil
}

func getResponseHeadersFunctionComment(parameters map[string]interface{}) (string, error) {
	//cloudfront function for response-headers-<deploymentID>
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("cloudfront function for response-headers-%s", deploymentID), nil
}

func getResponseHeadersFunctionCode(parameters map[string]interface{}) (string, error) {
	responseHeadersA, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.ResponseHeaders)
	if err != nil {
		return "", err
	}
	responseHeaders, err := commandUtils.ConvertPrimitiveAToTwoDStringSlice(responseHeadersA)
	if err != nil {
		return "", err
	}
	headerStatements := ""
	for _, responseHeader := range responseHeaders {
		if len(responseHeader) != 3 {
			return "", fmt.Errorf("invalid response header data")
		}
		path := responseHeader[0]
		name := strings.ToLower(responseHeader[1])
		value := responseHeader[2]
		headerStatement := fmt.Sprintf(`regex = new RegExp('%s')
    if (regex.test(uri)) {
            responseHeaders['%s'] = { value: '%s' }
    }`, path, name, value)
		headerStatements = headerStatements + "\n" + headerStatement
	}
	cloudfrontFunction := fmt.Sprintf(`function handler(event) {
    var response = event.response;
    var request = event.request;
    var uri = request.uri
    var responseHeaders = response.headers
    var regex
    %s
    response.headers = responseHeaders
    return response;
}`, headerStatements)
	return cloudfrontFunction, err
}

func (a *AddAwsStaticSiteResponseHeaders) Run(parameters map[string]interface{}, logsWriter io.Writer) (newParameters map[string]interface{}, err error) {

	// Create an Amazon Cloudfront service client
	cloudfrontClient, err := getCloudfrontClient(parameters, cloudfrontRegion)
	if err != nil {
		return parameters, err
	}

	responseHeadersFunctionName, err := getViewerResponseFunctionName(parameters)
	if err != nil {
		return parameters, err
	}

	cloudfrontDistributionId, err := jobs.GetParameterValue[string](parameters, parameters_enums.CloudfrontID)
	if err != nil {
		return parameters, err
	}

	//assign to the cloudfront distribution
	distributionConfigOutput, err := cloudfrontClient.GetDistributionConfig(context.TODO(), &cloudfront.GetDistributionConfigInput{
		Id: aws.String(cloudfrontDistributionId),
	})

	if err != nil {
		return parameters, err
	}

	distributionConfig := distributionConfigOutput.DistributionConfig
	functionAssociations := distributionConfig.DefaultCacheBehavior.FunctionAssociations
	items := functionAssociations.Items

	responseHeadersA, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.ResponseHeaders)
	if err != nil {
		return parameters, err
	}

	//check if headers function exists
	describeFunctionOutput, _ := cloudfrontClient.DescribeFunction(context.TODO(), &cloudfront.DescribeFunctionInput{
		Name: aws.String(responseHeadersFunctionName),
		//Stage: "",
	})

	if len(responseHeadersA) == 0 {
		if describeFunctionOutput == nil {
			return parameters, nil
		}
		//no headers so delete function and disassociate
		newArn := aws.ToString(describeFunctionOutput.FunctionSummary.FunctionMetadata.FunctionARN)
		var newItems []cloudfront_types.FunctionAssociation
		var q int32 = 0
		disAssociate := false
		for _, item := range items {
			//check if already associated
			associatedArn := aws.ToString(item.FunctionARN)
			if (associatedArn == newArn) && (item.EventType == cloudfront_types.EventTypeViewerResponse) {
				disAssociate = true
			} else {
				newItems = append(newItems, item)
				q++
			}
		}
		if disAssociate {
			functionAssociations = &cloudfront_types.FunctionAssociations{
				Quantity: aws.Int32(q),
				Items:    newItems,
			}
			distributionConfig.DefaultCacheBehavior.FunctionAssociations = functionAssociations
			_, err = cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
				DistributionConfig: distributionConfig,
				Id:                 aws.String(cloudfrontDistributionId),
				IfMatch:            distributionConfigOutput.ETag,
			})

			if err != nil {
				return parameters, err
			}

			err := invalidateCloudfrontDistribution(parameters, cloudfrontClient, cloudfrontDistributionId)
			if err != nil {
				return parameters, err
			}
		}

		_, err = cloudfrontClient.DeleteFunction(context.TODO(), &cloudfront.DeleteFunctionInput{
			IfMatch: describeFunctionOutput.ETag,
			Name:    aws.String(responseHeadersFunctionName),
		})
		if err != nil {
			return parameters, err
		}

		return parameters, nil
	}

	cloudfrontFunctionComment, err := getResponseHeadersFunctionComment(parameters)
	if err != nil {
		return parameters, err
	}

	cloudfrontFunction, err := getResponseHeadersFunctionCode(parameters)
	if err != nil {
		return parameters, err
	}

	config := &cloudfront_types.FunctionConfig{
		Comment: aws.String(cloudfrontFunctionComment),
		Runtime: cloudfront_types.FunctionRuntimeCloudfrontJs10,
	}

	var functionARN, etag *string

	if describeFunctionOutput != nil {
		//function exists
		//TODO logic can be better and we can specifically check for NoSuchFunctionExists: The specified function does not exist
		//if exists
		//update to add the new headers
		var updateFunctionOutput *cloudfront.UpdateFunctionOutput
		updateFunctionOutput, err = cloudfrontClient.UpdateFunction(context.TODO(), &cloudfront.UpdateFunctionInput{
			FunctionCode:   []byte(cloudfrontFunction),
			FunctionConfig: config,
			IfMatch:        describeFunctionOutput.ETag,
			Name:           aws.String(responseHeadersFunctionName),
		})
		if err != nil {
			return parameters, err
		}
		functionARN = updateFunctionOutput.FunctionSummary.FunctionMetadata.FunctionARN
		etag = updateFunctionOutput.ETag
	} else {
		//else create a new function
		var createFunctionOutput *cloudfront.CreateFunctionOutput
		createFunctionOutput, err = cloudfrontClient.CreateFunction(context.TODO(), &cloudfront.CreateFunctionInput{
			FunctionCode:   []byte(cloudfrontFunction),
			FunctionConfig: config,
			Name:           aws.String(responseHeadersFunctionName),
		})
		if err != nil {
			return parameters, err
		}
		functionARN = createFunctionOutput.FunctionSummary.FunctionMetadata.FunctionARN
		etag = createFunctionOutput.ETag
	}

	//publish function
	_, err = cloudfrontClient.PublishFunction(context.TODO(), &cloudfront.PublishFunctionInput{
		IfMatch: etag,
		Name:    aws.String(responseHeadersFunctionName),
	})

	if err != nil {
		return parameters, err
	}

	associate := associateFunctionToCloudfrontDistribution(distributionConfig, functionARN, cloudfront_types.EventTypeViewerResponse)
	if associate {
		_, err = cloudfrontClient.UpdateDistribution(context.TODO(), &cloudfront.UpdateDistributionInput{
			DistributionConfig: distributionConfig,
			Id:                 aws.String(cloudfrontDistributionId),
			IfMatch:            distributionConfigOutput.ETag,
		})

		if err != nil {
			return parameters, err
		}
	}

	err = invalidateCloudfrontDistribution(parameters, cloudfrontClient, cloudfrontDistributionId)
	if err != nil {
		return parameters, err
	}

	return parameters, err
}

func invalidateCloudfrontDistribution(parameters map[string]interface{}, cloudfrontClient *cloudfront.Client, cloudfrontDistributionId string) error {
	callerReference, err := getCallerReference(parameters)
	if err != nil {
		return err
	}

	var createInvalidationOutput *cloudfront.CreateInvalidationOutput
	createInvalidationOutput, err = cloudfrontClient.CreateInvalidation(context.TODO(), &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(cloudfrontDistributionId),
		InvalidationBatch: &cloudfront_types.InvalidationBatch{
			CallerReference: aws.String(callerReference),
			Paths: &cloudfront_types.Paths{
				Quantity: aws.Int32(1),
				Items: []string{
					"/*",
				},
			},
		},
	})

	if err != nil {
		return err
	}

	//Wait for invalidation to get done
	invalidationWaiter := cloudfront.NewInvalidationCompletedWaiter(cloudfrontClient)

	err = invalidationWaiter.Wait(context.TODO(), &cloudfront.GetInvalidationInput{
		DistributionId: aws.String(cloudfrontDistributionId),
		Id:             createInvalidationOutput.Invalidation.Id,
	}, 10*time.Minute)

	if err != nil {
		return err
	}
	return nil
}
