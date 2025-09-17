package utils

import (
	"fmt"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	agentTypes "github.com/deployment-io/deployment-runner-kit/agents"
	"github.com/deployment-io/deployment-runner-kit/automations"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/certificates"
	"github.com/deployment-io/deployment-runner-kit/clusters"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/notifications"
	"github.com/deployment-io/deployment-runner-kit/previews"
	"github.com/deployment-io/deployment-runner-kit/vpcs"
	"github.com/deployment-io/deployment-runner/client"
)

var UpdateBuildsPipeline *goPipeline.Pipeline[string, builds.UpdateBuildDtoV1]
var UpdatePreviewsPipeline *goPipeline.Pipeline[string, previews.UpdatePreviewDtoV1]
var SendNotificationPipeline *goPipeline.Pipeline[string, notifications.SendNotificationDtoV1]
var UpdateDeploymentsPipeline *goPipeline.Pipeline[string, deployments.UpdateDeploymentDtoV1]
var UpsertVpcsPipeline *goPipeline.Pipeline[string, vpcs.UpsertVpcDtoV1]
var UpsertClustersPipeline *goPipeline.Pipeline[string, clusters.UpsertClusterDtoV1]
var UpdateCertificatesPipeline *goPipeline.Pipeline[string, certificates.UpdateCertificateDtoV1]
var UpdateJobOutputPipeline *goPipeline.Pipeline[string, jobs.UpdateJobOutputDtoV1]
var UpdateAutomationOutputPipeline *goPipeline.Pipeline[string, automations.UpdateResponseDtoV1]
var UpdateAgentOutputPipeline *goPipeline.Pipeline[string, agentTypes.UpdateResponseDtoV1]

func Shutdown() {
	UpdateBuildsPipeline.Shutdown()
	UpdatePreviewsPipeline.Shutdown()
	UpdateDeploymentsPipeline.Shutdown()
	UpsertVpcsPipeline.Shutdown()
	UpsertClustersPipeline.Shutdown()
	UpdateCertificatesPipeline.Shutdown()
	SendNotificationPipeline.Shutdown()
	UpdateJobOutputPipeline.Shutdown()
	UpdateAutomationOutputPipeline.Shutdown()
	UpdateAgentOutputPipeline.Shutdown()
}

func Init() {
	c := client.Get()
	UpdateBuildsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(organizationId string, builds []builds.UpdateBuildDtoV1) {
			e := true
			for e {
				err := c.UpdateBuilds(builds, organizationId)
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
	UpdatePreviewsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(organizationId string, previews []previews.UpdatePreviewDtoV1) {
			e := true
			for e {
				err := c.UpdatePreviews(previews, organizationId)
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
	UpdateDeploymentsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(organizationId string, deployments []deployments.UpdateDeploymentDtoV1) {
			e := true
			for e {
				err := c.UpdateDeployments(deployments, organizationId)
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
	UpsertVpcsPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second, func(organizationId string, vpcs []vpcs.UpsertVpcDtoV1) {
		e := true
		for e {
			err := c.UpsertVpcs(vpcs, organizationId)
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
	UpsertClustersPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second, func(organizationId string, clusters []clusters.UpsertClusterDtoV1) {
		e := true
		for e {
			err := c.UpsertClusters(clusters, organizationId)
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
	UpdateCertificatesPipeline, _ = goPipeline.NewPipeline(5, 2*time.Second,
		func(organizationId string, certificates []certificates.UpdateCertificateDtoV1) {
			e := true
			for e {
				err := c.UpdateCertificates(certificates, organizationId)
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
	SendNotificationPipeline, _ = goPipeline.NewPipeline(5, 10*time.Second,
		func(organizationId string, notifications []notifications.SendNotificationDtoV1) {
			e := true
			for e {
				err := c.SendNotifications(notifications, organizationId)
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
	UpdateJobOutputPipeline, _ = goPipeline.NewPipeline(5, 1*time.Second,
		func(organizationId string, jobOutputs []jobs.UpdateJobOutputDtoV1) {
			e := true
			for e {
				err := c.UpdateJobOutputs(jobOutputs, organizationId)
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
	UpdateAutomationOutputPipeline, _ = goPipeline.NewPipeline(5, 1*time.Second,
		func(organizationId string, automationResponses []automations.UpdateResponseDtoV1) {
			e := true
			for e {
				err := c.UpdateAutomationResponses(automationResponses, organizationId)
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

	UpdateAgentOutputPipeline, _ = goPipeline.NewPipeline(5, 1*time.Second,
		func(organizationId string, agentResponses []agentTypes.UpdateResponseDtoV1) {
			e := true
			for e {
				err := c.UpdateAgentResponses(agentResponses, organizationId)
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
