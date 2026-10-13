package client

import (
	"net"
	"net/rpc"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// fakeJobsQA stands in for deployment-server's Jobs service.
type fakeJobsQA struct {
	got chan tasks.UpdateTaskStepQAArgsV1
}

func (f *fakeJobsQA) UpdateTaskStepQAV1(args *tasks.UpdateTaskStepQAArgsV1, reply *tasks.UpdateTaskStepQAReplyV1) error {
	f.got <- *args
	reply.Done = true
	return nil
}

// UpdateTaskStepQA calls Jobs.UpdateTaskStepQAV1 with the runner's token and
// the update as given.
func TestUpdateTaskStepQACallsTheQAMethod(t *testing.T) {
	f := &fakeJobsQA{got: make(chan tasks.UpdateTaskStepQAArgsV1, 1)}
	srv := rpc.NewServer()
	if err := srv.RegisterName("Jobs", f); err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	go srv.ServeConn(serverConn)
	r := &RunnerClient{c: rpc.NewClient(clientConn), isConnected: true, token: "tok"}
	defer r.c.Close()

	update := tasks.UpdateTaskStepQADtoV1{
		TaskID: "task", StepIndex: 1, JobID: "job",
		Result: tasks.QAResultDtoV1{
			Status:   tasks.QAStatusCompleted,
			Criteria: []tasks.QACriterionDtoV1{{Criterion: "c", State: tasks.QACriterionPassed}},
		},
	}
	if err := r.UpdateTaskStepQA("org", update); err != nil {
		t.Fatal(err)
	}
	got := <-f.got
	if got.Token != "tok" || got.OrganizationID != "org" {
		t.Errorf("auth = %+v", got.AuthArgsV1)
	}
	if got.Update.TaskID != "task" || got.Update.StepIndex != 1 || got.Update.Result.Status != tasks.QAStatusCompleted ||
		len(got.Update.Result.Criteria) != 1 {
		t.Errorf("update = %+v", got.Update)
	}
}

func TestUpdateTaskStepQAWhenDisconnected(t *testing.T) {
	if err := (&RunnerClient{}).UpdateTaskStepQA("org", tasks.UpdateTaskStepQADtoV1{}); err != ErrConnection {
		t.Errorf("err = %v, want ErrConnection", err)
	}
}
