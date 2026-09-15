# The tools that work this ground

*Every claim about another tool on this page was checked **as of September 2026**, against that
project's own licence text or project page. Other tools move: re-check before you choose.*

## We are not the only tool that verifies before it replaces

[**Alchemist**](https://github.com/bybrooklyn/alchemist) (AGPL-3.0, Rust) works the same axis: it
"never overwrites anything until the new file passes its quality checks", and it ships its own
*Migrate from Tdarr* guide. If you are choosing between us, choose on the difference, not on a claim of
uniqueness we would not be able to defend - and the difference does not run one way.

**Where Alchemist is ahead.** Seven capabilities it has and holdfast does not, two of them capabilities
holdfast only half has, said plainly rather than left out:

- **Per-library profiles**, giving movies, TV and home videos different behaviour per library. holdfast
  half has this: a library root or a path glob overrides the encode settings and the gates, and no
  more ([profiles.md](profiles.md)).
- **Audio stream rules** - commentary stripping, language filtering, default-track retention. holdfast
  has none, by design: audio, subtitles and attachments are stream-copied untouched.
- **Sonarr/Radarr webhook intake**, through a narrowed webhook token with optional container path
  translations. holdfast has no webhook receiver at all; an *arr calls the generic scan endpoint behind
  the one control token ([api-reference.md](api-reference.md)).
- **A Jellyfin integration** - a narrowed plugin token for enqueue, completion events, job details and
  library refresh. holdfast ships nothing of the kind.
- **Named API tokens with access classes** - read-only, webhook, plugin, full access. holdfast has one
  bearer token at one access level, the known limitation recorded in
  [the README](../README.md#running-a-modified-holdfast-on-a-network).
- **An off-peak scheduler with a priority queue.** holdfast half has this: a daily `run_window` and a
  per-core load cap, and no priority queue - work is taken in the order the scan finds it.
- **Automatic hardware selection with CPU fallback** across NVIDIA, Intel, AMD and Apple. holdfast will
  not guess: `encoder:` is configured, and a hardware encoder with no usable device stops the run
  rather than quietly falling back to CPU.

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

<a id="differentiator-gate"></a>

**The difference is where the default sits, and it is a claim about holdfast alone.** holdfast's verify
gate is **default-on**, **layered** and **fails closed**. Layered means every layer runs rather than the
first one that answers: structural parity (codec, duration, packets, per-type stream counts,
strictly-smaller), then full decode-integrity, then three VMAF floors - the mean (`min_vmaf`), the worst
frame (`vmaf_min_pool`) and chroma (`vmaf_min_chroma`, which the luma-only VMAF model cannot see at
all). Fails closed means an output that cannot be measured is rejected rather than assumed good: an
ffmpeg without libvmaf stops the tool instead of quietly downgrading the gate, and a score that could
not be produced is never read as a score that passed. That is the whole claim, and it is narrower and
truer than "the only one that checks".
