// Package metrics exposes Prometheus instrumentation for the transcoder
// (TRANSCODE-8). It is a pure observer: it subscribes to engine.Event notifications
// and reads the store on scrape — it never touches a media file or influences the
// engine, so it cannot affect the data-safety invariant. Metrics are best-effort; a
// store hiccup on scrape simply omits the queue-depth gauge for that scrape.
package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// Metrics holds the transcoder's Prometheus collectors on a private registry (so a
// test — or an embedding process — gets an isolated set, never the global default).
type Metrics struct {
	reg            *prometheus.Registry
	filesTotal     *prometheus.CounterVec // by terminal outcome: done|skipped|failed
	bytesReclaimed prometheus.Counter
	encodeDuration prometheus.Histogram
	vmaf           prometheus.Histogram
}

// New builds the metric set over st (used for the on-scrape queue-depth gauge) and
// registers the standard Go/process collectors alongside the transcoder's own.
func New(st store.Store) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		filesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "holdfast_files_total",
			Help: "Total files reaching a terminal outcome, by outcome (done|skipped|failed).",
		}, []string{"outcome"}),
		bytesReclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "holdfast_bytes_reclaimed_total",
			Help: "Total bytes of disk reclaimed by successful transcodes (source size minus output size).",
		}),
		encodeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "holdfast_encode_duration_seconds",
			Help:    "Wall-clock encode duration of successful transcodes.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 13), // 1s → ~68m
		}),
		vmaf: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "holdfast_vmaf_score",
			Help:    "VMAF harmonic-mean of accepted outputs (perceptual-quality distribution).",
			Buckets: []float64{80, 85, 90, 92, 94, 95, 96, 97, 98, 99, 100},
		}),
	}
	// Pre-create the outcome series so they read 0 (not absent) before the first event.
	for _, o := range []string{"done", "skipped", "failed"} {
		m.filesTotal.WithLabelValues(o)
	}
	m.reg.MustRegister(m.filesTotal, m.bytesReclaimed, m.encodeDuration, m.vmaf)
	m.reg.MustRegister(newQueueCollector(st))
	m.reg.MustRegister(collectors.NewGoCollector())
	m.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Observe implements engine.Observer. It runs on an engine worker goroutine, so it
// only does cheap, non-blocking counter/histogram updates (prometheus client metrics
// are lock-free/atomic) — it never blocks an encode. Only terminal transitions are
// counted; the Done rich event (emitted exactly once) also carries the reclaimed
// bytes, encode duration, and VMAF score.
func (m *Metrics) Observe(ev engine.Event) {
	switch ev.Status {
	case store.Done:
		m.filesTotal.WithLabelValues("done").Inc()
		if ev.BytesReclaimed > 0 {
			m.bytesReclaimed.Add(float64(ev.BytesReclaimed))
		}
		if ev.EncodeDuration > 0 {
			m.encodeDuration.Observe(ev.EncodeDuration.Seconds())
		}
		if ev.VmafScore > 0 {
			m.vmaf.Observe(ev.VmafScore)
		}
	case store.Skipped:
		m.filesTotal.WithLabelValues("skipped").Inc()
	case store.Failed:
		m.filesTotal.WithLabelValues("failed").Inc()
	}
	// pending/probing/encoding/verifying are non-terminal — reflected live by the
	// queue-depth gauge (read from the store on scrape), not counted here.
}

// Handler returns the /metrics HTTP handler for this metric set's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// queueCollector reports the current job count per status, read from the store at
// scrape time (a gauge, not a counter — it reflects live queue depth, not a total).
type queueCollector struct {
	st   store.Store
	desc *prometheus.Desc
}

func newQueueCollector(st store.Store) *queueCollector {
	return &queueCollector{
		st: st,
		desc: prometheus.NewDesc(
			"holdfast_queue_depth",
			"Current number of jobs in each status (pending/probing/encoding/verifying/done/skipped/failed).",
			[]string{"state"}, nil),
	}
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	// Best-effort: a store error on scrape omits the gauge this cycle rather than
	// failing the whole /metrics response.
	sum, err := c.st.Summary(context.Background())
	if err != nil {
		return
	}
	for st, n := range sum {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), string(st))
	}
}
