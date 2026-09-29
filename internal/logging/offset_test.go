package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

// timeField matches the time field every line starts with, in the form S0176 requires:
// RFC 3339 at millisecond precision, ending in a numeric offset.
var timeField = regexp.MustCompile(`^time=(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}[+-]\d{2}:\d{2}) `)

// render writes one record through a handler built by To, and returns the line.
func render(t *testing.T, when time.Time, attrs ...slog.Attr) string {
	t.Helper()
	var buf bytes.Buffer
	r := slog.NewRecord(when, slog.LevelInfo, "undo window: restored the original", 0)
	r.AddAttrs(attrs...)
	if err := To(&buf, "info").Handler().Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return buf.String()
}

// timeOf returns the time field's value, failing the test when the line does not start
// with one in the required form.
func timeOf(t *testing.T, line string) string {
	t.Helper()
	m := timeField.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the line does not start with an RFC 3339 millisecond time ending in a numeric "+
			"offset:\n%s", line)
	}
	return m[1]
}

// mustZone loads a zone from the zone database the test runs against.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %s: %v", name, err)
	}
	return loc
}

// TestS0176ACH1_UTCRendersANumericOffset grades AC-H1: a record logged while the zone is
// UTC says `+00:00`, never `Z` (the stock handler's UTC form) and never `-00:00` (RFC 3339's
// "offset unknown").
func TestS0176ACH1_UTCRendersANumericOffset(t *testing.T) {
	line := render(t, time.Date(2026, 7, 1, 12, 34, 56, 789_000_000, time.UTC))
	got := timeOf(t, line)
	if want := "2026-07-01T12:34:56.789+00:00"; got != want {
		t.Errorf("time field = %q, want %q", got, want)
	}
	if strings.HasSuffix(got, "Z") || strings.HasSuffix(got, "-00:00") {
		t.Errorf("time field %q ends in Z or -00:00", got)
	}
}

// TestS0176ACH2_ADaylightSavingZoneRendersItsOffsetAtTheInstant grades AC-H2: a zone that
// moves its clock renders the offset in force at the record's own instant, summer and
// winter. The spec names one particular zone; this public repository uses another zone
// with the same property (a one-hour summer shift), so no deployment's zone is recorded.
func TestS0176ACH2_ADaylightSavingZoneRendersItsOffsetAtTheInstant(t *testing.T) {
	berlin := mustZone(t, "Europe/Berlin")
	for _, tc := range []struct {
		when time.Time
		want string
	}{
		{time.Date(2026, 7, 15, 9, 0, 0, 0, berlin), "2026-07-15T09:00:00.000+02:00"},
		{time.Date(2026, 1, 15, 9, 0, 0, 0, berlin), "2026-01-15T09:00:00.000+01:00"},
	} {
		if got := timeOf(t, render(t, tc.when)); got != tc.want {
			t.Errorf("time field = %q, want %q", got, tc.want)
		}
	}
}

// TestS0176ACH3_OneInstantInTwoZonesIsTheSameInstant grades AC-H3: the same instant written
// under UTC and under a zone with an offset parses back, with a stock RFC 3339 parser, to
// the same instant.
func TestS0176ACH3_OneInstantInTwoZonesIsTheSameInstant(t *testing.T) {
	instant := time.Date(2026, 7, 1, 22, 15, 30, 250_000_000, time.UTC)
	utc := timeOf(t, render(t, instant))
	zoned := timeOf(t, render(t, instant.In(mustZone(t, "Europe/Berlin"))))
	if utc == zoned {
		t.Fatalf("both renderings are %q: the zone did not reach the time field", utc)
	}
	a, err := time.Parse(time.RFC3339Nano, utc)
	if err != nil {
		t.Fatalf("%q does not parse as RFC 3339: %v", utc, err)
	}
	b, err := time.Parse(time.RFC3339Nano, zoned)
	if err != nil {
		t.Fatalf("%q does not parse as RFC 3339: %v", zoned, err)
	}
	if !a.Equal(b) || !a.Equal(instant) {
		t.Errorf("%q and %q do not denote the instant %s", utc, zoned, instant)
	}
}

// TestS0176ACH7_TheLineIsTheStockLineButForTheOffset grades AC-H7: for the same record and
// options, the line is byte for byte the stock text handler's line except the offset
// suffix of the time value. In a zone with an offset the stock handler already writes it,
// so the lines are identical; in UTC they differ in exactly `Z` against `+00:00`. The
// records carry every kind of value, quoting, levels, and attributes before and after a
// group, and a time-valued attribute that must keep the stock rendering.
func TestS0176ACH7_TheLineIsTheStockLineButForTheOffset(t *testing.T) {
	ctx := context.Background()
	opts := &slog.HandlerOptions{Level: Level("debug")}
	build := func(zone *time.Location) []slog.Record {
		when := time.Date(2026, 3, 29, 1, 59, 59, 999_999_999, zone)
		var out []slog.Record
		for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError, slog.LevelWarn + 1} {
			r := slog.NewRecord(when, lvl, `a message with "quotes", spaces and key=value`, 0)
			r.AddAttrs(
				slog.String("path", "/library/a film (2001)/a film.mkv"),
				slog.String("empty", ""),
				slog.Int64("bytes", -42),
				slog.Uint64("free", 1<<40),
				slog.Float64("ratio", 0.125),
				slog.Bool("dry_run", false),
				slog.Duration("took", 1500*time.Millisecond),
				slog.Time("expires", when.Add(24*time.Hour)),
				slog.Any("err", errors.New("rename: invalid cross-device link")),
				slog.Group("gate", slog.String("name", "vmaf"), slog.Time("at", when)),
			)
			out = append(out, r)
		}
		return out
	}
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("", 5*3600+30*60), mustZone(t, "Europe/Berlin")} {
		for _, r := range build(zone) {
			var ours, stock bytes.Buffer
			oh := To(&ours, "debug").Handler().WithAttrs([]slog.Attr{slog.String("cmd", "restore")}).WithGroup("job")
			sh := slog.NewTextHandler(&stock, opts).WithAttrs([]slog.Attr{slog.String("cmd", "restore")}).WithGroup("job")
			if err := oh.Handle(ctx, r.Clone()); err != nil {
				t.Fatal(err)
			}
			if err := sh.Handle(ctx, r.Clone()); err != nil {
				t.Fatal(err)
			}
			want := stock.String()
			if zone == time.UTC {
				stamp, rest, ok := strings.Cut(want, " ")
				if !ok || !strings.HasSuffix(stamp, "Z") {
					t.Fatalf("the stock handler's UTC line does not start with a Z time field:\n%s", want)
				}
				want = strings.TrimSuffix(stamp, "Z") + "+00:00 " + rest
			}
			if ours.String() != want {
				t.Errorf("zone %s, level %s:\n got  %s want %s", zone, r.Level, ours.String(), want)
			}
		}
	}
}
