# Media-server clients

What holdfast says to Radarr, Sonarr and Plex, when, and why it is shaped the way it is. This
document is that argument's single home: `CLAUDE.md` names the rule and links here rather than
restating it. The keys, the requests and the deployment warning are in
[`docs/post-swap-hook.md`](../post-swap-hook.md); the code is `internal/mediaclient`, the path
maps are `internal/config/pathmap.go`, the engine's half is `internal/engine/playhold.go`, and
the wiring is `cmd/holdfast/mediaclients.go`.

## The rule

<a id="media-clients"></a>

**A media server is told about a swap only after it has committed, once, by directory; and a
file being played is held, which only ever delays.** Radarr, Sonarr and Plex are each asked one
time, off the engine's workers, to rescan the one movie, series or directory the swapped file
belongs to, and a request that fails is a `warn` record and nothing else. With Plex configured,
a file Plex is playing is not started and is not swapped until it stops; when Plex cannot be
asked, nothing is held. All of it is off until a target's address and credential are both set.

## After the commit, as an observer

The post-swap rescan is an `engine.Observer`. It receives a copy of a fact the engine has
already written - a job that ended `done` or `applied-despite-error` - and it can read nothing
back and change nothing. That placement is the whole safety argument:

- It is not on the swap path. The rename, the directory `fsync`, the undo retention and the
  ledger write have all happened before the first byte goes to a media server. Nothing a target
  answers, or fails to answer, can reach them.
- It holds no worker. `Observe` puts the path into a bounded queue and returns; one goroutine of
  the hook's own sends the requests. A target that accepts a connection and never answers costs
  that goroutine one timeout and costs the engine nothing.
- It cannot grow. The queue holds 64 swaps; one more is dropped with a `warn` record. A hook
  that queued without bound behind a dead target would turn a media server's outage into
  holdfast's memory.

Those two states are the ones in which the file at the path IS the replacement. A skip, a
failure, a parked job and a dry run's decision replaced nothing, so they say nothing.

## One attempt

Each target is asked once per swap. There is no retry, because nothing is lost when a request
fails: every one of these services rescans on its own schedule, and that schedule is the
fallback the failure record names. A retry loop would add state that has to survive a restart,
ordering between a stale retry and a newer swap, and a second way to hammer a service that is
already struggling - all to bring forward a refresh that happens anyway.

For the same reason a failure is `warn` and never `error`. An `error` record means a person
must act. Here nothing is degraded that anyone must act on: the swap is complete and correct,
and the only consequence is that a service shows old media info for a while longer.

At the end of `run`, and at `serve`'s shutdown, the requests still pending are attempted for at
most 30 seconds. The bound is what keeps a hung target from holding the process open; the exit
code is the one the engine's work earned, whatever the drain delivered.

## By directory, never by file name and never unscoped

The owner of a swapped file is found from its **directory**. An `applied-despite-error` event
carries the source path, and a container change moves the file name; neither moves the
directory, and the directory is what a movie's or a series' `path` and a Plex section's location
are compared against. The comparison is on whole path components, so `/movies/Film` never owns
`/movies/Film 2`, and the nearest owner - the longest matching path - wins. Two owners tied on
that path are an answer holdfast cannot choose between, so neither is used.

Every request names exactly one thing:

- `RescanMovie` and `RescanSeries` take a nullable id, and a command with none rescans every
  movie or series. The one function that builds a command body refuses an id that is not
  positive, and an item with no positive id is not an owner.
- A Plex refresh with no `path` rescans a whole section, and `/library/sections/all/refresh`
  rescans every section. The one function that builds a refresh takes a section number - never
  `all` - and a non-empty absolute directory, or it refuses.

A file that sits directly in a Plex section's location has that location as its directory, and
a refresh restricted to a location is a scan of all of it. That request is not sent either; an
`info` record says why, with a reason of its own.

A request holdfast cannot make specific is not sent. Triggering a whole-library rescan on every
swap would be the kind of load on a household's services that makes an operator turn the
feature off, and it is exactly the unscoped action the fail-safe rule exists to prevent.

<a id="path-maps"></a>

## Path maps

Each target has its own map from holdfast's view of the library to its own, because each is
usually a different container with different mounts. The longest `from` wins, on whole
components, and an unmapped path passes unchanged. `config.PathMap` carries both directions -
`Map` and `Reverse` - in one place, so the rescan (holdfast's path to the target's) and the play
hold (Plex's path back to holdfast's) cannot disagree about what a prefix covers. A map in which
two entries share a side is refused at start, since one direction would then have two answers.

<a id="play-hold"></a>

## The play hold only delays

Replacing a file under a player is at best a stalled stream. On a local filesystem `rename(2)`
leaves a reader's open descriptor on the old inode, so playback often continues; a player that
re-opens by path, or a network filesystem, gets the new file mid-stream or an error. So with
Plex configured holdfast asks what is being played and stays away from those files.

It asks at two points, and both are outside the gates:

1. **At the door of a job**, before the claim. A held file is left exactly as a held-back file
   is: no claim, no row, nothing written. It stays a candidate, and the next scan offers it
   again. This is the cheap case - no encode is spent on a file that cannot be swapped yet.
2. **In front of the swap**, after every gate has passed. This catches a playback that started
   during an encode that may have run for hours. The swap waits, polling, and then proceeds.

The second wait sits ahead of the collision re-check, the undo retention and the source
re-fingerprint, deliberately. Those checks promise that they ran immediately before the rename,
against the file as it then was. A wait placed after them would put minutes or hours between the
check and the rename it guards; placed before them, the wait changes nothing they promise. If
the file was rewritten while the swap waited, the re-fingerprint refuses it, as it always would.

The hold has no verdict. It cannot fail a job, skip a file or weaken a gate; it has no terminal
status and writes no row. A wait that is interrupted leaves the job exactly as an interrupted
encode leaves it: the working file discarded, the row active for the next start to recover, the
source untouched.

It has no upper bound either, and that is a cost: a session left paused pins one worker, with a
finished encode, until it ends. A timeout would have to choose between swapping under a player
and throwing away a finished encode, and neither is a decision to take on a guess, so a bound is
a proposal awaiting the owner. What is built is visibility: a waiting swap says so again every
10 minutes, naming the file.

## Fail-open

When Plex cannot be asked, no file is held, and one `warn` record per outage says so. This
mirrors the Tautulli pause in `internal/schedule`, which has always failed open.

The argument for it: the hold protects a viewer from a glitch, and the gates protect a source
from deletion. Failing closed would let the lesser of the two stop the tool entirely - an
unreachable, mis-tokened or upgraded-and-changed Plex would hold every file indefinitely, and
would do it silently from the library's point of view. Failing open costs at worst one
interrupted stream during an outage.

A failed question is reused for 60 seconds where an answer is reused for 2, so an outage in
which Plex accepts connections and never answers costs the workers one timeout a minute.

The argument against it is real: an operator who enabled the hold expects it to hold. That is
why the outage is a `warn`, why it is said again after every recovery, and why this choice is
recorded as **awaiting the owner's ratification** rather than as settled.

## Credentials

The three credentials are references, in `config.SecretBearingKeys`, resolved once at start to a
`secret.Value` and handed to one client each. Each travels in a request header - `X-Api-Key`,
`X-Plex-Token` - and never in a URL, so no request URL is credential-bearing. A failed request
is still never logged as its error text, because a Go `*url.Error` quotes the request URL and an
operator may have put something into an address that does not belong there: a record carries a
target name and a failure class from a closed vocabulary. The clients follow no redirect, so a
credential header cannot be carried to an address the operator did not configure.

<a id="webhook-intake"></a>

## Webhook intake

**An arr's webhook is authenticated by a credential that can only queue; both Sonarr Download
shapes and Radarr's are read; each file goes through the targeted scan; and a shape that is not
recognised queues nothing.** This is the opposite direction to everything above: here an arr
tells holdfast, and the code is `internal/server/webhook.go`.

**One credential, one purpose.** The intake has a key of its own, `webhook_token`, and three
properties follow from that choice:

- It authorises `/api/webhook/sonarr` and `/api/webhook/radarr` and nothing else. No other gate
  compares against it, so the secret an arr holds can queue a file that lies inside a configured
  library root and cannot pause, scan or withhold a path. On a control endpoint it is answered
  401, or 403 while no control token is configured; on a read endpoint 401 while
  `server_read_token` is set. While that key is unset the reads are open to every caller, as they
  are without this key: the webhook token is not what opens them.
- No other credential opens the intake. Accepting `server_auth_token` there would make the control
  token the convenient thing to paste into an arr, and the control token would then live in a
  second service's database. It is refused, so there is no reason to hand it over. For the
  same reason a `webhook_token` written as the same reference as either server token refuses to
  start, and `serve` refuses one that resolves to the same value as either by another reference
  (compared in constant time, naming the keys and no value).
- It arrives in a header only. An arr's Webhook connection can send HTTP Basic credentials or
  custom headers, so the token is accepted as the Basic password (any username) or as a bearer
  token. A URL form was not built: a query string or a path segment reaches access logs and
  proxies.

Without the key the intake answers 403, as every mutating endpoint does without its credential.

**The same pipeline.** The intake adds a caller to the targeted scan and nothing else. Each path
is judged by the engine's one eligibility decision and offered to the one submission queue, by the
same function `POST /api/scan` calls, so the root containment, the path hygiene, the per-request
limits and the claim are that endpoint's. It is not `requeue`: a file a terminal row answered
stays answered.

**Paths are mapped back, never guessed.** An arr names a file as its own container sees it.
`sonarr_path_map` and `radarr_path_map` already state the relation for the rescan clients, so the
intake reads the same maps in reverse; a path no entry matches is judged as it stands, and a path
that lands outside every root is refused by name.

**A path is never cleaned by spelling.** Rewriting a prefix is lexical; resolving `..` is not,
because the filesystem resolves it through whatever the segment before it really is. With a link
inside a root that points out of it, `/tv/link/../Show/x.mkv` names a file beside the link's
target, and a lexical clean would turn it into `/tv/Show/x.mkv`, a file inside the root:
`POST /api/scan` refuses that string and the intake would have queued it. So an absolute path
that is not already in its clean form is refused per file (`path-not-clean`) without being mapped,
cleaned or judged, and a clean one reaches the engine's judge exactly as `POST /api/scan` hands
it one. Property names, the nested `path` included, are matched exactly.

**A shape that is not recognised queues nothing.** Property names are matched exactly as the arr
serialises them. A `Download` is read for `episodeFile` or `episodeFiles` (Sonarr) or `movieFile`
(Radarr), a `Rename` for its renamed-file list, and nothing else in a payload names a file to
queue: in particular not `deletedFiles`, which an upgrade carries for the files it replaced. A
payload carrying the other arr's keys is a connection pointed at the wrong endpoint, and queues
nothing even where it also names a file under this endpoint's key.

**Not a failed delivery.** An arr records a failure for any delivery that throws, and stops
sending to a connection while it is in the resulting back-off. An event holdfast does not consume,
a file in a library it is not pointed at and a pause the operator chose are not failures, so each
is answered 200 with the reason in the body and one log record. A body that is not the arr's
JSON, a queue that could not take a file and a missing credential are failures and are answered
as such. A Test event from the wrong arr is answered 400, because a Test is a question and the
honest answer to it is that the connection is wrong.

**Rename.** The file is queued under its new path. Nothing is done about a job already queued
under the previous path: the submission queue has no removal, and adding one for this would be a
second way to take work out of the pipeline. Such a job ends on its own when its turn comes,
because the path no longer names a file.

**Sources** (read 2026-10-02), each under `src/NzbDrone.Core/Notifications/Webhook/` unless a
path is given:

- Sonarr at tag `v4.0.20.3014` (<https://github.com/Sonarr/Sonarr/tree/v4.0.20.3014>):
  `WebhookPayload.cs` (`eventType`), `WebhookEventType.cs` (the event names, and the converter
  that keeps their declared spelling), `WebhookImportPayload.cs` (`episodeFile`, `isUpgrade`,
  `deletedFiles`), `WebhookImportCompletePayload.cs` (`episodeFiles`), `WebhookEpisodeFile.cs`
  (`path`), `WebhookRenamePayload.cs` and `WebhookRenamedEpisodeFile.cs` (`renamedEpisodeFiles`,
  `previousPath`), `WebhookBase.cs` (both Download builders and the Test payload),
  `WebhookSettings.cs`, `WebhookMethod.cs` and `WebhookProxy.cs` (URL, POST or PUT, Username,
  Password, Headers).
- Radarr at tag `v6.4.4.10685` (<https://github.com/Radarr/Radarr/tree/v6.4.4.10685>): the same
  files, with `WebhookImportPayload.cs` (`movieFile`, `isUpgrade`, `deletedFiles`),
  `WebhookMovieFile.cs` (`path`), `WebhookRenamePayload.cs` and `WebhookRenamedMovieFile.cs`
  (`renamedMovieFiles`, `previousPath`).
- In both: `src/NzbDrone.Common/Serializer/Newtonsoft.Json/Json.cs` (camelCase property names,
  null properties omitted), `src/NzbDrone.Common/Http/Dispatchers/ManagedHttpDispatcher.cs`
  (Basic credentials are sent with the first request), and
  `src/NzbDrone.Core/Notifications/NotificationService.cs` with `NotificationFactory.cs` (a
  delivery that throws records a failure, and a connection in back-off is not sent to).

## What is not established

No goal of this program contacts a real Plex, Sonarr or Radarr, so everything above is proved
against fakes built from the documented shapes. Marked `ASSUMED` in `internal/mediaclient` and
left for the owner's live check: that a real Plex session carries `Media[].Part[].file`; that
the refresh path's `sectionId` is the section's `key`; the camelCase spelling of `seriesId` and
`movieId` in a command body; and how Plex's partial scan treats the `.holdfast-undo` folder.

The webhook intake is proved against fixtures written by hand from the payload classes cited
above, not against payloads captured from a running arr. `ASSUMED`, for the same live check: that
an arr answers a non-2xx Test by refusing to save the connection (the source shows the failure is
raised, and the user-interface behaviour was not read).
