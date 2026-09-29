# Verification: research-hw-encode.md (adversarial pass)

Read 2026-09-29. Repo `/workspace` @ `3c229da`, unchanged. Local binary: `/cache/ffmpeg/bin/ffmpeg`
(`N-125875-g5d4d3bdc61-20260731`; note `which ffmpeg` resolves to a mise shim, so `ldd $(readlink -f $(which ffmpeg))`
inspects mise, not ffmpeg). Container has no `/dev/dri`, no libva*.so.2, but does have libdrm.so.2, libX11.so.6, libxcb.so.1.
All ffmpeg tests: lavfi testsrc2, 0.1-0.2 s, 160x120.

## Verdicts

| # | Claim | Verdict |
|---|---|---|
| 1 | libva/libdrm dlopened; SIGABRT when missing | CONFIRMED, with a correction (see below: "explicit node avoids the X11 shim" is false) |
| 2 | QSV needs libmfx-gen1.2 (TGL+), amd64-only, trixie dropped libmfx1 | CONFIRMED |
| 3 | non-free iHD redistributable (Expat/BSD-3), needed for encode pre-Ice-Lake | CONFIRMED for HEVC; overstated for "encode" generally |
| 4 | trixie Mesa 25.0.x radeonsi VA, video-codecs=all, amd64+arm64 | CONFIRMED (build flag; AV1 encode still needs VCN4+ hardware) |
| 5 | trixie libs need glibc >= 2.38, base is cc-debian12, distroless documents -debian13 | CONFIRMED, plus a second blocker (libstdc++6 >= 14) |
| 6 | AMF runtime under AMD EULA, no redistribution; AMD says use VA-API/Mesa | CONFIRMED (EULA read from the deb; AMD quote via search index, amd.com fetch timed out) |
| 7 | vaapi `format=nv12,hwupload` makes 10-bit 8-bit; `Available()` vaapi probe can never pass | CONFIRMED (both); 7a is broader than stated |
| 8 | CRF reused as nvenc -cq / qsv -global_quality / vaapi -qp | CONFIRMED |
| 9 | default caps `utility,compute`; `capabilities: [gpu]` => libnvidia-encode not mounted | REFUTED as a blanket claim: stale for Docker >= 29.2 + NCT >= 1.18 (CDI path); true only on the legacy hook path |
| 10 | HW HEVC/AV1 near x265 faster/medium | UNCERTAIN (overstated): the cited paper supports "faster" in real time only; nothing for "medium" |
| 11 | libx264 GPL-2.0+ in GPLv3 ffmpeg beside AGPL program via exec is compatible | CONFIRMED |

## Evidence

### 1. Implib shims and abort
- Binary strings (python scan of `/cache/ffmpeg/bin/ffmpeg`): `implib-gen: <lib>: failed to load library ...` for
  libva.so.2, libva-drm.so.2, libva-x11.so.2, libdrm.so.2, libX11.so.6, libXext.so.6, libXv.so.1, libxcb.so.1,
  libxcb-shm/-shape/-xfixes. `ldd` shows none of them as NEEDED (only libc/libm/libdl/librt/libpthread/libmvec/libgcc_s).
- `env -i PATH=... LD_LIBRARY_PATH=` runs, scratch dir:
  - `-init_hw_device vaapi=va:/dev/null` (open() succeeds, reaches vaGetDisplayDRM): `implib-gen: libva-drm.so.2: failed to load library` then
    `Assertion '0 && "Assertion in generated code"' failed`, **exit 134 (SIGABRT, core)**.
  - `-init_hw_device vaapi=va` (default device, no node): **exit 134**, same message.
  - `-init_hw_device qsv=hw` and `qsv=hw:/dev/null`: **exit 134** (QSV child VAAPI device).
  - Repo shape `-vaapi_device /dev/dri/renderD128`, no node: clean error, exit 234 ("No VA display found").
  - `Available()` qsv shape (`-c:v hevc_qsv`, no device): clean, exit 171 ("Error creating a MFX session: -9"), 0-byte file.
- So the abort is real, but "any vaapi/qsv call is a SIGABRT" is overstated: it happens when a code path reaches a shimmed symbol.
- **Correction to the research (Q1 "Practical rule", Packaging note smoke test):** a scratch copy of ffmpeg with the
  `libX11.so.6` soname string patched (simulating distroless, which has no libX11) aborts on the repo's own
  `-vaapi_device /dev/dri/renderD128` and on `-init_hw_device vaapi=va:/dev/dri/renderD128` when the node is missing:
  `implib-gen: libX11.so.6: failed to load library`, **exit 134**. An explicit node that fails to open falls through to
  `XOpenDisplay(device)`. Adding `,connection_type=drm` gives a clean exit 234. The proposed smoke test ("missing node
  must print a device error") would red in a libX11-less image even with libva shipped unless it uses `connection_type=drm`.
- libdrm-missing path not exercised (libdrm.so.2 is present here); same template strings are in the binary.

### 2. QSV runtime
- packages.debian.org exact search `libmfx1`: bullseye 21.1.0-1, bookworm 22.5.4-1, amd64 only; **no trixie**.
- `libmfx-gen1.2`: bookworm 22.6.4-1, trixie 25.1.4-1, forky/sid 26.3.3-1; **amd64 only** in every suite.
- BtbN `scripts.d/50-onevpl.sh` (master): `[[ $TARGET == *arm64 ]] && return -1`, `-DBUILD_SHARED_LIBS=OFF`.
- Binary also carries `libmfxhw64.so.1` and `libmfx-gen.so.1.2` strings (dispatcher can load either).
- "Tiger Lake+" for vpl-gpu-rt not re-fetched (research cites the README).

### 3. iHD non-free
- Debian copyright (metadata.ftp-master .../intel-media-driver-non-free/unstable_copyright): only `Expat` and `BSD-3-clause`;
  non-free because "kernels ... come without source". No redistribution restriction; Debian itself redistributes it.
- packages.debian.org/trixie/intel-media-va-driver (free): "Only a limited set of encoders is available via this driver:
  JPEG (Skylake and newer), AVC (Broxton and newer), HEVC and VP9 (Ice Lake and newer)." So pre-ICL **AVC** encode works
  without non-free; the claim holds for **HEVC** (holdfast's only vaapi/qsv target).

### 4. Mesa
- packages.debian.org/trixie/mesa-va-drivers: 25.0.7-2+deb13u1; amd64, arm64, armel, armhf, i386, ppc64el, riscv64, s390x.
- salsa `debian-trixie/debian/rules`: `-Dvideo-codecs="all"`; radeonsi in `LLVM_ARCHS` (includes arm64); VA enabled unless
  `pkg.mesa.nolibva` profile. Build flag only: AV1 encode needs VCN4 (RDNA3) or later hardware.

### 5. Base image and glibc
- `Dockerfile:25` `ARG RUNTIME_IMAGE=gcr.io/distroless/cc-debian12:nonroot@sha256:ce0d66...`.
- trixie `libva2` 2.22.0-3 Depends `libc6 (>= 2.38)`; trixie `intel-media-va-driver-non-free` 25.2.3+ds1-1 Depends
  `libc6 (>= 2.38)` **and `libstdc++6 (>= 14)`** (bookworm ships GCC 12's libstdc++, a second blocker the research omits).
- distroless README: "Distroless images are based on Debian 13 (trixie)"; table lists only `-debian13` images.
- Not every trixie-era binary needs 2.38 (the Ubuntu AMF deb needs `libc6 (>= 2.34)`), but every package the research plans to copy does.

### 6. AMF
- Downloaded `repo.radeon.com/amf/25.20/ubuntu/pool/main/noble/amf-amdgpu-pro_25.20-399_amd64.deb` (4.56 MB). Control:
  `Depends: libc6 (>= 2.34), libdrm2 (>= 2.4.47), libstdc++6 (>= 11), libvulkan1, libamdenc-amdgpu-pro`. Ships
  `/opt/amf/lib/x86_64-linux-gnu/libamfrt64.so.1.5.0`. `copyright`: `License: AMDGPU PRO EULA`; s.2 grants only "install and
  use the Software solely in Object Code form"; s.3 "You have no other rights in the Software"; s.3(e) no distribution "so that
  any part becomes subject to a Free Software License". No redistribution grant.
- AMD statement "AMF will no longer be included in the release. AMF users are advised to transition to VA-API / Mesa Multimedia"
  surfaced by search on amd.com RN-AMDGPU-UNIFIED-LINUX-25-10-1 (and HandBrake discussion #6931); direct amd.com fetch timed out (60 s).

### 7. VAAPI argv and probe
- 7a: `internal/engine/encode.go:814-816` `-vf format=nv12,hwupload` (quality path) and `:926-928` (bitrate path). `:781` also emits
  the universal `-pix_fmt <pixFmt>`; `hdr.DerivePixFmt` maps even 8-bit `yuv420p` to `yuv420p10le` (hdr_test.go:236), so **every**
  vaapi job, not only 10-bit ones, is forced to 8-bit nv12. `ffmpeg -h encoder=hevc_vaapi` lists only `vaapi`, so the requested
  `-pix_fmt yuv420p10le` is replaced by auto-select. Local proof that auto-select is silent at holdfast's `-loglevel error`:
  `-pix_fmt yuv422p10le -c:v mpeg2video` exits 0 and writes `yuv422p` with no message; at `-loglevel warning` it prints
  "Incompatible pixel format ... auto-selecting". No `-profile:v main10` anywhere. End-to-end not runnable (no device).
- 7b: `internal/encoder/encoder.go:127-131` probe argv is `-f lavfi -i testsrc2=... -c:v <codec> -- out` with no device and no
  hwupload; called from `cmd/holdfast/main.go:523,535` via `RequireAvailable`. Running that argv with `hevc_vaapi` fails in
  **format negotiation**, before any device: "Impossible to convert between the formats supported by the filter 'Parsed_null_0'
  and the filter 'auto_scale_0'", exit 218, 0-byte file. That failure is independent of hardware, so it would also fail on a
  working VAAPI host. (Separately `Encode` hard-codes `/dev/dri/renderD128`, `encode.go:356`.)

### 8. Quality knob
- `internal/engine/encode.go:804` `-cq` CRF (nvenc/av1_nvenc), `:809` `-global_quality` CRF (qsv), `:816` `-qp` CRF (vaapi),
  `:821-822` `-qp_i/-qp_p` CRF (amf); doc comment `:760` "CRF reused as the CQ target". Quality path only; `bitrateArgs`
  (`:902-939`) drops them.

### 9. NVIDIA capabilities
- Confirmed facts: NVIDIA docs (docker-specialized.html): unset/empty => "use default driver capability: `utility`, `compute`";
  `video` "required for using the Video Codec SDK". moby `daemon/devices_nvidia_linux.go` (master): `allNvidiaCaps` =
  compute, compat32, graphics, utility, video, display; `gpu` is not one. Repo sets no `NVIDIA_*`: Dockerfile grep empty;
  `docker-compose.yml:121-123` and `docs/docker.md:353-355` use `driver: nvidia` + `capabilities: [gpu]`.
- **What makes the conclusion stale:** the same moby file now registers an `nvidia.cdi` driver when `nvidia-cdi-hook` is on PATH and
  the composite `nvidia` driver tries **CDI first**, falling back to the runtime hook only if CDI injection fails. The CDI path
  sets no `NVIDIA_DRIVER_CAPABILITIES`. moby PR #50228 "Use cdi device driver to handle nvidia --gpus requests", merged
  2026-01-20, milestone **29.2.0**. NVIDIA Container Toolkit v1.18.0: default mode is jit-cdi and a systemd unit generates CDI
  specs automatically. The toolkit's `pkg/nvcdi/driver-nvml.go` puts `libraries.Locate("*.so." + version)` into the spec with
  **no capability filtering**, so `libnvidia-encode.so.<ver>` and `libnvcuvid.so.<ver>` are included.
- So on Docker >= 29.2 with toolkit >= 1.18 (the current stack) the documented `capabilities: [gpu]` snippet very likely
  **does** mount libnvidia-encode. The research is right only for older Docker or hosts without `nvidia-cdi-hook` or a CDI
  spec (legacy runtime-hook path). Adding `ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` is still a harmless fix
  for the legacy path. Not tested on hardware.
- Search also surfaced moby#52048 (CDI for AMD `--gpus`, Docker 29.x); not checked further.

### 10. Efficiency
- arXiv 2511.18686 (HTML, read 2026-09-29): anchors are the slowest **real-time** presets on an i7-13700H: libx264 slow,
  **libx265 faster**, SVT-AV1 preset 8. The authors say it "wouldn't be fair to compare hardware encoders to the slowest
  software encoder preset". The figures are averages across codecs (HEVC/VP9/AV1/VVC). There is **no** comparison against
  x265 medium or slow. The text also says NVENC needs "about 10% more bits", which the research summary leaves out.
  Hardware AV1 is compared with SVT-AV1 p8, not x265.
- "medium" rests only on a single-clip SSIMULACRA2 blog (Rosato 2023) and a chained derivation. Fora Soft figures were not
  re-fetched (time).

### 11. Licences
- Local `ffmpeg -L`: "either version 3 of the License, or (at your option) any later version" (GPLv3+). `--enable-gpl
  --enable-version3`, no `--enable-nonfree`, `--disable-libfdk-aac` (local `-version` configuration line).
- x264 GPL-2.0-or-later (research cites the x264.c header; not re-fetched) folds into GPLv3+ via "or any later version".
- `/workspace/NOTICE:3` "AGPL-3.0-only"; holdfast execs ffmpeg as a separate program (argv + pipes). That is "aggregate" under
  AGPL s.5 (LICENSE), and s.13 permits GPLv3 combination anyway. Compatible. The GPLv3 s.6 Corresponding Source duty for the
  shipped binary is separate and still applies.
