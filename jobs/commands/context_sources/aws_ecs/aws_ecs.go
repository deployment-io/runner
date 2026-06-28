// Package aws_ecs is the AWS ECS context connector — the OBSERVED half of the service↔repo map.
// It walks live ECS (clusters → services → task definitions → container images) and best-effort
// recovers the repo that produced each image, emitting one Cluster-scoped Result per cluster
// (scope ID = the cluster ARN). It is the live counterpart to the DECLARED service-repo-map that
// app-server derives from each repo's deploy-config: same shape, different source of truth, so the
// agent can reconcile "what's wired up" against "what's actually running".
//
// Source name "aws-ecs", SourceKind Discovered. Self-registers via init(); BuildInfraContext
// blank-imports this package. Metadata/structure only — never secret values.
package aws_ecs

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner-kit/enums/context_pack_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/jobs/commands/context_sources"
	"github.com/deployment-io/deployment-runner/utils"
)

func init() {
	context_sources.Register(&source{})
}

type source struct{}

func (s *source) Name() string { return context_pack.SourceAwsEcs }

// observedService is one running ECS service mapped to the repo we recovered for it. The shape
// mirrors the declared service-repo-map (service/repo/image) plus the observed coordinates and the
// recovery method, so reconciliation can line the two up and weight the join. JSON tags are the
// on-disk artifact schema; `cloud` keeps the record self-describing in a cross-cloud view.
type observedService struct {
	Service     string `json:"service"`
	Cluster     string `json:"cluster"`               // human-facing cluster name (ARN is the scope ID)
	Image       string `json:"image"`                 // full container image URI
	Repo        string `json:"repo,omitempty"`        // recovered repo (best-effort; see recoverRepo)
	RecoveredBy string `json:"recoveredBy,omitempty"` // how Repo was derived, e.g. "image-name"
	Cloud       string `json:"cloud"`                 // "aws"
}

// Build self-provisions ECS read access, then emits one Cluster-scoped Result per ECS cluster in
// the runner's region that has services. A never-deployed (context-only) runner has no ecs:* yet, so
// the policy self-grant is what makes this work without a prior deployment; on an already-deployed
// runner it is a no-op. Region comes from the job parameters (set by the trigger to the runner's
// region); the cluster ARN carries account+region, so records stay unambiguous across runners.
func (s *source) Build(parameters map[string]interface{}, logsWriter io.Writer) ([]context_sources.Result, error) {
	runnerData := utils.RunnerData.Get()
	if err := ensurePolicy(parameters, runnerData); err != nil {
		return nil, err
	}

	// Scan the runner's OWN install region. A runner is installed per region and its default
	// credential chain is its own account, so the clusters it can see are exactly its region+account's
	// — precisely the slice of infra this runner is responsible for. Self-sourced from RunnerData, NOT
	// a job parameter: the trigger needn't pass a region and can't pass a wrong one. An org spanning
	// regions/accounts installs a runner per region/account, and each builds its own slice; the packs
	// compose by scope (cluster ARN carries account+region).
	if runnerData.RunnerRegion == "" {
		io.WriteString(logsWriter, "aws-ecs: runner has no region; skipping\n")
		return nil, nil
	}
	ecsClient, err := cloud_api_clients.GetEcsClientFromRegion(runnerData.RunnerRegion)
	if err != nil {
		return nil, err
	}
	io.WriteString(logsWriter, fmt.Sprintf("aws-ecs: scanning region %s (account %s)\n", runnerData.RunnerRegion, runnerData.AWSAccountID))

	ctx := context.TODO()
	clusterArns, err := listClusters(ctx, ecsClient)
	if err != nil {
		return nil, err
	}
	if len(clusterArns) == 0 {
		io.WriteString(logsWriter, "aws-ecs: no ECS clusters in this region; nothing to observe\n")
		return nil, nil
	}

	builtTs := time.Now().Unix()
	var results []context_sources.Result
	for _, clusterArn := range clusterArns {
		observed, err := observeCluster(ctx, ecsClient, clusterArn, logsWriter)
		if err != nil {
			// One cluster failing shouldn't sink the others — log + skip. The scope's last-good
			// record stands (per BuildInfraContext's failure posture: error != gap, no degraded write).
			io.WriteString(logsWriter, fmt.Sprintf("aws-ecs: cluster %s failed (skipping): %v\n", nameFromArn(clusterArn), err))
			continue
		}
		if len(observed) == 0 {
			continue // empty cluster: nothing to record (no degraded/empty pack)
		}
		results = append(results, scopedResult(clusterArn, observed, builtTs))
	}
	io.WriteString(logsWriter, fmt.Sprintf("aws-ecs: observed %d cluster(s) with services\n", len(results)))
	return results, nil
}

// ensurePolicy self-grants the infra-context read bundle (ecs:*) on the runner's own task role,
// mirroring every other command's policy self-grant. Idempotent; a no-op on a runner that already
// has ecs:* from a prior deployment.
func ensurePolicy(parameters map[string]interface{}, runnerData utils.RunnerDataType) error {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return err
	}
	return iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsInfraContext, runnerData.OsType.String(),
		runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
}

// listClusters returns every ECS cluster ARN in the region, following pagination.
func listClusters(ctx context.Context, c *ecs.Client) ([]string, error) {
	var arns []string
	var next *string
	for {
		out, err := c.ListClusters(ctx, &ecs.ListClustersInput{NextToken: next})
		if err != nil {
			return nil, err
		}
		arns = append(arns, out.ClusterArns...)
		if out.NextToken == nil {
			return arns, nil
		}
		next = out.NextToken
	}
}

// observeCluster walks one cluster's services → task definitions → container images. A failure to
// list the cluster's services is returned (caller skips the cluster); a single task definition that
// can't be read is logged and skipped so one bad service doesn't blank the whole cluster.
func observeCluster(ctx context.Context, c *ecs.Client, clusterArn string, logsWriter io.Writer) ([]observedService, error) {
	serviceArns, err := listServices(ctx, c, clusterArn)
	if err != nil {
		return nil, err
	}
	if len(serviceArns) == 0 {
		return nil, nil
	}
	clusterName := nameFromArn(clusterArn)
	taskDefImages := map[string][]string{} // task-def ARN -> image URIs; services often share a task def
	var observed []observedService
	// DescribeServices accepts at most 10 services per call.
	for _, batch := range chunk(serviceArns, 10) {
		out, err := c.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  aws.String(clusterArn),
			Services: batch,
		})
		if err != nil {
			return nil, err
		}
		for _, svc := range out.Services {
			taskDef := aws.ToString(svc.TaskDefinition)
			if taskDef == "" {
				continue
			}
			images, cached := taskDefImages[taskDef]
			if !cached {
				images, err = imagesForTaskDef(ctx, c, taskDef)
				if err != nil {
					io.WriteString(logsWriter, fmt.Sprintf("aws-ecs: task def %s unreadable (skipping service %s): %v\n",
						nameFromArn(taskDef), aws.ToString(svc.ServiceName), err))
					taskDefImages[taskDef] = nil
					continue
				}
				taskDefImages[taskDef] = images
			}
			for _, img := range images {
				repo, by := recoverRepo(img)
				observed = append(observed, observedService{
					Service:     aws.ToString(svc.ServiceName),
					Cluster:     clusterName,
					Image:       img,
					Repo:        repo,
					RecoveredBy: by,
					Cloud:       "aws",
				})
			}
		}
	}
	return observed, nil
}

// listServices returns every service ARN in a cluster, following pagination.
func listServices(ctx context.Context, c *ecs.Client, clusterArn string) ([]string, error) {
	var arns []string
	var next *string
	for {
		out, err := c.ListServices(ctx, &ecs.ListServicesInput{Cluster: aws.String(clusterArn), NextToken: next})
		if err != nil {
			return nil, err
		}
		arns = append(arns, out.ServiceArns...)
		if out.NextToken == nil {
			return arns, nil
		}
		next = out.NextToken
	}
}

// imagesForTaskDef returns the container image URIs declared in a task definition.
func imagesForTaskDef(ctx context.Context, c *ecs.Client, taskDefArn string) ([]string, error) {
	out, err := c.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(taskDefArn)})
	if err != nil {
		return nil, err
	}
	if out.TaskDefinition == nil {
		return nil, nil
	}
	var images []string
	for _, cd := range out.TaskDefinition.ContainerDefinitions {
		if img := aws.ToString(cd.Image); img != "" {
			images = append(images, img)
		}
	}
	return images, nil
}

// scopedResult packs one cluster's observations into a Cluster-scoped Result. The scope ID is the
// FULL cluster ARN (account + region embedded), so observations from different accounts/regions —
// produced by different runners — never collide when merged. Artifact name + manifest entry path
// both come from File(SourceAwsEcs); BuildInfraContext stamps the pack-level version/BuiltTs.
func scopedResult(clusterArn string, observed []observedService, builtTs int64) context_sources.Result {
	file := context_pack.File(context_pack.SourceAwsEcs)
	withRepo := 0
	for _, o := range observed {
		if o.Repo != "" {
			withRepo++
		}
	}
	entry := context_pack.ManifestEntry{
		Path:       file,
		Source:     context_pack.SourceAwsEcs,
		SourceKind: context_pack_enums.Discovered,
		SyncedTs:   builtTs,
		Confidence: context_pack_enums.ConfidenceHigh, // the services + images are directly observed; the repo join is qualified per-record by RecoveredBy
		Summary: fmt.Sprintf("%d service(s) observed on cluster %s; %d repo-matched",
			len(observed), nameFromArn(clusterArn), withRepo),
	}
	return context_sources.Result{
		Scope:     context_pack.Scope{Level: context_pack_enums.Cluster, ID: clusterArn},
		Artifacts: []context_pack.Artifact{{Name: file, Data: observed}},
		Entries:   []context_pack.ManifestEntry{entry},
	}
}

// recoverRepo best-effort maps a container image URI to the repo that produced it. First slice: the
// image's repository name (last path segment, tag/digest/registry-host stripped), which by
// convention usually matches the source repo — recorded with RecoveredBy "image-name" so
// reconciliation treats it as a hint to confirm against the repo catalog rather than ground truth.
// Higher-confidence recovery (the OCI org.opencontainers.image.source label, or tag-as-commit-SHA)
// is a follow-up and will need ecr:* added to the AwsInfraContext bundle.
func recoverRepo(image string) (repo string, recoveredBy string) {
	name := imageRepoName(image)
	if name == "" {
		return "", ""
	}
	return name, "image-name"
}

// imageRepoName extracts the repository name from a container image URI — the last path segment with
// any digest and tag removed:
//
//	123.dkr.ecr.us-east-1.amazonaws.com/team/billing-api:v3  -> billing-api
//	ghcr.io/acme/web@sha256:abc...                           -> web
//	billing-api:latest                                       -> billing-api
func imageRepoName(image string) string {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 { // strip digest
		ref = ref[:i]
	}
	path := ref
	if slash := strings.LastIndex(ref, "/"); slash >= 0 { // last path segment (drops registry host)
		path = ref[slash+1:]
	}
	if i := strings.LastIndex(path, ":"); i >= 0 { // a ":" in the last segment is a tag, not a port
		path = path[:i]
	}
	return path
}

// nameFromArn returns the human-facing name from an ECS ARN — its last "/"-segment:
//
//	arn:aws:ecs:us-east-1:123456789:cluster/prod -> prod
func nameFromArn(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// chunk splits s into batches of at most n.
func chunk(s []string, n int) [][]string {
	var out [][]string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}
