package metrics

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ContentType is the Prometheus text exposition format this package writes,
// both for a scrape and for a push.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// registry holds every metric family, in the order they were declared.
type registry struct {
	families []family
}

type family interface {
	// write renders the family, and nothing at all while it has no series:
	// a declared family with no samples is noise to a scrape and an error to
	// some Pushgateway versions.
	write(w *bufio.Writer)
}

func (r *registry) add(f family) { r.families = append(r.families, f) }

func (r *registry) writeText(out io.Writer) error {
	w := bufio.NewWriter(out)
	for _, f := range r.families {
		f.write(w)
	}
	return w.Flush()
}

// counterVec is a counter with a fixed list of label names.
type counterVec struct {
	name, help string
	labels     []string

	mu     sync.Mutex
	series map[string]*counterSeries
}

type counterSeries struct {
	values []string
	value  float64
}

func newCounterVec(r *registry, name, help string, labels ...string) *counterVec {
	c := &counterVec{name: name, help: help, labels: labels, series: map[string]*counterSeries{}}
	r.add(c)
	return c
}

func (c *counterVec) add(v float64, values ...string) {
	key := seriesKey(values)
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.series[key]
	if !ok {
		s = &counterSeries{values: append([]string(nil), values...)}
		c.series[key] = s
	}
	s.value += v
}

func (c *counterVec) inc(values ...string) { c.add(1, values...) }

func (c *counterVec) write(w *bufio.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.series) == 0 {
		return
	}
	writeHeader(w, c.name, c.help, "counter")
	for _, key := range sortedKeys(c.series) {
		s := c.series[key]
		writeSample(w, c.name, c.labels, s.values, "", "", s.value)
	}
}

// histogramVec is a histogram with a fixed list of label names and fixed
// buckets.
type histogramVec struct {
	name, help string
	labels     []string
	buckets    []float64

	mu     sync.Mutex
	series map[string]*histogramSeries
}

type histogramSeries struct {
	values []string
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

func newHistogramVec(r *registry, name, help string, buckets []float64, labels ...string) *histogramVec {
	h := &histogramVec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histogramSeries{}}
	r.add(h)
	return h
}

func (h *histogramVec) observe(v float64, values ...string) {
	key := seriesKey(values)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histogramSeries{values: append([]string(nil), values...), counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	// The first bucket the value fits in; the cumulative counts are summed
	// when written, which keeps an observation to one increment.
	if i := sort.SearchFloat64s(h.buckets, v); i < len(h.buckets) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
}

func (h *histogramVec) write(w *bufio.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.series) == 0 {
		return
	}
	writeHeader(w, h.name, h.help, "histogram")
	for _, key := range sortedKeys(h.series) {
		s := h.series[key]
		var cumulative uint64
		for i, bound := range h.buckets {
			cumulative += s.counts[i]
			writeSample(w, h.name+"_bucket", h.labels, s.values, "le", formatFloat(bound), float64(cumulative))
		}
		writeSample(w, h.name+"_bucket", h.labels, s.values, "le", "+Inf", float64(s.count))
		writeSample(w, h.name+"_sum", h.labels, s.values, "", "", s.sum)
		writeSample(w, h.name+"_count", h.labels, s.values, "", "", float64(s.count))
	}
}

// gaugeFunc is a gauge read when the metrics are rendered, with labels that
// never change.
type gaugeFunc struct {
	name, help string
	labels     []string
	values     []string
	read       func() float64
}

func newGaugeFunc(r *registry, name, help string, read func() float64) *gaugeFunc {
	g := &gaugeFunc{name: name, help: help, read: read}
	r.add(g)
	return g
}

func (g *gaugeFunc) write(w *bufio.Writer) {
	writeHeader(w, g.name, g.help, "gauge")
	writeSample(w, g.name, g.labels, g.values, "", "", g.read())
}

func writeHeader(w *bufio.Writer, name, help, kind string) {
	w.WriteString("# HELP ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(helpEscaper.Replace(help))
	w.WriteString("\n# TYPE ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(kind)
	w.WriteByte('\n')
}

// writeSample writes one line. extraName and extraValue are the histogram's
// `le`, appended after the family's own labels.
func writeSample(w *bufio.Writer, name string, labels, values []string, extraName, extraValue string, v float64) {
	w.WriteString(name)
	if len(labels) > 0 || extraName != "" {
		w.WriteByte('{')
		for i, label := range labels {
			if i > 0 {
				w.WriteByte(',')
			}
			writeLabel(w, label, values[i])
		}
		if extraName != "" {
			if len(labels) > 0 {
				w.WriteByte(',')
			}
			writeLabel(w, extraName, extraValue)
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(formatFloat(v))
	w.WriteByte('\n')
}

func writeLabel(w *bufio.Writer, name, value string) {
	w.WriteString(name)
	w.WriteString(`="`)
	w.WriteString(valueEscaper.Replace(value))
	w.WriteByte('"')
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	valueEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, +1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// seriesKey joins label values with a byte no label value in this package
// contains, so distinct label sets never share a key.
func seriesKey(values []string) string { return strings.Join(values, "\xff") }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
