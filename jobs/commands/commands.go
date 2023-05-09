package commands

import (
	"fmt"
	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/client"
	"time"
)

var updateBuildsPipeline *goPipeline.Pipeline[string, builds.UpdateBuildDtoV1]

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
}

func Get(p commands_enums.Type) (jobs.Command, error) {
	switch p {
	case commands_enums.CheckoutRepo:
		return &CheckoutRepository{}, nil
	case commands_enums.BuildStaticSite:
		return &BuildStaticSite{}, nil
	}
	return nil, fmt.Errorf("error getting command for %s", p)
}
