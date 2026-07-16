# Running holdfast in Docker

The container image is the supported way to run `holdfast`. It bundles a **pinned,
checksum-verified ffmpeg** carrying **libx265, libsvtav1 and libvmaf** — which matters more
than convenience: VMAF is the gate that rejects an encode which decodes cleanly but *looks*
worse, and a distro ffmpeg without `libvmaf` cannot measure it. The engine refuses to accept
an output it could not measure, so the wrong ffmpeg does not quietly weaken the no-loss
contract — it stops the tool. The image removes that whole class of problem.

It is the **same ffmpeg build CI runs the fixture safety proof against**, so the image ships
the ffmpeg that was actually proven, not one that resembles it.

## Quick start

```bash
mkdir -p state && sudo chown 1000:1000 state    # must be writable by the `user:` you run as
cp config.example.yaml config.yaml              # edit it — see below
docker compose config -q                        # validate before you run
docker compose up -d
```

A container config differs from a bare-metal one in exactly three places:

```yaml
library_roots:
  - /media                  # the CONTAINER path you mounted your library at
state_dir: /state           # the mounted volume — it must survive a restart
server_addr: 0.0.0.0:8080   # see "The control surface" below before you change this
```

## The image

| | |
|---|---|
| Base | `gcr.io/distroless/cc-debian12:nonroot` — glibc **+ libgcc_s/libstdc++** + CA certs, **no shell, no package manager**. It must be `cc`, not `base`: ffmpeg has a `DT_NEEDED` on `libgcc_s.so.1`, which `base` does not ship, so `base` builds fine and then cannot exec ffmpeg at all. |
| Platforms | `linux/amd64`, `linux/arm64` |
| User | non-root by default (`nonroot`, uid 65532); override with `user:` |
| ffmpeg | pinned by release tag **and verified by SHA-256** before it is trusted |
| Config | **nothing is baked in** — see below |
| Licences | `/usr/share/doc/holdfast/` (AGPL-3.0 + the bundled-ffmpeg NOTICE) |

**The image sets no `HOLDFAST_*` environment variables, on purpose.** An env var *beats* the
YAML file, so a baked-in default would silently override your config-as-code — and for
`server_addr` it would quietly widen a deliberate `127.0.0.1` fail-safe. The compose file sets
container paths in the open, where you can review them.

There is **no `HEALTHCHECK`**, also on purpose. The image has no shell to run one, and the
honest signal for a transcoder is not "is the HTTP port up" — a wedged encode answers that
question green. Watch `/metrics` (Prometheus) or point an external check at `/api/summary`.

## Volumes and permissions

| Mount | Why |
|---|---|
| `/media` (your library) | **The only place holdfast ever mutates anything.** Mount exactly the tree you want re-encoded — nothing more. |
| `/state` | The resumable SQLite job store. **Must survive restarts**, or a crashed run cannot resume and the whole library is rescanned. |
| `/config/config.yaml` | Read-only. holdfast never writes it. |

`user:` **must be the uid:gid that owns the media**. holdfast encodes to a temp file *next to
the source* and replaces it with an atomic same-directory rename, so it needs write access to
the library **directories**, not just the files. Getting this wrong is safe but useless: every
encode fails at the write step, and every source is left byte-for-byte intact.

The library mount must also be a **single filesystem per directory** — the swap is a
`rename(2)`, which cannot cross filesystems. (This is why the temp file lives beside the
source rather than in a scratch volume.)

## Timezone

`run_window` is evaluated in **local time**. The image carries the zone database, but a
container with no `TZ` is **UTC** — set `TZ` or your "encode overnight" window will run at the
wrong hours, silently and correctly, on the wrong clock.

## The control surface

`holdfast serve` exposes the API + dashboard. Its default bind is `127.0.0.1`, which inside a
container namespace means *nothing outside the container can reach it* — so a containerised
`serve` needs `server_addr: 0.0.0.0:8080`, and the real boundary moves to the **published
port**:

```yaml
ports:
  - "127.0.0.1:8080:8080"   # loopback ONLY
```

That is the shipped default, and it is the one to keep unless you have set
`HOLDFAST_SERVER_AUTH_TOKEN` **and** put a TLS-terminating reverse proxy in front. Without a
token the mutating endpoints (`rescan` / `pause` / `resume`) are **disabled entirely**, which
is a safe default, not a broken one — the dashboard and the read API still work.

Set the token via the environment (`.env`), never in `config.yaml`: an env var beats the file,
so an empty env var would override a token set there.

## GPU passthrough

Only needed if `config.yaml` sets a hardware `encoder:`. Hardware encoders are a
**backlog-drain** tool — meaningfully worse quality-per-bit than `encoder: cpu` (libx265),
which stays the archival default. The output is held to the **identical** no-loss gate either
way, so a bad hardware encode is rejected rather than shipped.

**NVIDIA (`nvenc`, `av1_nvenc`) — supported.** Needs the [NVIDIA Container
Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/) on the host:

```yaml
deploy:
  resources:
    reservations:
      devices:
        - driver: nvidia
          count: 1
          capabilities: [gpu]
```

This works because the NVIDIA toolkit **injects the driver libraries** (`libnvidia-encode`) into
the container, and the bundled ffmpeg is dynamically linked against glibc precisely so it can
`dlopen` them (a fully-static ffmpeg could not).

**Intel Quick Sync (`qsv`), VAAPI (`vaapi`), AMD (`amf`) — NOT supported by this image.** Be clear
about why, because the failure is otherwise baffling. ffmpeg *is* built with `--enable-vaapi` and
`--enable-libvpl`, but each of these needs a **vendor userspace library inside the container** that
nothing puts there: QSV/VAAPI need a VA driver (`iHD_drv_video.so` and friends), and AMF needs
AMD's own `libamfrt64.so` (it does not go through VA-API at all). Passing `/dev/dri` supplies only
the *kernel* device node — it is not a driver, and there is no Intel/AMD equivalent of the NVIDIA
toolkit's library injection. The distroless base has no package manager to install one either. So
`encoder: qsv` here fails its startup capability check and exits non-zero — loudly, never silently
falling back to CPU, but it does not work.

If you need QSV/VAAPI/AMF: run the binary on the host with an ffmpeg that has libvmaf, or build
your own image **on a base that carries the vendor driver stack** (not distroless — it has no
package manager) and copy the `holdfast` binary into it. Note the fixture suite is gated against
the pinned ffmpeg, so a different ffmpeg is a different measuring instrument.

**There is no silent fallback.** If the configured encoder cannot actually encode on this host,
`holdfast` fails loud at startup and exits non-zero. The capability check really encodes a
clip and probes the result, because a hardware encoder can exit 0 while writing nothing when no
device is present.

## Building and verifying it yourself

```bash
make image                     # docker buildx, single platform, tagged holdfast:dev
make image-smoke               # build, then drive a REAL encode inside the image
```

`scripts/smoke-image.sh` is the packaging gate CI runs. It does not check that the image built
— it checks that the image *works*: it encodes a real file with the bundled ffmpeg, and asserts
the source was replaced by a smaller HEVC file that passed every gate, with no temp left
behind. Run it against any image you are about to trust.

## Known limitations

- **Only NVIDIA hardware encoding works in this image.** `qsv` / `vaapi` / `amf` each need a vendor
  userspace library the image does not carry (a VA driver for QSV/VAAPI; `libamfrt64` for AMF) —
  see "GPU passthrough" above for why and what to do instead. `cpu` (libx265, the archival default)
  and `svtav1` need no device at all.
- **arm64 is built and its ffmpeg is checksum-verified, but CI only runs the full encode smoke
  test on amd64** — the arm64 image is exercised under QEMU far enough to prove it *executes*
  (binary, glibc, bundled ffmpeg), which is what a cross-built image gets wrong. A real arm64
  encode has not been timed on real hardware; an SBC will be slow with libx265.
- **No shell in the image.** `docker exec ... sh` will not work. To poke at the bundled ffmpeg:
  `docker run --rm --entrypoint /usr/local/bin/ffmpeg ghcr.io/nschatz/holdfast:latest -version`.
- **Power-loss durability of the swap is filesystem-dependent.** holdfast follows the POSIX
  durable-rename discipline — `fsync` the encode before the rename, `fsync` the parent directory
  after it, and (in the container-changing case) never remove the source until that directory
  `fsync` has succeeded. On a local journaled or copy-on-write filesystem (ext4, XFS, Btrfs, ZFS)
  that makes the completed swap survive a power cut. On a **networked or stacked** library mount
  (NFS, SMB/CIFS, overlayfs) the durability of a directory `fsync` is weaker or server-defined, so
  a power loss there can still lose a just-completed swap — it fails *safe* (a duplicate or the
  original, never a torn or missing file), but the reclaim may not persist. This cannot be proven
  in CI (it needs a power-cut harness), so it is stated as a limitation, not a guarantee; prefer a
  local filesystem for the `/media` mount.
- The image is **private until the repository is** — GHCR package visibility follows the repo.
