# After a swap: Radarr, Sonarr and Plex

When holdfast replaces a file, Radarr, Sonarr and Plex keep describing the file that is gone -
the old codec, the old size, the old streams - until their own scheduled rescan. This document
is the deployment reference for the three optional clients that close that gap, and for the Plex
play hold that comes with the Plex client. The reasoning behind their shape is in
[`docs/design/media-clients.md`](design/media-clients.md#media-clients).

**Everything here is off by default.** With none of the nine keys below written, holdfast makes
no request to any of the three services, and decides, encodes and swaps exactly as it did before
they existed.

Read [the warning](#x265-warning) before you enable `radarr_url` or `sonarr_url`.

## What it does

| target | enabled by | after a swap | before a swap |
|---|---|---|---|
| Radarr | `radarr_url` + `radarr_api_key` | `RescanMovie` for the one movie that owns the file's directory | nothing |
| Sonarr | `sonarr_url` + `sonarr_api_key` | `RescanSeries` for the one series that owns the file's directory | nothing |
| Plex | `plex_url` + `plex_token` | a partial scan of the file's directory | a file being played is held ([the play hold](#play-hold)) |

A target is enabled when its address and its credential are both set. One without the other
refuses to start, naming the key.

```yaml
radarr_url: http://radarr:7878
radarr_api_key: file:/run/secrets/radarr-api-key
radarr_path_map:
  - {from: /media/movies, to: /movies}

sonarr_url: http://sonarr:8989
sonarr_api_key: file:/run/secrets/sonarr-api-key
sonarr_path_map:
  - {from: /media/tv, to: /tv}

plex_url: http://plex:32400
plex_token: file:/run/secrets/plex-token
plex_path_map:
  - {from: /media, to: /data/media}
```

The hook acts when a job ends `done` or `applied-despite-error` - the two states in which the
file at the path is the replacement - under both `holdfast run` and `holdfast serve`. It does
nothing for a skip, a failure, a parked job or a dry run, and nothing after `holdfast restore`
puts an original back (restore is a local command and stays network-free).

### The requests

- **Radarr**: `GET /api/v3/movie`, then `POST /api/v3/command` with
  `{"name": "RescanMovie", "movieId": N}`. **Sonarr**: `GET /api/v3/series`, then
  `POST /api/v3/command` with `{"name": "RescanSeries", "seriesId": N}`. The key travels in the
  `X-Api-Key` header, never in a URL. The owner is the movie or series whose `path` is the
  file's directory or its nearest ancestor, compared on whole path components: `/movies/Film`
  never owns `/movies/Film 2`. Neither application has a rescan-by-path command, and a rescan
  command with no id rescans the whole library, so holdfast cannot build one: an owner with no
  positive id is treated as no owner.
- **Plex**: `GET /library/sections/all`, then
  `POST /library/sections/{id}/refresh?path=<directory>` on the section whose location owns the
  directory (the longest location, on whole components). The token travels in the
  `X-Plex-Token` header, never in a URL. holdfast never asks for a whole-section or an
  all-sections refresh.
- **A flat Plex library is not refreshed.** When the file sits directly in a section's location
  (`/movies/Film.mkv` with the location `/movies`), its directory IS the location, and a scan
  restricted to it would be a scan of the whole location. holdfast sends nothing to Plex for
  that swap and says so in one `info` record; Plex's own scheduled scan picks the file up.

Sources, read 2026-10-02: the Plex Media Server API reference at <https://developer.plex.tv/pms/>
(API version 1.2.3: `POST /library/sections/{sectionId}/refresh` with `path`, "Restrict refresh
to the specified path"; `GET /library/sections/all`; `GET /status/sessions`; the `X-Plex-Token`
header scheme); the Sonarr and Radarr v3 API descriptions
(<https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/Sonarr.Api.V3/openapi.json>,
<https://raw.githubusercontent.com/Radarr/Radarr/develop/src/Radarr.Api.V3/openapi.json>) and
their `RescanSeriesCommand.cs` and `RescanMovieCommand.cs`, checked at the releases v4.0.20.3014
and v6.4.4.10685.

### What a failure does

Nothing, to the job. The hook observes a swap that has already been committed:

- each target gets **one attempt** per swap, with a 10 second timeout per request, and no retry;
- a failure - the connection refused, a non-2xx answer (`401` included), an answer that does not
  parse, no answer in time - is **one `warn` record** naming the target, the file, what was
  attempted and the failure class. It is never an `error`: nothing is degraded that a person
  must act on, and the service's own scheduled rescan remains the fallback;
- one target failing does not stop the others being asked;
- the job's status, the replacement, the undo window's retained original and the exit code are
  what they would have been with no target configured;
- a directory no movie, series or section owns sends nothing and is one `info` record.

The requests are sent off the engine's workers, from a queue of 64 swaps. If a target hangs for
long enough that the queue fills, the next swap is dropped from it with one `warn` record.

<a id="drain-bound"></a>

**The drain bound is 30 seconds.** When `run` finishes, or `serve` shuts down on SIGTERM, with
rescan requests still pending, holdfast keeps attempting them for at most 30 seconds and then
exits. If any were not delivered it says how many in one `warn` record. The exit code is not
changed by the drain.

## Path maps

holdfast and each service usually see the library under different paths, because each runs in
its own container. A path map is a list of `from`/`to` prefix pairs: `from` is a directory as
holdfast sees it, `to` is the same directory as that service sees it.

- Both sides are absolute paths; anything else refuses to start, naming the key and the entry.
- The entry whose `from` is the **longest** prefix of a path wins.
- A prefix matches on whole path components: `/media/tv` covers `/media/tv/Show` and never
  `/media/tv-archive`.
- A path no entry covers is passed **unchanged**, so a service that mounts the library at the
  same path holdfast does needs no map.
- Two entries with the same `from`, or the same `to`, refuse to start: one path would map two
  ways.
- A path map is written in the config file. It has no environment form, and
  `HOLDFAST_PLEX_PATH_MAP` (or either of the other two) refuses to start.
- Both sides use `/`. A service that reports Windows paths is not supported by a path map.

The Plex play hold uses `plex_path_map` in the other direction, to turn the file Plex says it is
playing back into the path holdfast knows it by.

The webhook intake uses `sonarr_path_map` and `radarr_path_map` in that other direction too, to
turn the path an arr announces an import by into the path holdfast knows the file by. It needs
neither `sonarr_url` nor `radarr_url`: the maps are read whether or not the rescan target is on
([docs/docker.md](docker.md#telling-holdfast-about-one-file-sonarr--radarr)).

<a id="x265-warning"></a>

## Warning: an arr rescan can undo holdfast's work

Read this before enabling the Radarr or Sonarr target.

The TRaSH guides ship a custom format named **"x265 (HD)"** for both Radarr and Sonarr. Its
default score is **-10000**. It matches a release title against the pattern
`[xh][ ._-]?265|\bHEVC(\b|\d)`, on files that are not 2160p. Many setups sync it with Recyclarr.
Its purpose is to keep an arr from grabbing HD HEVC releases.

A file holdfast has replaced IS an HD HEVC file (or AV1, under another encoder), and holdfast
keeps the file's name. So the custom format normally does not fire on it: the name still says
whatever the original release said.

That changes if the arr's **naming format carries the `{MediaInfo VideoCodec}` token** and the
arr renames files. After a rescan the arr reads the new codec, the rename writes `x265` or
`HEVC` into the file name, the file now matches "x265 (HD)" and scores -10000, and the arr may
decide a better-scoring release exists and **download the title again**. That discards the
transcode. The source holdfast replaced is already deleted, so the drive fills back up and the
work is undone, on every file, as each is rescanned.

The rescan target does not create this risk - the arr's own scheduled rescan reaches the same
file eventually - but it makes it happen within minutes of each swap rather than over days.

### What to check before enabling an arr target

1. **The naming format.** In Radarr and Sonarr, Settings > Media Management: does the standard
   file format contain `{MediaInfo VideoCodec}` (or any token that writes the codec into the
   name)? If it does, a rescan followed by a rename will name your swapped files as HEVC.
2. **The custom formats.** Does the quality profile in use score "x265 (HD)", or any other
   format that matches `x265`, `h265` or `HEVC` in a title, below zero? With Recyclarr, check
   what it syncs.
3. **Upgrades.** Is "Upgrades Allowed" on for that profile, with a cutoff the file has not
   reached? An arr only re-downloads when it believes an upgrade is available.
4. **One title first.** Enable the target, swap one file, and watch that title in the arr's
   Activity and History for a day before letting it run across a library.

If 1 and 2 are both true, do not enable the arr target until one of them is changed.

**holdfast changes no Radarr, Sonarr or Recyclarr setting, and never will.** It sends the two
requests above and nothing else. Whether to change a naming format or a custom format's score
is a decision about your own setup.

Source, read 2026-10-02:
<https://raw.githubusercontent.com/TRaSH-Guides/Guides/master/docs/json/radarr/cf/x265-hd.json>
and the same file under `sonarr/cf/` - `"name": "x265 (HD)"`, `"trash_scores": {"default":
-10000, ...}`, a required `ReleaseTitleSpecification` with the value above and a required,
negated `ResolutionSpecification` of 2160.

<a id="play-hold"></a>

## The Plex play hold

With the Plex target configured, holdfast does not replace a file that Plex is playing. It
asks `GET /status/sessions`, maps each file a session names back to its own view with
`plex_path_map`, and checks twice:

- **Before a job starts.** A file being played is not started: it is not encoded, and no ledger
  row is written for it, so it stays a candidate and the next scan (or the next `run`) offers it
  again. One `info` record names the file and the reason.
- **Before the swap.** A playback that begins while the encode runs is caught here. Once every
  gate has passed, the swap waits, asking again every 15 seconds, until the file is no longer
  being played, and then proceeds exactly as it would have. An interrupt during the wait is the
  interrupt of any job in flight: the working file is discarded and the source is untouched.

The hold only ever **delays**. It changes no gate, no verdict and nothing about the swap. It
has no upper bound: **a session left paused pins a worker** - the one that finished encoding
that file - until the session ends. While a swap is waiting, holdfast repeats a "still waiting
for playback to end" `info` record every 10 minutes, naming the file, so a pinned worker is
visible in the log. A bound on the wait (after which the swap would proceed, or the encode be
discarded) is a proposal **awaiting the owner**; none is built.

One answer from Plex is reused for 2 seconds, so a burst of jobs costs one request. A failed
question is reused for 60 seconds: a Plex that accepts a connection and never answers costs one
10 second request timeout a minute, not one per job, and the hold comes back up to a minute
after Plex does.

**It fails open.** When Plex cannot be asked - the connection refused, a non-2xx answer, an
answer that does not parse, no answer in time - no file is held, and one `warn` record says so.
Nothing more is said until Plex answers again. This mirrors the Tautulli pause, which also fails
open, and it is **awaiting the owner's ratification**: the alternative, holding every file while
Plex is unreachable, would stop all work for as long as Plex is down.

With `plex_path_map` written, a file Plex names that no entry's `to` covers is ignored: it is
outside what the map says the two share. With no map, Plex's path is taken as holdfast's.

What it does not see: a session whose file Plex does not name (the hold then knows no path), a
file played through a path that only matches after resolving a symbolic link, and any player
that is not Plex. The Tautulli pause (`tautulli_url`) is separate and unchanged: it pauses the
feed of new files while anything at all is streaming, and it runs under `serve` only.

## The token and the keys

- **`plex_token` must be an admin token** - the server owner's. Both the partial scan and the
  sessions list are admin-scoped in the Plex API; a token without that scope is answered `401`
  or `403`, which holdfast reports as `unauthorized`.
- `radarr_api_key` and `sonarr_api_key` are each application's API key (Settings > General).
- All three are **references**, never values: `file:<path>` or `cmd:<argv>`. A literal in the
  config file or in `HOLDFAST_PLEX_TOKEN`, `HOLDFAST_SONARR_API_KEY` or
  `HOLDFAST_RADARR_API_KEY` refuses to start. See [`docs/secrets.md`](secrets.md).
- Each address is a plain `http://` or `https://` URL: a scheme, a host and an optional base
  path. Userinfo, a query or a fragment in it - or a bare `?` or `#` - refuses to start.
- No log record carries a credential or a request URL. A failed request is reported as a target
  name and a failure class.
- A redirect is not followed: it would carry the credential header to an address you did not
  configure. Point each address at the service itself (an `http` address that redirects to
  `https` is reported as `http-status 301`).

## Risks

- **The undo folder.** With `undo_window_hours` above 0, a replaced original is kept in
  `<dir>/.holdfast-undo/` until the window closes. Radarr's and Sonarr's disk scans exclude
  sub-folders whose name starts with a dot (`ExcludedSubFoldersRegex` in each
  `DiskScanService.cs` includes `\.[^\\/]+`), so an arr rescan does not import what is in it.
  How Plex treats a dot-prefixed folder during a partial scan is **not established** by any
  source read here (`ASSUMED` ignored, unverified): if retained originals appear in Plex as
  duplicates, that is where to look. The folder exists with or without these clients; they only
  make the scan sooner.
- **An arr re-download** - [the warning above](#x265-warning).
- **The whole-library list.** Finding the owner reads `GET /api/v3/movie` or
  `GET /api/v3/series`, which answers the whole library, once per swap. Nothing is cached.
- **What the services then do** is theirs. The tests here prove the requests holdfast sends
  against fakes; only a live check against your own services shows their reaction.
  `scripts/client-report.sh` is that check: it sends these requests to your own Plex, Sonarr or
  Radarr and writes a redacted report ([`docs/client-reports.md`](client-reports.md)).
