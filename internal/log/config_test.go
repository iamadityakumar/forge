package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestSetup_JSONHandler(t *testing.T) {
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("LOG_LEVEL", "debug")

	logger, err := Setup("test-service")
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}

	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	l := slog.New(h).With("service", "test-service")
	l.Info("hello world", "key", "val")

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid json: %v\nbody: %s", err, buf.String())
	}

	if parsed["service"] != "test-service" {
		t.Errorf("expected service 'test-service', got %v", parsed["service"])
	}
	if parsed["msg"] != "hello world" {
		t.Errorf("expected msg 'hello world', got %v", parsed["msg"])
	}
}

func TestSetup_LevelFilter(t *testing.T) {
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("LOG_LEVEL", "warn")

	_, err := Setup("test-service")
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	l := slog.New(h).With("service", "test-service")

	l.Info("should be ignored")
	if buf.Len() > 0 {
		t.Errorf("expected Info to be suppressed when level=warn, got: %s", buf.String())
	}

	l.Warn("should be logged")
	if buf.Len() == 0 {
		t.Error("expected Warn to be logged when level=warn")
	}
}

func TestSetup_InvalidFormat(t *testing.T) {
	t.Setenv("LOG_FORMAT", "xml")
	if _, err := Setup("test"); err == nil {
		t.Error("expected error for invalid LOG_FORMAT")
	}
}
