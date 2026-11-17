package openshell

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/otlp"
)

// The gateway must export OCSF events to DURABLE_TEST_OTLP_ADDR with the bearer token DURABLE_TEST_OTLP_TOKEN.
func TestGatewayOTLPDecisions(t *testing.T) {
	addr := os.Getenv("DURABLE_TEST_OTLP_ADDR")
	if addr == "" {
		t.Skip("set DURABLE_TEST_OTLP_ADDR to receive OCSF events over OTLP")
	}
	token := os.Getenv("DURABLE_TEST_OTLP_TOKEN")
	require.NotEmpty(t, token, "set DURABLE_TEST_OTLP_TOKEN")
	dump := os.Getenv("DURABLE_TEST_OTLP_DUMP")

	var (
		mu        sync.Mutex
		recs      []otlp.Record
		batches   []int
		encodings []string
		first     time.Time
	)
	sink := func(_ context.Context, rs []otlp.Record) error {
		mu.Lock()
		defer mu.Unlock()
		if first.IsZero() {
			first = time.Now()
		}
		recs = append(recs, rs...)
		batches = append(batches, len(rs))
		return nil
	}
	inner := otlp.Handler(token, sink)
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })

	started := time.Now()
	box := miseSession(t, []string{"mise"})
	sb, err := box.l.c.Sandboxes().Get(t.Context(), box.l.ws, box.name)
	require.NoError(t, err)

	since := time.Now()
	out := box.run("echo " + b64(miseFixtureConfig) + " | base64 -d > mise.toml && echo " + b64(miseFixtureLock) + " | base64 -d > mise.lock")
	require.Contains(t, out, "rc=0", out)
	out = box.run("mise install")
	require.Contains(t, out, "rc=0", out)
	out = box.run("curl -sS -m 20 -o /dev/null https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-arm64")
	require.NotContains(t, out, "rc=0", out)

	wants := []want{
		{Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/*/*/releases/download/**", Policy: "mise"},
		{Kind: "NET", Binary: "**/curl", Host: "github.com:443"},
	}
	var ds []Decision
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		ds = ds[:0]
		for _, r := range recs {
			if d, ok := decisionFromOCSF(r.Body); ok && r.SandboxID == sb.ID {
				ds = append(ds, d)
			}
		}
		return !slices.ContainsFunc(wants, func(w want) bool { return !slices.ContainsFunc(ds, w.matches) })
	}, 30*time.Second, 500*time.Millisecond, "no OTLP decision matched %+v", wants)

	mu.Lock()
	seen := map[string]int{}
	dups := 0
	for _, r := range recs {
		if r.SandboxID != "" {
			require.Equal(t, sb.ID, r.SandboxID, "record %s", r.UID)
		}
		if seen[r.UID]++; seen[r.UID] == 2 {
			dups++
		}
		if dump != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dump, strconv.Itoa(r.ClassUID)+"-"+r.UID+".json"), r.Body, 0o600))
		}
	}
	t.Logf("time to first record %s, batches %v, encodings %q, records %d, duplicate uids %d", first.Sub(started), batches, encodings, len(recs), dups)
	mu.Unlock()

	requireDecisions(t, box.l, box.name, since, wants...)
}
