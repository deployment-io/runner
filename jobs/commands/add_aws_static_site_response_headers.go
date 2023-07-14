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
	"strings"
	"time"
)

type AddAwsStaticSiteResponseHeaders struct {
}

func getResponseHeadersFunctionName(parameters map[string]interface{}) (string, error) {
	//response-headers-<deploymentID>
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

func (a *AddAwsStaticSiteResponseHeaders) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {

	// Create an Amazon Cloudfront service client
	cloudfrontClient, err := getCloudfrontClient(parameters, cloudfrontRegion)
	if err != nil {
		return parameters, err
	}

	responseHeadersFunctionName, err := getResponseHeadersFunctionName(parameters)
	if err != nil {
		return parameters, err
	}

	cloudfrontFunctionComment, err := getResponseHeadersFunctionComment(parameters)
	if err != nil {
		return parameters, err
	}

	cloudfrontFunction, err := getResponseHeadersFunctionCode(parameters)
	if err != nil {
		return parameters, err
	}

	//check if headers function exists
	describeFunctionOutput, err := cloudfrontClient.DescribeFunction(context.TODO(), &cloudfront.DescribeFunctionInput{
		Name: aws.String(responseHeadersFunctionName),
		//Stage: "",
	})

	config := &cloudfront_types.FunctionConfig{
		Comment: aws.String(cloudfrontFunctionComment),
		Runtime: cloudfront_types.FunctionRuntimeCloudfrontJs10,
	}

	var functionARN, etag *string

	if describeFunctionOutput != nil && err == nil {
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
	quantity := functionAssociations.Quantity
	associate := true
	for _, item := range items {
		//check if already associated
		associatedArn := aws.ToString(item.FunctionARN)
		newArn := aws.ToString(functionARN)
		if (associatedArn == newArn) && (item.EventType == cloudfront_types.EventTypeViewerResponse) {
			associate = false
		}
	}
	if associate {
		var q int32
		if quantity == nil {
			q = 0
		} else {
			q = aws.ToInt32(quantity)
		}
		q++
		quantity = aws.Int32(q)
		items = append(items, cloudfront_types.FunctionAssociation{
			EventType:   cloudfront_types.EventTypeViewerResponse,
			FunctionARN: functionARN,
		})
		functionAssociations = &cloudfront_types.FunctionAssociations{
			Quantity: quantity,
			Items:    items,
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
	}

	var callerReference string
	callerReference, err = getCallerReference(parameters)
	if err != nil {
		return parameters, err
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
		return parameters, err
	}

	//Wait for invalidation to get done
	invalidationWaiter := cloudfront.NewInvalidationCompletedWaiter(cloudfrontClient)

	err = invalidationWaiter.Wait(context.TODO(), &cloudfront.GetInvalidationInput{
		DistributionId: aws.String(cloudfrontDistributionId),
		Id:             createInvalidationOutput.Invalidation.Id,
	}, 10*time.Minute)

	if err != nil {
		return parameters, err
	}

	return parameters, err
}
