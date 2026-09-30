package encoder

import "github.com/NSchatz/holdfast/internal/version"

// amfImageReason is why `encoder: amf` cannot run in the container image. AMD's AMF runtime
// (libamfrt64.so.1, in the amf-amdgpu-pro package) is licensed under the AMDGPU PRO EULA,
// whose section 2 grants only a licence "to a) install and use the Software solely in Object
// Code form" and whose section 3 says "You have no other rights in the Software": there is no
// redistribution grant, so a published image of this AGPL project cannot carry it. AMD's own
// advice for Linux is VA-API through Mesa. Read 2026-09-29 (P3, approved at Checkpoint T):
// https://repo.radeon.com/amf/copyright and
// https://www.amd.com/en/resources/support-articles/release-notes/RN-AMDGPU-UNIFIED-LINUX-25-10-1.html
const amfImageReason = `encoder "amf" is refused in the holdfast container image: AMD's AMF ` +
	`runtime (libamfrt64) is licensed under the AMDGPU PRO EULA, which grants no right to ` +
	`redistribute it, so the image does not carry it. On AMD hardware use "encoder: vaapi" ` +
	`(Mesa radeonsi, which the image carries), or run holdfast outside the image on a host ` +
	`with AMD's runtime installed (docs/design/hardware.md#amf)`

// RefusedInImage is why spec cannot run in this build's packaging, or "" when nothing refuses
// it. Only `amf` in the container image is refused: it stays a valid key (holdfast validate
// accepts it, and a host install runs it), and it is never read as `vaapi`, which is a
// different encoder with a different quality scale.
func RefusedInImage(spec Spec) string {
	if spec.Key == "amf" && version.Packaging == version.PackagingImage {
		return amfImageReason
	}
	return ""
}
