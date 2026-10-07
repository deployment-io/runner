package commands

import (
	"errors"
	"sync"
	"testing"
	"time"

	goPipeline "github.com/ankit-arora/go-utils/go-concurrent-pipeline/go-pipeline"
	"github.com/deployment-io/deployment-runner-kit/builds"
	"github.com/deployment-io/deployment-runner-kit/deployments"
	"github.com/deployment-io/deployment-runner-kit/enums/build_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// markDeploymentDoneUpdates runs MarkDeploymentDone against recording
// pipelines and returns the deployment and build updates it queued.
func markDeploymentDoneUpdates(t *testing.T, parameters map[string]interface{}, err error) (
	[]deployments.UpdateDeploymentDtoV1, []builds.UpdateBuildDtoV1) {
	t.Helper()
	var mu sync.Mutex
	var deploymentUpdates []deployments.UpdateDeploymentDtoV1
	var buildUpdates []builds.UpdateBuildDtoV1
	originalDeployments, originalBuilds := commandUtils.UpdateDeploymentsPipeline, commandUtils.UpdateBuildsPipeline
	t.Cleanup(func() {
		commandUtils.UpdateDeploymentsPipeline, commandUtils.UpdateBuildsPipeline = originalDeployments, originalBuilds
	})
	commandUtils.UpdateDeploymentsPipeline, _ = goPipeline.NewPipeline(5, time.Hour,
		func(_ string, updates []deployments.UpdateDeploymentDtoV1) {
			mu.Lock()
			defer mu.Unlock()
			deploymentUpdates = append(deploymentUpdates, updates...)
		})
	commandUtils.UpdateBuildsPipeline, _ = goPipeline.NewPipeline(5, time.Hour,
		func(_ string, updates []builds.UpdateBuildDtoV1) {
			mu.Lock()
			defer mu.Unlock()
			buildUpdates = append(buildUpdates, updates...)
		})

	<-MarkDeploymentDone(parameters, err)
	// Shutdown flushes whatever was queued.
	commandUtils.UpdateDeploymentsPipeline.Shutdown()
	commandUtils.UpdateBuildsPipeline.Shutdown()
	return deploymentUpdates, buildUpdates
}

func deployJobParameters(restartOnly bool) map[string]interface{} {
	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.OrganizationIdFromJob, "org-1")
	jobs.SetParameterValue(parameters, parameters_enums.DeploymentID, "deployment-1")
	jobs.SetParameterValue(parameters, parameters_enums.BuildID, "build-1")
	if restartOnly {
		jobs.SetParameterValue(parameters, parameters_enums.RestartOnly, true)
	}
	return parameters
}

func TestMarkDeploymentDoneRestartOnlyQueuesNothing(t *testing.T) {
	for name, err := range map[string]error{"success": nil, "failure": errors.New("deploy failed")} {
		deploymentUpdates, buildUpdates := markDeploymentDoneUpdates(t, deployJobParameters(true), err)
		if len(deploymentUpdates) != 0 || len(buildUpdates) != 0 {
			t.Errorf("%s: restart queued deployment updates %v and build updates %v; want none",
				name, deploymentUpdates, buildUpdates)
		}
	}
}

func TestMarkDeploymentDoneWithoutRestartOnly(t *testing.T) {
	for name, tt := range map[string]struct {
		err          error
		status       build_enums.Status
		errorMessage string
	}{
		"success": {nil, build_enums.Success, ""},
		"failure": {errors.New("deploy failed"), build_enums.Error, "deploy failed"},
	} {
		deploymentUpdates, buildUpdates := markDeploymentDoneUpdates(t, deployJobParameters(false), tt.err)
		if len(deploymentUpdates) != 1 || len(buildUpdates) != 1 {
			t.Fatalf("%s: got %d deployment and %d build updates; want 1 each", name, len(deploymentUpdates), len(buildUpdates))
		}
		d, b := deploymentUpdates[0], buildUpdates[0]
		if d.ID != "deployment-1" || d.Status != tt.status || d.ErrorMessage != tt.errorMessage || d.LastDeploymentTs == 0 {
			t.Errorf("%s: deployment update = %+v", name, d)
		}
		if b.ID != "build-1" || b.Status != tt.status || b.ErrorMessage != tt.errorMessage || b.BuildTs == 0 {
			t.Errorf("%s: build update = %+v", name, b)
		}
	}
}
