# Design rationale

The index of the design rules: each rule named once, linked to the anchored document that
holds its argument.

One argument, one home, each anchored and each named here by its rule only - the
reasoning lives in the document, not in this index.

- **No source is mutated until a replacement has passed every gate** -
  [`docs/design/swap.md`](swap.md#swap-invariant).
- **The quality gate bounds the worst frame, not only the average** -
  [`docs/design/quality-gate.md`](quality-gate.md#vmaf-pooling).
- **An unreadable figure is reported as null, never as a zero** -
  [`docs/design/ledger-totals.md`](ledger-totals.md#null-is-not-zero).
- **Every job's encode is declared once, and the command line and every gate read that one
  plan** - [`docs/design/encode-plan.md`](encode-plan.md#encode-plan).
- **An output replaces its source only when it carries the bit depth, chroma, colour tags and
  HDR10 metadata its plan declares** - [`docs/design/encode-plan.md`](encode-plan.md#fidelity).
- **A hardware encoder runs only after a real encode through a job's own command line came out
  faithful at each bit depth** - [`docs/design/hardware.md`](hardware.md#probe).
- **A job whose hardware is missing or fails is encoded by nothing else unless its root says
  `hw_fallback: software`** - [`docs/design/hardware.md`](hardware.md#fallback).
- **A hardware decode hands the filters, the encoder and every gate the frames a software decode
  would** - [`docs/design/hardware.md`](hardware.md#decode).
- **An audio track is re-encoded only on request, only from a lossless codec, into a layout the
  codec carries as declared** - [`docs/design/audio.md`](audio.md#reencode).
- **Loudness runs in two passes to EBU R 128, resampled to the declared rate, its mode read from the
  encoder's report** - [`docs/design/audio.md`](audio.md#loudness).
- **A transformed audio track replaces nothing until its length, layout, rate and loudness match its
  plan and every audio stream decodes** - [`docs/design/audio.md`](audio.md#audio-gates).
- **A subtitle sidecar is published only after the swap commits and its parse-back gate passes, and never over an existing file** - [`docs/design/subtitles.md`](subtitles.md#sidecars).
- **A picture is cropped only where spread samples agree on its bars and the area removed is black on every frame; a Dolby Vision picture only to its RPU's own active area, zeroed and gated** - [`docs/design/crop.md`](crop.md#crop).
- **Dynamic HDR is carried only through libx265, and its output replaces the source only when its DOVI record and every frame's RPU and HDR10+ match its plan** - [`docs/design/dynamic-hdr.md`](dynamic-hdr.md#dynamic-hdr).
- **The queue decides only the order files are offered in: priority, then the declared order, never which files** - [`docs/design/queue-order.md`](queue-order.md#queue-order).
- **The health sweep reads every source and reports; it never moves, renames, deletes or repairs a file** - [`docs/design/health-sweep.md`](health-sweep.md#health-sweep).
- **A media server is told about a swap only after it has committed, once, by directory; and a file being played is held, which only ever delays** - [`docs/design/media-clients.md`](media-clients.md#media-clients).
- **A node's output is only ever a candidate: it lands in a working file the server named, on a live lease at the current epoch, with the declared length and digest, and the server's own gates and rename decide** - [`docs/design/nodes.md`](nodes.md#leases).
- **A worker speaks to its server over TLS or to loopback; plain HTTP to any other host is refused unless the operator wrote `worker_insecure_http: true`, which is logged at every start; and a node's source is streamed only on a live lease, with its sha-256 compared on the server before any gate** - [`docs/design/nodes.md`](nodes.md#transport).
- **The web UI is built by one pinned toolchain, embedded in the binary, and served at `/` only to a request that asks for HTML; every root response carries the source offer; its token lives in a variable of the running page and nowhere else** - [`docs/design/web-ui.md`](web-ui.md#embed), [`#token`](web-ui.md#token).
- **An arr's webhook is authenticated by a credential that can only queue; both Sonarr Download shapes and Radarr's are read; each file goes through the targeted scan; an unrecognised shape queues nothing** - [`docs/design/media-clients.md`](media-clients.md#webhook-intake).
