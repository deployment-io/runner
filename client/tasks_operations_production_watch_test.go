package client

import (
	"net"
	"net/rpc"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/tasks"
)

// fakeJobsWatch stands in for deployment-server's Jobs service.
type fakeJobsWatch struct {
	got   chan tasks.UpdateTaskProductionWatchArgsV1
	reply tasks.UpdateTaskProductionWatchReplyV1
	// block, when set, holds the reply until it's closed.
	block chan struct{}
}

func (f *fakeJobsWatch) UpdateTaskProductionWatchV1(args *tasks.UpdateTaskProductionWatchArgsV1, reply *tasks.UpdateTaskProductionWatchReplyV1) error {
	f.got <- *args
	if f.block != nil {
		<-f.block
	}
	*reply = f.reply
	return nil
}

func watchClient(t *testing.T, f *fakeJobsWatch) *RunnerClient {
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

// UpdateTaskProductionWatch calls Jobs.UpdateTaskProductionWatchV1 with the
// runner's token and the update as given, and returns the reply's Stop.
func TestUpdateTaskProductionWatchCallsTheWatchMethod(t *testing.T) {
	for _, wantStop := range []bool{false, true} {
		f := &fakeJobsWatch{got: make(chan tasks.UpdateTaskProductionWatchArgsV1, 1),
			reply: tasks.UpdateTaskProductionWatchReplyV1{Done: true, Stop: wantStop}}
		r := watchClient(t, f)
		update := tasks.UpdateTaskProductionWatchDtoV1{TaskID: "task", JobID: "job",
			Report: tasks.ProductionWatchReportDtoV1{State: tasks.ProductionWatchStateRunning, PolledAt: 10, WindowEndsAt: 20}}
		stop, err := r.UpdateTaskProductionWatch("org", update)
		if err != nil {
			t.Fatal(err)
		}
		if stop != wantStop {
			t.Errorf("stop = %v, want %v", stop, wantStop)
		}
		got := <-f.got
		if got.Token != "tok" || got.OrganizationID != "org" {
			t.Errorf("auth = %+v", got.AuthArgsV1)
		}
		if got.Update.TaskID != "task" || got.Update.JobID != "job" || got.Update.Report.State != tasks.ProductionWatchStateRunning ||
			got.Update.Report.WindowEndsAt != 20 {
			t.Errorf("update = %+v", got.Update)
		}
	}
}

func TestUpdateTaskProductionWatchErrorsWhenNotDone(t *testing.T) {
	f := &fakeJobsWatch{got: make(chan tasks.UpdateTaskProductionWatchArgsV1, 1),
		reply: tasks.UpdateTaskProductionWatchReplyV1{Done: false, Stop: true}}
	r := watchClient(t, f)
	stop, err := r.UpdateTaskProductionWatch("org", tasks.UpdateTaskProductionWatchDtoV1{TaskID: "task"})
	if err == nil {
		t.Fatal("err = nil, want an error when Done is false")
	}
	if stop {
		t.Error("stop = true with an error")
	}
}

func TestUpdateTaskProductionWatchWhenDisconnected(t *testing.T) {
	if _, err := (&RunnerClient{}).UpdateTaskProductionWatch("org", tasks.UpdateTaskProductionWatchDtoV1{}); err != ErrConnection {
		t.Errorf("err = %v, want ErrConnection", err)
	}
}

// UpdateTaskProductionWatch gives up on a server that never answers, so the
// watch isn't held past its stop signal or its cap.
func TestUpdateTaskProductionWatchGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	original := updateTaskProductionWatchTimeout
	updateTaskProductionWatchTimeout = 50 * time.Millisecond
	defer func() { updateTaskProductionWatchTimeout = original }()
	f := &fakeJobsWatch{got: make(chan tasks.UpdateTaskProductionWatchArgsV1, 1), block: make(chan struct{})}
	defer close(f.block)
	r := watchClient(t, f)
	done := make(chan error, 1)
	go func() {
		_, err := r.UpdateTaskProductionWatch("org", tasks.UpdateTaskProductionWatchDtoV1{TaskID: "task"})
		done <- err
	}()
	<-f.got
	select {
	case err := <-done:
		if err == nil {
			t.Error("err = nil, want a timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateTaskProductionWatch didn't give up")
	}
}

// UpdateTaskProductionWatch gives up even when the request can't be written:
// nobody reads the other end of this connection.
func TestUpdateTaskProductionWatchGivesUpOnAConnectionThatWontTakeTheRequest(t *testing.T) {
	original := updateTaskProductionWatchTimeout
	updateTaskProductionWatchTimeout = 50 * time.Millisecond
	defer func() { updateTaskProductionWatchTimeout = original }()
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	r := &RunnerClient{c: rpc.NewClient(clientConn), isConnected: true, token: "tok"}
	defer r.c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := r.UpdateTaskProductionWatch("org", tasks.UpdateTaskProductionWatchDtoV1{TaskID: "task"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("err = nil, want a timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateTaskProductionWatch blocked writing the request")
	}
}
