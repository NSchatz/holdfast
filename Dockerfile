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

ARG GO_IMAGE=golang:1.25.12-bookworm@sha256:a9c020ee3d1508c7be5435c262434e3d3fc1d0e76a11afeb9ddae7d60bc86aa4
ARG FETCH_IMAGE=debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df
# distroless CC, not BASE. ffmpeg/ffprobe carry a DT_NEEDED on libgcc_s.so.1, and the
# `base` variant ships glibc WITHOUT libgcc — so `base` builds perfectly and then dies
# at the dynamic loader the first time the engine execs ffmpeg ("libgcc_s.so.1: cannot
# open shared object file"). `cc` is `base` + libgcc_s + libstdc++, still no shell, still
# nonroot. Verified against the registry: base ships libc/libm/libmvec and no libgcc.
ARG RUNTIME_IMAGE=gcr.io/distroless/cc-debian12:nonroot@sha256:ce0d66bc0f64aae46e6a03add867b07f42cc7b8799c949c2e898057b7f75a151

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
FROM --platform=$BUILDPLATFORM ${FETCH_IMAGE} AS ffmpeg
# Unpinned apt versions are fine HERE and only here: this stage is a throwaway fetcher
# that ships nothing into the final image, and the one artifact it does produce is
# pinned by release tag and verified by SHA-256 below. Pinning these three would just
# break the build on every Debian point release.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
ARG FFMPEG_BUILD=autobuild-2026-07-13-14-11
ARG FFMPEG_VERSION=N-125573-g90436de5e1
ARG FFMPEG_SHA256_AMD64=d7c3084807d14c868eface59617107ec29a8ae729413d7c133d1bcf6ffe39f01
ARG FFMPEG_SHA256_ARM64=cca15acd6d7f2ecbd3f0508e7f25777ec2fdc4895cffebf8d919540c76e648b2
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
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w \
      -X github.com/NSchatz/transcode/internal/version.Version=${VERSION} \
      -X github.com/NSchatz/transcode/internal/version.Commit=${COMMIT} \
      -X github.com/NSchatz/transcode/internal/version.Date=${DATE}" \
    -o /out/transcode ./cmd/transcode

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
FROM ${RUNTIME_IMAGE}

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
LABEL org.opencontainers.image.title="transcode" \
      org.opencontainers.image.description="Config-as-code, data-safe, self-hosted media transcoder — never destroys a source until a replacement is provably faithful." \
      org.opencontainers.image.source="https://github.com/NSchatz/transcode" \
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
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/transcode /usr/local/bin/transcode
# The image redistributes prebuilt GPL ffmpeg binaries, so it ships their licence and
# source offer with them (NOTICE), alongside transcode's own AGPL text.
COPY --from=build /src/LICENSE /src/NOTICE /usr/share/doc/transcode/

# Deliberately NO ENV defaults for CONFIG keys. An env var BEATS the YAML file
# (TRANSCODE_<KEY>), so baking one here would silently override the user's
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
ENTRYPOINT ["/usr/local/bin/transcode"]
CMD ["version"]
