package openshell

import (
	"context"
	"net"
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

	type arrival struct {
		otlp.Record
		at time.Time
	}
	var (
		mu        sync.Mutex
		recs      []arrival
		batches   []int
		encodings []string
	)
	sink := func(_ context.Context, rs []otlp.Record) error {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		for _, r := range rs {
			recs = append(recs, arrival{r, now})
		}
		batches = append(batches, len(rs))
		return nil
	}
	inner, err := otlp.Handler(token, sink)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	box := miseSession(t, []string{"mise"})
	sb, err := box.l.c.Sandboxes().Get(t.Context(), box.l.ws, box.name)
	require.NoError(t, err)

	since := time.Now()
	out := box.run("echo " + b64(miseFixtureConfig) + " | base64 -d > mise.toml && echo " + b64(miseFixtureLock) + " | base64 -d > mise.lock")
	require.Contains(t, out, "rc=0", out)
	out = box.run("mise install")
	require.Contains(t, out, "rc=0", out)
	curlStart := time.Now()
	out = box.run("curl -sS -m 20 -o /dev/null https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-arm64")
	require.NotContains(t, out, "rc=0", out)

	wants := []want{
		{Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/*/*/releases/download/**", Policy: "mise"},
		{Kind: "NET", Binary: "**/curl", Host: "github.com:443"},
	}
	// Other sandboxes on the gateway may export to the same receiver.
	mine := func() []arrival {
		mu.Lock()
		defer mu.Unlock()
		return slices.DeleteFunc(slices.Clone(recs), func(a arrival) bool { return a.SandboxID != sb.ID })
	}
	var ds []Decision
	require.Eventually(t, func() bool {
		ds = ds[:0]
		for _, a := range mine() {
			if d, ok := decisionFromOCSF(a.Body); ok {
				ds = append(ds, d)
			}
		}
		return !slices.ContainsFunc(wants, func(w want) bool { return !slices.ContainsFunc(ds, w.matches) })
	}, 30*time.Second, 500*time.Millisecond, "no OTLP decision matched %+v", wants)

	got := mine()
	require.NotEmpty(t, got, "no OTLP records for sandbox %s", sb.ID)
	mu.Lock()
	batchSizes, encs := slices.Clone(batches), slices.Clone(encodings)
	mu.Unlock()

	seen := map[string]int{}
	dups := 0
	var latency time.Duration
	for _, a := range got {
		if a.UID != "" {
			if seen[a.UID]++; seen[a.UID] == 2 {
				dups++
			}
		}
		if d, ok := decisionFromOCSF(a.Body); ok && latency == 0 && wants[1].matches(d) && !a.at.Before(curlStart) {
			latency = a.at.Sub(curlStart)
		}
		if dump != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dump, strconv.Itoa(a.ClassUID)+"-"+a.UID+".json"), a.Body, 0o600))
		}
	}
	t.Logf("curl to denied NET record latency %s, batches %v, encodings %q, records %d, duplicate uids %d", latency, batchSizes, encs, len(got), dups)

	requireDecisions(t, box.l, box.name, since, wants...)
}
