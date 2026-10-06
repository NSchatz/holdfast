# CLAUDE.md

`holdfast`: a config-as-code, data-safe, self-hosted media transcoder in Go (a Tdarr replacement).
It re-encodes bloated video smaller and never destroys a source until a replacement is provably faithful.

## Dangers and never-do rules

- **Never mutate a source until a replacement passed every gate** (codec, duration and packet
  parity, strictly smaller, per-type stream counts, fidelity, full decode, VMAF). The swap is an
  atomic same-filesystem `rename()` from the source's own directory; do not weaken it.
- Fail safe: ambiguous, malformed or unsupported input SKIPS with a logged reason or returns a
  typed error. Never a confident wrong result, never a silent loss.
- Every change that touches the engine extends the fixture suite so it reds on that regression.
- Phase IDs `TRANSCODE-1`...`TRANSCODE-17` must survive; their pre-rename underscore forms must
  not exist (`scripts/check-pins.sh`).
- No secrets. A credential key takes `file:` or `cmd:` only, never a value; a new one joins
  `config.SecretBearingKeys`. `Expose()` is the only route to plaintext.
- No owner identity outside `LICENSE` and `NOTICE`; synthetic identities everywhere else.
- Plain hyphens only, no en or em dashes. No dates or narrated history in this file.

## The gate

CI is the only place it runs: no gate, tier, whole-package, `-race` or mutation run on this host.
A PR runs only what its change touches (`pr.yml`, `scripts/pr-scope.sh`, 2 minutes); the full
gate (`make check`) runs nightly on main (`ci.yml`), and a red nightly is fixed first. The inner
loop is one focused test (one package, one `-run`). The pinned ffmpeg is required, never skipped.
Never claim green without a green CI run.

## Git

One branch and PR per change, squash-merged when its checks are green with main merged in, the
run's link in the PR body. Conventional Commits, as the repository's configured git identity, no
`Co-Authored-By` or AI co-author trailer. `make install-hooks` once (the secret scan).

## Questions

A decision that is the owner's to make goes to the owner with the AskUserQuestion tool in the
session, never as an issue. Write "the owner" in this public repository, never a name.

## Siblings

- Before writing new code, search shopkit's index:
  `git -C /workspace/shopkit show origin/main:docs/INDEX.md | grep -i <word>`; reuse or extend
  shopkit instead of writing it again.
- Work a sibling repo must do goes to that repo as an exact spec; read its card first:
  `git -C /workspace/<repo> show origin/main:CARD.md`.

## Details

- [`CARD.md`](CARD.md) - what this repo offers its siblings and how to hand it work.
- [`docs/development.md`](docs/development.md) - the gate, the conventions and the identifier rule in full, and the reference index.
- [`docs/design/README.md`](docs/design/README.md) - every design rule, linked to its argument.
- [`docs/layout.md`](docs/layout.md) - where each package and script lives and what it owns.
- [`docs/secrets.md`](docs/secrets.md) - credential references, the resolver and the scanners.
- [`docs/mutation-testing.md`](docs/mutation-testing.md) - the mutation floor and domain.
- [`docs/api-reference.md`](docs/api-reference.md) - the HTTP API and every job-row field.
