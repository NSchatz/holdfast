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

These are the acts that cannot be taken back by re-running anything. The first three are
steps in `release.yml` and are checked against this table by `make check`: add a publishing
step to that workflow and the gate reds until this table names it.

**Every row below has already been taken**, on 2026-07-18, for `v0.1.0` (see the section
above). Read the table as the standing description of what each act costs, not as a list of
decisions still open.

| act | what leaves this machine | can it be undone? |
|---|---|---|
| `image-push@push-the-multi-arch-image-version-tag-only` | the multi-arch image, at `ghcr.io/<owner>/<repo>:<version>`, pullable the moment it lands | No. A GHCR tag can be deleted, but not un-fetched: anyone who pulled it, and any cache that mirrored it, keeps the bytes. Deleting it also breaks the compose files of anyone who pinned it. Supersede it with a higher version instead. |
| `tag-move@promote-latest-to-the-gated-image` | `:latest` starts resolving to that digest, for every user running `docker compose pull` | Partly. `:latest` can be re-pointed at an older digest by re-running `docker buildx imagetools create`, but everyone who pulled in between already has the new image, and containers already recreated from it stay recreated. |
| `github-release@cut-the-github-release` | a GitHub release, its notes, and the source/binary tarballs | Partly. The release object can be deleted; the tarballs it served, the notification it sent to watchers, and any mirror of the tag cannot. |
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
dry run: the planning step sets `publish=false`, and every publishing step is guarded on
it. It still does everything else, which is the point.

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
- Confirm **nothing new** was published: every step below `--- everything below this line
  only happens on a real release ---` shows as **skipped**. That is the assertion, and it is
  the only one available now that `ghcr.io/nschatz/holdfast` exists: "the package is absent"
  stopped being a check on 2026-07-18. If any of those steps ran, stop - a dispatch that
  publishes is the state `make check` refuses, so seeing it means the workflow on the
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
was, because every publishing step is guarded on the run's success. On the first release
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
decides from the values it produces that: a dispatch publishes nothing; the promotion comes
last, after the gate, both smokes, the push and the re-smoke; no published act runs once
something has failed; nothing before the promotion tolerates its own failure; a non-zero
major is refused; this table names every publishing step; and the reference
`docker-compose.yml` gives users is the one a release of THIS repository promotes.

"A dispatch publishes nothing" is decided rather than read, on both sides of a step: the
guard, and every action input that can make a step publish on its own. `docker/build-push-
action` has TWO of those, and its own input table says they are one act spelled two ways:
`push` is "shorthand for `--output=type=registry`", and `outputs` is the list of output
destinations. So `outputs: type=registry` - and `type=image,name=...,push=true`, the
spelling in the action's own multi-platform example - publish exactly as hard as
`push: true` does, with no `push:` key present at all. Both are decided, and the step
publishes if either says so.

GitHub also lets either input be an expression - `push: ${{ github.event_name !=
'pull_request' }}` is the action's own documented idiom - so it is evaluated for the event
under test, through the same evaluator as the guards. Anything the gate cannot decide (an
unknown context, a function it does not implement, a value that is not a boolean, a buildx
exporter it does not model) reds it by name. Nothing is read as "this step is harmless": a
step wrongly called publishing costs a runbook entry, one wrongly called harmless is an
unreviewed publish. The same rule holds for the references a push publishes, which are read
from `tags:` and from an `outputs:` entry's `name=`; a push whose references the gate cannot
read reds rather than being assumed to name nothing.

**Extending this:** an act is a property of a step's DEFINITION, so model every input
through which an action can perform it. Modelling one and inferring the act's absence from
that key's absence is how a dry run that pushed to GHCR came to be reported as "NONE of them
publishes anything", and it is why the input catalogue is a set rather than a key.

The example deployment's image reference has exactly ONE reader, in that gate.
`scripts/resolve-compose-image.sh` asks for it (`release-shape-gate -print-compose-ref`)
rather than parsing `docker-compose.yml` a second time, because two readers agree on today's
file and diverge on a quoted scalar, a second service with an `image:`, or an `image:` key
nested outside `services:` - the same "held in step by hope" shape the ffmpeg pin is parsed
out of the Dockerfile to avoid.

`make release-shape-selftest` defeats each of those on purpose against a mutated copy of
the repository and fails if any defeat did not run.

It does NOT: dispatch anything, resolve anything against a live registry (that is
`scripts/resolve-compose-image.sh`, on the release itself), compare versions between runs,
know whether a GHCR package is publicly readable, or know whether the GitHub repository has
been renamed - only that `go.mod`, `docker-compose.yml` and the workflow agree about the
name it will use.
