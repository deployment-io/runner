package agent_mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// TestServerPingRoundTrip exercises the runner half of the C0 transport: it
// serves on a real Unix socket and drives the MCP handshake + ping tool the way
// the agentbox bridge (and Claude Code behind it) will — newline-delimited
// JSON-RPC 2.0.
func TestServerPingRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	s := New("deployment-io-runner", "test")
	RegisterPing(s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx, sock) }()

	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ { // wait for the listener to bind
		if conn, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	call := func(line string) map[string]any {
		t.Helper()
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		respLine, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(respLine, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", respLine, err)
		}
		return m
	}

	// initialize echoes the client's protocol version.
	init := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	res, _ := init["result"].(map[string]any)
	if res == nil || res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("initialize result = %v", init)
	}

	// notifications/initialized is fire-and-forget (no response); send it and
	// make sure the next request still gets answered on the same conn.
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatalf("write notification: %v", err)
	}

	// tools/list surfaces exactly the ping tool.
	list := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	lr, _ := list["result"].(map[string]any)
	tools, _ := lr["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools/list = %v", list)
	}
	if first, _ := tools[0].(map[string]any); first["name"] != "ping" {
		t.Fatalf("expected ping tool, got %v", first)
	}

	// tools/call ping -> pong.
	callResp := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ping","arguments":{}}}`)
	cr, _ := callResp["result"].(map[string]any)
	content, _ := cr["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("tools/call result = %v", callResp)
	}
	if c0, _ := content[0].(map[string]any); c0["text"] != "pong" {
		t.Fatalf("expected pong, got %v", callResp)
	}

	// unknown tool -> JSON-RPC error.
	bad := call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if bad["error"] == nil {
		t.Fatalf("expected error for unknown tool, got %v", bad)
	}
}
