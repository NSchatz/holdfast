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
the library's directories plus a fixed amount of work per file. The FEED asks one question
of every candidate before it spends a worker on it, and the answer decides which of two
per-file costs that candidate pays.

The question is **one store READ**, on the ordinary `(path, fingerprint)` index's leading
column: are there rows here that a claim would refuse because their recorded decision inputs
still match the configuration in force (`TerminalHolds`)?

Where there are none - an unseen file, or one whose verdict a configuration change has
re-opened - the file is handed to a worker and costs what it always did:

- **one filesystem attribute read** (`os.Stat`, which follows a symbolic link). It answers
  every pre-claim question about the file: whether there is a file at the other end of the
  path at all, the source's size, the fingerprint its row is keyed by, and the hard-link
  count.
- **one store READ**, of the withheld paths (`PathIsExcluded`).
- **one store CLAIM**, which for a file whose row is terminal and whose recorded decision
  inputs still match is a `BEGIN` / `SELECT` / `ROLLBACK` that writes nothing.
- **no store WRITE at all.** A file that needs nothing done issues no write statement.

Where there ARE such rows - which is every file of the steady state this document is about -
the feed takes **one filesystem attribute read** to confirm the file on disk is still the
file that row describes, and then hands it to nobody. The withheld-paths read and the claim
transaction are not issued at all, because no worker runs. A file whose bytes have changed
since that row keys to none of them and is offered exactly as an unseen file is.

So the steady state trades a claim transaction for an indexed read, and a first pass over an
unprocessed library pays one indexed read per file for the privilege. Neither is nested and
neither grows with the size of the library or of the ledger.

A queue order other than the default `path` adds **one further attribute read per
candidate**, taken by the enumeration to read that candidate's ordering key. It is the only
thing that ordering costs per file; see docs/enumeration.md for what it holds.

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
| 10,000 files | 104 us | 1.04 s | 3 | 100 us to 107 us |
| 100,000 files | 96 us | 9.63 s | 3 | 95 us to 100 us |

- **Hardware**: Intel Xeon E5-2680 v4 at 2.40 GHz, `GOMAXPROCS=5`, Linux amd64. The
  library and the ledger were both on ext4 over a software RAID array.
- **Build**: `3385c19`, on `sdd/S0095-holdfast-queue-ordering`. Go 1.25.14.
- **Date**: 2026-09-19.
- **Runs**: `-benchtime 1x -count=3`, each run over a freshly built library and a freshly
  seeded ledger; the spread is the range across those runs. The third 100,000-file run shared
  the host with this repository's own suite, which is the top of that row's spread and is why
  the spread is stated rather than the median alone.

A larger library costs LESS per file, not more, and that is the per-pass constants
amortising over more files rather than anything getting cheaper. It is also the answer to
the question this table exists to make askable: the per-file work is flat, so a pass over
ten times the library takes about ten times as long and no more.

### What it cost before

Each row here is the same benchmark against the build immediately before the change named
beside it, taken the same way, on the same machine as the figures above:

| build | library | per file | per pass | runs | spread |
|---|---|---|---|---|---|
| before the feed asked about a terminal row (`63b40ab`, 2026-09-18) | 10,000 files | 295 us | 2.95 s | 3 | 286 us to 309 us |
| before the feed asked about a terminal row (`63b40ab`, 2026-09-18) | 100,000 files | 218 us | 21.8 s | 2 | 216 us to 221 us |
| before the pre-claim path was consolidated (2026-09-18) | 10,000 files | 555 us | 5.55 s | 3 | 554 us to 557 us |

The second of those two changes is the one the top table measures. The feed now asks the
ledger whether a claim would be refused before it spends a worker, and for a library whose
every file is already decided the answer is yes for all of them: a `BEGIN`/`SELECT`/
`ROLLBACK` on the single write connection, and the withheld-paths read beside it, are
replaced by one indexed read. At 10,000 files the pass is 2.8 times faster than the row
above it and at 100,000 it is 2.3 times faster, and the work removed is per file, so it is
saved on every pass for ever.

What it costs is in the other direction and is stated rather than netted off: a pass over a
library with NO terminal rows now issues one indexed read per file that the build above did
not. `BenchmarkScan_EnumerationAtScale`, over a synthetic library where every file's
attribute read fails and no file is ever claimed, moved from 5.2 s to 7.6 s at 100,000 paths
and from 48.9 s to 70.0 s at 1,000,000 - about 24 microseconds per candidate. That benchmark
does no per-file work at all beyond the failed read, so the added read is the whole of what
it measures there; against a real first pass, where an offered file is probed and usually
encoded, it is not a figure an operator can see.

The oldest row took four filesystem attribute reads per file and issued three `DELETE`
statements per file, one per mutable guard, each matching no row. Its 100,000-file figure
was never taken: the comparison at 10,000 files is the same shape, and each run at the
larger size costs about eleven minutes of which nearly all is building the fixture.

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
