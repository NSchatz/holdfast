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
container network, IS that decision: on the shipped defaults it removes the only protection
the read surface has. `server_read_token`, below, is how you give it one of its own.

<a id="reverse-proxy-posture"></a>

**Reverse-proxy posture.** Read this before you give holdfast a hostname.

With `server_read_token` unset - the shipped default - the read API (`/api/summary`,
`/api/queue`, `/api/history`, `/api/events`) is **unauthenticated**. Nothing in this daemon
checks a credential for it; it is protected by the loopback bind and by nothing else. Put a
proxy in front and that bind protects nothing, so the proxy's own authentication becomes
**the only barrier** in front of every media path in your library. Configure forward auth
(Authelia, oauth2-proxy, whatever your proxy calls it) on the route before the hostname
resolves, not after.

Point `server_read_token` at a secret and those four endpoints require an
`Authorization: Bearer` credential of their own, so the proxy in front of them becomes
**defence in depth** rather than the only barrier: a proxy misconfiguration stops being
total exposure of every path in your library. It is a second, independent key - it buys
reads and never a mutation, and the control token is accepted on the reads too, because one
`Authorization` header cannot carry two values. It is reached by reference like every other
credential here:

```yaml
services:
  holdfast:
    environment:
      - HOLDFAST_SERVER_READ_TOKEN=file:/run/secrets/holdfast_read_token
```

**It does not gate the dashboard.** Even with a read token set, the page is still served
with no credential, and so are its assets, so the proxy IS still the only barrier in front
of the page - keep forward auth on that route. A browser sends no `Bearer` header on a
navigation, so gating the page on this key would serve a login-less 401 to every operator
who opened it; the page needs a cookie set from a login form, which does not exist yet.
Until it does, with a read token set **the page loads but its data does not**: the document
renders and its own requests to `/api/summary` and `/api/events` carry no credential and
are refused. holdfast says so at startup rather than leaving you to find it. A half-gated
surface that reads as gated is worse than an open one that says it is open.

`/metrics` is gated by neither key. Its reachability is governed by `metrics_enable` alone,
because the exposition carries counters, a byte total and two histograms labelled only by
outcome and by state - it names no file - and a scrape credential is the one thing a
Prometheus deployment most often cannot supply.

The token-gated group stays **disabled** until a control token is configured. With no
`server_auth_token` reference set (or `HOLDFAST_SERVER_AUTH_TOKEN` in the environment),
`rescan`, `scan`, `pause`, `resume`, the ledger search (`/api/search`) and the withheld
paths (`/api/exclusions`) answer **403** to every caller - a safe default, not a broken
one, and the dashboard and the read API still work, neither of them being gated by this
key. The ledger search is in that group and not among the reads `server_read_token` gates,
for a reason worth stating: the capped reads ship at most a few hundred rows, so a search
over the whole ledger serves per-file rows they have never served, and gating it on the
control token keeps this a control-gated read rather than one more read that is open
whenever `server_read_token` is unset. A proxy identity header (`Remote-User`,
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

The control token is reached **by reference** and never written anywhere as a literal. Mount
the token as a file and point the key at it:

```yaml
services:
  holdfast:
    environment:
      - HOLDFAST_SERVER_AUTH_TOKEN=file:/run/secrets/holdfast_control_token
    secrets:
      - holdfast_control_token

secrets:
  holdfast_control_token:
    file: ./control-token.txt      # gitignored; mode 0400
```

A literal token in `config.yaml` **or** in `HOLDFAST_SERVER_AUTH_TOKEN` refuses to start.
That is deliberate: holdfast starts `ffmpeg` as a child process, a child inherits its
parent's environment, and a credential in the environment is readable from every encoder
invocation's `/proc/<pid>/environ`. The same applies to `server_read_token`, `notify_url`
and `tautulli_api_key`. `docs/secrets.md` has the reference forms and the migration.

## Telling holdfast about one file: Sonarr / Radarr

`POST /api/scan` takes a list of paths and looks at exactly those files. The *arr already
knows when an import finished, which is the hard part, so wiring this up lets you turn the
periodic scan off entirely (`scan_interval_sec: 0`) and still have every new file examined
the moment it lands.

It is a **targeted scan**, not a second pipeline. An accepted path goes through the same
guards, the same claim and the same swap discipline a whole-library scan puts it through,
and records the same verdict. It re-encodes nothing a scan would have skipped, and it is
**not** `requeue`: a file a terminal row already answered stays answered.

### The request

```bash
curl -sS -X POST http://holdfast:8080/api/scan \
  -H "Authorization: Bearer $(cat /run/secrets/holdfast_control_token)" \
  -H "Content-Type: application/json" \
  -d '{"paths": ["/library/tv/Show/Season 01/Show - S01E01.mkv"]}'
```

The token is the value `server_auth_token` points at - the same one `rescan`, `pause` and
`resume` take. With no control token configured this answers **403**, like every other
mutating endpoint.

The answer is **202** with a per-path report, and it comes back immediately: the file is
queued, not encoded while you wait. Full request and response shapes, every refusal status,
and the per-request limits are in [docs/api-reference.md](api-reference.md).

### Sonarr / Radarr: `Connect > Custom Script`

This is the route that needs nothing in between, because the *arr hands the script the
imported file's path in its own environment variable. Add a script to the container the
*arr runs in, and point `Settings > Connect > + > Custom Script` at it with **On Import**
and **On Upgrade** ticked:

```bash
#!/bin/sh
# Sonarr sets sonarr_episodefile_path; Radarr sets radarr_moviefile_path.
# On a Test both are empty, which is how this exits 0 without calling anything.
path="${sonarr_episodefile_path:-$radarr_moviefile_path}"
[ -n "$path" ] || exit 0

curl -sS --fail-with-body -X POST http://holdfast:8080/api/scan \
  -H "Authorization: Bearer ${HOLDFAST_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$(printf '{"paths":["%s"]}' "$path")"
```

### Sonarr / Radarr: `Connect > Webhook`

The Webhook connection posts **the *arr's own JSON**, which carries the path under
`episodeFile.path` (Sonarr) or `movieFile.path` (Radarr), inside an envelope with an
`eventType` and a good deal else. holdfast does not parse it: it reads `{"paths": [...]}`
and nothing else, deliberately, because a webhook payload shape is a third party's schema
and this endpoint is not an *arr client. So the Webhook connection reaches holdfast through
a shim that reshapes the body - anything that speaks HTTP will do:

| Field | Value |
|---|---|
| URL | `http://your-shim:9000/sonarr` |
| Method | `POST` |
| Username / Password | leave empty - holdfast takes a bearer token, not basic auth |

and the shim forwards, adding the header and picking the one field out:

```bash
# jq -r '.episodeFile.path // .movieFile.path' turns the *arr envelope into the path,
# and the body holdfast reads is built from that and nothing else.
curl -sS -X POST http://holdfast:8080/api/scan \
  -H "Authorization: Bearer ${HOLDFAST_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$(jq -c '{paths: [(.episodeFile.path // .movieFile.path)]}' <<<"$arr_payload")"
```

If you would rather not run a shim, use the Custom Script route above.

### The paths must be the paths holdfast sees

**This is the one thing that bites.** Sonarr sends the path *it* knows the file by, and
that is the path inside **Sonarr's** container. holdfast resolves what it is sent against
its own filesystem and against its own `library_roots`, so `/tv/Show/S01E01.mkv` from
Sonarr means nothing to a holdfast that mounts the same file at
`/library/tv/Show/S01E01.mkv`: the submission is **rejected**, under
`outside-library-roots` or `not-a-regular-file`, and reported as such in the response. It
is never silently ignored, and it never reaches a file holdfast was not pointed at.

Two ways out, and the first is much better:

- **Mount the library at the same path in both containers.** Give Sonarr, Radarr and
  holdfast the identical bind (`/library:/library`), so every path any of them produces is
  a path all of them understand. This is the same advice the *arr documentation gives for
  hardlinks and atomic moves, so a deployment that already follows it needs nothing here.
- **Rewrite the prefix in the shim or the script**, if the mounts genuinely cannot be
  aligned: `path=$(printf '%s' "$path" | sed 's|^/tv/|/library/tv/|')`. Keep the rewrite in
  one place. holdfast will not guess it for you - guessing a path prefix on a tool that
  deletes originals is not a trade worth making.

A submitted path is resolved (symbolic links followed, `..` resolved away) **before** it is
checked against `library_roots`, so a path that climbs out of your library, or a link
pointing outside it, is refused rather than acted on.

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
