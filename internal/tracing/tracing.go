// Package tracing sets up the OpenTelemetry trace exporter for durable-server.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	ProtocolHTTP = "http/protobuf"
	ProtocolGRPC = "grpc"

	serviceName = "durable-server"
)

// Config says where to export traces; an empty Endpoint turns tracing off.
type Config struct {
	Endpoint       string
	Protocol       string
	ServiceVersion string
	// Headers holds resolved values, so it must never be logged.
	Headers      map[string]string
	ErrorHandler func(error)
}

// Setup returns a tracer provider and the shutdown that flushes it.
func Setup(ctx context.Context, cfg Config) (trace.TracerProvider, func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	exp, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithAttributes(semconv.ServiceName(serviceName), semconv.ServiceVersion(cfg.ServiceVersion)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build the trace resource: %w", err)
	}
	res, err = resource.Merge(resource.Default(), res)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to merge the trace resource: %w", err)
	}
	if cfg.ErrorHandler != nil {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(cfg.ErrorHandler))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	return tp, tp.Shutdown, nil
}

func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Protocol {
	case "", ProtocolHTTP:
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(cfg.Endpoint)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create the OTLP http exporter: %w", err)
		}
		return exp, nil
	case ProtocolGRPC:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(cfg.Endpoint)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create the OTLP grpc exporter: %w", err)
		}
		return exp, nil
	}
	return nil, fmt.Errorf("unsupported OTLP protocol %q", cfg.Protocol)
}
