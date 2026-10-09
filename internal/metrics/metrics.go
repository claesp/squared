// Package metrics has counters in the Prometheus text format. It has only
// the functions that Squared needs, to avoid a dependency on the
// Prometheus client library.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Registry holds metrics and writes them in the Prometheus text format.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	write(w io.Writer)
}

// Counter is a counter with labels.
type Counter struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

// NewCounter adds a counter to the registry.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{name: name, help: help, labels: labels, values: map[string]float64{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, c)
	r.mu.Unlock()
	return c
}

// Add adds v to the counter with the label values. The number of label
// values must be the same as the number of labels.
func (c *Counter) Add(v float64, labelValues ...string) {
	key := labelKey(c.labels, labelValues)
	c.mu.Lock()
	c.values[key] += v
	c.mu.Unlock()
}

// Inc adds 1.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

func (c *Counter) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", c.name, k, formatValue(c.values[k]))
	}
}

// Summary counts observations and their sum, for example durations.
type Summary struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	counts     map[string]float64
	sums       map[string]float64
}

// NewSummary adds a summary to the registry.
func (r *Registry) NewSummary(name, help string, labels ...string) *Summary {
	s := &Summary{name: name, help: help, labels: labels, counts: map[string]float64{}, sums: map[string]float64{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, s)
	r.mu.Unlock()
	return s
}

// Observe records one value.
func (s *Summary) Observe(v float64, labelValues ...string) {
	key := labelKey(s.labels, labelValues)
	s.mu.Lock()
	s.counts[key]++
	s.sums[key] += v
	s.mu.Unlock()
}

func (s *Summary) write(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s summary\n", s.name, s.help, s.name)
	keys := make([]string, 0, len(s.counts))
	for k := range s.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s_sum%s %s\n", s.name, k, formatValue(s.sums[k]))
		fmt.Fprintf(w, "%s_count%s %s\n", s.name, k, formatValue(s.counts[k]))
	}
}

func labelKey(names, values []string) string {
	if len(names) != len(values) {
		panic(fmt.Sprintf("metrics: %d label values for %d labels", len(values), len(names)))
	}
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, len(names))
	for i, n := range names {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(values[i])
		parts[i] = n + `="` + v + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func formatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// Write writes all metrics.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	list := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range list {
		m.write(w)
	}
}

// Handler serves the metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}
