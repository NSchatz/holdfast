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
| Base | `gcr.io/distroless/cc-debian13:nonroot` (Debian 13, trixie; glibc 2.41), pinned by tag and digest: glibc **+ libgcc_s/libstdc++** + CA certs, **no shell, no package manager**. It must be `cc`, not `base`: ffmpeg has a `DT_NEEDED` on `libgcc_s.so.1`, which `base` does not ship, so `base` builds fine and then cannot exec ffmpeg at all. It is Debian 13 because trixie's VA-API stack (`libva2`, Mesa's gallium drivers, Intel's iHD driver) needs glibc 2.38 or later, which the Debian 12 base (glibc 2.36) does not have; the build stages are Debian 13 as well. Sources, read 2026-09-29: [trixie libc6](https://packages.debian.org/trixie/libc6), [bookworm libc6](https://packages.debian.org/bookworm/libc6), [libva2](https://packages.debian.org/trixie/libva2), [mesa-libgallium](https://packages.debian.org/trixie/mesa-libgallium), [intel-media-va-driver-non-free](https://packages.debian.org/trixie/intel-media-va-driver-non-free), [distroless](https://github.com/GoogleContainerTools/distroless/blob/main/README.md). |
| Platforms | `linux/amd64`, `linux/arm64` |
| User | non-root by default (`nonroot`, uid 65532); override with `user:` |
| ffmpeg | pinned by release tag **and verified by SHA-256** before it is trusted |
| Dynamic-HDR tools | `dovi_tool` and `hdr10plus_tool` at `/usr/local/bin/`, on both architectures: upstream's static musl release builds (MIT), each pinned in the Dockerfile by release version **and a per-architecture SHA-256**, verified before it is unpacked. CI installs the same builds with `scripts/install-dynhdr-tools.sh`, which parses that pin, and the image smoke runs both inside the image. The versions are the Dockerfile's `DOVI_TOOL_VERSION` and `HDR10PLUS_TOOL_VERSION`, which NOTICE names and `scripts/check-pins.sh` holds equal. Upstream: [dovi_tool](https://github.com/quietvoid/dovi_tool), [hdr10plus_tool](https://github.com/quietvoid/hdr10plus_tool) |
| Config | **nothing is baked in** — see below |
| Licences | `/usr/share/doc/holdfast/` (AGPL-3.0 + the NOTICE for the bundled ffmpeg, the two dynamic-HDR tools and, on amd64, the hardware runtime); each hardware-runtime package's Debian copyright file in `/usr/share/doc/<package>/` |

**How the pins stay current.** Every base image is pinned by tag and digest on its own `FROM`
line, and `.github/dependabot.yml` has GitHub's Dependabot open a pull request when one moves
upstream - weekly, together with the workflow actions and the Go modules. Nothing merges on
its own: each such pull request runs the full gate and the image smoke, and waits for a human
review. The bundled ffmpeg, `dovi_tool` and `hdr10plus_tool` are watched separately, by
`.github/workflows/pin-health.yml`, which asks upstream every week whether each pinned release
asset is still served.

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
defaults to `true`, because resetting it makes a first pass over a library look to Plex and
Jellyfin like the whole library arrived at once: "Recently Added", every date-based sort and
every smart collection built on one moves with it, and nothing puts it back. Set
`preserve_mtime: false` if you would rather the replacement's mtime say when the bytes were
actually written. Either way the file's identity still moves, because a swap always makes
the file smaller - so a resume reads the replacement as a new file and never as the source
it already processed.

Neither setting is free, and each one hides or moves something:

| | `preserve_mtime: true` (the default) | `preserve_mtime: false` |
|---|---|---|
| the replacement's modification time | the source's | when holdfast wrote the replacement |
| a media server's "Recently Added" and date-based sorts | do not move | move with every swap; a first pass reads as the whole library arriving at once |
| a tool that detects change by modification time alone | does not see the swap, although every byte is different | sees every swap |
| a tool that compares size as well | sees every swap | sees every swap |
| `holdfast restore` inside the undo window | returns the original with the original's modification time | returns the original with the original's modification time |
| after the undo window closes (at once when `undo_window_hours` is 0) | the source's time is still on the file | the source's time is gone for good |

The size row is holdfast's own invariant and not a statement about any particular tool: a
swap happens only when the output is strictly smaller than the source, so a file's size
changes on every swap under either setting. Whether a given scanner or backup reads the
size is for that tool's documentation to say. The restore row holds because the retained
original is the source file itself, kept under a second name and never rewritten
([undo.md](undo.md)).

The default was kept at `true` by the owner's decision (2026-09-29), with the change-detection
cost above known: an install that never set the key behaves as it always has. So that the
choice is not silent, `holdfast validate` prints one `note:` line stating the effective
value of `preserve_mtime`, whether it is the default or was set (in the file or by
`HOLDFAST_PRESERVE_MTIME`), and what that value costs; `run` and `serve` log the same line
at startup. A value that is not `true` or `false` refuses to start.

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
process: raise `workers`. To use ANOTHER machine, do not start a second `serve` or `run` there
either: run a `holdfast worker` on it, which opens neither the `state_dir` nor the library for
writing ([Worker nodes](#worker-nodes)).

```yaml
workers: 1   # concurrent encode workers inside the one daemon; the default
```

`workers` is 1 by default on purpose, and raising it - to a number, or to `auto` - is an opt-in.
It buys concurrency **inside the one daemon**: the process that owns a `state_dir` and a library
is a single process whatever you set it to, which is the point. Encodes can also be leased to
other machines ([Worker nodes](#worker-nodes)), and that changes nothing here: the gates and the
swap of every job stay in this one process, and `workers` is still how it uses more of its own
host. How it interacts with the container's `cpus:` limit and with `max_load` is the next section.

<a id="workers-cpus-and-max-load"></a>

## Workers, `cpus` and `max_load`

Three settings decide how hard holdfast works a host, and each acts somewhere different:
`workers` is how many files are in flight at once, the compose `cpus:` limit is how much CPU the
container may use, and `max_load` is when the feed of new files pauses.

**`workers`.** The default is 1: one file is probed, encoded, measured and swapped at a time. A
whole number from 1 to 1024 runs exactly that many, and `0` or no key at all means 1, whatever
the CPU quota reads. `workers: auto` (or `HOLDFAST_WORKERS=auto`) sizes the pool from the CPU
quota the process runs under instead:

```text
workers = max(1, floor(Q / cores_per_worker)), at most 1024
```

where `Q` is the smaller of the cgroup's `cpu.max` bandwidth (its quota over its period) and the
number of CPUs the process may run on, and `cores_per_worker` (a whole number from 1 to 1024) is
16 by default. It is resolved once, at start. `holdfast validate` prints the count, `Q`, where
`Q` came from (`cgroup cpu.max` or `cpu count`) and `cores_per_worker`, and `run` and `serve`
record the same values at start. A `cpu.max` that cannot be read or parsed never refuses a start:
`Q` falls back to the CPU count and a warning names the file. `cores_per_worker` beside a numeric
`workers` has no effect, and holdfast says so.

**The compose `cpus:` limit is the quota `auto` reads as `Q`.** Docker implements `cpus` as a CFS
bandwidth limit - `--cpus="1.5"` is the equivalent of a `--cpu-period` of 100000 and a
`--cpu-quota` of 150000 ([Docker: resource constraints](https://docs.docker.com/engine/containers/resource_constraints/),
read 2026-09-23) - and the kernel publishes that limit inside the container as its cgroup's
`cpu.max`, `$MAX $PERIOD`, with `max` for no limit
([cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html), read 2026-09-23). A worked
example on a 56-thread host with the default `cores_per_worker: 16`:

| compose `cpus:` | `cpu.max` in the container | `Q` | `workers: auto` |
|---|---|---|---|
| `"4.0"` | `400000 100000` | 4, from `cgroup cpu.max` | max(1, floor(4 / 16)) = **1** |
| no limit | `max 100000` | 56, from `cpu count` | floor(56 / 16) = **3** |

At `cpus: "4.0"`, a smaller `cores_per_worker` is what buys a second worker: `cores_per_worker: 2`
gives floor(4 / 2) = 2.

**`max_load` is the host's load, divided by the CPUs the process may run on - not by the
quota.** It reads the 1-minute load average, the first field of `/proc/loadavg`, and divides it
by the number of CPUs the process may be scheduled on (its affinity set), then pauses the feed
while the result is above `max_load`. Inside a container that load average is the HOST's: read
in a container limited to 2 CPUs of quota on a host with many more, it stood several times higher
than 2 CPUs of work could make it, and its fourth field counted thousands of tasks where the
container ran fewer than a hundred. And in a container given a `cpus:` limit but no CPU set, the
divisor is every CPU of the host. So on the 56-thread host above, `max_load: 0.8` pauses the feed
when the whole host's load passes about 45 (0.8 x 56), whatever `cpus:` gives holdfast and
whichever process is making the load.

**`max_load`, `run_window` and pause gate only the hand-out of NEW files.** The scan feeds every
worker from one queue, and while any of the three says stop, it hands no new file to ANY worker;
every encode already in flight runs to its end, gates and swap included. Nothing is interrupted
and nothing is lost: the files not handed out wait for the next scan. Two routes into the
pipeline are not the scan's feed and are gated differently: `POST /api/scan` refuses a
submission while holdfast is paused, but neither `run_window` nor `max_load` holds one back, and
a file a root's `watch` offers is held back by none of the three.

**Memory scales with workers.** Each worker is a concurrent encode plus, after it, a VMAF
measurement, so `N` workers need about `N` of each in memory at once. The encode memory watchdog
holds each encode to 85% of the container's limit on its own, not the pool to it in total
([docs/encode-memory.md](encode-memory.md)), so size `mem_limit` for `N` encodes together.

**Each encode brings its own threads.** The libx265 pool of every encode is sized to the whole
CPU quota, rounded down (or to `x265_cpus`), and is never divided by `workers`; with no quota it
sizes itself from the host. `N` workers therefore run up to `N` pools of that size against the
same `Q`, and the scheduler shares the quota between them. The VMAF measurement is the other way
round: each one's threads are the quota divided by the files in flight, and the measurements
running at once are held to the quota between them. A load average read while `N` workers encode
reflects all of that, plus every other process on the host.

**The free-space reservation can hold a worker waiting.** Before a job encodes, its source's
size is reserved against the filesystem its working file is written on - the scratch
filesystem, or the source's own - and a job whose source fits the free space but not beside
what the other jobs in flight there have reserved waits for one of them to finish, then checks
again, rather than failing. A worker can therefore sit idle on a nearly full filesystem while
another encodes; it says so once, at info, naming the file, the free space and the bytes
reserved. See [docs/scratch.md](scratch.md).

## Timezone

`run_window` is evaluated in **local time**. The image carries the zone database, but a
container with no `TZ` is **UTC** — set `TZ` or your "encode overnight" window will run at the
wrong hours, silently and correctly, on the wrong clock.

Every log line's `time=` field ends in the process's numeric UTC offset, so the clock a line
was written on is stated on the line itself: a container with no `TZ` logs `+00:00`, never a
bare `Z`, and a one-shot `docker run ... restore` whose `TZ` differs from the service's shows
a different offset beside the service's lines rather than a clock that only looks different.
Give every `docker run` the same `TZ` as the service and the two read as one timeline.

## The control surface

`holdfast serve` exposes the HTTP JSON API. Its default bind is `127.0.0.1`, which inside a
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

**It does not gate the root path.** holdfast ships no frontend, so `/` is a plain-text page
naming the endpoints and carrying the Corresponding Source offer. (A web UI is to ship on this
API - decided 2026-09-29 by the owner (T14, T18); until that release, `/` is this page.) It is still served with no
credential when a read token is set, and it holds no library datum for a credential to
protect - every media path is behind `/api/queue`, `/api/history` and `/api/events`, which
the key does gate. holdfast says so at startup rather than leaving you to find it.

`/metrics` is gated by neither key. Its reachability is governed by `metrics_enable` alone,
because the exposition carries counters, a byte total and two histograms labelled only by
outcome and by state - it names no file - and a scrape credential is the one thing a
Prometheus deployment most often cannot supply.

The token-gated group stays **disabled** until a control token is configured. With no
`server_auth_token` reference set (or `HOLDFAST_SERVER_AUTH_TOKEN` in the environment),
`rescan`, `scan`, `pause`, `resume`, the ledger search (`/api/search`) and the withheld
paths (`/api/exclusions`) answer **403** to every caller - a safe default, not a broken
one, and the read API still works, not being gated by this key. The ledger search is in that group and not among the reads `server_read_token` gates,
for a reason worth stating: the capped reads ship at most a few hundred rows, so a search
over the whole ledger serves per-file rows they have never served, and gating it on the
control token keeps this a control-gated read rather than one more read that is open
whenever `server_read_token` is unset. A proxy identity header (`Remote-User`,
`Remote-Groups`, `Remote-Email`, `Remote-Name`, any `X-Forwarded-*`) is **never**
authorization for them: only a matching `Authorization: Bearer` token is, so a proxy that
can be talked into forging one of those headers gains nothing by it. Enabling the controls
is a decision separate from putting a proxy in front, and it is the one that gives a stolen
bearer token something to buy.

A proxy that also carries worker nodes has requirements of its own - an upload-sized request
body, timeouts that outlast a long-poll, and no login on the node route:
[Worker nodes](#worker-nodes).

Serve holdfast at the **host root**, on a hostname of its own. The API's own paths are
absolute (`/api/events`, `/api/rescan`), so a router that strips or rewrites a path prefix
serves the root page and 404s every request under it. Pass the Host header through, and put
no prefix strip and no path rewrite on this route.

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
invocation's `/proc/<pid>/environ`. The same applies to `server_read_token`, `notify_url`,
`tautulli_api_key`, `radarr_api_key`, `sonarr_api_key`, `plex_token`, `webhook_token`,
`node_token` and `server_tls_key`.
`docs/secrets.md` has the reference forms and the migration.

## Telling holdfast about one file: Sonarr / Radarr

Sonarr and Radarr already know the moment an import finishes, which is the hard part. Point a
`Connect > Webhook` connection in each at holdfast's **native webhook intake** and every imported,
upgraded or renamed file is examined as it lands - so the periodic scan can be turned off entirely
(`scan_interval_sec: 0`). Nothing sits in between: no script in the arr's container and no shim to
reshape the payload. holdfast reads the arr's own JSON.

No *arr to wire up? A library root can carry `watch: true` instead, and holdfast hears about
the file from the platform's own filesystem events - see
[docs/profiles.md](profiles.md#watch-and-watch_settle_sec---how-a-new-file-under-this-root-is-found).
It is off unless a root asks for it, and it accelerates the periodic scan rather than
replacing it.

It is a **targeted scan**, not a second pipeline. An accepted path goes through the same
guards, the same claim and the same swap discipline a whole-library scan puts it through,
and records the same verdict. It re-encodes nothing a scan would have skipped, and it is
**not** `requeue`: a file a terminal row already answered stays answered.

<a id="webhook"></a>

### Sonarr / Radarr: `Connect > Webhook`

**1. Give the intake a credential of its own.** It is off until you do: with no `webhook_token`
both endpoints answer **403**.

```yaml
# config.yaml
webhook_token: file:/run/secrets/holdfast_webhook_token
```

```yaml
# compose
services:
  holdfast:
    secrets: [holdfast_webhook_token]
secrets:
  holdfast_webhook_token:
    file: ./secrets/holdfast_webhook_token
```

Like every credential it is a **reference** (`file:` or `cmd:`), and a literal value in the file
or in `HOLDFAST_WEBHOOK_TOKEN` refuses to start ([docs/secrets.md](secrets.md)). It is **not**
`server_auth_token`, deliberately: the secret an arr holds can queue a file inside a configured
library root and do nothing else - it cannot pause holdfast, start a scan or withhold a path, and
it is not accepted where `server_read_token` gates the reads - and the control token is not accepted on the intake, so there is no reason to hand it
to an arr. Writing `webhook_token` as the same reference as `server_auth_token` or
`server_read_token` refuses to start, and so does a `webhook_token` that resolves to the same
value as either. (If `server_read_token` is unset the read endpoints are open to every caller
anyway, as they are without this key - see [the control surface](#reverse-proxy-posture).)

**2. Add the connection.** In each arr: `Settings > Connect > + > Webhook`.

| Field | Sonarr | Radarr |
|---|---|---|
| Triggers | **On File Import**, **On File Upgrade**; optionally **On Import Complete** and **On Rename** | **On File Import**, **On File Upgrade**; optionally **On Rename** |
| Webhook URL | `http://holdfast:8080/api/webhook/sonarr` | `http://holdfast:8080/api/webhook/radarr` |
| Method | `POST` (`PUT` is accepted too) | `POST` (`PUT` is accepted too) |
| Username | anything, for example `holdfast` - it is not checked | the same |
| Password | the webhook token's value | the same |

That sends the token as the password of HTTP Basic authentication. If you would rather send a
header, leave Username and Password empty and add one under the advanced **Headers** field:
`Authorization` = `Bearer <the token's value>`. Either is accepted; nothing else is. There is no
URL form of the credential (`?token=...`), because a URL reaches access logs.

Press **Test**. holdfast answers **200** to a Test that carries the right credential and comes
from the right arr, and queues nothing for it. A Test that fails tells you which thing is wrong:

| Test result | What it means |
|---|---|
| 403 | `webhook_token` is not configured in holdfast |
| 401 | the Password (or the header) is not the token's value |
| 400 | the connection points at the other arr's endpoint (a Sonarr connection at `/api/webhook/radarr`, or the reverse) |

**3. Which events do what.**

| Event | What holdfast does |
|---|---|
| **On File Import** | queues the imported file |
| **On File Upgrade** | queues the new file. An upgrade arrives as the same `Download` event with `isUpgrade` set; the file it replaced is not touched |
| **On Import Complete** (Sonarr) | queues every file of the import. With On File Import also ticked each file is announced twice; both mentions go through the same claim, so the file is not encoded twice |
| **On Rename** | queues the file under its new path. A job already queued under the old path is not dropped: when its turn comes the old path no longer exists, and it ends with nothing claimed and nothing recorded |
| anything else, if ticked | answered **200** and ignored |

The answer is **202** with a per-file report, and it comes back immediately: the file is queued,
not encoded while the arr waits. Every case in which nothing is queued for a reason that is not a
fault - an event holdfast does not act on, a file outside every library root, holdfast paused - is
answered **200** with the reason in the body and one line in holdfast's log, so the arr does not
mark the connection as failing. The full response shape and every status are in
[docs/api-reference.md](api-reference.md#webhook-intake).

The payload shapes were read from the arr source at Sonarr `v4.0.20.3014` and Radarr
`v6.4.4.10685` (read 2026-10-02; the citations are in
[docs/design/media-clients.md](design/media-clients.md#webhook-intake)). It has not been run
against a live Sonarr or Radarr by this project: a live check is the owner's to run.

### The paths must be the paths holdfast sees

An arr sends the path *it* knows the file by, and that is the path inside **its** container.
holdfast resolves what it is sent against its own filesystem and its own `library_roots`, so
`/tv/Show/S01E01.mkv` from Sonarr means nothing to a holdfast that mounts the same file at
`/library/tv/Show/S01E01.mkv`: the file is **refused**, under `outside-library-roots` or
`not-a-regular-file`, named in the response and in the log. It is never silently ignored, and it
never reaches a file holdfast was not pointed at.

Two ways out:

- **Mount the library at the same path in every container.** Give Sonarr, Radarr and holdfast
  the identical bind (`/library:/library`), so every path any of them produces is a path all of
  them understand. This is the same advice the *arr documentation gives for hardlinks and atomic
  moves, so a deployment that already follows it needs nothing here.
- **Declare the difference in a path map.** Where the mounts cannot be aligned, say so once, in
  holdfast's configuration:

  ```yaml
  sonarr_path_map:
    - {from: /library/tv, to: /tv}          # holdfast's view -> Sonarr's view
  radarr_path_map:
    - {from: /library/movies, to: /movies}  # holdfast's view -> Radarr's view
  ```

  These are the same two maps the post-swap rescan uses
  ([docs/post-swap-hook.md](post-swap-hook.md)), read in the other direction: the intake takes a
  path in the arr's view (`to`) and answers holdfast's (`from`). The longest matching prefix wins,
  on whole path components, and a path no entry matches is left as it is. A path map needs no
  `sonarr_url` or `radarr_url` beside it: the intake works without the rescan client.

  holdfast will not guess a prefix that is not written down - guessing a path on a tool that
  replaces originals is not a trade worth making.

The mapped path is then resolved (symbolic links followed) **before** it is checked against
`library_roots`, exactly as a path sent to `POST /api/scan` is, so a link pointing outside your
library is refused rather than acted on. A path carrying a `.` or `..` segment, a doubled slash or
a trailing slash is refused outright (`path-not-clean`), unmapped: an arr does not send one.

### Anything else: `POST /api/scan`

`POST /api/scan` is the generic form of the same thing, for whatever is not an arr: it takes a
list of paths, in holdfast's own view, and looks at exactly those files.

```bash
curl -sS -X POST http://holdfast:8080/api/scan \
  -H "Authorization: Bearer $(cat /run/secrets/holdfast_control_token)" \
  -H "Content-Type: application/json" \
  -d '{"paths": ["/library/tv/Show/Season 01/Show - S01E01.mkv"]}'
```

The token is the value `server_auth_token` points at - the same one `rescan`, `pause` and
`resume` take, and **not** the webhook token. With no control token configured this answers
**403**, like every other mutating endpoint. It applies no path map: the paths it is sent are
judged as they stand. Full request and response shapes, every refusal status and the per-request
limits are in [docs/api-reference.md](api-reference.md).

<a id="worker-nodes"></a>

## Worker nodes

Off by default. A worker node is a second machine that **only encodes**: it leases one job at a
time from the server, runs the ffmpeg command line the server's plan built, and sends the output
back over HTTP. The server names the working file that output lands in, re-runs every gate
against its own copy of the source and makes the same-filesystem rename itself, so nothing in
"Volumes and permissions" above changes and no node's verdict licenses a swap. The argument is in
[docs/design/nodes.md](design/nodes.md#leases); this section is the deployment.

### What runs where

| | Where | Command | Holds |
|---|---|---|---|
| the server | the one host that owns the library and `/state` | `serve` | the configuration, the job store, every gate, the rename |
| a worker, one per node | any other host | `worker` | a work directory, the node credential and, in mapped mode, a read-only mount of the library |

Both are the **same image at the same tag**: the image's entrypoint is the `holdfast` binary, so a
worker is the compose `command: ["worker", "--config", "/config/worker.yaml"]` where the server's
is `["serve", "--config", "/config/config.yaml"]`. Only `serve` leases work; a oneshot `run`
leases nothing. A worker opens every connection itself and listens on nothing, so a worker
container publishes no port.

One server per `state_dir` is still the rule ([above](#one-process-per-state_dir)): more machines
means more workers leasing from the one server, never a second `serve`.

### The server side

Three things change on the server: the node credential, a listener the workers can reach, and
TLS in front of it.

```yaml
# config.yaml (the server) - the keys a node deployment adds
server_addr: 0.0.0.0:8080
node_token: file:/run/secrets/holdfast_node_token          # this is what turns nodes on
server_read_token: file:/run/secrets/holdfast_read_token   # see below: not optional here
server_auth_token: file:/run/secrets/holdfast_control_token
server_tls_cert: /config/tls/server.pem                    # a plain path: the PEM chain
server_tls_key: file:/run/secrets/holdfast_tls_key         # a secret reference, never a literal

# The caps, at their shipped defaults. Every one is ASSUMED: nobody has measured a
# node deployment.
node_lease_ttl_sec: 60        # a lease lives this long without a heartbeat
node_max_leases: 4            # live leases across every node
node_max_leases_per_node: 1   # live leases one node may hold
node_max_transfers: 2         # uploads in flight across every node
node_gate_slots: 1            # node outputs the server gates at once
```

```yaml
# compose (the server) - what changes from the shipped docker-compose.yml
services:
  holdfast:
    command: ["serve", "--config", "/config/config.yaml"]
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - ./tls/server.pem:/config/tls/server.pem:ro
      - ./state:/state
      - /srv/media:/media
    ports:
      - "192.0.2.10:8080:8080"   # the address the workers reach, no longer loopback only
    secrets:
      - holdfast_node_token
      - holdfast_read_token
      - holdfast_control_token
      - holdfast_tls_key

secrets:
  holdfast_node_token:
    file: ./secrets/holdfast_node_token      # gitignored; mode 0400, owned by the `user:` above
  holdfast_read_token:
    file: ./secrets/holdfast_read_token
  holdfast_control_token:
    file: ./secrets/holdfast_control_token
  holdfast_tls_key:
    file: ./secrets/holdfast_tls_key
```

**`node_token` is what turns nodes on, and it opens nothing else.** With it unset no lease is
granted and every endpoint under `/api/node/v1` answers **403**. It is a reference (`file:` or
`cmd:`) like every credential here, and a literal in the file or in `HOLDFAST_NODE_TOKEN` refuses
to start ([docs/secrets.md](secrets.md)). It must be a secret of its own: written as the same
reference as `server_auth_token`, `server_read_token` or `webhook_token` it refuses to start, and
so does one that resolves to the same value. The read, control and webhook tokens are not
accepted on the node endpoints, and the node token is accepted nowhere else.

**Set `server_read_token` in the same change.** Nodes need the server on an address other
machines can reach, and with `server_read_token` unset the read API (`/api/summary`,
`/api/queue`, `/api/history`, `/api/events`) is unauthenticated: publishing the port to a network
serves **every media path in your library** to that network. holdfast says so at startup; this is
the deployment in which that notice stops being hypothetical. `server_auth_token` is in the
excerpt for the same reason it is anywhere else - without it the controls answer 403 - and it is
not something a worker needs or should hold. [The control surface](#reverse-proxy-posture) has
both in full.

**TLS, built in.** `server_tls_cert` is a plain path to a PEM certificate chain and
`server_tls_key` a secret reference to its key; set both or neither. With them `serve` listens
with TLS on `server_addr`. The certificate has to be one the workers will verify for the host
name in their `worker_server`: one from a public authority, or one from your own, in which case
each worker is given that authority's certificate with `worker_tls_ca` (below). There is no
option that skips verification.

**TLS, at a reverse proxy instead.** Leave the two keys unset, keep the published port on
loopback or a private container network, and let the proxy terminate TLS. Everything in the
[reverse-proxy posture](#reverse-proxy-posture) applies, and a route that carries nodes has four
more requirements:

- **Its request-body limit must admit an output-sized upload.** An output is a whole encoded
  file, as large as its source less one byte at most. A proxy default of a megabyte or a hundred
  refuses every upload.
- **It must not time out a long-poll.** A worker with nothing to do waits up to 30 s for an
  answer (**ASSUMED**; [docs/design/nodes.md](design/nodes.md#leases)). A proxy that gives up on
  a quiet request sooner turns every idle poll into an error the worker backs off from. The same
  goes for the transfers: a source or an output takes as long as the link makes it.
- **No login in front of `/api/node/v1`.** A worker sends `Authorization: Bearer` with the node
  token and nothing else. It answers no challenge and **follows no redirect**, so forward auth
  on that route, or a redirect from `http://` to `https://`, stops it. The node token is the
  authentication of that route.
- **The hop from the proxy to holdfast is plain HTTP.** Keep it on the same host or a network
  only the two share.

### A mapped-mode worker

`worker_mode: mapped` is the default. The node reads each source through **its own mount** of the
library and translates the path the server names into the path it sees with `worker_path_map`.

```yaml
# worker.yaml (node-a)
worker_server: https://holdfast.example.internal:8080
worker_name: node-a
worker_mode: mapped
node_token: file:/run/secrets/holdfast_node_token
worker_slots: 1                    # encodes this worker runs at once; the default
worker_work_dir: /work             # local disk: each encode is written here, then uploaded
worker_tls_ca: /config/ca.pem      # only for a private or self-signed server certificate

library_roots:
  - /mnt/library                   # this worker's mount of the library
worker_path_map:
  - {from: /media, to: /mnt/library}   # the server's view -> this worker's view
```

```yaml
# compose (node-a)
services:
  holdfast-worker:
    image: ghcr.io/nschatz/holdfast:<the tag and digest the server runs>
    command: ["worker", "--config", "/config/worker.yaml"]
    restart: unless-stopped
    user: "1000:1000"              # may READ the library and write /work
    environment:
      - TZ=Etc/UTC
    volumes:
      - ./worker.yaml:/config/worker.yaml:ro
      - ./ca.pem:/config/ca.pem:ro
      - /srv/media:/mnt/library:ro   # READ-ONLY. A worker never writes into the library.
      - ./work:/work                 # local disk, writable by the `user:` above
    secrets:
      - holdfast_node_token
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL

secrets:
  holdfast_node_token:
    file: ./secrets/holdfast_node_token    # the same value the server's reference resolves to
```

**Mount the library read-only.** A worker only reads it: the output goes to `/work` and from
there over HTTP, and the server is the only writer beside the sources. `:ro` makes that true by
construction rather than by the worker's good behaviour, whatever a lease tells its ffmpeg to do.

**The path map is refused, never guessed.** The longest matching prefix wins, on whole path
components, and a source no entry covers fails that lease `unmapped_source` rather than being
read from wherever the unchanged name leads on the node. A worker that mounts the library at the
server's own path says so with an entry mapping the directory to itself. A wrong entry that
still finds a file is caught twice: the size and modification time the lease was granted on must
match what the worker sees, and the sha-256 of the bytes the worker read must equal the server's
own hash of its source before any gate runs.

**Set `worker_work_dir`.** Unset, it is `holdfast-worker` under the OS temp directory
(`cmd/holdfast/worker.go`), which in the hardened container above is the RAM-backed `/tmp`. Give
it a directory on local disk with room for `worker_slots` outputs, and nothing else in it. Like
the server's `state` directory it must be writable by the `user:` the container runs as:

```bash
mkdir -p work && sudo chown 1000:1000 work
```

**`worker_name`** defaults to the host name, which in a container is the container id; name the
node so the server's log lines say which machine they mean. 1 to 64 characters from letters,
digits, `.`, `_` and `-`.

### An http-mode worker

`worker_mode: http` is for a node that cannot mount the library. The server streams each source
to it on a live lease (`GET /api/node/v1/leases/{id}/source`), so the worker has **no library
mount and no path map**.

```yaml
# worker.yaml (node-b)
worker_server: https://holdfast.example.internal:8080
worker_name: node-b
worker_mode: http
node_token: file:/run/secrets/holdfast_node_token
worker_work_dir: /work             # holds the downloaded source AND the output
worker_tls_ca: /config/ca.pem
```

```yaml
# compose (node-b) - the mapped worker's service, without the library
services:
  holdfast-worker:
    image: ghcr.io/nschatz/holdfast:<the tag and digest the server runs>
    command: ["worker", "--config", "/config/worker.yaml"]
    restart: unless-stopped
    user: "1000:1000"              # owns ./work; there is no media for it to own
    environment:
      - TZ=Etc/UTC
    volumes:
      - ./worker.yaml:/config/worker.yaml:ro
      - ./ca.pem:/config/ca.pem:ro
      - ./work:/work                 # no /srv/media line: this node has no mount
    secrets:
      - holdfast_node_token
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL

secrets:
  holdfast_node_token:
    file: ./secrets/holdfast_node_token
```

**Size the work volume for a source plus its output, per slot.** The source is downloaded into
`worker_work_dir` before the encode and the output is written beside it, so the directory needs
room for the largest source in the library and for its output, which is never as large as the
source - twice the largest source, times `worker_slots`, is the bound that cannot run out.

**What it costs.** Every job moves a whole source across the network to the node and an output
back, where a mapped node moves only the output. The digests are the same in both modes: the
worker reports the sha-256 of the source bytes it read and sends the output with its
`Content-Length` and a sha-256 `Content-Digest`, and the server checks both against its own
figures before a gate runs.

<a id="worker-transport"></a>

### The transport

**A worker talks to `https://` or to loopback.** `worker_server` naming a plain `http://` host
that is not loopback **refuses to start**. `worker_insecure_http: true` overrides that, and says
so loudly in the log at every start. Before you set it, this is what plain HTTP means here:

- **The `node_token` crosses the network in cleartext on every request.** Every poll, every
  heartbeat, every upload carries it in a header anyone on the path can read.
- **Whoever captures it can lease jobs and upload outputs.** Those outputs still face every gate,
  so a captured token cannot put a bad file into your library - but each one costs the server a
  source hash, a full decode and a VMAF run, and holds a free-space reservation while it does. It
  is a way to burn the server's CPU and stall its queue.
- **In http mode it can also read the library's media.** A leased job's source is served to the
  holder of the token. On plain HTTP that is your library, to anyone who was listening.
- **Digests do not help against that attacker.** A sha-256 over a body proves the body arrived as
  it was sent; someone on the path replaces the digest along with the bytes. Digests here are
  transport integrity against accident, and the gates are the fidelity proof. Neither is
  confidentiality or authentication.

So: TLS on the server, or a proxy you trust terminating it, and `worker_insecure_http: true`
only as a deliberate choice for a network you have decided to trust - a choice the log restates
every time the worker starts. The token cannot pause, scan, search, exclude or queue anything,
and no node endpoint restores or requeues; what it can do is listed above, in full.

**`worker_tls_ca`** is a plain path to a PEM bundle the worker trusts **in addition to** the
system roots, for a server certificate from a private authority or a self-signed one. It is a
certificate, not a secret, and it is not a reference.

**There is no mTLS and no per-node login.** Every node holds the one `node_token`; a node is not
identified by a certificate, and a `worker_name` is a label, not an identity. Rotating the token
means changing the file on the server and on every worker and restarting each.

### Operating it

**The same version on both sides.** A worker whose build version is not the server's is answered
**409** naming both versions, and stops; a restart policy starts it again to the same answer
until the tags match. Pin every worker to the tag and digest the server runs, and upgrade the
server and the workers together.

**Stopping a worker.** On SIGTERM (`docker stop`, `docker compose down`) a worker stops its
encodes, fails their leases `worker_stopping`, removes its local output and exits 0. Nothing is
recorded against those files: the server encodes each one itself in the same attempt.

**A killed worker.** One that dies without the chance to say so - `SIGKILL`, a power cut, a
network that went away - stops heartbeating. Its lease expires after `node_lease_ttl_sec`
(60 s, **ASSUMED**), the server removes the working file that lease recorded and nothing else,
and the job is offered again at the next epoch, counted by `max_failures` like any failed encode.
On the node, the output it was writing stays in `worker_work_dir`; the next start removes the
files there that carry its own output naming and nothing else
([docs/design/nodes.md](design/nodes.md#worker)), which is the reason to give that directory to
the worker alone.

**A worker never writes into the library.** Not in mapped mode, where its mount is read-only, and
not in http mode, where it has none. The only writer beside your sources is the server.

**Size the server for the proof.** Nodes move the encode off the server and not the gates: each
node output costs the server a sha-256 of its own source, a full decode-integrity pass and a VMAF
run. `node_gate_slots` (1, **ASSUMED**) is how many node jobs the server does that work for at
once, **beside** its `workers` local jobs, so the memory and CPU arithmetic of
[Workers, `cpus` and `max_load`](#workers-cpus-and-max-load) is for `workers` plus
`node_gate_slots` jobs, not `workers` alone. While every slot is held and as many jobs again are
waiting for one, workers asking for work are answered that there is none, so adding nodes past
what the server can gate buys nothing. A job the server will not lease - see below - is also
encoded by the server inside one of those slots.

**Nodes are given work only while a pass runs**: the initial scan, an interval scan
(`scan_interval_sec`) or a rescan. A file submitted through `POST /api/scan` is encoded by the
server.

**Probing a server that has TLS on.** The image has no `HEALTHCHECK`
([The image](#the-image)), and the external check that section recommends has to change with the
listener: once `server_tls_cert` and `server_tls_key` are set, a probe must speak `https://` to
the name the certificate was issued for and trust the authority that issued it, and a probe left
on `http://` fails however healthy the server is. With `server_read_token` set, as it is in this
deployment, a probe of `/api/summary` also needs that token as a bearer credential; `/metrics`
needs none. A worker has no port to probe: watch its log and its exit status.

### What is not built

- **Hardware encoders on a node.** A worker offers only the software encoders its own start-up
  probe proved. Only a plan that is self-contained on another host is leased - software encoder,
  software decode, no loudness-normalised track, no dynamic-HDR carriage, no attached picture -
  and every other job, every hardware encode among them, is encoded by the server
  ([docs/design/nodes.md](design/nodes.md#leasable)). GPU passthrough on a worker container buys
  nothing.
- **Resumable transfers.** An upload that fails is sent again from its first byte, inside the
  same lease.
- **mTLS**, and any per-node credential or login.
- **A maximum lease lifetime.** A worker that keeps heartbeating keeps its job, as a hung local
  encode keeps its worker ([docs/design/nodes.md](design/nodes.md#gate-slots)).

## GPU passthrough

Only needed if `config.yaml` sets a hardware `encoder:`. Hardware encoders are a
**backlog-drain** tool — meaningfully worse quality-per-bit than `encoder: cpu` (libx265),
which stays the archival default. The output is held to the **identical** no-loss gate either
way, so a bad hardware encode is rejected rather than shipped.

**NVIDIA (`nvenc`, `av1_nvenc`, `h264_nvenc`).** Needs the [NVIDIA Container
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
`dlopen` them (a fully-static ffmpeg could not). Which libraries it injects depends on how the
host's Docker reaches the toolkit:

- On the **legacy runtime-hook path** the libraries are chosen by `NVIDIA_DRIVER_CAPABILITIES`,
  and when it is unset the toolkit uses `utility,compute`, which leaves out `video`, the
  capability "required for using the Video Codec SDK", that is `libnvidia-encode`. The image
  therefore sets `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`. The value **replaces** the
  default rather than adding to it, so if you set it yourself (`environment:` in compose, `-e` on
  `docker run`), keep `video` in it. Docker sets the variable itself only when a device request
  names an NVIDIA capability; `capabilities: [gpu]` names none, so the image's value is the one
  the hook reads.
- On a **CDI host** (Docker 29.2 and later with NVIDIA Container Toolkit 1.18 and later) Docker
  injects the devices through the CDI specification the toolkit generates, and that spec carries
  the driver's libraries with no capability filtering, so `libnvidia-encode` is there either way.

The image does **not** set `NVIDIA_VISIBLE_DEVICES`: Docker writes it from the device request
above (or `--gpus`), and an image default of `all` would hand every GPU to any container started
under the NVIDIA runtime without asking for one. Sources, read 2026-09-30: [the toolkit's
Specialized Configurations for Docker](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html)
(the capability list, `video`, and the `utility,compute` default) and moby's
[`daemon/devices_nvidia_linux.go`](https://github.com/moby/moby/blob/master/daemon/devices_nvidia_linux.go)
(which variables Docker sets on the hook path, and the CDI driver it tries first).

**Intel (`vaapi`, and `qsv` on Tiger Lake and newer) and AMD (`vaapi`) - the runtime is in the
amd64 image.** The same holds for their H.264 and AV1 siblings (`h264_vaapi`, `av1_vaapi`,
`h264_qsv`, `av1_qsv`), which open the same render node. ffmpeg is built with `--enable-vaapi` and `--enable-libvpl`, and each of these needs
a vendor userspace library inside the container, which passing `/dev/dri` does not supply: that is
only the kernel device node. The amd64 image carries that userspace, copied out of pinned Debian 13
(trixie) packages:

| What | Package | For |
|---|---|---|
| `libva.so.2`, `libva-drm.so.2`, `libdrm.so.2` | `libva2`, `libva-drm2`, `libdrm2` | every VAAPI and QSV device (the bundled ffmpeg aborts without them) |
| `iHD_drv_video.so` | `intel-media-va-driver-non-free` (the full-feature build) | VAAPI on Intel |
| `libmfx-gen.so.1.2` | `libmfx-gen1.2` | QSV on Tiger Lake and newer |
| `radeonsi_drv_video.so` | `mesa-va-drivers`, `mesa-libgallium` | VAAPI on AMD |

and the shared libraries those need (LLVM, libz3, the X client libraries Mesa links, and others;
`NOTICE` lists every package with its version, licence and source). It adds about 255 MB to the
amd64 image. The VA drivers are in `/usr/lib/x86_64-linux-gnu/dri`, the directory Debian's libva
searches. Intel parts older than Tiger Lake have no QSV runtime in Debian 13 (`libmfx1` is not in
trixie), so on those use `encoder: vaapi`, which the iHD driver serves.

The container needs the render node and permission to open it. The node is owned by a group on the
host (usually `render`); give the container that group's **numeric** GID, because the image's
`/etc/group` has no `render` entry and a name would not resolve:

```bash
stat -c %g /dev/dri/renderD128     # on the host; or: getent group render
```

```yaml
devices:
  - /dev/dri:/dev/dri
group_add:
  - "993"      # the number the command above printed
```

**AMD AMF (`amf`) - not available in this image, and it cannot be.** AMF does not go through
VA-API: it needs AMD's own runtime, `libamfrt64` from the `amf-amdgpu-pro` package, whose licence
(the AMDGPU PRO EULA, <https://repo.radeon.com/amf/copyright>, read 2026-09-29) grants the right to
install and use it and no right to redistribute it, so a published image cannot carry it. On AMD
hardware in this image use `encoder: vaapi`, which Mesa's `radeonsi` driver serves and which is
AMD's own advice for Linux ("AMF users are advised to transition to VA-API / Mesa Multimedia",
[AMD's Radeon Software for Linux 25.10.1 release notes](https://www.amd.com/en/resources/support-articles/release-notes/RN-AMDGPU-UNIFIED-LINUX-25-10-1.html),
read 2026-09-29). `amf` keeps working in a host install that has AMD's runtime. `h264_amf` and
`av1_amf` are refused and kept on exactly the same terms, naming `h264_vaapi` and `av1_vaapi`.

**arm64** carries no hardware runtime: its pinned ffmpeg is built without VAAPI and without libvpl,
and Debian builds the QSV runtime for amd64 only.

CI has no GPU, so what the image smoke proves is the part that needs none: every driver and
library above resolves all of its dependencies inside the image, and a VAAPI device init against a
missing render node ends in ffmpeg's own device error, not an abort. An encode on real Intel or AMD
hardware is not proven by CI.

**What holdfast checks at start.** It lists the render nodes under `/dev/dri`, reads each one's
vendor, and gives VAAPI the first Intel or AMD node it can open and QSV the first Intel one; each
node and the assignment are logged once (`hardware: render node`, `hardware: render nodes
assigned`). A node it cannot open is logged with the reason: permission denied names the node's
GID and the `group_add` line above. Then every encoder the configuration can reach is probed: two
tiny clips, one at 8 bits and one at 10, are encoded through the command line a job would run -
every VAAPI device opened with `connection_type=drm`, 10-bit uploaded as `p010` with Main 10 -
and each output must be real, of the right codec and of the depth asked for
(`hardware: encoder probed ... 8bit=... 10bit=... why=...`).
[`docs/design/hardware.md`](design/hardware.md#probe) has the reasoning.

**No silent fallback, unless you ask for one.** If a configured hardware encoder cannot encode on
this host, `holdfast` refuses to start and exits non-zero, naming the encoder, the library root,
the reason and the lever - as it always has, because the default `hw_fallback: skip` never
substitutes another encoder. Set `hw_fallback: software` (at the top level or on one library
root) to have such a root's jobs encoded by the software encoder of the same codec instead (`cpu`,
`svtav1` for the AV1 encoders, `x264` for the H.264 ones), at start and whenever a hardware encode fails; the row records the
encoder that ran. `encoder: auto` picks, per job, the first of `nvenc`, `qsv`, `vaapi`, `amf`
whose probe passed for that job's pixel format, and always writes HEVC; with no usable hardware
it refuses to start under `skip` and uses `cpu` under `software`. A job no probed encoder can
carry is skipped `hardware-unavailable` under `skip`, and offered again on every pass. See
[`docs/design/hardware.md`](design/hardware.md#fallback), including why `skip` is the default.

`hw_decode: hardware` (top level or per library root; off by default) also decodes each source on
the job's hardware encoder's vendor hardware, through the same passthrough as the encoder: the
NVIDIA runtime for CUDA, and `/dev/dri` with its `group_add` for VAAPI. Nothing else is needed in
the container. Every frame comes back to system memory before the filters, the encoder and the
gates read it; see [`docs/design/hardware.md`](design/hardware.md#decode).

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

`holdfast serve` offers its Corresponding Source on the root path (AGPL-3.0 section 13). An
image built from this repository unchanged offers this tree; an image you build from a
**modified** tree must offer yours, and one build argument does it:

```bash
docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast -t my/holdfast .
make image SOURCE_URL=https://git.example.org/me/holdfast    # the same thing through the Makefile
```

`SOURCE_URL` rides the same `-ldflags` invocation as `VERSION`/`COMMIT`/`DATE`, so the link and
the build identity the page shows always name the same tree. It must be an absolute `http://` or
`https://` URL: the container **exits non-zero at startup** on anything else, naming the value it
rejected, rather than serving an offer nobody can follow.

## Known limitations

- **Hardware encoding in this image is amd64 only, and never `amf`.** The amd64 image carries the
  VAAPI and QSV runtime (Intel iHD, Mesa radeonsi, libmfx-gen) and NVIDIA's comes from the host's
  toolkit; `amf` needs AMD's runtime, which its licence does not let an image carry, so AMD encodes
  here through `vaapi`. arm64 has no hardware runtime. CI proves the libraries resolve, never an
  encode on real hardware - see "GPU passthrough" above. `cpu` (libx265, the archival default) and
  `svtav1` need no device at all.
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
