# What a scan pass costs per file

A `scan_interval_sec` daemon spends almost all of its life scanning a library it has
already processed. Every file in it carries a terminal row, every pass reaches the answer
the last one did, and the whole of that pass is overhead. This is what that overhead
measures, what it scales with, and how to measure it again.

It is a REPORT and not a gate. No build fails on a number here: run-to-run variation on a
shared machine makes a threshold on an absolute figure false-positive often enough that the
gate would be ignored inside a month, which is worse than having none.

## What the cost scales with

**Linear in the number of files enumerated, and in nothing else.** One pass is one walk of
the library's directories plus a fixed amount of work per file, and the fixed part is:

- **one filesystem attribute read** (`os.Stat`, which follows a symbolic link). It answers
  every pre-claim question about the file: whether there is a file at the other end of the
  path at all, the source's size, the fingerprint its row is keyed by, and the hard-link
  count.
- **one store READ**, of the withheld paths (`PathIsExcluded`).
- **one store CLAIM**, which for a file whose row is terminal and whose recorded decision
  inputs still match is a `BEGIN` / `SELECT` / `ROLLBACK` that writes nothing.
- **no store WRITE at all.** A file that needs nothing done issues no write statement.

Nothing here nests and no per-file work grows with the size of the library or of the
ledger. What a pass costs beyond the per-file figure is fixed: one `RecoverStale`, the undo
window's expiry sweep, the temp sweep, once per pass whatever the library holds.

The store work is SERIALIZED. The ledger is opened with a single write connection
(`SetMaxOpenConns(1)`), deliberately, because the claim's mutual exclusion is the only
thing standing between two workers encoding one source. So the per-file figure is dominated
by two SQL statements queueing on that one connection rather than by the filesystem.

## The figures

`BenchmarkScan_NoOpPass`, over libraries whose every file already carries a terminal row
that still re-derives: exactly the steady state described above.

| library | per file | per pass | runs | spread |
|---|---|---|---|---|
| 10,000 files | 295 us | 2.95 s | 3 | 286 us to 309 us |
| 100,000 files | 218 us | 21.8 s | 2 | 216 us to 221 us |

- **Hardware**: Intel Xeon E5-2680 v4 at 2.40 GHz, `GOMAXPROCS=5`, Linux amd64. The
  library and the ledger were both on ext4 over a software RAID array.
- **Build**: `63b40ab`, on `sdd/S0098-holdfast-per-file-scan-cost`. Go 1.25.14.
- **Date**: 2026-09-18.
- **Runs**: `-benchtime 1x -count=3`, each run over a freshly built library and a freshly
  seeded ledger; the spread is the range across those runs. The 100,000-file figure rests
  on TWO runs and not three: the third lost its temporary directory to something else
  tidying up on the machine it shared. Two is what that row is worth, which is why the
  count is in the table.

A larger library costs LESS per file, not more, and that is the per-pass constants
amortising over more files rather than anything getting cheaper. It is also the answer to
the question this table exists to make askable: the per-file work is flat, so a pass over
ten times the library takes about ten times as long and no more.

### What it cost before

The same benchmark against the build immediately before the pre-claim path was
consolidated, taken the same way, on the same machine, on the same day:

| library | per file | per pass | runs | spread |
|---|---|---|---|---|
| 10,000 files | 555 us | 5.55 s | 3 | 554 us to 557 us |

That build took four filesystem attribute reads per file and issued three `DELETE`
statements per file, one per mutable guard, each of them matching no row. At 10,000 files
the pass is now 1.9 times faster; the work removed is per file, so it is saved on every
pass for ever. The 100,000-file figure was not taken against that build: the comparison at
10,000 files is the same shape, and each run at the larger size costs about eleven minutes
of which nearly all is building the fixture.

The removal is also visible without a stopwatch, which is the more reliable of the two
statements: `TestScan_IssuesNoStoreWriteForATerminalFile` and
`TestScan_ReadsFileAttributesOncePerFile` COUNT the writes and the reads rather than timing
them, because on a warm page cache elapsed time cannot tell one read from three.

## Running it again

The benchmark is deliberately NOT part of `make check` - the `test` target runs no `-bench`
- because a figure with a spread is not a pass or a fail. Run it by hand:

```sh
go test -run '^$' -bench '^BenchmarkScan_NoOpPass$' -benchtime 1x -count=3 ./internal/engine/
```

It builds its own libraries of 10,000 and then 100,000 files under the benchmark's own
temporary directory, and removes each one as soon as that size is finished rather than at
the end of the run. Before it writes a single file it asks the filesystem whether it can
take the library, and it FAILS rather than fills when the answer is no: a leaked library of
100,000 files on a small shared temporary filesystem reds unrelated suites with errors that
read as code defects rather than as a full disk.

Where the temporary directory is on a filesystem that cannot take it, or is one somebody
else prunes, point the fixture at one that can:

```sh
go test -run '^$' -bench '^BenchmarkScan_NoOpPass$' -benchtime 1x -count=3 ./internal/engine/ \
    -args -scan-fixture-dir=/var/tmp
```

Most of the wall time is the fixture and not the measurement: seeding 100,000 terminal rows
through the engine's own write path takes minutes, and all of it is outside the timer.

## Reading a new figure against these

A figure means something only against a stored previous figure taken the same way. Record
the machine, the build, the date, the number of runs and the spread beside any figure added
here, and state a change against the row above it rather than against a threshold. A single
run reported without its spread says less than no figure at all, because it invites a
comparison the measurement cannot support.
