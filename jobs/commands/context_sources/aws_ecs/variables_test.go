package aws_ecs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/context_pack"
	"github.com/deployment-io/deployment-runner/jobs/commands/context_sources"
)

func twoContainerTaskDef() *ecsTypes.TaskDefinition {
	return &ecsTypes.TaskDefinition{ContainerDefinitions: []ecsTypes.ContainerDefinition{
		{
			Image: aws.String("123.dkr.ecr.us-east-1.amazonaws.com/api:1"),
			Environment: []ecsTypes.KeyValuePair{
				{Name: aws.String("PORT"), Value: aws.String("env-value-sekrit")},
				{Name: aws.String("DB_PASSWORD"), Value: aws.String("hunter2-sekrit")},
				{Name: aws.String("PORT"), Value: aws.String("dup-value-sekrit")},
				{Name: aws.String(""), Value: aws.String("blank-value-sekrit")},
			},
			Secrets: []ecsTypes.Secret{
				{Name: aws.String("API_KEY"), ValueFrom: aws.String("arn:aws:secretsmanager:valuefrom-sekrit")},
				{Name: aws.String("DB_PASSWORD"), ValueFrom: aws.String("arn:aws:ssm:valuefrom2-sekrit")},
			},
			PortMappings: []ecsTypes.PortMapping{
				{ContainerPort: aws.Int32(8080)}, {ContainerPort: aws.Int32(0)}, {ContainerPort: aws.Int32(80)}, {ContainerPort: aws.Int32(8080)},
			},
		},
		{
			Image:        aws.String("public.ecr.aws/envoy:v1"),
			Environment:  []ecsTypes.KeyValuePair{{Name: aws.String("ENVOY_LOG"), Value: aws.String("envoy-value-sekrit")}},
			PortMappings: []ecsTypes.PortMapping{{ContainerPort: aws.Int32(9901)}},
		},
		{Image: aws.String("")}, // no image → skipped
	}}
}

func TestContainersFromTaskDef(t *testing.T) {
	cs := containersFromTaskDef(twoContainerTaskDef())
	if len(cs) != 2 {
		t.Fatalf("got %d containers, want 2: %+v", len(cs), cs)
	}
	if want := []string{"API_KEY", "DB_PASSWORD", "PORT"}; !reflect.DeepEqual(cs[0].variableNames, want) {
		t.Errorf("container 0 names = %v, want %v", cs[0].variableNames, want)
	}
	if want := []int32{80, 8080}; !reflect.DeepEqual(cs[0].ports, want) {
		t.Errorf("container 0 ports = %v, want %v", cs[0].ports, want)
	}
	if want := []string{"ENVOY_LOG"}; !reflect.DeepEqual(cs[1].variableNames, want) || !reflect.DeepEqual(cs[1].ports, []int32{9901}) {
		t.Errorf("container 1 = %+v", cs[1])
	}
	if containersFromTaskDef(nil) != nil {
		t.Errorf("nil task def should yield nothing")
	}

	recs := observedForService("prod", "api", "arn:svc/api", cs)
	b, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sekrit") || strings.Contains(string(b), "hunter2") {
		t.Errorf("a Value or ValueFrom leaked into the records: %s", b)
	}
	for _, key := range []string{`"variableNames":["API_KEY","DB_PASSWORD","PORT"]`, `"ports":[80,8080]`, `"variablesScanned":true`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("records %s missing %s", b, key)
		}
	}
}

func TestRedactKeepsVariableNames(t *testing.T) {
	recs := observedForService("prod", "api", "arn:svc/api", containersFromTaskDef(twoContainerTaskDef()))
	pack := &context_pack.Pack{Artifacts: []context_pack.Artifact{{Name: "aws-ecs.json", Data: recs}}}
	if err := context_sources.Redact(pack); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(pack.Artifacts[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "[redacted]") {
		t.Errorf("Redact blanked part of the record: %s", b)
	}
	for _, key := range []string{`"variableNames":["API_KEY","DB_PASSWORD","PORT"]`, `"ports":[80,8080]`, `"variablesScanned":true`, `"variableNames":["ENVOY_LOG"]`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("redacted records %s missing %s", b, key)
		}
	}
}
