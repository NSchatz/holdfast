# syntax=docker/dockerfile:1
#
# Production image (TRANSCODE-9). Multi-arch (linux/amd64 + linux/arm64), non-root,
# no shell, bundling a PINNED, CHECKSUM-VERIFIED ffmpeg that carries libx265 +
# libsvtav1 + libvmaf, and on amd64 the pinned VAAPI/QSV runtime (hwruntime stage).
#
# Why the ffmpeg pin is load-bearing rather than cosmetic: a distro ffmpeg can conceal
# HEVC corruption on decode-to-null, so VMAF is the real quality gate (roadmap §6). An
# ffmpeg without libvmaf makes that gate unmeasurable — and the engine REJECTS an
# unmeasured output rather than accepting it, so the wrong ffmpeg does not quietly
# weaken the no-loss contract, it stops the tool. The pin here is the SAME build CI
# runs the fixture safety proof against, so the image ships the ffmpeg that was proven.
#
# Every stage that RUNs anything is pinned to $BUILDPLATFORM, and the Go binary is
# cross-compiled (CGO_ENABLED=0 — pure-Go SQLite, no cgo), so the arm64 image needs no
# QEMU: its runtime stage only COPYs.
#
# Every stage is Debian 13 (trixie). The runtime has to be: trixie's VA-API stack (libva2,
# Mesa's gallium drivers, Intel's iHD driver) needs glibc 2.38 or later, and the debian12
# base ships 2.36, so this base comes first among the image changes. distroless documents
# only its debian13 images now. The build and fetch stages follow it, so the zone
# database the runtime stage copies from the build stage comes from the same Debian
# release as the base. Read 2026-09-29:
#   https://github.com/GoogleContainerTools/distroless/blob/main/README.md
#   https://packages.debian.org/trixie/libc6 (2.41-12+deb13u4)
#   https://packages.debian.org/bookworm/libc6 (2.36-9+deb12u14)
#   https://packages.debian.org/trixie/libva2 , .../trixie/mesa-libgallium ,
#   .../trixie/intel-media-va-driver-non-free (each: libc6 >= 2.38)
#
# Each base image is pinned ON ITS OWN FROM LINE, tag and digest together. That line is
# what Docker pulls, and it is the only form the update bot reads: Dependabot's Docker
# parser matches `FROM [--platform=...] <image>:<tag>@sha256:<digest>` and never resolves
# an ARG, so a base image written through an ARG is a pin nobody is ever told has gone
# stale (dependabot-core docker/lib/dependabot/docker/file_parser.rb, FROM_LINE, at
# 78005a8; read 2026-09-29). scripts/check-pins.sh section 7 refuses a base image written
# any other way.
#
# Each digest is the multi-arch index its tag resolved to when it was pinned, read from
# the registry's v2 API with the body's own sha256 checked against it (2026-09-29). The
# runtime digest carries libc6 2.41-12+deb13u4, libgcc-s1 14.2.0-19 and
# tzdata 2026c-0+deb13u1, read from the image's own var/lib/dpkg/status.d.
#
# GO_IMAGE is the one value that has to appear twice. The build stage's FROM line is the
# pin; this ARG is the copy the build stage's in-image toolchain check reads, because a
# RUN cannot see the reference its own stage was built from. scripts/check-pins.sh
# section 3 holds the two equal, so moving one without the other reds the gate instead of
# splitting silently.
ARG GO_IMAGE=golang:1.26.9-trixie@sha256:f89535b7caea67fa9ff0ba009894bff8f3be915e49cb635477c8ce04e045db5d

# --- ffmpeg: a pinned static build, verified by hash before it is trusted -----
# BtbN's builds link only glibc (>= 2.28), so they run on the distroless runtime while
# still being able to dlopen the vendor libraries a hardware encoder needs (NVENC) —
# which a fully-static binary could not do.
#
# THESE FOUR ARGs ARE THE PIN, and this is the only place it exists. CI and the release
# workflow do not restate it — scripts/install-ffmpeg.sh PARSES it from here and installs
# exactly this build, so the ffmpeg the fixture safety proof runs against cannot drift
# away from the ffmpeg the image ships. Do not copy these values anywhere; change them
# here and everything follows.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS ffmpeg
# Unpinned apt versions are fine HERE and only here: this stage is a throwaway fetcher
# that ships nothing into the final image, and the one artifact it does produce is
# pinned by release tag and verified by SHA-256 below. Pinning these three would just
# break the build on every Debian point release.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
# The tag MUST be a month-end build (the last calendar day of its month). Upstream's
# published retention policy is: the last build of each month is kept for TWO YEARS, the
# last 14 DAILY builds are kept, and "latest" floats. A mid-month daily is therefore a
# pin with a two-week fuse, which is exactly how the previous pin
# (autobuild-2026-07-13-14-11) turned into a 404 and took every job that installs ffmpeg
# down with it. scripts/check-pins.sh REFUSES a floating alias and refuses any tag that
# is not month-end, so this cannot regress by accident, and `make check-pin-live` asks
# upstream whether the pin is still served so the NEXT expiry reports itself.
ARG FFMPEG_BUILD=autobuild-2026-07-31-14-10
ARG FFMPEG_VERSION=N-125875-g5d4d3bdc61
ARG FFMPEG_SHA256_AMD64=16161335f2323ec74c5cec70427d3365ee9e0f581486eda35f6eba47375c45b4
ARG FFMPEG_SHA256_ARM64=a38f9976ff6377ed0a1117ed726c580da968cc8a0e9dc1328297cc60673e6f92
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) slug=linux64;    sha="${FFMPEG_SHA256_AMD64}" ;; \
      arm64) slug=linuxarm64; sha="${FFMPEG_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    tarball="ffmpeg-${FFMPEG_VERSION}-${slug}-gpl.tar.xz"; \
    curl -fsSL -o /tmp/ffmpeg.tar.xz \
      "https://github.com/BtbN/FFmpeg-Builds/releases/download/${FFMPEG_BUILD}/${tarball}"; \
    printf '%s  /tmp/ffmpeg.tar.xz\n' "${sha}" > /tmp/ffmpeg.sha256; \
    sha256sum -c /tmp/ffmpeg.sha256; \
    mkdir -p /ffmpeg; \
    tar -C /ffmpeg --strip-components=1 -xf /tmp/ffmpeg.tar.xz; \
    rm /tmp/ffmpeg.tar.xz; \
    test -x /ffmpeg/bin/ffmpeg; \
    test -x /ffmpeg/bin/ffprobe

# --- dynamic-HDR tools: dovi_tool and hdr10plus_tool, verified by hash ---------
# The Dolby Vision RPU and HDR10+ metadata toolchain (goal 8, T13, T25): dovi_tool reads,
# converts and counts an RPU, hdr10plus_tool extracts SMPTE 2094-40 metadata for libx265's
# dhdr10-info. Both are upstream's own static musl release builds, so they need nothing
# from the runtime base and are COPYed into it like ffmpeg. Both are MIT; NOTICE names
# each at the version pinned here, with its licence and source, and
# scripts/check-pins.sh section 11 holds the two equal, in both directions.
#
# THESE SIX ARGs ARE THE PIN, and this is the only place it exists. CI does not restate
# it: scripts/install-dynhdr-tools.sh PARSES it from here, so the tools the fixture suite
# runs against are the tools the image ships. Each sha256 is the GitHub release asset's
# own `digest`, confirmed by downloading the tarball and hashing it, and each tarball
# holds exactly one entry, the binary. Read 2026-10-01:
#   gh api repos/quietvoid/dovi_tool/releases/latest       (2.3.4, published 2026-09-10)
#   gh api repos/quietvoid/hdr10plus_tool/releases/latest  (1.7.2, published 2025-12-27)
#   https://github.com/quietvoid/dovi_tool/releases/tag/2.3.4
#   https://github.com/quietvoid/hdr10plus_tool/releases/tag/1.7.2
#   gh api repos/quietvoid/dovi_tool/license , .../hdr10plus_tool/license (MIT)
# Upstream publishes no retention policy for GitHub release assets and keeps every tag so
# far, so a plain release tag is the pin (no month-end rule as for ffmpeg);
# `make check-pin-live` asks whether all four assets are still served.
#
# This stage runs on $BUILDPLATFORM, so the binary for the OTHER architecture cannot be
# executed here. Each binary is proven to be an ELF executable for the TARGET machine
# (x86-64 or AArch64, read from its own header) on every build, and is also run and made
# to print its version whenever the build and target architectures agree.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS dynhdr
# The same throwaway-fetcher reasoning as the ffmpeg stage: nothing from this stage but
# the two verified binaries reaches the image.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
ARG BUILDARCH
ARG DOVI_TOOL_VERSION=2.3.4
ARG DOVI_TOOL_SHA256_AMD64=1844258e13c26607b32224bf1fa82b595d3b35949f5467405fda560daad32b3f
ARG DOVI_TOOL_SHA256_ARM64=b4f22a7db56954efe4ed8d02276d0f991799e84602f3584711be6c9df950cb15
ARG HDR10PLUS_TOOL_VERSION=1.7.2
ARG HDR10PLUS_TOOL_SHA256_AMD64=06385f37a639d61ba21d4be3150c863846933bc3b58110e094d8fc8f1c2249f2
ARG HDR10PLUS_TOOL_SHA256_ARM64=5fb90607cd94296640f1fc2355207b8107b67baac96d37481423d08a9fce437d
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) triple=x86_64-unknown-linux-musl;  machine="3e 00"; \
             dovi_sha="${DOVI_TOOL_SHA256_AMD64}"; h10p_sha="${HDR10PLUS_TOOL_SHA256_AMD64}" ;; \
      arm64) triple=aarch64-unknown-linux-musl; machine="b7 00"; \
             dovi_sha="${DOVI_TOOL_SHA256_ARM64}"; h10p_sha="${HDR10PLUS_TOOL_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /dynhdr; \
    fetch() { \
      tool="$1"; version="$2"; sha="$3"; url="$4"; \
      curl -fsSL --connect-timeout 30 --max-time 600 --retry 5 --retry-delay 10 --retry-all-errors \
        -o "/tmp/${tool}.tar.gz" "${url}"; \
      printf '%s  /tmp/%s.tar.gz\n' "${sha}" "${tool}" | sha256sum -c -; \
      entries="$(tar -tzf "/tmp/${tool}.tar.gz" | tr '\n' ' ')"; \
      case "${entries}" in \
        "./${tool} "|"${tool} ") ;; \
        *) echo "${url} holds '${entries}', not exactly ${tool}" >&2; exit 1 ;; \
      esac; \
      mkdir -p "/tmp/${tool}"; \
      tar -C "/tmp/${tool}" -xzf "/tmp/${tool}.tar.gz"; \
      test -f "/tmp/${tool}/${tool}"; \
      test -x "/tmp/${tool}/${tool}"; \
      test "$(od -An -tx1 -N4 "/tmp/${tool}/${tool}" | tr -s ' ' | sed 's/^ //;s/ $//')" = "7f 45 4c 46" \
        || { echo "${tool} is not an ELF binary" >&2; exit 1; }; \
      test "$(od -An -tx1 -j18 -N2 "/tmp/${tool}/${tool}" | tr -s ' ' | sed 's/^ //;s/ $//')" = "${machine}" \
        || { echo "${tool} is not built for ${TARGETARCH}" >&2; exit 1; }; \
      if [ "${TARGETARCH}" = "${BUILDARCH}" ]; then \
        got="$("/tmp/${tool}/${tool}" --version)"; \
        [ "${got}" = "${tool} ${version}" ] \
          || { echo "${tool} --version printed '${got}', pinned as ${version}" >&2; exit 1; }; \
      fi; \
      mv "/tmp/${tool}/${tool}" "/dynhdr/${tool}"; \
      rm -rf "/tmp/${tool}" "/tmp/${tool}.tar.gz"; \
    }; \
    fetch dovi_tool "${DOVI_TOOL_VERSION}" "${dovi_sha}" \
      "https://github.com/quietvoid/dovi_tool/releases/download/${DOVI_TOOL_VERSION}/dovi_tool-${DOVI_TOOL_VERSION}-${triple}.tar.gz"; \
    fetch hdr10plus_tool "${HDR10PLUS_TOOL_VERSION}" "${h10p_sha}" \
      "https://github.com/quietvoid/hdr10plus_tool/releases/download/${HDR10PLUS_TOOL_VERSION}/hdr10plus_tool-${HDR10PLUS_TOOL_VERSION}-${triple}.tar.gz"; \
    ls -l /dynhdr

# --- hardware runtime: pinned Debian trixie packages, verified by hash --------
# The userspace VAAPI and QSV need inside the container, per the approved P3 option (a)
# (https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/2026-09-holdfast-research/proposal-amd-image.md): libva, libva-drm and
# libdrm (the pinned ffmpeg dlopens them through implib-gen shims and ABORTS, exit 134,
# when one is missing), Intel's full-feature iHD VA driver and libmfx-gen (the QSV
# runtime for Tiger Lake and newer), and Mesa's radeonsi VA driver for AMD, each with the
# shared-library closure the dynamic loader really needs. The closure was computed with
# readelf -d on the unpacked packages, minus what the runtime base already carries
# (libc6, libgcc-s1, libstdc++6, libzstd1, zlib1g, libssl3t64, libgomp1, read from the
# base's own var/lib/dpkg/status.d on 2026-09-30), and the image smoke proves it: each
# driver is listed by the dynamic loader inside the image with nothing "not found".
#
# Every package is pinned by exact version AND sha256, and fetched from
# snapshot.debian.org, never from deb.debian.org: a pool file on the live mirror is
# removed when a point release supersedes it, while a snapshot URL serves the same bytes
# for good. Each sha256 below was read from the snapshot's own Packages index
# (dists/trixie/{main,non-free}/binary-amd64/Packages.xz at DEBIAN_SNAPSHOT, whose own
# sha256 the snapshot's Release file lists) and confirmed by downloading the .deb from
# that snapshot, on 2026-09-30:
#   https://snapshot.debian.org/archive/debian/20260929T202609Z/dists/trixie/Release
# NOTICE names every package with its version, licence and corresponding source, and
# scripts/check-pins.sh holds the two lists equal, both directions, and refuses a pin
# without a version, a 64-hex sha256 or a snapshot URL.
#
# Only shared objects and each package's copyright file are staged: from every package
# its usr/lib/x86_64-linux-gnu tree, from mesa-va-drivers only radeonsi_drv_video.so (the
# one gallium VA driver P3 chose; it is a symlink into libgallium), and from
# libdrm-common only the amdgpu.ids table libdrm_amdgpu reads to name an AMD device.
# Libraries land on the multiarch path /usr/lib/x86_64-linux-gnu, the VA drivers in its
# dri/ directory, which is the driver directory Debian's libva is built with (and the one
# BtbN's recipe names, scripts.d/50-vaapi/50-libva.sh:43 at the ffmpeg pin tag).
# Everything stays under usr/: the runtime base is UsrMerge, so /lib is a symlink into
# /usr/lib, and a staged top-level lib/ directory would clobber it.
#
# arm64 gets an EMPTY runtime, on purpose: the pinned arm64 ffmpeg is built without VAAPI
# (BtbN scripts.d/50-vaapi/50-libva.sh:16 returns early for linuxarm64) and without libvpl
# (scripts.d/50-onevpl.sh), and Debian builds libmfx-gen1.2 for amd64 only, so there is
# nothing on arm64 these libraries could serve. Read 2026-09-29 at
# https://github.com/BtbN/FFmpeg-Builds/tree/autobuild-2026-07-31-14-10/scripts.d
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS hwruntime
# The same throwaway-fetcher reasoning as the ffmpeg stage: curl only downloads, and every
# byte it fetches is checked against a pinned sha256 before it is unpacked.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
ARG DEBIAN_SNAPSHOT=20260929T202609Z
# One package per line: name, exact version, sha256 of the .deb, path under the
# snapshot's pool/. scripts/check-pins.sh section 10 parses this block.
COPY <<'DEBPINS' /hwruntime.pins
libva2                          2.22.0-3                                  b76bdd330de47a826698aaed10f53435b703e9a7d4415dd68269c97709f46a9b main/libv/libva/libva2_2.22.0-3_amd64.deb
libva-drm2                      2.22.0-3                                  5dce5007ddc0ce87a61db4d71476ce1a5a135737b5185ce2e8b7067342fcafc6 main/libv/libva/libva-drm2_2.22.0-3_amd64.deb
libdrm2                         2.4.124-2                                 fe2276901c7cd7b8079de63072d37fe1cbeb4eb001a3bc1f1d662ad89aa0890e main/libd/libdrm/libdrm2_2.4.124-2_amd64.deb
libdrm-common                   2.4.124-2                                 9a8a6c65c165e9964f106fb4ac710959b5d33e0790227e3ab6b27c4742d1254a main/libd/libdrm/libdrm-common_2.4.124-2_all.deb
libdrm-amdgpu1                  2.4.124-2                                 c1d97a5e32e2bc68e833b8abeae026b6306198e87ee4396a7ffecd4779caefbf main/libd/libdrm/libdrm-amdgpu1_2.4.124-2_amd64.deb
libdrm-intel1                   2.4.124-2                                 188ab1fd74c838b3c8055d62107351e16a4bb7a25d39c30bbb9aac30ddd37238 main/libd/libdrm/libdrm-intel1_2.4.124-2_amd64.deb
libpciaccess0                   0.17-3+b3                                 d9a0091071635a84e837051e4813005ac445071731becea28fba4d9806df5252 main/libp/libpciaccess/libpciaccess0_0.17-3+b3_amd64.deb
intel-media-va-driver-non-free  25.2.3+ds1-1                              e0aec5839e3a6c41b7b1815ed944b85c1334d38ee744165c9788f0e0fc8803f9 non-free/i/intel-media-driver-non-free/intel-media-va-driver-non-free_25.2.3+ds1-1_amd64.deb
libigdgmm12                     22.7.2+ds1-1                              81b668213cb59d2bcae8c66b1184277fd0228e3648ab63f4f2b3b578da291b06 main/i/intel-gmmlib/libigdgmm12_22.7.2+ds1-1_amd64.deb
libmfx-gen1.2                   25.1.4-1                                  9dd7ff697976075941a014ee9594c494040a7b4b56b9821b35475bb80e2aaddd main/o/onevpl-intel-gpu/libmfx-gen1.2_25.1.4-1_amd64.deb
mesa-va-drivers                 25.0.7-2+deb13u1                          708e6f7f87863605eef532f70b1f80615c1b6c00f4e2b52a2850ae90de917fbd main/m/mesa/mesa-va-drivers_25.0.7-2+deb13u1_amd64.deb
mesa-libgallium                 25.0.7-2+deb13u1                          3e610f29321cdcc61337c86e6b4031ff60f0c9915fe19e6a862ac06480620ff4 main/m/mesa/mesa-libgallium_25.0.7-2+deb13u1_amd64.deb
libllvm19                       1:19.1.7-3+b1                             db0d614d61345ca710fb73e75105f1ec6e38898fa7b3aa99fa7eb7d8b0a11d57 main/l/llvm-toolchain-19/libllvm19_19.1.7-3+b1_amd64.deb
libz3-4                         4.13.3-1                                  71383373523ef62d47eccf660cf6535c3febcbb3f88e54cb7a43124014b57359 main/z/z3/libz3-4_4.13.3-1_amd64.deb
libedit2                        3.1-20250104-1                            b002ea172b9c1e34a67bc497c523c67bb74c3f0a4e98113cb083990a1f1d3bfe main/libe/libedit/libedit2_3.1-20250104-1_amd64.deb
libbsd0                         0.12.2-2                                  e5a85986fa6bec3307ab1bc860736b478b331882bc45e17675a7bdf88eecb43a main/libb/libbsd/libbsd0_0.12.2-2_amd64.deb
libmd0                          1.1.0-2+b1                                7244ec3839b61fac0c1884fe08aaa040f26e8f1f35f1f5d3482eacefd30d1b44 main/libm/libmd/libmd0_1.1.0-2+b1_amd64.deb
libtinfo6                       6.5+20250216-2                            8b9f6a7983e9418564e48a627518de4c03917b56efe68d7f3e93bd8fffa1cc10 main/n/ncurses/libtinfo6_6.5+20250216-2_amd64.deb
libffi8                         3.4.8-2                                   0ebdc340de33333639c3c63874cd4b15ac2e83dfa1ef3053b7eefaf4919f4f68 main/libf/libffi/libffi8_3.4.8-2_amd64.deb
libxml2                         2.12.7+dfsg+really2.9.14-2.1+deb13u3      e0c6b63ce4602a036a526f60fe5e6c1586710688058d98fc1001b9b3147b7efd main/libx/libxml2/libxml2_2.12.7+dfsg+really2.9.14-2.1+deb13u3_amd64.deb
liblzma5                        5.8.1-1+deb13u1                           1cfcc6e0dc36f438a79b6e2189facdb9d150b08f57d190a60e01c98075c7f896 main/x/xz-utils/liblzma5_5.8.1-1+deb13u1_amd64.deb
libelf1t64                      0.192-4                                   94497b7e17b6f574a0605b380d454e20d3f01a9c63b70c2a2263f679d30053e1 main/e/elfutils/libelf1t64_0.192-4_amd64.deb
libsensors5                     1:3.6.2-2                                 f0a994a6d7cfa695dea5343d0d1ba7eed796c0ad920c7282998b95f60049c4f6 main/l/lm-sensors/libsensors5_3.6.2-2_amd64.deb
libexpat1                       2.8.3-1~deb13u1                           38abe0e710a07688e9c149d74536e67cfee0364bdb64dd6d644c32a1cfad389f main/e/expat/libexpat1_2.8.3-1~deb13u1_amd64.deb
libx11-xcb1                     2:1.8.12-1                                e05f94d21a932fba5b09b9b13d99df776b155d3bc792c0e294451df9ffe1ba25 main/libx/libx11/libx11-xcb1_1.8.12-1_amd64.deb
libxcb1                         1.17.0-2+b1                               5c222a72d11b866447da31693254f738430726e3e065a384e82687b2fd2f978b main/libx/libxcb/libxcb1_1.17.0-2+b1_amd64.deb
libxau6                         1:1.0.11-1                                689a9f0e0ba3e2c65431f864871e303ee904de69dd28abfc462663fae030227f main/libx/libxau/libxau6_1.0.11-1_amd64.deb
libxdmcp6                       1:1.1.5-1                                 0740dc760916b2008b45417a42a8fd7dd5de370fb57d31373f15034cda8acf0b main/libx/libxdmcp/libxdmcp6_1.1.5-1_amd64.deb
libxcb-dri3-0                   1.17.0-2+b1                               f446d42fb5fcebbb3e347368ba83616769fdb271d85b2f49048e337f3163d267 main/libx/libxcb/libxcb-dri3-0_1.17.0-2+b1_amd64.deb
libxcb-present0                 1.17.0-2+b1                               db95ea4630c55bd7f3281cb60ccf75b1627ef2f4399e1939f8f6161e584a92fb main/libx/libxcb/libxcb-present0_1.17.0-2+b1_amd64.deb
libxcb-randr0                   1.17.0-2+b1                               f5d9fe5fdf797918f81f0abf6cfd4270bd54659540cddb4da709ab7154523214 main/libx/libxcb/libxcb-randr0_1.17.0-2+b1_amd64.deb
libxcb-sync1                    1.17.0-2+b1                               0ce4770ed1505be9ddc6473045e4662d062aff2f3077ee684265f01cfb543559 main/libx/libxcb/libxcb-sync1_1.17.0-2+b1_amd64.deb
libxcb-xfixes0                  1.17.0-2+b1                               f9c1aafc18bf9e4662e34bbaf3bb00f1f4e8c2fc1dd786fdfb6cb9cb6c64fae7 main/libx/libxcb/libxcb-xfixes0_1.17.0-2+b1_amd64.deb
libxshmfence1                   1.3.3-1                                   7b339e9e5b2349723d35af4df89bcc7aa456bbdf8ba1754358f9b44c3fe1f964 main/libx/libxshmfence/libxshmfence1_1.3.3-1_amd64.deb
DEBPINS
RUN set -eu; \
    mkdir -p /hwroot; \
    if [ "${TARGETARCH}" != "amd64" ]; then \
      echo "no hardware runtime for ${TARGETARCH}: its pinned ffmpeg has no VAAPI and no libvpl"; \
      exit 0; \
    fi; \
    lib=usr/lib/x86_64-linux-gnu; \
    mkdir -p "/hwroot/${lib}" /hwroot/usr/share/doc /tmp/deb /tmp/x; \
    while read -r name version sha path; do \
      [ -n "${name}" ] || continue; \
      deb="/tmp/deb/${name}.deb"; \
      curl -fsSL --connect-timeout 30 --max-time 900 --retry 8 --retry-delay 10 --retry-all-errors \
        -o "${deb}" "https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/pool/${path}"; \
      printf '%s  %s\n' "${sha}" "${deb}" | sha256sum -c -; \
      got="$(dpkg-deb -f "${deb}" Package) $(dpkg-deb -f "${deb}" Version)"; \
      if [ "${got}" != "${name} ${version}" ]; then \
        echo "${path} is '${got}', pinned as '${name} ${version}'" >&2; exit 1; \
      fi; \
      dpkg-deb -x "${deb}" "/tmp/x/${name}"; \
      if [ -d "/tmp/x/${name}/${lib}" ]; then cp -a "/tmp/x/${name}/${lib}/." "/hwroot/${lib}/"; fi; \
      mkdir -p "/hwroot/usr/share/doc/${name}"; \
      cp "/tmp/x/${name}/usr/share/doc/${name}/copyright" "/hwroot/usr/share/doc/${name}/copyright"; \
    done < /hwruntime.pins; \
    for d in "/hwroot/${lib}/dri/"*_drv_video.so; do \
      case "${d##*/}" in iHD_drv_video.so|radeonsi_drv_video.so) ;; *) rm -f "${d}" ;; esac; \
    done; \
    mkdir -p /hwroot/usr/share/libdrm; \
    cp /tmp/x/libdrm-common/usr/share/libdrm/amdgpu.ids /hwroot/usr/share/libdrm/amdgpu.ids; \
    for f in dri/iHD_drv_video.so dri/radeonsi_drv_video.so libmfx-gen.so.1.2 \
             libva.so.2 libva-drm.so.2 libdrm.so.2; do \
      test -e "/hwroot/${lib}/${f}" || { echo "hardware runtime is missing ${lib}/${f}" >&2; exit 1; }; \
    done; \
    test "$(ls -A /hwroot)" = "usr" || { echo "hardware runtime staged outside usr/" >&2; exit 1; }; \
    rm -rf /tmp/deb /tmp/x; \
    du -sh /hwroot

# --- build the web UI ----------------------------------------------------------
# The UI (web/) is built here, by the same commands `make check` proves it with, on the
# Node that web/.node-version pins: scripts/check-pins.sh section 12 holds this tag to
# that file, and section 7 holds the reference to a tag and a digest written on this line.
# The digest is the multi-arch index node:24.21.0-trixie-slim resolved to on
# registry-1.docker.io, read 2026-10-03 (and unchanged from the reading of 2026-09-29).
# Node 24 is the Active LTS line until 2026-10-20 and maintained until 2028-04-30
# (https://raw.githubusercontent.com/nodejs/Release/main/schedule.json, read 2026-09-29).
#
# $BUILDPLATFORM, like every stage that RUNs: the build's output is JavaScript, CSS and
# HTML, the same bytes for every target architecture, so the arm64 image needs no QEMU
# here either. Nothing of this stage reaches the runtime image but those files, embedded
# in the Go binary: no Node, no pnpm and no node_modules ship.
FROM --platform=$BUILDPLATFORM node:26.9.0-trixie-slim@sha256:3a771f83944bb763050c23c0225c260638c4b7899e7a72485ef75e5e570499e5 AS ui

# The same question the build stage asks of its Go image, for the same reason: the digest
# is what Docker pulls and the tag beside it is a label nothing enforces, so ask the image
# which Node it is and hold that to the pin the gate ran on.
WORKDIR /src/web
COPY web/.node-version ./
RUN set -eu; \
    want="v$(tr -d '[:space:]' < .node-version)"; \
    got="$(node --version)"; \
    if [ "$got" != "$want" ]; then \
      echo "web/.node-version pins ${want} and this stage's pinned digest ships ${got}" >&2; \
      exit 1; \
    fi; \
    echo "node in the image matches web/.node-version (${got})"

# pnpm comes from corepack, which reads the `packageManager` field of package.json and
# refuses a package that does not hash to the sha512 written there. pnpm 12's package is a
# launcher that fetches the native pnpm of the same version and checks it against npm's
# registry signatures. Lifecycle scripts do
# not run: web/pnpm-workspace.yaml says so, and it is copied before the install reads it.
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 CI=true
RUN corepack enable pnpm
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN set -eu; \
    pnpm run build; \
    test -f dist/index.html; \
    grep -q '<footer id="source-offer"></footer>' dist/index.html \
      || { echo "the built page has no slot for the Corresponding Source offer" >&2; exit 1; }; \
    ls -lR dist

# --- build the binary --------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26.9-trixie@sha256:f89535b7caea67fa9ff0ba009894bff8f3be915e49cb635477c8ce04e045db5d AS build

# The DIGEST is what Docker pulls; the tag beside it is a label the registry does not
# enforce. scripts/check-pins.sh holds the Go version together across this file, ci.yml
# and release.yml (and holds GO_IMAGE equal to the FROM line above), but it can only
# compare the TAG it parses out of GO_IMAGE, because
# nothing outside the image can see which toolchain a digest actually contains. So bump
# the tag, leave the stale digest, and every file agrees, check-pins prints "ok", and the
# shipped binary is built by the superseded Go. That is the same silent detachment the
# ffmpeg pin is guarded against, in the one place the script cannot reach, so ask the
# image what it is, from inside it, where the answer is authoritative.
ARG GO_IMAGE
RUN set -eu; \
    want="${GO_IMAGE#*:}"; want="${want%%@*}"; want="${want%%-*}"; \
    got="$(go env GOVERSION)"; \
    if [ "$got" != "go${want}" ]; then \
      echo "GO_IMAGE tag names go${want} but its pinned digest ships ${got}: the digest was left at a superseded toolchain" >&2; \
      exit 1; \
    fi; \
    echo "go toolchain in the image matches the GO_IMAGE tag (${got})"

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The web UI, into the directory the binary embeds (internal/ui). The context carries only
# that directory's committed placeholder (.dockerignore), so what is embedded is this
# build's own UI and nothing a developer's tree happened to hold. A binary whose embed
# directory has no page serves the plain-text root page and says so at startup; the image
# is not allowed to be that binary, so the copy is checked.
COPY --from=ui /src/web/dist/ ./internal/ui/dist/
RUN set -eu; \
    test -f internal/ui/dist/index.html; \
    test -f internal/ui/dist/.gitkeep; \
    test -n "$(ls internal/ui/dist/assets)"
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
# The Corresponding Source URL the served page offers (AGPL-3.0 section 13, LICENSE-3).
# The image is the only way this project is distributed, so the offer has to be settable
# HERE and not only through the Makefile. It rides the same -ldflags invocation as the
# version stamp, because the offer is the link PLUS the build identity.
#
#   docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast .
#
# Leave it unset and the image offers the upstream tree. This default is checked against
# internal/sourceoffer.Upstream by a test inside `make check` (no docker required), so
# this copy cannot drift from the built-in one.
#
# The -X assignment below is SINGLE-QUOTED, which the others do not need to be. go's
# -ldflags value is split with shell-like quoting, so an unquoted URL carrying a space
# would be split into two flags and fail the build. The value is only ever escaped at
# render time, never rejected for being unclean, so the build path has to carry the same
# values the served page does.
ARG SOURCE_URL=https://github.com/NSchatz/holdfast
# -tags holdfast_image marks this binary as the image's (internal/version.Packaging): the
# image does not carry AMD's AMF runtime, whose EULA grants no redistribution, so this
# binary refuses `encoder: amf` at start with that reason instead of probing it
# (docs/design/hardware.md#amf). A host build carries no tag and keeps amf.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -tags holdfast_image \
    -ldflags="-s -w \
      -X github.com/NSchatz/holdfast/internal/version.Version=${VERSION} \
      -X github.com/NSchatz/holdfast/internal/version.Commit=${COMMIT} \
      -X github.com/NSchatz/holdfast/internal/version.Date=${DATE} \
      -X 'github.com/NSchatz/holdfast/internal/sourceoffer.URL=${SOURCE_URL}'" \
    -o /out/holdfast ./cmd/holdfast
# The owner's live check of their own Plex, Sonarr or Radarr (scripts/client-report.sh
# --image, docs/client-reports.md): the same clients as the binary above, behind a command
# that only reports. It rides the image so the owner needs no Go toolchain to run it, and
# nothing in the image ever invokes it.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w \
      -X github.com/NSchatz/holdfast/internal/version.Version=${VERSION} \
      -X github.com/NSchatz/holdfast/internal/version.Commit=${COMMIT}" \
    -o /out/holdfast-client-report ./scripts/clientreport

# --- runtime -----------------------------------------------------------------
# distroless cc: glibc + libgcc_s + ca-certificates, no shell, no package manager,
# non-root by default. Nothing RUNs in this stage, so it cross-builds without emulation.
#
# NOTE what the image carries for hardware encoders, and what it does not. On amd64 the
# hwruntime stage above supplies the VAAPI and QSV userspace from pinned Debian trixie
# packages: libva, libva-drm and libdrm, Intel's iHD VA driver (VAAPI on Intel), libmfx-gen
# (QSV on Tiger Lake and newer; older Intel parts still encode through VAAPI) and Mesa's
# radeonsi VA driver (VAAPI on AMD). Passing /dev/dri supplies only the kernel device; the
# drivers are what this image adds. arm64 carries none (see that stage).
#
# `amf` does NOT work in this image and cannot be made to: AMD's AMF runtime
# (libamfrt64, in amf-amdgpu-pro) is licensed under the AMDGPU PRO EULA, which grants
# "install and use" only and no redistribution, so a published image of an AGPL project
# cannot carry it (P3; https://repo.radeon.com/amf/copyright, read 2026-09-29). AMD
# hardware encodes here through `encoder: vaapi`, which is AMD's own advice.
#
# NVIDIA is supplied by the host: the NVIDIA Container Toolkit injects the driver's
# libraries (libnvidia-encode among them) into the container, and nvenc dlopens them.
# See NVIDIA_DRIVER_CAPABILITIES below and docs/docker.md "GPU passthrough".
#
# distroless CC, not BASE. ffmpeg/ffprobe carry a DT_NEEDED on libgcc_s.so.1, and the
# `base` variant ships glibc WITHOUT libgcc - so `base` builds perfectly and then dies
# at the dynamic loader the first time the engine execs ffmpeg ("libgcc_s.so.1: cannot
# open shared object file"). `cc` is `base` + libgcc_s + libstdc++, still no shell, still
# nonroot. Verified against the registry: base ships libc/libm/libmvec and no libgcc.
FROM gcr.io/distroless/cc-debian13:nonroot@sha256:54df941ed0d06a1bd95ef5e0ce391fd8d9f94b64782dc9a60062727849ee3f97

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
LABEL org.opencontainers.image.title="holdfast" \
      org.opencontainers.image.description="Config-as-code, data-safe, self-hosted media transcoder — never destroys a source until a replacement is provably faithful." \
      org.opencontainers.image.source="https://github.com/NSchatz/holdfast" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}"

# The hardware runtime: shared objects under /usr/lib/x86_64-linux-gnu (VA drivers in its
# dri/), each package's copyright file under /usr/share/doc/<package>/, and the amdgpu.ids
# table. Empty on arm64. No RUN here: the stage above staged exactly what is copied.
COPY --from=hwruntime /hwroot/ /
COPY --from=ffmpeg /ffmpeg/bin/ffmpeg  /usr/local/bin/ffmpeg
COPY --from=ffmpeg /ffmpeg/bin/ffprobe /usr/local/bin/ffprobe
# The dynamic-HDR tools: static musl binaries, so they need nothing from this base.
COPY --from=dynhdr /dynhdr/dovi_tool      /usr/local/bin/dovi_tool
COPY --from=dynhdr /dynhdr/hdr10plus_tool /usr/local/bin/hdr10plus_tool
# `run_window` is evaluated in LOCAL time, so the zone database has to be present for a
# TZ= setting to mean anything at all. The distroless base does ship one today — this
# COPY pins that fact down rather than depending on it, because if a base change ever
# dropped it, the failure is silent: no error, no wrong result, just an overnight window
# running on UTC. Cheap insurance against a failure mode that does not announce itself.
# The copy REPLACES the base's own zone files, so the image carries the build stage's
# tzdata release, which can trail the base's: at these pins the build stage has 2026b
# and the base 2026c. Refreshing the build stage's digest (and GO_IMAGE with it) is what
# refreshes it.
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/holdfast /usr/local/bin/holdfast
COPY --from=build /out/holdfast-client-report /usr/local/bin/holdfast-client-report
# The image redistributes prebuilt GPL ffmpeg binaries, so it ships their licence and
# source offer with them (NOTICE), alongside holdfast's own AGPL text.
COPY --from=build /src/LICENSE /src/NOTICE /usr/share/doc/holdfast/

# Deliberately NO ENV defaults for CONFIG keys. An env var BEATS the YAML file
# (HOLDFAST_<KEY>), so baking one here would silently override the user's
# config-as-code — and for server_addr it would quietly widen a deliberate 127.0.0.1
# fail-safe. docker-compose.yml sets the container paths explicitly instead, in the
# open, where they are reviewable.
#
# HOME is the one exception, and it is not a config key. config.Validate() REFUSES to
# run when it cannot determine the home directory — a delete-capable tool will not skip
# the "is a library root actually $HOME" check just because the check is unavailable.
# You are expected to override `user:` with the uid that owns your media (see
# docker-compose.yml), and an arbitrary uid is not in /etc/passwd, so HOME would
# otherwise be whatever the container runtime decides to default it to. Pin it, and the
# safety check has a definite answer under every uid.
ENV HOME=/home/nonroot
# NOT a config key either: it is read by the NVIDIA Container Toolkit, never by holdfast.
# On the toolkit's legacy runtime-hook path the driver libraries mounted into a container
# are chosen by capability, and "empty or unset" means the default `utility,compute`,
# which excludes `video`, the capability "required for using the Video Codec SDK"
# (libnvidia-encode, what nvenc dlopens). The list REPLACES the default rather than adding
# to it, hence all three. Docker sets this variable itself only when a request names an
# NVIDIA capability, and compose's `capabilities: [gpu]` names none, so on that path this
# value is the one the hook reads. On a CDI host (Docker 29.2 and later with NVIDIA
# Container Toolkit 1.18 and later) the generated spec mounts the driver's libraries with
# no capability filtering, so it changes nothing there. NVIDIA_VISIBLE_DEVICES is
# deliberately NOT set: Docker writes it from the device request (`--gpus`, or compose's
# `deploy.resources.reservations.devices`), and an image default of `all` would hand every
# GPU to any container started under the nvidia runtime without asking for one.
# Read 2026-09-30:
#   https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html
#   https://github.com/moby/moby/blob/master/daemon/devices_nvidia_linux.go (injectNVIDIARuntimeHook)
#   https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/2026-09-holdfast-research/verify-hw-encode.md claim 9 (the CDI path)
ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/holdfast"]
CMD ["version"]
