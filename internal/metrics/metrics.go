// Package metrics exposes Prometheus instrumentation for the transcoder
// (TRANSCODE-8). It is a pure observer: it subscribes to engine.Event notifications
// and reads the store on scrape — it never touches a media file or influences the
// engine, so it cannot affect the data-safety invariant. Metrics are best-effort; a
// store hiccup on scrape simply omits the one gauge that read would have filled.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// Namespace prefixes every series this build publishes. It is what tells holdfast's own
// metrics apart from the Go and process collectors registered beside them.
const Namespace = "holdfast_"

// GuardUnclassified is the bucket a skip is counted under when its recorded reason is not
// a member of the engine's skip vocabulary - an empty reason, a row with no outcome at
// all, or a token from a build newer than this one.
//
// It is a METRIC-ONLY value and never a guard token: no engine verdict records it, and
// nothing keys off it but this counter. It exists so that an unrecognised reason is
// COUNTED rather than either dropped (which would make the per-guard counts disagree with
// the skip count beside them) or used as a label value (which would put an unbounded
// string on a series and grow one per distinct reason).
const GuardUnclassified = "unclassified"

// Metrics holds the transcoder's Prometheus collectors on a private registry (so a
// test — or an embedding process — gets an isolated set, never the global default).
type Metrics struct {
	reg            *prometheus.Registry
	filesTotal     *prometheus.CounterVec // by terminal outcome: done|skipped|failed
	skipsTotal     *prometheus.CounterVec // by the guard that skipped the file
	failuresTotal  *prometheus.CounterVec // by the gate that rejected the job
	bytesReclaimed prometheus.Counter
	encodeDuration prometheus.Histogram
	vmaf           prometheus.Histogram
	vmafMin        prometheus.Histogram
	vmafChroma     prometheus.Histogram

	// guards and gates are the label values each counter above is allowed to carry, built
	// once from the engine's own vocabularies. A value outside the set is never used as a
	// label: it is counted under the fallback, so the number of series is knowable in
	// advance and nothing the engine meets on disk can move it.
	guards map[string]bool
	gates  map[string]bool
}

// New builds the metric set over st (used for the on-scrape gauges) and registers the
// standard Go/process collectors alongside the transcoder's own.
//
// log is where a scrape-time store read that FAILS is reported. A nil logger is accepted
// and discards, so an embedding process that wants no metrics logging says so by passing
// nil rather than by getting silence it did not ask for.
func New(st store.Store, log *slog.Logger) *Metrics {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "metrics")

	m := &Metrics{
		reg: prometheus.NewRegistry(),
		filesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "holdfast_files_total",
			Help: "Total files reaching a terminal outcome, by outcome (done|skipped|failed|would-transcode|indeterminate|applied-despite-error). would-transcode counts DECISIONS a dry run took, never transcodes that happened.",
		}, []string{"outcome"}),
		skipsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "holdfast_skips_total",
			Help: "Total files skipped, by the guard that skipped them. The label set is the engine's closed skip vocabulary plus `unclassified`, which counts a reason this build does not recognise; it never carries a path or an arbitrary string.",
		}, []string{"guard"}),
		failuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "holdfast_failures_total",
			Help: "Total files that FAILED, by the gate or stage that rejected them (probe|encode|codec|length|size|stream-parity|decode|vmaf-mean|vmaf-min|vmaf-chroma|vmaf-unmeasured|swap|other). Decided where the rejection is made, never read off the error text, so the sum equals holdfast_files_total{outcome=\"failed\"}.",
		}, []string{"gate"}),
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
		vmafMin: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "holdfast_vmaf_min",
			Help: "Worst (sub)sampled frame's VMAF on accepted outputs. The mean hides local damage; this is the statistic the worst-frame floor (vmaf_min_pool) is read against.",
			// Wider at the bottom than the mean's buckets, because that is where this
			// statistic lives: a locally-broken encode whose mean is ~97 has a worst frame
			// down at ~43, and the shipped floor is 60.
			Buckets: []float64{20, 40, 50, 55, 60, 65, 70, 80, 90, 95, 100},
		}),
		vmafChroma: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "holdfast_vmaf_chroma",
			Help: "Worst (sub)sampled frame's chroma PSNR in dB on accepted outputs - the only figure that says whether the COLOUR survived, since the VMAF model is luma-only. Read against the vmaf_min_chroma floor (30 dB by default).",
			// A PSNR in dB and NOT a 0-100 VMAF, so it cannot share the buckets above: a
			// healthy encode measures around 40 dB and a 15% desaturation falls to ~26.
			Buckets: []float64{20, 25, 28, 30, 32, 35, 40, 45, 50, 60},
		}),
		guards: vocabulary(append(append([]string{}, engine.SkipVocabulary...), GuardUnclassified)),
		gates:  vocabulary(engine.GateVocabulary),
	}
	// Pre-create the outcome series so they read 0 (not absent) before the first event.
	// The two FILESYSTEM-1 outcomes get their own series rather than being folded into
	// done or failed: an alert on "a job holdfast could not establish the outcome of"
	// is exactly the alert an operator wants, and it is unbuildable if the count is
	// hidden inside another label.
	//
	// A dry run's decision is pre-created for exactly that reason: an alert on "how many
	// files would this tool transcode" has to be buildable BEFORE the first dry run, and a
	// series that appears only once something has happened cannot be alerted on until it
	// is too late to be useful. Note this is a new LABEL VALUE on an existing counter and
	// not a new metric name - the published name set is frozen and stays frozen.
	for _, o := range []string{
		string(store.Done), string(store.Skipped), string(store.Failed),
		string(store.WouldTranscode),
		string(store.Indeterminate), string(store.AppliedDespiteError),
	} {
		m.filesTotal.WithLabelValues(o)
	}
	// The same argument, applied to the two vocabularies: every guard and every gate reads
	// 0 from the start. "Files began skipping as exotic-pixel-format after that config
	// change" is an alert somebody has to be able to write BEFORE the first such skip, and
	// a series that springs into existence at first use cannot carry one. The fallbacks are
	// pre-created too, so counting an unrecognised value never CREATES a label value.
	for g := range m.guards {
		m.skipsTotal.WithLabelValues(g)
	}
	for g := range m.gates {
		m.failuresTotal.WithLabelValues(g)
	}
	m.reg.MustRegister(m.filesTotal, m.skipsTotal, m.failuresTotal, m.bytesReclaimed,
		m.encodeDuration, m.vmaf, m.vmafMin, m.vmafChroma)
	// TWO scrape-time collectors and not one, which is the whole of AC-9's independence:
	// a collector returns on a store error and emits nothing more, so a pair of gauges
	// behind one of them would hide each other on any read failure. Registered separately,
	// the read that failed costs exactly the series it would have filled.
	m.reg.MustRegister(newQueueCollector(st, log))
	m.reg.MustRegister(newUndoWindowCollector(st, log))
	m.reg.MustRegister(collectors.NewGoCollector())
	m.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// vocabulary turns a closed list of label values into the set the counters check against.
func vocabulary(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// Observe implements engine.Observer. It runs on an engine worker goroutine, so it
// only does cheap, non-blocking counter/histogram updates (prometheus client metrics
// are lock-free/atomic) — it never blocks an encode. Only terminal transitions are
// counted; the Done rich event (emitted exactly once) also carries the reclaimed
// bytes, encode duration, and VMAF scores.
func (m *Metrics) Observe(ev engine.Event) {
	switch ev.Status {
	case store.Done:
		m.filesTotal.WithLabelValues("done").Inc()
		if n := ev.BytesReclaimed(); n > 0 {
			m.bytesReclaimed.Add(float64(n))
		}
		// The encode duration and the VMAF figures now come off the event's Outcome — the
		// same value the ledger stores (TRANSCODE-13). Note the nil checks rather than
		// `> 0`: a pointer distinguishes "not measured" from a measured zero, and the
		// old `ev.VmafScore > 0` would have silently dropped a genuine VMAF of 0.0 —
		// the single most alarming score there is — from the distribution.
		if o := ev.Outcome; o != nil {
			if o.EncodeMs != nil {
				m.encodeDuration.Observe(float64(*o.EncodeMs) / 1000.0) // the histogram is in seconds
			}
			if o.VmafMean != nil {
				m.vmaf.Observe(*o.VmafMean)
			}
			// The two floors, each observed ONLY where it was measured. The same rule the
			// mean keeps and for a sharper reason: 0.0 here is a destroyed frame and an
			// obliterated colour plane, so observing one for a job whose gate never ran
			// would put the worst reading this instrument can take into the distribution
			// on behalf of a measurement nobody made. A remux, a disabled gate and a
			// failed measurement all arrive here with nil and contribute nothing.
			if o.VmafMin != nil {
				m.vmafMin.Observe(*o.VmafMin)
			}
			if o.VmafChroma != nil {
				m.vmafChroma.Observe(*o.VmafChroma)
			}
		}
	case store.Skipped:
		m.filesTotal.WithLabelValues(string(store.Skipped)).Inc()
		m.skipsTotal.WithLabelValues(m.guardOf(ev)).Inc()
	case store.WouldTranscode:
		// A DECISION a dry run took, counted as itself. It is not folded into skipped -
		// that would say the file does not qualify, when the whole point of the row is
		// that it does - and not into done, which would claim a transcode that never
		// happened. Nothing was encoded, so no reclaimed bytes, no encode duration and no
		// VMAF ride with it.
		m.filesTotal.WithLabelValues(string(store.WouldTranscode)).Inc()
	case store.Failed:
		// The two counters move TOGETHER, in one branch, so the per-gate counts always add
		// up to the failure count beside them: a failure attributed to nothing is counted
		// under the fallback rather than dropped.
		m.filesTotal.WithLabelValues(string(store.Failed)).Inc()
		m.failuresTotal.WithLabelValues(m.gateOf(ev)).Inc()
	case store.Indeterminate:
		m.filesTotal.WithLabelValues(string(store.Indeterminate)).Inc()
	case store.AppliedDespiteError:
		m.filesTotal.WithLabelValues(string(store.AppliedDespiteError)).Inc()
	}
	// pending/probing/encoding/verifying are non-terminal — reflected live by the
	// queue-depth gauge (read from the store on scrape), not counted here.
}

// guardOf is the label value a skipped event is counted under: the guard token the engine
// recorded, when it is one this build knows, and the fallback otherwise.
//
// Membership is decided against the engine's Skip* CONSTANT SET (engine.SkipVocabulary),
// which is not engine.SkipGuards: that one is the smaller vocabulary `requeue --guard`
// accepts and omits the mutable guards, which do fire in ordinary operation.
func (m *Metrics) guardOf(ev engine.Event) string {
	if ev.Outcome != nil && m.guards[ev.Outcome.Reason] {
		return ev.Outcome.Reason
	}
	return GuardUnclassified
}

// gateOf is the label value a failed event is counted under: the gate the engine decided
// at the line that refused the job, or the vocabulary's own fallback when the event
// carries none. The Reason text is never consulted - it is unbounded, and a classifier
// matching on it drifts the moment a message is reworded.
func (m *Metrics) gateOf(ev engine.Event) string {
	if m.gates[ev.Gate] {
		return ev.Gate
	}
	return engine.GateOther
}

// Handler returns the /metrics HTTP handler for this metric set's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// fqNameRE reads the fully-qualified name out of a descriptor's rendering, which is the
// only route the client library offers: Desc keeps its fields unexported and exposes
// String alone.
var fqNameRE = regexp.MustCompile(`fqName: "([^"]*)"`)

// PublishedNames is every series name THIS build registers under the holdfast namespace,
// sorted. It is what the documentation check grades the shipped Markdown against, so that
// a metric can never be published without a line somewhere saying what it means.
//
// It is read from the registry's own DESCRIPTORS rather than from a scrape. A scrape
// reports the series that exist at that moment - a labelled counter with no values yet,
// or a gauge whose store read just failed, emits nothing at all - so a check built on one
// would quietly grade a shorter list than the build publishes, and would stop covering a
// metric without anything saying so.
//
// A descriptor whose name cannot be read is returned WHOLE rather than dropped, for the
// same reason: it fails the check that reads this, loudly, instead of narrowing it.
func (m *Metrics) PublishedNames() []string {
	ch := make(chan *prometheus.Desc, 128)
	go func() {
		m.reg.Describe(ch)
		close(ch)
	}()
	seen := map[string]bool{}
	for d := range ch {
		s := d.String()
		if match := fqNameRE.FindStringSubmatch(s); match != nil {
			if strings.HasPrefix(match[1], Namespace) {
				seen[match[1]] = true
			}
			continue
		}
		if strings.Contains(s, Namespace) {
			seen[s] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// queueCollector reports the current job count per status, read from the store at
// scrape time (a gauge, not a counter — it reflects live queue depth, not a total).
type queueCollector struct {
	st   store.Store
	log  *slog.Logger
	desc *prometheus.Desc
}

func newQueueCollector(st store.Store, log *slog.Logger) *queueCollector {
	return &queueCollector{
		st:  st,
		log: log,
		desc: prometheus.NewDesc(
			"holdfast_queue_depth",
			"Current number of jobs in each status (pending/probing/encoding/verifying/done/skipped/failed/would-transcode/indeterminate/applied-despite-error), read from the store at scrape time.",
			[]string{"state"}, nil),
	}
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	// Best-effort: a store error on scrape omits the gauge this cycle rather than
	// failing the whole /metrics response. It is reported as the degraded state it is -
	// which dependency, what was asked of it, and what happens next - because a gauge that
	// goes missing in silence reads to a monitoring system exactly like a daemon that died.
	sum, err := c.st.Summary(context.Background())
	if err != nil {
		c.log.Warn("the job store could not be read for the queue-depth gauge, so that gauge is omitted from this scrape; every other series is unaffected and the next scrape reads again",
			"dependency", "job store", "read", "Summary", "err", err)
		return
	}
	for st, n := range sum {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), string(st))
	}
}

// undoWindowCollector reports the bytes the undo window is still HOLDING, read from the
// store at scrape time. It is the one figure that explains "why has free space not gone up
// after all those transcodes": a retained original is a second link to the source's bytes,
// so the space is not returned until the window lets it go.
//
// It is its OWN collector rather than a second gauge inside the queue collector, and that
// is the point of it: a collector that meets a store error emits nothing further, so the
// two reads have to sit behind two collectors for one failure to cost one series.
type undoWindowCollector struct {
	st   store.Store
	log  *slog.Logger
	desc *prometheus.Desc
}

func newUndoWindowCollector(st store.Store, log *slog.Logger) *undoWindowCollector {
	return &undoWindowCollector{
		st:  st,
		log: log,
		desc: prometheus.NewDesc(
			"holdfast_bytes_held_by_undo_window",
			"Bytes the undo window is still holding: the sum of the retained originals that have not yet aged out, read from the store at scrape time. Space a swap reclaimed is not returned to the filesystem until the window releases the original.",
			nil, nil),
	}
}

func (c *undoWindowCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *undoWindowCollector) Collect(ch chan<- prometheus.Metric) {
	n, err := c.st.HeldByUndoWindow(context.Background())
	if err != nil {
		c.log.Warn("the job store could not be read for the undo-window gauge, so that gauge is omitted from this scrape; every other series - the queue depth included - is unaffected and the next scrape reads again",
			"dependency", "job store", "read", "HeldByUndoWindow", "err", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n))
}
