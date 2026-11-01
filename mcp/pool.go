package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/tools"
)

const (
	connectTimeout     = 30 * time.Second
	terminateDuration  = 2 * time.Second
	maxToolNameLen     = 64
	maxDescriptionLen  = 2048
	defaultCallTimeout = 10 * time.Minute
)

// Pinned is a tool as it was when the session pinned it; it is stored per
// session, so the Spec must not change after discovery.
type Pinned struct {
	Server string           `json:"server"`
	Tool   string           `json:"tool"`
	Spec   durable.ToolSpec `json:"spec"`
}

// Options configures a Pool.
type Options struct {
	// Root is the workspace; stdio servers run there only if their config sets UseWorkspaceCwd.
	Root string
	// LogDir receives the stderr of stdio servers as mcp-<server>.log.
	LogDir string
	// Version is reported to servers as the client version.
	Version string
}

// Pool owns one lazily connected session per configured server.
type Pool struct {
	servers map[string]Server
	opts    Options
	conns   map[string]*conn
	// live counts sessions whose cleanup has not run yet.
	live sync.WaitGroup
	// dial is a seam for tests. cleanup runs once the session is gone.
	dial func(name string, s Server) (t mcpsdk.Transport, cleanup func(), err error)
}

type conn struct {
	mu sync.Mutex
	cs *mcpsdk.ClientSession
}

// NewPool does not connect; servers start on first use.
func NewPool(servers map[string]Server, opts Options) *Pool {
	p := &Pool{servers: servers, opts: opts, conns: map[string]*conn{}}
	for name := range servers {
		p.conns[name] = &conn{}
	}
	p.dial = p.dialServer
	return p
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func (p *Pool) dialServer(name string, s Server) (mcpsdk.Transport, func(), error) {
	if s.Type == typeHTTP {
		u, err := url.Parse(s.URL)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse url of mcp server %q: %w", name, err)
		}
		client := &http.Client{Transport: headerTripper{headers: s.Headers, scheme: u.Scheme, host: u.Host, next: http.DefaultTransport}}
		return &mcpsdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: client, DisableStandaloneSSE: true}, func() {}, nil
	}

	dir := p.opts.Root
	removeDir := func() {}
	if !s.UseWorkspaceCwd {
		tmp, err := os.MkdirTemp("", "durable-mcp-*")
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create MCP working directory: %w", err)
		}
		dir = tmp
		removeDir = func() { _ = os.RemoveAll(tmp) }
	}

	// Not CommandContext: the server must outlive the context of any one call.
	cmd := exec.Command(s.Command, s.Args...) //nolint:gosec,noctx // running the configured command is the point; a call context must not kill it
	cmd.Dir = dir
	// With an explicit Env, os/exec does not update PWD, so the inherited one would name the workspace.
	cmd.Env = append(tools.ScrubEnv(os.Environ()), "PWD="+dir)
	for _, k := range sortedKeys(s.Env) {
		cmd.Env = append(cmd.Env, k+"="+s.Env[k])
	}
	// Own process group, so a terminal Ctrl-C does not reach the server.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cleanup := removeDir
	if p.opts.LogDir != "" {
		if err := os.MkdirAll(p.opts.LogDir, 0o700); err != nil {
			removeDir()
			return nil, nil, fmt.Errorf("failed to create MCP log directory: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(p.opts.LogDir, "mcp-"+unsafeName.ReplaceAllString(name, "_")+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			removeDir()
			return nil, nil, fmt.Errorf("failed to open MCP log file: %w", err)
		}
		cmd.Stderr = f
		cleanup = func() {
			_ = f.Close()
			removeDir()
		}
	}
	return &mcpsdk.CommandTransport{Command: cmd, TerminateDuration: terminateDuration}, cleanup, nil
}

// headerTripper adds headers only for the configured origin, so a redirect cannot carry them elsewhere.
type headerTripper struct {
	headers      map[string]string
	scheme, host string
	next         http.RoundTripper
}

func (h headerTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == h.scheme && strings.EqualFold(r.URL.Host, h.host) {
		r = r.Clone(r.Context())
		for k, v := range h.headers {
			r.Header.Set(k, v)
		}
	}
	return h.next.RoundTrip(r)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// session returns the live session for name, connecting if there is none.
func (p *Pool) session(ctx context.Context, name string) (*mcpsdk.ClientSession, error) {
	s := p.servers[name]
	c := p.conns[name]
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs != nil {
		return c.cs, nil
	}

	t, cleanup, err := p.dial(name, s)
	if err != nil {
		return nil, err
	}
	version := p.opts.Version
	if version == "" {
		version = "dev"
	}
	// No roots, sampling or elicitation: servers must not ask the client for them.
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "durable", Version: version}, &mcpsdk.ClientOptions{Capabilities: &mcpsdk.ClientCapabilities{}})
	// The session does not depend on this context after Connect returns.
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cs, err := client.Connect(cctx, t, nil)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to connect to mcp server %q: %w", name, err)
	}
	c.cs = cs
	p.live.Go(func() {
		_ = cs.Wait()
		c.mu.Lock()
		if c.cs == cs {
			c.cs = nil
		}
		c.mu.Unlock()
		cleanup()
	})
	return cs, nil
}

// Discover lists the tools of every server. A server that fails is reported
// as a warning and skipped.
func (p *Pool) Discover(ctx context.Context) ([]Pinned, []string) {
	names := make([]string, 0, len(p.servers))
	for name := range p.servers {
		names = append(names, name)
	}
	slices.Sort(names)

	var pinned []Pinned
	var warnings []string
	seen := map[string]bool{}
	for _, name := range names {
		got, warns := p.discoverServer(ctx, name)
		warnings = append(warnings, warns...)
		for _, pt := range got {
			if seen[pt.Spec.Name] {
				warnings = append(warnings, fmt.Sprintf("mcp tool %q from server %q skipped: name collides with an earlier tool", pt.Tool, name))
				continue
			}
			seen[pt.Spec.Name] = true
			pinned = append(pinned, pt)
		}
	}
	return pinned, warnings
}

func (p *Pool) discoverServer(ctx context.Context, name string) ([]Pinned, []string) {
	cs, err := p.session(ctx, name)
	if err != nil {
		return nil, []string{fmt.Sprintf("mcp server %q skipped: %v", name, err)}
	}
	lctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	var listed []*mcpsdk.Tool
	for t, err := range cs.Tools(lctx, nil) {
		if err != nil {
			return nil, []string{fmt.Sprintf("mcp server %q skipped: failed to list tools: %v", name, err)}
		}
		listed = append(listed, t)
	}
	slices.SortFunc(listed, func(a, b *mcpsdk.Tool) int { return strings.Compare(a.Name, b.Name) })

	var out []Pinned
	var warnings []string
	for _, t := range listed {
		schema, err := inputSchema(t.InputSchema)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp tool %q from server %q skipped: %v", t.Name, name, err))
			continue
		}
		out = append(out, Pinned{
			Server: name,
			Tool:   t.Name,
			Spec: durable.ToolSpec{
				Name:        toolName(name, t.Name),
				Description: capUTF8(t.Description, maxDescriptionLen),
				Schema:      schema,
				NonStrict:   true,
			},
		})
	}
	return out, warnings
}

func toolName(server, tool string) string {
	full := unsafeName.ReplaceAllString("mcp__"+server+"__"+tool, "_")
	if len(full) <= maxToolNameLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	// Hash the full name so truncated names stay distinct.
	return full[:maxToolNameLen-9] + "_" + hex.EncodeToString(sum[:4])
}

func inputSchema(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("input schema is not valid JSON: %w", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || string(probe["type"]) != `"object"` {
		return nil, errors.New("input schema must be a JSON object with type \"object\"")
	}
	// The Claude API rejects these at the top level, which would fail every request that includes the tool.
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := probe[k]; ok {
			return nil, fmt.Errorf("input schema must not use %s at the top level", k)
		}
	}
	return raw, nil
}

func capUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Close closes every open session and waits until each one's cleanup has run.
func (p *Pool) Close() error {
	var first error
	for _, c := range p.conns {
		c.mu.Lock()
		cs := c.cs
		c.cs = nil
		c.mu.Unlock()
		if cs == nil {
			continue
		}
		if err := cs.Close(); err != nil && first == nil {
			first = fmt.Errorf("failed to close mcp session: %w", err)
		}
	}
	p.live.Wait()
	return first
}
