package webui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

// The PRESERVATION graders (S0052 AC8 - AC14).
//
// The budget graders beside them prove the words went. These prove nothing an operator
// needs went with them. holdfast deletes source media after a transcode, and the whole
// safety property is that a source is never destroyed until the replacement is provably
// faithful - so the one way an editorial pass can do real harm is by deleting a fact
// somebody needs in order to trust a swap. Every entry of the specification's
// "Operational facts that must survive" list is asserted here INDIVIDUALLY, against a
// fixture that exercises it, and read back out of the browser rather than out of the
// template.

// --- the fixture every operational fact is asserted against ----------------------

// preservationAggregates is the full set of published figures: two distributions and
// four spreads, each available, each stating the set it covers and the rows it left out.
const preservationAggregates = `{
  "outcomes": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","window":"",
    "counted":21,"excluded":2,"buckets":[{"key":"done","count":12},{"key":"skipped","count":6},{"key":"failed","count":3}]},
  "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row in the ledger","window":"",
    "counted":6,"excluded":1,"buckets":[{"key":"hardlinked","count":4},{"key":"interlaced","count":2}]},
  "size_ratio": {"available":true,"unavailable":"","covers":"every done row that recorded both sizes","window":"",
    "counted":12,"excluded":4,"min":0.19,"mean":0.41,"max":0.88},
  "encode_ms": {"available":true,"unavailable":"","covers":"every done row that recorded an encode duration","window":"",
    "counted":12,"excluded":3,"min":90000,"mean":840000,"max":6120000},
  "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row that recorded a pooled mean","window":"the last 30 days",
    "counted":11,"excluded":5,"min":95.4,"mean":97.9,"max":99.6},
  "vmaf_min": {"available":true,"unavailable":"","covers":"every done row that recorded a worst frame","window":"",
    "counted":11,"excluded":6,"min":78.3,"mean":89.1,"max":95.5}
}`

// preservationSnapshot is AC8's own case, and it exercises every antecedent the criterion
// names at once: a PAUSED run with a scan under way, a queue carrying a pending, a
// probing, an encoding and a verifying row, a history carrying a done, a skipped and a
// failed row, both tables capped by the server, and the full set of aggregates.
func preservationSnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":4,"probing":1,"encoding":1,"verifying":1,"done":12,"skipped":6,"failed":3},
  "queue": [
    {"path":"/media/films/alpha.mkv","status":"pending","worker":"w1","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null},
    {"path":"/media/films/bravo.mkv","status":"probing","worker":"w2","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null},
    {"path":"/media/films/charlie.mkv","status":"encoding","worker":"w3","updated_at":%d,
     "progress_seconds":1500,"progress_duration_seconds":6000,"progress_fraction":0.25},
    {"path":"/media/films/delta.mkv","status":"verifying","worker":"w4","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null}
  ],
  "history": [
    {"path":"/media/films/echo.mkv","status":"done","worker":"w1","updated_at":%d,
     "encoder":"cpu","vmaf_mean":98.24,"vmaf_min":91.53,"vmaf_model":"version=vmaf_v0.6.1",
     "source_bytes":4294967296,"output_bytes":1073741824,"encode_ms":5430000},
    {"path":"/media/films/foxtrot.mkv","status":"skipped","worker":"w2","updated_at":%d,"reason":"hardlinked",
     "encoder":null,"vmaf_mean":null,"vmaf_min":null,"source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/films/golf.mkv","status":"failed","worker":"w3","updated_at":%d,
     "reason":"vmaf worst frame 43.2 below the floor 60",
     "encoder":null,"vmaf_mean":null,"vmaf_min":null,"source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/films/hotel.mkv","status":"done","worker":"w4","updated_at":%d,
     "encoder":null,"vmaf_mean":null,"vmaf_min":null,"vmaf_model":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":19},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":37},
  "bytes_reclaimed_session": 2147483648,
  "bytes_reclaimed_lifetime": 10737418240,
  "paused": true, "scanning": true, "now": %d,
  "aggregates": %s
}`, snapNow-4800, snapNow-300, snapNow-120, snapNow-60,
		snapNow-7200, snapNow-8100, snapNow-9000, snapNow-9600, snapNow, preservationAggregates))
}

// --- AC8: every operational fact, asserted individually --------------------------

// operationalFact is one entry of the specification's own must-survive list, with the
// question the RENDERED page has to answer for it.
type operationalFact struct {
	what  string
	probe func(proseVerdict) []string
}

func present(what, got string) []string {
	if strings.TrimSpace(got) == "" {
		return []string{what + " is not rendered as visible text at all"}
	}
	return nil
}

// onScreen is present() plus the question that catches a value a rule hid: the engine
// must have put this part on the screen, not merely into the document. A check that read
// only the text content would pass a page that shows a reader none of it - which is how
// the operational-fact mutation proof first caught this suite.
func onScreen(what string, p renderedPart) []string {
	if strings.TrimSpace(p.Text) == "" {
		return []string{what + " renders no text at all"}
	}
	if !p.Shown {
		return []string{what + " is in the document but did not reach the screen: " + p.Text}
	}
	return nil
}

func mustSay(what, got string, want ...string) []string {
	if p := present(what, got); p != nil {
		return p
	}
	var out []string
	for _, w := range want {
		if !strings.Contains(got, w) {
			out = append(out, fmt.Sprintf("%s reads %q, which does not carry %q", what, got, w))
		}
	}
	return out
}

// operationalFacts is the list, transcribed one entry per element from the
// specification's "Operational facts that must survive". Each is a separate probe so a
// failure names the fact that went, not "the fixture changed".
func operationalFacts() []operationalFact {
	return []operationalFact{
		{"the connection state, in words", func(v proseVerdict) []string {
			return mustSay("the connection state", v.dash.ConnText, "live")
		}},
		{"whether this holdfast is running or paused", func(v proseVerdict) []string {
			return mustSay("the running/paused badge", v.dash.Badges.Paused, "paused")
		}},
		{"whether a scan is under way", func(v proseVerdict) []string {
			return mustSay("the scan badge", v.dash.Badges.Scan, "scanning")
		}},
		{"reclaimed this run, and reclaimed lifetime", func(v proseVerdict) []string {
			out := mustSay("reclaimed this run", v.dash.ReclaimedSession, "2.0 GB")
			out = append(out, mustSay("reclaimed lifetime", v.dash.ReclaimedLifetime, "10.0 GB")...)
			// Both figures are useless without the words that say WHICH is which.
			for _, label := range []string{"reclaimed this run", "reclaimed lifetime"} {
				if !strings.Contains(v.prose.BodyText, label) {
					out = append(out, fmt.Sprintf("the text a reader can see does not carry the label %q beside its figure", label))
				}
			}
			return out
		}},
		{"the per-status count of files, one figure per status the server published", func(v proseVerdict) []string {
			want := map[string]string{
				"pending": "4", "probing": "1", "encoding": "1", "verifying": "1",
				"done": "12", "skipped": "6", "failed": "3",
			}
			var out []string
			if len(v.dash.Chips) != len(want) {
				out = append(out, fmt.Sprintf("the page shows %d per-status counts, want one per published status (%d)", len(v.dash.Chips), len(want)))
			}
			got := map[string]string{}
			for _, c := range v.dash.Chips {
				got[c.K] = c.N
			}
			for status, n := range want {
				if got[status] != n {
					out = append(out, fmt.Sprintf("the count for %q reads %q, want %q", status, got[status], n))
				}
			}
			return out
		}},
		{"for every queue row: path, status, elapsed, progress, worker", func(v proseVerdict) []string {
			var out []string
			if len(v.dash.Queue) != 4 {
				return []string{fmt.Sprintf("the queue rendered %d rows, want one per pending or active job (4)", len(v.dash.Queue))}
			}
			for i, r := range v.dash.Queue {
				out = append(out, present(fmt.Sprintf("queue row %d's path", i), r.Path)...)
				out = append(out, present(fmt.Sprintf("queue row %d's status", i), r.Status)...)
				out = append(out, present(fmt.Sprintf("queue row %d's elapsed", i), r.Elapsed)...)
				out = append(out, present(fmt.Sprintf("queue row %d's worker", i), r.Worker)...)
				if probs := shownProblems(fmt.Sprintf("queue row %d", i), r.Shown); probs != nil {
					out = append(out, probs...)
				}
			}
			// Each of the five fields REACHED THE SCREEN, cell by cell.
			if len(v.prose.QueueParts) != 4 {
				return append(out, fmt.Sprintf("the classifier read %d queue rows, want 4", len(v.prose.QueueParts)))
			}
			for i, r := range v.prose.QueueParts {
				for _, cell := range []string{"path", "st", "elapsed", "worker"} {
					out = append(out, onScreen(fmt.Sprintf("queue row %d's %s cell", i, cell), r.Cells[cell])...)
				}
				if _, ok := r.Cells["prog"]; !ok {
					out = append(out, fmt.Sprintf("queue row %d renders no progress field at all", i))
				}
			}
			out = append(out, onScreen("the running encode's progress cell", v.prose.QueueParts[2].Cells["prog"])...)
			// Progress is a measurement the ENCODER takes, so it exists for the running
			// encode and for no other state - the repository's own pinned invariant, and
			// the reason the field is asserted here on the row that has one.
			out = append(out, mustSay("the running encode's progress", v.dash.Queue[2].Progress, "25%", "of")...)
			return out
		}},
		{"for every history row: path, result, size, VMAF, encoder, encode duration, last updated", func(v proseVerdict) []string {
			var out []string
			if len(v.dash.History) != 4 {
				return []string{fmt.Sprintf("the history rendered %d rows, want one per terminal job (4)", len(v.dash.History))}
			}
			for i, r := range v.dash.History {
				out = append(out, present(fmt.Sprintf("history row %d's path", i), r.Path)...)
				out = append(out, present(fmt.Sprintf("history row %d's result", i), r.Status)...)
				out = append(out, present(fmt.Sprintf("history row %d's last updated", i), r.Upd)...)
				if probs := shownProblems(fmt.Sprintf("history row %d", i), r.Shown); probs != nil {
					out = append(out, probs...)
				}
			}
			// Every one of the seven fields REACHED THE SCREEN, cell by cell. A result
			// cell hidden by a rule is still in the row's text content, so only this
			// question catches it.
			if len(v.prose.HistoryParts) != 4 {
				return append(out, fmt.Sprintf("the classifier read %d history rows, want 4", len(v.prose.HistoryParts)))
			}
			for i, r := range v.prose.HistoryParts {
				for _, cell := range []string{"path", "st", "upd"} {
					out = append(out, onScreen(fmt.Sprintf("history row %d's %s cell", i, cell), r.Cells[cell])...)
				}
			}
			for _, cell := range []string{"size", "vmaf", "enc", "dur"} {
				out = append(out, onScreen("the done row's "+cell+" cell", v.prose.HistoryParts[0].Cells[cell])...)
				out = append(out, onScreen("the unrecorded done row's "+cell+" cell", v.prose.HistoryParts[3].Cells[cell])...)
			}
			// The row that recorded the proof shows all of it.
			done := v.dash.History[0]
			out = append(out, mustSay("the done row's size", done.Size, "4.0 GB", "smaller")...)
			out = append(out, mustSay("the done row's VMAF", done.Vmaf, "98.2", "91.5", "luma-only")...)
			out = append(out, mustSay("the done row's encoder", done.Enc, "cpu")...)
			out = append(out, mustSay("the done row's encode duration", done.Dur, "1h 30m")...)
			// A skipped row names its guard and a failed one its reason, or the operator
			// has to go and read the logs to learn which of eight guards fired.
			out = append(out, mustSay("the skipped row's result", v.dash.History[1].Status, "hardlinked")...)
			out = append(out, mustSay("the failed row's result", v.dash.History[2].Status, "vmaf worst frame 43.2 below the floor 60")...)
			// And the done row that recorded NOTHING says so in every one of those four
			// fields rather than reading as a zero.
			for what, got := range map[string]string{
				"size": v.dash.History[3].Size, "VMAF": v.dash.History[3].Vmaf,
				"encoder": v.dash.History[3].Enc, "encode duration": v.dash.History[3].Dur,
			} {
				out = append(out, mustSay("the unrecorded done row's "+what, got, "not recorded")...)
			}
			return out
		}},
		{"for every aggregate: its name, its value, the set of rows, and the count excluded", func(v proseVerdict) []string {
			var out []string
			if len(v.dash.Aggregates) != 6 {
				return []string{fmt.Sprintf("the page rendered %d whole-ledger figures, want all 6", len(v.dash.Aggregates))}
			}
			for i, a := range v.dash.Aggregates {
				out = append(out, present(fmt.Sprintf("aggregate %d's name", i), a.Title)...)
				out = append(out, present(fmt.Sprintf("aggregate %q's value", a.Title), a.Value)...)
				out = append(out, mustSay(fmt.Sprintf("aggregate %q's set", a.Title), a.Coverage, "over ")...)
				out = append(out, mustSay(fmt.Sprintf("aggregate %q's exclusions", a.Title), a.Excluded, "excluded")...)
				if probs := shownProblems("aggregate card "+a.Title, a.Shown); probs != nil {
					out = append(out, probs...)
				}
			}
			// And all four parts of every card REACHED THE SCREEN. The coverage and
			// exclusion statements are the whole reason a figure can be trusted, and
			// they are text content whether or not a rule shows them.
			if len(v.prose.AggregateParts) != 6 {
				return append(out, fmt.Sprintf("the classifier read %d figure cards, want 6", len(v.prose.AggregateParts)))
			}
			for i, a := range v.prose.AggregateParts {
				for what, part := range map[string]renderedPart{
					"name": a.Name, "value": a.Value, "set": a.Coverage, "exclusion count": a.Excluded,
				} {
					out = append(out, onScreen(fmt.Sprintf("figure %d's %s", i, what), part)...)
				}
			}
			return out
		}},
		{"the notice that a table is showing a capped subset, and the cap", func(v proseVerdict) []string {
			var out []string
			for _, c := range []struct {
				what string
				cap  dashCap
				want string
			}{
				{"the queue cap notice", v.dash.QueueCap, "Showing the most recent 4 of 19"},
				{"the history cap notice", v.dash.HistCap, "Showing the most recent 4 of 37"},
			} {
				out = append(out, mustSay(c.what, c.cap.Text, c.want, "capped")...)
				if probs := shownProblems(c.what, c.cap.Shown); probs != nil {
					out = append(out, probs...)
				}
			}
			return out
		}},
		{"the control token input and the rescan, pause and resume controls; the filter input", func(v proseVerdict) []string {
			var out []string
			c := v.prose.Controls
			for what, ctl := range map[string]renderedControl{
				"the control token input": c.Token, "the rescan control": c.Rescan,
				"the pause control": c.Pause, "the resume control": c.Resume,
				"the filter input": c.Filter,
			} {
				if !ctl.Present {
					out = append(out, what+" is not in the rendered document at all")
					continue
				}
				if !ctl.Shown {
					out = append(out, what+" is in the document but did not reach the screen")
				}
				if ctl.named() == "" {
					out = append(out, what+" carries no label, text or placeholder, so nothing on the screen says what it is")
				}
			}
			// The token field is a password field and the filter a search field, which is
			// what stops a token being typed into the clear or a filter into a secret.
			if c.Token.Type != "password" {
				out = append(out, fmt.Sprintf("the control token input is type %q, want password", c.Token.Type))
			}
			// Pause is meaningless while already paused, and this fixture is paused.
			if c.Pause.Enabled {
				out = append(out, "the pause control is enabled while the run is already paused")
			}
			if !c.Resume.Enabled {
				out = append(out, "the resume control is disabled while the run is paused")
			}
			return out
		}},
		{"the absence phrase, wherever a rendered field has no value", func(v proseVerdict) []string {
			n := 0
			for _, s := range v.prose.AbsencePhrases {
				if strings.Contains(s, "not recorded") {
					n++
				}
			}
			if n < 4 {
				return []string{fmt.Sprintf("the fixture's unrecorded done row shows the absence phrase %d times, want one per field it never recorded (4)", n)}
			}
			if !strings.Contains(v.prose.BodyText, "not recorded") {
				return []string{"the absence phrase is in the DOM but not in the text a reader can see"}
			}
			return nil
		}},
		{"the AGPL section 13 source offer, character for character", func(v proseVerdict) []string {
			want := offerFor(sourceoffer.Upstream)
			got := v.prose.SourceOffer.Text
			var out []string
			if !v.prose.SourceOffer.Present {
				return []string{"the rendered document carries no source offer at all"}
			}
			if !v.prose.SourceOffer.Shown {
				out = append(out, "the source offer is in the document but did not reach the screen")
			}
			// The offer's own rendering is fixed by internal/sourceoffer, so the exact
			// characters are derived from it rather than restated here.
			for _, w := range []string{
				"This is " + want.Build,
				"free software you may redistribute and modify under " + want.License,
				sourceoffer.Label + ": " + want.SourceURL,
			} {
				if !strings.Contains(got, w) {
					out = append(out, fmt.Sprintf("the rendered source offer does not carry %q\nshown: %q", w, got))
				}
			}
			return out
		}},
	}
}

func TestRendered_EveryOperationalFactSurvivesTheEditorialPass(t *testing.T) {
	b := proseBrowser(t)
	v := renderProse(t, b, proseOpts{snapshot: preservationSnapshot()})
	facts := operationalFacts()
	if len(facts) != 12 {
		t.Fatalf("the must-survive list has %d entries; the specification names 12", len(facts))
	}
	for _, f := range facts {
		t.Run(f.what, func(t *testing.T) {
			for _, p := range f.probe(v) {
				t.Error(p)
			}
		})
	}
	// And the whole point of the item still holds on the same render.
	for _, f := range gradeTotalBudget(v.prose) {
		t.Error(f)
	}
	t.Logf("%s", v.prose)
}

// --- AC9: an aggregate's set and its exclusions, and they are not page copy -------

func TestRendered_AnAggregateStatesItsSetAndItsExclusionsBesideItsFigure(t *testing.T) {
	b := proseBrowser(t)
	v := renderProse(t, b, proseOpts{snapshot: preservationSnapshot()})
	if len(v.dash.Aggregates) != 6 {
		t.Fatalf("the page rendered %d whole-ledger figures, want all 6", len(v.dash.Aggregates))
	}
	want := []struct{ title, covers, excluded string }{
		{"Outcomes", "over every terminal row in the ledger", "2 rows excluded: no recorded value"},
		{"Skips by guard", "over every skipped row in the ledger", "1 row excluded: no recorded value"},
		{"Replacement size", "over every done row that recorded both sizes", "4 rows excluded: no recorded value"},
		{"Encode time", "over every done row that recorded an encode duration", "3 rows excluded: no recorded value"},
		{"VMAF pooled mean", "over every done row that recorded a pooled mean · window: the last 30 days", "5 rows excluded: no recorded value"},
		{"VMAF worst frame", "over every done row that recorded a worst frame", "6 rows excluded: no recorded value"},
	}
	for i, w := range want {
		got := v.dash.Aggregates[i]
		if got.Title != w.title {
			t.Errorf("figure %d is titled %q, want %q", i, got.Title, w.title)
		}
		if got.Coverage != w.covers {
			t.Errorf("figure %q states its set as %q, want %q", w.title, got.Coverage, w.covers)
		}
		if got.Excluded != w.excluded {
			t.Errorf("figure %q states its exclusions as %q, want %q", w.title, got.Excluded, w.excluded)
		}
		// And neither statement is counted against the page-copy budget: they are values
		// the server published, not prose the operator is being asked to read.
		for what, text := range map[string]string{"set": w.covers, "exclusions": w.excluded} {
			if v.prose.isPageCopy(text) {
				t.Errorf("the classifier counted figure %q's %s as page copy: %q", w.title, what, text)
			}
			if v.prose.excludedUnder(text) == "" {
				t.Errorf("the classifier did not see figure %q's %s at all: %q", w.title, what, text)
			}
		}
	}
}

// --- AC10: the absence phrase, and it is not page copy ----------------------------

// absenceSnapshot carries a null, absent or unmeasured value for every field the page
// renders one for.
func absenceSnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":0,"probing":0,"encoding":1,"verifying":0,"done":1,"skipped":0,"failed":0},
  "queue": [
    {"path":"/media/films/india.mkv","status":"encoding","worker":"w1","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null}
  ],
  "history": [
    {"path":"/media/films/juliet.mkv","status":"done","worker":"w1","updated_at":%d,
     "encoder":null,"vmaf_mean":null,"vmaf_min":null,"vmaf_model":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":1},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":1},
  "bytes_reclaimed_session": 0, "bytes_reclaimed_lifetime": 0,
  "paused": false, "scanning": false, "now": %d,
  "aggregates": %s
}`, snapNow-120, snapNow-600, snapNow, brokenAggregates))
}

func TestRendered_AnUnrecordedFieldStillRendersTheAbsencePhraseAndIsNotPageCopy(t *testing.T) {
	b := proseBrowser(t)
	v := renderProse(t, b, proseOpts{snapshot: absenceSnapshot()})

	// Every one of the done row's four outcome fields says so, in words.
	if len(v.dash.History) != 1 {
		t.Fatalf("the fixture rendered %d history rows, want 1", len(v.dash.History))
	}
	h := v.dash.History[0]
	for what, got := range map[string]string{
		"size": h.Size, "VMAF": h.Vmaf, "encoder": h.Enc, "encode duration": h.Dur,
	} {
		if !strings.Contains(got, "not recorded") {
			t.Errorf("the %s of a row that recorded none reads %q, want the absence phrase", what, got)
		}
		for _, lie := range []string{"0 B", "0.0", "0%", "0 ms", "NaN", "undefined", "null"} {
			if strings.Contains(got, lie) {
				t.Errorf("the %s of a row that recorded none reads %q, which carries %q", what, got, lie)
			}
		}
	}
	// A running encode whose encoder has reported nothing usable is the same fact as a
	// field nobody recorded - nobody measured this - so it reads the page's ONE absence
	// phrase and not a figure. That is the repository's own pinned rule (F3), and AC10
	// asks for "the repository's single absence phrase", not a second one for this case.
	if got := v.dash.Queue[0].Progress; !strings.Contains(got, "not recorded") {
		t.Errorf("a running encode with no measurement shows progress %q, want the absence phrase", got)
	}
	// It is in the text a reader can SEE, and the classifier does not count it.
	if !strings.Contains(v.prose.BodyText, "not recorded") {
		t.Error("the absence phrase never reached the text a reader can see")
	}
	if v.prose.isPageCopy("not recorded") {
		t.Error("the classifier counted the absence phrase as page copy")
	}
	for _, f := range gradeTotalBudget(v.prose) {
		t.Error(f)
	}
}

// --- AC11: loading, empty and unreadable, in every data view ----------------------

// dataViews are the views AC11 is asked of. The criterion's own middle trigger is "a
// snapshot carrying no rows", so the views it is defined against are the two that render
// ROWS - and the page's three-state mechanism covers the live counts and the whole-ledger
// figures with the same words, so all four are graded here rather than two. What differs
// between them is only WHICH snapshot leaves each with nothing to show, which is why the
// empty case below is per view: a ledger with no rows leaves the whole-ledger figures with
// six perfectly good figures to draw, and demanding an empty state of that view would be
// demanding the page lie about what it has.
var dataViews = []string{"counts", "queue", "aggs", "history"}

// noFigureSnapshot is a snapshot whose ledger has rows but whose aggregate set is empty,
// which is what leaves the whole-ledger view with nothing of its own to show.
func noFigureSnapshot() []byte {
	return []byte(strings.Replace(string(emptySnapshot()), healthyAggregates, "{}", 1))
}

// viewStateCases are the three states, in the order the criterion names them, with the way
// the world has to be for each. emptyFor overrides the empty case for one named view.
func viewStateCases() []struct {
	name     string
	state    string
	opts     proseOpts
	emptyFor map[string]proseOpts
} {
	return []struct {
		name     string
		state    string
		opts     proseOpts
		emptyFor map[string]proseOpts
	}{
		{name: "before its first snapshot arrives", state: "loading", opts: proseOpts{noSnapshot: true}},
		{name: "a snapshot carrying no rows", state: "empty", opts: proseOpts{snapshot: emptySnapshot()},
			emptyFor: map[string]proseOpts{"aggs": {snapshot: noFigureSnapshot()}}},
		{name: "a snapshot that cannot be read at all", state: "unreadable", opts: proseOpts{badSnapshot: true}},
	}
}

func TestRendered_EveryDataViewShowsLoadingEmptyAndUnreadableStatesDistinctly(t *testing.T) {
	b := proseBrowser(t)
	// words[view][state] is the phrasing that view used for that state, so the three can
	// be required to be distinct WITHIN a view - which is the comparison the criterion
	// asks for, and the one a reader of that view actually makes.
	words := map[string]map[string]string{}
	for _, c := range viewStateCases() {
		readings := map[string]proseReading{"": renderProse(t, b, c.opts).prose}
		for name, o := range c.emptyFor {
			readings[name] = renderProse(t, b, o).prose
		}
		for _, name := range dataViews {
			p, ok := readings[name]
			if !ok {
				p = readings[""]
			}
			got, ok := p.view(name)
			if !ok {
				t.Errorf("%s: the rendered document carries no %q view at all", c.name, name)
				continue
			}
			if got.State != c.state {
				t.Errorf("%s: the %s view is in the %q state, want %q (it reads %q)", c.name, name, got.State, c.state, got.Text)
				continue
			}
			if strings.TrimSpace(got.Text) == "" {
				t.Errorf("%s: the %s view's %s state renders no words at all", c.name, name, c.state)
			}
			if !got.Shown {
				t.Errorf("%s: the %s view's %s state is in the document but did not reach the screen: %q", c.name, name, c.state, got.Text)
			}
			if !strings.Contains(p.BodyText, got.Text) {
				t.Errorf("%s: the %s view's state is in the DOM but not in the text a reader can see", c.name, name)
			}
			if words[name] == nil {
				words[name] = map[string]string{}
			}
			words[name][c.state] = got.Text
		}
		// The budget holds in every one of these renders.
		for _, p := range readings {
			for _, f := range gradeTotalBudget(p) {
				t.Errorf("%s: %s", c.name, f)
			}
			for _, f := range gradeBlockCeiling(p) {
				t.Errorf("%s: %s", c.name, f)
			}
		}
	}
	for _, name := range dataViews {
		seen := map[string]string{}
		for state, text := range words[name] {
			if prev, ok := seen[text]; ok {
				t.Errorf("the %s view says %q for BOTH its %s and its %s state; the three must be distinct", name, text, prev, state)
			}
			seen[text] = state
		}
		if len(seen) != 3 {
			t.Errorf("the %s view produced %d distinct state phrasings, want 3: %v", name, len(seen), words[name])
		}
	}
}

// --- AC12: a severed stream, and the connection text is not page copy -------------

func TestRendered_ASeveredStreamSaysSoInWordsAndKeepsEveryRow(t *testing.T) {
	b := proseBrowser(t)
	v := renderProse(t, b, proseOpts{snapshot: preservationSnapshot(), streamFails: true, mode: "down"})

	if v.dash.ConnText == "live" || strings.TrimSpace(v.dash.ConnText) == "" {
		t.Errorf("the page reports its connection as %q after the stream was severed", v.dash.ConnText)
	}
	if len(strings.Fields(v.dash.ConnText)) == 0 {
		t.Error("the connection state is not rendered in words")
	}
	// Every row and every figure is still on the screen.
	if len(v.dash.Queue) != 4 || len(v.dash.History) != 4 {
		t.Fatalf("the page dropped rows when the stream was severed: %d queue, %d history", len(v.dash.Queue), len(v.dash.History))
	}
	for _, r := range append(append([]dashRow{}, v.dash.Queue...), v.dash.History...) {
		if probs := shownProblems("row "+r.Path+" after the stream was severed", r.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
	}
	if len(v.dash.Aggregates) != 6 {
		t.Errorf("the page dropped %d whole-ledger figures when the stream was severed", 6-len(v.dash.Aggregates))
	}
	if v.dash.ReclaimedLifetime == "" || v.dash.Badges.Paused == "" {
		t.Error("the page dropped a figure from its frame when the stream was severed")
	}
	// And the connection text is not counted against the budget.
	if v.prose.isPageCopy(v.dash.ConnText) {
		t.Errorf("the classifier counted the connection state %q as page copy", v.dash.ConnText)
	}
	if v.prose.excludedUnder(v.dash.ConnText) == "" {
		t.Errorf("the classifier did not see the connection state %q at all", v.dash.ConnText)
	}
	for _, f := range gradeTotalBudget(v.prose) {
		t.Error(f)
	}
}

// --- AC13: a refused control action ------------------------------------------------

func TestRendered_ARefusedControlActionSaysNothingHappenedAndCostsNothingElse(t *testing.T) {
	b := proseBrowser(t)
	v := renderProse(t, b, proseOpts{
		snapshot: preservationSnapshot(), controlStatus: 401, clickRescan: true,
	})

	msg := v.prose.ControlMessage
	if msg == "" {
		t.Fatal("a refused control action rendered no message at all")
	}
	// It says the action did NOT happen BEFORE it says why. The criterion is about the
	// order a reader meets the two facts in, not about a particular phrase, so it is
	// decided over the message's FIRST sentence: an operator who pressed Rescan is asking
	// one question, and a message that opens with an explanation answers it second.
	lead, _, _ := strings.Cut(msg, ". ")
	saysNothingHappened := false
	for _, phrase := range []string{"nothing changed", "nothing happened", "did not happen", "not started"} {
		if strings.Contains(strings.ToLower(lead), phrase) {
			saysNothingHappened = true
		}
	}
	if !saysNothingHappened {
		t.Errorf("the refusal opens with %q; its first sentence must say the action did not happen", lead)
	}
	if !strings.Contains(strings.ToLower(msg), "token") {
		t.Errorf("the refusal reads %q, which does not say why the action was refused", msg)
	}
	if !strings.Contains(v.prose.BodyText, msg) {
		t.Error("the refusal is in the DOM but not in the text a reader can see")
	}
	// Every other view is still rendered.
	if len(v.dash.Queue) != 4 || len(v.dash.History) != 4 || len(v.dash.Aggregates) != 6 {
		t.Errorf("a refused control action cost the page its views: %d queue, %d history, %d figures",
			len(v.dash.Queue), len(v.dash.History), len(v.dash.Aggregates))
	}
	if len(v.dash.Chips) != 7 {
		t.Errorf("a refused control action cost the page its counts: %d", len(v.dash.Chips))
	}
	// And the refusal is not counted against the budget.
	if v.prose.isPageCopy(msg) {
		t.Errorf("the classifier counted the refusal %q as page copy", msg)
	}
	if v.prose.excludedUnder(msg) == "" {
		t.Errorf("the classifier did not see the refusal %q at all", msg)
	}
	for _, f := range gradeTotalBudget(v.prose) {
		t.Error(f)
	}
}

// --- AC14: shortening the page can never be achieved by shortening data ------------

// hostilePath and hostileReason are each well over 300 characters and each carry
// markup-like and non-ASCII characters.
var hostilePath = "/media/films/" +
	strings.Repeat("séquence-très-longue<img src=x onerror=alert(1)>/", 8) +
	"\"><script>alert(2)</scr" + "ipt>—ünïcøde—日本語—файл.mkv"

var hostileReason = "vmaf worst frame 43.2 below the floor 60: " +
	strings.Repeat("<b onmouseover=alert(3)>ffmpeg said «erreur d'entrée/sortie»</b> ", 6) +
	"</td><td onclick=alert(4)>—日本語—"

func hostileValueSnapshot() []byte {
	esc := func(s string) string {
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	}
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":0,"probing":0,"encoding":1,"verifying":0,"done":0,"skipped":0,"failed":1},
  "queue": [
    {"path":"%s","status":"encoding","worker":"w1","updated_at":%d,
     "progress_seconds":600,"progress_duration_seconds":1200,"progress_fraction":0.5}
  ],
  "history": [
    {"path":"%s","status":"failed","worker":"w1","updated_at":%d,"reason":"%s",
     "encoder":null,"vmaf_mean":null,"vmaf_min":null,"source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":1},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":1},
  "bytes_reclaimed_session": 0, "bytes_reclaimed_lifetime": 0,
  "paused": false, "scanning": false, "now": %d,
  "aggregates": %s
}`, esc(hostilePath), snapNow-60, esc(hostilePath), snapNow-600, esc(hostileReason), snapNow, healthyAggregates))
}

func TestRendered_ALongHostileValueRendersCharacterForCharacterAndIsNotPageCopy(t *testing.T) {
	b := proseBrowser(t)
	if len(hostilePath) < 300 {
		t.Fatalf("the hostile path is %d characters, want at least 300", len(hostilePath))
	}
	if len(hostileReason) < 300 {
		t.Fatalf("the hostile failure reason is %d characters, want at least 300", len(hostileReason))
	}

	benign := renderProse(t, b, proseOpts{snapshot: fixtureSnapshot()})
	v := renderProse(t, b, proseOpts{snapshot: hostileValueSnapshot()})

	// Character for character, in both tables.
	for what, got := range map[string][]string{
		"the queue path": v.prose.QueuePaths, "the history path": v.prose.HistoryPaths,
	} {
		if len(got) != 1 || got[0] != hostilePath {
			t.Errorf("%s rendered %q, want the value the server published character for character", what, got)
		}
	}
	if len(v.prose.FailureReasons) != 1 || v.prose.FailureReasons[0] != hostileReason {
		t.Errorf("the failure reason rendered %q, want the value the server published character for character", v.prose.FailureReasons)
	}
	// Nothing in either value became markup.
	if v.prose.ImgCount != 0 {
		t.Errorf("the values introduced %d img element(s) into the rendered document", v.prose.ImgCount)
	}
	if v.prose.HandlerAttrs != 0 {
		t.Errorf("the values introduced %d event-handler attribute(s)", v.prose.HandlerAttrs)
	}
	// A benign render of the same shape has one queue row and one fewer history row, so
	// the element counts are compared through the page's own frame rather than raw: what
	// must hold is that no element the VALUES could have introduced exists.
	if v.prose.ElementCount > benign.prose.ElementCount {
		t.Errorf("the hostile render has %d elements against a benign render's %d",
			v.prose.ElementCount, benign.prose.ElementCount)
	}
	// And neither value is counted against the budget: shortening the page can never be
	// achieved by shortening data.
	for what, value := range map[string]string{"the media path": hostilePath, "the failure reason": hostileReason} {
		if v.prose.isPageCopy(value) {
			t.Errorf("the classifier counted %s as page copy", what)
		}
	}
	for _, f := range gradeTotalBudget(v.prose) {
		t.Error(f)
	}
	for _, f := range gradeBlockCeiling(v.prose) {
		t.Error(f)
	}
}
