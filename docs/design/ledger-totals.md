# The whole-ledger figures

Why a figure holdfast could not read is reported as nothing rather than as zero.
This document is that argument's single home: `CLAUDE.md` names the rule and links
here rather than restating it. Which responses carry which figure, what each one
is computed over and what its envelope means is the reference in
[docs/api-reference.md](../api-reference.md).

## An unreadable figure, and why it is not a zero

<a id="null-is-not-zero"></a>

**An unreadable figure is reported as an explicit null and never as 0.** A capped
response says what it was capped against, and when that total could not be read
`available` is `false` and `count` is null.

A zero would claim the ledger is empty beside rows the caller can already see. It
is not a cautious answer, it is a confident wrong one: nothing in the response
distinguishes it from a real count of zero, so a client renders "0 jobs" over a
table of jobs and an operator reads a working library as an idle one. The same
rule holds wherever a figure is derived rather than counted - a VMAF of 0.0 is a
destroyed frame, not a missing measurement, and an absent size would invent a 100%
reclaim - so an unrecorded value is excluded and reported, never folded in as a
zero.

The rows still ship. One unreadable figure never suppresses the rest of the
response: the summary, the queue rows and the history rows are all returned, the
SSE broadcast still fires, and that one figure ships marked unavailable
and shows no number in its place while the rest of the page renders. One
unreadable figure never costs an operator the records.
