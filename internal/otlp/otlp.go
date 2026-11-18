package otlp

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

const (
	maxBody  = 4 << 20
	gapEvent = "openshell.ocsf.gap"
)

// Record is one OCSF event received as an OTLP log record.
type Record struct {
	SandboxID string
	UID       string // empty for gap records, so dedupe must not key gap records on it
	ClassUID  int
	Time      time.Time // zero when the exporter sent none
	Observed  time.Time // zero when the exporter sent none
	Severity  string
	Body      json.RawMessage // the OCSF event; nil for gap records and non-JSON bodies
	Gap       *Gap
}

// Gap says the exporter dropped records, so the stream is incomplete.
type Gap struct {
	Dropped int64
	Since   time.Time
}

// Handler serves OTLP/HTTP protobuf log export at POST /v1/logs and passes each request's records to sink.
func Handler(token string, sink func(context.Context, []Record) error) (http.Handler, error) {
	if token == "" {
		return nil, errors.New("otlp token must not be empty")
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-protobuf" {
			http.Error(w, "content type must be application/x-protobuf", http.StatusUnsupportedMediaType)
			return
		}
		body, status, err := readBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		if recs := records(&req); len(recs) > 0 {
			if err := sink(r.Context(), recs); err != nil {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		out, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}), nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, int, error) {
	var src io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, http.StatusBadRequest, errors.New("malformed gzip body")
		}
		defer zr.Close()
		// The cap applies after decompression, so a small gzip bomb can't expand past it.
		src = http.MaxBytesReader(w, zr, maxBody)
	default:
		return nil, http.StatusUnsupportedMediaType, errors.New("unsupported content encoding")
	}
	b, err := io.ReadAll(src)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, http.StatusRequestEntityTooLarge, errors.New("request too large")
		}
		return nil, http.StatusBadRequest, errors.New("failed to read body")
	}
	return b, 0, nil
}

func records(req *collogspb.ExportLogsServiceRequest) []Record {
	var out []Record
	for _, rl := range req.GetResourceLogs() {
		resSandbox := attr(rl.GetResource().GetAttributes(), "openshell.sandbox.id")
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := lr.GetAttributes()
				rec := Record{
					SandboxID: attr(attrs, "openshell.sandbox.id"),
					UID:       attr(attrs, "ocsf.metadata.uid"),
					Time:      nanoTime(lr.GetTimeUnixNano()),
					Observed:  nanoTime(lr.GetObservedTimeUnixNano()),
					Severity:  lr.GetSeverityText(),
				}
				if body := lr.GetBody().GetStringValue(); lr.GetEventName() != gapEvent && json.Valid([]byte(body)) {
					rec.Body = json.RawMessage(body)
				}
				if rec.SandboxID == "" {
					rec.SandboxID = resSandbox
				}
				rec.ClassUID, _ = strconv.Atoi(attr(attrs, "ocsf.class_uid"))
				if lr.GetEventName() == gapEvent {
					dropped, _ := strconv.ParseInt(attr(attrs, "openshell.ocsf.gap.dropped"), 10, 64)
					since, _ := strconv.ParseInt(attr(attrs, "openshell.ocsf.gap.since_unix_ms"), 10, 64)
					rec.Gap = &Gap{Dropped: dropped, Since: time.UnixMilli(since).UTC()}
				}
				out = append(out, rec)
			}
		}
	}
	return out
}

func nanoTime(ns uint64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns)).UTC() //nolint:gosec // nanosecond timestamps fit int64 until 2262
}

// attr renders a string or integer attribute as a string, because exporters differ on which they use for numbers.
func attr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() != key {
			continue
		}
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			return v.StringValue
		case *commonpb.AnyValue_IntValue:
			return strconv.FormatInt(v.IntValue, 10)
		}
	}
	return ""
}
