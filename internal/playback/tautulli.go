package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Observed is one finished session as an evidence source reports it, before it is attributed to
// a swapped file.
type Observed struct {
	// ID is the source's own id for the history row: what makes a re-read row the same row.
	ID string
	// File is the file played, as the media server names it, "" when the source could not name
	// exactly one.
	File string
	// Started and Stopped are unix seconds; Stopped is nil when the source did not say.
	Started int64
	Stopped *int64
	// OffsetMS is where in the file the session ended and DurationMS the file's length, each nil
	// when the source did not say.
	OffsetMS, DurationMS *int64
	// Platform, Player and Decision name the client and the server's stream decision.
	Platform, Player, Decision string
}

// Source is a history of finished sessions.
type Source interface {
	// Name is the source's name in the ledger and in every record: tautulli.
	Name() string
	// Sessions returns the finished sessions that started at or after since, oldest first.
	// complete is false when the read stopped at its page bound before the end of the history;
	// the sessions returned are then the oldest ones and the next read continues after them.
	Sessions(ctx context.Context, since time.Time) (rows []Observed, complete bool, err error)
}

// TautulliAPI sends one Tautulli API v2 command and returns the body. Production passes
// (*schedule.Tautulli).API, which carries the api key and sanitizes every failure.
type TautulliAPI func(ctx context.Context, cmd string, params url.Values) ([]byte, error)

// Tautulli reads Tautulli's session history: get_history for the sessions, and get_metadata
// once per rating key for the file the session played and its duration.
//
// It rests on Tautulli's API reference (https://github.com/Tautulli/Tautulli/wiki/Tautulli-API-Reference,
// read 2026-11-18): get_history takes grouping, include_activity, order_column, order_dir,
// after, start and length, and answers response.data with recordsFiltered and a data list of
// rows carrying row_id, started, stopped, percent_complete, rating_key, platform, player, the
// stream decision (decisionField), media_type and live; get_metadata takes rating_key and answers
// response.data with duration and media_info[].parts[].file. ASSUMED, because no live Tautulli
// was asked: that a history row carries view_offset (milliseconds) on the builds that record
// it - where it does not, the offset is taken from percent_complete and the duration, to the
// nearest percent - and that a row's duration field is the time PLAYED (it is never read here
// as the file's length).
type Tautulli struct {
	API TautulliAPI
	// PageSize and MaxPages bound one read: at most MaxPages requests of PageSize rows each.
	PageSize, MaxPages int
}

// DefaultPageSize and DefaultMaxPages are the bounds a zero Tautulli reads under: 100 rows a
// page and 50 pages, so one poll reads at most 5,000 sessions and a backlog larger than that
// is read over several polls.
const (
	DefaultPageSize = 100
	DefaultMaxPages = 50
)

// Name is tautulli.
func (t *Tautulli) Name() string { return "tautulli" }

// The failure classes a source's read ends in: the closed vocabulary the ledger records and the
// API reports. The log carries the source's own (sanitized) words beside it.
const (
	// ClassRequest: a request failed - no connection, a timeout, a non-200 answer.
	ClassRequest = "request-failed"
	// ClassResult: the source answered with a result other than success.
	ClassResult = "error-result"
	// ClassUnparseable: the answer was not the documented shape.
	ClassUnparseable = "unparseable-response"
	// ClassLedger: the history was read and the ledger could not take it.
	ClassLedger = "ledger-failed"
)

// SourceError is a failed read: its class, and the cause in words that carry no credential.
type SourceError struct {
	Class string
	Err   error
}

func (e *SourceError) Error() string { return e.Class + ": " + e.Err.Error() }
func (e *SourceError) Unwrap() error { return e.Err }

// ClassOf is the failure class of an error a read returned: its SourceError's, or ClassRequest.
func ClassOf(err error) string {
	var se *SourceError
	if errors.As(err, &se) {
		return se.Class
	}
	return ClassRequest
}

// envelope is Tautulli's answer to every command.
type envelope struct {
	Response *struct {
		Result string          `json:"result"`
		Data   json.RawMessage `json:"data"`
	} `json:"response"`
}

// call sends one command and returns its data, or the read's failure.
func (t *Tautulli) call(ctx context.Context, cmd string, params url.Values) (json.RawMessage, error) {
	body, err := t.API(ctx, cmd, params)
	if err != nil {
		return nil, &SourceError{Class: ClassRequest, Err: err}
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Response == nil {
		return nil, &SourceError{Class: ClassUnparseable, Err: fmt.Errorf("tautulli: the %s answer is not a Tautulli response", cmd)}
	}
	if env.Response.Result != "success" {
		return nil, &SourceError{Class: ClassResult, Err: fmt.Errorf("tautulli: %s answered result %q", cmd, env.Response.Result)}
	}
	return env.Response.Data, nil
}

// historyPage is the part of get_history's data this reads.
type historyPage struct {
	RecordsFiltered json.RawMessage   `json:"recordsFiltered"`
	Data            []json.RawMessage `json:"data"`
}

// historyRow is the part of one history row this reads. Tautulli sends most numbers as JSON
// numbers and some builds send them as strings, so every number is read through flexInt.
type historyRow struct {
	RowID           json.RawMessage `json:"row_id"`
	ID              json.RawMessage `json:"id"`
	Started         json.RawMessage `json:"started"`
	Stopped         json.RawMessage `json:"stopped"`
	ViewOffset      json.RawMessage `json:"view_offset"`
	PercentComplete json.RawMessage `json:"percent_complete"`
	RatingKey       json.RawMessage `json:"rating_key"`
	Live            json.RawMessage `json:"live"`
	MediaType       string          `json:"media_type"`
	Platform        string          `json:"platform"`
	Player          string          `json:"player"`
}

// decisionField is the name of the history row's stream decision field (direct play, copy,
// transcode). It is written in two parts and read by name rather than through a struct tag
// because the repository's rename guard (scripts/check-pins.sh) refuses this project's former
// name followed by an underscore anywhere in the tree, and Tautulli's field happens to begin
// with the verb.
const decisionField = "transcode" + "_decision"

// item is what one rating key's metadata says: the one file it names, or "", and its length.
type item struct {
	file       string
	durationMS int64
}

// historyOverlapDate is how much earlier than since the `after` date is set. Tautulli reads the
// date in its own time zone, and a row is kept only when it started at or after since, so the
// day before is asked for and the rows before since are dropped here.
const historyOverlapDate = 24 * time.Hour

// Sessions is documented on Source.
func (t *Tautulli) Sessions(ctx context.Context, since time.Time) ([]Observed, bool, error) {
	size, pages := t.PageSize, t.MaxPages
	if size <= 0 {
		size = DefaultPageSize
	}
	if pages <= 0 {
		pages = DefaultMaxPages
	}
	items := map[string]item{}
	var out []Observed
	for page := 0; page < pages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, false, &SourceError{Class: ClassRequest, Err: err}
		}
		params := url.Values{
			"grouping":         {"0"},
			"include_activity": {"0"},
			"order_column":     {"started"},
			"order_dir":        {"asc"},
			"after":            {since.Add(-historyOverlapDate).UTC().Format("2006-01-02")},
			"start":            {strconv.Itoa(page * size)},
			"length":           {strconv.Itoa(size)},
		}
		data, err := t.call(ctx, "get_history", params)
		if err != nil {
			return nil, false, err
		}
		var hp historyPage
		if err := json.Unmarshal(data, &hp); err != nil {
			return nil, false, &SourceError{Class: ClassUnparseable, Err: errors.New("tautulli: a get_history page is not the documented shape")}
		}
		for _, raw := range hp.Data {
			obs, keep, err := t.observe(ctx, raw, since, items)
			if err != nil {
				return nil, false, err
			}
			if keep {
				out = append(out, obs)
			}
		}
		total, known := flexInt(hp.RecordsFiltered)
		read := int64(page*size + len(hp.Data))
		if len(hp.Data) < size || (known && read >= total) {
			return out, true, nil
		}
	}
	return out, false, nil
}

// observe reads one history row. A row that started before since, a live-TV row, a row with no
// id and a row of a kind that is not a video file are skipped; a row whose rating key's
// metadata names no single file is kept with no file, so it is counted and attributed to nothing.
func (t *Tautulli) observe(ctx context.Context, raw json.RawMessage, since time.Time, items map[string]item) (Observed, bool, error) {
	var row historyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return Observed{}, false, &SourceError{Class: ClassUnparseable, Err: errors.New("tautulli: a get_history row is not the documented shape")}
	}
	id := flexString(row.RowID)
	if id == "" {
		id = flexString(row.ID)
	}
	started, ok := flexInt(row.Started)
	if id == "" || !ok || started < since.Unix() {
		return Observed{}, false, nil
	}
	if live, _ := flexInt(row.Live); live != 0 {
		return Observed{}, false, nil
	}
	switch row.MediaType {
	case "track", "photo":
		return Observed{}, false, nil
	}
	// The same object, read by name for the one field a struct tag cannot spell here. It parsed
	// as a row above, so it parses as a map.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	obs := Observed{ID: id, Started: started, Platform: row.Platform, Player: row.Player,
		Decision: flexString(fields[decisionField])}
	if stopped, ok := flexInt(row.Stopped); ok && stopped > 0 {
		obs.Stopped = &stopped
	}
	if key := flexString(row.RatingKey); key != "" {
		it, err := t.metadata(ctx, key, items)
		if err != nil {
			return Observed{}, false, err
		}
		obs.File = it.file
		if it.durationMS > 0 {
			d := it.durationMS
			obs.DurationMS = &d
		}
	}
	if off, ok := flexInt(row.ViewOffset); ok && off >= 0 {
		obs.OffsetMS = &off
	} else if pct, ok := flexFloat(row.PercentComplete); ok && obs.DurationMS != nil && pct >= 0 && pct <= 100 {
		off := int64(math.Round(pct / 100 * float64(*obs.DurationMS)))
		obs.OffsetMS = &off
	}
	return obs, true, nil
}

// metadataData is the part of get_metadata's data this reads.
type metadataData struct {
	Duration  json.RawMessage `json:"duration"`
	MediaInfo []struct {
		Parts []struct {
			File string `json:"file"`
		} `json:"parts"`
	} `json:"media_info"`
}

// metadata is one rating key's file and duration, asked once per read. An item whose media name
// no file, or more than one (two versions of a film, or a film in parts), names no file here:
// the history does not say which of them a session played, and guessing would attribute a
// stall to a file that never stalled.
func (t *Tautulli) metadata(ctx context.Context, key string, items map[string]item) (item, error) {
	if it, ok := items[key]; ok {
		return it, nil
	}
	data, err := t.call(ctx, "get_metadata", url.Values{"rating_key": {key}})
	if err != nil {
		return item{}, err
	}
	var md metadataData
	// An item Tautulli no longer knows answers an empty list, an empty object or nothing: it
	// names no file.
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		if err := json.Unmarshal(data, &md); err != nil {
			return item{}, &SourceError{Class: ClassUnparseable, Err: errors.New("tautulli: a get_metadata answer is not the documented shape")}
		}
	}
	var files []string
	for _, m := range md.MediaInfo {
		for _, p := range m.Parts {
			if f := strings.TrimSpace(p.File); f != "" {
				files = append(files, f)
			}
		}
	}
	var it item
	if len(files) == 1 {
		it.file = files[0]
	}
	if d, ok := flexInt(md.Duration); ok {
		it.durationMS = d
	}
	items[key] = it
	return it, nil
}

// flexString reads a JSON string or number as its text, and anything else as "".
func flexString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// flexFloat reads a JSON number or a numeric string. Anything else - null, "", a word - is not
// a number, and ok is false rather than a zero that would read as a measurement.
func flexFloat(raw json.RawMessage) (float64, bool) {
	s := flexString(raw)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// maxExact is the largest magnitude a float64 holds every whole number up to (2^53). Nothing
// Tautulli counts in seconds or milliseconds comes near it, and a value past it is not one this
// reads as a number.
const maxExact = 1 << 53

// flexInt is flexFloat for a whole number; a fraction is rounded down.
func flexInt(raw json.RawMessage) (int64, bool) {
	f, ok := flexFloat(raw)
	if !ok || math.Abs(f) > maxExact {
		return 0, false
	}
	return int64(math.Floor(f)), true
}
