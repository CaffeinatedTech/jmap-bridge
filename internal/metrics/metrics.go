// Package metrics is a minimal, hand-rolled Prometheus text-format
// exposer for the FR-D.6 signals. It exists instead of
// client_golang to keep the bridge's dependency set small: it needs
// only counters and pull-based gauges, and the exposition format is
// small enough to render directly.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Label is one name/value pair on a series. Prometheus orders labels
// by name, so callers may pass them in any order.
type Label struct {
	Name  string
	Value string
}

// Sample is one gauge series: its labels and current value.
type Sample struct {
	Labels []Label
	Value  float64
}

// Counter is a single monotonically increasing series; it is safe for
// concurrent use.
type Counter struct {
	v atomic.Int64
}

// Inc adds one to the counter.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds delta to the counter. A negative delta is accepted at the
// type level to keep the API small; callers use it as a counter.
func (c *Counter) Add(delta int64) { c.v.Add(delta) }

// value formats the current count as the exposition's float64 value.
func (c *Counter) value() float64 { return float64(c.v.Load()) }

// CounterVec is a family of Counters sharing one name and one ordered
// set of label names. It is safe for concurrent use.
type CounterVec struct {
	name       string
	help       string
	labelNames []string

	mu     sync.Mutex
	series map[string]*counterSeries
}

// counterSeries is one realised label set and its counter.
type counterSeries struct {
	labels []Label
	c      *Counter
}

// With returns the counter for labelValues, creating it on first use.
// The number of values must match the label names the family was
// registered with; a mismatch is a programmer error and panics.
func (v *CounterVec) With(labelValues ...string) *Counter {
	if len(labelValues) != len(v.labelNames) {
		panic(fmt.Sprintf("metrics: %s: got %d label values, want %d",
			v.name, len(labelValues), len(v.labelNames)))
	}
	key := encodeValues(labelValues)
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.series[key]; ok {
		return s.c
	}
	labels := make([]Label, len(v.labelNames))
	for i, name := range v.labelNames {
		labels[i] = Label{Name: name, Value: labelValues[i]}
	}
	c := &Counter{}
	v.series[key] = &counterSeries{labels: labels, c: c}
	return c
}

// snapshot freezes the family's current series. The caller renders it
// outside the lock so a slow writer cannot block With.
func (v *CounterVec) snapshot() []renderSeries {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]renderSeries, 0, len(v.series))
	for _, s := range v.series {
		out = append(out, renderSeries{labels: s.labels, value: s.c.value()})
	}
	return out
}

// encodeValues builds a collision-free cache key from label values:
// each value is length-prefixed so a separator byte inside a value
// cannot be mistaken for a boundary.
func encodeValues(values []string) string {
	var b strings.Builder
	for _, s := range values {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	return b.String()
}

// gaugeFunc is a pull-based gauge family: its samples are produced at
// scrape time, so the metric always reflects live state.
type gaugeFunc struct {
	name string
	help string
	f    func() []Sample
}

// Registry holds metric families and is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*CounterVec
	gauges   map[string]*gaugeFunc
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		counters: map[string]*CounterVec{},
		gauges:   map[string]*gaugeFunc{},
	}
}

// Counter registers a counter family. Registering the same name twice
// with a matching label set returns the existing family; a conflicting
// re-registration is a programmer error and panics.
func (r *Registry) Counter(name, help string, labelNames ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.counters[name]; ok {
		if len(existing.labelNames) != len(labelNames) {
			panic(fmt.Sprintf("metrics: %s already registered with a different label set", name))
		}
		return existing
	}
	if _, ok := r.gauges[name]; ok {
		panic(fmt.Sprintf("metrics: %s already registered as a gauge", name))
	}
	v := &CounterVec{
		name:       name,
		help:       help,
		labelNames: labelNames,
		series:     map[string]*counterSeries{},
	}
	r.counters[name] = v
	return v
}

// GaugeFunc registers a pull-based gauge family. f is called on every
// scrape; returning nil (or an empty slice) emits the family's HELP and
// TYPE with no series, which is how a metric with no state yet stays
// absent rather than lying with a zero.
func (r *Registry) GaugeFunc(name, help string, f func() []Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.gauges[name]; ok {
		panic(fmt.Sprintf("metrics: %s already registered as a gauge", name))
	}
	if _, ok := r.counters[name]; ok {
		panic(fmt.Sprintf("metrics: %s already registered as a counter", name))
	}
	r.gauges[name] = &gaugeFunc{name: name, help: help, f: f}
}

// renderSeries is the common shape a counter or gauge series renders
// from.
type renderSeries struct {
	labels []Label
	value  float64
}

// renderFamily is one metric family ready to render. Gauge samples are
// evaluated before rendering, so a gauge function can never hold the
// registry lock.
type renderFamily struct {
	name   string
	help   string
	typ    string
	series []renderSeries
}

// WriteTo renders the registry in Prometheus text format, version
// 0.0.4, to w. Families are ordered by name and each family's series by
// label tuple, so two scrapes of the same state are byte-identical.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	families := make([]renderFamily, 0, len(r.counters)+len(r.gauges))
	for _, v := range r.counters {
		families = append(families, renderFamily{
			name: v.name, help: v.help, typ: "counter", series: v.snapshot(),
		})
	}
	gauges := make([]*gaugeFunc, 0, len(r.gauges))
	for _, g := range r.gauges {
		gauges = append(gauges, g)
	}
	r.mu.Unlock()

	for _, g := range gauges {
		fam := renderFamily{name: g.name, help: g.help, typ: "gauge"}
		for _, s := range g.f() {
			fam.series = append(fam.series, renderSeries{labels: s.Labels, value: s.Value})
		}
		families = append(families, fam)
	}
	sort.Slice(families, func(i, j int) bool { return families[i].name < families[j].name })

	var buf bytes.Buffer
	for _, fam := range families {
		fmt.Fprintf(&buf, "# HELP %s %s\n", fam.name, escapeHelp(fam.help))
		fmt.Fprintf(&buf, "# TYPE %s %s\n", fam.name, fam.typ)
		for _, s := range fam.ordered() {
			writeSeries(&buf, fam.name, s.labels, s.value)
		}
	}
	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// ordered returns the family's series normalised to Prometheus label
// order and sorted by label tuple.
func (f renderFamily) ordered() []renderSeries {
	series := append([]renderSeries(nil), f.series...)
	for i := range series {
		series[i].labels = normalizeLabels(series[i].labels)
	}
	sort.SliceStable(series, func(i, j int) bool {
		return lessLabels(series[i].labels, series[j].labels)
	})
	return series
}

// normalizeLabels copies labels into Prometheus's canonical order.
func normalizeLabels(in []Label) []Label {
	out := append([]Label(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// lessLabels compares two normalised label sets lexicographically.
func lessLabels(a, b []Label) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].Name != b[i].Name {
			return a[i].Name < b[i].Name
		}
		if a[i].Value != b[i].Value {
			return a[i].Value < b[i].Value
		}
	}
	return len(a) < len(b)
}

// writeSeries renders one sample line. The format is "name value" for a
// series with no labels and "name{...} value" otherwise.
func writeSeries(w io.Writer, name string, labels []Label, value float64) {
	if len(labels) == 0 {
		_, _ = fmt.Fprintf(w, "%s %s\n", name, formatValue(value))
		return
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.Value))
		b.WriteByte('"')
	}
	b.WriteString("} ")
	b.WriteString(formatValue(value))
	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

// formatValue renders a metric value exactly as the text format
// expects.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabelValue escapes a label value per the text-format grammar:
// backslash, double-quote and newline carry meaning and must be quoted.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes a HELP docstring: only backslash and newline carry
// meaning there.
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
