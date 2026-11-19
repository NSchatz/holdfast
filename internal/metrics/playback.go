package metrics

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/NSchatz/holdfast/internal/playback"
	"github.com/NSchatz/holdfast/internal/store"
)

// Playback evidence's series (docs/design/playback-evidence.md#playback-evidence), read from the
// ledger at scrape time. They are absent unless this daemon collects evidence, and until a poll
// has read the history to its end: a library nobody has asked about is not a library with no
// stalls.

// PlaybackReader is the part of the ledger the playback series are read from.
type PlaybackReader interface {
	PlaybackPollOf(ctx context.Context, source string) (store.PlaybackPoll, bool, error)
	PlaybackEvidence(ctx context.Context) ([]store.PlaybackSession, error)
}

// playbackConfig is what SetPlayback hands the collector.
type playbackConfig struct {
	source string
	rules  playback.Rules
	reads  PlaybackReader
}

// SetPlayback turns the playback evidence series on, for the source this daemon polls and the
// rules its verdicts are derived under, read through reads. `serve` passes the REPORTING door,
// so a scrape's read of the evidence never queues on the one write connection the engine's
// claims and finishes go through. Without it the series are never emitted.
func (m *Metrics) SetPlayback(source string, rules playback.Rules, reads PlaybackReader) {
	m.playback.cfg.Store(&playbackConfig{source: source, rules: rules, reads: reads})
}

// playbackCollector reports the suspected stalls, the files played through, and how far the
// history has been read.
type playbackCollector struct {
	log                     *slog.Logger
	cfg                     atomic.Pointer[playbackConfig]
	now                     func() time.Time
	suspect, played, polled *prometheus.Desc
}

func newPlaybackCollector(log *slog.Logger) *playbackCollector {
	return &playbackCollector{
		log: log,
		now: time.Now,
		suspect: prometheus.NewDesc("holdfast_playback_suspect_files",
			"Swapped files whose playback evidence is a suspected stall: at least two sessions ending at one point well before the end. Report only: nothing is restored. Absent until a poll has read the history to its end.", nil, nil),
		played: prometheus.NewDesc("holdfast_playback_played_through_files",
			"Swapped files a session has played through since the swap, and no stall is suspected in. Absent until a poll has read the history to its end.", nil, nil),
		polled: prometheus.NewDesc("holdfast_playback_history_read_timestamp_seconds",
			"Unix time the newest poll that read the playback history to its end began. Absent until one has.", nil, nil),
	}
}

func (c *playbackCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.suspect
	ch <- c.played
	ch <- c.polled
}

func (c *playbackCollector) Collect(ch chan<- prometheus.Metric) {
	cfg := c.cfg.Load()
	if cfg == nil {
		return
	}
	ctx := context.Background()
	poll, ok, err := cfg.reads.PlaybackPollOf(ctx, cfg.source)
	if err != nil {
		c.log.Warn("the job store could not be read for the playback evidence gauges, so they are omitted from this scrape; every other series is unaffected and the next scrape reads again",
			"dependency", "job store", "read", "PlaybackPollOf", "err", err)
		return
	}
	if !ok || poll.CoveredThrough == 0 {
		return
	}
	ev, err := cfg.reads.PlaybackEvidence(ctx)
	if err != nil {
		c.log.Warn("the job store could not be read for the playback evidence gauges, so they are omitted from this scrape; every other series is unaffected and the next scrape reads again",
			"dependency", "job store", "read", "PlaybackEvidence", "err", err)
		return
	}
	suspect, played := playback.Tally(ev, cfg.rules, c.now().Unix())
	ch <- prometheus.MustNewConstMetric(c.suspect, prometheus.GaugeValue, float64(suspect))
	ch <- prometheus.MustNewConstMetric(c.played, prometheus.GaugeValue, float64(played))
	ch <- prometheus.MustNewConstMetric(c.polled, prometheus.GaugeValue, float64(poll.CoveredThrough))
}
