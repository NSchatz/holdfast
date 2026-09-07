# Cutting the first release

This is the operator runbook for taking holdfast from "never published" to "a stranger can
`docker compose pull` it". Run it in order. Every step below says whether it can be undone
and by what, because most of the second half cannot.

`.github/workflows/release.yml` has never executed. `scripts/release-shape-gate` (inside
`make check`) proves the workflow still has the shape this document describes, by running
its planning logic for both event shapes rather than reading its comments; `make
release-shape-selftest` proves that gate still bites. Neither of them can dispatch a
workflow, rename a repository, flip its visibility or push a tag. Those four are yours.

## The irreversible set

These are the acts that cannot be taken back by re-running anything. The first three are
steps in `release.yml` and are checked against this table by `make check`: add a publishing
step to that workflow and the gate reds until this table names it.

| act | what leaves this machine | can it be undone? |
|---|---|---|
| `image-push@push-the-multi-arch-image-version-tag-only` | the multi-arch image, at `ghcr.io/<owner>/<repo>:<version>`, pullable the moment it lands | No. A GHCR tag can be deleted, but not un-fetched: anyone who pulled it, and any cache that mirrored it, keeps the bytes. Deleting it also breaks the compose files of anyone who pinned it. Supersede it with a higher version instead. |
| `tag-move@promote-latest-to-the-gated-image` | `:latest` starts resolving to that digest, for every user running `docker compose pull` | Partly. `:latest` can be re-pointed at an older digest by re-running `docker buildx imagetools create`, but everyone who pulled in between already has the new image, and containers already recreated from it stay recreated. |
| `github-release@cut-the-github-release` | a GitHub release, its notes, and the source/binary tarballs | Partly. The release object can be deleted; the tarballs it served, the notification it sent to watchers, and any mirror of the tag cannot. |
| the git tag itself | an annotated tag on origin, which the Go module proxy may fetch | No, in the sense that matters. `git push --delete` removes the ref, but the Go module proxy caches a version permanently once anything requests it, and a released version's contents "MUST NOT be modified" (semver.org). `retract` in `go.mod` does not unpublish either: the Go modules reference is explicit that retracted versions "should remain available in version control repositories and on module proxies". |
| the repository rename | the module path, the image reference and every link that names the repository | No. Go has **no module-path rename primitive**, nothing rewrites an image reference sitting in a user's compose file, and GitHub's redirect from the old name survives only while the old name is left permanently vacant. This is why the rename is part of THIS set and not a cosmetic follow-up: after the first tag it can no longer be done cleanly at all. |
| making the repository public | the whole history, every commit message, every file ever committed | No. Anything fetched, forked, mirrored or indexed while it is public stays fetched. Making it private again removes access, not copies. |

Everything before step 5 below is reversible. Nothing from step 5 on is.

## 1. Land this branch first

`workflow_dispatch` is offered by GitHub only for a workflow already on the DEFAULT
branch, so the dry run in step 2 cannot be run from a pull request. Merge first, then
dispatch.

Undone by: reverting the merge. Nothing has left the machine yet.

## 2. Dispatch the dry run, and read its artifacts

Actions -> Release -> Run workflow, on the default branch. `workflow_dispatch` is always a
dry run: the planning step sets `publish=false`, and every publishing step is guarded on
it. It still does everything else, which is the point.

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
- Confirm **nothing** was published: the four steps below `--- everything below this line
  only happens on a real release ---` all show as skipped, and `ghcr.io/<owner>/<repo>` has
  no package.

Undone by: nothing to undo. This step exists precisely so that the first execution of the
release path is not the one that publishes.

## 3. The repository must already carry the name the example deployment pulls

`release.yml` never spells the image out. It derives it:
`ghcr.io/$(echo "$GITHUB_REPOSITORY" | tr '[:upper:]' '[:lower:]')`. So the GitHub
repository's name IS the published image reference, and `docker-compose.yml` names
`ghcr.io/nschatz/holdfast:latest`.

Check, before anything is published:

```sh
gh repo view --json nameWithOwner -q .nameWithOwner   # must be NSchatz/holdfast
grep -n '^ *image:' docker-compose.yml               # ghcr.io/nschatz/holdfast:latest
make check                                            # the gate that holds those two together
```

`make check` fails if they disagree, and prints both. It derives the expected reference
from `go.mod`'s module path, so it is checking the same fact the workflow will use, not a
copy of it.

If the repository is still named something else, rename it NOW. After the first tag it is
in the irreversible set above and cannot be done cleanly: the old module path is already in
the proxy, and images published under the old name stay published. When you do rename,
leave the old name permanently vacant. GitHub redirects it, but reclaiming the old name
kills the redirect and silently serves a different repository in its place.

Undone by: renaming back, while nothing is published. After step 5, not at all.

## 4. Deal with the `v0.1.0` tag that is already on origin

An annotated `v0.1.0` already exists on origin. It points at `38fb8b3`, which predates
work that has landed since, and it matches `release.yml`'s `push: tags: ["v*"]` trigger. It
was created before the release workflow existed, so it has never run one.

It is a loaded gun: re-push it, or push anything that re-delivers it, and the release path
builds and publishes an image from that stale tree.

Decide explicitly, and do it before step 6:

- **Preferred:** leave it alone and cut the first real release at a HIGHER version
  (`v0.2.0`). Nothing re-delivers an existing tag, so an untouched `v0.1.0` publishes
  nothing.
- If you want the first release to BE `v0.1.0`, delete it on origin first
  (`git push --delete origin v0.1.0`) and re-create it on the commit you are releasing.
  Only safe because nothing has ever consumed it - do not do this to a tag that has been
  published.

Undone by: re-creating the tag at the same commit, as long as no release has run on it.

## 5. Make the repository public

The image inherits the repository's visibility: while the repository is private, GHCR
serves the package only to authenticated users, so a stranger cannot pull it and the north
star is not testable.

**This is the first irreversible act.** Everything in the history becomes public at once.

Undone by: nothing. See the table above.

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
was, because every publishing step is guarded on the run's success. On the FIRST release
`:latest` does not exist yet, so a failure leaves nothing published at all.

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
reference the published compose file names. The release itself already asserted this
(`scripts/resolve-compose-image.sh` runs after the promotion and fails the release if that
reference does not resolve to the digest the run just gated), so this is confirmation, not
the only check. Every release after the first one enforces it on its own.

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

`make release-shape-selftest` defeats each of those on purpose against a mutated copy of
the repository and fails if any defeat did not run.

It does NOT: dispatch anything, resolve anything against a live registry (that is
`scripts/resolve-compose-image.sh`, on the release itself), compare versions between runs,
or know whether the GitHub repository has been renamed - only that `go.mod`,
`docker-compose.yml` and the workflow agree about the name it will use.
