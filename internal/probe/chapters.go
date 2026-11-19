package probe

import (
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"strconv"
)

// Chapter is one entry of a file's chapter table, as ffprobe reports it: where it starts and
// ends, in seconds on the file's own timeline, and its title ("" where it carries none).
type Chapter struct {
	Start, End float64
	Title      string
}

// ChapterTable is a file's chapter table and the two container facts a chapter is read
// against: where the file's timeline starts, and how long it is.
//
// StartTime is the container's start_time. Chapter times are on the same timeline as the
// packets, so a file whose first packet is at 5 s and whose first chapter is at 5 s has a
// chapter at the start of the film, and that is the position a player shows (and the one
// ffmpeg keeps when it rebases a copy to start at 0). StartKnown is false where ffprobe
// reported no start_time, and nothing is guessed in its place.
//
// Duration is the container's duration, and DurationKnown is false where it reported none
// (the literal "N/A"), which is never read as 0.
type ChapterTable struct {
	Chapters      []Chapter
	StartTime     float64
	StartKnown    bool
	Duration      float64
	DurationKnown bool
}

// Chapters reads f's chapter table AND reports whether ffprobe established it at all. It is
// false when ffprobe could not run, was cancelled, exited non-zero, or printed something
// this reader cannot parse - a chapter whose start or end is not a number included. A table
// that could not be read is unknown, never empty: a gate that read "no answer" as "no
// chapters" would pass an output that lost every chapter whenever both probes failed.
func (p *Prober) Chapters(ctx context.Context, f string) (ChapterTable, bool) {
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-show_chapters",
		"-show_entries", "format=start_time,duration", "-of", "json", "--", f).Output()
	if err != nil || ctx.Err() != nil {
		return ChapterTable{}, false
	}
	return parseChapters(out)
}

// parseChapters reads the JSON Chapters asks ffprobe for. ffprobe prints every chapter's
// start_time and end_time as decimal seconds already scaled from the chapter's own time base
// (nanoseconds in Matroska, the movie timescale in MP4), which is the precision the gate
// needs; a chapter's title is its "title" tag.
func parseChapters(out []byte) (ChapterTable, bool) {
	var doc struct {
		Chapters []struct {
			StartTime string            `json:"start_time"`
			EndTime   string            `json:"end_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
		Format struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return ChapterTable{}, false
	}
	var t ChapterTable
	for _, c := range doc.Chapters {
		start, okS := seconds(c.StartTime)
		end, okE := seconds(c.EndTime)
		if !okS || !okE {
			return ChapterTable{}, false
		}
		t.Chapters = append(t.Chapters, Chapter{Start: start, End: end, Title: c.Tags["title"]})
	}
	t.StartTime, t.StartKnown = seconds(doc.Format.StartTime)
	t.Duration, t.DurationKnown = seconds(doc.Format.Duration)
	return t, true
}

// seconds parses one of ffprobe's decimal time fields. "N/A", an empty field and anything
// that is not a finite number are not a time.
func seconds(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}
