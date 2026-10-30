package commands

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sd_types "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// fakeNamespaces stands in for Cloud Map holding several namespaces. namespaces maps a name to
// its ID; services maps a namespace ID to its service count. errs makes an operation on a name
// (ListNamespaces) or an ID (ListServices, DeleteNamespace) fail. calls records
// "<operation> <argument>".
type fakeNamespaces struct {
	serviceDiscoveryAPI
	namespaces map[string]string
	services   map[string]int
	errs       map[string]error
	calls      []string
}

func (f *fakeNamespaces) call(operation, argument string) error {
	f.calls = append(f.calls, operation+" "+argument)
	return f.errs[operation+" "+argument]
}

func (f *fakeNamespaces) ListNamespaces(_ context.Context, in *servicediscovery.ListNamespacesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.ListNamespacesOutput, error) {
	filter := in.Filters[0]
	if filter.Name != sd_types.NamespaceFilterNameName || filter.Condition != sd_types.FilterConditionEq || len(filter.Values) != 1 {
		return nil, errors.New("unexpected namespace filter")
	}
	name := filter.Values[0]
	if err := f.call("ListNamespaces", name); err != nil {
		return nil, err
	}
	// The match comes on a second page, after an empty filtered one.
	if in.NextToken == nil {
		return &servicediscovery.ListNamespacesOutput{NextToken: aws.String("1")}, nil
	}
	id, ok := f.namespaces[name]
	if !ok {
		return &servicediscovery.ListNamespacesOutput{}, nil
	}
	return &servicediscovery.ListNamespacesOutput{Namespaces: []sd_types.NamespaceSummary{{Id: aws.String(id), Name: aws.String(name)}}}, nil
}

func (f *fakeNamespaces) ListServices(_ context.Context, in *servicediscovery.ListServicesInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.ListServicesOutput, error) {
	filter := in.Filters[0]
	if filter.Name != sd_types.ServiceFilterNameNamespaceId || len(filter.Values) != 1 {
		return nil, errors.New("unexpected service filter")
	}
	id := filter.Values[0]
	if err := f.call("ListServices", id); err != nil {
		return nil, err
	}
	// One service per page.
	page := 0
	if in.NextToken != nil {
		page = int(aws.ToString(in.NextToken)[0] - '0')
	}
	output := &servicediscovery.ListServicesOutput{}
	if page < f.services[id] {
		output.Services = []sd_types.ServiceSummary{{Id: aws.String("srv")}}
	}
	if page+1 < f.services[id] {
		output.NextToken = aws.String(string(rune('0' + page + 1)))
	}
	return output, nil
}

func (f *fakeNamespaces) DeleteNamespace(_ context.Context, in *servicediscovery.DeleteNamespaceInput, _ ...func(*servicediscovery.Options)) (*servicediscovery.DeleteNamespaceOutput, error) {
	id := aws.ToString(in.Id)
	if err := f.call("DeleteNamespace", id); err != nil {
		return nil, err
	}
	return &servicediscovery.DeleteNamespaceOutput{OperationId: aws.String("op-" + id)}, nil
}

// deleted are the namespace IDs DeleteNamespace was called with.
func (f *fakeNamespaces) deleted() []string {
	var ids []string
	for _, c := range f.calls {
		if id, ok := strings.CutPrefix(c, "DeleteNamespace "); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// listed are the names ListNamespaces was first called with.
func (f *fakeNamespaces) listed() []string {
	var names []string
	seen := map[string]bool{}
	for _, c := range f.calls {
		if name, ok := strings.CutPrefix(c, "ListNamespaces "); ok && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func runDeleteAwsCloudMapNamespaces(t *testing.T, cloudMap *fakeNamespaces, recorded ...string) (string, error) {
	t.Helper()
	originalPolicy, originalClient := addDeleteNamespacesPolicy, getServiceDiscoveryClient
	t.Cleanup(func() { addDeleteNamespacesPolicy, getServiceDiscoveryClient = originalPolicy, originalClient })
	addDeleteNamespacesPolicy = func(map[string]interface{}) error { return nil }
	getServiceDiscoveryClient = func(map[string]interface{}) (serviceDiscoveryAPI, error) { return cloudMap, nil }

	parameters := map[string]interface{}{}
	jobs.SetParameterValue(parameters, parameters_enums.EnvironmentName, "Rollout Test")
	jobs.SetParameterValue(parameters, parameters_enums.EnvironmentID, "6720a1b2c3d4e5f60718cc26")
	names := primitive.A{}
	for _, name := range recorded {
		names = append(names, name)
	}
	jobs.SetParameterValue(parameters, parameters_enums.CloudMapNamespaceNames, names)

	command, err := Get(commands_enums.DeleteAwsCloudMapNamespaces)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var logs bytes.Buffer
	_, err = command.Run(parameters, &logs)
	return logs.String(), err
}

func TestDeleteAwsCloudMapNamespacesDeletesAnUnusedNamespace(t *testing.T) {
	cloudMap := &fakeNamespaces{namespaces: map[string]string{"rollout-test-cc26": "ns-1"}}
	logs, err := runDeleteAwsCloudMapNamespaces(t, cloudMap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cloudMap.deleted(); !reflect.DeepEqual(got, []string{"ns-1"}) {
		t.Errorf("deleted %v; want [ns-1]", got)
	}
	if !strings.Contains(logs, "Deleting namespace rollout-test-cc26 (operation op-ns-1)\n") {
		t.Errorf("logs = %q", logs)
	}
}

func TestDeleteAwsCloudMapNamespacesLeavesANamespaceWithServices(t *testing.T) {
	cloudMap := &fakeNamespaces{
		namespaces: map[string]string{"rollout-test-cc26": "ns-1"},
		services:   map[string]int{"ns-1": 2},
	}
	logs, err := runDeleteAwsCloudMapNamespaces(t, cloudMap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := cloudMap.deleted(); len(got) != 0 {
		t.Errorf("deleted %v; want nothing", got)
	}
	if !strings.Contains(logs, "Namespace rollout-test-cc26 still has 2 services; left in place\n") {
		t.Errorf("logs = %q", logs)
	}
}

func TestDeleteAwsCloudMapNamespacesSkipsAMissingNamespace(t *testing.T) {
	cloudMap := &fakeNamespaces{}
	logs, err := runDeleteAwsCloudMapNamespaces(t, cloudMap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range cloudMap.calls {
		if !strings.HasPrefix(c, "ListNamespaces ") {
			t.Errorf("unexpected call %q for a missing namespace", c)
		}
	}
	if !strings.Contains(logs, "Namespace rollout-test-cc26 doesn't exist; nothing to delete\n") {
		t.Errorf("logs = %q", logs)
	}
}

// The recorded names come first, each once; the computed name is added (once, even when recorded).
func TestDeleteAwsCloudMapNamespacesCandidates(t *testing.T) {
	cloudMap := &fakeNamespaces{namespaces: map[string]string{
		"old-name-cc26":     "ns-old",
		"rollout-test-cc26": "ns-1",
	}}
	if _, err := runDeleteAwsCloudMapNamespaces(t, cloudMap, "old-name-cc26", "old-name-cc26", "rollout-test-cc26"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := cloudMap.listed(), []string{"old-name-cc26", "rollout-test-cc26"}; !reflect.DeepEqual(got, want) {
		t.Errorf("looked up %v; want %v", got, want)
	}
	if got, want := cloudMap.deleted(), []string{"ns-old", "ns-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v; want %v", got, want)
	}
}

func TestDeleteAwsCloudMapNamespacesIncludesTheComputedName(t *testing.T) {
	cloudMap := &fakeNamespaces{namespaces: map[string]string{"rollout-test-cc26": "ns-1"}}
	if _, err := runDeleteAwsCloudMapNamespaces(t, cloudMap, "old-name-cc26"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := cloudMap.listed(), []string{"old-name-cc26", "rollout-test-cc26"}; !reflect.DeepEqual(got, want) {
		t.Errorf("looked up %v; want %v", got, want)
	}
}

// A failed call fails the command, but only after every other candidate was tried.
func TestDeleteAwsCloudMapNamespacesFailureTriesTheRest(t *testing.T) {
	for _, failing := range []string{"ListNamespaces a-0001", "ListServices ns-b", "DeleteNamespace ns-c"} {
		cloudMap := &fakeNamespaces{
			namespaces: map[string]string{"a-0001": "ns-a", "b-0001": "ns-b", "c-0001": "ns-c", "rollout-test-cc26": "ns-1"},
			errs:       map[string]error{failing: errors.New("throttled")},
		}
		_, err := runDeleteAwsCloudMapNamespaces(t, cloudMap, "a-0001", "b-0001", "c-0001")
		if err == nil {
			t.Fatalf("%s failing: want an error", failing)
		}
		if !strings.Contains(err.Error(), "throttled") {
			t.Errorf("%s failing: error = %v", failing, err)
		}
		if got := cloudMap.listed(); !reflect.DeepEqual(got, []string{"a-0001", "b-0001", "c-0001", "rollout-test-cc26"}) {
			t.Errorf("%s failing: looked up %v; want every candidate", failing, got)
		}
		if !reflect.DeepEqual(cloudMap.deleted()[len(cloudMap.deleted())-1:], []string{"ns-1"}) {
			t.Errorf("%s failing: deleted %v; want the last candidate deleted too", failing, cloudMap.deleted())
		}
	}
}

func TestDeleteAwsCloudMapNamespacesErrorNamesTheFailedNamespaces(t *testing.T) {
	cloudMap := &fakeNamespaces{
		namespaces: map[string]string{"b-0001": "ns-b", "rollout-test-cc26": "ns-1"},
		errs: map[string]error{
			"DeleteNamespace ns-b":  errors.New("resource in use"),
			"ListNamespaces a-0001": errors.New("access denied"),
		},
	}
	_, err := runDeleteAwsCloudMapNamespaces(t, cloudMap, "a-0001", "b-0001")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"a-0001: access denied", "b-0001: resource in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "rollout-test-cc26") {
		t.Errorf("error %q names a namespace that was deleted", err)
	}
}

func TestDeleteAwsCloudMapNamespacesPolicyFailure(t *testing.T) {
	originalPolicy := addDeleteNamespacesPolicy
	t.Cleanup(func() { addDeleteNamespacesPolicy = originalPolicy })
	addDeleteNamespacesPolicy = func(map[string]interface{}) error { return errors.New("no iam") }
	if _, err := (&DeleteAwsCloudMapNamespaces{}).Run(map[string]interface{}{}, &bytes.Buffer{}); err == nil || err.Error() != "no iam" {
		t.Errorf("Run = %v; want the policy error", err)
	}
}
