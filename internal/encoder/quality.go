package encoder

import (
	"fmt"
	"sort"
)

// PER-ENCODER QUALITY: each encoder's quality-targeted rate control is set on its own scale,
// by its own configuration key.
//
// Before this, one number - `crf` - was passed as libx265's and libsvtav1's -crf, as NVENC's
// -cq, as QSV's -global_quality, as VAAPI's -qp and as AMF's -qp_i/-qp_p: five different
// instructions (an x265 rate factor, a constant-quality target, an ICQ level and two fixed
// quantisers) under one name, on scales that do not even share a range. `crf` stays the key
// for the two software encoders; each hardware encoder gets `quality.<registry key>`, on the
// scale below, and a key left out inherits the job's `crf` unchanged - which is what every
// configuration written before the key existed did, so its command line does not move.

// QualityScale is the scale one encoder's quality target is set on.
type QualityScale struct {
	// ConfigKey is the configuration key that sets the value: "crf" for the software
	// encoders, "quality.<registry key>" for each hardware encoder.
	ConfigKey string
	// Option is the ffmpeg option (or options) the value is passed as.
	Option string
	// Min and Max bound the values this build passes, inclusive.
	Min, Max int
}

// PerEncoder reports whether the scale is set by a `quality.<key>` entry of its own, rather
// than by crf.
func (q QualityScale) PerEncoder() bool { return q.ConfigKey != CRFKey }

// Contains reports whether v is on the scale.
func (q QualityScale) Contains(v int) bool { return v >= q.Min && v <= q.Max }

// String renders the scale for a refusal: its option and its range.
func (q QualityScale) String() string {
	return fmt.Sprintf("%s %d-%d", q.Option, q.Min, q.Max)
}

// CRFKey is the configuration key the software encoders' quality is set by.
const CRFKey = "crf"

// QualityMapKey is the top-level configuration key whose entries set each hardware
// encoder's quality: `quality.<registry key>`.
const QualityMapKey = "quality"

// The scales. Every range is the pinned ffmpeg's own (N-125875-g5d4d3bdc61-20260731,
// `ffmpeg -h encoder=<name>`, run 2026-09-30), narrowed where the encoder gives a value
// inside that range a meaning other than a quality target; TestQualityScales_MatchThePinnedBinary
// re-reads each option's range from the binary the gate runs. Sources at
// https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/ , read 2026-09-30.
//
// THE DEFAULT of every hardware scale - an absent `quality.<key>` - is the job's `crf`
// carried onto this encoder's scale UNCHANGED, the number the command line has always
// passed. ASSUMED: that number is not measured to mean anything comparable on any of
// these encoders; it is kept only so an existing configuration's command line does not
// move. A hardware report per encoder (NEEDS-OWNER, brief T43) calibrates it, and changing
// it is its own owner-visible commit because it changes argv for existing configurations.
var (
	// scaleCRF is libx265's and libsvtav1's `crf`, validated 0-51 by the configuration
	// (config.Profile.validate). libx265 -crf is a float from -1 and libsvtav1 -crf 0-63
	// on the pinned binary; the 0-51 bound is the configuration's own, unchanged here.
	scaleCRF = QualityScale{ConfigKey: CRFKey, Option: "-crf", Min: 0, Max: 51}

	// scaleNVENC is hevc_nvenc -cq: "0 to 51, 0 means automatic" (pinned binary;
	// libavcodec/nvenc_hevc.c:113), and the value becomes rcParams.targetQuality only when
	// it is non-zero (libavcodec/nvenc.c:1143-1147). 0 is not a quality target, so the
	// scale starts at 1. ASSUMED default: the job's crf, unchanged (see above; T43).
	scaleNVENC = QualityScale{ConfigKey: QualityMapKey + ".nvenc", Option: "-cq", Min: 1, Max: 51}

	// scaleAV1NVENC is av1_nvenc -cq: "0 to 63, 0 means automatic" (pinned binary;
	// libavcodec/nvenc_av1.c:110), 0 excluded for the reason above. ASSUMED default: the
	// job's crf, unchanged (see above; T43).
	scaleAV1NVENC = QualityScale{ConfigKey: QualityMapKey + ".av1_nvenc", Option: "-cq", Min: 1, Max: 63}

	// scaleQSV is hevc_qsv -global_quality with no maxrate and no lookahead, which selects
	// ICQ only when the value is above 0 (libavcodec/qsvenc.c:623-625): "For the ICQ modes,
	// global quality range is 1 to 51" (doc/encoders.texi:3739-3740). -global_quality is a
	// generic codec option, so the binary prints no per-encoder range for it. ASSUMED
	// default: the job's crf, unchanged (see above; T43).
	scaleQSV = QualityScale{ConfigKey: QualityMapKey + ".qsv", Option: "-global_quality", Min: 1, Max: 51}

	// scaleVAAPI is hevc_vaapi -qp, "Constant QP (for P-frames; scaled by qfactor/qoffset
	// for I/B)", 0-52 (pinned binary; libavcodec/vaapi_encode_h265.c:1108-1109). The
	// encoder takes its explicit QP only when the value is above 0
	// (vaapi_encode_h265.c:1084-1085), so 0 means unset and the scale starts at 1. ASSUMED
	// default: the job's crf, unchanged (see above; T43).
	scaleVAAPI = QualityScale{ConfigKey: QualityMapKey + ".vaapi", Option: "-qp", Min: 1, Max: 52}

	// scaleAMF is hevc_amf -qp_i and -qp_p under -rc cqp, each "-1 to 51" (pinned binary;
	// libavcodec/amfenc_hevc.c:107-108), where -1 is the option's default, "not set". The
	// scale is 0-51. ASSUMED default: the job's crf, unchanged (see above; T43).
	scaleAMF = QualityScale{ConfigKey: QualityMapKey + ".amf", Option: "-qp_i/-qp_p", Min: 0, Max: 51}
)

// QualityKeys are the registry keys a `quality.<key>` entry may name, sorted: every encoder
// whose scale is its own.
func QualityKeys() []string {
	var keys []string
	for k, s := range registry {
		if s.Quality.PerEncoder() {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// ValidateQualityKey refuses a `quality.<key>` entry that names no encoder with a scale of
// its own, or a value outside that encoder's scale, naming why and the scale. key must be a
// registry key exactly: an ffmpeg codec alias is refused, so one encoder cannot be given
// two values under two spellings.
func ValidateQualityKey(key string, value int) error {
	spec, ok := registry[key]
	if !ok {
		if canon, alias := aliases[key]; alias && registry[canon].Quality.PerEncoder() {
			return fmt.Errorf("%s.%s names the ffmpeg codec, not the registry key: write %s.%s",
				QualityMapKey, key, QualityMapKey, canon)
		}
		if canon, alias := aliases[key]; alias {
			key = canon
			spec = registry[canon]
		} else {
			return fmt.Errorf("%s.%s names no encoder this build ships (a quality key is one of %v)",
				QualityMapKey, key, QualityKeys())
		}
	}
	if !spec.Quality.PerEncoder() {
		return fmt.Errorf("%s.%s is not a key: %s's quality is set by %s (%s), not under %s",
			QualityMapKey, key, spec.FFmpegCodec, spec.Quality.ConfigKey, spec.Quality, QualityMapKey)
	}
	if !spec.Quality.Contains(value) {
		return fmt.Errorf("%s %d is outside %s's scale (%s)", spec.Quality.ConfigKey, value, spec.FFmpegCodec, spec.Quality)
	}
	return nil
}
