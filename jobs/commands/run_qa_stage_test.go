package commands

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/commands_enums"
)

func TestRunQAStageReturnsTheParametersUnchanged(t *testing.T) {
	parameters := map[string]interface{}{"1": "a", "2": int64(3)}
	want := map[string]interface{}{"1": "a", "2": int64(3)}
	var logs bytes.Buffer
	got, err := (&RunQAStage{}).Run(parameters, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parameters = %v, want %v", got, want)
	}
	if logs.Len() != 0 {
		t.Errorf("logs = %q, want nothing", logs.String())
	}
}

func TestGetMapsRunQAStage(t *testing.T) {
	cmd, err := Get(commands_enums.RunQAStage)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cmd.(*RunQAStage); !ok {
		t.Errorf("Get(RunQAStage) = %T, want *RunQAStage", cmd)
	}
}
