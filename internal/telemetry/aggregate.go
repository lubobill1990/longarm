package telemetry

import (
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// aggregator sums requests, dependencies and counters per minute, so a
// busy worker sends a few metric items instead of one item per call.
type aggregator struct {
	mu     sync.Mutex
	durs   map[string]*durStats
	counts map[string]*counter
}

type durStats struct {
	kind, name string
	stream     bool
	n, failed  int64
	sum        float64
	min, max   float64
	samples    []float64 // reservoir for percentiles
}

type counter struct {
	name  string
	props map[string]string
	n     float64
}

const (
	maxAggKeys = 500 // distinct names per period
	reservoir  = 256
)

func newAggregator() *aggregator {
	return &aggregator{durs: map[string]*durStats{}, counts: map[string]*counter{}}
}

func (a *aggregator) duration(kind, name string, ms float64, ok, stream bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := kind + "\x00" + name
	d := a.durs[k]
	if d == nil {
		if len(a.durs) >= maxAggKeys {
			return
		}
		d = &durStats{kind: kind, name: name, stream: stream, min: ms, max: ms}
		a.durs[k] = d
	}
	d.n++
	if !ok {
		d.failed++
	}
	d.sum += ms
	d.min, d.max = min(d.min, ms), max(d.max, ms)
	if len(d.samples) < reservoir {
		d.samples = append(d.samples, ms)
	} else if i := rand.Int64N(d.n); i < reservoir {
		d.samples[i] = ms
	}
}

func (a *aggregator) count(name string, props map[string]string, n float64) {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + props[k])
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.counts[b.String()]
	if c == nil {
		if len(a.counts) >= maxAggKeys {
			return
		}
		cp := make(map[string]string, len(props))
		for k, v := range props {
			cp[k] = v
		}
		c = &counter{name: name, props: cp}
		a.counts[b.String()] = c
	}
	c.n += n
}

// drain returns the period's metric items and starts a new period.
func (a *aggregator) drain() []Item {
	a.mu.Lock()
	durs, counts := a.durs, a.counts
	a.durs, a.counts = map[string]*durStats{}, map[string]*counter{}
	a.mu.Unlock()
	now := time.Now().UTC()
	var out []Item
	for _, d := range durs {
		sort.Float64s(d.samples)
		props := map[string]string{
			"name": d.name, "failures": strconv.FormatInt(d.failed, 10),
			"p50": pct(d.samples, 0.50), "p95": pct(d.samples, 0.95), "p99": pct(d.samples, 0.99),
		}
		if d.stream {
			props["stream"] = "true"
		}
		out = append(out, Item{Kind: KindMetric, Time: now, Name: d.kind + ".duration_ms", Value: d.sum, Count: d.n,
			Min: d.min, Max: d.max, Props: props})
	}
	for _, c := range counts {
		out = append(out, Item{Kind: KindMetric, Time: now, Name: c.name, Value: c.n, Count: 1, Min: c.n, Max: c.n, Props: c.props})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func pct(sorted []float64, p float64) string {
	if len(sorted) == 0 {
		return "0"
	}
	i := int(float64(len(sorted)-1) * p)
	return strconv.FormatFloat(sorted[i], 'f', 1, 64)
}
