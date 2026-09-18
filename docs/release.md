# Cutting a release

This is the operator runbook for the release path. It is written as the ordered procedure
the FIRST release went through and every later one repeats, because that order is the whole
safety property: each step says what leaves the machine, whether it can be undone and by
what. Most of the second half cannot.

Nothing in this repository proves the workflow still has the shape this document describes:
the mechanical gate over the shape of `release.yml` is retired, and `docs/test-mass.md`
carries the register and the reason. This runbook, and whoever reviews a change to
`release.yml`, are what hold it - see "What holds the release path" below for the properties
that review covers. Dispatching a workflow, renaming a repository, flipping its visibility
and pushing a tag were never machine acts here either. Those four are yours.

## Where this repository actually stands

**`v0.2.0` is the newest published version, and the run that published it reported failure
over a promotion that landed.** Read this before anything below: several of the steps are
already done, the ones that are done are in the irreversible set, and the last release
skipped its final two steps.

| fact | evidence |
|---|---|
| the dry run has been dispatched | run `29646149489`, `workflow_dispatch` on `main`, 2026-07-18 13:26Z, green. Its five publishing steps all show `skipped`. |
| `v0.1.0` has been released | run `29646482337`, tag push, 2026-07-18 13:37Z, green. Every step ran, including the promotion and the release. |
| `v0.2.0` has been released | run `34350141882`, tag push, 2026-09-09 12:17Z. Its `build` job is green; its `publish` job pushed the version tag, re-smoked the pushed artefact on both arches and promoted `:latest` - and then reported FAILURE at `publish/promote-latest` over a promotion that had landed. The run's conclusion is `failure`. |
| the newest image is published | `ghcr.io/nschatz/holdfast:v0.2.0` at `sha256:cb5125e1e95c93ee05256a37a8878c30349b75bb2ee2918c6e7b0ee9b4a5ec32` - the multi-arch index, `linux/amd64` and `linux/arm64` - pulled back and re-smoked on both arches by that run. `v0.1.0` stays published at `sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2`. |
| `:latest` points at it | `ghcr.io/nschatz/holdfast:latest` resolves to `sha256:cb5125e1e95c93ee05256a37a8878c30349b75bb2ee2918c6e7b0ee9b4a5ec32`, the same index digest as `v0.2.0`. The promotion in run `34350141882` therefore LANDED, which is what makes that step's failure false: the `imagetools create` retag took and the step died reading the inspect printed after it. |
| two steps of that release did not run | `publish/resolve-compose` and `publish/github-release` both show `skipped` on run `34350141882`, because the step before them reported failure. So no run has ever resolved the reference `docker-compose.yml` pins against the registry, and the `v0.2.0` release object was not cut by the workflow. |
| the example deployment pins the newest version | `docker-compose.yml` pins `ghcr.io/nschatz/holdfast:v0.2.0` by that index digest, so both architectures resolve under it. |
| a GitHub release exists for each | tag `v0.1.0`, cut by the workflow token, carrying `holdfast_v0.1.0_linux_amd64.tar.gz`, `holdfast_v0.1.0_linux_arm64.tar.gz` and `SHA256SUMS`. tag `v0.2.0`, cut BY HAND because `publish/github-release` was skipped, carrying the `v0.2.0` tarballs and `SHA256SUMS` taken from run `34350141882`'s own `build` job and verified against those checksums - those bytes, never a rebuild. |
| the repository is public | `NSchatz/holdfast`, `visibility: public`. |
| the annotated tags are on origin | `refs/tags/v0.1.0` -> `3468562c`, pointing at commit `38fb8b3`. `refs/tags/v0.2.0` -> `2b109243`, pointing at commit `f525a9e`. |

Re-derive any of it:

```sh
gh run list --workflow=release.yml --limit 10 --json conclusion,event,headBranch,createdAt
gh run view 34350141882 --json jobs --jq '.jobs[] | {name, steps: [.steps[] | {name, conclusion}]}'
gh api repos/NSchatz/holdfast/releases --jq '.[] | {tag_name, assets: [.assets[].name]}'
gh api repos/NSchatz/holdfast --jq '{name:.full_name, visibility:.visibility}'
docker buildx imagetools inspect ghcr.io/nschatz/holdfast:latest --format '{{.Manifest.Digest}}'
```

What this means for the steps below. Steps **1, 3 and 5** are done for good; they are
one-time acts and they have been taken. Steps **2 and 7** have each run twice and run again
per release. Step **8** has never been done, and run `34350141882` did not do it either -
the step that resolves the compose reference is one of the two that run skipped. So the NEXT
release is: dispatch the dry run (2), pick and choose a version HIGHER THAN `v0.2.0` (4, 6),
push that tag (7), confirm the pull (8), regenerate and commit the API surface baseline (9).
`v0.1.0` and `v0.2.0` are both spent: a released
version's contents "MUST NOT be modified" (semver.org), and nothing here can un-publish
either.

## The irreversible set

These are the acts that cannot be taken back by re-running anything.

**The first ten are every step of `release.yml`'s `publish` job**, and that is the whole of
the inventory rather than a selection from it. `release.yml` is split so that every step
which could publish lives in that one job, and that job is the only one granted a write
permission - `packages: write` to push the image, `contents: write` to cut the release. A
step in the `build` job holds neither, so it can say `docker push` in any spelling it likes
and publish nothing; a step in `publish` holds both, whatever it says it does. The inventory
is therefore THE JOB, not the steps' text, and this table names every id in it: adding a step
to `publish` means adding a row here. That is stronger than a catalogue of publishing
commands and it is also the only version that survived review: deciding what an arbitrary
shell script does is undecidable, and six attempts to do it were each beaten by ordinary
shell.

**Every act below has already been taken**: all of them on 2026-07-18 for `v0.1.0`, and
every one up to and including `publish/promote-latest` again on 2026-09-09 for `v0.2.0`,
whose run skipped `publish/resolve-compose` and `publish/github-release` (the release object
at that tag was cut by hand instead). See the section above. Read the table as the standing
description of what each act costs, not as a list of decisions still open.

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
  `packages: write` is the state this whole path is shaped to make impossible, so seeing it
  means the workflow on the default branch is not the one this runbook describes.

Undone by: nothing to undo; a dispatch publishes nothing. This step exists precisely so that
a change to the release path is exercised before a tag commits it.

## 3. The repository must already carry the name the example deployment pulls

`release.yml` never spells the image out. It derives it:
`ghcr.io/$(echo "$GITHUB_REPOSITORY" | tr '[:upper:]' '[:lower:]')`. So the GitHub
repository's name IS the published image reference, and the NAME half of what
`docker-compose.yml` pins has to be that same reference.

Check, before anything is published:

```sh
gh repo view --json nameWithOwner -q .nameWithOwner   # must be NSchatz/holdfast
go run ./scripts/compose-image-ref                    # ghcr.io/nschatz/holdfast:vX.Y.Z@sha256:…
make check                                            # refuses a floating or undigested pin
```

That second command is the ONE reader of the compose file's image reference, and it is the
same one `scripts/resolve-compose-image.sh` uses at release time. Do not grep for `image:`
instead: a second reader agrees on today's file and diverges on the shapes that matter.

Reading the two and comparing the NAME halves is YOURS: nothing here holds the compose
file's name against the repository's own. What `make check` still refuses, in
`scripts/check-pins.sh`, is a compose reference that carries no `@sha256:` digest, and one
that pins the tag a release MOVES: the example deployment pins a version, and `:latest` is
published rather than depended on.

Status: done. The repository is `NSchatz/holdfast` and `docker-compose.yml` pins
`ghcr.io/nschatz/holdfast:v0.2.0` by digest, which is the multi-arch index `v0.2.0` published
under.

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

`v0.1.0` and `v0.2.0` are both taken. An annotated `v0.1.0` is on origin (`3468562c`,
pointing at commit `38fb8b3`) and an annotated `v0.2.0` is too (`2b109243`, pointing at
commit `f525a9e`); a release run stands behind each, and a GitHub release and a published
image carry each name.

**Do not delete and re-create it.** The runbook used to say you could, on the premise that
nothing had consumed it; that premise is false. Semantic Versioning is explicit that "the
contents of that version MUST NOT be modified", and re-pointing the tag would do exactly
that: one version name, two different artefacts, with the first one already pulled and
already in the release object. Deleting the tag would not un-publish either of them - a
GHCR tag can be removed but not un-fetched, and the Go module proxy caches a version
permanently once anything requests it.

So:

- **Cut the next release at a HIGHER version** (`v0.3.0`). That is the only supported move.
- Do not re-push an existing tag. `release.yml` triggers on `push: tags: ["v*"]`, so
  re-delivering `v0.1.0` or `v0.2.0` would build and publish an image from that stale tree
  under a name that already means something else.
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

- Format: `v0.MINOR.PATCH`, optionally with a pre-release suffix (`v0.3.0-rc1`).
- A suffixed tag is a pre-release: it publishes the version tag and cuts a pre-release, but
  does NOT become `:latest`. Use one if you want a rehearsal that real users will not pull.
- Pick a version nothing has used. See step 4.

Undone by: nothing yet; choosing is free.

## 7. Push the tag

```sh
git tag -a v0.3.0 -m "v0.3.0"
git push origin v0.3.0
```

That is the whole trigger. There is deliberately no "publish" checkbox: the only thing that
can publish is a tag, so a release always carries a real version name.

**This is the second irreversible act, and it starts the other three.** The run then, in
this order: runs the full `make check`; builds and smokes both architectures; pushes the
version tag ONLY; pulls that artifact back for both architectures and re-smokes it;
promotes `:latest` onto the same digest; resolves BOTH `:latest` (which must now carry the
digest this run gated) and the reference `docker-compose.yml` pins (which must resolve to an
image at all) against the registry; cuts the GitHub release. `make check` refuses any
reordering of that, and refuses any step before the promotion being marked
`continue-on-error`.

If any gate or smoke run fails, the run stops there and `:latest` stays exactly where it
was: the `publish` job `needs: build` and its `if:` calls no status function, so GitHub does
not start it when what it needs failed - and it is the only job that holds a write
permission, so nothing else could publish in its place. On the first release
`:latest` did not exist yet, so a failure would have left nothing published at all; from
now on it means `:latest` keeps resolving to the previous release, which is the property
`make check` asserts by re-deciding every guard with the run marked failed.

Status: done twice. For `v0.1.0` on 2026-07-18 (run `29646482337`), which ran in exactly the
order above. For `v0.2.0` on 2026-09-09 (run `34350141882`), which ran that order as far as
the promotion and then reported FAILURE at `publish/promote-latest` over a promotion that had
landed - `:latest` carries the `v0.2.0` index digest - so `publish/resolve-compose` and
`publish/github-release` were skipped and the `v0.2.0` release object was cut by hand
afterwards. Doing it again means a higher version, per step 4.

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
reference the published compose file pins, tag and digest together. Every release from now
on asserts it on its own (`scripts/resolve-compose-image.sh` runs after the promotion, fails
the release if `:latest` does not resolve to the digest the run just gated, and fails it if
the reference `docker-compose.yml` pins does not resolve at all), so this is confirmation,
not the only check.

Status: NOT confirmed. `v0.1.0` predates that step, and on `v0.2.0` the step was one of the
two that run skipped after the promotion reported a failure it had not suffered, so no run has
ever resolved the compose reference. A GHCR package carries its OWN visibility, separate from the repository's, and it
is not readable from a plain `gh` token - so whether a stranger with no credentials can pull
`ghcr.io/nschatz/holdfast` is exactly what this step, and only this step, settles. If
it 401s or 404s, the package is still private: link it to the repository and set it public
in the package settings. Nothing above proves this one.

Then point it at a real library and let it run. That is the north star, and until this step
passes on someone else's machine, nothing above proves it.

Undone by: nothing to undo.

## 9. Regenerate the API surface baseline and commit it

`docs/api-schema.json` is the committed record of the HTTP surface of the LAST RELEASED
version, and `make api-schema-diff` compares every later build against it on every
`make check`. Until this step runs, that record still describes the PREVIOUS release, so
every addition made since it shipped goes on being reported as an addition - which is
harmless but is not what the file claims to be.

From a clean checkout of the tag you just pushed:

```sh
git fetch --tags && git checkout v0.3.0
make api-schema-baseline      # prints the endpoint count and the version it recorded
```

The command writes the file and then reads back what it wrote, failing if the result does
not parse as the document the gate consumes or records no version. The version it records is
`git describe`, so run it ON the tag: a run from an untagged commit records a development
build's identity and the file then names a version nobody released.

Commit the result to the default branch, on its own, with a message naming the tag:

```sh
git checkout main && git checkout v0.3.0 -- docs/api-schema.json
git commit -m "chore: record the v0.3.0 HTTP surface baseline" docs/api-schema.json
```

This is the step the release workflow cannot take for you: a tag-triggered run has
`contents: read` in the job that could do it and cannot push to the default branch, and the
job that holds a write grant is the one that publishes. So it is yours.

Also clear `.api-schema-breaks.yaml` of any entry this release has now carried. The gate
fails on a record whose difference the tree no longer has, so a stale entry reds the next
`make check` rather than sitting there as a standing exemption - but deleting it here is
what makes that a tidy-up instead of a surprise.

Undone by: `git revert` of that commit. Nothing has left the machine - the file is a record
this repository keeps about itself, and no published artefact reads it. It is the only step
after the tag push that is fully reversible.

## Known limits

### Backport tags move `:latest` backwards

`release.yml` records this and does not fix it. A backport tag (`v0.2.4` cut after
`v0.3.0`) is not a pre-release, so the promotion would move `:latest` back onto the older
release. Until the promotion is gated on a version comparison, cut backports from a
maintenance branch and accept it, or push the backport as a pre-release so it never
promotes. Nothing catches this: no check in this repository compares the version a run is
publishing with the version `:latest` already points at.

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

The HTTP surface is now enumerated and drift-gated: the running server generates it at
`GET /api/schema`, `docs/api-schema.json` records the last release's copy, and
`make api-schema-diff` refuses a breaking change to it on every `make check`. That is one of
the three. The configuration keys and the metric names are still neither enumerated nor
drift-gated, and declaring the set stable means doing for them what step 9 and that gate do
for this one, and only then editing the refusal in `release.yml`'s plan step. Until those
records exist, a non-zero major would be a promise nobody could check.
## What holds the release path, now that no gate does

A committed program used to decide, on every `make check`, that `release.yml` still had the
shape this document describes. It is retired: its subject was the shape of a workflow file
rather than the transcode-and-delete behaviour this repository exists to be trusted about,
and that grading belongs to review (`docs/test-mass.md` carries the register and the
reason). What it asserted is written here instead, as the properties a reviewer of
`release.yml` holds it to. Nothing below is checked by a machine.

**A dispatch publishes nothing, because it holds no credential.** `release.yml` is split so
that every step which could publish lives in the `publish` job. That job is the only one
granted a write permission, and its `if:` runs it only when the planning step says a tag is
being released. The `build` job - the whole of a dry run - holds `contents: read`, so a step
in it may say `docker push` in any spelling, quoting or nesting at all and publish nothing:
the registry refuses a read-only token. The workflow's top-level `permissions:` is
`contents: read`, so a job that forgets to declare its own inherits read rather than write.

That split is the design, and it replaced a question nobody could answer. Six adversarial
reviews found six fail-opens in one direction in a reader that tried to decide whether a
step's text publishes: a publishing input the decision could not see, detectors that could
not cross a shell line continuation, a command catalogue that knew spellings and no
destination, a push inside a quoted `sh -c "..."` or `eval "..."`, and buildx's attached
shorthand `-otype=registry` read as "local". Running each step in a stubbed environment
moved the hole rather than closing it: every control such an environment has is an ordinary
shell object the step owns, `export PATH=/usr/bin:/bin` is one line, and `exec docker push`
never reaches a recorder at all. **Do not reintroduce a reader of step text.** If a step
needs to publish, put it in the job that holds the grant and give it a row in the table at
the top of this file. If it must not publish, put it in the job that holds none.

**What a reviewer checks, on any change to `release.yml`:**

- `on:` admits a tag push and a dispatch, and nothing else. A `push:` filter that admits a
  branch is the trap: `branches: ["v0.**"]` beside `tags: ["v*"]` makes a push to a BRANCH
  named `v0.9.9` an event whose `ref_name` is `v0.9.9`, which the planning logic - which
  cannot tell a branch from a tag - reads as a release. A bare `on: push` carries no filter
  at all, and a `workflow_dispatch:` with `inputs:` is how a publish tick-box gets added.
- No job that can run on a dispatch holds any `write` scope, declares an `environment:`,
  takes `secrets: inherit`, or carries registry `credentials:` on a `container:` or
  `services:` block. Each of those hands over something `permissions:` does not bound.
- The order in `publish` is: push the version reference, pull it back, smoke it on both
  architectures, and only then move `:latest` onto the digest that was smoked. The promotion
  is last and it is skipped for a pre-release. Nothing before it tolerates its own failure
  with `continue-on-error:`.
- Every step in `publish` has a row in the irreversible-set table above, by its `id:`.
- Each value a step is HANDED names this run's own planning output rather than a literal.
  `REF: ${IMAGE}:latest` on the re-smoke is one line that leaves the order, the grant and
  the step names untouched while the release pulls back and smokes the PREVIOUS release,
  and then promotes `:latest` onto the one it never smoked. `IMAGE:
  ghcr.io/nschatz/holdfast` written into the release notes is the same edit again. A literal
  that happens to equal what a real run produced is the failure mode a sample cannot catch,
  which is why the rule is "it must BE the output", not "it must look right".
- `FLOATING_TAG` is the one value a release DECLARES rather than derives, and it is a
  literal on purpose: `release.yml` names it once and `scripts/release-promote.sh` and
  `scripts/resolve-compose-image.sh` read it from there. It must not be the tag
  `docker-compose.yml` pins - retagging the version the example deployment pins would leave
  that file's tag and digest disagreeing the day the next release lands, and would modify
  the contents of an already-released version, which semver.org forbids outright.

**What a reviewer cannot see, stated rather than left to be found.** An earlier step in a
job can neuter a later one: writing `MAKEFLAGS=-n` to `$GITHUB_ENV`, or a `make` shim
directory to `$GITHUB_PATH`, makes the full gate run nothing while its step reads exactly as
it does today, and overwriting the `check:` target is as total as either. This is house
style rather than obfuscation - `release.yml` already puts the pinned ffmpeg in front of
`PATH` that way, three steps above the gate. What it costs, precisely: a release can publish
having gated less than its own log says it did. What it does not cost: nothing there can
hand a dry run a credential, because the `build` job holds `contents: read` whatever an
earlier step in it wrote.

**The example deployment's image reference has exactly ONE reader**, and it is
`scripts/compose-image-ref`. `scripts/resolve-compose-image.sh` asks that program rather
than parsing `docker-compose.yml` a second time, because two readers agree on today's file
and diverge on a quoted scalar, a second service with an `image:`, or an `image:` key nested
outside `services:` - the same "held in step by hope" shape the ffmpeg pin is parsed out of
the Dockerfile to avoid.

**What the release SCRIPTS do is proved by running them, not by reading them.**
`scripts/release-promote.sh`, `scripts/release-resmoke.sh` and
`scripts/resolve-compose-image.sh` are the bodies of three publishing steps. Each carries
its own named failure modes and its own exit code per mode; the promotion retags the exact
gated version reference onto the floating one and nothing else, and the re-smoke pulls the
pushed reference back for BOTH architectures and drives the packaging gate over each. They
live in files rather than inline in `release.yml` so each step invokes one program a
reviewer can read whole.
