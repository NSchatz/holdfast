# syntax=docker/dockerfile:1
#
# Genesis stub (TRANSCODE-0). Builds the static transcode binary and bundles a
# known-good ffmpeg/ffprobe that carries libvmaf — because a distro ffmpeg can
# conceal HEVC corruption on decode-to-null, so VMAF is the real quality gate
# (see the umbrella roadmap operations/roadmaps/transcode.md §6). The hardened,
# smoke-tested, multi-arch, non-root production image lands in TRANSCODE-9; this
# stub exists so the packaging path is real from day one.

# --- build the binary -------------------------------------------------------
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/NSchatz/transcode/internal/version.Version=${VERSION} \
      -X github.com/NSchatz/transcode/internal/version.Commit=${COMMIT} \
      -X github.com/NSchatz/transcode/internal/version.Date=${DATE}" \
    -o /out/transcode ./cmd/transcode

# --- ffmpeg source (ships ffmpeg + ffprobe built with libvmaf) --------------
# Pinned digest to be added in TRANSCODE-9; tag-pinned here for the stub.
FROM lscr.io/linuxserver/ffmpeg:7.1.1 AS ffmpeg

# --- runtime ----------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# Bring ffmpeg/ffprobe (with libvmaf) and their shared libs from the ffmpeg image.
COPY --from=ffmpeg /usr/local/bin/ffmpeg  /usr/local/bin/ffmpeg
COPY --from=ffmpeg /usr/local/bin/ffprobe /usr/local/bin/ffprobe
COPY --from=ffmpeg /usr/local/lib/        /usr/local/lib/
RUN ldconfig
COPY --from=build /out/transcode /usr/local/bin/transcode

# Non-root by default (overridable via PUID/PGID wiring in TRANSCODE-9).
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/transcode"]
CMD ["version"]
