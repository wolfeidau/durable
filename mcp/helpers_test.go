package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const stdioServerEnv = "DURABLE_MCP_TEST_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		runStdioServer()
		return
	}
	os.Exit(m.Run())
}

func runStdioServer() {
	os.Stderr.WriteString("stdio server started\n")
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stdio", Version: "1"}, nil)
	s.AddTool(objectTool("report"), func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		cwd, _ := os.Getwd()
		out := map[string]string{
			"anthropic": os.Getenv("ANTHROPIC_API_KEY"),
			"op":        os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"),
			"foo":       os.Getenv("FOO"),
			"cwd":       cwd,
		}
		b, _ := json.Marshal(out)
		return text(string(b)), nil
	})
	_ = s.Run(context.Background(), &mcpsdk.StdioTransport{})
}

func objectTool(name string) *mcpsdk.Tool {
	return &mcpsdk.Tool{Name: name, Description: "tool " + name, InputSchema: map[string]any{"type": "object"}}
}

func text(s string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: s}}}
}

// memServer is an in-process MCP server reachable through a Pool dial seam.
type memServer struct {
	server *mcpsdk.Server
	mu     sync.Mutex
	// sessions are the server ends, so tests can drop the connection.
	sessions []*mcpsdk.ServerSession
	dials    int
}

func newMemServer(opts *mcpsdk.ServerOptions) *memServer {
	return &memServer{server: mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mem", Version: "1"}, opts)}
}

func (m *memServer) drop(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ss := range m.sessions {
		_ = ss.Close()
	}
	m.sessions = nil
}

func (m *memServer) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

func newMemPool(t *testing.T, servers map[string]*memServer, cfg map[string]Server) *Pool {
	t.Helper()
	if cfg == nil {
		cfg = map[string]Server{}
		for name := range servers {
			cfg[name] = Server{Type: typeStdio, Command: "unused"}
		}
	}
	p := NewPool(cfg, Options{})
	p.dial = func(name string, _ Server) (mcpsdk.Transport, func(), error) {
		m := servers[name]
		ct, st := mcpsdk.NewInMemoryTransports()
		ss, err := m.server.Connect(context.Background(), st, nil)
		if err != nil {
			return nil, nil, err
		}
		m.mu.Lock()
		m.sessions = append(m.sessions, ss)
		m.dials++
		m.mu.Unlock()
		return ct, func() {}, nil
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func discover(t *testing.T, p *Pool) ([]Pinned, []string) {
	t.Helper()
	pinned, warnings := p.Discover(context.Background())
	require.NotNil(t, pinned)
	return pinned, warnings
}
