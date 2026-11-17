package client

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"strings"
	"testing"
	"time"

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

// fakeJobsEvents is a Jobs service whose events method never answers.
type fakeJobsEvents struct {
	called chan struct{}
	block  chan struct{}
}

func (f *fakeJobsEvents) AppendTaskEventsV1(args *tasks.AppendTaskEventsArgsV1, reply *tasks.AppendTaskEventsReplyV1) error {
	f.called <- struct{}{}
	<-f.block
	reply.Done = true
	return nil
}

// AppendTaskEvents gives up on a server that never answers, so no sender
// waits on it for good.
func TestAppendTaskEventsGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	original := appendTaskEventsTimeout
	appendTaskEventsTimeout = 50 * time.Millisecond
	defer func() { appendTaskEventsTimeout = original }()
	f := &fakeJobsEvents{called: make(chan struct{}, 1), block: make(chan struct{})}
	defer close(f.block)
	srv := rpc.NewServer()
	if err := srv.RegisterName("Jobs", f); err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	go srv.ServeConn(serverConn)
	r := &RunnerClient{c: rpc.NewClient(clientConn), isConnected: true, token: "tok"}
	defer r.c.Close()

	done := make(chan error, 1)
	go func() {
		done <- r.AppendTaskEvents("org", tasks.AppendTaskEventsArgsV1{TaskID: "task", JobID: "job", BatchID: "b"})
	}()
	<-f.called
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("AppendTaskEvents succeeded without an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendTaskEvents is still waiting for an answer")
	}
}

// fakeJobsEvidence stands in for deployment-server's Jobs service with
// UploadQAEvidenceV1; block, when set, holds every call.
type fakeJobsEvidence struct {
	got   chan tasks.UploadQAEvidenceArgsV1
	id    string
	block chan struct{}
}

func (f *fakeJobsEvidence) UploadQAEvidenceV1(args *tasks.UploadQAEvidenceArgsV1, reply *tasks.UploadQAEvidenceReplyV1) error {
	f.got <- *args
	if f.block != nil {
		<-f.block
	}
	reply.EvidenceID = f.id
	return nil
}

// serveJobs serves service as Jobs on a local listener, one net/rpc ServeConn
// per connection as deployment-server does, and returns a client dialing it
// whose shared connection is a pipe nobody serves.
func serveJobs(t *testing.T, service any) *RunnerClient {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("Jobs", service); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go srv.ServeConn(conn)
		}
	}()
	shared, sharedServer := net.Pipe()
	r := &RunnerClient{c: rpc.NewClient(shared), isConnected: true, token: "tok",
		dial: dialerFor(Options{Service: listener.Addr().String()})}
	t.Cleanup(func() { r.c.Close(); sharedServer.Close() })
	return r
}

// UploadQAEvidence calls Jobs.UploadQAEvidenceV1 with the runner's token and
// the upload as given, and returns the stored id.
func TestUploadQAEvidenceCallsTheEvidenceMethod(t *testing.T) {
	f := &fakeJobsEvidence{got: make(chan tasks.UploadQAEvidenceArgsV1, 1), id: "abc123"}
	r := serveJobs(t, f)
	upload := tasks.UploadQAEvidenceDtoV1{
		TaskID: "task", StepIndex: 1, JobID: "job", Round: 2, CheckID: "c4",
		Criteria: []int{0, 2}, Image: []byte{0xFF, 0xD8, 0xFF, 0xE0}, Width: 1280, Height: 720,
	}
	id, err := r.UploadQAEvidence("org", upload)
	if err != nil || id != "abc123" {
		t.Fatalf("UploadQAEvidence = %q, %v", id, err)
	}
	got := <-f.got
	if got.Token != "tok" || got.OrganizationID != "org" {
		t.Errorf("auth = %+v", got.AuthArgsV1)
	}
	if got.Upload.CheckID != "c4" || got.Upload.Round != 2 || len(got.Upload.Criteria) != 2 || got.Upload.Width != 1280 || len(got.Upload.Image) != 4 {
		t.Errorf("upload = %+v", got.Upload)
	}
}

// An empty id is an error: nothing can cite it.
func TestUploadQAEvidenceRefusesAnEmptyID(t *testing.T) {
	f := &fakeJobsEvidence{got: make(chan tasks.UploadQAEvidenceArgsV1, 1)}
	if _, err := serveJobs(t, f).UploadQAEvidence("org", tasks.UploadQAEvidenceDtoV1{}); err == nil {
		t.Fatal("an empty id was accepted")
	}
}

// A deployment-server without the RPC is an error naming the missing method.
func TestUploadQAEvidenceWithoutTheMethod(t *testing.T) {
	_, err := serveJobs(t, &fakeJobsQA{}).UploadQAEvidence("org", tasks.UploadQAEvidenceDtoV1{})
	if err == nil || !strings.Contains(err.Error(), "can't find method") {
		t.Fatalf("err = %v", err)
	}
}

// A server that never answers: the upload gives up, and the shared connection
// is free for the QA result's report meanwhile.
func TestUploadQAEvidenceGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	original := uploadQAEvidenceTimeout
	uploadQAEvidenceTimeout = 100 * time.Millisecond
	defer func() { uploadQAEvidenceTimeout = original }()
	f := &fakeJobsEvidence{got: make(chan tasks.UploadQAEvidenceArgsV1, 1), id: "x", block: make(chan struct{})}
	defer close(f.block)
	r := serveJobs(t, f)
	done := make(chan error, 1)
	go func() {
		_, err := r.UploadQAEvidence("org", tasks.UploadQAEvidenceDtoV1{})
		done <- err
	}()
	<-f.got
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UploadQAEvidence is still waiting for an answer")
	}
}

// The deadline covers sending too: a deployment-server that accepts but never
// reads holds the screenshot's write, and the call still gives up — without
// holding the shared connection, which a report made while it is stalled
// still uses.
func TestUploadQAEvidenceGivesUpOnAServerThatStopsReading(t *testing.T) {
	original := uploadQAEvidenceTimeout
	uploadQAEvidenceTimeout = 200 * time.Millisecond
	defer func() { uploadQAEvidenceTimeout = original }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn // hold it open, never read or reply
		}
	}()
	defer func() {
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	}()

	qa := &fakeJobsQA{got: make(chan tasks.UpdateTaskStepQAArgsV1, 1)}
	srv := rpc.NewServer()
	if err := srv.RegisterName("Jobs", qa); err != nil {
		t.Fatal(err)
	}
	shared, sharedServer := net.Pipe()
	go srv.ServeConn(sharedServer)
	r := &RunnerClient{c: rpc.NewClient(shared), isConnected: true, token: "tok",
		dial: dialerFor(Options{Service: listener.Addr().String()})}
	defer r.c.Close()

	done := make(chan error, 1)
	go func() {
		// Larger than any socket buffer, so the write itself stalls.
		_, err := r.UploadQAEvidence("org", tasks.UploadQAEvidenceDtoV1{Image: make([]byte, 64<<20)})
		done <- err
	}()
	reported := make(chan error, 1)
	go func() { reported <- r.UpdateTaskStepQA("org", tasks.UpdateTaskStepQADtoV1{TaskID: "task"}) }()
	select {
	case err := <-reported:
		if err != nil {
			t.Fatalf("UpdateTaskStepQA = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateTaskStepQA waited on the stalled upload")
	}
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UploadQAEvidence is still sending")
	}
}

func TestUploadQAEvidenceWhenDisconnected(t *testing.T) {
	if _, err := (&RunnerClient{}).UploadQAEvidence("org", tasks.UploadQAEvidenceDtoV1{}); err != ErrConnection {
		t.Errorf("err = %v, want ErrConnection", err)
	}
}
