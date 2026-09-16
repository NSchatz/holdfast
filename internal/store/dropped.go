package store

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DroppedStream is one source stream a job selected away: where it sat, what kind it was,
// and what language the source tagged it with.
//
// The bytes of a dropped stream are not recoverable from the replacement, so this record
// is the only evidence there is that they were ever there. Language is the tag AS THE
// SOURCE SPELLED IT and is "" when the source carried none - never a guess, and never the
// code some later build would have inferred.
type DroppedStream struct {
	Index    int
	Type     string
	Language string
}

// DroppedStreams is what ONE job recorded about the streams it dropped.
//
// Absence is REPRESENTABLE and is not an empty set, which is the whole reason this is a
// type rather than a slice. A row written before the column existed recorded NOTHING, and
// "nothing recorded" must never read as "dropped nothing" - one of those is a statement
// about a job and the other is the absence of one, and on the one table whose entire job
// is to be evidence they have to stay apart all the way into the column, where not
// recorded is NULL.
type DroppedStreams struct {
	list     []DroppedStream
	recorded bool
}

// RecordDroppedStreams records list as what a job dropped. An EMPTY list is a real record
// and not an absence: a job that applied a selection and dropped nothing has said
// something, and saying it is what distinguishes that job from every row written before
// this build existed.
func RecordDroppedStreams(list []DroppedStream) DroppedStreams {
	return DroppedStreams{list: append([]DroppedStream(nil), list...), recorded: true}
}

// Recorded reports whether this row recorded anything at all. The zero DroppedStreams is
// NOT RECORDED, which is what every row written before the column existed carries.
func (d DroppedStreams) Recorded() bool { return d.recorded }

// Streams returns the dropped streams, in the order they were recorded. It is empty both
// for a row that recorded nothing and for one that recorded dropping nothing; Recorded
// is what tells those apart, and every reader must ask it.
func (d DroppedStreams) Streams() []DroppedStream {
	return append([]DroppedStream(nil), d.list...)
}

// Len is how many streams this record names.
func (d DroppedStreams) Len() int { return len(d.list) }

// noStreamsDropped is what a job that dropped NOTHING stores. It has to be a non-empty
// token because "" is how this package spells NULL in every other column (nullString), so
// an empty encoding would land in the database as "not recorded" and claim that a job
// which applied a selection had never been asked about it. It can never collide with a
// real encoding: every encoded stream carries two ":" separators and this carries none.
const noStreamsDropped = "none"

// Encode renders the record as the single TEXT value the column holds: `index:type:lang`
// per stream, ";"-joined in the recorded order, or the empty string when nothing was
// recorded (which nullString then stores as NULL).
//
// Every part is escaped, on the same reasoning DecisionInputs.Encode escapes its pairs: a
// language tag is whatever an operator's muxer wrote, and an unescaped ";" or ":" in one
// would silently split into a stream that was never dropped. QueryEscape leaves every
// value this build actually writes (eng, jpn, audio, subtitle) untouched, so the stored
// text stays something an operator can read.
func (d DroppedStreams) Encode() string {
	if !d.recorded {
		return ""
	}
	if len(d.list) == 0 {
		return noStreamsDropped
	}
	parts := make([]string, 0, len(d.list))
	for _, s := range d.list {
		parts = append(parts, strconv.Itoa(s.Index)+":"+
			url.QueryEscape(s.Type)+":"+url.QueryEscape(s.Language))
	}
	return strings.Join(parts, ";")
}

// ParseDroppedStreams is Encode's inverse: the column's text back into a record.
//
// Anything it cannot read is NOT RECORDED, which is the fail-safe direction here as it is
// for the decision inputs: a record nothing can interpret must not be reported as a list
// of streams somebody can act on, and it must not be reported as "dropped nothing"
// either.
func ParseDroppedStreams(s string) DroppedStreams {
	if s == "" {
		return DroppedStreams{}
	}
	if s == noStreamsDropped {
		return RecordDroppedStreams(nil)
	}
	list := make([]DroppedStream, 0, strings.Count(s, ";")+1)
	for _, part := range strings.Split(s, ";") {
		fields := strings.SplitN(part, ":", 3)
		if len(fields) != 3 {
			return DroppedStreams{}
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil {
			return DroppedStreams{}
		}
		typ, err := url.QueryUnescape(fields[1])
		if err != nil {
			return DroppedStreams{}
		}
		lang, err := url.QueryUnescape(fields[2])
		if err != nil {
			return DroppedStreams{}
		}
		list = append(list, DroppedStream{Index: idx, Type: typ, Language: lang})
	}
	return RecordDroppedStreams(list)
}

// String renders the record for a log line or a human-readable report. It states NOT
// RECORDED in as many words rather than printing nothing, because a blank beside a label
// is exactly the ambiguity this type exists to remove.
func (d DroppedStreams) String() string {
	if !d.recorded {
		return "not recorded"
	}
	if len(d.list) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(d.list))
	for _, s := range d.list {
		lang := s.Language
		if lang == "" {
			lang = "no language tag"
		}
		parts = append(parts, fmt.Sprintf("%d:%s (%s)", s.Index, s.Type, lang))
	}
	return strings.Join(parts, ", ")
}
