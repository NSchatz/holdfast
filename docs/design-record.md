# The design record: what holdfast's dashboard looks like, and why

`interface-craft` C1 asks every repository for exactly this document: the display face,
the text face, the accent, the radius signature and the shadow signature, each with one
sentence saying why that value. An unstated value is the gap a generator fills with the
average of its training data, and the whole point of writing them down is that the next
change to this surface is a decision somebody made rather than whatever was reached for.

This file is graded. `make check-design-record` reads it, requires all five values, holds
each to the token that carries it, and then scans the dashboard's identity sources for
every entry of the C2 blocklist below. `make check-design-record-selftest` defeats that
check once per failure mode and once per blocklist entry, so the guard is known to bite.

**The token file is the one writer of a VALUE; this file is the one writer of a REASON.**
Every value below names the token in `internal/webui/src/tokens.css` that carries it and
repeats that token's own declared value, and the check fails if the two disagree - so this
document cannot quietly drift away from the surface it describes. Where a token is
declared per theme (S6 hand-authors both), the value here is the one the default `:root`
block declares, which is the light palette, and the dark counterpart is named in the
reason.

Two of the five name the same token. holdfast sets its headings and its prose in one
face, so the display face and the text face ARE `--font-ui`; `styling` S5 reserves
`--font-mono` for data, which is a third face and not one of C1's five.

## Identity

### display face
token: `--font-ui`
value: `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "Noto Sans", "Liberation Sans", sans-serif`
why: holdfast serves one self-contained document to a LAN that may have no route out and
resolves nothing at load time, so the face its headings are drawn in is the host's own UI
face rather than one fetched from a font service.

### text face
token: `--font-ui`
value: `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "Noto Sans", "Liberation Sans", sans-serif`
why: the prose on this page is labels and one-line explanations rather than long-form
copy, so a second face would add a channel the page does not use, and C3 already builds
its hierarchy from size, weight and colour.

### accent
token: `--accent`
value: `#0f5aa8`
why: one non-neutral hue and no more (S10), a deep blue measured to clear 4.5:1 against
both surfaces in both themes (6.43:1 on `--bg` light, 8.11:1 dark, where the hand-authored
dark palette declares `#6cb0e8`), and picked as a measured value rather than a step off a
framework ramp.

### radius signature
token: `--radius-md`
value: `8px`
why: 8px is the corner every panel, card and control strip on this page takes, with
`--radius-sm` at 4px for the controls sitting inside them and `--radius-pill` for status
dots alone, so the signature is a shallow two-step corner rather than one uniform heavy
round applied to everything.

### shadow signature
token: `--shadow-raised`
value: `0 1px 4px rgba(9, 12, 18, 0.18)`
why: exactly one shadow depth exists (S8, C4) and it is spent on the one genuinely raised
surface, the sticky header the document scrolls under, while every other separation on the
page is a border plus a surface token (the dark palette declares
`0 1px 4px rgba(0, 0, 0, 0.55)`, because a shadow tuned for light does not read on dark).

## Blocklist exceptions

`interface-craft` C2 names the defaults an unspecified interface converges on, and C2's
last sentence is the only sanctioned route past one: a repository wanting an entry names
it HERE with its reason, and it is allowed. The blocklist itself is never weakened to
reach a green check, and an entry not named below is refused wherever it appears in the
identity sources.

Each heading is the entry's id as `make check-design-record` prints it. An exception with
no reason sentence grants nothing, which is the point: an exception is bought with a
reason or it is not bought.

### roboto
why: `--font-ui` names Roboto as Android's platform UI face, four positions deep in a
fallback chain whose earlier entries reach the real UI font on macOS, Windows and a Linux
desktop; it is one operating system's system face, not a display face this repository
chose.

### helvetica
why: `--font-ui` names "Helvetica Neue" as the pre-Catalina macOS UI face, sitting behind
`-apple-system` and `BlinkMacSystemFont`, which resolve to the current one, so it is
reached only by a host old enough that nothing ahead of it resolved.

### arial
why: `--font-ui` names Arial as the last-resort proportional face ahead of the
`sans-serif` generic, for a host carrying neither a platform UI face nor Liberation Sans;
S5 requires prose and controls to be proportional, and the tail of the chain is what
guarantees they are.

### bare-system-stack
why: both declared stacks are system stacks by construction, because this repository
ships no licensed face and fetches nothing at load time, and `styling` S5 puts prose and
controls in system sans and data in monospace. The honest reading is that holdfast's faces
are the host's, deliberately, and that its identity is carried by the accent, the radius
signature and the shadow signature instead; the token file records the one place this is
not free, which is that the stack must not begin with the `system-ui` generic, since a
host may nominate a fixed-advance face for it.
