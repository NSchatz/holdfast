package health

import (
	"context"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// MaxProblemsListed bounds the problem list each sweep in the report carries. The count
// beside it is always the whole figure.
const MaxProblemsListed = 500

// Report is the body of GET /api/health: what the sweep is doing, the sweep under way if
// there is one, and the last one that finished, each with the files it found corrupt or
// unreadable. Every time is Unix seconds, and a time that does not exist is null rather
// than zero.
type Report struct {
	// Enabled is whether this daemon's configuration schedules a sweep.
	Enabled bool `json:"enabled"`
	// IntervalHours is health_sweep_interval_hours; 0 is off.
	IntervalHours int `json:"interval_hours"`
	// State is off, idle, running or waiting.
	State string `json:"state"`
	// Waiting is why no new decode may start, while State is waiting.
	Waiting string `json:"waiting,omitempty"`
	// NextDueAt is when the next sweep is due, while one is scheduled and none is under way.
	NextDueAt *int64 `json:"next_due_at"`
	// Current is the sweep under way (or interrupted and waiting to resume), or null.
	Current *SweepReport `json:"current"`
	// LastCompleted is the newest sweep that ran to the end, or null.
	LastCompleted *SweepReport `json:"last_completed"`
}

// SweepReport is one sweep: its times, its counts and its problem files.
type SweepReport struct {
	ID         int64  `json:"id"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
	// Checked is the files this sweep has recorded a result for, so far for a sweep under
	// way. The files still to come are not known until the enumeration reaches them.
	Checked    int64 `json:"checked"`
	OK         int64 `json:"ok"`
	Corrupt    int64 `json:"corrupt"`
	Unreadable int64 `json:"unreadable"`
	// Problems is the corrupt and unreadable files, by path, at most MaxProblemsListed.
	Problems []Problem `json:"problems"`
	// ProblemsTruncated says Problems was cut at MaxProblemsListed.
	ProblemsTruncated bool `json:"problems_truncated"`
}

// Problem is one file a sweep found corrupt or unreadable.
type Problem struct {
	Path      string `json:"path"`
	Result    string `json:"result"`
	Reason    string `json:"reason"`
	CheckedAt int64  `json:"checked_at"`
	Size      int64  `json:"size"`
}

// BuildReport reads the report from the ledger. interval is the configured interval (0 is
// off) and live the sweeper's in-memory state; a daemon with no sweeper passes StateOff.
func BuildReport(ctx context.Context, ledger store.HealthLedger, interval time.Duration, live Live) (Report, error) {
	r := Report{Enabled: interval > 0, IntervalHours: int(interval / time.Hour), State: string(live.State)}
	if live.State == StateWaiting {
		r.Waiting = live.Why
	}
	latest, ok, err := ledger.LatestHealthSweep(ctx)
	if err != nil {
		return Report{}, err
	}
	if ok && !latest.Finished() {
		counts, err := ledger.HealthCountsOf(ctx, latest.ID)
		if err != nil {
			return Report{}, err
		}
		latest.Counts = counts
		if r.Current, err = sweepReport(ctx, ledger, latest); err != nil {
			return Report{}, err
		}
	}
	last, ok, err := ledger.LastFinishedHealthSweep(ctx)
	if err != nil {
		return Report{}, err
	}
	if ok {
		if r.LastCompleted, err = sweepReport(ctx, ledger, last); err != nil {
			return Report{}, err
		}
		if r.Enabled && r.Current == nil {
			due := last.FinishedAt.Add(interval).Unix()
			r.NextDueAt = &due
		}
	}
	return r, nil
}

func sweepReport(ctx context.Context, ledger store.HealthLedger, sw store.HealthSweep) (*SweepReport, error) {
	problems, err := ledger.HealthProblems(ctx, sw.ID, MaxProblemsListed+1)
	if err != nil {
		return nil, err
	}
	out := &SweepReport{
		ID: sw.ID, StartedAt: sw.StartedAt.Unix(),
		Checked: sw.Counts.Checked(), OK: sw.Counts.OK, Corrupt: sw.Counts.Corrupt, Unreadable: sw.Counts.Unreadable,
		Problems: []Problem{},
	}
	if sw.Finished() {
		f := sw.FinishedAt.Unix()
		out.FinishedAt = &f
	}
	if len(problems) > MaxProblemsListed {
		problems = problems[:MaxProblemsListed]
		out.ProblemsTruncated = true
	}
	for _, p := range problems {
		out.Problems = append(out.Problems, Problem{Path: p.Path, Result: string(p.Result), Reason: p.Reason,
			CheckedAt: p.CheckedAt.Unix(), Size: p.Size})
	}
	return out, nil
}
