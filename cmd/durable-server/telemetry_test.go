package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/wolfeidau/durable/internal/config"
	"github.com/wolfeidau/durable/internal/otlp"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/telemetry"
)

const (
	fixtureSandbox   = "dt-63539bd5bc24"
	fixtureSandboxID = "126316d7-ba94-4724-b97b-50ba61698af0"
	telemetryConfig  = `{"telemetry":{"otlp_addr":"127.0.0.1:0"}}`
	// Wide enough that the fixtures' fixed timestamps are inside the window whenever the test runs.
	auditSince = "87600h"
)

var headersLine = regexp.MustCompile(`^authorization=Bearer ([0-9a-f]{64})\n$`)

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func fixtureRecords(t *testing.T) []*logspb.LogRecord {
	t.Helper()
	var recs []*logspb.LogRecord
	for _, name := range []string{"net-allowed-grant", "net-denied-curl", "net-refused-dns", "http-allowed-grant", "net-session-established"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "internal", "sandbox", "openshell", "testdata", "ocsf", name+".json"))
		require.NoError(t, err)
		var ev struct {
			ClassUID int `json:"class_uid"`
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(body, &ev))
		recs = append(recs, &logspb.LogRecord{
			ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
			Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(body)}},
			Attributes:           []*commonpb.KeyValue{strAttr("ocsf.metadata.uid", ev.Metadata.UID), intAttr("ocsf.class_uid", int64(ev.ClassUID))},
		})
	}
	return recs
}

func gapRecord() *logspb.LogRecord {
	return &logspb.LogRecord{
		EventName:            "openshell.ocsf.gap",
		ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "OCSF log records dropped before export: 3"}},
		Attributes: []*commonpb.KeyValue{
			intAttr("openshell.ocsf.gap.dropped", 3),
			intAttr("openshell.ocsf.gap.since_unix_ms", time.Now().Add(-time.Hour).UnixMilli()),
		},
	}
}

func exportLogs(t *testing.T, addr, token string, recs ...*logspb.LogRecord) int {
	t.Helper()
	return exportLogsFrom(t, addr, token, fixtureSandboxID, recs...)
}

// exportLogsFrom sends recs with no sandbox id on the resource when sandboxID is empty.
func exportLogsFrom(t *testing.T, addr, token, sandboxID string, recs ...*logspb.LogRecord) int {
	t.Helper()
	var attrs []*commonpb.KeyValue
	if sandboxID != "" {
		attrs = append(attrs, strAttr("openshell.sandbox.id", sandboxID))
	}
	body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: attrs},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}},
	}}})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/v1/logs", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func readHeadersToken(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".durable", "otlp-headers")
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	m := headersLine.FindStringSubmatch(string(data))
	require.NotNil(t, m, string(data))
	return m[1]
}

func readOTLPAddr(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".durable", "server.json"))
	require.NoError(t, err)
	var info serverInfo
	require.NoError(t, json.Unmarshal(data, &info))
	require.NotEmpty(t, info.OTLPAddr)
	return info.OTLPAddr
}

func writeAuditPin(t *testing.T, home, id string) {
	t.Helper()
	writeAuditPinNamed(t, home, id, fixtureSandbox)
}

func writeAuditPinNamed(t *testing.T, home, id, name string) {
	t.Helper()
	dir := filepath.Join(home, ".durable", "sessions", id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	data, err := json.Marshal(sandbox.Pin{Name: name, Image: "worker:test", Token: "unused"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, sandbox.PinFile), data, 0o600))
}

func stopServer(t *testing.T, r *running) {
	t.Helper()
	require.NoError(t, r.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("server did not stop: %s", r.out.String())
	}
}

func TestTelemetryReceivesAndAuditReadsStore(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, telemetryConfig)
	r := startIn(t, home)
	addr := readOTLPAddr(t, home)
	token := readHeadersToken(t, home)

	recs := fixtureRecords(t)
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, recs...))
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, recs...), "duplicate export")
	require.Equal(t, http.StatusUnauthorized, exportLogs(t, addr, strings.Repeat("0", 64), recs...))
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, gapRecord()))

	const session = "aaaaaaaaaaaaaaaa"
	writeAuditPin(t, home, session)

	// The server is still running, so this also covers a read-only open beside the writer.
	out, err := sandboxAudit(t, home, session, "--since", auditSince)
	require.NoError(t, err, out)
	require.Regexp(t, `allowed\s+\S+\s+\S+\s+GET\s+github\.com`, out)
	require.Regexp(t, `denied\s+\S+\s+/usr/bin/curl\s+\S+\s+github\.com:443`, out)
	require.Regexp(t, `denied\s+\S*:REFUSE\s+.*mise-versions\.jdx\.dev`, out)
	require.Contains(t, out, "2 allowed, 2 denied")
	require.Contains(t, out, "source: telemetry")
	require.Contains(t, out, "warning: 3 events from this sandbox were dropped before export since ")

	out, err = sandboxAudit(t, home, session, "--since", auditSince, "--denied")
	require.NoError(t, err, out)
	require.NotContains(t, out, "allowed  ")
	require.Contains(t, out, "mise-versions.jdx.dev")

	out, err = sandboxAudit(t, home, session, "--daily", "--days", "3650")
	require.NoError(t, err, out)
	require.Contains(t, out, "DAY")
	require.Contains(t, out, "TOP DENIED")
	require.Contains(t, out, "mise-versions.jdx.dev (1)")
	require.Contains(t, out, "github.com (1)")
	var rows int
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) > 3 && regexp.MustCompile(`^\d{4}-\d\d-\d\d$`).MatchString(f[0]) {
			rows++
			require.Equal(t, "2", f[1], line)
			require.Equal(t, "2", f[2], line)
		}
	}
	require.Equal(t, 1, rows, out)

	stdout, stderr, err := sandboxAuditSplit(t, home, session, "--raw", "--since", auditSince)
	require.NoError(t, err, stderr)
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	require.Len(t, lines, 6, "four decisions, the session event and the gap")
	uids := map[string]bool{}
	var gaps int
	for _, line := range lines {
		recs, err := otlp.DecodeRaw([]byte(line))
		require.NoError(t, err, line)
		require.Len(t, recs, 1, line)
		require.Equal(t, fixtureSandboxID, recs[0].SandboxID)
		if recs[0].Gap != nil {
			gaps++
			continue
		}
		require.NotEmpty(t, recs[0].UID, line)
		require.False(t, uids[recs[0].UID], "duplicate uid %s", recs[0].UID)
		uids[recs[0].UID] = true
	}
	require.Equal(t, 1, gaps)
	require.Len(t, uids, 5)
	require.Contains(t, stderr, "warning: 3 events from this sandbox were dropped before export since ")

	store, err := telemetry.OpenReadOnly(t.Context(), filepath.Join(home, ".durable", "telemetry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	raw, err := store.RawEvents(t.Context(), fixtureSandbox, []string{fixtureSandboxID}, time.Time{})
	require.NoError(t, err)
	require.Len(t, raw, 6)
	require.True(t, json.Valid(raw[0].Record))
	stopServer(t, r)
}

func TestAuditDailyNeedsTelemetryStore(t *testing.T) {
	home := t.TempDir()
	writeAuditPin(t, home, "bbbbbbbbbbbbbbbb")

	out, err := sandboxAudit(t, home, "bbbbbbbbbbbbbbbb", "--daily")
	require.Error(t, err, out)
	require.Contains(t, out, "no telemetry store; enable telemetry.otlp_addr in config.json")
}

func TestAuditRawNeedsTelemetryStore(t *testing.T) {
	home := t.TempDir()
	writeAuditPin(t, home, "eeeeeeeeeeeeeeee")

	out, err := sandboxAudit(t, home, "eeeeeeeeeeeeeeee", "--raw")

	require.Error(t, err, out)
	require.Contains(t, out, "--raw needs the telemetry store; enable telemetry.otlp_addr in config.json")
}

func TestAuditRawRejectsConflictingFlags(t *testing.T) {
	home := t.TempDir()
	writeAuditPin(t, home, "ffffffffffffffff")

	out, err := sandboxAudit(t, home, "ffffffffffffffff", "--raw", "--daily")
	require.Error(t, err, out)
	require.Contains(t, out, "--raw does not apply to --daily")

	out, err = sandboxAudit(t, home, "ffffffffffffffff", "--raw", "--denied")
	require.Error(t, err, out)
	require.Contains(t, out, "--denied does not apply to --raw")
}

func TestTelemetryPortClashFailsBeforeServing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	home := t.TempDir()
	writeConfig(t, home, `{"telemetry":{"otlp_addr":"`+ln.Addr().String()+`"}}`)

	out := runFailingIn(t, home, nil)

	require.Contains(t, out, "telemetry")
	require.NoFileExists(t, filepath.Join(home, ".durable", "server.json"))
}

func TestTelemetryHeadersFileSurvivesRestart(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, telemetryConfig)
	r := startIn(t, home)
	first := readHeadersToken(t, home)
	stopServer(t, r)

	startIn(t, home)
	require.Equal(t, first, readHeadersToken(t, home))
}

func TestMalformedHeadersFileFailsStartup(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, telemetryConfig)
	path := filepath.Join(home, ".durable", "otlp-headers")
	require.NoError(t, os.WriteFile(path, []byte("authorization=Bearer short\n"), 0o600))

	out := runFailingIn(t, home, nil)

	require.Contains(t, out, "otlp-headers")
	require.NoFileExists(t, filepath.Join(home, ".durable", "server.json"))
}

func TestReceiverCloseWithExpiredContextStillClosesStore(t *testing.T) {
	home := t.TempDir()
	settings := &config.Settings{TelemetryAddr: "127.0.0.1:0", TelemetryRetention: time.Hour}
	r, _, err := startReceiver(t.Context(), slog.New(slog.DiscardHandler), home, settings, nil)
	require.NoError(t, err)

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.close(expired)

	_, err = r.store.Decisions(t.Context(), fixtureSandbox, time.Time{})
	require.ErrorContains(t, err, "closed")
}

func TestAuditDailyRejectsSince(t *testing.T) {
	home := t.TempDir()
	writeAuditPin(t, home, "cccccccccccccccc")

	out, err := sandboxAudit(t, home, "cccccccccccccccc", "--daily", "--since", "1h")

	require.Error(t, err, out)
	require.Contains(t, out, "--since does not apply to --daily; use --days")
}

// ocsfWithoutDevice is a decision the receiver cannot attribute to a sandbox.
func ocsfWithoutDevice(t *testing.T) *logspb.LogRecord {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "sandbox", "openshell", "testdata", "ocsf", "net-denied-curl.json"))
	require.NoError(t, err)
	var ev map[string]any
	require.NoError(t, json.Unmarshal(body, &ev))
	delete(ev, "device")
	body, err = json.Marshal(ev)
	require.NoError(t, err)
	return &logspb.LogRecord{
		ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(body)}},
		Attributes:           []*commonpb.KeyValue{strAttr("ocsf.metadata.uid", "unattributed-1"), intAttr("ocsf.class_uid", 4001)},
	}
}

func TestAuditReportsDropsAndCoverage(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, telemetryConfig)
	r := startIn(t, home)
	addr := readOTLPAddr(t, home)
	token := readHeadersToken(t, home)
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, fixtureRecords(t)...))
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, gapRecord()))
	require.Equal(t, http.StatusOK, exportLogsFrom(t, addr, token, "", gapRecord()))
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, ocsfWithoutDevice(t)))
	require.Equal(t, http.StatusOK, exportLogsFrom(t, addr, token, "never-seen", gapRecord()))
	const session = "dddddddddddddddd"
	writeAuditPin(t, home, session)

	out, err := sandboxAudit(t, home, session, "--since", auditSince)
	require.NoError(t, err, out)
	require.Contains(t, out, "warning: 3 events from this sandbox were dropped before export since ")
	require.Contains(t, out, "warning: 3 events were dropped gateway-wide since ")
	require.Contains(t, out, "; some may belong to this sandbox")
	require.Contains(t, out, "warning: 3 events were dropped for sandboxes with no stored decisions since ")
	require.Contains(t, out, "warning: 1 network decisions on or after ")
	require.Contains(t, out, " (UTC) had no sandbox or event id, so they are not attributed to any sandbox")
	require.Contains(t, out, "note: telemetry history for this sandbox starts at ")
	require.Contains(t, out, "; earlier decisions were not recorded")
	require.Contains(t, out, "note: raw events older than 720h0m0s are pruned; use --daily for older totals")

	out, err = sandboxAudit(t, home, session, "--daily", "--days", "3650")
	require.NoError(t, err, out)
	require.Contains(t, out, "note: telemetry history for this sandbox starts on ")
	require.Contains(t, out, "warning: 3 events from this sandbox were dropped before export since ")
	require.Contains(t, out, "warning: 3 events were dropped gateway-wide since ")
	require.Contains(t, out, "warning: 1 network decisions on or after ")
	stopServer(t, r)
}

func TestAuditWithStoreSurvivesGatewayError(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, telemetryConfig)
	r := startIn(t, home)
	addr := readOTLPAddr(t, home)
	token := readHeadersToken(t, home)
	require.Equal(t, http.StatusOK, exportLogs(t, addr, token, fixtureRecords(t)...))
	require.Equal(t, http.StatusOK, exportLogsFrom(t, addr, token, "never-seen", gapRecord()))
	const session = "1111111111111111"
	writeAuditPinNamed(t, home, session, "dt-without-decisions")

	stdout, stderr, err := sandboxAuditSplit(t, home, session, "--since", auditSince)

	require.NoError(t, err, stderr)
	require.Contains(t, stdout, "No network decisions in the telemetry store for that period; the gateway log could not be read: ")
	require.Contains(t, stdout, "warning: 3 events were dropped for sandboxes with no stored decisions since ")
	stopServer(t, r)
}

func TestAuditWithoutStoreReportsGatewayError(t *testing.T) {
	home := t.TempDir()
	writeAuditPin(t, home, "2222222222222222")

	out, err := sandboxAudit(t, home, "2222222222222222")

	require.Error(t, err, out)
	require.Contains(t, out, "failed to connect to the gateway")
}

func TestAuditRejectsBadFlags(t *testing.T) {
	home := t.TempDir()
	const session = "3333333333333333"
	writeAuditPin(t, home, session)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"days without daily", []string{"--days", "7"}, "--days applies only to --daily"},
		{"denied with daily", []string{"--daily", "--denied"}, "--denied does not apply to --daily"},
		{"negative days", []string{"--daily", "--days=-1"}, "--days must be at least 1"},
		{"negative since", []string{"--since=-1h"}, "--since must be positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := sandboxAudit(t, home, session, tc.args...)
			require.Error(t, err, out)
			require.Contains(t, out, tc.want)
		})
	}
}
