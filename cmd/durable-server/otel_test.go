package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const otelHeaderValue = "test-header-value"

// collector records the spans POSTed to /v1/traces.
type collector struct {
	srv     *httptest.Server
	mu      sync.Mutex
	spans   []*tracepb.Span
	service []string
	header  string
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req collectortracepb.ExportTraceServiceRequest
		if r.URL.Path != "/v1/traces" || proto.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.header = r.Header.Get("x-test")
		for _, rs := range req.GetResourceSpans() {
			for _, kv := range rs.GetResource().GetAttributes() {
				if kv.GetKey() == "service.name" {
					c.service = append(c.service, kv.GetValue().GetStringValue())
				}
			}
			for _, ss := range rs.GetScopeSpans() {
				c.spans = append(c.spans, ss.GetSpans()...)
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collectortracepb.ExportTraceServiceResponse{})
		_, _ = w.Write(out)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) snapshot() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...)
}

func spanAttrs(s *tracepb.Span) map[string]*commonpb.AnyValue {
	m := map[string]*commonpb.AnyValue{}
	for _, kv := range s.GetAttributes() {
		m[kv.GetKey()] = kv.GetValue()
	}
	return m
}

// runToolPrompt drives one prompt with one approved bash call to completion.
func runToolPrompt(t *testing.T, home, ws string, r *running, api *fakeAPI) {
	t.Helper()
	id := createSession(t, ws, home, "s")
	c := newClient(r)
	_, err := c.submit(t, id, "run it")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return api.count() == 1 }, testTimeout, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		_, err := c.decide(t, id, 1, true)
		return connect.CodeOf(err) != connect.CodeNotFound
	}, testTimeout, 20*time.Millisecond)
	waitDone(t, c, id)
}

// requireSpanTree checks the run, agent, chat and tool spans and returns the tool span.
func requireSpanTree(t *testing.T, spans []*tracepb.Span) *tracepb.Span {
	t.Helper()
	names := map[string]string{} // span ID to name
	var chat bool
	var tool *tracepb.Span
	for _, s := range spans {
		names[string(s.GetSpanId())] = s.GetName()
		chat = chat || strings.HasPrefix(s.GetName(), "chat ")
		if s.GetName() == "execute_tool bash" {
			tool = s
		}
	}
	require.True(t, chat, "no chat span in %v", names)
	require.NotNil(t, tool, "no tool span in %v", names)
	// A run that parks for approval resumes as a new run, so match by parent rather than by name.
	parented := func(child, parent string) bool {
		for _, s := range spans {
			if s.GetName() == child && names[string(s.GetParentSpanId())] == parent {
				return true
			}
		}
		return false
	}
	require.True(t, parented("invoke_agent durable", "durable.run"), "%v", names)
	require.True(t, parented("execute_tool bash", "invoke_agent durable"), "%v", names)
	return tool
}

func TestTracesReachTheCollector(t *testing.T) {
	t.Parallel()
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture_content=%t", capture), func(t *testing.T) {
			t.Parallel()
			ws, home := newWorkspace(t)
			col := newCollector(t)
			writeConfig(t, home, fmt.Sprintf(`{"otel": {"endpoint": %q, "headers": {"x-test": "env:DURABLE_TEST_OTEL_HEADER"}, "capture_content": %t}}`, col.srv.URL+"/v1/traces", capture))
			api := newFakeAPI(t, bashReply("echo ran"), 0)
			env := append(apiEnv(api), "DURABLE_TEST_OTEL_HEADER="+otelHeaderValue)
			r := startEnv(t, home, env, "--api-token", "dummy")

			runToolPrompt(t, home, ws, r, api)
			// A graceful stop flushes spans the batcher hasn't exported yet.
			r.stop(t)

			var spans []*tracepb.Span
			require.Eventually(t, func() bool {
				spans = col.snapshot()
				return len(spans) >= 4
			}, 10*time.Second, 50*time.Millisecond)

			tool := requireSpanTree(t, spans)

			col.mu.Lock()
			require.Contains(t, col.service, "durable-server")
			require.Equal(t, otelHeaderValue, col.header)
			col.mu.Unlock()

			if capture {
				require.Contains(t, spanAttrs(tool), "gen_ai.tool.call.arguments")
				return
			}
			for _, s := range spans {
				for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result"} {
					require.NotContains(t, spanAttrs(s), key, "span %s", s.GetName())
				}
			}
		})
	}
}

func TestInvalidOTelConfigFailsStartup(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ otel, want string }{
		"relative endpoint":      {`{"endpoint": "localhost:4318"}`, "otel.endpoint"},
		"unknown protocol":       {`{"endpoint": "http://localhost:4318", "protocol": "udp"}`, "otel.protocol"},
		"pasted header":          {`{"endpoint": "http://localhost:4318", "headers": {"authorization": "Bearer abc"}}`, "otel.headers.authorization"},
		"content without target": {`{"capture_content": true}`, "otel.capture_content"},
		"unresolvable header":    {`{"endpoint": "http://localhost:4318", "headers": {"authorization": "env:DURABLE_TEST_UNSET_VAR"}}`, "otel.headers.authorization"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			writeConfig(t, home, `{"otel": `+tt.otel+`}`)

			out := runFailingIn(t, home, []string{"DURABLE_TEST_UNSET_VAR="})

			require.Contains(t, out, tt.want)
			require.NotContains(t, out, "Bearer abc")
			require.NoFileExists(t, filepath.Join(home, ".durable", "server.json"))
		})
	}
}
