// Package subtitle copies a job's carried text subtitle streams to sidecar files beside its
// replacement, under `subtitle_sidecars: text`. The rules and their reasoning are in
// docs/design/subtitles.md (#sidecars, #sidecar-gate); this package is their one
// implementation.
//
// A sidecar is an ADDED file. Nothing here renames, moves, deletes or overwrites anything:
// a name already on disk is skipped, and the final name is only ever created by link(2),
// which fails on an existing target. Nothing appears under a final name until the stream has
// been extracted to a temp, parsed back and counted, and made durable.
package subtitle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The reason tokens a record carries where no sidecar was published. They are a WIRE FORMAT:
// written onto a terminal row, read back by later builds and printed by `holdfast export`.
// Add to them freely; renaming one changes what a stored row means.
const (
	// SkipBitmap: the stream is a picture-based subtitle (PGS, DVD, DVB, XSUB). It has no
	// text to copy and no text sidecar format to copy it into.
	SkipBitmap = "bitmap-subtitle"
	// SkipMovText: an MP4 timed-text stream. Its only text sidecar is a conversion, and
	// this build copies, never converts (brief I17).
	SkipMovText = "mov-text-not-converted"
	// SkipUnsupported: any other subtitle codec. Nothing is guessed about it.
	SkipUnsupported = "subtitle-codec-not-supported"
	// SkipLanguageTag: the stream's language tag is not something a file name can carry
	// (anything but a 2-3 letter code with optional hyphenated subtags).
	SkipLanguageTag = "language-tag-not-a-name"
	// SkipExists: a file already sits at the sidecar's name. It is never overwritten.
	SkipExists = "sidecar-exists-never-overwritten"
	// SkipNameTaken: an earlier stream of the same job already took this name (same
	// language, same forced flag, same format).
	SkipNameTaken = "sidecar-name-taken-by-earlier-stream"

	// FailSourceCount: the source stream's event count could not be established, so the
	// parse-back gate has nothing to compare against.
	FailSourceCount = "source-event-count-unreadable"
	// FailExtract: the stream could not be copied out of the source.
	FailExtract = "sidecar-extract-failed"
	// FailParseBack: the written file does not parse back as one stream of its format.
	FailParseBack = "sidecar-parse-back-failed"
	// FailEventCount: the written file parses back with a different event count from the
	// source stream's.
	FailEventCount = "sidecar-event-count-mismatch"
	// FailPublish: the gated file could not be made durable or linked under its name.
	FailPublish = "sidecar-publish-failed"
	// FailNotPublished: the job ended without a committed swap, so the gated file was
	// removed. Recorded only by a caller that records a row for such a job.
	FailNotPublished = "swap-not-committed"
)

// IsFailure reports whether a token names a failure (something went wrong) rather than a
// decision (the stream is not one this build writes a sidecar for, or its name is taken).
func IsFailure(token string) bool {
	switch token {
	case FailSourceCount, FailExtract, FailParseBack, FailEventCount, FailPublish, FailNotPublished:
		return true
	}
	return false
}

// Format is a text subtitle format a stream is copied into: ffprobe's codec name for it,
// the ffmpeg muxer (and demuxer) of its native file format, and the sidecar extension.
type Format struct {
	Codec string
	Muxer string
	Ext   string
}

// formats is the closed set copied to a sidecar. Each is a stream copy into the codec's own
// native file format and nothing else: a copy across formats is refused by the muxer, and a
// conversion is not a copy. At https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 (read
// 2026-10-01): libavformat/srtenc.c:42-48 refuses any codec but SubRip (and plain text);
// webvttenc.c:108-112 and assenc.c:239-242 declare WebVTT and ASS as their one subtitle
// codec under FF_OFMT_FLAG_ONLY_DEFAULT_CODECS.
var formats = map[string]Format{
	"subrip": {Codec: "subrip", Muxer: "srt", Ext: "srt"},
	"ass":    {Codec: "ass", Muxer: "ass", Ext: "ass"},
	"webvtt": {Codec: "webvtt", Muxer: "webvtt", Ext: "vtt"},
}

// bitmapCodecs are the picture-based subtitle codecs ffprobe names.
var bitmapCodecs = map[string]bool{
	"hdmv_pgs_subtitle": true,
	"dvd_subtitle":      true,
	"dvb_subtitle":      true,
	"xsub":              true,
}

// Classify answers what a subtitle stream of this codec becomes: the Format it is copied
// into, or the token saying why it is not copied. Exactly one of the two is set.
func Classify(codec string) (Format, string) {
	c := strings.ToLower(strings.TrimSpace(codec))
	if f, ok := formats[c]; ok {
		return f, ""
	}
	switch {
	case bitmapCodecs[c]:
		return Format{}, SkipBitmap
	case c == "mov_text":
		return Format{}, SkipMovText
	}
	return Format{}, SkipUnsupported
}

// languageName is what a language tag must look like to be written into a file name: a 2-3
// letter code, optionally followed by hyphenated subtags (`en`, `eng`, `pt-br`). Anything
// else - a path separator, a dot, a space, a word - is refused rather than cleaned, because
// a cleaned tag is a language the source did not write.
var languageName = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{1,8})*$`)

// Undetermined is the code a sidecar of an untagged stream is named with (ISO 639-2 `und`).
const Undetermined = "und"

// LanguageToken is the `<lang>` part of a sidecar name for a stream tagged lang: the tag as
// the source wrote it, trimmed and lowercased; an absent tag is `und`. ok is false where the
// tag cannot be a name part.
func LanguageToken(lang string) (token string, ok bool) {
	l := strings.ToLower(strings.TrimSpace(lang))
	if l == "" {
		return Undetermined, true
	}
	if !languageName.MatchString(l) {
		return "", false
	}
	return l, true
}

// Name is a sidecar's file name: `<stem>.<lang>[.forced].<ext>`, the form Sonarr, Radarr and
// Jellyfin parse (docs/design/subtitles.md#sidecars cites each).
func Name(stem, lang string, forced bool, ext string) string {
	n := stem + "." + lang
	if forced {
		n += ".forced"
	}
	return n + "." + ext
}

// fontExts and fontMimes recognise a font attachment, by its file name or its mimetype.
var (
	fontExts  = map[string]bool{".ttf": true, ".otf": true, ".ttc": true, ".otc": true, ".woff": true, ".woff2": true}
	fontMimes = map[string]bool{
		"application/x-truetype-font": true, "application/x-font-ttf": true, "application/x-font-otf": true,
		"application/x-font-opentype": true, "application/vnd.ms-opentype": true, "application/font-sfnt": true,
		"application/x-font": true, "application/font-woff": true,
	}
)

// IsFont reports whether an attachment stream is a font.
func IsFont(s probe.Stream) bool {
	if s.Type != probe.TypeAttachment {
		return false
	}
	m := strings.ToLower(s.MimeType)
	if strings.HasPrefix(m, "font/") || fontMimes[m] {
		return true
	}
	return fontExts[strings.ToLower(filepath.Ext(s.Filename))]
}

// facts is what the source probe established about one subtitle stream.
type facts struct {
	forced bool
	events int
	known  bool // events was read
}

// sourceEntries is the one -show_entries the source probe issues.
const sourceEntries = "stream=index,codec_name,nb_read_packets:stream_disposition=forced"

type probed struct {
	Streams []struct {
		Index       int    `json:"index"`
		CodecName   string `json:"codec_name"`
		Packets     string `json:"nb_read_packets"`
		Disposition struct {
			Forced int `json:"forced"`
		} `json:"disposition"`
	} `json:"streams"`
}

// probeSource reads every subtitle stream's forced disposition and event (packet) count in
// one ffprobe pass over the source.
func probeSource(ctx context.Context, ffprobe, src string) (map[int]facts, error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-count_packets", "-select_streams", "s",
		"-show_entries", sourceEntries, "-of", "json", "-i", src).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe the source's subtitle streams: %w", err)
	}
	var p probed
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("read ffprobe's answer: %w", err)
	}
	m := make(map[int]facts, len(p.Streams))
	for _, s := range p.Streams {
		n, err := strconv.Atoi(s.Packets)
		m[s.Index] = facts{forced: s.Disposition.Forced == 1, events: n, known: err == nil && n >= 0}
	}
	return m, nil
}

// countEvents parses a written sidecar back with ffprobe, reading it as the format it was
// written in, and returns its one stream's codec and event (packet) count.
func countEvents(ctx context.Context, ffprobe, path, demuxer string) (codec string, n int, err error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-count_packets",
		"-show_entries", "stream=codec_name,nb_read_packets", "-of", "json", "-f", demuxer, "-i", "file:"+path).Output()
	if err != nil {
		return "", 0, fmt.Errorf("ffprobe could not parse it: %w", err)
	}
	var p probed
	if err := json.Unmarshal(out, &p); err != nil {
		return "", 0, fmt.Errorf("read ffprobe's answer: %w", err)
	}
	if len(p.Streams) != 1 {
		return "", 0, fmt.Errorf("it parses as %d streams, want 1", len(p.Streams))
	}
	n, err = strconv.Atoi(p.Streams[0].Packets)
	if err != nil {
		return "", 0, fmt.Errorf("its event count is %q, not a number", p.Streams[0].Packets)
	}
	return p.Streams[0].CodecName, n, nil
}

// ExtractArgs is the ffmpeg command line that copies stream index of src into dst in the
// muxer's format: a stream copy, nothing re-encoded.
func ExtractArgs(src string, index int, muxer, dst string) []string {
	return []string{"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-i", src, "-map", "0:" + strconv.Itoa(index), "-c", "copy", "-f", muxer, "file:" + dst}
}

// Request is one job's sidecar work.
type Request struct {
	FFmpeg, FFprobe string
	// Source is the file the streams are copied out of.
	Source string
	// Dir is the directory the sidecars are published in, and Stem the replacement's own
	// name without its extension.
	Dir, Stem string
	// Streams is every stream the job's replacement carries (its intended stream map). The
	// subtitle ones are considered; the attachments are read for fonts.
	Streams []probe.Stream
	// TempPath is the temp the i-th extracted stream is written to. It must be in Dir (a
	// link(2) does not cross filesystems) and must not look like a subtitle or video file.
	TempPath func(i int) string
	// Perm is the permission the published sidecar gets: the source's, without execute.
	Perm os.FileMode
	// AfterExtract, when non-nil, is called with each temp after its extraction and before
	// its gate. A test seam; nil in production.
	AfterExtract func(temp string)
}

type pending struct {
	rec         int
	temp, final string
}

// Prepared is a job's gated sidecars, held at their temps until the swap commits.
type Prepared struct {
	records []store.Sidecar
	pending []pending
}

// Prepare extracts and gates every sidecar the request's streams call for, leaving each one
// that passes at its temp. Nothing is created under a final name. A job whose streams carry
// no subtitle runs no probe and writes nothing.
func Prepare(ctx context.Context, r Request) *Prepared {
	p := &Prepared{records: []store.Sidecar{}}
	var subs []probe.Stream
	fonts := false
	for _, s := range r.Streams {
		switch {
		case s.Type == probe.TypeSubtitle:
			subs = append(subs, s)
		case IsFont(s):
			fonts = true
		}
	}
	if len(subs) == 0 {
		return p
	}
	known, perr := probeSource(ctx, r.FFprobe, r.Source)
	chosen := map[string]bool{}
	ordinal := 0
	for _, s := range subs {
		f := known[s.Index]
		rec := store.Sidecar{Index: s.Index, Codec: s.Codec, Language: s.Language, Forced: f.forced}
		p.records = append(p.records, rec)
		at := len(p.records) - 1
		skip := func(token, detail string) {
			p.records[at].Skipped, p.records[at].Detail = token, detail
		}
		format, why := Classify(s.Codec)
		if why != "" {
			skip(why, "")
			continue
		}
		lang, ok := LanguageToken(s.Language)
		if !ok {
			skip(SkipLanguageTag, "")
			continue
		}
		name := Name(r.Stem, lang, f.forced, format.Ext)
		final := filepath.Join(r.Dir, name)
		if chosen[name] {
			skip(SkipNameTaken, "")
			continue
		}
		chosen[name] = true
		if _, err := os.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
			skip(SkipExists, "")
			continue
		}
		p.records[at].FontsLost = fonts && format.Codec == "ass"
		if perr != nil || !f.known {
			skip(FailSourceCount, errText(perr))
			continue
		}
		p.records[at].Events = ptr(f.events)
		temp := r.TempPath(ordinal)
		ordinal++
		if err := r.extract(ctx, s.Index, format, temp); err != nil {
			_ = os.Remove(temp)
			skip(FailExtract, err.Error())
			continue
		}
		if r.AfterExtract != nil {
			r.AfterExtract(temp)
		}
		if token, err := gate(ctx, r.FFprobe, temp, format, f.events); err != nil {
			_ = os.Remove(temp)
			skip(token, err.Error())
			continue
		}
		p.pending = append(p.pending, pending{rec: at, temp: temp, final: final})
	}
	return p
}

// extract claims temp exclusively (it never writes over a file it did not create) and
// copies the stream into it.
func (r Request) extract(ctx context.Context, index int, format Format, temp string) error {
	fh, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("claim the temp: %w", err)
	}
	_ = fh.Close()
	if out, err := exec.CommandContext(ctx, r.FFmpeg, ExtractArgs(r.Source, index, format.Muxer, temp)...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gate is the parse-back gate (docs/design/subtitles.md#sidecar-gate): the written file
// parses back as exactly one stream of its own format, and its event count equals the
// source stream's. It returns the failure token beside the error.
func gate(ctx context.Context, ffprobe, temp string, format Format, want int) (string, error) {
	codec, got, err := countEvents(ctx, ffprobe, temp, format.Muxer)
	if err != nil {
		return FailParseBack, err
	}
	if codec != format.Codec {
		return FailParseBack, fmt.Errorf("it parses back as %q, want %q", codec, format.Codec)
	}
	if got != want {
		return FailEventCount, fmt.Errorf("source stream has %d events, sidecar has %d", want, got)
	}
	return "", nil
}

// Temps is every gated sidecar still held at its temp.
func (p *Prepared) Temps() []string {
	var t []string
	for _, q := range p.pending {
		t = append(t, q.temp)
	}
	return t
}

// Publish puts every gated sidecar under its final name, by link(2) from its temp, and
// removes the temps. It runs only after the swap has committed. A name that appeared since
// Prepare is skipped, never overwritten; a sidecar that cannot be made durable or linked is
// recorded as a failure. It returns the job's records.
func (p *Prepared) Publish(perm os.FileMode) []store.Sidecar {
	for _, q := range p.pending {
		rec := &p.records[q.rec]
		if err := publish(q.temp, q.final, perm); err != nil {
			if errors.Is(err, fs.ErrExist) {
				rec.Skipped = SkipExists
			} else {
				rec.Skipped, rec.Detail = FailPublish, err.Error()
			}
		} else {
			rec.Path = q.final
		}
		_ = os.Remove(q.temp)
	}
	p.pending = nil
	return p.Records()
}

// publish makes temp durable with its final permission and links it to final. The link
// fails on an existing final, which is what makes "never overwritten" true at the moment of
// creation and not only at the moment of the check.
func publish(temp, final string, perm os.FileMode) error {
	fh, err := os.Open(temp)
	if err != nil {
		return err
	}
	err = fh.Chmod(perm)
	if err == nil {
		err = fh.Sync()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Link(temp, final)
}

// Discard removes every gated sidecar still at its temp and records each as not published.
// It is what every way out of a job that did not reach Publish runs; after Publish it does
// nothing.
func (p *Prepared) Discard() {
	for _, q := range p.pending {
		_ = os.Remove(q.temp)
		p.records[q.rec].Skipped = FailNotPublished
	}
	p.pending = nil
}

// Records is the job's records, in the source's stream order.
func (p *Prepared) Records() []store.Sidecar {
	return append([]store.Sidecar{}, p.records...)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func ptr[T any](v T) *T { return &v }
