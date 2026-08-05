package trace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// TraceContext represents W3C TraceContext metadata persisted across workers or sent over HTTP.
type TraceContext struct {
	TraceID    string `json:"trace_id"`
	SpanID     string `json:"span_id"`
	TraceFlags string `json:"trace_flags"`
}

// ToW3CHeader formats the context as a W3C traceparent header value.
// Format: 00-{trace_id}-{span_id}-{trace_flags}
func (tc *TraceContext) ToW3CHeader() string {
	flags := tc.TraceFlags
	if flags == "" {
		flags = "01"
	}
	return fmt.Sprintf("00-%s-%s-%s", tc.TraceID, tc.SpanID, flags)
}

// ParseW3CHeader parses a W3C traceparent header string.
func ParseW3CHeader(h string) (*TraceContext, error) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 || parts[0] != "00" {
		return nil, fmt.Errorf("invalid traceparent format %q", h)
	}
	return &TraceContext{
		TraceID:    parts[1],
		SpanID:     parts[2],
		TraceFlags: parts[3],
	}, nil
}

// Attribute is a key-value pair attached to a Span.
type Attribute struct {
	Key   string
	Value any
}

// Span is a thin wrapper around an OpenTelemetry span. It keeps the exact
// public surface callers have always used; TraceID/SpanID/ParentSpanID are
// copied from the real OTel SpanContext at creation so they are readable
// without reaching into the SDK. otel is nil for synthetic parent spans (see
// ExtractContext), in which case the write methods are no-ops.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	otel         oteltrace.Span
}

// SetAttribute sets a key-value attribute on the span.
func (s *Span) SetAttribute(key string, val any) {
	if s == nil || s.otel == nil {
		return
	}
	s.otel.SetAttributes(toKeyValue(key, val))
}

// SetStatus sets the span status. "ok" (or "") maps to codes.Ok; anything else
// maps to codes.Error while preserving the caller's exact status string
// ("error", "canceled", ...) via a status_str attribute so the slog output
// keeps today's wording.
func (s *Span) SetStatus(status string, err error) {
	if s == nil || s.otel == nil {
		return
	}
	if status == "" || status == "ok" {
		s.otel.SetStatus(codes.Ok, "")
		return
	}
	s.otel.SetAttributes(attribute.String("status_str", status))
	desc := ""
	if err != nil {
		desc = err.Error()
	}
	s.otel.SetStatus(codes.Error, desc)
}

// End finishes the span. With a real provider (Setup), the SDK span processor
// emits the span line immediately; on the no-op provider End is a no-op.
func (s *Span) End() {
	if s == nil || s.otel == nil {
		return
	}
	s.otel.End()
}

// Tracer creates and propagates spans.
type Tracer struct {
	service string
	t       oteltrace.Tracer
}

// NewTracer constructs a Tracer tagged with the given service name.
func NewTracer(service string) *Tracer {
	return &Tracer{
		service: service,
		t:       otel.Tracer("forge/" + service),
	}
}

// StartSpan creates a new span as a child of the span in ctx, or a new root
// span. The returned context carries the new span so children chain onto it.
func (t *Tracer) StartSpan(ctx context.Context, name string, attrs ...Attribute) (context.Context, *Span) {
	parentSC := oteltrace.SpanContextFromContext(ctx)
	cctx, os := t.t.Start(ctx, name, oteltrace.WithAttributes(attrsToKVs(attrs)...))

	span := &Span{
		ParentSpanID: spanIDOrEmpty(parentSC),
		otel:         os,
	}
	if sc := os.SpanContext(); sc.IsValid() {
		span.TraceID = sc.TraceID().String()
		span.SpanID = sc.SpanID().String()
	}
	return cctx, span
}

// ExtractContext parses a persisted JSON trace context (from jobs.trace_context)
// and injects it as the *remote parent* of the returned context, so a
// subsequent StartSpan continues the same trace. The returned *Span is a
// synthetic parent (nil OTel span) carrying the persisted TraceID/SpanID for
// callers that want to read them; propagation happens through the context.
func (t *Tracer) ExtractContext(ctx context.Context, raw json.RawMessage) (context.Context, *Span) {
	if len(raw) == 0 || string(raw) == "null" {
		return ctx, nil
	}
	var tc TraceContext
	if err := json.Unmarshal(raw, &tc); err != nil || tc.TraceID == "" || tc.SpanID == "" {
		return ctx, nil
	}
	if sc, ok := spanContextFromJSON(&tc); ok {
		ctx = oteltrace.ContextWithRemoteSpanContext(ctx, sc)
	}
	return ctx, &Span{TraceID: tc.TraceID, SpanID: tc.SpanID}
}

// MarshalContext serializes a span's TraceID and SpanID into JSONB for
// jobs.trace_context. Returns nil if the span has no valid recording context.
func MarshalContext(s *Span) json.RawMessage {
	if s == nil || s.otel == nil {
		return nil
	}
	sc := s.otel.SpanContext()
	if !sc.IsValid() {
		return nil
	}
	tc := TraceContext{
		TraceID:    sc.TraceID().String(),
		SpanID:     sc.SpanID().String(),
		TraceFlags: "01",
	}
	raw, _ := json.Marshal(tc)
	return json.RawMessage(raw)
}

// InjectW3C injects the W3C traceparent header into an HTTP request from the
// span (or remote parent) carried by ctx.
func InjectW3C(ctx context.Context, req *http.Request) {
	if req == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// Provider manages the lifecycle of tracing for a service.
type Provider struct {
	tp *sdktrace.TracerProvider
}

// Shutdown flushes and closes tracing resources.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// Setup initializes tracing for the specified service. It always wires a
// slogExporter (spans as structured slog lines — zero infrastructure, grep
// trace_id reconstructs a job's journey) and, when OTEL_EXPORTER_OTLP_ENDPOINT
// is set, adds an OTLP HTTP exporter so spans also reach a collector (e.g. the
// compose Jaeger). OTLP export is best-effort: an unreachable endpoint must not
// take the process down — the batch processor retries in the background while
// the slog spans keep landing.
func Setup(service string) (*Provider, error) {
	return newProvider(service, slog.Default(), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

// newProvider builds a TracerProvider with the given slog logger and optional
// OTLP endpoint. Unexported so tests can construct providers with a captured
// logger without touching the process-global exporter.
func newProvider(service string, logger *slog.Logger, otlpEndpoint string) (*Provider, error) {
	res := resource.NewSchemaless(attribute.String("service.name", service))

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		// Emit each ended span immediately as one structured slog line.
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(newSlogExporter(logger))),
	}

	if ep := strings.TrimSpace(otlpEndpoint); ep != "" {
		otlpOpts := []otlptracehttp.Option{otlptracehttp.WithTimeout(5 * time.Second)}
		if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
			otlpOpts = append(otlpOpts, otlptracehttp.WithEndpointURL(ep))
		} else {
			otlpOpts = append(otlpOpts, otlptracehttp.WithEndpoint(ep))
		}
		otlpExp, err := otlptracehttp.New(context.Background(), otlpOpts...)
		if err != nil {
			return nil, fmt.Errorf("trace: create OTLP exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(otlpExp))
	}

	tp := sdktrace.NewTracerProvider(opts...)

	// Install the global provider and W3C tracecontext propagator so the thin
	// wrapper's otel.Tracer / otel.GetTextMapPropagator calls pick them up.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Note: no explicit "service" attr here — the process logger (set up by
	// internal/log.Setup before trace.Setup) already tags every line with it.
	slog.Info("trace provider initialized", "otlp_enabled", otlpEndpoint != "")
	return &Provider{tp: tp}, nil
}

// slogExporter writes every finished span as one structured slog line, giving
// full trace visibility (IDs, parent, status, duration, attributes) with zero
// infrastructure. It also carries the two exporters' shared format so a
// reclaimer's `grep trace_id` output reconstructs a cross-worker journey.
type slogExporter struct {
	logger *slog.Logger
}

func newSlogExporter(logger *slog.Logger) sdktrace.SpanExporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &slogExporter{logger: logger}
}

// ExportSpans emits each span as msg="span" with the documented keys:
// trace_id, span_id, parent_span_id, name, service, duration_ms, status,
// optional error, and the span's own attributes.
func (e *slogExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		// "service" is intentionally absent here: the process logger (set up by
		// internal/log.Setup before trace.Setup) already tags every line — spans
		// included — with its service name, so adding it again would duplicate
		// the key in JSON output. The resource service.name is used by OTLP/Jaeger.
		args := []any{
			"trace_id", s.SpanContext().TraceID().String(),
			"span_id", s.SpanContext().SpanID().String(),
			"parent_span_id", spanIDOrEmpty(s.Parent()),
			"name", s.Name(),
			"duration_ms", s.EndTime().Sub(s.StartTime()).Milliseconds(),
		}

		status := "ok"
		if s.Status().Code == codes.Error {
			status = "error"
		}
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "status_str" {
				status = kv.Value.AsString()
				continue
			}
			args = append(args, string(kv.Key), kv.Value.AsInterface())
		}
		args = append(args, "status", status)

		if desc := s.Status().Description; desc != "" {
			args = append(args, "error", desc)
		}

		e.logger.Info("span", args...)
	}
	return nil
}

// Shutdown is a no-op: there is nothing to flush for a synchronous writer.
func (e *slogExporter) Shutdown(context.Context) error { return nil }

func spanIDOrEmpty(sc oteltrace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}

// spanContextFromJSON parses a persisted TraceContext into an OTel SpanContext,
// marking it remote + sampled so a reclaimer's parent-based sampler keeps the trace.
func spanContextFromJSON(tc *TraceContext) (oteltrace.SpanContext, bool) {
	tid, err := hex.DecodeString(tc.TraceID)
	if err != nil || len(tid) != 16 {
		return oteltrace.SpanContext{}, false
	}
	sid, err := hex.DecodeString(tc.SpanID)
	if err != nil || len(sid) != 8 {
		return oteltrace.SpanContext{}, false
	}
	var t oteltrace.TraceID
	copy(t[:], tid)
	var s oteltrace.SpanID
	copy(s[:], sid)
	return oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    t,
		SpanID:     s,
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	}), true
}

func attrsToKVs(attrs []Attribute) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		kvs = append(kvs, toKeyValue(a.Key, a.Value))
	}
	return kvs
}

func toKeyValue(key string, val any) attribute.KeyValue {
	switch v := val.(type) {
	case string:
		return attribute.String(key, v)
	case int:
		return attribute.Int(key, v)
	case int64:
		return attribute.Int64(key, v)
	case float64:
		return attribute.Float64(key, v)
	case bool:
		return attribute.Bool(key, v)
	case []string:
		return attribute.StringSlice(key, v)
	default:
		return attribute.String(key, fmt.Sprint(v))
	}
}
