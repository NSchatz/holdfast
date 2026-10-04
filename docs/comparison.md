# The tools that work this ground

*Every claim about another tool on this page was checked **as of September 2026**, against that
project's own licence text or project page. Other tools move: re-check before you choose.*

## We are not the only tool that verifies before it replaces

[**Alchemist**](https://github.com/bybrooklyn/alchemist) (AGPL-3.0, Rust) works the same axis: it
"never overwrites anything until the new file passes its quality checks", and it ships its own
*Migrate from Tdarr* guide. If you are choosing between us, choose on the difference, not on a claim of
uniqueness we would not be able to defend - and the difference does not run one way.

**Where Alchemist is ahead.** Four capabilities, one that holdfast does not have and three it only
half has, said plainly rather than left out:

- **A Jellyfin integration** - a narrowed plugin token for enqueue, completion events, job details and
  library refresh. holdfast ships nothing of the kind for Jellyfin: its media-server clients are
  Radarr, Sonarr and Plex, each off until configured
  ([post-swap-hook.md](post-swap-hook.md), [design/media-clients.md](design/media-clients.md#play-hold)).
- **Audio stream rules** - commentary stripping, language filtering, default-track retention. holdfast
  half has this: a root keeps audio and subtitle streams by language and can drop the tracks the
  container marks as commentary (`audio_languages`, `subtitle_languages`, `keep_commentary` -
  [profiles.md](profiles.md#stream-selection)), and has no rule about which track is the default.
  What it keeps is stream-copied unless a root asks otherwise: a lossless track can be re-encoded, a
  stereo track added beside a surround one, and both normalised to EBU R 128, every one of those keys
  off by default and each transformed track behind gates of its own
  ([profiles.md](profiles.md#audio), [design/audio.md](design/audio.md#audio-gates)).
- **Named API tokens with access classes** - read-only, webhook, plugin, full access. holdfast half has
  this: four credentials at four access levels, `server_read_token` for the read endpoints,
  `server_auth_token` for control, `webhook_token`, which may only enqueue a file through the
  Sonarr/Radarr webhook intake, and `node_token`, which may only lease and upload as a worker node
  ([api-reference.md](api-reference.md)). They are four fixed keys, not named tokens an operator
  mints, and there is no plugin class.
- **Automatic hardware selection with CPU fallback** across NVIDIA, Intel, AMD and Apple. holdfast half
  has this, and deliberately no more by default: `encoder: auto` chooses per job among the NVIDIA,
  Intel and AMD HEVC encoders that passed a real encode on this host at start
  ([design/hardware.md](design/hardware.md#auto)), it is opt-in, and a job whose hardware is missing
  or fails is left alone unless its root says `hw_fallback: software`
  ([design/hardware.md](design/hardware.md#fallback)). It has no Apple encoder.

**Two capabilities holdfast has since built.** This page used to list both as Alchemist's lead. What
holdfast ships is described here, and how it compares with Alchemist's is the reader's to judge:

- **Per-library profiles**, giving movies, TV and home videos different behaviour per library. In
  holdfast a library root carries its own encoder, gates, stream selection, audio, crop, subtitle
  sidecars, queue priority and watch, and a resolution band inside it or a path glob overrides part
  of that ([profiles.md](profiles.md)).
- **An off-peak scheduler with a priority queue.** holdfast has a daily `run_window`, a per-core load
  cap, a `queue_order` (path, largest, smallest, newest, oldest, or the most bytes saved per hour of
  work) and a `priority` on a root, a rule or a profile that orders ahead of it
  ([design/queue-order.md](design/queue-order.md#priority)).

Both tools take a **Sonarr/Radarr webhook** behind a narrowed token with container path translations:
holdfast's is the native intake in [docker.md](docker.md#telling-holdfast-about-one-file-sonarr--radarr).

**Two more tools work this ground.** Both are described from their own project pages and nothing else -
no ranking, no popularity, no weight class, because no source this project could obtain carries one.

[**FileFlows**](https://fileflows.com/) designs, schedules and runs automated file-processing pipelines
from a single server up to a distributed cluster, offloading tasks to multiple nodes, and transcodes to
AV1, HEVC or H.264 with hardware acceleration, VMAF-optimized encoding and Dolby Vision support. Its own
site offers a free tier and carries a pricing page, so "free" there names a tier and not the product.

[**Unmanic**](https://github.com/Unmanic/unmanic) (GPL-3.0, Python, plugin-based, with a web UI) calls
itself a library optimiser: it converts a library into a single uniform format, manages file movements
based on timestamps, and runs custom commands against a file based on its size. It monitors files and
directories, so a modified or newly added file is tested against its configured presets again.

Where those pages name a web UI, nodes, hardware encoding or Dolby Vision, holdfast has its own answer
to each, described here from holdfast's side only and left for the reader to compare: an embedded web
UI that shows and drives only what the JSON API offers ([design/web-ui.md](design/web-ui.md#views)), worker nodes whose output is only ever a
candidate the server's own gates decide ([design/nodes.md](design/nodes.md#leases)), hardware
encoders that run only after a real encode came out faithful
([design/hardware.md](design/hardware.md#probe)), Dolby Vision profile 8.1 and HDR10+ carried through
libx265 ([design/dynamic-hdr.md](design/dynamic-hdr.md#dynamic-hdr)), and a report-only library
health sweep ([design/health-sweep.md](design/health-sweep.md#health-sweep)).

<a id="differentiator-gate"></a>

**The difference is where the default sits, and it is a claim about holdfast alone.** holdfast's verify
gate is **default-on**, **layered** and **fails closed**. Layered means every layer runs rather than the
first one that answers: structural parity (codec, duration, packets, per-type stream counts,
strictly-smaller), output fidelity (the bit depth, chroma, colour tags and HDR10 metadata the job's
plan declares - [design/encode-plan.md](design/encode-plan.md#fidelity)), full decode-integrity, and
three VMAF floors - the mean (`min_vmaf`), the worst
frame (`vmaf_min_pool`) and chroma (`vmaf_min_chroma`, which the luma-only VMAF model cannot see at
all). Fails closed means an output that cannot be measured is rejected rather than assumed good: an
ffmpeg without libvmaf stops the tool instead of quietly downgrading the gate, and a score that could
not be produced is never read as a score that passed. Every transformation holdfast makes on request -
audio, crop, subtitle sidecars, dynamic HDR, an encode on a worker node - adds checks of its own in front
of the same swap and removes none. That is the whole claim, and it is narrower and
truer than "the only one that checks".
