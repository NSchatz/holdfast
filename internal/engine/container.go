package engine

// The container an encode is written in, NAMED on the command line rather than left to
// ffmpeg to infer (S0177).
//
// Given no -f, ffmpeg chooses an output's muxer from the output's NAME: every muxer's
// extension list is matched against it and the first muxer that claims the extension wins.
// The working file beside a source no longer ends in a container extension - it ends in
// TempSuffix, so a media server scanning the library by extension does not offer a
// half-written encode as a film - and for that name ffmpeg can choose nothing at all
// ("Unable to choose an output format"). So every encode this build runs names its
// container, from the container extension the working name still carries ahead of the
// suffix.
//
// The answer has to be the one ffmpeg itself gives for a file named `x.<ext>`, or a working
// file would be written in a container nobody configured - and it is not always the muxer
// the extension looks like: `m4v` is ipod, `wmv` is asf and `vob` is svcd, because the
// first muxer to claim an extension wins and those come first. So outputContainers is
// ffmpeg's own choice for each extension, read off the pinned build and proved against it,
// byte for byte, for every entry (TestS0177AC4_TheWorkingFileIsInTheContainerFFmpegChoosesForItsExtension).
// An extension it does not list is REFUSED with a reason naming it, rather than encoded on
// a guess: a wrong guess is a file in a container the operator never asked for, and no
// gate would object to it.
//
// One muxer reads its output's name for more than the choice of muxer. mpegts writes
// 192-byte BDAV packets (m2ts mode) when, and only when, the name ends in `.m2ts`: its
// mpegts_m2ts_mode option defaults to auto and decides from the name. The m2ts entry
// carries that option, so the working file is byte for byte the file ffmpeg writes for
// `x.m2ts`.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// outputContainer is how an encode names one container on its command line.
type outputContainer struct {
	// muxer is the ffmpeg muxer, passed as -f.
	muxer string
	// options are the muxer options ffmpeg would otherwise have derived from the name.
	options []string
}

// args is the container's half of an encode's argv. Every element is an OUTPUT option.
func (c outputContainer) args() []string {
	return append([]string{"-f", c.muxer}, c.options...)
}

// outputContainers is the pinned ffmpeg's own choice of muxer for a file named `x.<ext>`,
// keyed by the lower-cased extension: ffmpeg's extension match ignores case, and so does
// the lookup. The first nine are the shipped default video_exts, in its order; the rest are
// the other video containers a library carries.
var outputContainers = map[string]outputContainer{
	"mkv":  {muxer: "matroska"},
	"mp4":  {muxer: "mp4"},
	"avi":  {muxer: "avi"},
	"mov":  {muxer: "mov"},
	"m4v":  {muxer: "ipod"},
	"ts":   {muxer: "mpegts"},
	"m2ts": {muxer: "mpegts", options: []string{"-mpegts_m2ts_mode", "1"}},
	"wmv":  {muxer: "asf"},
	"flv":  {muxer: "flv"},

	"webm": {muxer: "webm"},
	"mpg":  {muxer: "mpeg"},
	"mpeg": {muxer: "mpeg"},
	"vob":  {muxer: "svcd"},
	"mts":  {muxer: "mpegts"},
	"m2t":  {muxer: "mpegts"},
	"3gp":  {muxer: "3gp"},
	"3g2":  {muxer: "3g2"},
	"asf":  {muxer: "asf"},
	"ogv":  {muxer: "ogv"},
}

// UnknownContainerError refuses an encode whose output names a container extension this
// build does not know ffmpeg's choice of muxer for. Nothing has been written when it is
// returned: it is decided before any subprocess runs.
type UnknownContainerError struct {
	// Ext is the output container extension as the output's name carries it, "" for a
	// name that carries none.
	Ext string
	// Output is the file the encode would have written.
	Output string
}

func (e *UnknownContainerError) Error() string {
	ext := fmt.Sprintf("the extension %q", e.Ext)
	if e.Ext == "" {
		ext = "a name with no extension"
	}
	return fmt.Sprintf("no output container is known for %s (output %s), so nothing was encoded: every encode "+
		"names the container it writes, and this build knows ffmpeg's own choice only for %s - set container_ext "+
		"to one of those, or take the extension out of video_exts",
		ext, e.Output, strings.Join(knownContainerExts(), ", "))
}

// knownContainerExts is every extension outputContainers names, sorted, for a refusal.
func knownContainerExts() []string {
	out := make([]string, 0, len(outputContainers))
	for ext := range outputContainers {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// containerExtOf is the container extension an encode's output name carries: the one
// ahead of TempSuffix on a working file beside a source, the last one on any other name
// (a scratch working file, or a direct caller's own output).
func containerExtOf(out string) string {
	return strings.TrimPrefix(filepath.Ext(strings.TrimSuffix(filepath.Base(out), TempSuffix)), ".")
}

// outputContainerFor is the container an encode writing out names on its command line,
// or the refusal when there is none to name.
func outputContainerFor(out string) (outputContainer, error) {
	ext := containerExtOf(out)
	c, ok := outputContainers[strings.ToLower(ext)]
	if !ok {
		return outputContainer{}, &UnknownContainerError{Ext: ext, Output: out}
	}
	return c, nil
}
