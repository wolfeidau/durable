package commands

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/mcp"
)

const (
	// maxMCPLine bounds one message from a sandboxed server, whose output is untrusted.
	maxMCPLine = 4 << 20
	// maxMCPLog bounds the stderr kept per connection, for the same reason.
	maxMCPLog = 1 << 20
	// headersEnv is read by `durable-worker mcp-http`.
	headersEnv = "DURABLE_MCP_HEADERS"
)

// stdioSpec maps a configured server to the process the worker runs. An HTTP server runs through the bridge, so its
// traffic leaves from the sandbox.
func stdioSpec(s mcp.Server) sandbox.StdioSpec {
	if s.URL == "" {
		return sandbox.StdioSpec{Command: s.Command, Args: s.Args, Env: s.Env, WorkspaceCwd: s.UseWorkspaceCwd}
	}
	spec := sandbox.StdioSpec{Command: sandbox.WorkerBinary, Args: []string{"mcp-http", "--url", s.URL}}
	if len(s.Headers) > 0 {
		// A map of strings always encodes.
		b, _ := json.Marshal(s.Headers)
		spec.Env = map[string]string{headersEnv: string(b)}
	}
	return spec
}

// mcpEgress lists the host and port of each HTTP server, which is all the bridge may reach.
func mcpEgress(servers map[string]mcp.Server) []sandbox.Endpoint {
	var out []sandbox.Endpoint
	for _, s := range servers {
		if s.URL == "" {
			continue
		}
		u, err := url.Parse(s.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "443"
			if u.Scheme == "http" {
				port = "80"
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			continue
		}
		e := sandbox.Endpoint{Host: strings.ToLower(u.Hostname()), Port: uint32(n)}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b sandbox.Endpoint) int {
		return cmp.Or(strings.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port))
	})
	return out
}

// sandboxDial runs each server on the sandbox's worker and speaks MCP over the stream.
func sandboxDial(c *sandbox.Client, logDir string) func(context.Context, string, mcp.Server) (mcpsdk.Transport, func(), error) {
	return func(ctx context.Context, name string, s mcp.Server) (mcpsdk.Transport, func(), error) {
		var stderr io.Writer
		closeLog := func() {}
		if logDir != "" {
			f, err := mcp.OpenLog(logDir, name)
			if err != nil {
				return nil, nil, err
			}
			stderr = &cappedWriter{w: f, left: maxMCPLog}
			closeLog = func() { _ = f.Close() }
		}
		rwc, err := c.Stdio(ctx, stdioSpec(s), stderr)
		if err != nil {
			closeLog()
			return nil, nil, err
		}
		cleanup := func() {
			_ = rwc.Close()
			closeLog()
		}
		return &mcpsdk.IOTransport{Reader: rwc, Writer: rwc, MaxLineLength: maxMCPLine}, cleanup, nil
	}
}

// cappedWriter drops output past its limit, leaving one marker line.
type cappedWriter struct {
	w      io.Writer
	left   int
	marked bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		if !c.marked {
			c.marked = true
			_, _ = io.WriteString(c.w, "\n[durable] stderr limit reached; the rest is dropped\n")
		}
		return len(p), nil
	}
	n := min(len(p), c.left)
	c.left -= n
	if _, err := c.w.Write(p[:n]); err != nil {
		return 0, err
	}
	return len(p), nil
}
