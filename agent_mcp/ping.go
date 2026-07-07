package agent_mcp

import (
	"context"
	"encoding/json"
)

// RegisterPing adds the C0 skeleton tool: a no-arg `ping` that returns "pong".
// Its sole purpose is to validate the end-to-end transport — agent's MCP client
// -> stdio bridge -> Unix socket -> runner -> back — before real tools land.
func RegisterPing(s *Server) {
	s.Register(Tool{
		Name:        "ping",
		Description: `Health check for the deployment.io runner tool channel. Returns "pong".`,
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return "pong", nil
		},
	})
}
