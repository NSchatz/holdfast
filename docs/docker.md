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

<a id="swap-metadata"></a>

**What a swap CHANGES about a replaced file.** Everything above is what holdfast NEEDS from
your filesystem. This is what it does to the file it publishes - none of it visible unless
you go looking, and all of it library-wide the first time you point holdfast at a library.

The replacement carries the source's mode. A source at `0640` is replaced by a file at
`0640`, whatever umask holdfast is running under. The nine permission bits travel, and so do
setuid, setgid and sticky if the source had them.

Ownership is carried only where holdfast is privileged to carry it. Changing a file's owner
needs `CAP_CHOWN` or root, and a container running as an ordinary `user:` has neither - so
on a rootless deployment the replacement is owned by the holdfast uid and gid rather than by
the source's, holdfast says so once per run, and the swap still happens. Run as a `user:`
that already owns the media and the question never arises, which is the same advice the
paragraph above gives for a different reason.

The modification time is carried from the source unless `preserve_mtime` is false. It
defaults to true, because resetting it makes a first pass over a library look to Plex and
Jellyfin like the whole library arrived at once: "Recently Added", every date-based sort and
every smart collection built on one moves with it, and nothing puts it back. Set
`preserve_mtime: false` if you would rather the replacement's mtime say when the bytes were
actually written. Either way the file's identity still moves, because a swap always makes
the file smaller - so a resume reads the replacement as a new file and never as the source
it already processed.

ACLs and xattrs are not carried. POSIX ACLs, SELinux labels and every other extended
attribute on the source are left behind: the replacement gets whatever your filesystem gives
a newly created file, and nothing in holdfast reads or writes them. A library whose access
depends on POSIX ACLs needs them reapplied after a pass.

### One process per `state_dir`

**Running more than one holdfast process against a single `state_dir` is unsupported.** The job
store under `/state` is single-writer: one holdfast process serializes every access to it, and
that serialization does not reach across processes. A second process pointed at the same
`state_dir` contends for a store that is neither built nor proven to be shared, so this is not a
deployment to tune - it is one not to build. Pointing two processes at the same **library** is
worse still, for the reason two different transcoders must not share one: both write a temp file
beside the source and both delete sources.

**Do this instead.** Run one container per `state_dir`. A library that genuinely needs its own
daemon gets its own container, its own `state_dir` volume and its own `/media` mount - never a
second process on the first one's state. To use more of one machine, do not start a second
process: raise `workers`.

```yaml
workers: 1   # concurrent encode workers inside the one daemon; the default
```

`workers` is 1 by default on purpose: a CPU libx265 encode already **saturates the available
cores** by itself, so a second concurrent encode mostly takes cores from the first and the pair
finishes no sooner. Raising it is an opt-in for a library of many small or low-resolution files,
or for a hardware encoder - the cases where one encode does not use the whole machine. It buys
concurrency **inside the one daemon**: holdfast is a single process whatever you set it to, which
is the point. That is a design decision, not an unbuilt feature, and the README's
[non-goal](../README.md#non-goals) says why.

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

That is the shipped default. It is loopback-only for THAT deployment, and it is the one to
keep until putting holdfast behind a reverse proxy is a decision you have actually made.
Publishing the port more widely, or letting a proxy reach the container over a shared
container network, IS that decision: it removes the only protection the read surface has.

<a id="reverse-proxy-posture"></a>

**Reverse-proxy posture.** Read this before you give holdfast a hostname.

The dashboard and the read API (`/api/summary`, `/api/queue`, `/api/history`,
`/api/events`) are **unauthenticated**. Nothing in this daemon checks a credential for
them; on the shipped defaults they are protected by the loopback bind and by nothing else.
Put a proxy in front and that bind protects nothing, so the proxy's own authentication
becomes **the only barrier** in front of every media path in your library. Configure
forward auth (Authelia, oauth2-proxy, whatever your proxy calls it) on the route before
the hostname resolves, not after.

The mutating endpoints stay **disabled** until a control token is configured. With no
`server_auth_token` set (or `HOLDFAST_SERVER_AUTH_TOKEN` in the environment), `rescan`,
`pause` and `resume` answer **403** to every caller - a safe default, not a broken one, and
the dashboard and the read API still work. A proxy identity header (`Remote-User`,
`Remote-Groups`, `Remote-Email`, `Remote-Name`, any `X-Forwarded-*`) is **never**
authorization for them: only a matching `Authorization: Bearer` token is, so a proxy that
can be talked into forging one of those headers gains nothing by it. Enabling the controls
is a decision separate from putting a proxy in front, and it is the one that gives a stolen
bearer token something to buy.

Serve holdfast at the **host root**, on a hostname of its own. The page asks for its own
API and its own assets with **root-relative** paths (`/api/events`, `/api/rescan`), so a
router that strips or rewrites a path prefix breaks it silently: the document loads and
every request under it 404s. Pass the Host header through, and put no prefix strip and no
path rewrite on this route.

Set the control token via the environment (`.env`), never in `config.yaml`: an env var
beats the file, so an empty env var would override a token set there.

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

### Building a modified holdfast: the source offer

The dashboard offers its Corresponding Source (AGPL-3.0 section 13). An image built from this
repository unchanged offers this tree; an image you build from a **modified** tree must offer
yours, and one build argument does it, with no patching of the embedded HTML:

```bash
docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast -t my/holdfast .
make image SOURCE_URL=https://git.example.org/me/holdfast    # the same thing through the Makefile
```

`SOURCE_URL` rides the same `-ldflags` invocation as `VERSION`/`COMMIT`/`DATE`, so the link and
the build identity the page shows always name the same tree. It must be an absolute `http://` or
`https://` URL: the container **exits non-zero at startup** on anything else, naming the value it
rejected, rather than serving an offer nobody can follow.

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
  `docker run --rm --entrypoint /usr/local/bin/ffmpeg ghcr.io/nschatz/holdfast:v0.1.0 -version`
  (use the same tag and digest `docker-compose.yml` pins, so you are inspecting the image you run).
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
- A GHCR package carries **its own visibility**, separate from the repository's. The repository is
  public and the reference `docker-compose.yml` pins has been published; if a pull without
  credentials is refused, the package itself is still private. `docs/release.md` step 8 is the check
  that settles it.
- **The source-mutation guard has a residual window, and it is different on local storage than on
  a network mount.** The guard re-checks the source immediately before the swap, but it can only
  be as sharp as the attributes it compares. Both windows are stated in
  [The filesystem holdfast runs on](filesystem.md#residual-window-local): one place, so the two
  statements cannot drift apart from each other or from the label holdfast records per job.
