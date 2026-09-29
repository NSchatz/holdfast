# P3 - AMD, AMF and the hardware runtime in the image (T45, I8, I9)

Proposal for Checkpoint T. Research: `research-hw-encode.md` Q1, Q2, Q6 and its packaging note, and
`verify-hw-encode.md` (its corrections override the research). Every claim a version, size, licence
or API rests on was re-checked on 2026-09-29 against a primary source; see "Claims re-verified".
Code references are at `30d245f`.

## What this decides

1. **AMD on Linux (T45):** ship AMD's AMF runtime, support AMF on host installs only, or encode on
   AMD through Mesa's VAAPI driver (`radeonsi`).
2. **Where the VAAPI/QSV runtime lives (I8):** in the default image, or in a separate `-hw` tag.
3. **`amf` in the image (I9):** refused at start with a named reason, kept for host installs.

**Flag, stated plainly: VAAPI-only was NOT chosen as a final answer in T45.** T45 lists "VAAPI-only
(as a final decision)" and "separate AMF image variant (as a final decision)" as not chosen. The
recommendation below makes AMD in the shipped image go through VAAPI only, while keeping `amf` for
host installs; approving it is the owner's deliberate choice, not a default the program may assume.

## Where things stand

- **Registry:** `qsv` is `hevc_qsv`, `vaapi` is `hevc_vaapi`, `amf` is `hevc_amf`
  (`internal/encoder/encoder.go:39-47`). `Available()` probes every encoder with one generic argv
  (`internal/encoder/encoder.go:117-140`) that has no device and no `hwupload`, so it cannot pass for
  `hevc_vaapi` on a working host (verify claim 7b). VAAPI jobs open a hard-coded
  `/dev/dri/renderD128` (`internal/engine/encode.go:352-357`).
- **Image:** runtime base `gcr.io/distroless/cc-debian12:nonroot` pinned by digest
  (`Dockerfile:25`); it deliberately carries no vendor GPU userspace (`Dockerfile:135-141`), and
  `docs/docker.md:362-375` and `:412-414` say QSV, VAAPI and AMF do not work in the image.
- **The pinned ffmpeg** (`Dockerfile:55-58`) is built, on amd64, with `--enable-vaapi`,
  `--enable-libvpl` and `--enable-amf` (`ffmpeg -buildconf`); the arm64 build has no VAAPI at all
  (BtbN `scripts.d/50-vaapi/50-libva.sh:16` returns early for `linuxarm64`). At the pin tag, BtbN's recipe builds libvpl static and not
  at all for arm64 (`scripts.d/50-onevpl.sh:7,19`), compiles libva's driver directory as
  `/usr/lib/x86_64-linux-gnu/dri` (`scripts.d/50-vaapi/50-libva.sh:43`), and takes only AMF's
  public headers (`scripts.d/50-amf.sh`). libva and libdrm are loaded lazily and a missing one aborts
  the process (`verify-hw-encode.md` claim 1); `connection_type=drm` keeps a missing render node
  away from the X11 shim (verify's correction).

### AMF

- **The runtime** is `libamfrt64.so.1`, in AMD's `amf-amdgpu-pro` package. The newest listed is
  `26.10.499-1` (under `repo.radeon.com/amf/26.10.1/`, packages for Ubuntu, RHEL and SLE only), which
  installs `/opt/amf/lib/x86_64-linux-gnu/libamfrt64.so.1.5.2`, is amd64 only, and declares
  `Depends: libc6 (>= 2.38), libdrm2 (>= 2.4.47), libstdc++6 (>= 13.1), libvulkan1,
  libamdenc-amdgpu-pro` (control file of the downloaded deb, not installed).
- **Its licence** is the "AMDGPU PRO EULA", identical in the deb and at `repo.radeon.com/amf/copyright`.
  Section 2 grants "a non-exclusive, royalty-free, revocable, non-transferable, limited, copyright
  license to a) install and use the Software solely in Object Code form in conjunction with systems
  or components that include or incorporate AMD processors". Section 3 continues: "Except for the
  limited license expressly granted in Section 2 herein, You have no other rights in the Software",
  and 3(e) forbids to "use, modify and/or distribute any of the Software or Documentation so that any
  part becomes subject to a Free Software License." **There is no redistribution clause to quote:
  the EULA grants none.** A public image of an AGPL project cannot carry the runtime.
- **The SDK headers** ffmpeg compiles against are MIT (`LICENSE.txt` of the AMF repository); they
  are not the runtime.
- **AMD's direction:** its Radeon Software for Linux 25.10.1 release notes say, for releases from
  25.20: "AMF will no longer be included in the release. AMF users are advised to transition to
  VA-API / Mesa Multimedia", with ffmpeg VAAPI examples. The AMF README: "starting with the 25.20
  Linux driver, the AMF runtime is released separately".

### AMD via Mesa VAAPI, and Intel, from Debian trixie

| Package (trixie) | Version | Section | Installed, amd64 | Licence (Debian copyright) |
|---|---|---|---|---|
| `libva2` / `libva-drm2` | 2.22.0-3 | main | 250 kB / 44 kB | Expat |
| `libdrm2` / `libdrm-amdgpu1` | 2.4.124-2 | main | 128 kB / 81 kB | MIT-style permission notice |
| `mesa-va-drivers` (VA for gallium drivers incl. `radeonsi`) | 25.0.7-2+deb13u1 | main | 47 kB | `Files: *` MIT |
| `mesa-libgallium` (depended on at the same version) | 25.0.7-2+deb13u1 | main | 41,615 kB (arm64 34,238) | as above |
| `libllvm19` (a `mesa-libgallium` dependency) | 1:19.1.7-3+b1 | main | 126,696 kB (arm64 120,416) | Apache-2.0 with LLVM exceptions, and others |
| `libz3-4` (a `libllvm19` dependency) | 4.13.3-1 | main | 27,142 kB | not re-checked |
| `intel-media-va-driver` (free-kernel build) | 25.2.3+dfsg1-1 | main | 17,751 kB | Expat, BSD-3-clause |
| `intel-media-va-driver-non-free` (full-feature build) | 25.2.3+ds1-1 | non-free | 40,457 kB | Expat, BSD-3-clause |
| `libigdgmm12` | 22.7.2+ds1-1 | main | 858 kB | Expat, BSD-3-clause |
| `libmfx-gen1.2` (QSV GPU runtime, Tiger Lake and newer) | 25.1.4-1 | main, amd64 only | 8,496 kB | MIT |
| `libvpl2` (dispatcher; ffmpeg links it statically, not needed) | 1:2.14.0-1+b1 (amd64) | main | 415 kB | MIT, BSD-3-clause, Apache-2.0 |
| `libmfx1` (legacy Media SDK runtime) | not in trixie | - | - | - |

- Debian's trixie Mesa build enables VA for gallium with `-Dvideo-codecs="all"` and builds
  `radeonsi` on every LLVM architecture, amd64 and arm64 included (salsa `debian/rules` lines 54,
  113-116, 153-154). AV1 encode additionally needs a VCN generation that has it (ASSUMED: RDNA3 and
  newer).
- Debian's free iHD says "Only a limited set of encoders is available via this driver: JPEG (Skylake
  and newer), AVC (Boxton and newer), HEVC and VP9 (Ice Lake and newer)"; Intel's README calls the
  non-free package the "Full feature build". The non-free section is a source-availability matter:
  "we have to move those kernels to non-free as they come without source" (its Debian copyright);
  the licence is still Expat and BSD-3-clause and Debian redistributes it.
- QSV on Intel older than Tiger Lake has no runtime in trixie (`libmfx1` absent); VAAPI through
  iHD still covers those parts.
- The Intel VPL dispatcher finds the runtime on Linux through `LD_LIBRARY_PATH`, then the default
  paths ("On Debian: /usr/lib/x86_64-linux-gnu"), then `ONEVPL_SEARCH_PATH`, with no
  `/etc/ld.so.cache` step (that step belongs to the legacy dispatcher), so `libmfx-gen1.2` at the
  multiarch path is found without a loader cache.
- **glibc:** most trixie packages above need `libc6 (>= 2.38)` (`libva-drm2` and `libigdgmm12` need
  only `>= 2.34`, `mesa-va-drivers` declares none; iHD, `libmfx-gen1.2` and `libz3-4` also need
  `libstdc++6 (>= 14)`);
  trixie ships glibc 2.41-12+deb13u4 and libstdc++6 14.2.0-19, while bookworm ships glibc
  2.36-9+deb12u14, so the goal-2 move to `cc-debian13` is a prerequisite. Distroless says its images
  "are based on Debian 13 (trixie)", lists `gcr.io/distroless/cc-debian13` with `nonroot`, and states
  Debian 13 images use the UsrMerge scheme, so `/lib` and `/usr/lib` are one tree. Whether the loader
  resolves copied libraries without an `ld.so.cache` is proven by the image smoke step, not assumed.

**Size, from the installed sizes above (amd64, package-level `Depends` closure; the exact `ldd`
closure is goal 5's job):** libva and libdrm about 0.4 MB; the Intel set with non-free iHD about
50 MB (27 MB with the free iHD); the AMD set about 205 MB, of which `libllvm19`, `mesa-libgallium`
and `libz3-4` are 195 MB. Total about 255 MB on amd64. For scale, the pinned amd64 `ffmpeg` and
`ffprobe` binaries are about 292 MB together (measured locally). The research's "+100-150 MB" for
Mesa was an underestimate (corrected).

## Options

### (a) The runtime in the default image; `amf` refused in-image with a named reason; AMD via VAAPI

The research recommendation and I9. The default image carries libva, libva-drm, libdrm, Intel iHD
(non-free), `libigdgmm12`, `libmfx-gen1.2` and Mesa `radeonsi` VA with its closure on amd64; arm64
stays without a hardware runtime and says so: the pinned arm64 ffmpeg is built without VAAPI
(`50-libva.sh:16`), libvpl is not built for arm64 either, and arm64 boards with Intel or AMD
encode hardware are rare. `amf` stays a valid key: on a host install it works as today; in the image `Available()`
refuses it at start, naming the EULA and pointing at `encoder: vaapi`. It is never aliased to
`vaapi`: they are different encoders, so different measuring conditions.

Costs: about +255 MB on amd64 for every user, including CPU-only ones; a larger third-party surface
(LLVM, Mesa, X client libraries) to pin, update and list in `NOTICE` with each copyright file; a
corresponding-source duty for the LGPL or GPL members of the closure (`libsensors5`, `libelf1t64`),
which a published image must honour; a
DFSG non-free component (iHD kernels without source) inside an AGPL project's image, stated in
`NOTICE`; Debian's drivers lag the newest GPUs (iHD 25.2.3, Mesa 25.0.7); CI can prove only that the
libraries resolve, never an encode. T12 asked for AMF "working in the shipped image": this delivers
AMD encode in the image, but not through AMF. Gain: one image, and `docker run` works on NVIDIA,
Intel and AMD hosts.

### (b) The same runtime in a separate `-hw` tag

The default image keeps today's size; a `-hw` amd64 tag carries the runtime of (a) with the same
`amf` refusal.

Costs: two images to build, pin, smoke-test, publish and document; the release matrix grows; an
operator who picks the wrong tag gets a loud `Available()` failure rather than a working encode;
T47's "a tagged minor release whose image carries every chosen area" then means the `-hw` image, and
the owner should say so. Code, argv and detection are identical to (a).

### (c) AMF in a separate image variant

Costs: publishing it would distribute the runtime, which the EULA does not permit (install and use
only; "no other rights"; 3(e) against distribution that brings it under a Free Software License).
The only lawful shape (ASSUMED reading, not legal advice) is a recipe the operator builds
privately after accepting AMD's EULA themselves, never published by this project: it needs AMD's
Ubuntu, RHEL or SLE packages on a matching base plus a Vulkan loader and ICD and
`libamdenc-amdgpu-pro`, it cannot be built or tested in CI, and AMD itself advises moving to VA-API.

### (d) Remove the `amf` key

A breaking change for host installs that work today, for no safety gain. Not recommended.

## Recommendation

Approve option (a): the VAAPI/QSV runtime in the default amd64 image from pinned Debian trixie
packages (after goal 2's `cc-debian13` base), AMD encoded through Mesa `radeonsi` with
`encoder: vaapi`, and `amf` kept for host installs but refused in the image at start with a named
reason (AMD's EULA grants no redistribution), never aliased to `vaapi`. It is AMD's own advice, fully
redistributable, and one image keeps `docker run` working on every vendor. If the owner weighs the
roughly 255 MB against CPU-only users more heavily, choose (b): the work is the same plus one more
release target.

Stated plainly: VAAPI-only was not chosen as a final answer in T45, and (a) makes AMD in the shipped
image VAAPI-only, so approving it is the owner's deliberate choice.

## Test plan

All on fakes, image smoke steps and golden argv; no GPU is ever opened (T9), and hardware tests
stay behind the `hwlive` build tag (`rg -n hwlive .github Makefile` prints nothing).

- **Pins:** each Debian package pinned by version and sha256 (or a snapshot URL) and covered by
  `scripts/check-pins.sh`, with a bite test; `NOTICE` names every copied package and licence, and a
  test reds when the Dockerfile's package list and `NOTICE` disagree.
- **Image smoke (CI `package` job, no device):** a VAAPI device init with `connection_type=drm` on a
  missing render node ends in a device error, never exit 134 or an `implib-gen` line; each driver
  object's dependencies resolve inside the image (for example the loader's `--list` mode, ASSUMED
  usable without a shell).
- **`amf` refusal:** with the image marker set, `RequireAvailable("amf")` returns the named EULA
  reason without probing; `holdfast validate` still accepts `encoder: amf`; a test proves `amf` never
  resolves to `vaapi`.
- **Argv:** golden argv for `vaapi` with `connection_type=drm`, `format=p010` and `main10` (goal 5).
- **Real hardware:** NEEDS-OWNER hardware reports for VAAPI on Intel, VAAPI on AMD, QSV, and AMF on
  a host install (goal 6, T43).

## Claims re-verified

| Claim | Source (URL, read 2026-09-29) | Outcome |
|---|---|---|
| AMF runtime is under an EULA with no redistribution grant | https://repo.radeon.com/amf/copyright ; https://repo.radeon.com/amf/26.10.1/ubuntu/pool/main/noble/amf-amdgpu-pro_26.10.499-1_amd64.deb | confirmed; quoted sections 2, 3 and 3(e) |
| Runtime version 25.20-399, `libc6 (>= 2.34)`, `libstdc++6 (>= 11)` | same deb, control file | corrected: newest is 26.10.499-1 (`libamfrt64.so.1.5.2`), needing `libc6 (>= 2.38)` and `libstdc++6 (>= 13.1)` |
| AMF SDK headers are MIT | https://github.com/GPUOpen-LibrariesAndSDKs/AMF/blob/master/LICENSE.txt | confirmed |
| AMF runtime released separately from 25.20 | https://github.com/GPUOpen-LibrariesAndSDKs/AMF/blob/master/README.md | confirmed |
| AMD advises moving from AMF to VA-API / Mesa | https://www.amd.com/en/resources/support-articles/release-notes/RN-AMDGPU-UNIFIED-LINUX-25-10-1.html | confirmed from the primary page (was LEAD in the research) |
| Mesa VA with `radeonsi`, `video-codecs=all`, amd64 and arm64 | https://packages.debian.org/trixie/mesa-va-drivers ; https://salsa.debian.org/xorg-team/lib/mesa/-/raw/debian-trixie/debian/rules | confirmed |
| Mesa licence MIT | https://metadata.ftp-master.debian.org/changelogs//main/m/mesa/mesa_25.0.7-2+deb13u1_copyright | confirmed for `Files: *`; other files carry other licences |
| Mesa adds "roughly +100-150 MB" | https://packages.debian.org/trixie/libllvm19 ; https://packages.debian.org/trixie/mesa-libgallium ; https://packages.debian.org/trixie/libz3-4 | corrected: about 205 MB installed on amd64 |
| Free vs non-free iHD, versions, sections, licences | https://packages.debian.org/trixie/intel-media-va-driver ; https://packages.debian.org/trixie/intel-media-va-driver-non-free ; https://metadata.ftp-master.debian.org/changelogs//non-free/i/intel-media-driver-non-free/intel-media-driver-non-free_25.2.3+ds1-1_copyright ; https://github.com/intel/media-driver/blob/master/README.md | confirmed |
| `libmfx-gen1.2` amd64-only, Tiger Lake and newer; `libmfx1` absent from trixie | https://packages.debian.org/trixie/libmfx-gen1.2 ; https://packages.debian.org/trixie/libmfx1 ; https://github.com/intel/vpl-gpu-rt/blob/main/README.md | confirmed |
| libvpl dispatcher search order | https://intel.github.io/libvpl/latest/programming_guide/VPL_prg_session.html | corrected by the adversarial verification: the Intel VPL dispatcher's Linux order has no `ld.so.cache` step; the quoted list was the legacy dispatcher's |
| libvpl static and arm64-disabled; libva driver dir | https://github.com/BtbN/FFmpeg-Builds/blob/autobuild-2026-07-31-14-10/scripts.d/50-onevpl.sh ; https://github.com/BtbN/FFmpeg-Builds/blob/autobuild-2026-07-31-14-10/scripts.d/50-vaapi/50-libva.sh | confirmed at the pin tag (the research read master); the adversarial verification added that `50-libva.sh:16` builds arm64 without VAAPI |
| trixie packages need glibc >= 2.38; trixie glibc and libstdc++ versions | https://packages.debian.org/trixie/libc6 ; https://packages.debian.org/bookworm/libc6 ; https://packages.debian.org/trixie/libstdc++6 | confirmed: trixie 2.41, bookworm 2.36, libstdc++6 14.2.0; corrected by the adversarial verification: most, not every, package needs 2.38 (`libva-drm2` and `libigdgmm12` need 2.34) |
| distroless is Debian 13 based, `cc-debian13` exists, UsrMerge | https://github.com/GoogleContainerTools/distroless/blob/main/README.md | confirmed; UsrMerge is new relative to the research |
| AV1 encode on AMD needs RDNA3 or newer | none fetched | not verified (ASSUMED) |
| `libz3-4`, X client and other small dependency licences | none fetched | not verified; goal 5 reads each copyright file |

## Sources

- AMD: https://repo.radeon.com/amf/copyright ;
  https://repo.radeon.com/amf/26.10.1/ubuntu/pool/main/noble/amf-amdgpu-pro_26.10.499-1_amd64.deb
  (downloaded and unpacked for its control and copyright files, not installed);
  https://www.amd.com/en/resources/support-articles/release-notes/RN-AMDGPU-UNIFIED-LINUX-25-10-1.html ;
  https://github.com/GPUOpen-LibrariesAndSDKs/AMF/blob/master/README.md ;
  https://github.com/GPUOpen-LibrariesAndSDKs/AMF/blob/master/LICENSE.txt ; all read 2026-09-29.
- Debian (suite trixie unless named), read 2026-09-29: https://packages.debian.org/trixie/libva2 ,
  https://packages.debian.org/trixie/libva-drm2 , https://packages.debian.org/trixie/libdrm2 ,
  https://packages.debian.org/trixie/libdrm-amdgpu1 ,
  https://packages.debian.org/trixie/mesa-va-drivers ,
  https://packages.debian.org/trixie/mesa-libgallium , https://packages.debian.org/trixie/libllvm19 ,
  https://packages.debian.org/trixie/libz3-4 ,
  https://packages.debian.org/trixie/intel-media-va-driver ,
  https://packages.debian.org/trixie/intel-media-va-driver-non-free ,
  https://packages.debian.org/trixie/libigdgmm12 , https://packages.debian.org/trixie/libmfx-gen1.2 ,
  https://packages.debian.org/trixie/libvpl2 , https://packages.debian.org/trixie/libmfx1 ,
  https://packages.debian.org/trixie/libc6 , https://packages.debian.org/trixie/libstdc++6 ,
  https://packages.debian.org/bookworm/libc6 , the copyright files under
  https://metadata.ftp-master.debian.org/changelogs/ for libva 2.22.0-3, libdrm 2.4.124-2, mesa
  25.0.7-2+deb13u1, intel-media-driver 25.2.3+dfsg1-1, intel-media-driver-non-free 25.2.3+ds1-1,
  intel-gmmlib 22.7.2+ds1-1, onevpl-intel-gpu 25.1.4-1, libvpl 2.14.0-1 and llvm-toolchain-19
  19.1.7-3, and https://salsa.debian.org/xorg-team/lib/mesa/-/raw/debian-trixie/debian/rules .
- Intel, read 2026-09-29: https://github.com/intel/media-driver/blob/master/README.md ,
  https://github.com/intel/vpl-gpu-rt/blob/main/README.md ,
  https://intel.github.io/libvpl/latest/programming_guide/VPL_prg_session.html .
- Mesa: https://docs.mesa3d.org/envvars.html (VA-API and `radeonsi` variables), read 2026-09-29.
- Distroless: https://github.com/GoogleContainerTools/distroless/blob/main/README.md , read
  2026-09-29.
- BtbN build recipe at the pin tag, read 2026-09-29:
  https://github.com/BtbN/FFmpeg-Builds/tree/autobuild-2026-07-31-14-10/scripts.d
- holdfast at `30d245f`: `Dockerfile`, `docs/docker.md`, `internal/encoder/encoder.go`,
  `internal/engine/encode.go`; the pinned ffmpeg's `-buildconf`, run 2026-09-29.
- The adversarial verification of this proposal: `verify-proposals.md` (read 2026-09-29).
