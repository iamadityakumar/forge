package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics_IncrementAndValue(t *testing.T) {
	m := New()
	m.Inc("forge_llm_calls_total", []Label{{Name: "backend", Value: "groq"}})
	m.Inc("forge_llm_calls_total", []Label{{Name: "backend", Value: "groq"}})
	m.Add("forge_llm_tokens_total", []Label{{Name: "backend", Value: "groq"}, {Name: "kind", Value: "prompt"}}, 42)

	if got := m.Value("forge_llm_calls_total", []Label{{Name: "backend", Value: "groq"}}); got != 2 {
		t.Fatalf("calls total = %v, want 2", got)
	}
	if got := m.Value("forge_llm_tokens_total", []Label{{Name: "backend", Value: "groq"}, {Name: "kind", Value: "prompt"}}); got != 42 {
		t.Fatalf("tokens total = %v, want 42", got)
	}
	// Different label set stays isolated.
	if got := m.Value("forge_llm_calls_total", []Label{{Name: "backend", Value: "ollama"}}); got != 0 {
		t.Fatalf("ollama calls = %v, want 0", got)
	}
}

func TestMetrics_RenderFormat(t *testing.T) {
	m := New()
	m.Inc("forge_llm_calls_total", []Label{{Name: "backend", Value: "groq"}})
	m.Add("forge_rate_limit_wait_seconds", nil, 0.5)

	out := m.Render()
	for _, want := range []string{
		"# TYPE forge_llm_calls_total counter",
		`forge_llm_calls_total{backend="groq"} 1`,
		"# TYPE forge_rate_limit_wait_seconds counter",
		"forge_rate_limit_wait_seconds 0.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
}

func TestMetrics_LabelsSortedStable(t *testing.T) {
	m := New()
	// Pass labels in a different order; lookup and render must still be canonical.
	m.Add("forge_llm_tokens_total", []Label{{Name: "kind", Value: "prompt"}, {Name: "backend", Value: "groq"}}, 1)
	if got := m.Value("forge_llm_tokens_total", []Label{{Name: "backend", Value: "groq"}, {Name: "kind", Value: "prompt"}}); got != 1 {
		t.Fatalf("value with reordered labels = %v, want 1", got)
	}
	if want, out := `forge_llm_tokens_total{backend="groq",kind="prompt"} 1`, m.Render(); !strings.Contains(out, want) {
		t.Fatalf("render not canonical, want %q in:\n%s", want, out)
	}
}

func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics
	// None of these may panic.
	m.Inc("a", nil)
	m.Add("b", nil, 1)
	_ = m.Render()
	_ = m.Value("a", nil)

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil metrics status = %d, want 404", rec.Code)
	}
}

func TestMetrics_ServeHTTP(t *testing.T) {
	m := New()
	m.Inc("forge_llm_calls_total", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "forge_llm_calls_total 1") {
		t.Fatalf("body missing counter:\n%s", rec.Body.String())
	}
}
