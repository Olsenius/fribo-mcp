package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func newLogger(w io.Writer) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error")
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), nil
}

// Export is opt-in: without an endpoint the API remains a no-op. The HTTP
// exporter reads standard OTEL endpoint, TLS, header and timeout settings.
func initTracing(ctx context.Context) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	disabled, err := strconv.ParseBool(env("OTEL_SDK_DISABLED", "false"))
	if err != nil {
		return nil, fmt.Errorf("OTEL_SDK_DISABLED must be a boolean")
	}
	if disabled || env("OTEL_TRACES_EXPORTER", "") == "none" ||
		(env("OTEL_EXPORTER_OTLP_ENDPOINT", "") == "" && env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "") == "") {
		return noop, nil
	}
	if exporter := env("OTEL_TRACES_EXPORTER", "otlp"); exporter != "otlp" {
		return nil, fmt.Errorf("OTEL_TRACES_EXPORTER must be otlp or none")
	}
	if protocol := env("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", env("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")); protocol != "http/protobuf" {
		return nil, fmt.Errorf("OTLP tracing supports http/protobuf; use an HTTP endpoint (normally port 4318)")
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, fmt.Errorf("trace resource: %w", err)
	}
	// Environment resource attributes override defaults (including service.name).
	res, err = resource.Merge(resource.NewSchemaless(
		attribute.String("service.name", "fribo-mcp"), attribute.String("service.version", version)), res)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Trace only explicitly selected metadata: query strings, headers and
// request/response contents must never be added to spans.
func traceHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.URL.Query().Get("check") == "" {
			next.ServeHTTP(w, r)
			return
		}
		parent := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := otel.Tracer("fribo-mcp").Start(parent, r.Method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		defer span.End()
		sw := &statusWriter{ResponseWriter: w}
		req := r.WithContext(ctx)
		next.ServeHTTP(sw, req)
		setRouteSpan(req)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		span.SetAttributes(attribute.Int("http.response.status_code", sw.code))
		if sw.code >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.code))
		}
	})
}

func setRouteSpan(r *http.Request) {
	if r.Pattern == "" {
		return
	}
	span := trace.SpanFromContext(r.Context())
	route := r.Pattern
	if _, path, ok := strings.Cut(route, " "); ok {
		route = path
	}
	span.SetName(r.Method + " " + route)
	span.SetAttributes(attribute.String("http.route", route))
}

// This wrapper preserves streaming and lets the SDK use ResponseController.
// No response content is inspected or captured.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code != 0 {
		return
	}
	s.ResponseWriter.WriteHeader(code)
	if code >= 200 {
		s.code = code
	}
}
func (s *statusWriter) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(p)
}
func (s *statusWriter) Flush() {
	if s.code == 0 {
		s.WriteHeader(http.StatusOK)
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// SDK middleware sees decoded RPC methods and tool names, so arguments and
// results never need to be parsed or buffered by the tracing layer.
func traceMCP(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		name := method
		attrs := []attribute.KeyValue{attribute.String("rpc.system.name", "jsonrpc"), attribute.String("rpc.method", method)}
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
			name += " " + call.Params.Name
			attrs = append(attrs, attribute.String("mcp.tool.name", call.Params.Name))
		}
		ctx, span := otel.Tracer("fribo-mcp").Start(ctx, name, trace.WithAttributes(attrs...))
		defer span.End()
		result, err := next(ctx, method, req)
		if err != nil {
			span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
			span.SetStatus(codes.Error, "MCP request failed")
		} else if tool, ok := result.(*mcp.CallToolResult); ok && tool != nil && tool.IsError {
			span.SetStatus(codes.Error, "MCP tool returned an error")
		}
		return result, err
	}
}
