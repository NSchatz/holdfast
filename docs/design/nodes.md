# Worker nodes

What a worker node may do, what the server keeps for itself, and why the lease protocol is
shaped the way it is. This document is that argument's single home: the [design index](README.md) names the rule
and links here rather than restating it. The keys are described in
[`docs/profiles.md`](../profiles.md#worker-nodes), the endpoints in
[`docs/api-reference.md`](../api-reference.md#node-leases); the code is `internal/node`, the
ledger is `internal/store/lease.go`, and the route group and its credential check are in
`internal/server`.

**What this build has.** The protocol, its durable lease rows, the `node_token` credential, the
configuration keys, the engine's hand-off of a job to a node ([the seam](#seam)) and the
`holdfast worker` command ([the worker](#worker)). A node works in one of two modes, its own
choice, stated in every request for work: **mapped mode**, in which it reads the source through
its own mount of the library, and **http mode**, in which the server streams the source on the
lease ([the source stream](#http-mode)). Either way the output is uploaded over HTTP. `serve`
can listen with TLS on a certificate of its own, and a worker refuses to send its credential in
the clear ([the transport](#transport)). A `serve` with `node_token` set leases work as soon as
its start-up recovery has run; `run` leases nothing.

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

The mode a lease was granted in, and the sha-256 of what the server streamed on it, are not in
the row: they are the memory of the server process that granted it ([below](#http-mode)).

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
5. A transfer slot is free (`node_max_transfers`, which counts source streams and uploads
   together) and the filesystem holding the working file
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
recorded) and the sha-256 of the source bytes the node read. Where the server streamed the whole
source on that lease, the completion is held to the digest of what it streamed, in the
transaction that would record it ([below](#http-mode)). The source digest is recorded and handed
to the engine, which compares it with its own hash of the source before the gates, in both modes:
it is what catches a wrong path map or a stale network-mount cache. `POST .../fail` ends the lease with
the node's typed reason. Either way the engine call waiting on the lease returns - with the
figures, or with a typed error naming the node, the epoch and the reason.

## Backpressure

A node asks for work with a bounded long-poll (30 s, **ASSUMED**), answered `204` with
`Retry-After` when none arrived. The poll returns as soon as the server starts draining, so a
graceful shutdown never waits one out. The caps - live leases across every node
(`node_max_leases`, 4), per node (`node_max_leases_per_node`, 1) and source streams plus uploads
in flight (`node_max_transfers`, 2), all **ASSUMED** - answer `503` with `Retry-After` and a typed reason,
and are enforced again inside the grant transaction. `node_gate_slots` (1, **ASSUMED**) is
enforced by the engine ([the gate slots](#gate-slots)). A node whose build version is not the server's is answered `409` naming both
versions. Not `426`: RFC 9110 section 15.5.22 says "The server MUST send an Upgrade header field
in a 426 response to indicate the required protocol(s)", and no protocol is on offer.

A node reports the encoders its own probe found, and a job is leased only to a node that reports
the job's encoder.

## The source stream (http mode)

<a id="http-mode"></a>

**A node with no mount of the library is streamed the source of its own live lease, and of
nothing else; the server hashes what it streams, and a completion that reports another digest
fails the job before any gate.**

A node asks for work in a mode, `mapped` or `http`, and its lease is granted in that mode; the
answer says which. The mode is per node and is the node's own choice (the owner's decision that
a node either shares a mount or is streamed the source): the server has no key that permits or
forbids http mode, and what that means for whoever holds a `node_token` is stated under
[the transport](#transport). A plan that is leasable in mapped mode is leasable in http mode and
nothing else becomes leasable ([which jobs are leased](#leasable)).

`GET /api/node/v1/leases/{id}/source` carries the epoch in `Holdfast-Lease-Epoch`, as the upload
does. **No request names a path.** The file served is the source the lease row records, and only
when all of these hold:

1. The lease is live at that epoch: otherwise `410` with `Cache-Control: no-store`.
2. An engine call in this process is waiting on it: otherwise `503 not_ready`, as for an upload.
3. It was granted in http mode **by this server process** and has admitted no output yet:
   otherwise `409 source_not_offered`. A mapped lease's source is never served.
4. A transfer slot is free: otherwise `503 transfers_full` with `Retry-After`, and the lease
   stays live.
5. The file, once opened, is a regular file of exactly the size and modification time the lease
   was granted on: otherwise `409 source_changed`. It is opened without following a symbolic
   link at the leased path (`O_NOFOLLOW`): the engine refuses a source that is itself a link,
   so one found there was put there after the grant, and what it points at is not the leased
   file however its size and time read. The lease stays live - the node fails it
   `source_withdrawn`, and the server's own guards then decide about the file it finds.

The body is sent by `http.ServeContent`, which "handles Range requests properly", under
`Content-Type: application/octet-stream` set **before** the call: "If the response's Content-Type
header is not set, ServeContent first tries to deduce the type from name's file extension and,
if that fails, falls back to reading the first block of the content and passing it to
DetectContentType". A sniffed head would be read twice, so the type is fixed, no name is given
and no modification time (so no `Last-Modified`). `ServeContent` also "handles If-Match,
If-Unmodified-Since, If-None-Match, If-Modified-Since, and If-Range requests", which would
answer `304` or `412`; this endpoint has neither answer, so those five request headers are
dropped before it is called and a conditional request is served like any other. The reader the server hands `ServeContent` hashes only bytes read in order from the first
one, each once.

**What the server streamed, it hashed.** One unranged `GET` that sent the whole source leaves
its sha-256 with the lease. When the node later reports `source_digest` in `complete`, the two
are compared inside the transaction that would complete the lease: a different digest **fails
the lease** (`source_digest_mismatch`, answered `409`), the admitted working file is removed, the
engine call returns the typed ending, the job fails at the encode gate - before any gate ran -
and the source is untouched. `max_failures` counts it, as it counts a source digest the server's
own hash refuses.

**A ranged response leaves no digest**, even one that spans the whole source: it is not one
response carrying the whole representation. Nor does a stream that was cut. The proof is then
the engine's own hash of its own copy of the source, taken before the gates in **both** modes, so
a ranged read is never unchecked. The worker itself never sends `Range`: a failed download
starts again from the first byte.

**The mode and the streamed digest are not in the lease row.** They live in the server process
that granted the lease. Recovery after a restart is correct without them, and no schema change
was made for them: a recovered lease's mode is unknown, so its source is **not served** (rule
3) rather than served on a guess; a node that already holds the whole source carries on, uploads
and completes, and is held to the server's own hash; a node that was still downloading fails
its lease `source_withdrawn`, and the server encodes that job itself, charging the file nothing
and - because the withdrawal was the server's own doing - not counting it toward that node's
cool-off. The streamed digest is transport integrity for one process's stream and nothing else
relies on it.

**A stalled download cannot hold a transfer slot.** The stream is written under a deadline set
through `http.ResponseController.SetWriteDeadline` - which "sets the deadline for writing the
response" - and moved on as the stream advances: no write may take longer than 30 s, and the
whole stream no longer than 30 s plus one second for every 64 KiB of the source (both
**ASSUMED**, the upload's own figures). A stream also ends when its lease does: a source is sent
only while the lease is live, for the whole of the stream.

**`Repr-Digest` is sent where it can be, and nothing rests on it.** After a whole source the
server sets the RFC 9530 `Repr-Digest` trailer. Over HTTP/2 - which a TLS listener negotiates -
it arrives beside the declared length; over HTTP/1.1 the response has a `Content-Length`, is not
chunked, and carries no trailer. Go's own documentation says "Few HTTP clients, servers, or
proxies support HTTP trailers", so the comparison in `complete` is the load-bearing one. A
worker that does receive a readable trailer that disagrees with what it read stops before it
encodes.

## The transport

<a id="transport"></a>

**A worker speaks to its server over TLS or to loopback; plain HTTP to any other host is refused
unless the operator wrote `worker_insecure_http: true`, which is logged at every start; and a
node's source is streamed only on a live lease, with its sha-256 compared on the server before
any gate.**

**The server.** `server_tls_cert` is a plain path to a PEM certificate chain and
`server_tls_key` a secret reference (`file:` or `cmd:`) to its PEM private key
([docs/secrets.md](../secrets.md)). Both or neither: one without the other refuses to start
naming the missing key, a literal key refuses as every literal credential does, and a pair that
does not parse or does not belong together refuses with an error that names the key at fault
and prints nothing of the key. With both set, `serve` listens with TLS on `server_addr` - the
whole surface, not only the node endpoints - and answers nothing in the clear. With neither it
listens exactly as it always has. The minimum version is TLS 1.2, written into the listener's
configuration rather than left to the toolchain's default ("By default, TLS 1.2 is currently
used as the minimum", which "can be reverted to TLS 1.0" by a `GODEBUG` setting). There is no
client certificate and no user login: the credentials are the bearer tokens they were, and a
reverse proxy that terminates TLS in front of a plain listener remains a supported deployment.

With TLS on, anything that probes the listener - a container health check, a monitor, a
Prometheus scrape of `/metrics` - must speak `https://` and trust the certificate; a plain-HTTP
probe gets no answer from the API.

**The worker.** `worker_server` is `https://`, or `http://` to a loopback host (`127.0.0.0/8`,
`::1`, `localhost`). Plain `http://` to any other host refuses to start, before the credential
is even resolved, unless `worker_insecure_http: true` is written; the worker then starts, and
says what that costs in one `warn` record at **every** start. With `https://` or a loopback
server the key does nothing and says nothing. `worker_tls_ca` is a plain path to a PEM bundle the
worker trusts **in addition to** the system roots, for a server whose certificate is private; an
unreadable bundle, or one holding no certificate, refuses to start naming the key. **Nothing
switches certificate verification off**: there is no such key, and a server whose certificate
the worker does not trust fails the TLS handshake before any request - and so before the
credential, a source request or an upload - is sent.

**What plain HTTP costs.** These are the consequences `worker_insecure_http: true` accepts, and
equally those of a TLS-terminating proxy that is bypassed:

- Over plain HTTP the `node_token` crosses the network **in cleartext on every request**.
- Whoever captures it can lease jobs and upload outputs. Those outputs still face every gate, so
  no source is replaced by them, but each costs the server a decode and a VMAF run, and each
  lease holds a free-space reservation.
- In **http mode** the same captured credential reads the library's media: a lease's source is
  streamed to whoever holds the lease, and the media itself also crosses the network in the
  clear.
- **Digests do not help against that attacker.** RFC 9530 section 6.1: "an on-path malicious
  actor can either remove a digest value entirely or substitute it with a new digest value
  computed over manipulated representation data or content", mitigated by "Transport Layer
  Security (TLS) or digital signatures". The digests here are transport integrity against
  accident; TLS is the defence against someone on the path.
- Nodes on other hosts need an explicit non-loopback `server_addr`. Without `server_read_token`
  that address serves every media path in the read API to that network, without a credential
  (`serve` says so when it starts). **A worker deployment sets `server_read_token` too.**
- **No mTLS.** A node is authenticated by `node_token` and by nothing else; the server asks for
  no client certificate.

## The seam

<a id="seam"></a>

**A node job is an ordinary job whose encode step is "lease it and wait for the upload into the
working file this job already named"; everything before that step and everything after it is the
code a local job runs, on the server.**

`ProcessFile` has one encode seam. Before it: the guards, the claim, the server-named working
file (beside the source, or the scratch working path under `scratch_dir`), the free-space hold
and the owner record. After it: every gate, the play hold, the copy beside the source, the
metadata carry and the durable rename. None of that knows whether the bytes in the working file
came from the server's ffmpeg or from a node, so no verdict of a node licenses a swap, and
[swap.md](swap.md#swap-invariant) and [quality-gate.md](quality-gate.md#vmaf-pooling) are
unchanged.

**The feeders.** With nodes on, a pass starts `node_max_leases` feeders beside its `workers`
local workers, on the same feed. A feeder takes a file from the feed **only while it holds a
node's poll**: with no node asking for work the feeders consume nothing and the local workers see
the feed exactly as they do with nodes off. A feeder that ends a file without leasing it gives
the poll back; one that holds a poll with no file arriving - the feed is paused, or every file is
with a local worker - lets it go after one second (**ASSUMED**) and asks again. When the feed
closes or the pass is cancelled every feeder stops, and none outlives the pass. With `node_token`
unset no feeder is started and the pool, every command line and every decision are the ones of a
build without nodes.

**A poll is always answered at its long-poll bound.** One still queued, and one a feeder holds
reserved, are both answered `204` when the bound runs out. A reserved poll's ticket is dead from
that moment, and so is the ticket of a poll whose request ended: nothing is granted on a dead
ticket - no lease row, nothing recorded against the job. The job that carried it waits for
another node to ask, for up to two long-poll bounds (**ASSUMED**), and is leased to that one;
failing that the server encodes it.

Nodes are given work only while a pass runs: the initial scan, an interval scan
(`scan_interval_sec`) or a rescan. A file submitted through `POST /api/scan` is encoded by the
server.

<a id="leasable"></a>

**Which jobs are leased.** Only a plan whose command line is self-contained on another host: a
software encoder the node reported, software decode, no device argument, no loudness-normalised
track (its gate reads the encoder's own report channel), no dynamic-HDR carriage (its pre-pass
files sit beside the working file), no picture carried as a Matroska attachment (copied out
beside the working file first), and never the software second attempt of a failed hardware
encode. Every other job a feeder carries to the seam is encoded by the server itself, the poll
going back to its node unused: fail safe, never a guess on the node. Hardware encoders on a node
would need the node's own start-time probe to gate the job ([hardware.md](hardware.md#probe));
that is not built.

**What a lease carries.** The ffmpeg options the server's plan builds - those between the input
and the output, with the container's - and not a second description of the plan, so the command
line and every gate still read one plan ([encode-plan.md](encode-plan.md#encode-plan)). The
server's libx265 pool figures are left out: they describe the server, and the node's libx265 uses
its own. The fixed leading options, `-i`, the muxer-queue bounds and the output are added on the
node by the one function every local encode's command line goes through, so the two cannot drift.

**Before the gates.** When a lease completes, the server hashes **its own copy of the source**
once (sha-256, one sequential read) and compares it with the digest of the bytes the node read.
A mismatch fails the job at the encode gate naming a wrong `worker_path_map` entry or a stale
mount; nothing is gated, the working file is removed and the source is untouched. The working
file must also be the length the lease recorded.

<a id="endings"></a>

**How a lease that ended without an output is read.** There are two kinds, and only one is a
fact about the file.

- **The node could not run it**: the worker failed the lease `unmapped_source`,
  `source_mismatch`, `source_unreadable`, `unsupported_encoder`, `refused_plan`,
  `worker_stopping`, or in http mode `source_download_failed`, `work_dir_full` or
  `source_withdrawn`; or the re-derived job after a restart is not the leased one; or the hub
  granted nothing. **Nothing is recorded against the file.** The server encodes the job itself,
  in the same attempt, inside a gate slot, exactly as it does a plan that is not leasable, and
  one record names the node and the reason.
- **The lease was really attempted**: it expired, the node's encode failed, its uploads failed
  the digest bound, it reported a source digest that is not what the server streamed, or the
  server refused the completed output before its gates. The job fails
  at the encode gate, class transient, through the branch a failed local encode leaves by.
  `max_failures` counts it, and at the bound the existing claim holds the row out, so a poison
  job cannot loop across nodes; one record then names the node attempts this process saw since
  the file's last other outcome.

The approved proposal charged every ended lease to the file. That let one broken node - a wrong
path map fails a lease in milliseconds - park every file the feed offered it, so the first kind
is split off: a departure from the letter of the proposal's retry rule, recorded as one.

**A node whose leases keep ending is cooled off.** After three leases of one node end in a row
(either kind) with none succeeding between, that node is offered nothing for five minutes (both
**ASSUMED**): its polls answer `503 node_cooling_off` with the time left as `Retry-After`, and
one `warn` record names the node, the reasons and the cool-off. A lease whose output the server
took clears the run. A poll that left before its grant counts for nothing, and neither does a
lease the worker failed `source_withdrawn`: the server stopped offering that source itself (it
restarted, or the file is no longer the leased one), which says nothing about the node, so it
neither lengthens the node's run nor clears it.

A cancelled pass, and a lease the server ended because it is stopping, record nothing against
the file, exactly as an interrupted local encode does not.

**Free space.** A feeder's job takes the same free-space reservation a local job takes, before
anything is leased. A refusal fails the job as it always did, and the node's poll is answered
`503 no_room`. The upload re-checks the filesystem once its length is known.

<a id="gate-slots"></a>

**The gate slots.** Every node output costs the server a source hash, a full decode-integrity
pass and a VMAF run. `node_gate_slots` (1, **ASSUMED**) is how many node jobs the server does
that work for at once, beside its `workers` local jobs: a slot is held from the moment a lease
hands its output back - or from the start of the server's own encode of a job that was not
leasable - until the job ends. While every slot is held **and** as many jobs again are already
waiting for one, the feeders stop asking for demand, so a node's poll is answered with no work
rather than with a job whose output would only queue. Leases granted before the queue filled are
not withdrawn.

A job a feeder carried to the seam that the server ends up encoding itself - a plan that is not
leasable, a node that could not run its lease - is one more encode on the server beside its
`workers`, and `node_gate_slots` is what bounds how many of those run at once.

**Known limit: a lease has no maximum lifetime.** A node that keeps heartbeating holds its job
for as long as it does, as a hung local encode holds its worker. Nothing ends such a lease but
the node, a restart of either side, or the pass being cancelled.

<a id="worker"></a>

## The worker

`holdfast worker --config <file>` is the node. It reads `worker_server`, `node_token` (by
reference, resolved once), `worker_name` (default: the host name), `worker_slots`, `worker_mode`
(`mapped`, the default, or `http`), `worker_path_map`, `worker_work_dir` (default:
`holdfast-worker` under the OS temp directory), `worker_insecure_http` and `worker_tls_ca`, and
it refuses to start naming the key when `worker_server` or `node_token` is missing.

**A mapped worker names its mount; an http worker names no library at all.** In mapped mode the
configuration is loaded and validated like every other command's, so it names `library_roots`:
the worker's own mount of the library. In http mode the worker reads no library, so its
configuration carries no `library_roots` and no `worker_path_map` - no fake root is asked for -
and one that names either beside `worker_mode: http` is refused by name, because a file that
says both cannot mean both. `holdfast validate` accepts an http worker's file and says what it
is - after taking every start-or-refuse decision `holdfast worker` takes from the file alone
(the address, the transport, `worker_tls_ca`, the presence of `node_token`, the name), so it
never says "config OK" of a file the worker would refuse; `run` and `serve` refuse it, having
nothing to scan.

**It refuses a plain `http://` server whose host is not loopback**, unless
`worker_insecure_http: true` is written ([the transport](#transport)).

At start it runs the start-up probe encode of every software encoder in the registry through its
own ffmpeg and reports the ones that work, with its build version, in every request for work.
Each of its slots then loops:

1. **Ask for work.** Only a `200` whose body is the JSON of a whole lease is a lease. A `204` is
   asked again after the server's `Retry-After`; a `503`, any other status, a `200` that is not
   JSON and a failed call are backed off from exponentially with jitter, 5 s to 5 min
   (**ASSUMED**), never under a stated `Retry-After`; a `409 version_mismatch` stops the worker,
   naming both versions. An error page is never taken for a lease, whatever its status.
2. **Check the lease.** A lease in another mode than the one the worker asked in is no lease.
   In mapped mode the source path is mapped through `worker_path_map`; one no entry covers
   fails the lease `unmapped_source`. An encoder the worker did not report fails it
   `unsupported_encoder`. A mapped source that is not exactly the size the lease was granted on,
   or whose modification time is more than two seconds away, fails it `source_mismatch`: one file
   read through two mounts can show two times (FAT keeps two-second stamps, SMB and NFS round),
   and this is only the early refusal - the proof is the source digest the server compares. A
   command line outside the shape the server's plans have fails it `refused_plan`, unrun (below).
   In **http mode** there is no path to map: the worker first checks that the work directory
   has room for the source and the largest output the lease admits, **beside what its other
   slots' leases in flight have reserved there**, and reserves it until the lease ends - check
   and reservation are one step under one lock, so two slots never both pass on the same free
   bytes (a failed lookup refuses nothing; short of room, or on a write that fails for want of
   space, the lease fails `work_dir_full`). It then **downloads** the source into
   the work directory with one unranged `GET`, heartbeating all the while and hashing what
   arrives as it writes it. **The response is media only when it is a `200` that delivers
   exactly `source_size` bytes**: a `404` with a body, any other status, a `200` that declares
   another length, a body that ends early, one that runs long and one under a content coding
   are never encoded. A redirect is never followed. A download that broke off, or whose
   `Repr-Digest` trailer disagrees, starts again from the first byte - at most three times in
   all (**ASSUMED**), never resumed with `Range` - and one that receives no byte for 60 s
   (**ASSUMED**) is cut; a `410` ends the lease there. What cannot be cured fails the lease
   `source_download_failed`. **A `503` is not a failed download and is not counted**: the
   server has no transfer slot free (`node_max_transfers` is below the number of nodes
   downloading, which is ordinary) or is not ready, so the worker asks again after the
   server's `Retry-After`, jittered, heartbeating all the while, for as long as the lease
   lives. A `409 source_not_offered` or `source_changed` is the server withdrawing the source:
   the lease fails `source_withdrawn` at once.
3. **Encode** into the work directory, heartbeating every `heartbeat_sec` with its progress. A
   `410` stops the encode at once and discards the output. In http mode the input is the
   downloaded file, and the command line is assembled by the same function as in mapped mode.
4. **Hash** the source it read and the output (sha-256) - in http mode the source's digest is
   the one taken while it arrived - and **upload** the output with its
   `Content-Length`, `Content-Digest` and the epoch. A digest mismatch and a `503` are sent again
   while the lease lives, at most five times (**ASSUMED**); the server's own bound ends the lease
   first.
5. **Complete**, and remove the local output and, in http mode, the downloaded source - which
   it does on every way out it lives through. A killed worker leaves them in the work
   directory; the next start removes the files there that carry its own naming (32 hex
   characters, the epoch, then `.out`, or `.src` with an optional short extension) and nothing
   else.

After a lease it failed itself the worker backs off before it asks again, exponentially: what
stopped it is very likely still there.

<a id="refused-plan"></a>

**The worker does not run whatever a lease carries.** It holds the node credential's word for
what to execute, so it refuses, unrun, a lease with any option before the input, or whose
options carry `-i`, `-attach`, `-dump_attachment`, `-y`, `-progress`, `-filter_script`,
`-filter_complex_script` (each with or without a stream specifier), a `--` token, an argument
that is an absolute path, or one that climbs out of its directory with `..`. The server's
leasable plans emit none of these.

**Mount the library read-only on a mapped node.** A worker only ever reads the library: the output goes
to its work directory and from there over HTTP, and the server is the only writer beside the
sources. The refusal above is a second line, not the first - a filter or a muxer option this
list does not name could still name a file - so the mount should not let the node's ffmpeg write
there whatever it is told. A read-only mount also makes "the server is the only writer into the
library" true by construction rather than by the worker's good behaviour.

**It follows no redirect.** A `3xx` is the answer itself and never a lease: following one would
carry the credential, and an upload's body, to wherever it points. `worker_server` carrying
userinfo, a query or a fragment refuses to start.

On SIGTERM it stops its encodes, fails their leases `worker_stopping` best-effort, and exits 0.
The worker writes nothing into the library: the server stays the only writer there.

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

<a id="restart"></a>

**The engine takes each lease back before anything is granted.** `serve` runs the recovery before
its listener accepts and before its first scan. Each recovered lease's path is run through
`ProcessFile` again: it claims the row (the rows a dead process left active are reset first),
re-takes the free-space hold and a working file through its ordinary code, re-derives the plan,
and at the seam re-attaches the lease. A lease whose job never gets there - the guards now skip
the file, it is gone, its plan is no longer leasable - is abandoned. Only when every recovered
lease has re-taken its holds or been abandoned may the server grant again.

The recovered leases' jobs run at once, not one after another, and the whole step is bounded by
one lease TTL: a lease whose job has not reached the seam by then is abandoned, and that job
carries on as the server's own. The listener is therefore never held for longer than that. When
the server is ready to grant, every recovered lease still live is given **one TTL from that
moment**, so the grace is not spent on the server's own start-up work.

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

The nodes and their leases can be READ at `GET /api/nodes`, which is not a lease endpoint: it sits
in the read group, behind `server_read_token` where one is set, and the node token does not open
it. It serves each node's name, the mode and encoders of its last poll, whether it is waiting or
cooling off, and each lease's node, path, state, epoch, times, reason and sizes. It never serves a
lease id, a working file's path, a digest or a token - a lease id is what a heartbeat, an upload and
a completion are authorised by - and it grants, ends, adopts and re-opens nothing
([reference](../api-reference.md#nodes-read)).

In http mode a node token also reads the source of every lease it is granted, which over a pass
is the library's leasable media. Digests are no defence against someone on the path, and what
keeps the credential and the media off the wire in the clear is TLS: both are stated, with the
RFC's own words, under [the transport](#transport).

## The worker's path map

In mapped mode `worker_path_map` translates a source path as the server names it into the path
the worker reads it at. A source no entry covers is **refused, never passed through**: a path handed back
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
  after it has been exceeded will not extend it". `ServeContent` "handles Range requests
  properly, sets the MIME type, and handles If-Match, If-Unmodified-Since, If-None-Match,
  If-Modified-Since, and If-Range requests", and its Content-Type rule is quoted in full under
  [the source stream](#http-mode). `ResponseController.SetWriteDeadline` "sets the deadline for
  writing the response. Writes to the response body after the deadline has been exceeded will
  not block, but may succeed if the data has been buffered", and "Setting the write deadline
  after it has been exceeded will not extend it". `Server.ServeTLS` and
  `Server.ListenAndServeTLS` need certificate and key files only "if neither the Server's
  TLSConfig.Certificates nor TLSConfig.GetCertificate are populated" (`ServeTLS` adds
  `config.GetConfigForClient` to that list). `TrailerPrefix` "is a magic prefix for
  ResponseWriter.Header map keys that, if present, signals that the map entry is actually for
  the response trailers"; a client reads them from `Response.Trailer`, which "After Body.Read has
  returned io.EOF ... will contain any trailer values sent by the server"; and `Request.Trailer`
  carries the caution "Few HTTP clients, servers, or proxies support HTTP trailers".
- `crypto/tls`, from `go doc` of the same toolchain (https://pkg.go.dev/crypto/tls):
  `X509KeyPair` "parses a public/private key pair from a pair of PEM encoded data";
  `Config.MinVersion`: "By default, TLS 1.2 is currently used as the minimum. TLS 1.0 is the
  minimum supported by this package. The server-side default can be reverted to TLS 1.0 by
  including the value "tls10server=1" in the GODEBUG environment variable."
- The approved proposal this implements: [`.claude/goals/2026-09-holdfast-research/proposal-node-protocol.md`](https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/2026-09-holdfast-research/proposal-node-protocol.md)
  (option (a): rules 1 to 9 and 11, and rule 10's TLS stance).
