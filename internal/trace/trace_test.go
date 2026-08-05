package trace

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// newTestProvider installs a real (non-noop) provider whose span lines are
// written into a captured buffer, and returns the buffer. The provider is
// registered as the global OTel provider (same path as Setup) so NewTracer's
// otel.Tracer lookup picks it up; it is shut down on test cleanup.
func newTestProvider(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	// The logger is tagged with the service name just like internal/log.Setup
	// tags process loggers; the slogExporter relies on that tag for "service".
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "test-service")
	p, err := newProvider("test-service", logger, "")
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return buf
}

func TestTracer_StartSpan_ParentChildChain(t *testing.T) {
	newTestProvider(t)
	tracer := NewTracer("test-service")

	ctx, root := tracer.StartSpan(context.Background(), "job.run", Attribute{Key: "job_id", Value: "123"})
	if root == nil || root.TraceID == "" || root.SpanID == "" {
		t.Fatalf("expected valid root span")
	}
	if root.ParentSpanID != "" {
		t.Errorf("expected empty parent for root span, got %q", root.ParentSpanID)
	}

	_, child := tracer.StartSpan(ctx, "step.plan", Attribute{Key: "step_number", Value: 1})
	if child == nil {
		t.Fatalf("expected valid child span")
	}
	if child.TraceID != root.TraceID {
		t.Errorf("child TraceID %q != root TraceID %q", child.TraceID, root.TraceID)
	}
	if child.ParentSpanID != root.SpanID {
		t.Errorf("child ParentSpanID %q != root SpanID %q", child.ParentSpanID, root.SpanID)
	}

	root.End()
	child.End()
}

func TestW3CHeader_FormatAndParse(t *testing.T) {
	tc := &TraceContext{
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:     "00f067aa0ba902b7",
		TraceFlags: "01",
	}

	h := tc.ToW3CHeader()
	if h != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("unexpected header: %s", h)
	}

	parsed, err := ParseW3CHeader(h)
	if err != nil {
		t.Fatalf("ParseW3CHeader failed: %v", err)
	}
	if parsed.TraceID != tc.TraceID || parsed.SpanID != tc.SpanID {
		t.Errorf("parsed context mismatched: %+v", parsed)
	}
}

func TestInjectW3C(t *testing.T) {
	newTestProvider(t)
	tracer := NewTracer("worker-1")
	ctx, _ := tracer.StartSpan(context.Background(), "llm.complete")

	req, _ := http.NewRequest(http.MethodPost, "http://localhost", nil)
	InjectW3C(ctx, req)

	tp := req.Header.Get("traceparent")
	if tp == "" || !strings.HasPrefix(tp, "00-") {
		t.Fatalf("expected valid traceparent header, got %q", tp)
	}
}

func TestMarshalAndExtractContext(t *testing.T) {
	newTestProvider(t)
	tracer1 := NewTracer("worker-1")
	_, span1 := tracer1.StartSpan(context.Background(), "job.run")

	raw := MarshalContext(span1)
	if len(raw) == 0 {
		t.Fatal("expected non-empty json")
	}

	tracer2 := NewTracer("worker-2")
	ctx2, parentSpan := tracer2.ExtractContext(context.Background(), raw)
	if parentSpan == nil {
		t.Fatal("expected extracted parent span")
	}
	if parentSpan.TraceID != span1.TraceID {
		t.Errorf("extracted TraceID %q != original %q", parentSpan.TraceID, span1.TraceID)
	}

	_, childSpan := tracer2.StartSpan(ctx2, "reclaim")
	if childSpan.TraceID != span1.TraceID {
		t.Errorf("child span under extracted parent TraceID %q != %q", childSpan.TraceID, span1.TraceID)
	}
	if childSpan.ParentSpanID != span1.SpanID {
		t.Errorf("child span ParentSpanID %q != original span id %q", childSpan.ParentSpanID, span1.SpanID)
	}
}

func TestSetup_InitLine_NoDuplicateService(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	buf := &bytes.Buffer{}
	// Mirror the real program: internal/log.Setup runs first and sets the
	// default logger tagged with "service". trace.Setup then uses slog.Info
	// (the default logger) for its init line, so the tag comes from that.
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "orch"))
	p, err := newProvider("orch", slog.Default(), "")
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// The only line written so far is the "trace provider initialized" info line.
	if n := strings.Count(buf.String(), "service=orch"); n != 1 {
		t.Errorf("expected exactly one service attr in init line, got %d:\n%s", n, buf.String())
	}
}

func TestSlogExporter_EmitsSpanLine(t *testing.T) {
	buf := newTestProvider(t)
	tracer := NewTracer("worker-1")

	ctx, root := tracer.StartSpan(context.Background(), "job.run",
		Attribute{Key: "job_id", Value: "abc123"},
	)
	root.SetStatus("ok", nil)
	root.End()

	_, child := tracer.StartSpan(ctx, "step.plan",
		Attribute{Key: "step_number", Value: 2},
		Attribute{Key: "worker_id", Value: "worker-2"},
	)
	child.SetStatus("error", context.DeadlineExceeded)
	child.End()

	out := buf.String()
	for _, want := range []string{
		"msg=span",
		"name=job.run",
		"name=step.plan",
		"service=test-service",
		"trace_id=",
		"job_id=abc123",
		"status=ok",
		"status=error",
		"context deadline exceeded", // SetStatus error description
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected slog output to contain %q, got:\n%s", want, out)
		}
	}
}
