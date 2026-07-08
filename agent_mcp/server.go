// Package agent_mcp serves a minimal Model Context Protocol (MCP) endpoint over
// a Unix socket so the coding agent running inside agentbox can invoke
// runner-executed tools. The runner holds all credentials and performs the work;
// the agent only speaks MCP over a bind-mounted socket (bridged to its stdio MCP
// client). This is the C0 transport skeleton: a generic tool registry plus a
// `ping` tool. Later capabilities (deploy_preview, verify_preview, and
// eventually Fetched-context connectors) register as tools here.
//
// Framing matches MCP's stdio transport — newline-delimited JSON-RPC 2.0 — so a
// trivial stdio<->socket byte bridge on the agentbox side is all that connects
// the agent's (stdio-speaking) MCP client to this socket. The runner owns the
// protocol; the bridge is a dumb pipe.
package agent_mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

// defaultProtocolVersion is the MCP version advertised when the client doesn't
// request one. On initialize we echo the client's requested version (maximally
// compatible) and fall back to this.
const defaultProtocolVersion = "2024-11-05"

// Tool is one capability the agent can invoke. Handler runs on the runner (with
// the runner's credentials); args is the decoded `arguments` object from a
// tools/call. The returned string is surfaced to the agent as text content; a
// returned error becomes an MCP result with isError=true so the agent can read
// the failure and react (rather than the call hard-failing).
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema for the tool's arguments (raw JSON). Nil is
	// fine for a no-arg tool — an empty object schema is advertised.
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server is a registry of tools served over a Unix socket. Register all tools
// BEFORE calling Serve/Listen: the serving goroutines read the registry without
// synchronization, which is safe only because the go-statement that starts them
// happens-after registration. Registration is NOT safe to interleave with
// serving. If tools ever need to change at runtime, guard the maps with a mutex
// and emit notifications/tools/list_changed.
type Server struct {
	name    string
	version string

	tools map[string]Tool
	order []string // registration order, for a stable tools/list
}

// New returns a Server that identifies itself with name/version in the MCP
// initialize handshake.
func New(name, version string) *Server {
	return &Server{name: name, version: version, tools: map[string]Tool{}}
}

// Register adds (or replaces, last-wins) a tool, preserving first-registration
// order in tools/list. Call before Serve/Listen — see the Server doc.
func (s *Server) Register(t Tool) {
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
}

// Listen creates the Unix socket at socketPath (removing any stale file first)
// and chmods it 0666 so the container's non-root agent user (UID 1000) can
// connect through the bind mount — safe because the socket is task-scoped and
// only reachable from inside that one task's container. Call Listen
// synchronously before spawning the container (Docker turns a missing bind
// source into a directory), then hand the listener to ServeListener.
func (s *Server) Listen(socketPath string) (net.Listener, error) {
	if err := os.RemoveAll(socketPath); err != nil {
		return nil, fmt.Errorf("agent_mcp: clear stale socket %s: %w", socketPath, err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("agent_mcp: listen %s: %w", socketPath, err)
	}
	_ = os.Chmod(socketPath, 0o666)
	return ln, nil
}

// ServeListener accepts and handles agent connections on ln until ctx is
// cancelled (which closes ln). Blocks; run it in a goroutine.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil // clean shutdown
			default:
				return fmt.Errorf("agent_mcp: accept: %w", err)
			}
		}
		go s.handleConn(ctx, conn)
	}
}

// Serve is Listen + ServeListener for callers that don't need the socket to
// exist before a separate step (e.g. tests). Blocks; run it in a goroutine.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	ln, err := s.Listen(socketPath)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

// --- JSON-RPC 2.0 / MCP wire types ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// MCP stdio framing is newline-delimited JSON. Raise the buffer so large
	// args/results don't truncate a line.
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(conn) // Encode appends '\n' — keeps framing

	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue // can't recover an id from a malformed line; skip it
		}
		resp, isNotification := s.dispatch(ctx, req)
		if isNotification {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	isNotification := len(req.ID) == 0
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = s.initializeResult(req.Params)
	case "notifications/initialized", "notifications/cancelled":
		return resp, true // fire-and-forget notifications
	case "ping": // MCP protocol keepalive (distinct from the `ping` tool)
		resp.Result = map[string]interface{}{}
	case "tools/list":
		resp.Result = s.toolsList()
	case "tools/call":
		result, rerr := s.toolsCall(ctx, req.Params)
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
	default:
		if isNotification {
			return resp, true // ignore unknown notifications
		}
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, isNotification
}

func (s *Server) initializeResult(params json.RawMessage) map[string]interface{} {
	version := defaultProtocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion // echo the client's version
		}
	}
	return map[string]interface{}{
		"protocolVersion": version,
		"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		"serverInfo":      map[string]interface{}{"name": s.name, "version": s.version},
	}
}

func (s *Server) toolsList() map[string]interface{} {
	tools := make([]map[string]interface{}, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return map[string]interface{}{"tools": tools}
}

func (s *Server) toolsCall(ctx context.Context, params json.RawMessage) (map[string]interface{}, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	t, ok := s.tools[p.Name]
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
	text, err := t.Handler(ctx, p.Arguments)
	if err != nil {
		// Surface tool failure as an MCP result (isError) so the agent reads the
		// text and iterates, rather than the JSON-RPC call erroring out.
		return map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": err.Error()}},
			"isError": true,
		}, nil
	}
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	}, nil
}
