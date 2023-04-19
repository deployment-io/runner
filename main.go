package main

import (
	"github.com/deployment-io/deployment-runner/client"
	"github.com/deployment-io/deployment-runner/jobs/commands"
	"github.com/deployment-io/deployment-runner/utils"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/deployment-io/jobs-runner-kit/jobs"
	"log"
	"time"
)

var service = "localhost:1234"

//var service = "nlb-deployment-load-balancer-8240e82289b3f92e.elb.eu-west-1.amazonaws.com:443"

func main() {
	client.Connect(service, false)
	c := client.Get()
	for true {
		pendingJobs, err := c.GetPendingJobs()
		if err != nil {
			time.Sleep(1 * time.Minute)
			continue
		}

		for _, pendingJob := range pendingJobs {
			//each job executed concurrently
			go func(jobDtoV1 jobs.JobDtoV1) {
				parameters := jobDtoV1.Parameters
				logger, err := loggers.Get(parameters)
				jobContext := utils.GetJobContext(parameters)
				if err != nil {
					//TODO send message back
				}
				for _, commandEnum := range jobDtoV1.CommandEnums {
					command, err := commands.Get(commandEnum)
					if err != nil {
						//TODO send message back
						continue
					}
					parameters, err = command.Run(parameters, logger, jobContext)
					if err != nil {
						//TODO send message back
						log.Println(err)
						utils.LogError(jobContext)
						continue
					}
				}
			}(pendingJob)
		}

		//TODO change back to 15 seconds when server code is ready
		//time.Sleep(15 * time.Second)
		time.Sleep(15 * time.Minute)
	}
}
