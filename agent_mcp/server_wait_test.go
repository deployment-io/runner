package agent_mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serveForTest serves s on a fresh socket until the returned cancel is called.
func serveForTest(t *testing.T, s *Server) (sock string, cancel context.CancelFunc) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := s.Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.ServeListener(ctx, ln) }()
	return sock, cancel
}

// sendCall writes a tools/call of name on a new connection and returns the
// connection's reader.
func sendCall(t *testing.T, sock, name string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

func TestWaitWithNoCallsReturnsAtOnce(t *testing.T) {
	s := New("test", "0")
	start := time.Now()
	if got := s.Wait(time.Minute); got != nil {
		t.Errorf("Wait = %v, want nil", got)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Wait took %s with no calls", time.Since(start))
	}
}

func TestWaitWaitsForACallThatStopsOnCancel(t *testing.T) {
	s := New("test", "0")
	started := make(chan struct{})
	var returned atomic.Bool
	s.Register(Tool{Name: "slow", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		returned.Store(true)
		return "", ctx.Err()
	}})
	sock, cancel := serveForTest(t, s)
	sendCall(t, sock, "slow")
	<-started
	cancel()
	if got := s.Wait(5 * time.Second); got != nil {
		t.Fatalf("Wait = %v, want nil", got)
	}
	if !returned.Load() {
		t.Error("Wait returned before the call did")
	}
}

func TestWaitReturnsTheCallsStillRunning(t *testing.T) {
	s := New("test", "0")
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 2)
	ignoring := func(ctx context.Context, _ json.RawMessage) (string, error) {
		started <- struct{}{}
		<-release // ignores ctx
		return "", nil
	}
	s.Register(Tool{Name: "b_tool", Handler: ignoring})
	s.Register(Tool{Name: "a_tool", Handler: ignoring})
	sock, cancel := serveForTest(t, s)
	sendCall(t, sock, "b_tool")
	sendCall(t, sock, "a_tool")
	<-started
	<-started
	cancel()
	got := s.Wait(50 * time.Millisecond)
	if want := []string{"a_tool", "b_tool"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Wait = %v, want %v", got, want)
	}
}

func TestCallAfterWaitIsRefused(t *testing.T) {
	s := New("test", "0")
	var ran atomic.Bool
	s.Register(Tool{Name: "t", Handler: func(context.Context, json.RawMessage) (string, error) {
		ran.Store(true)
		return "ok", nil
	}})
	s.Wait(time.Second)
	res, rerr := s.toolsCall(context.Background(), json.RawMessage(`{"name":"t","arguments":{}}`))
	if rerr != nil {
		t.Fatalf("rpc error: %v", rerr)
	}
	if res["isError"] != true {
		t.Errorf("result = %v, want isError", res)
	}
	if content, _ := res["content"].([]map[string]interface{}); len(content) != 1 || content[0]["text"] != "the agent run is over" {
		t.Errorf("result = %v", res)
	}
	if ran.Load() {
		t.Error("the handler ran after Wait started")
	}
}

func TestRequestAfterCancelIsNotDispatched(t *testing.T) {
	s := New("test", "0")
	var calls atomic.Int32
	s.Register(Tool{Name: "t", Handler: func(context.Context, json.RawMessage) (string, error) {
		calls.Add(1)
		return "ok", nil
	}})
	sock, cancel := serveForTest(t, s)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := r.ReadBytes('\n'); err != nil {
		t.Fatalf("read: %v", err)
	}
	cancel()
	_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"t","arguments":{}}}` + "\n"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := r.ReadBytes('\n'); err == nil {
		t.Errorf("got a reply after cancel: %s", line)
	} else if strings.Contains(err.Error(), "timeout") {
		t.Error("the connection stayed open after cancel")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("handler ran %d times after cancel", n)
	}
}

func TestNoGoroutineLeftPerConnection(t *testing.T) {
	s := New("test", "0")
	RegisterPing(s)
	sock, _ := serveForTest(t, s)
	// Let the serving goroutines start before taking the baseline.
	conn, r := sendCall(t, sock, "ping")
	if _, err := r.ReadBytes('\n'); err != nil {
		t.Fatalf("read: %v", err)
	}
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		conn, r := sendCall(t, sock, "ping")
		if _, err := r.ReadBytes('\n'); err != nil {
			t.Fatalf("read: %v", err)
		}
		conn.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d after the connections closed, want at most %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
