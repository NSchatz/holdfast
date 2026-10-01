package metrics

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/NSchatz/holdfast/internal/store"
)

// The library health sweep's series (docs/design/health-sweep.md#health-sweep). The
// counter moves as results are recorded; the three gauges are read from the ledger at
// scrape time, about the newest sweep that FINISHED, and are absent until one has: a
// library nobody has swept is not a library with no corrupt files.

func newHealthChecked() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "holdfast_health_sweep_files_checked_total",
		Help: "Files the library health sweep fully decoded and recorded a result for, by result (ok|corrupt|unreadable). Report only: the sweep never changes a file.",
	}, []string{"result"})
	for _, r := range []store.HealthResult{store.HealthOK, store.HealthCorrupt, store.HealthUnreadable} {
		c.WithLabelValues(string(r))
	}
	return c
}

// HealthFileChecked implements the health sweep's reporter: one recorded result.
func (m *Metrics) HealthFileChecked(result store.HealthResult) {
	if store.ValidHealthResult(result) {
		m.healthChecked.WithLabelValues(string(result)).Inc()
	}
}

// HealthSweepFinished implements the health sweep's reporter. The gauges are read from the
// ledger at scrape time, so there is nothing to do here.
func (m *Metrics) HealthSweepFinished(store.HealthSweep, []store.HealthCheck) {}

// healthCollector reports the newest finished sweep, read from the store at scrape time.
type healthCollector struct {
	st                         store.Store
	log                        *slog.Logger
	corrupt, unreadable, ended *prometheus.Desc
}

func newHealthCollector(st store.Store, log *slog.Logger) *healthCollector {
	return &healthCollector{
		st:  st,
		log: log,
		corrupt: prometheus.NewDesc("holdfast_health_sweep_corrupt_files",
			"Files the newest FINISHED library health sweep found corrupt (read but not decoded cleanly to the end). Absent until a sweep has finished.", nil, nil),
		unreadable: prometheus.NewDesc("holdfast_health_sweep_unreadable_files",
			"Files the newest FINISHED library health sweep could not read at all. Absent until a sweep has finished.", nil, nil),
		ended: prometheus.NewDesc("holdfast_health_sweep_last_completed_timestamp_seconds",
			"Unix time the newest library health sweep finished. Absent until one has.", nil, nil),
	}
}

func (c *healthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.corrupt
	ch <- c.unreadable
	ch <- c.ended
}

func (c *healthCollector) Collect(ch chan<- prometheus.Metric) {
	sw, ok, err := c.st.LastFinishedHealthSweep(context.Background())
	if err != nil {
		c.log.Warn("the job store could not be read for the health sweep gauges, so they are omitted from this scrape; every other series is unaffected and the next scrape reads again",
			"dependency", "job store", "read", "LastFinishedHealthSweep", "err", err)
		return
	}
	if !ok {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.corrupt, prometheus.GaugeValue, float64(sw.Counts.Corrupt))
	ch <- prometheus.MustNewConstMetric(c.unreadable, prometheus.GaugeValue, float64(sw.Counts.Unreadable))
	ch <- prometheus.MustNewConstMetric(c.ended, prometheus.GaugeValue, float64(sw.FinishedAt.Unix()))
}
