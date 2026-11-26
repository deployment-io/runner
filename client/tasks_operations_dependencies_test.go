package client

import (
	"net"
	"net/rpc"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// fakeJobsDependencies stands in for deployment-server's Jobs service.
type fakeJobsDependencies struct {
	graphs  chan tasks.RecordTaskDependenciesArgsV1
	bumps   chan tasks.UpdateTaskDependencyBumpArgsV1
	done    bool
	blocked chan struct{}
}

func (f *fakeJobsDependencies) RecordTaskDependenciesV1(args *tasks.RecordTaskDependenciesArgsV1, reply *tasks.RecordTaskDependenciesReplyV1) error {
	f.graphs <- *args
	if f.blocked != nil {
		<-f.blocked
	}
	reply.Done = f.done
	return nil
}

func (f *fakeJobsDependencies) UpdateTaskDependencyBumpV1(args *tasks.UpdateTaskDependencyBumpArgsV1, reply *tasks.UpdateTaskDependencyBumpReplyV1) error {
	f.bumps <- *args
	if f.blocked != nil {
		<-f.blocked
	}
	reply.Done = f.done
	return nil
}

func dependenciesClient(t *testing.T, f *fakeJobsDependencies) *RunnerClient {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("Jobs", f); err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	go srv.ServeConn(serverConn)
	r := &RunnerClient{c: rpc.NewClient(clientConn), isConnected: true, token: "tok"}
	t.Cleanup(func() { r.c.Close() })
	return r
}

func newFakeJobsDependencies(done bool) *fakeJobsDependencies {
	return &fakeJobsDependencies{graphs: make(chan tasks.RecordTaskDependenciesArgsV1, 1), bumps: make(chan tasks.UpdateTaskDependencyBumpArgsV1, 1), done: done}
}

func TestRecordTaskDependenciesCallsTheMethod(t *testing.T) {
	f := newFakeJobsDependencies(true)
	r := dependenciesClient(t, f)
	dto := tasks.TaskDependenciesDtoV1{TaskID: "task", JobID: "job", Provided: []tasks.DependencyProvidedV1{{Repository: 1, Ecosystem: "go", Name: "github.com/acme/kit"}}}
	if err := r.RecordTaskDependencies("org", dto); err != nil {
		t.Fatal(err)
	}
	got := <-f.graphs
	if got.Token != "tok" || got.OrganizationID != "org" {
		t.Errorf("auth = %+v", got.AuthArgsV1)
	}
	if got.Dependencies.TaskID != "task" || got.Dependencies.JobID != "job" || len(got.Dependencies.Provided) != 1 {
		t.Errorf("dependencies = %+v", got.Dependencies)
	}
}

func TestUpdateTaskDependencyBumpCallsTheMethod(t *testing.T) {
	f := newFakeJobsDependencies(true)
	r := dependenciesClient(t, f)
	dto := tasks.UpdateTaskDependencyBumpDtoV1{TaskID: "task", JobID: "job", Repository: 2,
		Results: []tasks.DependencyBumpResultDtoV1{{Ecosystem: "go", Name: "github.com/acme/kit", State: tasks.DependencyBumpStateAlreadyCurrent}}}
	if err := r.UpdateTaskDependencyBump("org", dto); err != nil {
		t.Fatal(err)
	}
	got := <-f.bumps
	if got.Token != "tok" || got.OrganizationID != "org" {
		t.Errorf("auth = %+v", got.AuthArgsV1)
	}
	if got.Update.TaskID != "task" || got.Update.Repository != 2 || len(got.Update.Results) != 1 {
		t.Errorf("update = %+v", got.Update)
	}
}

func TestTaskDependenciesErrorWhenNotDone(t *testing.T) {
	f := newFakeJobsDependencies(false)
	r := dependenciesClient(t, f)
	if err := r.RecordTaskDependencies("org", tasks.TaskDependenciesDtoV1{TaskID: "task"}); err == nil {
		t.Error("RecordTaskDependencies: err = nil, want an error when Done is false")
	}
	if err := r.UpdateTaskDependencyBump("org", tasks.UpdateTaskDependencyBumpDtoV1{TaskID: "task"}); err == nil {
		t.Error("UpdateTaskDependencyBump: err = nil, want an error when Done is false")
	}
}

func TestTaskDependenciesWhenDisconnected(t *testing.T) {
	if err := (&RunnerClient{}).RecordTaskDependencies("org", tasks.TaskDependenciesDtoV1{}); err != ErrConnection {
		t.Errorf("RecordTaskDependencies: err = %v, want ErrConnection", err)
	}
	if err := (&RunnerClient{}).UpdateTaskDependencyBump("org", tasks.UpdateTaskDependencyBumpDtoV1{}); err != ErrConnection {
		t.Errorf("UpdateTaskDependencyBump: err = %v, want ErrConnection", err)
	}
}

func TestTaskDependenciesGiveUpOnAServerThatNeverAnswers(t *testing.T) {
	original := taskDependenciesTimeout
	taskDependenciesTimeout = 50 * time.Millisecond
	defer func() { taskDependenciesTimeout = original }()
	f := newFakeJobsDependencies(true)
	f.blocked = make(chan struct{})
	defer close(f.blocked)
	r := dependenciesClient(t, f)
	done := make(chan error, 1)
	go func() { done <- r.UpdateTaskDependencyBump("org", tasks.UpdateTaskDependencyBumpDtoV1{TaskID: "task"}) }()
	<-f.bumps
	select {
	case err := <-done:
		if err == nil {
			t.Error("err = nil, want a timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateTaskDependencyBump didn't give up")
	}
}
