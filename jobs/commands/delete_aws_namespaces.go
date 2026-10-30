package commands

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sd_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DeleteAwsNamespaces is the only command of a Delete environment namespaces Job, sent when an
// environment is deleted. The first web service, private service or database deployed into an
// environment creates a Cloud Map private DNS namespace for it (createNamespaceIfNeeded), and
// with it a Route 53 private hosted zone; nothing else ever deletes them. It deletes each of the
// environment's namespaces — the names its deployments recorded (NamespaceNames) and the one
// getNamespaceName computes from its current name — that no Cloud Map service uses any more. A
// namespace with a service is left in place: no service is ever deleted here.
type DeleteAwsNamespaces struct {
}

// addDeleteNamespacesPolicy grants the runner the web-service deploy policy, which has the
// servicediscovery actions. Swapped by tests.
var addDeleteNamespacesPolicy = func(parameters map[string]interface{}) error {
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationIDNamespace)
	if err != nil {
		return err
	}
	runnerData := utils.RunnerData.Get()
	return iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsWebServiceDeployment,
		runnerData.OsType.String(), runnerData.CpuArchEnum.String(), organizationID, runnerData.RunnerRegion, runnerData.Mode, runnerData.TargetCloud)
}

func (d *DeleteAwsNamespaces) Run(parameters map[string]interface{}, logsWriter io.Writer) (map[string]interface{}, error) {
	if err := addDeleteNamespacesPolicy(parameters); err != nil {
		return parameters, err
	}
	names, err := namespaceCandidates(parameters)
	if err != nil {
		return parameters, err
	}
	client, err := getServiceDiscoveryClient(parameters)
	if err != nil {
		return parameters, err
	}
	// Every candidate is tried; only a failed Cloud Map call fails the command.
	var failures []string
	for _, name := range names {
		if err := deleteNamespaceIfUnused(client, name, logsWriter); err != nil {
			io.WriteString(logsWriter, fmt.Sprintf("Couldn't delete namespace %s: %s\n", name, err))
			failures = append(failures, fmt.Sprintf("%s: %s", name, err))
		}
	}
	if len(failures) > 0 {
		return parameters, fmt.Errorf("error deleting namespaces: %s", strings.Join(failures, "; "))
	}
	return parameters, nil
}

// namespaceCandidates are the job's NamespaceNames followed by the environment's computed
// namespace name, de-duplicated.
func namespaceCandidates(parameters map[string]interface{}) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if len(name) > 0 && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	recorded, err := jobs.GetParameterValue[primitive.A](parameters, parameters_enums.NamespaceNames)
	if err == nil {
		for _, value := range recorded {
			if name, ok := value.(string); ok {
				add(name)
			}
		}
	}
	computed, err := getNamespaceName(parameters)
	if err != nil {
		return nil, err
	}
	add(computed)
	return names, nil
}

// deleteNamespaceIfUnused deletes the namespace called name unless it doesn't exist or still has
// a service. It doesn't wait for the deletion to finish.
func deleteNamespaceIfUnused(client serviceDiscoveryAPI, name string, logsWriter io.Writer) error {
	namespaceID, err := findNamespaceID(client, name)
	if err != nil {
		return err
	}
	if len(namespaceID) == 0 {
		io.WriteString(logsWriter, fmt.Sprintf("Namespace %s doesn't exist; nothing to delete\n", name))
		return nil
	}
	services, err := countNamespaceServices(client, namespaceID)
	if err != nil {
		return err
	}
	if services > 0 {
		io.WriteString(logsWriter, fmt.Sprintf("Namespace %s still has %d services; left in place\n", name, services))
		return nil
	}
	deleteNamespaceOutput, err := client.DeleteNamespace(context.TODO(), &servicediscovery.DeleteNamespaceInput{
		Id: aws.String(namespaceID),
	})
	if err != nil {
		return err
	}
	io.WriteString(logsWriter, fmt.Sprintf("Deleting namespace %s (operation %s)\n", name,
		aws.ToString(deleteNamespaceOutput.OperationId)))
	return nil
}

// findNamespaceID is the ID of the namespace called name, or "" if there is none.
func findNamespaceID(client serviceDiscoveryAPI, name string) (string, error) {
	listNamespacesInput := &servicediscovery.ListNamespacesInput{
		Filters: []sd_types.NamespaceFilter{{
			Name:      sd_types.NamespaceFilterNameName,
			Values:    []string{name},
			Condition: sd_types.FilterConditionEq,
		}},
	}
	// Cloud Map filters each page, so an empty page can come before the match.
	for {
		listNamespacesOutput, err := client.ListNamespaces(context.TODO(), listNamespacesInput)
		if err != nil {
			return "", err
		}
		if len(listNamespacesOutput.Namespaces) > 0 {
			return aws.ToString(listNamespacesOutput.Namespaces[0].Id), nil
		}
		if len(aws.ToString(listNamespacesOutput.NextToken)) == 0 {
			return "", nil
		}
		listNamespacesInput.NextToken = listNamespacesOutput.NextToken
	}
}

// countNamespaceServices is the number of Cloud Map services in the namespace.
func countNamespaceServices(client serviceDiscoveryAPI, namespaceID string) (int, error) {
	listServicesInput := &servicediscovery.ListServicesInput{
		Filters: []sd_types.ServiceFilter{{
			Name:      sd_types.ServiceFilterNameNamespaceId,
			Values:    []string{namespaceID},
			Condition: sd_types.FilterConditionEq,
		}},
	}
	count := 0
	for {
		listServicesOutput, err := client.ListServices(context.TODO(), listServicesInput)
		if err != nil {
			return 0, err
		}
		count += len(listServicesOutput.Services)
		if len(aws.ToString(listServicesOutput.NextToken)) == 0 {
			return count, nil
		}
		listServicesInput.NextToken = listServicesOutput.NextToken
	}
}
