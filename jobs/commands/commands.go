package commands

import (
	"context"
	"fmt"
	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/client"
	"log"
	"time"
)

var updateBuildsPipeline *goPipeline.Pipeline[string, builds.UpdateBuildDtoV1]
var updateDeploymentsPipeline *goPipeline.Pipeline[string, deployments.UpdateDeploymentDtoV1]

func Init() {
	c := client.Get()
	updateBuildsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(build string, builds []builds.UpdateBuildDtoV1) {
			e := true
			for e {
				err := c.UpdateBuilds(builds)
				//TODO we can handle for ErrConnection
				//will block till error
				if err != nil {
					fmt.Println(err)
					time.Sleep(2 * time.Second)
					continue
				}
				e = false
			}
		})
	goShutdownHook.ADD(func() {
		fmt.Println("waiting for builds update pipeline shutdown")
		updateBuildsPipeline.Shutdown()
		fmt.Println("waiting for builds update pipeline shutdown -- done")
	})
	updateDeploymentsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(deployment string, deployments []deployments.UpdateDeploymentDtoV1) {
			e := true
			for e {
				err := c.UpdateDeployments(deployments)
				//TODO we can handle for ErrConnection
				//will block till error
				if err != nil {
					fmt.Println(err)
					time.Sleep(2 * time.Second)
					continue
				}
				e = false
			}
		})
	goShutdownHook.ADD(func() {
		fmt.Println("waiting for deployments update pipeline shutdown")
		updateDeploymentsPipeline.Shutdown()
		fmt.Println("waiting for deployments update pipeline shutdown -- done")
	})
}

func Get(p commands_enums.Type) (jobs.Command, error) {
	switch p {
	case commands_enums.CheckoutRepo:
		return &CheckoutRepository{}, nil
	case commands_enums.BuildStaticSite:
		return &BuildStaticSite{}, nil
	case commands_enums.DeployStaticSiteAWS:
		return &DeployStaticSiteAWS{}, nil
	}
	return nil, fmt.Errorf("error getting command for %s", p)
}

func markBuildDone(parameters map[parameters_enums.Key]interface{}, err error) {
	status := build_enums.Success
	errorMessage := ""
	if err != nil {
		status = build_enums.Error
		errorMessage = err.Error()
	}
	buildID, e := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
	if e != nil {
		//Weird error. Would show up in logs. Return for now.
		return
	}
	updateBuildsPipeline.Add("updateBuilds", builds.UpdateBuildDtoV1{
		ID:           buildID,
		BuildTs:      time.Now().Unix(),
		Status:       status,
		ErrorMessage: errorMessage,
	})
}

func listAllS3Objects(s3Client *s3.Client, bucketName string) ([]s3Types.Object, error) {
	params := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	}

	listObjectsPaginator := s3.NewListObjectsV2Paginator(s3Client, params)

	var i int
	log.Println("Objects:")
	var objects []s3Types.Object
	for listObjectsPaginator.HasMorePages() {
		i++
		page, err := listObjectsPaginator.NextPage(context.TODO())
		if err != nil {
			return nil, fmt.Errorf("failed to get page %v, %v", i, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

func deleteAllS3Files(s3Client *s3.Client, bucketName string) error {
	allS3Objects, err := listAllS3Objects(s3Client, bucketName)
	if err != nil {
		return err
	}
	var objectIds []s3Types.ObjectIdentifier
	for _, object := range allS3Objects {
		objectIds = append(objectIds, s3Types.ObjectIdentifier{Key: object.Key})
		if len(objectIds) == 9000 {
			//delete 9000 at a time. Limit is 10000
			_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
				Bucket: aws.String(bucketName),
				Delete: &s3Types.Delete{Objects: objectIds},
			})
			if err != nil {
				return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
			}
			objectIds = nil
		}
	}
	if len(objectIds) > 0 {
		_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &s3Types.Delete{Objects: objectIds},
		})
		if err != nil {
			return fmt.Errorf("error deleting objects from bucket %s : %s", bucketName, err)
		}
	}

	return nil
}
