package main

import (
	"bytes"
	"encoding/json"
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

	"github.com/wolfeidau/durable/internal/sandbox"
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
	body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("openshell.sandbox.id", fixtureSandboxID)}},
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
	dir := filepath.Join(home, ".durable", "sessions", id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	data, err := json.Marshal(sandbox.Pin{Name: fixtureSandbox, Image: "worker:test", Token: "unused"})
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
	require.Contains(t, out, "warning: 3 events dropped since ")

	out, err = sandboxAudit(t, home, session, "--since", auditSince, "--denied")
	require.NoError(t, err, out)
	require.NotContains(t, out, "allowed  ")
	require.Contains(t, out, "mise-versions.jdx.dev")

	out, err = sandboxAudit(t, home, session, "--since", auditSince, "--daily", "--days", "3650")
	require.NoError(t, err, out)
	require.Contains(t, out, "DAY")
	require.Contains(t, out, "TOP DENIED")
	require.Contains(t, out, "mise-versions.jdx.dev (1)")
	require.Contains(t, out, "github.com:443 (1)")
	var rows int
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) > 3 && regexp.MustCompile(`^\d{4}-\d\d-\d\d$`).MatchString(f[0]) {
			rows++
			require.Equal(t, "2", f[1], line)
			require.Equal(t, "2", f[2], line)
		}
	}
	require.Equal(t, 1, rows, out)
	stopServer(t, r)
}

func TestAuditDailyNeedsTelemetryStore(t *testing.T) {
	home := t.TempDir()
	writeAuditPin(t, home, "bbbbbbbbbbbbbbbb")

	out, err := sandboxAudit(t, home, "bbbbbbbbbbbbbbbb", "--daily")
	require.Error(t, err, out)
	require.Contains(t, out, "no telemetry store; enable telemetry.otlp_addr in config.json")
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
