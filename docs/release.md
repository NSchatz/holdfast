# Cutting a release

This is the operator runbook for the release path. It is written as the ordered procedure
the FIRST release went through and every later one repeats, because that order is the whole
safety property: each step says what leaves the machine, whether it can be undone and by
what. Most of the second half cannot.

`scripts/release-shape-gate` (inside `make check`) proves the workflow still has the shape
this document describes, by running its planning logic for each event shape rather than by
reading its comments; `make release-shape-selftest` proves that gate still bites. Neither of
them can dispatch a workflow, rename a repository, flip its visibility or push a tag. Those
four are yours.

## Where this repository actually stands

**`v0.1.0` is published.** Read this before anything below: several of the steps are already
done, and the ones that are done are in the irreversible set.

| fact | evidence |
|---|---|
| the dry run has been dispatched | run `29646149489`, `workflow_dispatch` on `main`, 2026-07-18 13:26Z, green. Its five publishing steps all show `skipped`. |
| `v0.1.0` has been released | run `29646482337`, tag push, 2026-07-18 13:37Z, green. Every step ran, including the promotion and the release. |
| the image is published | `ghcr.io/nschatz/holdfast:v0.1.0` at `sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2`, pulled back and re-smoked on both arches by that run. |
| `:latest` points at it | the same run promoted `ghcr.io/nschatz/holdfast:latest` onto that digest with `imagetools create`. |
| a GitHub release exists | tag `v0.1.0`, cut by the workflow token, carrying `holdfast_v0.1.0_linux_amd64.tar.gz`, `holdfast_v0.1.0_linux_arm64.tar.gz` and `SHA256SUMS`. |
| the repository is public | `NSchatz/holdfast`, `visibility: public`. |
| the annotated tag is on origin | `refs/tags/v0.1.0` -> `3468562c`, pointing at commit `38fb8b3`. |

Re-derive any of it:

```sh
gh run list --workflow=release.yml --limit 10 --json conclusion,event,headBranch,createdAt
gh api repos/NSchatz/holdfast/releases --jq '.[] | {tag_name, assets: [.assets[].name]}'
gh api repos/NSchatz/holdfast --jq '{name:.full_name, visibility:.visibility}'
```

What this means for the steps below. Steps **1, 3 and 5** are done for good; they are
one-time acts and they have been taken. Steps **2 and 7** have each run once and run again
per release. Step **8** has never been done. So the NEXT release is: dispatch the dry run
(2), pick and choose a HIGHER version (4, 6), push that tag (7), confirm the pull (8).
`v0.1.0` itself is spent: a released version's contents "MUST NOT be modified"
(semver.org), and nothing here can un-publish it.

## The irreversible set

These are the acts that cannot be taken back by re-running anything.

**The first ten are every step of `release.yml`'s `publish` job**, and that is the whole of
the inventory rather than a selection from it. `release.yml` is split so that every step
which could publish lives in that one job, and that job is the only one granted a write
permission - `packages: write` to push the image, `contents: write` to cut the release. A
step in the `build` job holds neither, so it can say `docker push` in any spelling it likes
and publish nothing; a step in `publish` holds both, whatever it says it does. `make check`
therefore inventories the job, not the steps' text, and reds until this table names every id
in it. That is stronger than a catalogue of publishing commands and it is also the only
version that survived review: deciding what an arbitrary shell script does is undecidable,
and six attempts to do it were each beaten by ordinary shell.

**Every act below has already been taken**, on 2026-07-18, for `v0.1.0` (see the section
above). Read the table as the standing description of what each act costs, not as a list of
decisions still open.

| act | what leaves this machine | can it be undone? |
|---|---|---|
| `publish/checkout` | nothing. It reads the tree the tag points at. It is listed because it runs inside the job that holds the write grant, and everything in that job is inventoried. | Yes, trivially: nothing left. |
| `publish/setup-qemu` | nothing. It installs the emulation the arm64 re-smoke needs. | Yes, trivially: nothing left. |
| `publish/setup-buildx` | nothing. It configures the builder. | Yes, trivially: nothing left. |
| `publish/setup-go` | nothing. It installs the pinned Go toolchain this job needs to build the single reader of `docker-compose.yml`'s image reference (`publish/resolve-compose` below). `make check` and its own `setup-go` are in the `build` job, on a different runner. | Yes, trivially: nothing left. |
| `publish/download-dist` | nothing. It fetches the tarballs the `build` job already gated, so the release ships exactly the bytes that were checked rather than a rebuild nobody smoked. | Yes, trivially: nothing left. |
| `publish/registry-login` | nothing yet, but this is where the job's `packages: write` token becomes a registry credential. Everything after it can publish. | Yes: a login is local to the runner and the runner is destroyed. |
| `publish/push-version` | the multi-arch image, at `ghcr.io/<owner>/<repo>:<version>`, pullable the moment it lands | No. A GHCR tag can be deleted, but not un-fetched: anyone who pulled it, and any cache that mirrored it, keeps the bytes. Deleting it also breaks the compose files of anyone who pinned it. Supersede it with a higher version instead. |
| `publish/resmoke` | nothing. It pulls the pushed artefact back, for BOTH architectures, and drives a real encode through it. It is the gate on what was published rather than on a local build of it, and it runs before the floating reference moves so that a failure here leaves `:latest` where it was. | Yes, trivially: nothing left. |
| `publish/promote-latest` | `:latest` starts resolving to that digest, for every user running `docker compose pull` | Partly. `:latest` can be re-pointed at an older digest by re-running `docker buildx imagetools create`, but everyone who pulled in between already has the new image, and containers already recreated from it stay recreated. |
| `publish/resolve-compose` | nothing. It resolves the exact reference `docker-compose.yml` hands users and fails the release if it does not resolve to the digest this run gated. | Yes, trivially: nothing left - but a failure here means something already published is wrong, which is not undone by this step failing. |
| `publish/github-release` | a GitHub release, its notes, and the source/binary tarballs | Partly. The release object can be deleted; the tarballs it served, the notification it sent to watchers, and any mirror of the tag cannot. |
| the git tag itself | an annotated tag on origin, which the Go module proxy may fetch | No, in the sense that matters. `git push --delete` removes the ref, but the Go module proxy caches a version permanently once anything requests it, and a released version's contents "MUST NOT be modified" (semver.org). `retract` in `go.mod` does not unpublish either: the Go modules reference is explicit that retracted versions "should remain available in version control repositories and on module proxies". |
| the repository rename | the module path, the image reference and every link that names the repository | No. Go has **no module-path rename primitive**, nothing rewrites an image reference sitting in a user's compose file, and GitHub's redirect from the old name survives only while the old name is left permanently vacant. This is why the rename is part of THIS set and not a cosmetic follow-up: after the first tag it can no longer be done cleanly at all. |
| making the repository public | the whole history, every commit message, every file ever committed | No. Anything fetched, forked, mirrored or indexed while it is public stays fetched. Making it private again removes access, not copies. |

Everything before step 5 is reversible in principle. From step 5 on, nothing is - and
steps 5 and 7 have already been taken, so this repository is past that line already.

## 1. Land the branch first

`workflow_dispatch` is offered by GitHub only for a workflow already on the DEFAULT
branch, so the dry run in step 2 cannot be run from a pull request. Merge first, then
dispatch.

Status: done. The workflow has been on `main` since 2026-07-13.

Undone by: reverting the merge. Nothing has left the machine at this point.

## 2. Dispatch the dry run, and read its artifacts

Actions -> Release -> Run workflow, on the default branch. `workflow_dispatch` is always a
dry run: the planning step sets `publish=false`, the whole `publish` job is guarded on that
value, and that job is the only one holding a write permission. So a dispatch runs the
`build` job alone, with `contents: read` and nothing else - it could not publish if a step
tried. It still does everything else, which is the point.

Status: done on 2026-07-18 (run `29646149489`, green, publishing steps skipped). Dispatch it
again after any change to `release.yml` or to the gate: it is free, it publishes nothing,
and it is the only way to see the whole path run before a tag commits you to it.

Wait for a green run, then check that it actually did the work rather than skipping to the
end:

- The **plan** step's log line reads `publish=false version=0.0.0-dev-<sha> ...`. If it
  says `publish=true`, stop: the dispatch would publish. (`make check` refuses that state,
  so seeing it means the workflow on the default branch is not the one that was gated.)
- **the full gate (make check)** ran, and is green. This is the real-ffmpeg fixture suite,
  including the data-safety proof, not a subset.
- **both** image builds ran, and **both** smoke tests: `smoke test the image (real encode,
  real gate)` drove a real encode inside the amd64 container, and `smoke test the arm64
  image` executed the cross-built arm64 binary under QEMU.
- The run's **artifact** `release-dist-0.0.0-dev-<sha>` is present. Download it. It must
  contain `holdfast_<version>_linux_amd64.tar.gz`, `holdfast_<version>_linux_arm64.tar.gz`
  and `SHA256SUMS`. Untar one and confirm `LICENSE` and `NOTICE` are inside it beside the
  binary: holdfast is AGPL-3.0-only and a tarball that ships the binary alone distributes
  it with no licence at all.
- Confirm **nothing new** was published: the whole **`publish` job** shows as **skipped**.
  That is the assertion, and it is the only one available now that
  `ghcr.io/nschatz/holdfast` exists: "the package is absent" stopped being a check on
  2026-07-18. If that job ran, stop - a dispatch that starts the job holding
  `packages: write` is the state `make check` refuses, so seeing it means the workflow on the
  default branch is not the one that was gated.

Undone by: nothing to undo; a dispatch publishes nothing. This step exists precisely so that
a change to the release path is exercised before a tag commits it.

## 3. The repository must already carry the name the example deployment pulls

`release.yml` never spells the image out. It derives it:
`ghcr.io/$(echo "$GITHUB_REPOSITORY" | tr '[:upper:]' '[:lower:]')`. So the GitHub
repository's name IS the published image reference, and `docker-compose.yml` names
`ghcr.io/nschatz/holdfast:latest`.

Check, before anything is published:

```sh
gh repo view --json nameWithOwner -q .nameWithOwner            # must be NSchatz/holdfast
go run ./scripts/release-shape-gate -print-compose-ref          # ghcr.io/nschatz/holdfast:latest
make check                                                      # the gate that holds those two together
```

That second command is the ONE reader of the compose file's image reference, and it is the
same one `scripts/resolve-compose-image.sh` uses at release time. Do not grep for `image:`
instead: a second reader agrees on today's file and diverges on the shapes that matter.

`make check` fails if they disagree, and prints both. It derives the expected reference
from `go.mod`'s module path, so it is checking the same fact the workflow will use, not a
copy of it.

Status: done. The repository is `NSchatz/holdfast` and `docker-compose.yml` names
`ghcr.io/nschatz/holdfast:latest`, which is what `v0.1.0` published under.

The window for this closed with the first release. A rename now would not just be
irreversible, it would strand what is already out: `ghcr.io/nschatz/holdfast:v0.1.0` and
`:latest` stay published under the old name, the old module path is in the Go proxy, and
nothing rewrites the reference in a compose file somebody already copied. If the repository
must ever be renamed, treat the published references as permanent and plan a redirect for
them, not a rename of them. When you do rename anything, leave the old name permanently
vacant: GitHub redirects it, but reclaiming the old name kills the redirect and silently
serves a different repository in its place.

Undone by: nothing, now that a release has published under this name. Before the first
release it was undone by renaming back.

## 4. Pick a version nothing has used

`v0.1.0` is taken. An annotated `v0.1.0` is on origin (`3468562c`, pointing at commit
`38fb8b3`), a green release run stands behind it, and a GitHub release and a published image
carry that name.

**Do not delete and re-create it.** The runbook used to say you could, on the premise that
nothing had consumed it; that premise is false. Semantic Versioning is explicit that "the
contents of that version MUST NOT be modified", and re-pointing the tag would do exactly
that: one version name, two different artefacts, with the first one already pulled and
already in the release object. Deleting the tag would not un-publish either of them - a
GHCR tag can be removed but not un-fetched, and the Go module proxy caches a version
permanently once anything requests it.

So:

- **Cut the next release at a HIGHER version** (`v0.2.0`). That is the only supported move.
- Do not re-push an existing tag. `release.yml` triggers on `push: tags: ["v*"]`, so
  re-delivering `v0.1.0` would build and publish an image from that stale tree under a name
  that already means something else.
- If a released version turns out to be broken, supersede it. Deleting it breaks the compose
  file of everyone who pinned it and takes back nothing.

Undone by: nothing needs undoing; choosing a version costs nothing until step 7.

## 5. Make the repository public

The image inherits the repository's visibility: while the repository is private, GHCR
serves the package only to authenticated users, so a stranger cannot pull it and the north
star is not testable.

Status: done. `NSchatz/holdfast` is public. This was the first irreversible act, and
everything in the history became public with it.

Undone by: nothing. See the table above. Making the repository private again removes access,
not copies.

## 6. Choose the tag

The major version must be zero. `release.yml` refuses anything else before it publishes,
and says why. See "Before a major version above zero" below for what would have to be true
first.

- Format: `v0.MINOR.PATCH`, optionally with a pre-release suffix (`v0.2.0-rc1`).
- A suffixed tag is a pre-release: it publishes the version tag and cuts a pre-release, but
  does NOT become `:latest`. Use one if you want a rehearsal that real users will not pull.
- Pick a version nothing has used. See step 4.

Undone by: nothing yet; choosing is free.

## 7. Push the tag

```sh
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

That is the whole trigger. There is deliberately no "publish" checkbox: the only thing that
can publish is a tag, so a release always carries a real version name.

**This is the second irreversible act, and it starts the other three.** The run then, in
this order: runs the full `make check`; builds and smokes both architectures; pushes the
version tag ONLY; pulls that artifact back for both architectures and re-smokes it;
promotes `:latest` onto the same digest; resolves the reference `docker-compose.yml` names
against the registry; cuts the GitHub release. `make check` refuses any reordering of that,
and refuses any step before the promotion being marked `continue-on-error`.

If any gate or smoke run fails, the run stops there and `:latest` stays exactly where it
was: the `publish` job `needs: build` and its `if:` calls no status function, so GitHub does
not start it when what it needs failed - and it is the only job that holds a write
permission, so nothing else could publish in its place. On the first release
`:latest` did not exist yet, so a failure would have left nothing published at all; from
now on it means `:latest` keeps resolving to the previous release, which is the property
`make check` asserts by re-deciding every guard with the run marked failed.

Status: done once, for `v0.1.0`, on 2026-07-18 (run `29646482337`). It ran in exactly the
order above. Doing it again means a higher version, per step 4.

Undone by: nothing. See the table above.

## 8. Pull it the way a stranger would

From a machine that is not yours, with no credentials:

```sh
docker pull ghcr.io/nschatz/holdfast:latest
docker run --rm ghcr.io/nschatz/holdfast:latest version
mkdir -p /tmp/holdfast-check && cd /tmp/holdfast-check
curl -fsSLO https://raw.githubusercontent.com/NSchatz/holdfast/main/docker-compose.yml
docker compose config -q
docker compose pull
```

`docker compose pull` is the acceptance test for the whole phase: it resolves the exact
reference the published compose file names. Every release from now on asserts it on its own
(`scripts/resolve-compose-image.sh` runs after the promotion and fails the release if that
reference does not resolve to the digest the run just gated), so this is confirmation, not
the only check.

Status: NOT confirmed. `v0.1.0` predates that step, so no run has ever resolved the compose
reference. A GHCR package carries its OWN visibility, separate from the repository's, and it
is not readable from a plain `gh` token - so whether a stranger with no credentials can pull
`ghcr.io/nschatz/holdfast:latest` is exactly what this step, and only this step, settles. If
it 401s or 404s, the package is still private: link it to the repository and set it public
in the package settings. Nothing above proves this one.

Then point it at a real library and let it run. That is the north star, and until this step
passes on someone else's machine, nothing above proves it.

Undone by: nothing to undo.

## Known limits

### Backport tags move `:latest` backwards

`release.yml` records this and does not fix it. A backport tag (`v0.2.4` cut after
`v0.3.0`) is not a pre-release, so the promotion would move `:latest` back onto the older
release. Until the promotion is gated on a version comparison, cut backports from a
maintenance branch and accept it, or push the backport as a pre-release so it never
promotes. `make check` does not catch this: it checks the ORDER of the steps, not the
ordering of versions between runs.

### Before a major version above zero

The release path refuses a tag whose major version is not zero, and points here.

Semantic Versioning: major version zero "is for initial development. Anything MAY change at
any time. The public API SHOULD NOT be considered stable." Version 1.0.0 "defines the
public API", and once released, "the contents of that version MUST NOT be modified". So
cutting a non-zero major is not a bigger number, it is a promise, and it is one this
project has not made about three surfaces:

- **the configuration keys** - every key `internal/config` accepts, and the `HOLDFAST_*`
  environment overrides that shadow them
- **the HTTP surface** - the routes `internal/server` serves and the JSON shapes they
  return, including which fields may be `null`
- **the metric names** - the `holdfast_*` Prometheus namespace, where a rename silently
  breaks every dashboard built on it

None of the three is enumerated or drift-gated yet. Declaring them stable means listing
them here, gating them against drift the way `scripts/check-pins.sh` gates the pins, and
only then editing the refusal in `release.yml`'s plan step. Until that record exists, a
non-zero major would be a promise nobody could check.

## What the gate holds, and what it does not

`make check` runs `scripts/release-shape-gate`, which executes `release.yml`'s planning
shell for a dispatch, a version tag, a pre-release tag and a non-zero-major tag, and
decides from the values it produces that: a dispatch runs no job that CAN publish; the
promotion comes last, after the gate, both smokes, the push and the re-smoke; no job holding
a publishing grant runs once something has failed; nothing before the promotion tolerates its
own failure; a non-zero major is refused; this document names every step in the job that can
publish; and the reference `docker-compose.yml` gives users is the one a release of THIS
repository promotes.

Those four shapes are shapes the gate INVENTS, so it also grades `on:` against them. A
trigger no shape plans reds by name, and so does a `push:` filter that admits anything but a
tag - `branches: ["v0.**"]` beside `tags: ["v*"]` would make a push to a BRANCH named
`v0.9.9` a `push` event whose `ref_name` is `v0.9.9`, which the planning logic (which cannot
tell a branch from a tag) reads as a release. `on: push` and `on: [push]` are refused for the
same reason: they carry no filter at all. So is a `workflow_dispatch:` with `inputs:`, which
is how a dispatch-publish tick-box gets added.

### It does not decide what a `run:` step does, and it never will

That question is undecidable and this repository has the receipts. Six adversarial reviews
found six fail-opens in one direction in a reader that tried: a publishing input the act
decision could not see, detectors that could not cross a shell line continuation, a command
catalogue that knew spellings and no destination, a push inside a quoted `sh -c "…"` or
`eval "…"`, and buildx's attached shorthand `-otype=registry` read as "local". Every fix made
the reader cleverer and the next spelling of ordinary shell beat it.

Replacing the reader with an OBSERVER - running each step in an environment where nothing
external executes and recording the argv bash actually built - moved the hole rather than
closing it. Every control such an environment has lives INSIDE the shell it is watching: the
recorder is a shell function, the guard a `DEBUG` trap, the emptied `PATH` an ordinary
exported variable the step owns. `export PATH=/usr/bin:/bin` is one line, and after it a real
`git push` runs and is recorded nowhere. `exec docker push …` never reaches a recorder at all,
because `exec` is a builtin and the shell resolves the program itself.

### It decides what a step CAN do instead

A step publishes nothing it holds no credential for. That is not a reading of anything - it
is what GitHub enforces - and it is decidable from structured YAML with no shell in the
question:

- **Every job that runs on a dispatch must hold no write scope at all.** The effective
  `permissions:` are read (a job's own block replaces the workflow's; GitHub does not merge
  them), and any `write` on any scope reds by name with what that scope would authorise.
- **An unstated `permissions:` reads CLOSED and reds.** With neither the workflow nor the job
  declaring one, the token is whatever the repository's default setting is - which is not in
  the file, and may be write-all.
- **No secret beyond `GITHUB_TOKEN` may reach a dispatch-path job.** `permissions:` bounds
  that one token and nothing else; a repository secret's scope is whatever was put in it. The
  scan walks every scalar in the job, so a reference in a key this gate does not model is
  found too.
- **A key nobody has classified reds by name, at all three levels.** `environment:` hands over
  that environment's secrets; `container:`/`services:` may carry registry `credentials:`;
  `secrets: inherit` hands over everything. Each is named with what it grants, and a key in
  neither list is refused rather than assumed harmless. The workflow's OWN top-level keys are
  classified the same way, so a key GitHub adds arrives here as a refusal rather than as
  silence - two levels saying so and the third staying quiet is an asymmetry a reader of the
  output cannot see.

The consequence is the point: a step in the `build` job may say `docker push` in any spelling,
quoting or nesting at all - `sh -c "…"`, `eval`, `exec`, after resetting `PATH` - and publish
nothing, because the registry will refuse a read-only token. `make release-shape-selftest`
asserts exactly that, in those four spellings, and asserts that the IDENTICAL step in a job
granted `packages: write` IS caught. What changes the verdict is the grant.

### And which step is which, without reading prose either

A7 is an ORDER, so the gate has to know which step is the full gate and which is the
promotion. That used to be decided by searching a step for a mention of the thing, and it cost
a finding: `echo "make check"` satisfied the full-gate role, so a step that PRINTS the gate's
name and runs nothing passed.

A role is now DECLARED. The step carries an `id:`, the gate names the id, and the two must
agree about what the step invokes - by WHOLE-VALUE comparison, never a search. A role step's
`run:` is ONE line, its fields are compared as whole words, and the FIRST field must BE the
program. `echo "make check"`, `echo make check`, `printf '%s' "make check"` and `"make" check`
all fail, because none of their first fields is `make`; `make check` and `make -C . check`
both pass, because both really are the gate. Nothing is searched for inside anything, so no
quoting or nesting reaches the comparison. That is why
`scripts/release-resmoke.sh` and `scripts/release-promote.sh` are files: a role step invokes
one program so the invocation can be compared whole. The gate also checks that the script it
names is really in the repository and executable.

**And naming the program is not enough, which is the second half.** `make -n check` names the
full gate exactly, and `-n` is GNU make's dry-run mode: it prints every recipe in `check`,
executes not one of them and exits 0. The role held, the order sentence printed, and a tag
push would have published an image whose `make check` never ran. So do `-q`, `-t`,
`--dry-run`, a clustered `-Bn`, `check SHELL=/bin/true` (SHELL is a make variable, and
overriding it from the command line replaces the interpreter of every recipe), `-f /dev/null`,
and a `-C` pointing somewhere else. Listing those buys exactly the spellings it names, which
is the mistake this gate's other half already made six times.

A role's invocation is therefore ACCOUNTED FOR, deny-by-default, over its whole structured
surface. Every field must be one the role requires or one it has DECLARED as permitted, with
the reason that field cannot make the invocation do less; `-C` is permitted for the full gate
and its VALUE is declared too, so `-C .` passes and `-C /tmp` reds. The same rule covers
everything else that decides what an invocation does without touching it:

- **The environment in scope.** `env: MAKEFLAGS: -n` on the step, the job or the workflow
  neuters `run: make check` with the `run:` line untouched. A name nobody classified reds.
- **The step's own keys.** `shell: cat` makes GitHub print the script and exit 0;
  `working-directory: /tmp` makes `make check` a different Makefile's `check`. Both red, and
  so does a step key nobody classified.
- **`defaults:`**, at the job or workflow level, which sets the same two things from further
  away. A definition carrying a release role declares none.
- **An action role's inputs.** `docker/build-push-action` with `push: false` publishes
  nothing while still being the action the role names, and `outputs: type=local,dest=./out`
  sends the build to a directory. The role requires `push: true` by whole value, and an input
  nobody classified reds.

The order itself comes from the `needs:` graph and declaration order. Two jobs with no path
between them are CONCURRENT, and the gate refuses to order them rather than reporting an order
it did not check.

### What it executes, and the bound on that

Exactly one step: the one holding the `plan` role. A second step writing to `$GITHUB_OUTPUT`
reds, because a second planning script would be a second script to execute, and executing a
workflow's step scripts to find out what they do is the mechanism that was defeated. That one
script runs with `PATH` set to a stub directory alone - recording stubs for every tool that
could reach a registry, plus a declared set of pure utilities - with `HOME` and the working
directory in a throwaway directory, and with a 90-second timeout. The planning step invoking a
registry tool is itself an error: planning DECIDES, it does not publish.

The residue, stated rather than left to be found: a planning script that resets `PATH` itself
can still run a program. That is a property of executing repository code at all, which `make
check` already does when it runs the test suite. What it is not is a way to make this gate
report the wrong answer - nothing about the verdict depends on what that script invokes, only
on what it appends to `$GITHUB_OUTPUT`.

A second residue: a credential written LITERALLY into `release.yml` rather than through
`secrets.` is outside this model. Such a credential would be committed in plaintext, which is
a louder problem than this gate and one the repository forbids outright.

**Extending this:** do not teach the gate to read a step's text. If a step needs to publish,
put it in the job that holds the grant and name it in the table above. If it must not publish,
put it in the job that holds none. If a key reds as unclassified, classify it with what it can
hand a job. The one thing this must never gain is a rule that reads a step's script and
concludes it is harmless.

The example deployment's image reference has exactly ONE reader, in that gate.
`scripts/resolve-compose-image.sh` asks for it (`release-shape-gate -print-compose-ref`)
rather than parsing `docker-compose.yml` a second time, because two readers agree on today's
file and diverge on a quoted scalar, a second service with an `image:`, or an `image:` key
nested outside `services:` - the same "held in step by hope" shape the ffmpeg pin is parsed
out of the Dockerfile to avoid.

`make release-shape-selftest` defeats each of those on purpose against a mutated copy of
the repository and fails if any defeat did not run.

### What the release SCRIPTS do is proved somewhere else, not here

`scripts/release-promote.sh`, `scripts/release-resmoke.sh` and
`scripts/resolve-compose-image.sh` are the bodies of three role steps, and the rule above -
the gate does not decide what a `run:` step does - covers them too. The gate checks that each
is in the repository and executable, that its step declares the role, and that every value its
`env:` HANDS it is the one this run produced, compared whole: `IMAGE` and `VERSION` against
the image and version the planning logic wrote to `$GITHUB_OUTPUT`, `REF` against
`${IMAGE}:${VERSION}` - the reference this run pushed and gated - and `FLOATING_TAG` against
the tag `docker-compose.yml` itself names, which is the reference a user actually pulls. The
version-tag push is an action rather than a script and gets the same treatment on the one
input that names its object: its `tags:` is held against `${IMAGE}:${VERSION}` too. It does
not open the scripts, and it must not.

That last part is not decoration, and it was missing until impl-gate ordinal 8 of S0046 asked
for it. Naming what a value is FOR holds it to nothing: `REF: ${IMAGE}:latest` on the
re-smoke is one line, it leaves the role held, the invocation untouched and the order sentence
printing, and it makes the release pull back the PREVIOUS release - which passes, it was
gated last time - while the artefact this run just pushed is never pulled back at all and
`:latest` is then promoted onto it. `VERSION: latest` on the resolver is the same line again
and turns the check below into a comparison of the compose reference's digest with its own,
which can never fail. A name a role declares and nothing compares now reds by name, the same
way an unclassified field, key or action input does.

**And a comparison is only as good as the value on its other side.** Those names were held
against the outputs of ONE planned release, and that bought exactly one literal back: the one
equal to the gate's own sample. The sample is `v0.1.0`, which is not an arbitrary string - it
is the version this repository has actually published, it is named all over this file, and it
is what a maintainer copies out of a green run's log. `REF: ghcr.io/nschatz/holdfast:v0.1.0`
would then leave every assertion in the gate green while every later release pulled back and
smoked the already-published v0.1.0, which passes because it was gated in July, and `:latest`
moved onto an artefact nothing in that run had smoked. So each value the RUN produces is now
held against SEVERAL independently planned runs - a real release, a second real release at a
different version, and the dry run - and compared whole against each. A literal equals one
value; it cannot equal three. The gate also grades its OWN anchor: if those runs did not
actually produce different values for a name, it reds saying so, because an anchor that has
quietly collapsed back to one sample is invisible in every other line it prints.

Two of the values are anchored differently and the output says which: `IMAGE` and the
repository it is derived from come from `go.mod`, and `FLOATING_TAG` comes from
`docker-compose.yml`. Those are constant across every planned run by construction - the
committed file IS the anchor, and there is no sample for a literal to coincide with - so the
gate names the file rather than claiming a variation that did not happen.

So the gate's own output says only what it checked. It used to end a green run with "the same
digest, not a rebuild" and with an order sentence describing "the re-smoke of the pulled
artefact" - two statements about what those scripts DO, over a question nothing asked. Gut
either script and both sentences still printed. They now name the step and the program it
invokes, and say where the behaviour is proved instead.

Where it is proved is `make release-shape-selftest`, which runs each of those scripts for
real against a recording stub - the same treatment `scripts/install-ffmpeg.sh` gets from
`make install-ffmpeg-selftest`. It asserts that the promotion retags the exact gated version
reference onto the floating one and nothing else, that the re-smoke pulls the pushed
reference back for BOTH architectures and drives the packaging gate over each, and that each
script's named failure modes exit with their own code. Each of those assertions is also run
against a deliberately gutted copy of the script, so an assertion that could not fail is not
counted as evidence.

**A control that was lost when those bodies moved out of `release.yml` into files, stated
rather than left to be found:** at impl-gate ordinal 1 of S0046 the gate caught a promotion
that re-pointed `:latest` at a locally built image the run never pushed, because the body was
then an inline `run:` the gate could compare. It is out of the gate's reach now, and the
self-test cases above are where that property lives instead.

It does NOT: dispatch anything, resolve anything against a live registry (that is
`scripts/resolve-compose-image.sh`, on the release itself), compare versions between runs,
know whether a GHCR package is publicly readable, read the bodies of the three release
scripts above, know whether the `Makefile`'s own `check:` target still does anything (only
that it still depends on `release-shape`), see what an EARLIER step in the same job did to
the environment a later one runs in (below), or know whether the GitHub repository has been
renamed - only that `go.mod`, `docker-compose.yml` and the workflow agree about the name it
will use.

**The third residue, and it is the largest one: a step can be neutered by a step above it,
and nothing structural can see that.** Everything this gate grades about a role step is that
step's OWN declared surface - its `run:` fields, the environment names in scope for it, the
values it is handed, its keys, `defaults:`, an action's inputs. GitHub Actions gives an
earlier step in the same job three ways past all of that. It can write `NAME=value` to the
file named by `$GITHUB_ENV`, or a directory to `$GITHUB_PATH`, and the runner applies either
to every LATER step in the job - `MAKEFLAGS=-n` through the first, or a `make` shim in front
of `PATH` through the second, and the full gate runs nothing while its step reads exactly as
it does today. It can also simply overwrite a file the later step depends on: a `check:`
target rewritten to `@true` is as total as either and touches no environment at all. This is
house style rather than obfuscation - `release.yml` already puts the pinned ffmpeg in front
of `PATH` that way, three steps above the gate.

Closing it means deciding what an ordinary `run:` script DOES, which is the question this
whole design exists because nobody could answer - eight fail-opens over six review ordinals,
by reading and by observing both - and which the conductor's capability ruling retires
outright. A substring test for `GITHUB_ENV` would be that reader again, and the Makefile case
proves it would not even be a complete one. So it is written down here instead of pretended
away. What it costs, precisely: a release can publish having gated LESS than the gate's own
output says it did. What it does not cost: nothing here can hand a dry run a credential - the
`build` job holds `contents: read` whatever an earlier step in it wrote - so the capability
split is untouched, and the reviewer of a change to `release.yml` is the control, which is
why every step in the publishing job has to be named in the table at the top of this file.
