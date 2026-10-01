// Package dynhdr is what carrying dynamic HDR metadata through a libx265 re-encode needs and
// nothing else: which Dolby Vision and HDR10+ sources this build carries and why it skips the
// rest (Decide), the two external tools (dovi_tool and hdr10plus_tool) and the pre-passes run
// through them, the VBV ceiling x265 requires before it codes a Dolby Vision RPU, and the
// arithmetic of the output gates that license the swap (docs/design/dynamic-hdr.md).
//
// The rules it implements, each argued in docs/design/dynamic-hdr.md:
//
//   - Only the cpu encoder (libx265) carries dynamic metadata. Every other encoder, and a
//     remux-only root, keeps skipping such a source exactly as before.
//   - Dolby Vision profile 8.1 is carried by libx265's own RPU coding (`-dolbyvision 1`), with
//     the VBV settings and the mastering display x265 refuses to open without. Profile 7 is
//     converted to 8.1 first, only where its root opts in (dolby_vision_p7: convert), with
//     dovi_tool mode 2 and the enhancement layer discarded. Profile 5, and every profile 8
//     whose base layer is not HDR10-compatible, stays skipped.
//   - HDR10+ is extracted with hdr10plus_tool, validated (it parses, and it carries one entry
//     per source frame), and handed to libx265 as dhdr10-info.
//   - The output replaces nothing until its DOVI configuration record names the planned
//     profile and compatibility id and every frame carries an RPU, and every frame carries
//     HDR10+ where the plan does.
package dynhdr

// The skip tokens this package's verdicts carry. They are part of the engine's skip
// vocabulary (a wire format: rows store them and a UI keys off them), which declares each AS
// the constant here so there is one spelling.
const (
	// ReasonDolbyVision: a Dolby Vision source this build does not carry - an encoder other
	// than cpu, a remux-only root, profile 5, a profile 8 whose base layer is not
	// HDR10-compatible, a profile this build does not know, a DOVI configuration record that
	// could not be read, or a colour description that would not code compatibility id 1.
	ReasonDolbyVision = "dolby-vision"
	// ReasonHDR10Plus: an HDR10+ source this build does not carry: an encoder other than cpu,
	// or a remux-only root.
	ReasonHDR10Plus = "hdr10-plus"
	// ReasonProfile7: a Dolby Vision profile 7 source under a root whose dolby_vision_p7 is
	// skip, the default. Setting it to convert offers the file back.
	ReasonProfile7 = "dolby-vision-profile-7"
	// ReasonNoMasteringDisplay: a Dolby Vision source that carries no complete mastering
	// display, without which x265 refuses to code profile 8.1.
	ReasonNoMasteringDisplay = "dolby-vision-no-mastering-display"
	// ReasonFrameRate: a profile 7 source opted into conversion whose frame rate is not one
	// constant, established rate starting at zero: the converted raw stream carries no
	// timestamps of its own and would be retimed wrongly. Also a source whose frame rate and
	// size give no HEVC level to take the VBV ceiling from.
	ReasonFrameRate = "dolby-vision-frame-rate"
	// ReasonToolMissing: the dovi_tool or hdr10plus_tool this source needs is not installed.
	// A condition of the host, not a verdict about the file: re-decided on every pass.
	ReasonToolMissing = "dynamic-hdr-tool-missing"
	// ReasonHDR10PlusUnreadable: the source's HDR10+ metadata could not be extracted, did not
	// parse, or did not carry exactly one entry per frame.
	ReasonHDR10PlusUnreadable = "hdr10-plus-unreadable"
	// ReasonConversionFailed: the profile 7 to 8.1 conversion did not complete.
	ReasonConversionFailed = "dolby-vision-conversion-failed"
)

// Reasons is every token above, in the order the engine's vocabulary lists them.
var Reasons = []string{
	ReasonProfile7, ReasonNoMasteringDisplay, ReasonFrameRate, ReasonToolMissing,
	ReasonHDR10PlusUnreadable, ReasonConversionFailed,
}

// The side-data type names ffprobe gives the metadata this package reads, as they appear in
// its flat and JSON output (libavutil/frame.c av_frame_side_data_name and
// libavcodec/packet.c av_packet_side_data_name at the pinned 5d4d3bdc61,
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01; observed on the pinned
// binary the same day).
const (
	// DoviRecordType is the stream-level DOVI configuration record (dvcC/dvvC).
	DoviRecordType = "DOVI configuration record"
	// DoviRPUType is the per-frame side data the HEVC decoder exports for a parsed RPU.
	DoviRPUType = "Dolby Vision RPU Data"
	// HDR10PlusType is the per-frame side data the decoder exports for a parsed SMPTE ST
	// 2094-40 (HDR10+) SEI.
	HDR10PlusType = "HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"
)
