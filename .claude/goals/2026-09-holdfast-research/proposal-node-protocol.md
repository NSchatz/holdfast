# P4 - The worker-node protocol (T46)

Proposal for Checkpoint T. Research: `research-nodes-clients-spa.md` section 1 and its T46 sketch,
and `verify-nodes-clients-spa.md` (claim 10; its corrections override the research). Every claim a
version, licence, status code or API rests on was re-checked on 2026-09-29 against a primary
source; see "Claims re-verified". Code references are at `30d245f`. A value marked `ASSUMED` is a
proposed default nobody has measured; it stays marked in code and docs until a report replaces it.

## What this decides

The owner already decided the shape (T16, T17, T23). This proposal decides what T46 left open:

1. **Transport:** HTTP+JSON on the existing chi server, gRPC, or a persistent WebSocket.
2. **Lease model:** TTL, heartbeat, the fencing token that makes a stale worker's upload
   refusable, and the bound that ends a poison job.
3. **Verification on arrival:** digests of the source as leased and of the output, size limits,
   and where the uploaded bytes land.
4. **Uploads:** resumable or restart-from-zero.
5. **Backpressure:** per-node and global caps, and the free-space reservation.
6. **TLS stance:** built-in TLS files, a reverse proxy, plain HTTP, and what the docs must warn.
7. **Failure handling:** worker crash, server restart mid-lease, duplicate upload.

## Where things stand

**Decided (not re-opened here):**

- T16: workers only encode; the server that owns the library re-runs every gate and performs the
  same-filesystem rename itself. The invariant text is unchanged.
- T17: per node, either a shared mount with a path map, or the server streams the source and
  receives the output over HTTP.
- T23: a new `node_token` joins `config.SecretBearingKeys`, by reference; it can lease and upload,
  never control. No mTLS, no user login; the `127.0.0.1` default bind stays.
- T49: no goal calls a real worker node; everything here is proven on loopback and `httptest`.
- I5: nodes are OFF until configured, so an existing config serves the same surface as today.

**Open (T46, decided here at Checkpoint T):** the transport, the lease and fencing contract, the
digest and size rules, resumability, backpressure, the TLS stance, and the failure rules listed
under "What this decides". Goal 11 builds the protocol and shared-mount mode on the approved
answer; goal 12 builds HTTP streaming, TLS and the deployment docs.

**The code this lands in:**

- Router: `internal/server/server.go:120-192`. `/api` carries the schema document outside every
  token group (`:132`), a read group behind `requireReadToken` (`:142-148`) and a control group
  behind `requireToken` (`:158-173`); `/metrics` (`:183-185`) and `/` (`:190`) are ungated. Both
  gates compare a bearer token in constant time and never echo it (`:422-439`, `:460-482`). A
  third group fits this shape without touching the other two.
- Listener: `cmd/holdfast/main.go:1114` builds `http.Server{Addr, Handler, ReadHeaderTimeout: 10s}`
  and `:1130` calls `ListenAndServe`. There is no TLS code under `cmd/` or `internal/server/`;
  `docs/docker.md:145` treats a reverse proxy as a decision the operator makes.
- Credentials: `internal/config/config.go:633` is the closed `SecretBearingKeys` list (four keys),
  and `SecretRefs` (`:642-653`) reads a parallel slice of raw values, so a new key is added in
  both places; the AC-7 suite in `cmd/holdfast` enumerates the list, so a key added there is graded
  by the literal-refusal cases automatically. The default bind is `:41`, and `:1616-1640` already
  prints a startup notice when `server_addr` is not loopback and `server_read_token` is empty.
- Claims: `internal/store/sqlite.go:239-271` hands a job to exactly one claimant inside one
  transaction, and `Claim` already takes a `worker` string.
- Free space: `internal/engine/source_room.go:83-125` refuses a beside-the-source job that cannot
  fit and holds its size in an in-memory account released on every way out of the job. A failed
  lookup does not fail the job (`:93-95`). The account is per process, so it is empty after a
  restart.
- The swap: `internal/engine/engine.go:36` is the temp-name construction marker, `:2256` fsyncs
  the temp before the rename, `:2371` is the rename and `:2379` fsyncs the parent directory
  after it; `internal/engine/swap.go:65-67` (`tempPath`) is the whole of the temp-name
  construction the startup sweep matches; `docs/design/swap.md:11-20` is the
  invariant and `:36-43` the rule that where the encode is written may move while the swap shape
  does not.
- The statement this reverses: `README.md:113` and `docs/migration.md:120-131` (R2, rewritten by
  goal 12).

### How the incumbents do it (re-verified today)

| | Transport | Media access | Result return | Lease or verification contract |
|---|---|---|---|---|
| Tdarr | The node opens a persistent Socket.IO connection (WebSocket or HTTP polling) to port `8266`; "The Node initiates this connection and does not require an inbound port"; troubleshooting asks for the same version on both | "Mapped" nodes share the file system or mapped shares with path translators; "Unmapped" nodes download and upload working files through the server's network API, and without Pro an unmapped video file is capped at 10 MB | The unmapped node uploads the new file to the server | None documented |
| FileFlows (nodes are now "Agents") | Three modes: internal; SignalR over WebSockets, "the default and recommended connection mode for external Agents"; and Polling over HTTP/REST, "only if you experience issues with the SignalR connection" | A mapping rewrites the server path to an agent path and normalises separators; "Mappings only work if the Agent can access the server's files" | Not read today | None documented |
| Unmanic (linked installations) | HTTP API between installations; tasks routed by identical library name | Shared storage is read in place, otherwise the file is uploaded through the API | The remote first tries to move the result into a temp directory beside the original on shared storage, otherwise the origin downloads it over HTTP | None documented; issue #635 (open, one report, no maintainer reply) describes the origin receiving a ~55-byte HTTP 404 body instead of the finished file |

Lessons: the worker opens the connection (no inbound port on the node); an ordered path-prefix map
is the proven shared-mount UX; nobody publishes a lease or fencing contract, so holdfast defines
one; and #635 is the argument for never treating a response body as media on its status alone.
Two of three incumbents default to a push channel (Socket.IO, SignalR), and both still need a
separate path for bulk bytes and gain nothing a lease does not also need: a dropped socket does not
prove the worker stopped encoding.

## Options

### (a) HTTP+JSON leases on the existing chi server; `holdfast worker` pulls

A third route group, `/api/node/v1/...`, behind a `requireNodeToken` middleware; `holdfast worker`
polls it (long-poll on acquire), heartbeats, streams or reads the source, encodes, uploads, and
reports.

- **Costs:** the lease state machine, retry and backoff are written by hand (they are needed in
  every option). Disconnect detection is bounded by the lease TTL, not instant. Long-poll handlers
  must watch the server's base context the way `/api/events` does, or graceful drain waits on them.
- **Gains:** no new dependency (`net/http`, chi, `crypto/sha256`); reuses bearer auth, the schema
  document and `api-schema-diff`, `httptest` fakes and curl; one API style for the UI (T18) and for
  nodes; works through an ordinary HTTP/1.1 reverse proxy, whose own request-body limit must admit
  an output-sized upload (a line the goal-12 deployment docs carry).

### (b) gRPC (bidi stream for leases, client stream for upload)

- **Costs:** `google.golang.org/grpc` (v1.84.0, Apache-2.0, compatible with AGPL-3.0 for
  distribution; the compatibility is `ASSUMED`, standard knowledge) plus protobuf code generation
  and its toolchain, pinned in the Makefile and `check-pins.sh`. `Server.ServeHTTP` requires the
  request to arrive on HTTP/2, "practically this means that the Request must also have arrived over
  TLS" with the standard library server, and it is marked EXPERIMENTAL and "does not support some
  gRPC features". Go 1.24 added unencrypted HTTP/2 to `net/http` (`Protocols.SetUnencryptedHTTP2`),
  which removes the TLS-or-second-port constraint on the Go side but not the experimental handler.
  Reverse proxies must carry HTTP/2 end to end; not curl-able; `api-schema-diff` does not cover it;
  the UI still needs JSON, so there would be two API styles.
- **Gains:** a typed schema and stream-close disconnect signals.

### (c) Persistent WebSocket (the Tdarr and FileFlows default) plus HTTP for the bytes

- **Costs:** the standard library has no WebSocket; `golang.org/x/net/websocket` says it "lacks some
  features found in an alternative and more actively maintained WebSocket packages" and names
  `gorilla/websocket` and `coder/websocket`, so a new pinned dependency. Reconnection and replay
  semantics; proxies must allow the upgrade; leases are still required because a socket drop proves
  nothing about the worker's ffmpeg.
- **Gains:** push assignment and a faster disconnect signal.

### (d) An external broker (NATS, Redis)

Rejected: a second service breaks "a stranger can `docker run` it".

### Sub-option: resumable uploads

A failed upload either restarts from zero within the same lease, or resumes. The IETF resumable
upload protocol is an active Internet-Draft (`draft-ietf-httpbis-resumable-upload-12`, revised
2026-07-06, expires 2027-01-07), not an RFC; tus is the LEAD alternative. Cost of resuming:
per-upload offset state on the server, a draft that may still change, and more fixtures. Outputs
are strictly smaller than their sources, so a restart costs at most one source-sized transfer.

## Recommendation

**Option (a), HTTP+JSON leases on the existing chi server with a `holdfast worker` subcommand,
uploads restart from zero (not resumable) in v1, under the rules below.** Defaults marked `ASSUMED`
are unmeasured and stay marked until a report replaces them.

1. **Surface and auth.** One new group, `/api/node/v1`, behind `requireNodeToken`, which accepts
   ONLY `node_token` (constant time, never echoed). The read and control groups do not accept
   `node_token`, and `requireNodeToken` accepts neither the read nor the control token, so a stolen
   node token can lease and upload but cannot pause, exclude, search or scan, and no token opens two
   roles. With `node_token` unset the group answers 403 naming the key, the way control does today.
   No node endpoint restores, requeues, resolves or re-opens anything: those stay local commands.
   `node_token` is by reference (`file:` or `cmd:`), joins `config.SecretBearingKeys`, and a literal
   refuses start; the worker reads its own `node_token` the same way. Endpoints (JSON bodies):
   - `POST /api/node/v1/leases` - acquire. Body: node name, build version, free slots, mode
     (`mapped` or `http`). `200` with `lease_id`, `epoch`, `ttl_sec`, `heartbeat_sec`, the job's
     encode plan (goal 3), the source's size and mtime, the path or stream location, and
     `max_output_bytes`; `204` with `Retry-After` when there is no work (a bounded long-poll first,
     30 s `ASSUMED`); `503` with `Retry-After` when a cap or the reservation refuses; `409` with a
     typed reason when the worker's version is not the server's.
   - `POST /api/node/v1/leases/{id}/heartbeat` with `epoch` and progress: `200` with the renewed TTL,
     or `410`.
   - `GET /api/node/v1/leases/{id}/source` (http mode): served with `http.ServeContent`, which
     handles `Range`.
   - `PUT /api/node/v1/leases/{id}/output` with `Content-Length`, `Content-Digest` and the epoch.
   - `POST /api/node/v1/leases/{id}/complete` (output digest, source digest, size, encode stats) and
     `POST /api/node/v1/leases/{id}/fail` (typed reason).
   - Versioning: `v1` in the path; the worker and server must run the same holdfast version in v1
     (Tdarr's troubleshooting guide asks for the same version on both), answered `409` with both
     versions named. Not `426`: RFC 9110 defines 426 as a protocol upgrade and requires an
     `Upgrade` header naming the protocol.
2. **Leases are durable rows** in `internal/store`: lease id, job key, node, `epoch`, expiry, state,
   the reserved byte count, and the recorded digests. `epoch` is the **fencing token**: it rises by
   one at every grant of the same job, and every call carries lease id and epoch. The server checks
   both against the current row inside the same transaction that acts on the call; an expired lease
   or a lower epoch gets `410 Gone` and its bytes are discarded, never written into a library
   directory, so a stale worker can never contribute an output to a swap. Expiry is judged by the
   server's clock only. TTL 60 s and heartbeat every 15 s (TTL/4) are `ASSUMED`.
3. **Retry bound.** An expiry, a `fail`, or a refused upload increments the job's attempt count
   through the existing `max_failures` machinery (`Claim` already takes it); at the bound (3,
   `ASSUMED`) the job SKIPS with a logged reason, so a poison job cannot loop across nodes.
4. **Where the output lands.** The server names it, never the worker: a temp in the source's own
   directory built by the engine's own construction (`tempPath`) and recorded on the lease row, so
   the existing startup sweep covers a partial upload exactly as it covers a killed local encode;
   or inside `scratch_dir` when that is set, after which the existing copy-beside-and-
   prove-identical step runs. The worker only sends bytes. The server then re-runs every gate
   against its own copy of the source and performs the existing durable rename (T16), so the swap
   shape, the rewritten-mid-encode guard and the `fsclass` classification of the source directory
   are exactly today's. In mapped mode the worker reads the source through the mount and still
   uploads the output over HTTP, so the server stays the only writer into library directories; a
   worker writing into the mapped source directory is not offered (it widens who writes into the
   library, and NFS close-to-open semantics would force a re-hash anyway).
5. **Digests (sha-256, RFC 9530).** Output: the worker sends `Content-Digest: sha-256=:<base64>:` as
   a request HEADER (it has the whole file before sending, so no trailer is needed); the server
   hashes while writing, fsyncs, and admits the file only when the length equals `Content-Length`
   and the digest matches. Source, http mode: the server hashes the bytes it streams on a full,
   unranged GET and records the figure on the lease; it MAY also send `Repr-Digest` as a trailer so
   the worker can stop before encoding, but the trailer is never load-bearing, because Go's own docs
   say "Few HTTP clients, servers, or proxies support HTTP trailers". The worker always reports the
   sha-256 of the bytes it read in `complete`, and the server compares. After a ranged (resumed)
   download, or in mapped mode, the server hashes its own copy of the source once before the gates
   (one extra sequential read) and compares with the worker's figure, which catches a wrong path
   map or a stale network-mount cache. Digests are transport integrity only: the gates remain the
   fidelity proof, and RFC 9530 section 6.1 says an on-path attacker can simply replace a digest, so
   digests are not a substitute for TLS.
6. **The #635 class is closed at both ends.** The worker treats a source response as media only on
   `200` or `206` with the expected length and digest. The server admits an upload only on a live
   lease with a matching epoch, a declared `Content-Length`, a matching digest, and a size below
   `max_output_bytes`, and even then it is only a candidate for the gates.
7. **Size limits.** `Content-Length` is required (`411` otherwise). The body is wrapped in
   `http.MaxBytesReader`, which returns `*http.MaxBytesError` past the limit and asks the server to
   close the connection; the limit is the declared length, itself capped at source size minus one,
   since gate 4 accepts only a strictly smaller output (`413` above it). The listener sets only
   `ReadHeaderTimeout`, so the upload handler sets a per-request read deadline with
   `http.ResponseController.SetReadDeadline` (Go 1.20+) sized from the declared length and a
   minimum rate (`ASSUMED`), so a stalled upload cannot hold a reservation forever.
8. **Backpressure.** The worker declares `slots` (default 1). The server caps leases per node
   (`node_max_leases_per_node`) and concurrent source streams plus uploads across all nodes
   (`node_max_transfers`), and it refuses a grant while its own gate queue is full, because every
   node output costs the server a full decode-integrity pass and a VMAF run (a cost to size the
   server for, `ASSUMED` a meaningful fraction of encode time). A grant takes the existing
   free-space hold for the source's directory (`sourceRoomFor`) as a reservation; the upload
   re-checks it once `Content-Length` is known. Refusals are `503` or `204` with `Retry-After`; the
   worker backs off exponentially with jitter (5 s to 5 min, `ASSUMED`).
9. **Failure handling.**
   - Worker crash: heartbeats stop, the lease expires after one TTL, the server deletes only the temp
     it named for that lease, releases the reservation, and requeues with attempt+1.
   - Server restart mid-lease: live lease rows survive. At start, before any new grant, the server
     re-takes the free-space holds of live leases (the in-memory account starts empty), leaves every
     temp a lease recorded to the existing startup sweep exactly as it treats a killed local
     encode's temp (an upload admitted but not yet gated is re-run from its lease, never gated
     after the restart), and gives each live lease one TTL of grace; a heartbeat inside the grace
     continues the lease, and after it the lease expires as above.
   - Duplicate upload: the upload is keyed by lease id and epoch. A repeat `PUT` or `complete` with
     the recorded digest answers `200` with the recorded result and rewrites nothing; a different
     digest for an accepted lease answers `409`; a mismatch on first arrival deletes the temp and
     answers a typed `digest_mismatch`, and the worker may retry the `PUT` within the same lease a
     bounded number of times.
   - Upload during expiry: the handler re-checks the lease and epoch in the transaction that admits
     the file; a lease that expired mid-body is `410` and the temp is deleted.
10. **TLS stance.** Optional built-in TLS with `server_tls_cert` (a plain path) and `server_tls_key`
    (by reference, joining `config.SecretBearingKeys`, as section 4 of the brief requires for a TLS
    key file): the resolved key and the certificate become `tls.X509KeyPair` in the server's
    `TLSConfig.Certificates`, and `ServeTLS` needs no file paths once that is populated; or a
    reverse proxy. The worker refuses a plain `http://` server URL unless the host is loopback or
    `worker_insecure_http: true` is set, which is logged loudly at every start. The docs (goal 12)
    must warn, in `docs/docker.md` and `docs/design/nodes.md`: over plain HTTP the `node_token` crosses the network in cleartext on
    every request; whoever captures it can lease jobs and upload outputs (which still face every
    gate, but burn server CPU and reservations) and, in http mode, read the library's media; digests
    do not help against that attacker; and nodes need an explicit non-loopback `server_addr`, which
    without `server_read_token` serves every media path in the read API to that network (the
    existing startup notice says so), so the worker deployment sets `server_read_token` too. No
    mTLS (T23).
11. **Worker capability.** The worker reports the encoders its own startup probe found; the server
    leases a job only to a worker that reports the job's encoder, and the worker refuses a plan it
    cannot execute with a typed `fail`, never a guess.

Why not (b) or (c): both add a pinned dependency and a second API style for no safety gain, since
the lease, the fencing epoch and the digests are needed in every option, and the push channel that
(b) and (c) buy only shortens disconnect detection, which the TTL already bounds.

## Test plan

All fakes, loopback and synthetic media; never a live node, never a GPU (T9, T49). The package is
`internal/node`, inside the mutation domain at the 70% floor.

- **Unit, pure:** the lease state machine (grant, renew, expire, re-grant with a higher epoch,
  complete, fail, skip at the bound) against a fake clock; the path map (ordered prefixes, longest
  match, no match refuses); the size cap arithmetic.
- **`httptest` server with a fake worker and a temp SQLite store,** one fixture each (goal 11's list
  first):
  - an upload on an expired lease answers `410` and leaves no file in the source directory;
  - a stale worker (lower epoch after a re-grant) answers `410`;
  - a duplicate upload with the same digest answers `200`, and the temp's inode and mtime are
    unchanged; a different digest after acceptance answers `409`;
  - a digest mismatch is refused and the temp deleted;
  - a 404 body offered as media: a fake source endpoint returns `404` with a 55-byte body and the
    worker fails the lease; a 55-byte `PUT` whose digest was computed over the real output is
    refused;
  - a free-space reservation refusal at grant, and again at upload start;
  - a server restart with live leases: close and reopen the store, the holds are re-taken, a
    heartbeat inside the grace continues, one after it answers `410`;
  - an oversized upload answers `413` through `http.MaxBytesReader`; a missing `Content-Length`
    answers `411`;
  - backpressure: per-node and global caps answer `503` with `Retry-After`; the long-poll returns
    when the base context is cancelled.
- **Auth:** `node_token` on every control and read endpoint is refused; the control and read tokens
  on node endpoints are refused; a literal `node_token` refuses start (the AC-7 suite picks the key
  up from `SecretBearingKeys`); a literal `server_tls_key` likewise.
- **TLS:** `httptest.NewTLSServer` with the worker trusting its certificate; the worker refuses a
  non-loopback `http://` URL unless `worker_insecure_http` is set, and logs the override.
- **End to end (goal 11 line C, goal 12 line C):** a real tiny lavfi encode under the heavy lock,
  once over a shared mount with a path map and once in http mode, with the server re-running every
  gate and doing the rename.
- **Surface:** `api-schema-diff` stays green with `.api-schema-breaks.yaml` `[]` (additions only).

## Claims re-verified

| Claim | Source (read 2026-09-29) | Outcome |
|---|---|---|
| Tdarr nodes open a persistent Socket.IO connection (WebSocket or HTTP polling) to port 8266; the node initiates it and needs no inbound port | https://docs.tdarr.io/docs/troubleshooting/ | confirmed |
| Tdarr's troubleshooting guide asks that node and server run the same version | https://docs.tdarr.io/docs/troubleshooting/ | confirmed |
| Tdarr mapped vs unmapped nodes; unmapped transfers go through the server's network API; "It's best to have authentication enabled" | https://docs.tdarr.io/docs/nodes/nodes | confirmed |
| Tdarr without Pro caps unmapped video files at 10 MB (research: LEAD from a secondary guide) | https://docs.tdarr.io/docs/nodes/nodes | confirmed on the primary page (also 0.1 MB for other files) |
| Tdarr path translator config shape `{"server": ..., "node": ...}` | https://docs.tdarr.io/docs/nodes/nodes | not confirmed (the page names path translators without a shape); stays LEAD, nothing here relies on it |
| FileFlows nodes talk HTTP REST and poll for work (research: ASSUMED) | https://fileflows.com/docs/webconsole/agents/connection-modes | corrected: the default and recommended mode for external Agents is SignalR over WebSockets; HTTP/REST polling is a fallback; nodes are now called Agents |
| FileFlows maps server paths to node paths and normalises separators (research: LEAD, docs host did not resolve) | https://fileflows.com/docs/webconsole/agents/mapping | confirmed on the moved docs; `docs.fileflows.com` still does not resolve |
| Unmanic routes by library name, reads shared storage in place, else uploads through the API, and results come back over HTTP | https://docs.unmanic.app/docs/configuration/linking/link_overview/ | confirmed; addition: the remote first tries to move the result into a temp directory beside the original on shared storage |
| Unmanic issue #635: a ~55-byte HTTP 404 body received instead of the finished file | https://github.com/Unmanic/unmanic/issues/635 (via the GitHub API) | confirmed: open, 0 comments, a single report |
| gRPC-Go is Apache-2.0 (research: ASSUMED) | https://github.com/grpc/grpc-go (licence API; latest release v1.84.0, 2026-09-17) | confirmed |
| gRPC `ServeHTTP` needs HTTP/2, practically TLS with the standard server, and is EXPERIMENTAL | https://github.com/grpc/grpc-go/blob/v1.84.0/server.go | confirmed |
| gRPC "does not mount on the chi mux without h2c or a second port" (research) | https://pkg.go.dev/net/http (`Protocols`, `SetUnencryptedHTTP2` in `api/go1.24.txt` of Go 1.25.14) | corrected in part: unencrypted HTTP/2 is in the standard library since Go 1.24; the handler is still experimental and HTTP/2-only |
| The Go standard library has no WebSocket; `x/net/websocket` lacks features and names gorilla and coder | https://pkg.go.dev/golang.org/x/net/websocket (and `websocket/websocket.go` in https://github.com/golang/net) | confirmed, quoted verbatim |
| `http.MaxBytesReader` returns `*MaxBytesError` past the limit and tells the writer to close the connection | https://pkg.go.dev/net/http (and `go doc` of Go 1.25.14) | confirmed |
| `ResponseController.SetReadDeadline` exists since Go 1.20 | https://pkg.go.dev/net/http (`api/go1.20.txt`) | confirmed |
| `http.ServeContent` handles `Range` requests | https://pkg.go.dev/net/http | confirmed |
| A `Repr-Digest` trailer on the source stream as the digest carrier (research design) | https://pkg.go.dev/net/http (`Request.Trailer`: "Few HTTP clients, servers, or proxies support HTTP trailers") | corrected: allowed by RFC 9530 but not load-bearing; the server-side comparison in `complete` is |
| `Content-Digest` and `Repr-Digest`, sendable as trailers; sha-256 and sha-512 are the Active algorithms | https://www.rfc-editor.org/rfc/rfc9530.html | confirmed (Standards Track, February 2024) |
| Digests are no protection against an on-path attacker | https://www.rfc-editor.org/rfc/rfc9530.html (section 6.1) | confirmed |
| `426` for a worker version mismatch (research) | https://www.rfc-editor.org/rfc/rfc9110.html (426 requires an `Upgrade` header naming a protocol) | refuted as a fit; `409` with a typed reason instead |
| `410 Gone` for an expired lease; `409` for a conflicting state | https://www.rfc-editor.org/rfc/rfc9110.html | confirmed as fits |
| The resumable upload protocol is a draft, not an RFC | https://datatracker.ietf.org/doc/draft-ietf-httpbis-resumable-upload/ | confirmed: active draft -12, revised 2026-07-06, expires 2027-01-07 |
| holdfast has no TLS code; the listener sets only `ReadHeaderTimeout` | `cmd/holdfast/main.go:1114`, `:1130` at `30d245f` | confirmed |
| `ServeTLS` needs no certificate or key file when `TLSConfig.Certificates` is populated | https://pkg.go.dev/net/http (`Server.ServeTLS`; `go doc` of Go 1.25.14) | confirmed |

## Sources

- https://docs.tdarr.io/docs/troubleshooting/ (read 2026-09-29)
- https://docs.tdarr.io/docs/nodes/nodes (read 2026-09-29)
- https://fileflows.com/docs/webconsole/agents/connection-modes (read 2026-09-29)
- https://fileflows.com/docs/webconsole/agents/mapping (read 2026-09-29)
- https://docs.unmanic.app/docs/configuration/linking/link_overview/ (read 2026-09-29)
- https://github.com/Unmanic/unmanic/issues/635 (read 2026-09-29)
- https://github.com/grpc/grpc-go/blob/v1.84.0/server.go and the repository licence (read 2026-09-29)
- https://pkg.go.dev/net/http (read 2026-09-29; cross-checked with `go doc` and the `api/` files of
  the Go 1.25.14 toolchain)
- https://pkg.go.dev/golang.org/x/net/websocket (read 2026-09-29)
- https://www.rfc-editor.org/rfc/rfc9530.html (read 2026-09-29)
- https://www.rfc-editor.org/rfc/rfc9110.html (read 2026-09-29)
- https://datatracker.ietf.org/doc/draft-ietf-httpbis-resumable-upload/ (read 2026-09-29)
- LEAD: tus (https://tus.io/), named only as an alternative, not read today
