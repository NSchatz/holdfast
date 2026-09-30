package encoder

import "github.com/NSchatz/holdfast/internal/version"

// amfImageReason is why an AMF encoder (`encoder: amf`, `h264_amf` or `av1_amf`) cannot run
// in the container image. AMD's AMF runtime
// (libamfrt64.so.1, in the amf-amdgpu-pro package) is licensed under the AMDGPU PRO EULA,
// whose section 2 grants only a licence "to a) install and use the Software solely in Object
// Code form" and whose section 3 says "You have no other rights in the Software": there is no
// redistribution grant, so a published image of this AGPL project cannot carry it. AMD's own
// advice for Linux is VA-API through Mesa. Read 2026-09-29 (P3, approved at Checkpoint T):
// https://repo.radeon.com/amf/copyright and
// https://www.amd.com/en/resources/support-articles/release-notes/RN-AMDGPU-UNIFIED-LINUX-25-10-1.html
//
// key is the AMF encoder refused and vaapi the VAAPI encoder of the same codec, which the
// refusal names as the one to use; for `amf` the text is the one this build has always
// printed.
func amfImageReason(key, vaapi string) string {
	return `encoder "` + key + `" is refused in the holdfast container image: AMD's AMF ` +
		`runtime (libamfrt64) is licensed under the AMDGPU PRO EULA, which grants no right to ` +
		`redistribute it, so the image does not carry it. On AMD hardware use "encoder: ` + vaapi + `" ` +
		`(Mesa radeonsi, which the image carries), or run holdfast outside the image on a host ` +
		`with AMD's runtime installed (docs/design/hardware.md#amf)`
}

// RefusedInImage is why spec cannot run in this build's packaging, or "" when nothing refuses
// it. Only the AMF encoders in the container image are refused: each stays a valid key
// (holdfast validate accepts it, and a host install runs it), and none is ever read as its
// VAAPI sibling, which is a different encoder with a different quality scale.
func RefusedInImage(spec Spec) string {
	if spec.API == APIAMF && version.Packaging == version.PackagingImage {
		return amfImageReason(spec.Key, vaapiOf(spec.TargetCodec))
	}
	return ""
}

// vaapiOf is the key of the VAAPI encoder that writes codec.
func vaapiOf(codec string) string {
	for _, k := range Known() {
		if s := registry[k]; s.API == APIVAAPI && s.TargetCodec == codec {
			return k
		}
	}
	return "vaapi"
}
