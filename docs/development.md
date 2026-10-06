# Development

The gate, the conventions and the identifier rule in full. `CLAUDE.md` carries the
must-knows and points here.

## Build, test and gate

Go 1.25+. The gate is `make check`, and the `check:` target IS its definition -
read the target rather than any prose about it (`make tier-full` is `check`; `make tier-fast` its quick subset). The Makefile owns the tool pins
and CI invokes the same target, so the nightly run, a release and a human run the identical thing.

**CI is where the gate runs, and the only place** ([`AMENDMENT-2026-10-03-owner-2026-09-holdfast.md`](https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/AMENDMENT-2026-10-03-owner-2026-09-holdfast.md)). On
the development host no gate, tier, whole-package or `-race` suite or mutation run happens: push the branch, open the
PR, and read CI. A PR merges when its checks are green on its branch with main merged in, with the run's link in the
PR body.

**A PR runs only what its change touches, in 2 minutes or less** (`pr.yml`): `scripts/pr-scope.sh` picks the packages
the diff touches and every package that depends on them, and each check or selftest whose inputs changed; the job
prints its elapsed time and warns, never fails, past 2 minutes. internal/engine's real encodes never run on a PR (it
is vetted only). A change to the Makefile, a workflow or `go.mod` runs every package and every check but the engine's.
`mutation.yml` stays diff-scoped on every PR. **The full gate runs nightly on main** (`ci.yml`: `make check` with the
engine suite, every selftest and the image smoke gate, plus `workflow_dispatch` to run it by hand); a red nightly
run is fixed on main before anything else merges. While working, one focused test (one
package, one `-run` filter) is the inner loop, not a gate.

The nightly run adds two things `check` deliberately does not: the config-schema self-test (proves `validate`
reds on a bad config) and the image smoke gate (`scripts/smoke-image.sh`, needs Docker).

The gate needs the pinned ffmpeg (`scripts/install-ffmpeg.sh`), and it is not
skipped when absent - a grader that skips is a false green. A focused test that needs a real encode
may run on the shared host; nothing larger runs there.

Never claim green without a green CI run. Every change that touches the engine
extends the fixture suite so it reds on that specific regression: a data-safety
tool proves its unhappy paths.

## Conventions

- Small, testable functions; fail safe; match Go idiom and the existing layout.
- No secrets, ever, and it is MECHANICAL now: `make secret-scan` refuses a tracked
  file carrying an issued credential or named like a credential store, and
  `make install-hooks` (the one setup step) puts it on the pre-commit path. Both `secret-scan` and
  its self-test ride `make check`. Synthetic `config.example.yaml` only; real `config.yaml` is gitignored.
- A credential is reached BY REFERENCE. `server_auth_token`, `server_read_token`, `notify_url`,
  `tautulli_api_key`, `radarr_api_key`, `sonarr_api_key`, `plex_token`, `webhook_token`, `node_token` and `server_tls_key` carry
  `file:<path>` or `cmd:<argv>`, never a value, and a literal in the file or in `HOLDFAST_*` refuses to
  start - a credential in holdfast's environment is inherited by every `ffmpeg` child.
  `config.SecretBearingKeys` is the closed list; a new credential-bearing key joins it or it is not one. A
  resolved value is a `secret.Value`, which renders as `<redacted>` through `fmt`, `slog`, JSON and text;
  `Expose()` is the only route to the plaintext, so grep for it. `docs/secrets.md` is the reference.
- Commit as the repository's configured git identity; no `Co-Authored-By` and no AI
  co-author trailer.
- No owner identity in a tracked file: the owner's name and email appear only in `LICENSE`
  and `NOTICE`, and synthetic identities stand in everywhere else. `make identity-scan`
  enforces it in `check`, reading the identity from the repository's first commit at run
  time, so the rule carries no copy of what it guards.
- Conventional Commits.
- Plain hyphens only - no en or em dashes, anywhere.
- No dates and no narrated history in `CLAUDE.md`. Git holds that. Work arrives as tasks
  from the owner's agent harness, not from a roadmap in the umbrella (decided by the owner,
  T2, T8); the retired program's brief, goal files and ledgers are pinned at
  https://github.com/NSchatz/holdfast/tree/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals.

## Identifier rule

Identifier rule: the phase IDs `TRANSCODE-1`...`TRANSCODE-17` (there is no
`TRANSCODE-10`) are historical labels and must survive - git log names the work by
them. The underscore forms are pre-rename identifiers and must not exist;
`scripts/check-pins.sh` fails on any that reappear and is mutation-tested to
prove it still bites. A line may quote a banned identifier only to prohibit it,
and only with a `rename-guard-allow` marker.

## References

`docs/secrets.md` the reference forms, the resolver contract and its documented timeout bound, the
scanner's ruleset, exit codes and one setup step, and the identity scan · `docs/docker.md` deployment
(volumes, permissions, TZ, GPU passthrough, security posture) · `docs/migration.md` the cutover from the
Bash transcoder and Tdarr · `docs/post-swap-hook.md` the Radarr, Sonarr and Plex clients: their keys and
requests, the arr re-download warning, the drain bound and the Plex play hold · `docs/requeue.md` what a
terminal row records about the configuration it was decided under, and the lever for the rows a
configuration change cannot reason about · `docs/test-mass.md` how much of this repository is test code,
what `scripts/test-mass.sh` counts, what was retired for grading presentation rather than behaviour, and
why line coverage is not assertion · `docs/mutation-testing.md` the mutation score floor, the figure it
is applied to, which packages are in the mutation domain and why each exclusion is there, what a pull
request runs against what the schedule runs, and how to reproduce either by hand ·
`docs/hardware-reports.md` how a hardware report is run, what it records and what it redacts · `docs/client-reports.md` the same for the owner's live check of Plex, Sonarr and Radarr ·
`docs/encode-memory.md` the encode memory watchdog, the mux-queue bounds on every ffmpeg argv, and the
reproduction attempt behind them.
