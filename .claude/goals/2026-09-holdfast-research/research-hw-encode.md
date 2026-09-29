# Research: hardware encode in the holdfast image (QSV, VAAPI, AMF, NVENC)

Date read for every URL below: 2026-09-29 unless stated. Tags:
- (PRIMARY) upstream source, vendor docs, Debian package pages, or a file inspected locally.
- (SECONDARY) a measurement published by a third party; used for numbers only.
- LEAD: a forum or maintainer comment, not relied on.
- ASSUMED: from memory, not verified in this session. Verify before building on it.

Repository state read: `/workspace` at `3c229da` (Dockerfile, docs/docker.md:330-420,
internal/encoder/encoder.go, internal/engine/encode.go buildArgs, internal/hdr/probe.go,
docs/design/quality-gate.md, NOTICE, LICENSE). Nothing in `/workspace` was changed.

Pinned ffmpeg: BtbN `autobuild-2026-07-31-14-10` / `N-125875-g5d4d3bdc61`. FFmpeg source
quoted below was fetched at that exact revision (`raw.githubusercontent.com/FFmpeg/FFmpeg/5d4d3bdc61/...`).
BtbN build scripts were read at BtbN HEAD `1e936193` (2026-09-29), not at the pin tag; the
scripts quoted are stable across months but confirm at the tag before relying on a detail.

---

## Headline findings (read these first)

1. **The pinned ffmpeg dlopens libva/libdrm at runtime and ABORTS if they are missing.**
   BtbN builds libva, libva-drm, libva-x11 and libdrm as shared libs, then replaces them with
   Implib.so shims generated with `--dlopen --lazy-load`, and deletes the `.so` files
   (PRIMARY: BtbN `scripts.d/50-vaapi/50-libva.sh`, `40-libdrm.sh`, `images/base-linux*/gen-implib.sh`).
   The Implib.so template's failure path is `fprintf(...); assert(0 ...); abort();`
   (PRIMARY: https://raw.githubusercontent.com/yugr/Implib.so/master/arch/common/init.c.tpl).
   So `libva.so.2`, `libva-drm.so.2`, `libdrm.so.2` (and `libva-x11.so.2` + libX11 if the X11
   path is ever reached) must be in the image, or any vaapi/qsv call is a SIGABRT, not an error.
2. **libva's driver dir is compiled in as the Debian multiarch path.** `-Ddriverdir=/usr/lib/x86_64-linux-gnu/dri`
   (amd64) and `/usr/lib/aarch64-linux-gnu/dri` (arm64), `--sysconfdir=/etc` (PRIMARY: 50-libva.sh).
   Debian's VA driver packages install exactly there, so copying Debian's drivers to the same
   path needs no `LIBVA_DRIVERS_PATH`.
3. **libvpl (QSV) is a static dispatcher, amd64 only.** `50-onevpl.sh` returns disabled for
   `*arm64` and builds `-DBUILD_SHARED_LIBS=OFF` (PRIMARY). The dispatcher then dlopens the GPU
   runtime `libmfx-gen.so.1.2` (Tiger Lake+) or legacy `libmfxhw64.so.1` from `LD_LIBRARY_PATH`,
   then default dirs incl. `/usr/lib/x86_64-linux-gnu` on Debian, then `ONEVPL_SEARCH_PATH`
   (PRIMARY: https://intel.github.io/libvpl/latest/programming_guide/VPL_prg_session.html).
   The BtbN README itself says "libmfx and libva: ... no aarch64 support" (PRIMARY:
   https://github.com/BtbN/FFmpeg-Builds), but the current libva script does build for
   linuxarm64 - treat the README line as stale for libva, accurate for libvpl.
4. **AMF is headers only in the build; the runtime is AMD-EULA software with no redistribution grant.**
   `50-amf.sh` just copies `amf/public/include` (PRIMARY). The runtime deb's copyright file is
   the "AMDGPU PRO EULA", granting only a licence to "install and use the Software solely in Object Code
   form", with no distribution right and a restriction on distributing it "so that any part becomes
   subject to a Free Software License" (PRIMARY: `amf-amdgpu-pro_25.20-399_amd64.deb`,
   `/usr/share/doc/amf-amdgpu-pro/copyright`, downloaded from
   https://repo.radeon.com/amf/25.20/ubuntu/pool/main/noble/). A public AGPL image cannot ship it.
5. **The NVIDIA path documented today probably does not inject the encoder library.** The NVIDIA
   Container Toolkit's default capabilities when `NVIDIA_DRIVER_CAPABILITIES` is "empty or unset"
   are `utility` and `compute`, and `video` is "required for using the Video Codec SDK" (PRIMARY:
   https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html).
   Docker sets `NVIDIA_DRIVER_CAPABILITIES` only from device-request capabilities that are NVIDIA
   caps (`compute, compat32, graphics, utility, video, display`); `gpu` is not one (PRIMARY:
   moby `daemon/devices_nvidia_linux.go` @ `561a5a9b`). docs/docker.md:349-357 and
   docker-compose.yml:123 use `capabilities: [gpu]` and the Dockerfile sets no `NVIDIA_*` ENV, so
   `libnvidia-encode.so.1` would not be mounted. Jellyfin and linuxserver both set
   `NVIDIA_DRIVER_CAPABILITIES="compute,video,utility"` in their images (PRIMARY: jellyfin-packaging
   docker/Dockerfile; linuxserver/docker-ffmpeg Dockerfile). Not tested on hardware here; holdfast's
   `Available()` would correctly fail loud, so this is a doc/ENV bug, not a safety bug.
6. **Two latent defects in the current hardware argv (not reachable today, reachable once runtimes ship):**
   - `vaapi`: `buildArgs` emits `-vf format=nv12,hwupload` (internal/engine/encode.go:815). nv12 is
     8-bit, so a 10-bit/HDR10 source would be encoded as 8-bit Main while carrying PQ/BT.2020 tags.
     Must be `format=p010,hwupload` (or the derived 10-bit format) plus `-profile:v main10`.
   - `Available()` probes vaapi with plain `-c:v hevc_vaapi` on a software `testsrc2` with no
     `-vaapi_device` and no `hwupload` (internal/encoder/encoder.go Available). `hevc_vaapi` accepts
     only `AV_PIX_FMT_VAAPI` (PRIMARY: vaapi_encode_h265.c:1213 `CODEC_PIXFMTS(AV_PIX_FMT_VAAPI)`),
     so this probe fails on a perfectly working VAAPI host. It must use the same device/filter argv
     the real encode uses.
   - For nvenc/qsv the universal `-pix_fmt yuv420p10le` is not in either encoder's list; ffmpeg's
     `choose_pixel_fmt` then auto-selects the "best" supported format and logs only a WARNING
     ("Incompatible pixel format ... auto-selecting") (PRIMARY: fftools/ffmpeg_mux_init.c:465-497).
     holdfast runs with `-loglevel error`, so a 4:2:2 or 4:4:4 source on an encoder without that
     chroma would be silently subsampled - the exact thing the exotic-pix_fmt guard exists to refuse.

---

## Q1. Runtime userspace for QSV and VAAPI, per arch

### What each path needs at runtime (with the pinned BtbN binary)

| Path | amd64 | arm64 |
|---|---|---|
| VAAPI, Intel | `libva.so.2`, `libva-drm.so.2`, `libdrm.so.2`, `iHD_drv_video.so` in `/usr/lib/x86_64-linux-gnu/dri`, `libigdgmm.so.12`, libstdc++/libgcc_s (in distroless cc) | n/a (no Intel GPUs) |
| QSV, Intel | everything in VAAPI Intel, plus `libmfx-gen.so.1.2` (Tiger Lake+) and/or `libmfxhw64.so.1` (pre-Tiger Lake). libvpl dispatcher is static inside ffmpeg | not built (libvpl disabled for arm64) |
| VAAPI, AMD | `libva*.so.2`, `libdrm.so.2`, `libdrm_amdgpu.so.1`, `radeonsi_drv_video.so` (a link into `libgallium-<ver>.so`), plus Mesa's deps: `libLLVM-19`, `libelf`, `libexpat`, `libzstd`, `zlib`, `libsensors`, `libx11-xcb`, `libxcb*`, `libxshmfence` | same package set exists on arm64 (radeonsi is built for arm64 in Debian) |
| Device | `/dev/dri/renderD*` passed in, process in the node's group | same |

Evidence: BtbN scripts (above); Debian package pages below; vpl-gpu-rt README: "Tiger Lake and
newer", depends on LibVA and iHD, MIT (PRIMARY: https://github.com/intel/vpl-gpu-rt).

### Debian packages (all PRIMARY: packages.debian.org, read 2026-09-29)

| Package | bookworm (12) | trixie (13) | Arch | Licence |
|---|---|---|---|---|
| libva2 / libva-drm2 | 2.17 (ASSUMED) | 2.22.0-3 | all incl. arm64 | MIT (ASSUMED, libva upstream) |
| intel-media-va-driver (main) | - | 25.2.3+dfsg1-1 | amd64, i386 | MIT + BSD-3. Encoder support limited: "JPEG on Skylake and newer; AVC on Broxton and newer; HEVC and VP9 on Ice Lake and newer" |
| intel-media-va-driver-non-free | 23.1.1+ds1-1 (non-free) | 25.2.3+ds1-1 (non-free), 6.4 MB download | amd64, i386 | Expat + BSD-3; in non-free because "kernels ... come without source" (https://metadata.ftp-master.debian.org/changelogs/non-free/i/intel-media-driver-non-free/unstable_copyright) |
| libigdgmm12 | - | 22.7.2+ds1-1 | amd64, i386 | MIT (ASSUMED, gmmlib upstream) |
| libmfx-gen1.2 (vpl-gpu-rt) | 22.6.4-1 | 25.1.4-1, 2.1 MB download | amd64 | MIT |
| libvpl2 (dispatcher, not needed: ffmpeg links it statically) | 2023.1.1-1 | 1:2.14.0-1 | amd64 | MIT |
| libmfx1 (legacy Media SDK runtime, pre-TGL QSV) | 22.5.4-1 | **not in trixie** | amd64 | MIT |
| mesa-va-drivers | 22.3.6-1+deb12u2, `-Dvideo-codecs="vc1dec, h264dec, h264enc, h265dec, h265enc"` (no AV1 enc) | 25.0.7-2+deb13u1, `-Dvideo-codecs="all"` | amd64, arm64, ... | MIT |
| mesa-libgallium (trixie) | - | 25.0.7, 41.6 MB installed amd64 / 34.2 MB arm64; depends libllvm19, libdrm-amdgpu1, libxcb-*, libsensors5, libelf1t64, libzstd1 ... | | MIT; LLVM is Apache-2.0 WITH LLVM-exception (ASSUMED); elfutils LGPL-3+/GPL-2+ dual and libsensors LGPL-2.1+ (ASSUMED) |
| libllvm19 | - | 24.8 MB download | | see above |

Mesa video-codec flags: PRIMARY https://salsa.debian.org/xorg-team/lib/mesa/-/raw/debian-bookworm/debian/rules
and `.../debian-trixie/debian/rules`. Trixie radeonsi gallium list includes arm64 (debian-unstable rules).

Consequences:
- **Encode on Intel before Ice Lake needs the non-free iHD.** Debian's free build omits the
  prebuilt encode kernels. The non-free one is redistributable (Expat/BSD-3, the "non-free" is a DFSG
  source-availability classification, not a redistribution ban).
- **QSV on pre-Tiger Lake Intel is not possible from trixie packages** (no libmfx1). VAAPI still
  covers those chips through iHD. Jellyfin solves this by building MediaSDK itself (below).
- **Trixie packages need glibc >= 2.38**; the image base is `gcr.io/distroless/cc-debian12`
  (bookworm, glibc 2.36). The distroless README now lists only `-debian13` images and says
  "Distroless images are based on Debian 13 (trixie)" (PRIMARY: https://github.com/GoogleContainerTools/distroless).
  Moving the base to `cc-debian13` is a prerequisite for copying trixie libs, and is worth doing anyway.
- Debian's iHD 25.2.x may lag the newest Intel parts (Battlemage/Lunar Lake/Panther Lake); ASSUMED,
  check media-driver release notes for the pinned version's platform list.

### Copying into distroless: does it work?

Yes, with care. Mechanics that matter:
- **No QEMU needed.** In a `--platform=$BUILDPLATFORM` stage, `dpkg --add-architecture arm64;
  apt-get download <pkg>:<arch>=<ver>` then `dpkg -x` into a staging root. No maintainer scripts
  run, so this preserves the Dockerfile's "arm64 runtime stage only COPYs" property. Pin versions
  and verify SHA-256 the way the ffmpeg tarball is, or pin by snapshot.debian.org URL (ASSUMED design).
- **Dependency closure.** Compute it with `ldd`/`readelf -d` on the staged driver .so files, not
  by hand; mesa's closure is large (LLVM). Copy to `/usr/lib/<triplet>/` and `/usr/lib/<triplet>/dri/`.
- **Loader search path.** distroless cc already resolves `libgcc_s.so.1` from the multiarch dir,
  which shows the multiarch dirs are on the loader path; whether `/usr/lib/<triplet>` (vs `/lib/<triplet>`)
  is covered depends on merged-/usr in the base (ASSUMED; verify with `holdfast` executing ffmpeg
  in the smoke test). If not, add an `/etc/ld.so.conf.d` + `ld.so.cache` is impossible without a shell -
  copy libs into the directory the base already searches instead.
- **`LIBVA_DRIVERS_PATH`**: not needed for Debian paths (compiled-in driverdir matches). Needed only
  if drivers are placed elsewhere (linuxserver sets `LIBVA_DRIVERS_PATH=/usr/local/lib/x86_64-linux-gnu/dri`).
  `LIBVA_DRIVER_NAME=iHD|radeonsi` can force a driver; libva otherwise picks by kernel driver
  (ASSUMED; libva va.c). Jellyfin patches libva to ignore `LIBVA_DRIVER_NAME` and hard-codes its own
  search path (PRIMARY: jellyfin-ffmpeg docker-build.sh:375-376).
- **Device node and group.** `/dev/dri/renderD128` is typically `root:render 0660` (ASSUMED). The
  container needs `devices: [/dev/dri/renderD128]` and `group_add: ["<host render gid>"]` - numeric,
  because distroless has no `/etc/group` entry for it. Jellyfin documents exactly this: "pass the
  host's `render` group id to Docker", "On some releases, the group may be `video` or `input`"
  (PRIMARY: https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/intel).
- **VAAPI device open order.** At the pinned revision `vaapi_device_create` tries DRM render nodes
  first (renderD128.. up to 8, filterable by `kernel_driver` / `vendor_id`), then X11 (PRIMARY:
  libavutil/hwcontext_vaapi.c:1745-1900 @ 5d4d3bdc61). ffmpeg.html still says "attempt to open the
  default X11 display ($DISPLAY) and then the first DRM render node" (PRIMARY: https://ffmpeg.org/ffmpeg.html) -
  the doc is stale. Practical rule: always pass an explicit node (`-init_hw_device vaapi=va:/dev/dri/renderD128`),
  so the X11 shim (and its abort-on-missing libX11) is never reached.

### Reference designs

- **jellyfin-ffmpeg** (PRIMARY: `docker-build.sh` on branch `jellyfin` via GitHub API): builds from
  source and ships inside `/usr/lib/jellyfin-ffmpeg/lib`: libva 2.24.1 (patched: driver path hard-coded to
  `/usr/lib/jellyfin-ffmpeg/lib/dri:<multiarch>/dri:...`, `LIBVA_DRIVER_NAME` renamed), gmmlib 22.10.2,
  MediaSDK 23.2.2 runtime (`libmfxhw64.so.1`, "for 11th Gen Rocket Lake and older"), libvpl v2.17.0
  dispatcher (shared, patched search path), vpl-gpu-rt `intel-onevpl-26.3.5`, media-driver
  `intel-media-26.3.5` with `-DENABLE_NONFREE_KERNELS=ON`, i965 driver, and a minimal Mesa 26.0
  (`-Dgallium-drivers=radeonsi -Dgallium-va=enabled -Dvideo-codecs=all`, plus RADV) for AMD. AMF: headers
  only. The Jellyfin runtime image (`jellyfin-packaging/docker/Dockerfile`) is `debian:<ver>-slim` plus
  the jellyfin-ffmpeg deb, Intel OpenCL runtime (amd64), and `NVIDIA_DRIVER_CAPABILITIES="compute,video,utility"`.
  Jellyfin AMD doc: on Linux "VA-API - Preferred on all GPUs"; "AMF - Not recommended, limited support,
  hardware encoder only, closed source"; "AMF is not available in Windows Docker and WSL/WSL2" (PRIMARY:
  https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/amd).
- **linuxserver/docker-ffmpeg** (PRIMARY: Dockerfile via GitHub API): Ubuntu `resolute` base, builds
  libva 2.24.1, libdrm 2.4.134, Mesa 26.2.0 (`-Dvideo-codecs=all`), gmmlib 22.10.0, iHD 26.2.4, libvpl 2.17.0,
  vpl-gpu-rt 26.2.4, MediaSDK 22.5.4 into `/usr/local`, sets `LIBVA_DRIVERS_PATH=/usr/local/lib/x86_64-linux-gnu/dri`,
  `LD_LIBRARY_PATH=/usr/local/lib`, `NVIDIA_DRIVER_CAPABILITIES="compute,video,utility"`, and installs
  `libllvm21` + X/xcb libs at runtime. The arm64 Dockerfile has no mesa/iHD/vpl lines.

Both reference designs build the whole Intel/AMD stack from source at pinned tags and ship it
beside their own ffmpeg. That is the higher-maintenance, newer-hardware option; copying Debian
trixie packages is the lower-maintenance, older-driver option.

---

## Q2. AMF on Linux (T45)

Facts:
- Runtime: `libamfrt64.so.1` from `amf-amdgpu-pro` (25.20-399, amd64 only), installed to
  `/opt/amf/lib/x86_64-linux-gnu/` (not a default loader dir). `Depends: libc6, libdrm2, libstdc++6,
  libvulkan1, libamdenc-amdgpu-pro`; `Recommends: rocm-opencl-runtime, mesa-vulkan-drivers | vulkan-amdgpu-pro ...`
  (PRIMARY: deb control file, downloaded and inspected 2026-09-29).
- Licence: "AMDGPU PRO EULA": s.2 grants "install and use the Software solely in Object Code form in
  conjunction with systems ... that include ... AMD processors"; s.3(e) forbids distributing it "so that
  any part becomes subject to a Free Software License"; no redistribution grant anywhere; s.10 terminates
  on breach (PRIMARY: same deb, `/usr/share/doc/amf-amdgpu-pro/copyright`).
- Distribution change: AMF README: "Starting with the 25.20 Linux driver, the AMF runtime is released
  separately"; RADV support: "Stable support for RADV drivers for AMF on Linux in VideoConverter/HQScaler/VideoEncoder
  and experimental for decoder" (v1.4.34) (PRIMARY: https://github.com/GPUOpen-LibrariesAndSDKs/AMF).
  Install is `amf_installer_25.20.sh [--accept-eula]` on top of the "All-Open" stack (PRIMARY:
  https://github.com/GPUOpen-LibrariesAndSDKs/AMF/wiki/Driver-Linux). Latest AMF release v1.5.2 (2026-05-06)
  ships headers only; v1.5.0 (2025-10-29) shipped `amf_installer_25.20.zip` (PRIMARY: GitHub releases API).
- AMD's own release notes for the 25.10/25.20 stacks say "AMF will no longer be included in the release.
  AMF users are advised to transition to VA-API / Mesa Multimedia" (SECONDARY quote via search of
  amd.com RN-AMDGPU-UNIFIED-LINUX-25-10-x / 25-20-3; the amd.com pages timed out on direct fetch - LEAD
  until fetched; the HandBrake discussion #6931 quotes the same text).
- Also required at runtime: a Vulkan loader + AMD Vulkan ICD (RADV) inside the container, plus
  `libamdenc`. None of this is in distroless.

Alternative: AMD through VAAPI (Mesa radeonsi). MIT-licensed, in Debian main for amd64 and arm64,
AMD's recommended path, Jellyfin's preferred path. Encodes H.264/HEVC (and AV1 on RDNA3+ with trixie's
Mesa 25 "all" codecs; ASSUMED for the generation list).

See **Proposal T45** below.

---

## Q3. Hardware decode pipelines that keep 10-bit/HDR intact

How holdfast works today (PRIMARY: internal/engine/encode.go, internal/vmaf): decode is **software**
for every encoder (no `-hwaccel`), frames go to the encoder as system-memory frames; VMAF decodes
source and output in software and converts both to one holdfast-named pix_fmt. So VMAF never touches
GPU frames, and hardware decode is purely an encode-throughput option.

Canonical pipelines (argv shapes; pix_fmt lists PRIMARY from source @ 5d4d3bdc61; flags from
https://ffmpeg.org/ffmpeg.html and NVIDIA's guide
https://docs.nvidia.com/video-technologies/video-codec-sdk/13.0/ffmpeg-with-nvidia-gpu/index.html):

- **Software decode, hardware encode (recommended first step, closest to today):**
  - NVENC: `-c:v hevc_nvenc -pix_fmt p010le -profile:v main10`. nvenc accepts `yuv420p, nv12, p010, yuv444p,
    p012 (truncated to 10 bits), nv24, p016 (truncated)` and 4:2:2 formats only under `NVENC_HAVE_422_SUPPORT`
    (nvenc.c:53-65). NVIDIA: "pix_fmt should be changed to yuv444p/p010/yuv444p16 for encoding YUV 444, 420-10
    and 444-10".
  - QSV: `-c:v hevc_qsv -pix_fmt p010le -profile:v main10`. hevc_qsv accepts `nv12, p010, p012, yuyv422,
    y210, qsv, bgra, x2rgb10, vuyx, xv30` (qsvenc_hevc.c:399-402).
  - VAAPI: `-init_hw_device vaapi=va:/dev/dri/renderD128 -filter_hw_device va ... -vf format=p010,hwupload
    -c:v hevc_vaapi -profile:v main10`. Only `AV_PIX_FMT_VAAPI` input (vaapi_encode_h265.c:1213), so the
    sw_format chosen before `hwupload` IS the bit depth.
  - AMF: sw frames accepted; 10-bit requires p010 (ASSUMED).
- **Full hardware (decode + encode on GPU):**
  - NVIDIA: `-hwaccel cuda -hwaccel_output_format cuda -i in -c:v hevc_nvenc` (NVIDIA guide). A 10-bit HEVC
    source decodes to p010 CUDA surfaces (ASSUMED).
  - Intel: `-hwaccel qsv -hwaccel_output_format qsv -c:v hevc_qsv -i in -c:v hevc_qsv`, or VAAPI decode with a
    derived QSV device: `-init_hw_device vaapi=va:/dev/dri/renderD128 -init_hw_device qsv=hw@va` (ffmpeg.html example).
  - VAAPI: `-hwaccel vaapi -hwaccel_output_format vaapi -hwaccel_device /dev/dri/renderD128 -i in -c:v hevc_vaapi`
    (ASSUMED; standard form, trac wiki was blocked by Anubis when fetched).
- **When frames must come back to system memory:** any software filter. holdfast's deinterlace
  (`withDeinterlace`) and `withDownscale` are software filters placed at the head of the chain
  (encode.go:824-876). With `-hwaccel_output_format <hw>` they need `hwdownload,format=p010le` first (then
  `hwupload` again), or their hardware equivalents (`scale_cuda`/`scale_vaapi`/`vpp_qsv`, `bwdif_cuda`,
  `deinterlace_vaapi`, `vpp_qsv=deinterlace=`). Each equivalent is a different filter, i.e. a different
  picture than the software reference VMAF is scored against, so it changes what the gate measures
  (docs/design/quality-gate.md "Why a number needs its conditions"). Keep filters in software.
- **Silent fallback pitfall:** with `-hwaccel X` and no output format, ffmpeg falls back to software decode on
  an unsupported profile (e.g. HEVC 4:2:2 on most GPUs) (ASSUMED); with `-hwaccel_output_format` set the
  same source fails. Either way the probe-and-refuse design handles it, but it must be tested per profile.

HDR10 static metadata (mastering display + content light), PRIMARY from source @ 5d4d3bdc61:
- **hevc_nvenc / av1_nvenc**: `outputMasteringDisplay`/`outputMaxCll` are switched on at init only if the
  metadata is in `avctx->decoded_side_data` (stream-level), then per frame taken from frame side data or the
  stream-level copy (nvenc.c:1388-1395, 1602-1608, 2931-2960), guarded by `NVENC_HAVE_HEVC_AND_AV1_MASTERING_METADATA`
  (needs a new-enough nv-codec-headers and driver; ASSUMED driver >= 550-series). Pitfall: a source whose
  mastering metadata is only in-band SEI and not in the container/stream side data will not enable it.
- **hevc_qsv**: per-frame `mfxExtMasteringDisplayColourVolume` and `mfxExtContentLightLevelInfo` from frame
  side data (qsvenc_hevc.c:175-235). VUI colour description from `avctx->color_*` (qsvenc.c:1216-1233).
- **hevc_vaapi**: `sei` option default `SEI_MASTERING_DISPLAY | SEI_CONTENT_LIGHT_LEVEL | SEI_A53_CC`
  (vaapi_encode_h265.c:1154-1163), filled from frame side data (531-545). av1_vaapi also carries them
  (vaapi_encode_av1.c:675, 717).
- **hevc_amf**: `AMF_VIDEO_ENCODER_HEVC_INPUT_HDR_METADATA` from frame side data (amfenc.c:451-469).
- holdfast today passes mastering metadata ONLY for libx265 (`-x265-params master-display/max-cll`,
  internal/hdr/probe.go:92-100), relying on ffmpeg side-data propagation for the others. There appears to be no
  output-side check that the mastering/CLL block, transfer, primaries and bit depth survived
  (`rg` of internal/engine/verify.go found no colour or pix_fmt parity check). VMAF cannot see a dropped SEI or
  a PQ tag on 8-bit samples. Recommend an encoder-agnostic **fidelity gate**: ffprobe the output's first frame
  and stream and require pix_fmt chroma + bit depth, colour_primaries/transfer/space/range, and (for HDR10) the
  mastering + CLL side data to equal what the job intended. It closes the vaapi nv12 hole and the
  nvenc/qsv auto-select hole in one place.

---

## Q4. Hardware vs libx265 at equal VMAF (T10)

Published measurements:
- Arunruangsirilert & Katto, "Evaluation of Hardware-based Video Encoders on Modern GPUs for UHD
  Live-Streaming", arXiv:2511.18686 (2025-11-24) (PRIMARY, academic): anchors were the slowest
  real-time presets on an i7-13700H: "libx264 slow", "libx265 faster", "SVT-AV1 preset 8". Against those,
  VMAF BD at 1080p: Intel QSV +0.51 VMAF (about 5% bitrate saving), NVENC +0.38, Qualcomm -6.77 (about 50%
  more bits); 2160p: QSV +0.75; 4320p NVENC -0.29. Conclusion: "modern GPU hardware encoders can match the RD
  performance of software encoders in real-time encoding scenarios". https://arxiv.org/html/2511.18686
- Fora Soft, "x264 vs x265 vs SVT-AV1 vs Hardware" (2026-06-25) (SECONDARY, vendor blog): BD-rate vs x264
  medium at equal VMAF: x265 slow -44%, NVENC AV1 -40%, QSV AV1 -38%, AMF AV1 -33%, SVT-AV1 p6 -55%, p4 -57%.
  https://www.forasoft.com/learn/video-quality/articles-vqm/encoder-comparison-x264-x265-svt-av1
- Streaming Learning Center, "Choosing an x265 Preset: an ROI Analysis" (2021-10-23) (SECONDARY): slow vs
  medium: "23% reduction in top bitrate", "26% ... across the encoding ladder".
  https://streaminglearningcenter.com/encoding/choosing-an-x265-preset-an-roi-analysis.html
- Gianni Rosato (2023-04-15) (SECONDARY, SSIMULACRA2 not VMAF): NVENC AV1 slightly ahead of QSV AV1 on Arc;
  on one clip NVENC beat x265 medium 10-bit. https://giannirosato.com/blog/post/nvenc-v-qsv/
- Jellyfin AMD doc (PRIMARY for their claim): AMD encoders "are currently no match for Intel QSV and NVIDIA NVENC",
  VCN5 (RX 9000) "substantially better".
- LEAD: intel/vpl-gpu-rt issue #375 "QSV AV1 encoding quality poorer than QSV HEVC, with low-latency settings".

Reading: modern hardware HEVC/AV1 is roughly at x265 `faster`/`medium` efficiency; holdfast's archival
default is libx265 `slow` (config default; baseline JSON shows `-preset slow -crf 22`). Chaining the
two secondary figures (derived, not measured): hardware HEVC needs on the order of 15-35% more bits than
x265 slow for the same VMAF; hardware AV1 (NVENC/QSV) lands near x265 slow; AMD trails both.

What it means for holdfast's gates:
- **Strictly-smaller**: unchanged in meaning; hardware outputs are bigger at equal quality, so more jobs
  on already-efficient sources (existing HEVC/AV1 or low-bitrate H.264) will fail strictly-smaller or reclaim
  little. That is a correct outcome, and the ledger already reports it.
- **VMAF mean + worst-frame + chroma floors**: these measure the OUTPUT, not the encoder, so they are
  already encoder-agnostic. Hardware rate control has weaker lookahead/AQ (ASSUMED), so worst-frame pooling
  is likely the floor that bites on fades/dark scenes - which is exactly what that floor is for.
- **Quality knob mapping is the real gap**: `buildArgs` reuses the CRF integer as nvenc `-cq`, qsv
  `-global_quality`, vaapi `-qp`, amf `-qp_i/-qp_p`. These are different scales (constant QP vs
  quality-targeted), so "crf: 22" produces very different quality per encoder (ASSUMED magnitude; needs a
  fixture measurement).
- **HDR fidelity**: see Q3 - needs an output-side check for every encoder.

See **Proposal T10** below.

---

## Q5. Automatic detection at startup

Keep `Available()`'s principle (really encode, then ffprobe the result). Harden it:

1. **Enumerate devices before probing** (cheap, gives named reasons):
   - DRM: glob `/dev/dri/renderD*`; for each, read `/sys/class/drm/renderDN/device/vendor`
     (`0x8086` Intel, `0x1002` AMD, `0x10de` NVIDIA) and `.../device/uevent` `DRIVER=` (`i915`, `xe`, `amdgpu`,
     `nouveau`) (ASSUMED: sysfs readable in a default Docker container). ffmpeg's own selector uses the same
     facts via `kernel_driver=` / `vendor_id=` (hwcontext_vaapi.c:1770-1854).
   - Try `open(O_RDWR)` on the node from Go: `EACCES` means "add `group_add: [<render gid>]`", a far better
     message than a failed encode. Report the node's owning gid from `stat`.
   - NVIDIA: `/dev/nvidiactl` + `/dev/nvidia0` present, `/proc/driver/nvidia/version` readable, and
     `libnvidia-encode.so.1` resolvable (the toolkit mounts it only with the `video` capability).
     **`nvidia-smi` absence is not a reliable signal** either way: it is injected only with the `utility`
     capability (toolkit docs), which is independent of `video`.
   - Runtime libs: check that `libva.so.2`, `libdrm.so.2`, the VA driver, and `libmfx-gen.so.1.2` exist before
     probing, because the BtbN shims `abort()` otherwise (headline 1). Capture stderr: the shim prints
     `implib-gen: <lib>: failed to load library ...` - surface that line as the reason.
2. **Probe with the production argv, not a generic one.** Build the probe through the same `buildArgs` +
   device/filter code the engine uses (fixes the vaapi probe that can never pass), with `-init_hw_device`
   on the chosen node.
3. **Probe the capability you will use, not just "can encode".** Per encoder: 8-bit and 10-bit (p010,
   `-profile:v main10`), and verify the output's `profile`, `pix_fmt`, and colour tags, and that mastering/CLL
   side data survives when fed a tagged synthetic source (a `testsrc2` with `-color_primaries bt2020
   -color_trc smpte2084` and a `setparams`/side-data injection; ASSUMED feasible via `-bsf:v hevc_metadata`
   on an intermediate). A device that does HEVC 8-bit but not Main10 (e.g. older Intel/NVIDIA; ASSUMED list)
   must refuse 10-bit jobs rather than hand them to auto-select.
4. **Size the probe to hardware minimums.** 160x120 is below some encoders' minimums (ASSUMED: NVENC HEVC
   minimum width ~129-145, VAAPI alignment 16/32/64). A false negative is safe (fails loud), but a 320x240 or
   640x360 probe removes the risk at negligible cost.
5. **Report a capability table once at startup** (device, driver, encoder, 8/10-bit, HDR metadata carried),
   and fail loud as today if the configured encoder is not in it. No fallback to CPU.

---

## Q6. Licences

- libx264: "GNU General Public License ... either version 2 of the License, or (at your option) any later version"
  (PRIMARY: https://code.videolan.org/videolan/x264/-/raw/master/x264.c header). libx265 is also GPL-2.0-or-later
  (ASSUMED). FFmpeg lists both in `EXTERNAL_LIBRARY_GPL_LIST`; nvenc/amf/vaapi/libvpl are not in any nonfree
  list, only `decklink libfdk_aac libmpeghdec` and `cuda_nvcc cuda_sdk` are (PRIMARY: configure @ 5d4d3bdc61).
  So the BtbN `-gpl` build with nvenc/amf/vaapi/qsv enabled is distributable.
- BtbN gpl variant: `FF_CONFIGURE="--enable-gpl --enable-version3 ..."`, `LICENSE_FILE="COPYING.GPLv3"`
  (PRIMARY: BtbN `variants/defaults-gpl.sh`), so the binary is GPL-3.0-or-later as a whole; GPL-2.0+ x264 is
  compatible with that via "or any later version".
- holdfast (AGPL-3.0-only) runs ffmpeg as a separate program via exec. AGPL s.5: "A compilation of a covered
  work with other separate and independent works ... is called an 'aggregate' ... Inclusion of a covered work
  in an aggregate does not cause this License to apply to the other parts" (PRIMARY: /workspace/LICENSE:223-231).
  Even if it were a combination, AGPL s.13 "Use with the GNU General Public License" permits combining with
  GPLv3 works (PRIMARY: LICENSE:540). Compatible.
- NOTICE implications:
  - NOTICE currently describes the bundle as "static builds carrying libx265, libsvtav1, libvmaf". The binary also
    carries libx264 (GPL-2.0+) and many other libraries; name libx264 explicitly and state that the full list and
    their licences are in the BtbN recipe at the tag.
  - GPLv3 s.6 requires the distributor to provide Corresponding Source for the ffmpeg binary. Pointing at BtbN's
    GitHub and upstream FFmpeg is a third-party server; GPLv3 s.6(d) allows that only while the distributor ensures
    it stays available (ASSUMED reading of s.6(d)). A release-time source mirror (tarball of FFmpeg @ rev + each
    dependency at BtbN's pinned commits) removes that dependence. Worth a decision, not urgent.
  - If VAAPI/QSV runtimes are added: each copied Debian package's `/usr/share/doc/<pkg>/copyright` must ship in
    the image, and NOTICE must list them. Most are MIT/BSD (libva, iHD, gmmlib, vpl-gpu-rt, Mesa, libdrm, xcb/X11);
    LLVM is Apache-2.0 WITH LLVM-exception (NOTICE file obligations); elfutils (LGPL-3+/GPL-2+) and libsensors
    (LGPL-2.1+) bring a source-offer obligation (ASSUMED licences; confirm from the copyright files).
    snapshot.debian.org URLs for the exact versions satisfy it cheaply.
  - AMF runtime: must NOT be in the image (Q2).

---

## Proposal T45: AMD on Linux

| Option | What | Cost | Risk |
|---|---|---|---|
| **A. AMD via VAAPI (Mesa radeonsi) in the image; `amf` refused in the image with a named reason** | Add Mesa VA driver + closure to an amd64 (and optionally arm64) image; document AMD = `encoder: vaapi`. Keep `amf` as a config key for host installs, but the image's startup check reports "AMF runtime is AMD-EULA software this image cannot ship; use encoder: vaapi" | Image size: mesa-libgallium ~42 MB + libLLVM19 (24.8 MB download, larger installed) + X/xcb libs, so roughly +100-150 MB (estimate). Moderate: needs the vaapi argv/probe fixes from Q3/Q5 anyway | Mesa lags new AMD parts; AMD HEVC quality is lowest of the three vendors (Jellyfin doc), so more strictly-smaller rejections |
| B. Bring-your-own AMF runtime | Document bind-mounting `/opt/amf` and setting `LD_LIBRARY_PATH` | Still needs Vulkan loader + RADV ICD + libamdenc inside the image; untestable in CI; per-user EULA | High support load for a path AMD itself told Linux users to leave |
| C. Ship AMF | - | - | Blocked: EULA grants install/use only, no redistribution, and s.3(e) bars distribution alongside Free Software licensing |
| D. Remove the `amf` key | Registry shrinks | Breaking config change for host users | Low value |

**Recommendation: A.** It is the vendor's recommended path, fully redistributable, arch-portable, and reuses
the VAAPI work Intel needs anyway. Keep `amf` for host runs (it costs nothing and fails loud), but never alias
`amf` to `vaapi` - they are different encoders, i.e. different measuring conditions, and silently swapping one
for the other is the "silent fallback" holdfast refuses. Ship Mesa in a separate image tag if the size is
unwelcome in the default image (see packaging note below).

---

## Proposal T10: gates for hardware encoders

| Option | What | Cost | Risk |
|---|---|---|---|
| **1. Same gates, unchanged, plus per-encoder quality calibration and an output fidelity gate** | Keep strictly-smaller, VMAF mean, worst-frame pool and chroma floors identical for every encoder. Replace "CRF reused as cq/qp/global_quality" with a per-encoder default quality value calibrated on the fixture corpus to land above the floors; add the Q3 fidelity gate (bit depth, chroma, colour tags, HDR10 side data) for all encoders | One calibration pass per encoder family on real hardware; fidelity gate is small and CPU-testable with x265/svtav1 fixtures | Lower savings per file with hardware, more "not smaller" skips. Both are honest and visible in the ledger |
| 2. Per-encoder VMAF thresholds with a hard floor | e.g. HW floors a few points below CPU, never below a global minimum | Config and docs complexity | Weakens the invariant: "never trade a good file for a worse one" becomes encoder-dependent; a lower worst-frame floor is exactly where hardware rate control fails. A threshold describes the output the operator will accept, and the encoder that made it is irrelevant to that |
| 3. Stricter gates for hardware | Raise floors for HW | Many more rejections | Punishes an encoder for being an encoder; same measurement already rejects bad frames |
| 4. Hardware first, CPU on rejection | Try HW; if a gate rejects or savings are below a threshold, requeue on `cpu` | Up to two encodes per rejected file; queue design | Useful later as a backlog-drain mode; must record both attempts; not needed for T10 itself |

**Recommendation: Option 1.** The gates are already defined on the output (quality-gate.md: the floors
bound what reaches the library regardless of what produced it), so hardware needs no special case there;
what hardware actually needs is (a) a quality knob that means something per encoder, so jobs do not waste a
full encode landing below the floor, and (b) the fidelity checks that the software path got implicitly from
libx265's `-x265-params` and that hardware paths lose silently (vaapi nv12, nvenc/qsv auto-select, nvenc
mastering metadata enabled only from stream side data). Option 4 is a sensible follow-on once Option 1 has
measured rejection rates on real hardware.

---

## Packaging note (applies to both proposals)

- **Base first:** move `RUNTIME_IMAGE` to `gcr.io/distroless/cc-debian13:nonroot` (pinned digest) - required
  for trixie libs, and upstream distroless now documents only debian13.
- **Variant tags:** default image stays small (CPU + NVIDIA); a `-hw` (or `-vaapi`) amd64 tag adds libva, iHD
  non-free, gmmlib, libmfx-gen and Mesa radeonsi. arm64 gets nothing (no Intel, rare AMD dGPU) unless demand
  appears. Cost: one more image to smoke-test; the smoke test cannot exercise a GPU in CI, so it should at least
  assert the libs resolve (ffmpeg `-init_hw_device vaapi` on a missing node must print a device error, not an
  `implib-gen` abort).
- **Debian packages vs source build:** Debian trixie gives signed, versioned, low-maintenance binaries but
  older drivers (iHD 25.2.3, Mesa 25.0.7) and no pre-TGL QSV. Jellyfin/linuxserver-style source builds give
  current hardware support at a large build/maintenance cost. Start with Debian; revisit if users report
  unsupported new GPUs.
- **NVIDIA fix independent of all this:** add `ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` (not a
  config key, so CLAUDE.md's "no ENV defaults for config keys" does not apply) or change docs to
  `capabilities: [gpu, video, compute, utility]`. Verify on real NVIDIA hardware before claiming it.

## Open items to verify on hardware (not provable in this container)

- nvenc actually loads with the documented compose snippet vs with `video` capability.
- Loader resolution of `/usr/lib/<triplet>` libs in distroless cc-debian13.
- Minimum probe dimensions per encoder; Main10 availability per GPU generation.
- Whether ffmpeg CLI propagates mastering metadata to `decoded_side_data` for MKV sources where it is in-band only.
- Calibrated per-encoder quality values that clear holdfast's default floors on the fixture corpus.
