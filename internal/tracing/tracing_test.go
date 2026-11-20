package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSetupExportsSpansWithHeaders(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tp, shutdown, err := Setup(t.Context(), Config{
		Endpoint:       srv.URL + "/v1/traces",
		ServiceVersion: "test",
		Headers:        map[string]string{"Authorization": "Bearer test-token"},
	})
	require.NoError(t, err)
	_, span := tp.Tracer("test").Start(t.Context(), "work")
	span.End()
	require.NoError(t, shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, got)
	require.Equal(t, http.MethodPost, got[0].Method)
	require.Equal(t, "/v1/traces", got[0].URL.Path)
	require.Equal(t, "Bearer test-token", got[0].Header.Get("Authorization"))
}

func TestSetupWithoutEndpointIsNoop(t *testing.T) {
	tp, shutdown, err := Setup(t.Context(), Config{})
	require.NoError(t, err)
	require.IsType(t, noop.NewTracerProvider(), tp)
	require.NoError(t, shutdown(t.Context()))
}

func TestSetupBuildsGRPCExporter(t *testing.T) {
	_, shutdown, err := Setup(t.Context(), Config{Endpoint: "http://localhost:4317", Protocol: ProtocolGRPC, Headers: map[string]string{"authorization": "x"}})
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
}

func TestSetupRejectsUnknownProtocol(t *testing.T) {
	_, _, err := Setup(t.Context(), Config{Endpoint: "http://localhost:4317", Protocol: "udp"})
	require.ErrorContains(t, err, "udp")
}
