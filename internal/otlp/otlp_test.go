package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const testToken = "secret"

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func request(resAttrs []*commonpb.KeyValue, recs ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: resAttrs},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}},
	}}}
}

func event(sandbox, uid string) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{num("ocsf.class_uid", 4001), str("ocsf.metadata.uid", uid)}
	if sandbox != "" {
		attrs = append(attrs, str("openshell.sandbox.id", sandbox))
	}
	return &logspb.LogRecord{
		TimeUnixNano: uint64(time.UnixMilli(1700000000123).UnixNano()),
		SeverityText: "INFO",
		Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: `{"class_uid":4001}`}},
		Attributes:   attrs,
	}
}

type harness struct {
	h    http.Handler
	got  [][]Record
	fail error
}

func newHarness() *harness {
	s := &harness{}
	s.h = Handler(testToken, func(_ context.Context, recs []Record) error {
		s.got = append(s.got, recs)
		return s.fail
	})
	return s
}

func (s *harness) post(t *testing.T, mutate func(*http.Request), body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/x-protobuf")
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	return b
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(b)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	s := newHarness()
	w := s.post(t, nil, marshal(t, request(nil, event("sb-1", "u1"), event("sb-1", "u2"))))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/x-protobuf", w.Header().Get("Content-Type"))
	var resp collogspb.ExportLogsServiceResponse
	require.NoError(t, proto.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, s.got, 1)
	require.Len(t, s.got[0], 2)
	rec := s.got[0][0]
	require.Equal(t, "sb-1", rec.SandboxID)
	require.Equal(t, "u1", rec.UID)
	require.Equal(t, 4001, rec.ClassUID)
	require.Equal(t, "INFO", rec.Severity)
	require.True(t, time.UnixMilli(1700000000123).Equal(rec.Time))
	require.JSONEq(t, `{"class_uid":4001}`, string(rec.Body))
	require.Nil(t, rec.Gap)
}

func TestGzip(t *testing.T) {
	s := newHarness()
	w := s.post(t, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, gz(t, marshal(t, request(nil, event("sb-1", "u1")))))
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, s.got[0], 1)
}

func TestIdentityEncoding(t *testing.T) {
	s := newHarness()
	w := s.post(t, func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") }, marshal(t, request(nil, event("sb-1", "u1"))))
	require.Equal(t, http.StatusOK, w.Code)
}

func TestAuth(t *testing.T) {
	for name, header := range map[string]string{"missing": "", "wrong": "Bearer nope", "no scheme": testToken} {
		s := newHarness()
		w := s.post(t, func(r *http.Request) {
			if header == "" {
				r.Header.Del("Authorization")
				return
			}
			r.Header.Set("Authorization", header)
		}, marshal(t, request(nil, event("sb-1", "u1"))))
		require.Equal(t, http.StatusUnauthorized, w.Code, name)
		require.Empty(t, s.got, name)
	}
}

func TestJSONContentTypeIs415(t *testing.T) {
	s := newHarness()
	w := s.post(t, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, []byte(`{}`))
	require.Equal(t, http.StatusUnsupportedMediaType, w.Code)
}

func TestUnsupportedEncodingIs415(t *testing.T) {
	s := newHarness()
	w := s.post(t, func(r *http.Request) { r.Header.Set("Content-Encoding", "zstd") }, marshal(t, request(nil, event("sb-1", "u1"))))
	require.Equal(t, http.StatusUnsupportedMediaType, w.Code)
}

func TestOversizedBodyIs413(t *testing.T) {
	s := newHarness()
	big := bytes.Repeat([]byte{0}, maxBody+1)
	require.Equal(t, http.StatusRequestEntityTooLarge, s.post(t, nil, big).Code)
	// A small gzip body that expands past the cap is refused too.
	require.Equal(t, http.StatusRequestEntityTooLarge, s.post(t, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, gz(t, big)).Code)
	require.Empty(t, s.got)
}

func TestMalformedIs400(t *testing.T) {
	s := newHarness()
	require.Equal(t, http.StatusBadRequest, s.post(t, nil, []byte{0xff, 0xff, 0xff}).Code)
	require.Equal(t, http.StatusBadRequest, s.post(t, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, []byte("not gzip")).Code)
}

func TestEmptyRequestSkipsSink(t *testing.T) {
	s := newHarness()
	require.Equal(t, http.StatusOK, s.post(t, nil, nil).Code)
	require.Equal(t, http.StatusOK, s.post(t, nil, marshal(t, request(nil))).Code)
	require.Empty(t, s.got)
}

func TestSinkErrorIs503(t *testing.T) {
	s := newHarness()
	s.fail = errors.New("boom")
	require.Equal(t, http.StatusServiceUnavailable, s.post(t, nil, marshal(t, request(nil, event("sb-1", "u1")))).Code)
}

func TestGapRecord(t *testing.T) {
	s := newHarness()
	gap := &logspb.LogRecord{
		EventName:  gapEvent,
		Attributes: []*commonpb.KeyValue{num("openshell.ocsf.gap.dropped", 7), num("openshell.ocsf.gap.since_unix_ms", 1700000000000)},
	}
	require.Equal(t, http.StatusOK, s.post(t, nil, marshal(t, request(nil, gap))).Code)
	require.Equal(t, &Gap{Dropped: 7, Since: time.UnixMilli(1700000000000).UTC()}, s.got[0][0].Gap)
}

func TestSandboxIDSources(t *testing.T) {
	s := newHarness()
	res := []*commonpb.KeyValue{str("openshell.sandbox.id", "sb-res")}
	require.Equal(t, http.StatusOK, s.post(t, nil, marshal(t, request(res, event("sb-rec", "u1"), event("", "u2")))).Code)
	require.Equal(t, "sb-rec", s.got[0][0].SandboxID)
	require.Equal(t, "sb-res", s.got[0][1].SandboxID)
}

func TestRoutingAndMethods(t *testing.T) {
	s := newHarness()
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader("")))
	require.Equal(t, http.StatusNotFound, w.Code)

	w = httptest.NewRecorder()
	s.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/logs", nil))
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
