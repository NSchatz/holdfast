# Playback evidence

What holdfast learns about a swapped file from the people who later play it, what it does with
that, and why it is shaped the way it is. This document is that argument's single home: the
[design index](README.md) names the rule and links here rather than restating it. The keys are
described in [`docs/profiles.md`](../profiles.md#playback-evidence); the code is
`internal/playback`, the ledger's half is `internal/store/playback.go`, and the daemon's wiring is
`cmd/holdfast/playback.go`.

## The rule

<a id="playback-evidence"></a>

**A swapped file is tied to what later happened when people played it; several sessions ending at
the same point well before the end are reported as a suspected stall, and nothing is acted on.**
Under `holdfast serve`, with `playback_source: tautulli`, the daemon reads Tautulli's session
history every `playback_poll_minutes`, attributes each finished session to the swapped file whose
bytes it played, and derives one verdict per file. A suspected stall is said in the log, on
`GET /api/playback` and in `/metrics`, with the `holdfast restore` command that would walk the swap
back while the original is still retained. Nothing restores, re-encodes, releases or holds a file
on the strength of it. With the key unset (the default) none of this runs and Tautulli is asked
for nothing it was not asked before.

## Why it exists

Every gate in front of a swap measures whether the replacement is **faithful** to its source:
structure, a full linear decode, the perceptual score. None of them measures whether the players
in a household can **play** it. Issue #200 is the case: a replacement passed every gate, stalled
most Plex clients about 34 minutes into a film, and the first failed playback came three days
after the swap, a day after the 24-hour undo window had released the original. The owner learned
of it four weeks later, and nothing in holdfast had connected the file to the failures.

The gates can be made stronger before the swap, and that is separate work. This is the other half:
after the swap, the evidence that matters is what real clients do with the file, and it arrives
on its own schedule. Its value is time. A stall reported while the undo window still holds the
original is one `holdfast restore` away from being undone; one reported after it is still a file
the operator now knows to replace, rather than one found weeks later by a viewer.

## Where the evidence comes from

<a id="source"></a>

**Tautulli, and only Tautulli.** Its history is a record of every finished session with where in
the file it ended, which is the one fact a stall is made of. The client is the Tautulli client the
streaming pause already holds (`tautulli_url`, `tautulli_api_key`), so the key is the one resolved
at start, it travels in the query string as before, and every failure leaves through the same
sanitizer, naming the command and the key's reference and never the request URL
([docs/secrets.md](../secrets.md)).

Two commands are sent, both reads:

- `get_history` with `grouping=0` (every session its own row, not merged into one per viewing),
  `include_activity=0` (finished sessions only), `order_column=started`, `order_dir=asc`, `after`
  (a date: the day before the earliest start this poll needs, since Tautulli reads the date in its
  own time zone; rows before that start are dropped here), `start` and `length` (100 rows a page).
  Each row is read for `row_id` (or `id`), `started`, `stopped`, `view_offset`, `percent_complete`,
  `play_duration`, `machine_id` (the client), `rating_key`, `platform`, `player`, the stream
  decision, `media_type` and `live`. A live-TV row, a music or photo row and a row with no id are
  skipped.
- `get_metadata` with `rating_key`, for the item's `media_info[].parts[].file` and its `duration`.
  A history row's own `duration` is the time PLAYED, and is never read as the file's length. The
  answer is kept across polls and reused for every row that started BEFORE it was asked: the file it
  names is then at least as new as the one that session played. A row that started later - a new
  session, which may have played a file swapped since - asks again, so a poll that re-reads two days
  of history asks Tautulli again only for what is new in it.

**One refused item fails nothing.** An item Tautulli answers with a status other than 200 or a
result other than `success`, or in a shape this does not read (an answer cut off at the client's
1 MiB bound included), names no file for its rows and the read goes on; the next row of that item
asks again. Only a request Tautulli never answered - refused, timed out, unreachable - fails the
read, so an outage is not mistaken for a history with no swapped file in it.

Sources, read 2026-11-18: the Tautulli API reference,
<https://github.com/Tautulli/Tautulli/wiki/Tautulli-API-Reference> (`get_history`, `get_metadata`
and their parameters and response fields).

**Why not Plex itself.** Plex's history (`GET /status/sessions/history/all`) records a view only
when it reaches Plex's watched mark, with no offset for a session that stopped early: it could
confirm a play-through and could never see a stall. Plex's live sessions (`GET /status/sessions`)
do carry an offset, but reading a stall from them means sampling every session until it
disappears, which is rebuilding Tautulli with a worse resolution and losing every session that
ends between two samples. `playback_source: plex` is refused at start with that explanation.

## Attribution

<a id="attribution"></a>

A session is evidence about one swapped file, or about none:

- **Its file, in holdfast's terms.** Tautulli names a file as Plex sees it, so the name is mapped
  back through `plex_path_map` exactly as the play hold maps a session's file
  ([path maps](media-clients.md#path-maps)); with a map written, a file no entry's `to` covers is
  outside what holdfast shares with Plex and is ignored.
- **The swap whose bytes it played.** The session belongs to the newest swap of that path at or
  before the session started, while that swap was still the file there. A session before every swap
  of its path played the source, and one after the swap was superseded played other bytes; neither
  is anybody's evidence. The session is recorded against the swap's path AND fingerprint, so it is
  evidence about those bytes.
- **Exactly one file.** An item whose media name more than one file (two versions of a film, or a
  film in parts) is attributed to nothing, because the history does not say which of them the
  session played, and guessing would put a stall on a file that never stalled.

<a id="swap-identity"></a>

**A swap is recorded in its own table, not read off the done row.** A done row is not durable
enough to be the identity: a changed encoder setting (`crf`, `preset`, `encoder`, the target
codec, `x265_open_gop`, `keep_bit_depth`) or a `requeue` re-opens it, and the guard then records it
`skipped` (already at the target codec) under the same path and fingerprint. Evidence keyed to the
`done` status would vanish at that moment, suspects included. So each poll, and `serve` once before
its first scan, records every done row and every live retention's swapped file as a swap - path,
fingerprint and the second it committed - in `playback_swaps`, and a swap stays **current** while:

- the newest jobs row at its path, of ANY status, still carries its fingerprint (or there is none);
- and no restore of it is recorded.

When either stops holding - an operator restored the original, or another file arrived at the path
and the scan recorded it - the swap is superseded at the earliest second the ledger shows for that,
and its evidence is no longer reported. A swap made while playback evidence was off, whose done row
a setting change re-opened before the first poll, is recorded only if the undo window still holds
its original.

## The verdicts

<a id="verdicts"></a>

Each session says one of three things on its own. It **played the file through** when it ended at
or past `playback_played_fraction` of the file (0.9 by default) or within **3 minutes** of the end,
so a long film's credits do not hold it below the mark. It **stopped early** when it ended before
**85%** of the file and started within the last **60 days**. Otherwise it **says nothing**: it has
no offset or the item no positive length, it stopped in the last 15% without reaching the mark
(viewers leave at the credits, and an animated feature's credits can start before 90%), or it is
older than the window (a stop months ago is not evidence about how the file plays now, and
sessions that never expire collide at one point by chance).

Stopped-early sessions whose ends lie within **90 seconds** of one another ended at one point. Where
an end was computed from a whole percent (no `view_offset`), the rounding can have moved it by half
a percent of the file, so the tolerance widens by one percent of the file: on a 2h40m film, 186
seconds. A point is a **stall** rather than people choosing to stop there when it holds at least
**2** sessions and either:

- they played on at least **2 distinct clients** (Tautulli's `machine_id`, or the platform and
  player where it gives none); or
- at least one of them was a **retry that got nowhere**: it played for no more than **5 minutes**,
  so it began within 5 minutes of the file before where it ended - a resume at the point that froze.

| verdict | when |
| --- | --- |
| `suspect-stall` | a stall, as above |
| `played-through` | no stall, and at least one session played the file through |
| `unplayed` | no stall and no play-through, and either a session exists or a poll has read the history to its end since the swap |
| `unknown` | no session, and no poll has read the history to its end since the swap |

A suspected stall **wins over a play-through**. In the issue the film played to the end twice on
an Apple TV and stalled on the browser and desktop clients: one client that plays a file is not
evidence that the others can. The issue's pattern is exactly what the rule asks for: Chrome stopped
near 33:19 and resumed there to no avail, the desktop app resumed at 33:59 and froze at 34:02, and
Firefox stopped near 34:00. The cluster reported is the largest set that qualifies (the earliest on
a tie), placed at its middle session's end.

It is a **suspicion**, and the name says so. Two people can stop a film at the same scene for
reasons of their own. That is why nothing acts on it and why the report carries the sessions it
rests on - their clients, how long each played, their stream decisions and where each ended - so
the operator can judge it. The fixed figures (3 minutes, 85%, 90 seconds, 2 sessions, 5 minutes,
60 days) are not keys: they are stated here and asserted by the suite, and a deployment that needs
others is a conversation, not a setting.

**No evidence is `unknown`, never `played-through`.** A poll that fails changes no verdict, and a
file swapped since the last poll that read the history to its end stays `unknown` until one does.

## Polling

<a id="polling"></a>

The poll runs in `serve` only, on its own goroutine, every `playback_poll_minutes` (15 by default),
and once at start. It holds no worker, takes no lease, asks nothing of the scheduler and touches no
library file: it reads the ledger through the reporting door and writes its own three tables
(`playback_swaps`, `playback_sessions` and `playback_polls`, schema v26), which nothing in the
encode pipeline reads. A Tautulli that is slow costs the poll its request timeout (5 seconds per
request) and nothing else, and the metadata kept across polls keeps what it asks of the Tautulli
the streaming pause shares small.

- **Where it reads from.** The first poll reads from the earliest swap in the ledger. A poll that
  read the history to its end begins the next one **48 hours** before itself: a session joins the
  history when it stops, carrying the time it started, so one still playing when a poll read is
  found by the next only if that one reads back to its start. A re-read session is the same row
  (keyed by Tautulli's row id and the file) and is recorded once.
- **How much it reads.** At most 50 pages of 100 rows a poll. A backlog larger than that - a first
  poll over a long history - is read forward over several polls, and the history is not counted as
  covered until one reaches its end. Each continues from the newest row the last one went through,
  SKIPPED rows included, so a run of music or of rows before the cursor cannot hold it in place;
  and because Tautulli's `after` is a whole date, a read under the same date as an incomplete one
  continues from the row offset that one reached rather than from the first row of the day. If
  three polls in a row stop at the bound without moving, one `warn` record says so.
- **When nothing is swapped**, Tautulli is not asked.
- **It fails open.** A request that fails, an answer that is not the documented shape and a result
  other than `success` end the poll with a failure class (`request-failed`, `unparseable-response`,
  `error-result`, or `ledger-failed` when the ledger could not take what was read). The attempt and
  its class are recorded, the cursor and the coverage stay where the last good poll left them, and
  one `warn` record says so per outage; the recovery is one `info` record.

## Reporting

<a id="reporting"></a>

- **The log.** Each file that becomes a suspected stall is one `warn` record, once per process and
  again only after it stopped being one: the file, where it stalls (h:mm:ss), how many sessions
  ended there, on how many clients and how many of them were retries that got nowhere, how many
  sessions there were and how many played it through, and the clients. Where
  a live retention of THIS swap still holds the original, the record names the command,
  `holdfast restore <path>`; otherwise it says the original is no longer retained and the file has
  to be acquired again.
- **`GET /api/playback`**, behind the read token like every other read: whether evidence is
  collected, where the poll has got to, every current swap counted by verdict, and every suspected
  stall with its cluster, its sessions and the restore command where it applies
  ([docs/api-reference.md](../api-reference.md#playback)).
- **Metrics:** `holdfast_playback_suspect_files`, `holdfast_playback_played_through_files` and
  `holdfast_playback_history_read_timestamp_seconds`, read at scrape time through the reporting
  door - never on the one write connection the engine's claims and finishes go through - and
  absent until a poll has read the history to its end: a library nobody has asked about is not a
  library with no stalls.

**Nothing restores automatically.** `holdfast restore` overwrites a library file with older bytes,
which is why it is a local command and never an HTTP endpoint
([layout](../layout.md)); the report names it and an operator runs it. Nor does the evidence move
the undo window: a retained original is released when its window closes whatever the evidence
says. Holding originals until each file had been played through was considered and not chosen by
the owner - many files in a library are never watched, so that hold would never end - and is not
built.

## What is not established

No goal of this program contacts a real Tautulli, so all of the above is proved against fakes built
from the documented shapes (`internal/playback`'s suite runs the real Tautulli client against an
`httptest` server). `ASSUMED`, for the owner's live check:

- that a `get_history` row carries `view_offset` (milliseconds) on the builds that record it. Where
  a row does not - which may be the common case - the offset is taken from `percent_complete` and
  the item's duration, to the nearest percent, and the stall tolerance widens to match;
- that `play_duration` is the seconds the session played, paused time excluded, and that
  `machine_id` names the playing client;
- that `get_history` honours `grouping=0`, `include_activity=0` and ascending order by `started`
  for offset paging, and that `after` is inclusive;
- that a rating key's `get_metadata` names the current file after a swap, including one that
  changed the container extension (Plex's own rescan, which the post-swap hook requests, is what
  updates it);
- that Tautulli does not log a session that played for less than its *Ignore Interval* setting
  (120 seconds by default): a resume that stalls within seconds never reaches the history, so it is
  not evidence here. Lowering that setting makes such sessions visible.
