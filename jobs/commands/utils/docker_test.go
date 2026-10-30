package utils

import (
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestGetDockerBuildArgs(t *testing.T) {
	p := map[string]interface{}{}
	jobs.SetParameterValue(p, parameters_enums.DockerBuildArgs, primitive.A{"URL=https://x/?a=b", "K=", "A=b=c=", "BARE"})
	args, err := GetDockerBuildArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"URL": "https://x/?a=b", "K": "", "A": "b=c="}
	if len(args) != len(want) {
		t.Errorf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for k, w := range want {
		if args[k] == nil || *args[k] != w {
			t.Errorf("%s = %v, want %q", k, args[k], w)
		}
	}
	if _, ok := args["BARE"]; ok {
		t.Error("an entry without = was kept")
	}
}

func TestGetDockerBuildArgsNone(t *testing.T) {
	args, err := GetDockerBuildArgs(map[string]interface{}{})
	if args != nil || err != nil {
		t.Errorf("got %v, %v; want nil, nil", args, err)
	}
}
