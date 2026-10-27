package client

import (
	"net"
	"net/rpc"
	"reflect"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// fakeTaskPreviews stands in for deployment-server's TaskPreviews service.
type fakeTaskPreviews struct {
	got   chan task_previews.StackWiringArgsV1
	reply task_previews.StackWiringReplyV1
}

func (f *fakeTaskPreviews) StackWiringV1(args *task_previews.StackWiringArgsV1, reply *task_previews.StackWiringReplyV1) error {
	f.got <- *args
	*reply = f.reply
	return nil
}

// StackWiring calls TaskPreviews.StackWiringV1 on its own connection with the
// runner's token, the Task and every repository.
func TestStackWiringCallsTheMethodOnItsOwnConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	link := task_previews.StackLinkV1{FrontendRepository: "0-acme/web", Frontend: "web", APIRepository: "1-acme/api", API: "api",
		APIURLVariables: []task_previews.StackURLVariableV1{{Name: "VITE_API_URL", Path: "/v1"}}}
	f := &fakeTaskPreviews{got: make(chan task_previews.StackWiringArgsV1, 1), reply: task_previews.StackWiringReplyV1{Links: []task_previews.StackLinkV1{link}}}
	srv := rpc.NewServer()
	if err := srv.RegisterName("TaskPreviews", f); err != nil {
		t.Fatal(err)
	}
	go srv.Accept(listener)

	r := &RunnerClient{isConnected: true, token: "tok", dial: dialerFor(Options{Service: listener.Addr().String()})}
	repos := []task_previews.StackRepositoryV1{{Repository: "0-acme/web", CloneURL: "https://github.com/acme/web.git"}, {Repository: "1-acme/api", CloneURL: "https://github.com/acme/api.git"}}
	reply, err := r.StackWiring("org", "task", repos, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reply.Links, []task_previews.StackLinkV1{link}) {
		t.Errorf("reply = %+v", reply)
	}
	got := <-f.got
	if got.Token != "tok" || got.OrganizationID != "org" || got.TaskID != "task" || !reflect.DeepEqual(got.Repositories, repos) {
		t.Errorf("args = %+v", got)
	}
}

func TestStackWiringWhenDisconnected(t *testing.T) {
	if _, err := (&RunnerClient{}).StackWiring("org", "task", nil, time.Second); err != ErrConnection {
		t.Errorf("err = %v, want ErrConnection", err)
	}
}
