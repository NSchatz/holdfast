package probe

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// Stream is ONE stream of a container as a stream SELECTION needs to see it: where it
// sits, what kind it is, what language the source tagged it with, and the two
// dispositions that decide whether it is artwork or a commentary track.
//
// It is what an intended stream map is derived from, so every field here is a FACT THE
// SOURCE CARRIES and never an interpretation of one. In particular Commentary is the
// container's own `comment` disposition: a title, a stream name or a filename is not a
// commentary flag, and nothing in this package reads one.
type Stream struct {
	// Index is the stream's absolute index in the container, which is what an ffmpeg
	// `-map 0:<index>` addresses and what an operator reading a probe sees.
	Index int
	// Type is ffprobe's codec_type verbatim: "video", "audio", "subtitle",
	// "attachment", "data".
	Type string
	// Language is the `language` stream tag exactly as the source spells it, lowercased
	// but otherwise untouched. "" is an ABSENT tag, which is not the same fact as the
	// undefined code "und" even though a selection treats them the same way - the
	// distinction is kept here so a record of a dropped stream reports what the source
	// actually said.
	Language string
	// Commentary is the container's own `comment` disposition.
	Commentary bool
	// AttachedPicture is the container's own `attached_pic` disposition: cover art
	// carried as a one-frame video stream.
	AttachedPicture bool
}

// The codec_type values this build names. They are ffprobe's own spelling and are
// treated as a wire format: a selection, a ledger record and the documentation all use
// these tokens.
const (
	TypeVideo      = "video"
	TypeAudio      = "audio"
	TypeSubtitle   = "subtitle"
	TypeAttachment = "attachment"
	TypeData       = "data"
)

// StreamEntries is the single -show_entries argument the stream-list probe issues. It is
// spelled once and exported so nothing has to restate it: a caller that needed to name the
// probe (a test driving the unenumerable path, say) names THIS rather than a copy that
// would go on matching after the probe itself moved.
const StreamEntries = "stream=index,codec_type:stream_disposition=attached_pic,comment:stream_tags=language"

// probeStreams is the JSON shape ffprobe answers with. It is JSON and not the csv the
// scalar probes use for one reason: a csv row omits an entry the stream does not carry,
// so a file whose second audio track has no language tag prints a SHORTER row and every
// column after it shifts. A shifted column is a language read off the wrong stream, on
// the probe an intended stream map is derived from.
type probeStreams struct {
	Streams []struct {
		Index       int    `json:"index"`
		CodecType   string `json:"codec_type"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
			Comment     int `json:"comment"`
		} `json:"disposition"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
}

// Streams returns every stream the file carries, in container order, AND reports whether
// ffprobe ESTABLISHED that answer at all.
//
// The two results are separate for the same reason VideoStreams splits its own: a caller
// deciding which streams a replacement must carry before the source is DELETED has to
// tell "this file carries these streams" from "I could not find out what this file
// carries", and a slice that is empty or short in both cases collapses them. established
// is false whenever ffprobe could not be run, was cancelled, exited non-zero, or answered
// something this parser cannot read - every one of which leaves the shape of the file
// unknown, and an unknown shape must fail safe rather than default to the common one.
//
// It is ONE ffprobe call and it is never taken eagerly: a file that skips at one of the
// cheap source guards must not pay for it (see VideoProps.AllStreams, which is where the
// engine reaches it and which memoises it).
func (p *Prober) Streams(ctx context.Context, f string) (streams []Stream, established bool) {
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error",
		"-show_entries", StreamEntries, "-of", "json", "--", f).Output()
	if err != nil || ctx.Err() != nil {
		return nil, false
	}
	var parsed probeStreams
	if jerr := json.Unmarshal(out, &parsed); jerr != nil {
		return nil, false
	}
	for _, s := range parsed.Streams {
		if strings.TrimSpace(s.CodecType) == "" {
			// A stream whose KIND ffprobe did not name cannot be selected on and cannot
			// be checked for, so the whole answer is unknown rather than partly known.
			return nil, false
		}
		streams = append(streams, Stream{
			Index:           s.Index,
			Type:            s.CodecType,
			Language:        strings.ToLower(strings.TrimSpace(s.Tags["language"])),
			Commentary:      s.Disposition.Comment == 1,
			AttachedPicture: s.Disposition.AttachedPic == 1,
		})
	}
	return streams, true
}

// VideoStreamHashes returns a hash of the BITSTREAM of each of the file's video streams,
// in container order, and whether ffprobe's sibling ffmpeg established them.
//
// It exists for one question: did a stream-copy actually copy? A remux declines the
// perceptual gate, so the property VMAF proved has to be established another way, and
// "every video stream this output carries is byte-identical to the stream it came from"
// is strictly stronger than a perceptual score. The hash is taken over the packets the
// streamhash muxer writes with `-c copy`, so it is the stream's own bytes and not the
// container's framing - two containers of the same copied stream hash the same.
//
// established is false whenever ffmpeg could not be run, was cancelled, exited non-zero,
// or printed a line this parser cannot read. A caller must treat that as "not
// established" and REJECT, never as "identical": a remux whose identity could not be
// checked is a remux with no gate in front of it at all.
func (p *Prober) VideoStreamHashes(ctx context.Context, f string) (hashes []string, established bool) {
	out, err := exec.CommandContext(ctx, p.FFmpeg, "-hide_banner", "-nostdin", "-v", "error",
		"-i", f, "-map", "0:v", "-c", "copy", "-f", "streamhash", "-hash", "md5", "-").Output()
	if err != nil || ctx.Err() != nil {
		return nil, false
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// `<stream index>,<type>,<hash name>=<hash>` - the streamhash muxer's own format.
		fields := strings.SplitN(line, ",", 3)
		if len(fields) != 3 {
			return nil, false
		}
		_, digest, ok := strings.Cut(fields[2], "=")
		if !ok || strings.TrimSpace(digest) == "" {
			return nil, false
		}
		hashes = append(hashes, strings.TrimSpace(digest))
	}
	if len(hashes) == 0 {
		// A file with no video stream at all, or a muxer that printed nothing. Either way
		// nothing was established about the video this output carries.
		return nil, false
	}
	return hashes, true
}
