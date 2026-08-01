// Package metrics provides a minimal, dependency-free Prometheus counter store
// used to seed observability (Week 5 stretch). It renders a small subset of the
// Prometheus text exposition format so that GET /metrics on a worker surfaces
// non-zero counters after a burst run.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Label is a single Prometheus label pair rendered as name="value".
type Label struct {
	Name  string
	Value string
}

// Metrics is a thread-safe store of named counters with sorted labels. Every
// method is nil-safe: a nil *Metrics is a silent no-op, so the store can be
// optional without scattering nil checks through callers.
type Metrics struct {
	mu   sync.Mutex
	rows map[string]*row
}

type row struct {
	name   string
	labels []Label
	value  float64
}

// New returns an empty counter store.
func New() *Metrics {
	return &Metrics{rows: make(map[string]*row)}
}

// Inc increments a counter by 1.
func (m *Metrics) Inc(name string, labels []Label) {
	m.Add(name, labels, 1)
}

// Add increments a counter by delta.
func (m *Metrics) Add(name string, labels []Label, delta float64) {
	if m == nil || delta == 0 {
		return
	}
	ls := sortedLabels(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	key := makeKey(name, ls)
	r, ok := m.rows[key]
	if !ok {
		r = &row{name: name, labels: ls}
		m.rows[key] = r
	}
	r.value += delta
}

// Value returns the current value of a counter (0 if never incremented).
func (m *Metrics) Value(name string, labels []Label) float64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[makeKey(name, sortedLabels(labels))]; ok {
		return r.value
	}
	return 0
}

// Render serializes the store in Prometheus text exposition format, one # TYPE
// line per metric name followed by its sample lines (name{labels} value).
func (m *Metrics) Render() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.rows))
	for k := range m.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	seenType := make(map[string]bool)
	for _, k := range keys {
		r := m.rows[k]
		if !seenType[r.name] {
			seenType[r.name] = true
			b.WriteString("# TYPE ")
			b.WriteString(r.name)
			b.WriteString(" counter\n")
		}
		fmt.Fprintf(&b, "%s %g\n", r.nameWithLabels(), r.value)
	}
	return b.String()
}

// ServeHTTP renders the store for a Prometheus scrape.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if m == nil {
		http.Error(w, "metrics disabled", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	io.WriteString(w, m.Render())
}

func (r *row) nameWithLabels() string {
	if len(r.labels) == 0 {
		return r.name
	}
	var b strings.Builder
	b.WriteString(r.name)
	b.WriteByte('{')
	for i, l := range r.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString("=\"")
		b.WriteString(l.Value)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func sortedLabels(labels []Label) []Label {
	ls := append([]Label(nil), labels...)
	sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	return ls
}

// makeKey produces a stable lookup key from a name and its sorted labels.
func makeKey(name string, labels []Label) string {
	var b strings.Builder
	b.WriteString(name)
	for _, l := range labels {
		b.WriteByte(0x00)
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(l.Value)
	}
	return b.String()
}
