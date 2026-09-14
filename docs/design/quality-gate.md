# The quality gate

Why the perceptual gate is pooled the way it is, and why it fails closed. This
document is that argument's single home: `CLAUDE.md` names the rule and links
here rather than restating it. What a job's row RECORDS about a measurement -
`vmaf_mean`, `vmaf_min`, `vmaf_model`, `vmaf_pix_fmt`, `vmaf_stream`,
`vmaf_chroma` - is the reference in
[docs/api-reference.md](../api-reference.md).

## Pooling, and the three floors

<a id="vmaf-pooling"></a>

**The gate bounds the worst frame, not only the average.** An average hides local
damage, so a short destroyed segment inside an otherwise-clean encode passes a
mean-only gate - and passes every structural check beside it too, because it
decodes fine and carries the right duration, packets and streams. The mean floor
(`min_vmaf`), the worst-frame pool floor (`vmaf_min_pool`) and the chroma floor
(`vmaf_min_chroma`) are therefore all on by default, and a configuration that
zeroes all three with the gate still enabled is refused at startup rather than run
as a gate that can never reject.

The luma-only VMAF model cannot see colour at all: it scores the luma plane and is
structurally blind to chroma damage, which is why the chroma floor is separate
from the other two and measured with its own metric rather than folded into the
VMAF number.

An output that cannot be measured is rejected rather than assumed good. An ffmpeg
without libvmaf stops the tool instead of quietly downgrading the gate, and a
score that could not be produced is never read as a score that passed.

## Why a number needs its conditions

A VMAF score is a regression onto a subjective opinion scale under one viewing
condition, and it is not comparable across different sources. The model, the pixel
format both streams were converted to before scoring, and the stream that was
compared therefore travel with every score on the record, so a reader can say what
was measured and in what. The per-field semantics are in
[docs/api-reference.md](../api-reference.md).
