# syntax=docker/dockerfile:1
#
# Production image (TRANSCODE-9). Multi-arch (linux/amd64 + linux/arm64), non-root,
# no shell, bundling a PINNED, CHECKSUM-VERIFIED ffmpeg that carries libx265 +
# libsvtav1 + libvmaf.
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
ARG GO_IMAGE=golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73

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

# --- build the binary --------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73 AS build

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

# --- runtime -----------------------------------------------------------------
# distroless cc: glibc + libgcc_s + ca-certificates, no shell, no package manager,
# non-root by default. Nothing RUNs in this stage, so it cross-builds without emulation.
#
# NOTE what this base deliberately does NOT carry: any vendor userspace GPU library.
# ffmpeg dlopens those at runtime — a VA driver (iHD/i965) for qsv/vaapi, and AMD's own
# libamfrt64 for amf, which does NOT go through VA-API — and passing /dev/dri supplies
# only the KERNEL device, not a driver. So `encoder: qsv|vaapi|amf` cannot start here.
# NVIDIA is different and does work: the NVIDIA Container Toolkit INJECTS
# libnvidia-encode into the container, which is exactly what nvenc dlopens. See
# docs/docker.md "GPU passthrough" — a documented limitation, not an oversight.
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

COPY --from=ffmpeg /ffmpeg/bin/ffmpeg  /usr/local/bin/ffmpeg
COPY --from=ffmpeg /ffmpeg/bin/ffprobe /usr/local/bin/ffprobe
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
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/holdfast"]
CMD ["version"]
