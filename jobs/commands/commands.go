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
	"github.com/deployment-io/deployment-runner-kit/certificates"
	"github.com/deployment-io/deployment-runner-kit/clusters"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/notifications"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner-kit/vpcs"
	"github.com/deployment-io/deployment-runner/client"
	"os"
	"time"
)

const updateDeploymentsKey = "updateDeployments"
const updateBuildsKey = "updateBuilds"
const updatePreviewsKey = "updatePreviews"
const updateCertificatesKey = "updateCertificates"
const sendNotificationsKey = "sendNotifications"
const updateJobOutputsKey = "updateJobOutputs"

const cloudfrontRegion = "us-east-1"

var updateBuildsPipeline *goPipeline.Pipeline[string, builds.UpdateBuildDtoV1]
var updatePreviewsPipeline *goPipeline.Pipeline[string, previews.UpdatePreviewDtoV1]
var sendNotificationPipeline *goPipeline.Pipeline[string, notifications.SendNotificationDtoV1]
var updateDeploymentsPipeline *goPipeline.Pipeline[string, deployments.UpdateDeploymentDtoV1]
var upsertVpcsPipeline *goPipeline.Pipeline[string, vpcs.UpsertVpcDtoV1]
var upsertClustersPipeline *goPipeline.Pipeline[string, clusters.UpsertClusterDtoV1]
var updateCertificatesPipeline *goPipeline.Pipeline[string, certificates.UpdateCertificateDtoV1]
var updateJobOutputPipeline *goPipeline.Pipeline[string, jobs.UpdateJobOutputDtoV1]

func Shutdown() {
	updateBuildsPipeline.Shutdown()
	updatePreviewsPipeline.Shutdown()
	updateDeploymentsPipeline.Shutdown()
	upsertVpcsPipeline.Shutdown()
	upsertClustersPipeline.Shutdown()
	updateCertificatesPipeline.Shutdown()
	sendNotificationPipeline.Shutdown()
	updateJobOutputPipeline.Shutdown()
}

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
		//fmt.Println("waiting for builds update pipeline shutdown")
		//updateBuildsPipeline.Shutdown()
		//fmt.Println("waiting for builds update pipeline shutdown -- done")
	})
	updatePreviewsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(preview string, previews []previews.UpdatePreviewDtoV1) {
			e := true
			for e {
				err := c.UpdatePreviews(previews)
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
		//fmt.Println("waiting for previews update pipeline shutdown")
		//updatePreviewsPipeline.Shutdown()
		//fmt.Println("waiting for previews update pipeline shutdown -- done")
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
		//fmt.Println("waiting for deployments update pipeline shutdown")
		//updateDeploymentsPipeline.Shutdown()
		//fmt.Println("waiting for deployments update pipeline shutdown -- done")
	})
	upsertVpcsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second, func(vpc string, vpcs []vpcs.UpsertVpcDtoV1) {
		e := true
		for e {
			err := c.UpsertVpcs(vpcs)
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
		//fmt.Println("waiting for vpcs upsert pipeline shutdown")
		//upsertVpcsPipeline.Shutdown()
		//fmt.Println("waiting for vpcs upsert pipeline shutdown -- done")
	})
	upsertClustersPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second, func(cluster string, clusters []clusters.UpsertClusterDtoV1) {
		e := true
		for e {
			err := c.UpsertClusters(clusters)
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
		//fmt.Println("waiting for clusters upsert pipeline shutdown")
		//upsertClustersPipeline.Shutdown()
		//fmt.Println("waiting for clusters upsert pipeline shutdown -- done")
	})
	updateCertificatesPipeline, _ = goPipeline.NewPipeline(5, 2*time.Second,
		func(certificate string, certificates []certificates.UpdateCertificateDtoV1) {
			e := true
			for e {
				err := c.UpdateCertificates(certificates)
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
		//fmt.Println("waiting for certificates update pipeline shutdown")
		//updateCertificatesPipeline.Shutdown()
		//fmt.Println("waiting for certificates update pipeline shutdown -- done")
	})
	sendNotificationPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(notification string, notifications []notifications.SendNotificationDtoV1) {
			e := true
			for e {
				err := c.SendNotifications(notifications)
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
		//fmt.Println("waiting for notifications send pipeline shutdown")
		//sendNotificationPipeline.Shutdown()
		//fmt.Println("waiting for notifications send pipeline shutdown -- done")
	})
	updateJobOutputPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(job string, jobOutputs []jobs.UpdateJobOutputDtoV1) {
			e := true
			for e {
				err := c.UpdateJobOutputs(jobOutputs)
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
}

func Get(p commands_enums.Type) (jobs.Command, error) {
	switch p {
	case commands_enums.CheckoutRepo:
		return &CheckoutRepository{}, nil
	case commands_enums.BuildStaticSite:
		return &BuildStaticSite{}, nil
	case commands_enums.DeployAwsStaticSite:
		return &DeployAwsStaticSite{}, nil
	case commands_enums.BuildDockerImage:
		return &BuildDockerImage{}, nil
	case commands_enums.DeployAwsWebService:
		return &DeployAwsWebService{}, nil
	case commands_enums.CreateAwsVpc:
		return &CreateDefaultAwsVPC{}, nil
	case commands_enums.CreateEcsCluster:
		return &CreateEcsCluster{}, nil
	case commands_enums.UploadImageToEcr:
		return &UploadDockerImageToEcr{}, nil
	case commands_enums.AddAwsStaticSiteResponseHeaders:
		return &AddAwsStaticSiteResponseHeaders{}, nil
	case commands_enums.UpdateAwsStaticSiteDomains:
		return &UpdateAwsStaticSiteDomains{}, nil
	case commands_enums.DeployAwsCloudfrontViewerRequestFunction:
		return &DeployAwsCloudfrontViewerRequestFunction{}, nil
	case commands_enums.CreateAcmCertificate:
		return &CreateAcmCertificate{}, nil
	case commands_enums.UpdateAwsWebServiceDomain:
		return &AddAwsWebServiceDomain{}, nil
	case commands_enums.VerifyAcmCertificate:
		return &VerifyAcmCertificate{}, nil
	case commands_enums.DeleteAwsStaticSite:
		return &DeleteAwsStaticSite{}, nil
	case commands_enums.DeleteAwsWebService:
		return &DeleteAwsWebService{}, nil
	case commands_enums.ListCloudWatchMetricsAwsEcsWebService:
		return &ListCloudWatchMetricsAwsEcsWebService{}, nil
	}
	return nil, fmt.Errorf("error getting command for %s", p)
}

func isPreview(parameters map[string]interface{}) bool {
	p, err := jobs.GetParameterValue[bool](parameters, parameters_enums.IsPreview)
	if err != nil {
		p = false
	}
	return p
}

func MarkBuildDone(parameters map[string]interface{}, err error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer func() {
			//Delete old repo directory to clean up
			repoDirectoryPath, _ := getRepositoryDirectoryPath(parameters)
			if len(repoDirectoryPath) > 0 {
				os.RemoveAll(repoDirectoryPath)
			}
			close(done)
		}()
		status := build_enums.Success
		errorMessage := ""
		if err != nil {
			status = build_enums.Error
			errorMessage = err.Error()
		}
		nowEpoch := time.Now().Unix()
		if isPreview(parameters) {
			//update preview and return
			previewID, e := jobs.GetParameterValue[string](parameters, parameters_enums.PreviewID)
			if e != nil {
				//Weird error. Would show up in logs. Return for now.
				return
			}
			updatePreviewsPipeline.Add(updatePreviewsKey, previews.UpdatePreviewDtoV1{
				ID:           previewID,
				BuildTs:      nowEpoch,
				Status:       status,
				ErrorMessage: errorMessage,
			})
			return
		}

		buildID, e := jobs.GetParameterValue[string](parameters, parameters_enums.BuildID)
		if e != nil {
			//Weird error. Would show up in logs. Return for now.
			return
		}
		updateBuildsPipeline.Add(updateBuildsKey, builds.UpdateBuildDtoV1{
			ID:           buildID,
			BuildTs:      nowEpoch,
			Status:       status,
			ErrorMessage: errorMessage,
		})
		deploymentID, e := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
		if e != nil {
			//Weird error. Would show up in logs. Return for now.
			return
		}
		updateDeploymentsPipeline.Add(updateDeploymentsKey, deployments.UpdateDeploymentDtoV1{
			ID:               deploymentID,
			LastDeploymentTs: nowEpoch,
			Status:           status,
			ErrorMessage:     errorMessage,
		})
	}()
	return done
}

func listAllS3Objects(s3Client *s3.Client, bucketName string) ([]s3Types.Object, error) {
	params := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	}

	listObjectsPaginator := s3.NewListObjectsV2Paginator(s3Client, params)

	var i int
	//log.Println("Objects:")
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

func getDockerImageNameAndTag(parameters map[string]interface{}) (string, error) {
	//ex. <organizationID>-<deploymentID>:<commit-hash>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	deploymentID, err := jobs.GetParameterValue[string](parameters, parameters_enums.DeploymentID)
	if err != nil {
		return "", err
	}
	commitHashFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.CommitHash)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s:%s", organizationID, deploymentID, commitHashFromParams), nil
}
