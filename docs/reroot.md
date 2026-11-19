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
   evidence below.
4. **Apply it**, the same command with `--apply`. The rewrite is one transaction: it all lands, or
   nothing does.
5. **Start holdfast again.** The next scan finds the rows under the new root, and `validate` no
   longer reports them.

Do it **before** the first scan under the new root. A scan of the new root records rows at the new
paths, and a reroot refuses to carry an old row onto a key a new row already holds (below).

With the example compose file, `--from` and `--to` are the paths **holdfast sees**, inside the
container. If only the host side of a bind mount moved and the container path stayed the same, no row
names the host path and there is nothing to carry.

```bash
docker compose stop holdfast
docker compose run --rm holdfast reroot --config /config/config.yaml --from /media/old --to /media/new
docker compose run --rm holdfast reroot --config /config/config.yaml --from /media/old --to /media/new --apply
docker compose start holdfast
```

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
| `node_leases` | `path`, `temp_path` |

Nothing else about a row changes. Its `updated_at` and its schema version stamp stay as they were,
because a rename decided nothing and no field of the row was written by this build. Free-text columns
(a failure's `reason`, a swap's error) keep the words they were written with, including any path they
quote: they are what was said at the time. A test reads the ledger's schema and fails on any column
that is in neither the list above nor its list of columns that hold no path, so a new path column
cannot be added without being carried.

## The evidence that it is the same files

A reroot claims the files under `--to` are the files the ledger recorded under `--from`, so it checks
the files rather than trusting the flags. For every path it would rewrite that the ledger records a
file for, it reads the file now at the new path:

- **a media file** must match a `size:mtime` fingerprint the ledger records for that path: any of the
  path's `jobs` rows (a file with a superseded row beside its current one records two), or the swap's
  record of what it left there;
- **a retained original** must be the size of the original its retention recorded.

A path with no file at the new path is **fine**: a file deleted since its row was written is the
ordinary state of a ledger, and the row is carried as recorded. The reroot refuses when **no** recorded
file exists under `--to` (nothing shows it is the same library), and when **any** file that exists
there does not match (it prints a bounded sample, each with what was recorded and what is there now).
A file replaced since the move (an *arr upgrade, say) therefore refuses the reroot; the sample names it.

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
  path in its sweep, a lease's path at its epoch. Each collision is named. **Nothing is merged**: the
  pair are two records of one file, and which stands is not a decision this command takes.
- **The evidence fails**, as above.

Like every command that writes the ledger, a reroot first migrates a ledger an earlier build wrote,
and the log records each step.

## What it logs

One structured record per table (`component=ledger.reroot`, `table`, `rows`, and each path column's
count under `columns`), and one for the whole (`rows`, and the evidence counts `evidence_checked`,
`evidence_matched`, `evidence_absent`, `evidence_mismatched`), with `applied=true` for a rewrite and
`applied=false` for a dry run. A refusal is one `warn` record carrying its `reason` token:
`paths`, `to-not-a-root`, `from-is-a-root`, `no-ledger`, `ledger-in-use`, `ledger-unopenable`,
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
