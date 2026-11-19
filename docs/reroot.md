# Moving a library root: `holdfast reroot`

Every path the ledger records is absolute, and names the library root as the configuration
wrote it when the row was recorded. When the same files are mounted at a new path and
`library_roots` is edited to match, nothing on disk changed, but every row still names the old
tree:

- a scan never enumerates the old paths, so each file is decided again under its new name. A file
  whose encode failed deterministically (a VMAF score below the floor, a stream the output lost) is
  encoded and fails again, and every done and skipped verdict is taken again;
- a retained original (the undo window's second link) recorded under the old path is out of reach of
  `holdfast restore` and of the release at the start of each pass.

`holdfast validate` and every daemon start report this as terminal rows that "lie under no configured
library root", with an example path. `holdfast reroot` is the remedy when that root **moved**: it
rewrites every path the ledger records under the old root to the same place under the new one.

It is not for a root you removed or a library you replaced. Rows under a root that is gone for good
change nothing a scan does, and are left where they are.

## How to move a root

1. **Stop holdfast.** The reroot refuses while anything else has the ledger open: a `serve` or
   `run` daemon, or another holdfast command. While the reroot runs it holds the ledger exclusively,
   so a daemon started meanwhile cannot read it until the reroot is done.
2. **Edit `library_roots`** to name the new path, and nothing else about that root.
3. **Look first.** Without `--apply` nothing is rewritten:

   ```bash
   holdfast reroot --config config.yaml --from /media/old --to /media/new
   ```

   It prints, per ledger table, how many rows and which path columns it would rewrite, and the
   evidence below. It changes nothing, not even the ledger's schema version.
4. **Apply it**, the same command with `--apply`. The rewrite is one transaction: it all lands, or
   nothing does.
5. **Start holdfast again.** The next scan finds the rows under the new root, and `validate` no
   longer reports them.

**Run it before a holdfast that scans the new root first starts.** The first scan of the new root
records a row for every file there, under the file's own fingerprint, and a reroot refuses to carry an
old row onto a key a new row already holds (below): every file unchanged since the move would collide.
That includes the first start of an upgraded build, so stop the daemon, upgrade, edit
`library_roots`, reroot, and only then start it.

**If the old tree still exists, let the reroot see it.** Where the old path is still mounted - an old
mount point left in place beside the new one - the reroot proves the move file by file: one file at
both paths is one entry, the same device and inode. It can only do that where it can read the old
path, so in a container bind the old tree **read-only** for the run as well as the new one.

With the example compose file, `--from` and `--to` are the paths **holdfast sees**, inside the
container. If only the host side of a bind mount moved and the container path stayed the same, no row
names the host path and there is nothing to carry.

```bash
docker compose stop holdfast
docker compose run --rm -v /srv/media-old:/media/old:ro holdfast \
  reroot --config /config/config.yaml --from /media/old --to /media/new
docker compose run --rm -v /srv/media-old:/media/old:ro holdfast \
  reroot --config /config/config.yaml --from /media/old --to /media/new --apply
docker compose start holdfast
```

(`/srv/media-old` is the old tree on the host; leave the `-v` out where it no longer exists.)

### Previewing over a ledger an earlier build wrote

A dry run changes nothing, the schema version included, so over a ledger an earlier build wrote it
refuses rather than upgrade it: upgrading stamps a version that build's daemon then refuses to open.
To see what the reroot would do anyway, run it with `--apply` against a **copy** of the ledger. With
holdfast stopped, copy `jobs.db` (and a `jobs.db-wal` beside it, if there is one) into a scratch
directory and point the reroot at that directory:

```bash
mkdir /tmp/reroot-preview
cp <state_dir>/jobs.db* /tmp/reroot-preview/
HOLDFAST_STATE_DIR=/tmp/reroot-preview holdfast reroot --config config.yaml \
  --from /media/old --to /media/new --apply
```

It migrates and rewrites only the copy, and reads the library exactly as the real run will. Delete
the copy afterwards.

## What it rewrites

Every column whose value is a library path, where the value is `--from` itself or lies under it.
The comparison is on **path components**, never a string prefix: `/media/old2/film.mkv` is not under
`/media/old` and is left alone.

| table | path columns |
|---|---|
| `jobs` | `path`, `target_path`, `library_root`, and each published sidecar's `path` inside `subtitle_sidecars` |
| `retained_originals` | `source_path`, `swapped_path`, `retained_path` |
| `swap_incidents` | `source_path`, `replacement_path` |
| `path_exclusions` | `path` |
| `health_checks` | `path` |
| `playback_swaps` | `path` |
| `playback_sessions` | `path` |
| `node_leases` | `path`, `temp_path` |

Nothing else about a row changes. Its `updated_at` and its schema version stamp stay as they were,
because a rename decided nothing and no field of the row was written by this build. Free-text columns
(a failure's `reason`, a swap's error) keep the words they were written with, including any path they
quote: they are what was said at the time. A test reads the ledger's schema and fails on any column
that is in neither the list above nor its list of columns that hold no path, so a new path column
cannot be added without being carried.

## The evidence that it is the same files

A reroot claims the files under `--to` are the tree `--from` was: a **move** of one filesystem, not a
copy of it and not another tree laid out alike. So it checks the files rather than trusting the flags.
Presence alone cannot tell a move from a copy - a copy of a few recorded files under `--to`, with the
old tree still in place, matches every size and mtime it holds - so every recorded file is read at
**both** paths, the old and the new:

- **at both paths**: a host can have the old root still mounted, as a second mount of the same
  filesystem. Then each file is ONE entry with two names, the same device and inode at both paths, and
  that is the strongest evidence there is: it counts as matched, whatever has happened to the file
  since. Two different entries is a **copy**, and refuses. The entries are compared without following
  a symbolic link, so a `--to` tree of links into a still-present `--from` is a copy too: a link is not
  the file it points at.
- **only at the old path**: a partial copy, or the wrong `--to`. It refuses, for an ordinary row and a
  retained original alike: carrying the row would point it at nothing while the file it describes is
  still where it was, and a retained original's record carried to an empty path is the next restore's
  "unrestorable" under a configured root, which drops the record and orphans the original.
- **only at the new path** (the old mount is gone): judged by what the ledger recorded. A media file is
  **matched** when it matches a `size:mtime` fingerprint recorded for that path (any of the path's
  `jobs` rows - a file with a superseded row beside its current one records two - or a swap's record
  of what it left there, in the retention or in the playback evidence), and otherwise **replaced since**: files are replaced under the same name all
  the time (an *arr upgrade of an episode, a film acquired again). A retained original is matched when
  it is the size of the original its retention recorded.
- **at neither path**: deleted since its row was written.

Then two shares decide, and both must hold beside at least one match:

- the **match ratio**, matched over matched plus replaced, at least `--min-match` (default `0.95`). An
  unrelated tree matches almost nothing; the same tree has replaced only a few.
- the **coverage**, matched over every recorded file checked, deleted ones included, at least
  `--min-coverage` (default `0.5`). A tree that shares a few files and lacks the rest has a perfect
  ratio and is still not the library that moved; a library loses files between a row being written
  and a move, but not most of them.

Each threshold takes any value above 0 and at most 1. One match is enough as the floor under them
because a match is not a coincidence: the same relative path, the same size to the byte, the same
modification second.

**Replaced and deleted files are carried as recorded**, rewritten to the new path with every other
column as it was. A `jobs` row is keyed by its path **and** its fingerprint, so a replaced file's old
row is history and holds nothing: the next scan claims the file at that path as the new file it is,
exactly as it would have without the move. Neither collides with anything.

**Retained originals are strict.** A retained original found under `--to` that is not the size of the
original its retention recorded (or is not a regular file) refuses the reroot, whatever the shares:
`holdfast restore` renames that file over a library file.

**A retained original missing under `--to` needs the old path, or an acknowledgement.** For a retention
the undo window still holds, absence under `--to` is a finding only where the reroot could look at
`--from` - it is a directory this process sees and holds recorded files, so an old mount point left
empty does not count - and found the file gone there too. Otherwise the original may be out of sight
rather than gone (the documented container run mounts only the new root), and carrying the record
would hand the next `holdfast restore` a path with nothing at it, which drops the record and orphans
the original. So the reroot refuses, naming each such record, unless the old tree is made visible to
it (above) or `--carry-missing-retained` acknowledges that those originals are gone: their records are
then carried as recorded, and the undo window drops them as missing, as it does any retention whose
file is gone. A **restored** retention never counts: its retained file was renamed back over the source
by the restore, so its absence is the record's own account of it. The report says whether the
identity check ran, and why not when it did not.

The report prints the counts, both shares beside their thresholds, and a bounded sample of the
replaced files (`replaced since (carried as recorded)`), each with what was recorded and what is there
now. A refusal names a bounded sample of the files it refused on.

## What it refuses

Every refusal exits non-zero, names its reason, and rewrites nothing.

- **`--to` is not a configured library root.** The point is a ledger the scan reaches, so the new path
  is in `library_roots` first.
- **`--from` overlaps a configured root**: it is one, contains one, or lies inside one. Rows a scan
  still reaches are not stranded, and carrying a tree that holds a configured root would carry that
  root's rows with it. `--from` is the old path, the one `library_roots` no longer names.
- **The ledger is open in another process.**
- **Nothing is recorded under `--from`.** A reroot that reported success over no rows would be
  indistinguishable from one that carried them.
- **A rewritten row would take a key a row under `--to` already holds.** The key is the table's own:
  a `jobs` row's path and fingerprint, a retention's source path, a withheld path, a health check's
  path in its sweep, a playback swap's path and fingerprint, a playback session's path with its source
  and session id, a lease's path at its epoch. Each collision is named. **Nothing is merged**: the
  pair are two records of one file, and which stands is not a decision this command takes.
- **The evidence fails**, as above: a recorded file is a different file at each path or is only at
  the old one, a retained original is not the recorded size, no recorded file matched, or the coverage
  or the match ratio is under its threshold.
- **A retained original the undo window holds is missing under `--to`** and `--from` could not be
  checked, without `--carry-missing-retained` (above).
- **A dry run meets a ledger an earlier build wrote.** A dry run changes nothing, the schema included:
  upgrading the ledger stamps a version the holdfast that wrote it then refuses to open. Preview it on a
  copy instead (above). With `--apply` the reroot migrates it first, as `restore` and `requeue` do, and
  then refuses or rewrites.

With `--apply`, like every command that writes the ledger, a reroot first migrates a ledger an
earlier build wrote, and the log records each step.

## What it logs

One structured record per table (`component=ledger.reroot`, `table`, `rows`, and each path column's
count under `columns`), and one for the whole (`rows`, the evidence counts `evidence_checked`,
`evidence_matched`, `evidence_same_file`, `evidence_replaced`, `evidence_absent` and
`evidence_retained_mismatched`, `evidence_identity_checked`, `evidence_from_witnesses` and
`evidence_retained_carried_missing`, `match_ratio` beside `min_match`, and `coverage` beside
`min_coverage`), with `applied=true` for a rewrite and `applied=false` for a dry run. A refusal is one
`warn` record carrying its `reason` token: `threshold`, `paths`, `to-not-a-root`, `from-is-a-root`, `no-ledger`, `schema-behind`, `ledger-in-use`, `ledger-unopenable`,
`plan-failed`, `nothing-recorded`, `collision`, `evidence` or `apply-failed`.

## The undo window before a reroot

Until the reroot, a retention recorded under the old root has no file at its recorded path. That is
not taken as the file being gone: a record whose retained original is missing **and** lies under no
configured library root is **kept**. The release at the start of each pass counts it and logs a
`warn` naming `holdfast reroot` instead of dropping it, and `holdfast restore` refuses it, keeping the
record, and names the same command. After the reroot both reach it at the new path
([docs/undo.md](undo.md#when-the-window-closes)).

It is LOCAL by design, never an HTTP endpoint, for the reason `restore` and `requeue` are not
([docs/design/web-ui.md](design/web-ui.md#controls)).
