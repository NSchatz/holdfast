package chapters

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

// film is a 100 s file starting at 0 with three chapters, the shape every case below varies.
func film() probe.ChapterTable {
	return probe.ChapterTable{
		Chapters: []probe.Chapter{
			{Start: 0, End: 30, Title: "Opening"},
			{Start: 30, End: 70, Title: "Middle"},
			{Start: 70, End: 100, Title: "End"},
		},
		StartKnown: true, Duration: 100, DurationKnown: true,
	}
}

// with returns a copy of t with chapter i replaced by c.
func with(t probe.ChapterTable, i int, c probe.Chapter) probe.ChapterTable {
	t.Chapters = append([]probe.Chapter(nil), t.Chapters...)
	t.Chapters[i] = c
	return t
}

// TestCheck_Passes: every pair an encode that damaged nothing can produce passes, and only a
// source's own defects are reported beside the pass.
func TestCheck_Passes(t *testing.T) {
	shorter := film()
	shorter.Duration = 99.5 // a faithful output a frame or two short, inside the length gate's drift
	rebased := film()       // a source whose timeline starts at 5 s, chapters on that timeline
	rebased.StartTime = 5
	for i := range rebased.Chapters {
		rebased.Chapters[i].Start += 5
		rebased.Chapters[i].End += 5
	}
	rebased.Duration = 105 // Matroska's duration counts from 0, so it includes the start
	early := with(film(), 0, probe.Chapter{Start: 0, End: 30, Title: "Opening"})
	early.StartTime = 0.5 // the first chapter begins before the first packet
	earlyOut := with(film(), 0, probe.Chapter{Start: 0, End: 29.5, Title: "Opening"})
	earlyOut.Chapters[1] = probe.Chapter{Start: 29.5, End: 69.5, Title: "Middle"}
	earlyOut.Chapters[2] = probe.Chapter{Start: 69.5, End: 99.5, Title: "End"}
	unknownDur := film()
	unknownDur.DurationKnown = false

	for _, tc := range []struct {
		name     string
		src, out probe.ChapterTable
	}{
		{"identical tables", film(), film()},
		{"no chapters on either side", probe.ChapterTable{}, probe.ChapterTable{}},
		{"no chapters and no start time on either side", probe.ChapterTable{DurationKnown: true, Duration: 5},
			probe.ChapterTable{}},
		{"a millisecond rescale of every time", film(), with(film(), 1, probe.Chapter{Start: 30.001, End: 69.999, Title: "Middle"})},
		{"a start exactly at the tolerance", film(), with(film(), 1, probe.Chapter{Start: 30 + Tolerance, End: 70, Title: "Middle"})},
		{"an end exactly at the tolerance", film(), with(film(), 1, probe.Chapter{Start: 30, End: 70 - Tolerance, Title: "Middle"})},
		{"a last chapter ending on the source's end, the output a little shorter", film(), shorter},
		{"a source rebased to start at 0", rebased, film()},
		{"a chapter before the first packet, rebased to 0 as ffmpeg does", early, earlyOut},
		{"neither duration known", unknownDur, unknownDur},
	} {
		t.Run(tc.name, func(t *testing.T) {
			malformed, err := Check(tc.src, tc.out, 1)
			if err != nil {
				t.Fatalf("Check refused a faithful table: %v", err)
			}
			if len(malformed) != 0 {
				t.Errorf("a well-formed source reported defects: %v", malformed)
			}
		})
	}
}

// TestCheck_Refuses: every way an output's table can differ from its source's is refused,
// and the refusal names the chapter and what differs.
func TestCheck_Refuses(t *testing.T) {
	dropped := film()
	dropped.Chapters = dropped.Chapters[:2]
	noStartOut := film()
	noStartOut.StartKnown = false
	pastEnd := film()
	pastEnd.Duration = 60 // an output whose own end falls inside the chapters the source places in range
	pastEnd.DurationKnown = true
	unknownSrcDur := film()
	unknownSrcDur.DurationKnown = false
	unknownSrcDur.Chapters[2] = probe.Chapter{Start: 70, End: 1000, Title: "End"}
	knownOut := unknownSrcDur
	knownOut.DurationKnown = true
	srcNoStart := film()
	srcNoStart.StartKnown = false

	for _, tc := range []struct {
		name     string
		src, out probe.ChapterTable
		durTol   float64
		want     string
	}{
		{"every chapter dropped", film(), probe.ChapterTable{StartKnown: true},
			0, "the output carries 0 chapter(s) and the source carries 3"},
		{"one chapter dropped", film(), dropped, 0, "the output carries 2 chapter(s) and the source carries 3"},
		{"a chapter added to a source with none", probe.ChapterTable{StartKnown: true}, dropped,
			0, "the output carries 2 chapter(s) and the source carries 0"},
		{"a chapter retitled", film(), with(film(), 1, probe.Chapter{Start: 30, End: 70, Title: "middle"}),
			0, `chapter 2 is titled "middle" in the output and "Middle" in the source`},
		{"a title lost", film(), with(film(), 2, probe.Chapter{Start: 70, End: 100}),
			0, `chapter 3 is titled "" in the output and "End" in the source`},
		{"a start shifted past the tolerance", film(), with(film(), 1, probe.Chapter{Start: 30.11, End: 70, Title: "Middle"}),
			0, `chapter 2 ("Middle") starts at 30.110s in the output and 30.000s in the source`},
		{"a start shifted earlier", film(), with(film(), 1, probe.Chapter{Start: 29, End: 70, Title: "Middle"}),
			0, `chapter 2 ("Middle") starts at 29.000s`},
		{"an end shifted past the tolerance", film(), with(film(), 0, probe.Chapter{Start: 0, End: 30.2, Title: "Opening"}),
			0, `chapter 1 ("Opening") ends at 30.200s in the output and 30.000s in the source`},
		{"an end shifted earlier", film(), with(film(), 0, probe.Chapter{Start: 0, End: 29.8, Title: "Opening"}),
			0, `chapter 1 ("Opening") ends at 29.800s`},
		{"the output reports no start time", film(), noStartOut, 0, "the output reports no start time"},
		{"the source reports no start time", srcNoStart, film(), 0, "the source reports no start time"},
		{"a chapter starting past the output's end", film(), pastEnd,
			20, `chapter 3 ("End") starts at 70.000s, at or after the end of the 60.000s output`},
		{"a chapter ending past the output's end", film(), func() probe.ChapterTable {
			o := film()
			o.Duration = 99.8
			return o
		}(), 0.05, `chapter 3 ("End") ends at 100.000s, after the end of the 99.800s output`},
		{"a source of unknown length waives nothing", unknownSrcDur, knownOut,
			1, `chapter 3 ("End") ends at 1000.000s, after the end of the 100.000s output`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(tc.src, tc.out, tc.durTol)
			if err == nil {
				t.Fatal("Check accepted an output whose chapter table is not the source's")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCheck_RangeAllowanceIsTheLengthGatesDrift: a chapter ending on the source's last frame
// passes an output shorter by up to the tolerance plus the length gate's own drift, and is
// refused one hair beyond it. The source here is well formed, so nothing is waived.
func TestCheck_RangeAllowanceIsTheLengthGatesDrift(t *testing.T) {
	out := film()
	out.Duration = 99
	if _, err := Check(film(), out, 0.9); err != nil {
		t.Errorf("an output 1 s shorter under a 0.9 s drift allowance was refused: %v", err)
	}
	if _, err := Check(film(), out, 0.89); err == nil {
		t.Error("an output 1 s shorter under a 0.89 s drift allowance was accepted")
	}
	at := film()
	at.Chapters[2] = probe.Chapter{Start: 99.95, End: 100, Title: "End"}
	at.Chapters[1].End = 99.95
	atOut := at
	atOut.Duration = 99.95
	if _, err := Check(at, atOut, 1); err == nil || !strings.Contains(err.Error(), "at or after the end") {
		t.Errorf("a chapter starting exactly at the output's end was not refused: %v", err)
	}
}

// TestCheck_MalformedSourceReproducedPasses is the policy for a source whose own table is
// broken: an output that reproduces it exactly passes, each defect is reported, and an output
// that differs from it is still refused.
func TestCheck_MalformedSourceReproducedPasses(t *testing.T) {
	// The table issue #200 describes: one chapter spanning the film and three starting
	// after its end, on an 8700 s file.
	broken := probe.ChapterTable{
		Chapters: []probe.Chapter{
			{Start: 0, End: 8700, Title: "1"},
			{Start: 10000, End: 44000, Title: "2"},
			{Start: 44000, End: 78000, Title: "3"},
			{Start: 78000, End: 90000, Title: "4"},
		},
		StartKnown: true, Duration: 8700, DurationKnown: true,
	}
	malformed, err := Check(broken, broken, 1)
	if err != nil {
		t.Fatalf("an output reproducing the source's own table exactly was refused: %v", err)
	}
	if len(malformed) != 3 {
		t.Fatalf("malformed = %v, want the three chapters past the end", malformed)
	}
	for i, want := range []string{
		`chapter 2 ("2") starts at 10000.000s, at or after the end of the 8700.000s source`,
		`chapter 3 ("3") starts at 44000.000s`,
		`chapter 4 ("4") starts at 78000.000s`,
	} {
		if !strings.Contains(malformed[i], want) {
			t.Errorf("malformed[%d] = %q, want it to contain %q", i, malformed[i], want)
		}
	}

	// The same table, encoded with one of the broken chapters moved: still refused.
	moved := broken
	moved.Chapters = append([]probe.Chapter(nil), broken.Chapters...)
	moved.Chapters[3].Start = 70000
	if _, err := Check(broken, moved, 1); err == nil {
		t.Error("an output that moved a chapter of a malformed source was accepted")
	}

	// A good source whose output grew the defect is refused: the waiver is the SOURCE's.
	good := probe.ChapterTable{
		Chapters:   []probe.Chapter{{Start: 0, End: 8700, Title: "1"}},
		StartKnown: true, Duration: 8700, DurationKnown: true,
	}
	short := good
	short.Duration = 100
	if _, err := Check(good, short, 1); err == nil || !strings.Contains(err.Error(), "chapter range check failed") {
		t.Errorf("an output whose chapter runs 8600 s past its end was not refused by the range check: %v", err)
	}
}

// TestCheck_ReportsEverySourceDefect: each kind of defect in the source's own table is named,
// and only a chapter placed outside the film waives the output's range check.
func TestCheck_ReportsEverySourceDefect(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    []probe.Chapter
		want string
	}{
		{"an end before its start", []probe.Chapter{{Start: 0, End: 50}, {Start: 60, End: 55}},
			"chapter 2 (\"\") ends at 55.000s, before it starts at 60.000s"},
		{"an end past the source's end", []probe.Chapter{{Start: 0, End: 50}, {Start: 50, End: 100.2}},
			"chapter 2 (\"\") ends at 100.200s, after the end of the 100.000s source"},
		{"out of order", []probe.Chapter{{Start: 50, End: 60}, {Start: 10, End: 20}},
			"chapter 2 (\"\") starts at 10.000s, before the chapter ahead of it (50.000s)"},
		{"overlapping", []probe.Chapter{{Start: 0, End: 50}, {Start: 40, End: 60}},
			"chapter 2 (\"\") starts at 40.000s, inside the chapter ahead of it (which ends at 50.000s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := probe.ChapterTable{Chapters: tc.c, StartKnown: true, Duration: 100, DurationKnown: true}
			malformed, err := Check(src, src, 0)
			if err != nil {
				t.Fatalf("an exact reproduction was refused: %v", err)
			}
			if len(malformed) != 1 || malformed[0] != tc.want {
				t.Errorf("malformed = %q, want [%q]", malformed, tc.want)
			}
		})
	}

	// Within the tolerance is not a defect: a last chapter a hair past the end, and a
	// chapter starting a hair inside the end of the one before it.
	sloppy := probe.ChapterTable{
		Chapters:   []probe.Chapter{{Start: 0, End: 50.05}, {Start: 50, End: 100 + Tolerance}},
		StartKnown: true, Duration: 100, DurationKnown: true,
	}
	if malformed, err := Check(sloppy, sloppy, 0); err != nil || len(malformed) != 0 {
		t.Errorf("a table sloppy only within the tolerance: malformed=%v err=%v", malformed, err)
	}

	// An overlap is reported but waives nothing: an output whose overlapping chapter runs past
	// the output's end is still refused.
	overlap := probe.ChapterTable{
		Chapters:   []probe.Chapter{{Start: 0, End: 50}, {Start: 40, End: 100}},
		StartKnown: true, Duration: 100, DurationKnown: true,
	}
	cut := overlap
	cut.Duration = 60
	if _, err := Check(overlap, cut, 0); err == nil {
		t.Error("an overlap in the source waived the range check on an output that ends at 60 s")
	}
}
