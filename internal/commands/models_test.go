package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func modelsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchContextWindow(t *testing.T) {
	const list = `{"data":[{"id":"a/other","context_length":8000},{"id":"~a/alias","context_length":64000},{"id":"a/zero","context_length":0}]}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		model  string
		want   int64
		errMsg string
	}{
		{"found", 200, list, "a/other", 8000, ""},
		{"alias compares exactly", 200, list, "~a/alias", 64000, ""},
		{"missing", 200, list, "a/alias", 0, "not in the model list"},
		{"bad json", 200, `{`, "a/other", 0, "failed to decode"},
		{"empty body", 200, ``, "a/other", 0, "failed to decode"},
		{"non-200", 500, list, "a/other", 0, "HTTP 500"},
		{"zero length", 200, list, "a/zero", 0, "no context length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, tc.status, tc.body)
			got, err := fetchContextWindow(context.Background(), srv.Client(), srv.URL, "secret-key", tc.model)
			if tc.errMsg != "" {
				require.ErrorContains(t, err, tc.errMsg)
				require.NotContains(t, err.Error(), "secret-key")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFetchContextWindowUnreachableDoesNotLeakKey(t *testing.T) {
	_, err := fetchContextWindow(context.Background(), &http.Client{}, "http://127.0.0.1:1", "secret-key", "m")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-key")
}
