package commands

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

// fakeTaskDefinitionFamilies serves each family's revisions in pages of
// pageSize; a family it doesn't have lists no revisions, as ECS does.
type fakeTaskDefinitionFamilies struct {
	families     map[string][]string
	pageSize     int
	listInputs   []*ecs.ListTaskDefinitionsInput
	deregistered []string
	deleted      []string
}

func (f *fakeTaskDefinitionFamilies) ListTaskDefinitions(_ context.Context, in *ecs.ListTaskDefinitionsInput, _ ...func(*ecs.Options)) (*ecs.ListTaskDefinitionsOutput, error) {
	f.listInputs = append(f.listInputs, in)
	revisions := f.families[aws.ToString(in.FamilyPrefix)]
	start := 0
	if in.NextToken != nil {
		start = len(aws.ToString(in.NextToken))
	}
	end := min(start+f.pageSize, len(revisions))
	output := &ecs.ListTaskDefinitionsOutput{TaskDefinitionArns: revisions[start:end]}
	if end < len(revisions) {
		output.NextToken = aws.String(string(bytes.Repeat([]byte("x"), end)))
	}
	return output, nil
}

func (f *fakeTaskDefinitionFamilies) DeregisterTaskDefinition(_ context.Context, in *ecs.DeregisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error) {
	f.deregistered = append(f.deregistered, aws.ToString(in.TaskDefinition))
	return &ecs.DeregisterTaskDefinitionOutput{}, nil
}

func (f *fakeTaskDefinitionFamilies) DeleteTaskDefinitions(_ context.Context, in *ecs.DeleteTaskDefinitionsInput, _ ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error) {
	if len(in.TaskDefinitions) > 10 {
		panic("DeleteTaskDefinitions takes at most 10 revisions")
	}
	f.deleted = append(f.deleted, in.TaskDefinitions...)
	return &ecs.DeleteTaskDefinitionsOutput{}, nil
}

func revisions(family string, n int) []string {
	var arns []string
	for i := 1; i <= n; i++ {
		arns = append(arns, "arn:aws:ecs:eu-west-1:123456789012:task-definition/"+family+":"+string(rune('0'+i)))
	}
	return arns
}

func TestDeleteTaskDefinitions(t *testing.T) {
	original := deleteTaskDefinitionPause
	t.Cleanup(func() { deleteTaskDefinitionPause = original })
	deleteTaskDefinitionPause = 0
	parameters := preDeployParameters("")

	service, preDeploy := revisions("td-deployment-1", 2), revisions("td-deployment-1-pre-deploy", 7)
	ecsClient := &fakeTaskDefinitionFamilies{pageSize: 3, families: map[string][]string{
		"td-deployment-1":            service,
		"td-deployment-1-pre-deploy": preDeploy,
	}}
	var logs bytes.Buffer
	if err := deleteTaskDefinitions(parameters, ecsClient, &logs); err != nil {
		t.Fatalf("deleteTaskDefinitions: %v", err)
	}
	want := append(append([]string{}, service...), preDeploy...)
	if !reflect.DeepEqual(ecsClient.deregistered, want) {
		t.Errorf("deregistered %q; want every revision of both families %q", ecsClient.deregistered, want)
	}
	if !reflect.DeepEqual(ecsClient.deleted, want) {
		t.Errorf("deleted %q; want %q", ecsClient.deleted, want)
	}

	// No pre-deploy family: the service's own is still deleted.
	ecsClient = &fakeTaskDefinitionFamilies{pageSize: 100, families: map[string][]string{"td-deployment-1": service}}
	if err := deleteTaskDefinitions(parameters, ecsClient, &logs); err != nil {
		t.Fatalf("a missing pre-deploy family must not fail the delete: %v", err)
	}
	if !reflect.DeepEqual(ecsClient.deregistered, service) {
		t.Errorf("deregistered %q; want %q", ecsClient.deregistered, service)
	}
	var families []string
	for _, in := range ecsClient.listInputs {
		families = append(families, aws.ToString(in.FamilyPrefix))
	}
	if want := []string{"td-deployment-1", "td-deployment-1-pre-deploy"}; !reflect.DeepEqual(families, want) {
		t.Errorf("listed %q; want %q", families, want)
	}
}

// Both delete commands delete the task definitions through
// deleteTaskDefinitions (tested above with fakes). The rest of Run needs live
// AWS, so this pins it in the source.
func TestDeleteCommandsDeleteBothTaskDefinitionFamilies(t *testing.T) {
	for _, file := range []string{"delete_aws_web_service.go", "delete_aws_private_service.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		calls := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					calls[selector.Sel.Name] = true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok {
					calls[ident.Name] = true
				}
			}
			return true
		})
		if !calls["deleteTaskDefinitions"] || calls["ListTaskDefinitions"] {
			t.Errorf("%s: Run must delete the task definitions through deleteTaskDefinitions", file)
		}
	}
}
