# Client reports

`scripts/client-report.sh` is how a result from a real Plex, Sonarr or Radarr reaches this
repository. The gate and CI never contact one: the clients in `internal/mediaclient` are proven
against fakes ([`docs/design/media-clients.md`](design/media-clients.md#media-clients)). A report
is the evidence from a real service, run by the owner against their own installation and
committed under `testdata/client-reports/`. It is the same pattern as
[`docs/hardware-reports.md`](hardware-reports.md).

## What it does

1. Reads the service's address, its credential and its path map from a holdfast configuration,
   exactly as holdfast reads them: `plex_url`, `plex_token` and `plex_path_map`, or the `sonarr_`
   and `radarr_` keys ([`docs/post-swap-hook.md`](post-swap-hook.md)). The credential is a
   `file:` or `cmd:` reference resolved inside the process that sends the requests
   ([`docs/secrets.md`](secrets.md)); it is never an argument and never in the environment. Only
   the checked service's credential is resolved.
2. Sends the requests below through the clients holdfast ships - the same address handling,
   headers, timeout (10 seconds a request), refusal to follow a redirect and failure classes - so
   a report proves the shipped path.
3. Writes `testdata/client-reports/<service>-<date>.json` (UTC date), or `--out`. An existing
   report is never overwritten. The date in the name and the date in the report are one reading
   of the clock: the script hands the tool the date it named the file with (`--date`).

A request that fails is recorded, not fatal: the report is still written, with the failure's
class (`timeout`, `unreachable`, `unauthorized`, `http-status`, `unparseable-response`) and its
HTTP status. No report is written when the configuration is refused (as `holdfast validate`
refuses it), the service is not configured, the credential cannot be resolved, the invocation is
wrong, or the final checks below find something a report may not carry.

## What a report records

Every report: `schema`, `service`, `date`, the holdfast `version` and `commit` that ran it, the
address's scheme and whether it has a base path, how many path-map entries and library roots the
configuration has, and `credential_accepted` (false when a request that needs the credential was
answered 401 or 403, true when one succeeded, null when the service was never reached). Each
request is recorded as its method and path, `ok`, `status` and `failure_class`.

**Plex** (the token must be the server owner's: the two requests that need it declare the
`admin` scope, and a 401 or 403 is recorded as `unauthorized`):

| request | what is recorded |
|---|---|
| `GET /identity` | the server `version`. It needs no token, so it says nothing about one. |
| `GET /library/sections/all` | the number of sections; for each its `type` (`movie`, `show`, `artist`, `photo` or `other`), its number of locations, whether every `Location` carries a `path`, and whether its `key` is a number the refresh request can name; how many configured library roots, through `plex_path_map`, land inside a section location, and how many contain one. |
| `GET /status/sessions` | the number of sessions; how many carry `Media`, how many a `Part`, how many carry `file` on every part; the parts counted, and those with `file`. `every_session_carries_part_file` is null when nothing was playing. |

The sessions check settles what the play hold assumes
([`docs/post-swap-hook.md`](post-swap-hook.md#play-hold)): that a real session's
`Metadata[].Media[].Part[].file` is present. It is only observed while something is playing, so
start a playback before running the Plex check.

**Sonarr and Radarr:**

| request | what is recorded |
|---|---|
| `GET /api/v3/system/status` | the `version`, and which application answered (`sonarr`, `radarr` or `other`). |
| `GET /api/v3/series` or `GET /api/v3/movie` | the number of items; how many have an integer `id`, a string `path`, both a positive id and an absolute path, and how many the shipped decoding accepts; how many configured library roots, through the path map, contain an item's path. |

The list endpoint answers the whole library, so on a large one this request can take the full
timeout; a `timeout` there is the same one a post-swap rescan would meet.

## The write checks

The checks above only read. A write is sent only when the owner types its flag:

- `--refresh-dir <dir>` (Plex): one partial refresh of that one directory,
  `POST /library/sections/{section}/refresh?path={directory}`.
- `--rescan-dir <dir>` (Sonarr, Radarr): the series or movie that owns that directory is found
  and sent one `RescanSeries` or `RescanMovie` carrying its id.

`<dir>` is a directory as holdfast sees it. Both are the requests a swap would cause, built by
the same code, which cannot build a whole-section refresh or a command with no id; with no owner
found, nothing is sent and the report says so. The report records the lookup, whether an owner
was found, whether the request was sent and accepted, its status, and for Sonarr and Radarr the
command's `name` and `status` as answered (`queued`, `started` and so on). Read
[the warning](post-swap-hook.md#x265-warning) first: an arr rescan can lead to a re-download.

## What it removes

Nothing a service answers is copied into a report. The report is one closed structure
(`scripts/clientreport/report.go`): numbers, booleans, and strings that are either this build's
own words (the request labels, the failure classes, the section types, `sonarr` and `radarr`,
the command names and statuses) or shaped like a version number. A string from a service that is
neither becomes `other` or `unrecognized`. So a report has no field for an address, host name,
port, URL, token or API key, machine identifier, library or section title, series or movie
title, file or directory path, user name or device name, and the directory given to a write flag
is not recorded either.

Two checks then run over the encoded report, and either one refusing means no file is written:

- every string in it is one of this build's words in the field it belongs to, or shaped like a
  version, a commit or a date, and its bytes are exactly what this build writes (so a report
  edited by hand does not pass);
- the resolved credential, the configured address, its host and port and its host name are not in
  its bytes. A host name of one label (`plex`, a compose service name) is compared with the
  strings that are not this build's own words, because every Plex report says `"service": "plex"`
  whatever the host is called. The cost of failing closed: a credential or a one-label host name
  that happens to spell something a report carries refuses every report.

`scripts/client-report.sh --verify <file>` runs the first check alone on an existing report, and
`make check` runs it over every report committed under `testdata/client-reports/`
(`TestClientReport_EveryCommittedReportPassesTheIdentityCheck`). `scripts/clientreport` proves the
rest: fakes that plant a canary in every title, path, address, identifier, user and device field
produce reports and output that carry none of them, nor the fake's host and port, nor the
credential, on success and on every failure (refused, 401, 403, 500, a redirect, an unparseable
body, a timeout).

What the command prints is the same closed facts, one line a request. A refusal to start prints
holdfast's own configuration or resolver message, which names a key and a reference and never a
resolved value.

## Running it

Image mode runs the check inside the holdfast image as your user, so no Go toolchain is needed;
the image carries the command as `/usr/local/bin/holdfast-client-report`. The configuration is
mounted read-only. A `file:` reference names a path inside the container, so mount what the
references name with `--docker-arg`, one `docker run` argument per use, and add
`--docker-arg=--network=<name>` when the configured address is a name only a Docker network
resolves. From a clone of `main`:

```bash
docker build --build-arg COMMIT="$(git rev-parse --short HEAD)" -t holdfast:live .
# Plex (start a playback first, so the sessions check has something to observe)
scripts/client-report.sh --service plex --config /srv/holdfast/config.yaml --image holdfast:live --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro
# Sonarr
scripts/client-report.sh --service sonarr --config /srv/holdfast/config.yaml --image holdfast:live --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro
# Radarr
scripts/client-report.sh --service radarr --config /srv/holdfast/config.yaml --image holdfast:live --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro
```

Each writes `testdata/client-reports/<service>-<date>.json` in the clone. Read it, then commit it
to `main`. A report is never edited by hand: a wrong one is replaced by a new run.

The optional write checks, one directory each:

```bash
scripts/client-report.sh --service plex --refresh-dir "/media/movies/Example Film (2001)" --out testdata/client-reports/plex-2026-10-02-refresh.json --config /srv/holdfast/config.yaml --image holdfast:live --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro
scripts/client-report.sh --service sonarr --rescan-dir "/media/tv/Example Show" --out testdata/client-reports/sonarr-2026-10-02-rescan.json --config /srv/holdfast/config.yaml --image holdfast:live --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro
```

A committed report is named `<service>-<date>` with an optional `-<suffix>`, and the gate holds
it to that.

Host mode needs Go: without `--image` the script runs `go run ./scripts/clientreport`, and
`--bin <path>` runs a binary built with `go build -o <path> ./scripts/clientreport`. The
`file:` references are then paths on the host.

Image mode has been exercised here only through a stand-in `docker` that records its arguments
(the gate has no Docker daemon); CI's image smoke test runs the bundled command's usage text.

## The webhook direction

This command sends requests to the services; it cannot make a service send one to holdfast. The
receiving direction is checked from the application itself: in Sonarr or Radarr,
`Settings > Connect`, open the Webhook connection that points at holdfast and press `Test`. The
application posts a body whose `eventType` is `Test`; holdfast's webhook intake answers it 200
and queues nothing, and the connection's test turns green. A red test with a 401 or 403 is the
webhook token; anything else is the address ([`docs/docker.md`](docker.md)).

## Sources

Read 2026-10-02:

- Plex: `GET /identity` ("Get PMS identity", `security: [{}]`, `MediaContainer.version` "The
  full version string of the PMS"), `GET /library/sections/all`, `GET /status/sessions` and
  `POST /library/sections/{sectionId}/refresh` with their `admin` scope: the OpenAPI document
  embedded in <https://developer.plex.tv/pms/>. The section `type` values `artist` and `photo`
  are ASSUMED (the reference's example shows `movie` and `show`); any other value is recorded as
  `other`. Whether a live session carries `Part[].file` is ASSUMED by the play hold, and is what
  the Plex report settles.
- Sonarr and Radarr: `SystemResource` (`appName`, `version`), `CommandResource` (`name`,
  `status`) and `CommandStatus` (`queued`, `started`, `completed`, `failed`, `aborted`,
  `cancelled`, `orphaned`):
  <https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/Sonarr.Api.V3/openapi.json> and
  <https://raw.githubusercontent.com/Radarr/Radarr/develop/src/Radarr.Api.V3/openapi.json>.
- The `eventType` `Test` body of the Webhook connection's test is ASSUMED from the applications'
  webhook payloads; no live application was asked.
