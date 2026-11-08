package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	agentmcp "github.com/deployment-io/deployment-runner/agent_mcp"
)

// withRegisterTools serves register's tools instead of the real ones for the test.
func withRegisterTools(t *testing.T, register func(*agentmcp.Server, agentboxSpawnSpec) agentTools) {
	t.Helper()
	saved := registerTools
	registerTools = register
	t.Cleanup(func() { registerTools = saved })
}

// callAgentTool sends a tools/call of name with args on a new connection to sock;
// the reply is not read.
func callAgentTool(t *testing.T, sock, name, args string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}` + "\n"
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// orderLog records the teardown's steps.
type orderLog struct {
	mu    sync.Mutex
	steps []string
}

func (o *orderLog) add(step string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, step)
}

func (o *orderLog) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.steps...)
}

func TestAgentToolsTeardownOrder(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	var order orderLog
	started := make(chan struct{})
	sawCancel := make(chan struct{})
	stopped := make(chan struct{})
	withRegisterTools(t, func(srv *agentmcp.Server, _ agentboxSpawnSpec) agentTools {
		srv.Register(agentmcp.Tool{Name: "blocks", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(started)
			<-ctx.Done()
			order.add("cancelled")
			close(sawCancel)
			<-stopped // returns only once the tools are stopped: the wait must come after stop
			order.add("returned")
			return "", ctx.Err()
		}})
		return agentTools{
			stop: func() {
				// The handler hears the cancel on its own goroutine: give it the
				// time to, so a stop that came first shows in the order.
				select {
				case <-sawCancel:
				case <-time.After(2 * time.Second):
				}
				order.add("stop")
				close(stopped)
			},
			finish: func() {
				if _, err := os.Stat(sock); err != nil {
					t.Errorf("the socket is gone before finish: %v", err)
				}
				order.add("finish")
			},
		}
	})
	var logs bytes.Buffer
	closeTools, err := serveAgentTools(agentboxSpawnSpec{mcpSocketHost: sock}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	callAgentTool(t, sock, "blocks", `{}`)
	<-started
	closeTools()
	if want := []string{"cancelled", "stop", "returned", "finish"}; !reflect.DeepEqual(order.get(), want) {
		t.Errorf("teardown order = %v, want %v", order.get(), want)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("the socket is still there: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("logged %q", logs.String())
	}
}

func TestAgentToolsTeardownWarnsAboutCallsStillRunning(t *testing.T) {
	saved := toolCallsWait
	toolCallsWait = 50 * time.Millisecond
	t.Cleanup(func() { toolCallsWait = saved })
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	finished := false
	withRegisterTools(t, func(srv *agentmcp.Server, _ agentboxSpawnSpec) agentTools {
		srv.Register(agentmcp.Tool{Name: "stubborn", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(started)
			<-release // ignores ctx
			return "", nil
		}})
		return agentTools{stop: func() {}, finish: func() { finished = true }}
	})
	var logs bytes.Buffer
	closeTools, err := serveAgentTools(agentboxSpawnSpec{mcpSocketHost: sock}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	callAgentTool(t, sock, "stubborn", `{}`)
	<-started
	closeTools()
	if want := "warning: tool call(s) still running after 50ms: stubborn; the run goes on without them\n"; logs.String() != want {
		t.Errorf("log = %q, want %q", logs.String(), want)
	}
	if !finished {
		t.Error("the teardown didn't go on to finish")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("the socket is still there: %v", err)
	}
}

// A deploy_static_site_preview call the agent left waiting on its build is
// cancelled with the run; the build's result is still heard before the run
// returns.
func TestAgentToolsTeardownHearsTheBuildTheAgentLeft(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	started := make(chan struct{})
	buildDone := make(chan struct{})
	var heard []string
	withRegisterTools(t, func(srv *agentmcp.Server, _ agentboxSpawnSpec) agentTools {
		srv.Register(agentmcp.Tool{Name: "deploy_static_site_preview", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			select {
			case <-buildDone:
				return `{"url":"https://d1.cloudfront.net","status":"deployed"}`, nil
			default:
			}
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}})
		finish := observeSiteDeploys(srv, func(repository, url string, err error) {
			heard = append(heard, repository+" "+url)
		})
		return agentTools{stop: func() { close(buildDone) }, finish: finish}
	})
	closeTools, err := serveAgentTools(agentboxSpawnSpec{mcpSocketHost: sock}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	callAgentTool(t, sock, "deploy_static_site_preview", `{"repository":"/work/0-acme/web"}`)
	<-started
	closeTools()
	if want := []string{"0-acme/web https://d1.cloudfront.net"}; !reflect.DeepEqual(heard, want) {
		t.Errorf("heard %v, want %v", heard, want)
	}
}

func TestRegisterAgentToolsReturnsNoOpsWithoutDeployTools(t *testing.T) {
	for name, spec := range map[string]agentboxSpawnSpec{
		"no preview deps": {},
		"production":      {productionTools: &productionQATools{record: productionQARecord(productionTestTargets)}},
	} {
		tools := registerAgentTools(agentmcp.New("test", "0"), spec)
		if tools.stop == nil || tools.finish == nil {
			t.Fatalf("%s: tools = %+v", name, tools)
		}
		tools.stop()
		tools.finish()
	}
}
