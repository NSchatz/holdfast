# Worker nodes

What a worker node may do, what the server keeps for itself, and why the lease protocol is
shaped the way it is. This document is that argument's single home: `CLAUDE.md` names the rule
and links here rather than restating it. The keys are described in
[`docs/profiles.md`](../profiles.md#worker-nodes), the endpoints in
[`docs/api-reference.md`](../api-reference.md#node-leases); the code is `internal/node`, the
ledger is `internal/store/lease.go`, and the route group and its credential check are in
`internal/server`.

**What this build has.** The protocol, its durable lease rows, the `node_token` credential and
the configuration keys. Only **mapped mode** exists: a node reads the source through its own
mount of the library and uploads the output over HTTP. The `holdfast worker` command, the
engine's hand-off of a job to a node, and the mode in which the server streams the source over
HTTP land in following changes. Until the engine is wired, a server with `node_token` set
answers the lease endpoints `503`, and leases nothing.

## The rule

<a id="leases"></a>

**A node's output is only ever a candidate: it lands in a working file the server named, on a
live lease at the current epoch, with the declared length and digest, and the server's own gates
and rename decide.**

A node only encodes. The server that owns the library derives the plan, names the working file,
re-runs every gate against its own copy of the source and performs the same-filesystem rename
itself, so the swap invariant ([swap.md](swap.md#swap-invariant)) is untouched: the code that
gates and swaps a node's output is the code that gates and swaps a local encode. What the lease
protocol adds is the proof that the bytes in the working file are the ones a node currently
entitled to send them declared - nothing more. Digests are transport integrity; the gates remain
the fidelity proof.

Nodes are off until `node_token` is set. With it unset no lease is granted, the endpoints answer
`403` naming the key, and nothing about an existing configuration's behaviour changes.

## The lease

A lease is one grant of one job's encode to one node. It is a durable row (`node_leases`), so a
restarted server can tell a live node's work from a stale one's.

| field | what it is |
|---|---|
| lease id | 16 random bytes in hex, unique to one grant |
| epoch | the **fencing token**: one more than the highest epoch any earlier grant of the same path carried, computed inside the transaction that inserts the row. `UNIQUE (path, epoch)` in the schema |
| expiry | the server's clock, in unix seconds. Nothing a node sends moves it except a heartbeat the server itself timed |
| state | `granted`, `uploaded`, `completed`, `failed`, `expired` |
| working file | the path the server named for this grant. An upload is written there and nowhere else; no request carries a path |
| argument-list digest, source size and mtime | what the restarted server compares a re-derived job with before it takes a lease back |
| recorded figures | the admitted output's sha-256 and length, and the source sha-256 the node reported |

**Every call carries the lease id and the epoch, and both are checked with the expiry inside the
transaction that acts.** The ledger reads the row, hands it to a decision (`internal/node`,
`lease.go`: pure functions over the row, the epoch and the clock) and writes what the decision
returns in that same transaction, so the check and the act cannot be separated by another writer.
A lease that is not live at the epoch presented - ended, run out, or superseded by a later
grant - answers `410 Gone`, and nothing it sent is written. A lease at its expiry instant is
expired.

Every `410` carries `Cache-Control: no-store`: RFC 9110 says "A 410 response is heuristically
cacheable" (section 15.5.11), and a cached "this lease is gone" must never answer for another.

**Expiry is the server's clock only.** A node heartbeats every quarter of the TTL
(`node_lease_ttl_sec`, 60 s, **ASSUMED**: nobody has measured a node deployment). A lease whose
heartbeats stop for one TTL expires; the server then removes the working file **that lease
recorded** and nothing else, and the job is offered again at the next epoch.

**A lease removes its working file only while the engine is waiting on it.** The path is the
lease's for exactly as long as an engine call in this process is attached to it. A lease that
ends later than that - a recovered lease the engine did not take back, or one whose engine call
left while the ledger could not end the row - no longer owns the path: the engine may already
have written its next attempt at the same name, so that late ending unlinks nothing. A lease
nothing waits on also takes no upload and no completion, and its heartbeats do not extend it.
A grant is refused when its working file is a live lease's working file or a live lease's
source.

## The upload

`PUT /api/node/v1/leases/{id}/output` is admitted only when all of these hold, in this order:

1. `Content-Length` is declared (`411` otherwise; RFC 9110 section 15.5.12: "the server refuses
   to accept the request without a defined Content-Length"). The length is what the size cap,
   the free-space check and the read deadline are taken from.
2. A `Content-Digest` header carries a readable `sha-256` member (RFC 9530 section 2: the field
   "is a Dictionary ... where each: key conveys the hashing algorithm ... value is a Byte
   Sequence", so `sha-256=:<base64>:`; `sha-256` is one of the two algorithms section 5 lists as
   Active). The node has the whole file before it sends it, so a header is enough and no trailer
   is needed.
3. The lease is live at the epoch in `Holdfast-Lease-Epoch` - checked **before the working file
   is opened**, so an expired or superseded lease writes no byte anywhere.
4. The declared length is at most the source size minus one (`413` otherwise). The
   strictly-smaller gate accepts nothing else, so anything larger could only be discarded.
5. A transfer slot is free (`node_max_transfers`) and the filesystem holding the working file
   has room for the declared length. Either refusal is `503` with `Retry-After`, and the lease
   stays live. A failed free-space lookup refuses nothing, as the engine's own check does not.
6. The body, hashed while it is written, has exactly the declared length and the declared
   digest. Otherwise the working file is deleted and the answer is a typed `digest_mismatch`;
   the node may send the body again within the lease, and the third mismatch (**ASSUMED** bound)
   fails the lease.
7. The lease is still live at that epoch in the transaction that admits the file. A lease that
   ran out or was superseded while the body arrived is `410`, and the working file is deleted.

The body is wrapped in `http.MaxBytesReader` at the declared length, and read under a deadline
set through `http.ResponseController.SetReadDeadline`: the whole upload may take 30 s plus one
second for every 64 KiB declared, and no single read longer than 30 s (both **ASSUMED**), so a
stalled upload cannot hold a transfer slot and a reservation for ever. The listener itself sets
only `ReadHeaderTimeout`.

**A repeat is answered from the record.** A `PUT` that declares the recorded digest and length
on a lease that already accepted an output answers `200` with the recorded figures and rewrites
nothing: the working file's inode and modification time are unchanged. A different digest or
length after acceptance is `409`. `complete` is idempotent the same way.

**Why this closes the 404-body class.** The incumbent failure this guards against is an HTTP
error page received in place of the finished file and taken for media. Here a 55-byte error page
sent under the digest of the real output fails rule 6 and leaves no file, and one sent under its
own honest digest is still only a candidate the server's gates then refuse.

**A slow upload can never remove a later grant's file.** The working file's path is the same
across grants of one path. Every terminal transition and the removal it licenses, every grant,
and the creation of a working file are ordered by one lock, and an upload whose lease ended is
disowned under it: when it finally fails it removes nothing.

## Completion, failure and the engine

`POST .../complete` reports the output digest and length (which must be the ones the upload
recorded) and the sha-256 of the source bytes the node read. That source digest is recorded and
handed to the engine, which compares it with its own hash of the source before the gates: it is
what catches a wrong path map or a stale network-mount cache. `POST .../fail` ends the lease with
the node's typed reason. Either way the engine call waiting on the lease returns - with the
figures, or with a typed error naming the node, the epoch and the reason.

## Backpressure

A node asks for work with a bounded long-poll (30 s, **ASSUMED**), answered `204` with
`Retry-After` when none arrived. The poll returns as soon as the server starts draining, so a
graceful shutdown never waits one out. The caps - live leases across every node
(`node_max_leases`, 4), per node (`node_max_leases_per_node`, 1) and uploads in flight
(`node_max_transfers`, 2), all **ASSUMED** - answer `503` with `Retry-After` and a typed reason,
and are enforced again inside the grant transaction. `node_gate_slots` (1, **ASSUMED**) is
declared and validated in this change and enforced by nothing yet: the engine wiring that
follows is what holds the number of node outputs gated at once to it. A node whose build version is not the server's is answered `409` naming both
versions. Not `426`: RFC 9110 section 15.5.22 says "The server MUST send an Upgrade header field
in a 426 response to indicate the required protocol(s)", and no protocol is on offer.

A node reports the encoders its own probe found, and a job is leased only to a node that reports
the job's encoder.

## A server restart

Lease rows survive. At start, before anything is granted:

- every lease that was `granted` gets **one TTL of grace from the restart**, whatever its expiry
  said: the server's downtime is not the node's silence. A heartbeat inside the grace is `200`
  and one after it `410`.
- every lease that was `uploaded` and not completed is ended. Its output is **never gated after
  the restart**, and the recovery itself removes the recorded working file; the job is leased
  again from the start, at the next epoch. This is the one file a restart removes on a lease
  row's word, and a deliberate departure from leaving every working file to the startup sweep:
  that sweep holds back a full-length file in the target codec with no job record as a possibly
  stranded replacement, which is exactly what a complete, never-gated upload looks like. The
  lease row is the record that it is not one, and the recovery runs before any grant and any
  engine work, so nothing else can have written at that path.
- every other working file a lease recorded before the restart is left to the startup sweep,
  exactly as a killed local encode's is. Taking a lease back, abandoning it, or letting its
  grace run out removes no file.
- a recovered lease is taken back only by a re-derived job with the same path, the same
  argument-list digest and the same source size and modification time. Any other job ends it.
  Until the engine has taken a lease back, the node can heartbeat it - without stretching its
  grace - and nothing else; one the engine never takes back runs out after its grace.

Until this recovery has run every lease endpoint answers `503`, and no lease is expired: an
expiry read before the grace was given is one the server's own downtime ran out.

## What is kept

Terminal rows are the epoch history. A prune deletes a terminal row once it ended more than
seven days ago (**ASSUMED**) - except the newest row of each path, which the next grant counts
its epoch up from and which is therefore never pruned. A live row never is. The table is bounded
by the number of paths ever leased, not by the number of grants.

## The credential

`node_token` is a secret reference ([docs/secrets.md](../secrets.md)) and a key of its own, so
that the credential a worker holds is the least privileged one that can do a worker's job. It
opens `/api/node/v1` and nothing else; the read, control and webhook tokens are not accepted
there; it may not be written as the same reference as any of them, and `serve` refuses one that
resolves to the same value. A stolen node token can lease jobs and upload candidates - which
still face every gate, but cost the server a decode and a VMAF run each - and cannot pause,
exclude, search, scan or queue. No lease endpoint restores, requeues, resolves or re-opens
anything.

Digests are no defence against someone on the path: RFC 9530 section 6.1 says "an on-path
malicious actor can either remove a digest value entirely or substitute it with a new digest
value computed over manipulated representation data or content", mitigated by "Transport Layer
Security (TLS) or digital signatures". TLS for nodes is not built in this change.

## The worker's path map

`worker_path_map` translates a source path as the server names it into the path the worker reads
it at. A source no entry covers is **refused, never passed through**: a path handed back
unchanged because nothing matched would be read from wherever that name leads on the worker. A
worker that mounts the library at the server's own paths says so with an entry that maps the
directory to itself.

## Sources

Read 2026-10-03.

- RFC 9530, Digest Fields (Standards Track, February 2024): sections 2, 5 and 6.1, and the
  `sha-256` sample for `{"hello": "world"}` in appendix D, which `internal/node`'s digest test
  pins. https://www.rfc-editor.org/rfc/rfc9530.html
- RFC 9110, HTTP Semantics: sections 15.5.9 (408), 15.5.10 (409), 15.5.11 (410), 15.5.12 (411),
  15.5.14 (413), 15.5.22 (426) and 15.6.4 (503, which "MAY send a Retry-After header field").
  https://www.rfc-editor.org/rfc/rfc9110.html
- `net/http`, from `go doc` of the Go 1.25.14 toolchain this repository builds with
  (https://pkg.go.dev/net/http): `MaxBytesReader` "returns a non-nil error of type
  *MaxBytesError for a Read beyond the limit" and "tells the ResponseWriter to close the
  connection after the limit has been reached"; `ResponseController.SetReadDeadline` "sets the
  deadline for reading the entire request, including the body", and "Setting the read deadline
  after it has been exceeded will not extend it".
- The approved proposal this implements: `.claude/goals/2026-09-holdfast-research/proposal-node-protocol.md`
  (option (a), rules 1 to 9 and 11 in mapped mode).
