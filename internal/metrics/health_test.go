package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// seedFinishedHealthSweep records one finished sweep with one corrupt and two ok files.
func seedFinishedHealthSweep(t *testing.T, st *store.SQLite) store.HealthSweep {
	t.Helper()
	ctx := context.Background()
	t0 := time.Unix(1_790_000_000, 0)
	sw, err := st.StartHealthSweep(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range []store.HealthResult{store.HealthOK, store.HealthOK, store.HealthCorrupt} {
		if err := st.RecordHealthCheck(ctx, store.HealthCheck{SweepID: sw.ID, Path: "/lib/" + string(rune('a'+i)) + ".mkv",
			Size: 1, MtimeNS: 1, CheckedAt: t0, Result: r, Reason: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FinishHealthSweep(ctx, sw.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return sw
}

// TestMetrics_HealthSweepSeries: the counter counts each recorded result under its own
// label and ignores a value outside the vocabulary; the gauges are ABSENT before any sweep
// finished and report the newest finished sweep after.
func TestMetrics_HealthSweepSeries(t *testing.T) {
	st := openStore(t)
	m := New(st, nil)
	body := scrape(t, m)
	for _, absent := range []string{"holdfast_health_sweep_corrupt_files ", "holdfast_health_sweep_unreadable_files ",
		"holdfast_health_sweep_last_completed_timestamp_seconds "} {
		if strings.Contains(body, absent) {
			t.Errorf("%s is published before any sweep finished; nobody has looked, which is not zero", absent)
		}
	}
	if !strings.Contains(body, `holdfast_health_sweep_files_checked_total{result="corrupt"} 0`) {
		t.Error("the checked counter is not pre-created at 0")
	}

	m.HealthFileChecked(store.HealthOK)
	m.HealthFileChecked(store.HealthCorrupt)
	m.HealthFileChecked(store.HealthCorrupt)
	m.HealthFileChecked(store.HealthUnreadable)
	m.HealthFileChecked("bogus")
	m.HealthSweepFinished(store.HealthSweep{}, nil)
	seedFinishedHealthSweep(t, st)
	body = scrape(t, m)
	for _, want := range []string{
		`holdfast_health_sweep_files_checked_total{result="ok"} 1`,
		`holdfast_health_sweep_files_checked_total{result="corrupt"} 2`,
		`holdfast_health_sweep_files_checked_total{result="unreadable"} 1`,
		"holdfast_health_sweep_corrupt_files 1",
		"holdfast_health_sweep_unreadable_files 0",
		"holdfast_health_sweep_last_completed_timestamp_seconds 1.7900036e+09",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not carry %q", want)
		}
	}
	if strings.Contains(body, "bogus") {
		t.Error("a result outside the vocabulary became a label value")
	}
}

// failingHealth is a store whose health read fails.
type failingHealth struct{ store.Store }

func (failingHealth) LastFinishedHealthSweep(context.Context) (store.HealthSweep, bool, error) {
	return store.HealthSweep{}, false, errors.New("disk on fire")
}

// TestMetrics_AFailedHealthReadCostsOnlyItsOwnGauges.
func TestMetrics_AFailedHealthReadCostsOnlyItsOwnGauges(t *testing.T) {
	st := openStore(t)
	seedFinishedHealthSweep(t, st)
	body := scrape(t, New(failingHealth{st}, nil))
	if strings.Contains(body, "holdfast_health_sweep_corrupt_files ") {
		t.Error("a failed read published a gauge")
	}
	if !strings.Contains(body, "holdfast_bytes_held_by_undo_window") {
		t.Error("a failed health read cost another series")
	}
}
