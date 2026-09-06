# The filesystem holdfast runs on

holdfast's whole promise is that it never destroys a source until the replacement
is provably faithful. That promise is stated against **local POSIX semantics**,
and three of the things it rests on are simply not true on a network filesystem:

- **A rename that fails did not happen.** On NFS you cannot assume that: "if the
  server does the rename operation and then crashes, the retransmitted RPC which
  will be processed when the server is up again causes a failure"
  (`rename(2)`). The swap can have happened while holdfast reports that it did
  not.
- **A stat can see a concurrent rewrite.** An NFS client refreshes a file's
  attributes only every few seconds, so the guard that re-checks a source
  immediately before the swap has a window measured in seconds, not
  microseconds.
- **The job store works at all.** SQLite is explicit: "WAL does not work over a
  network filesystem", and `jobs.db` is opened in WAL mode wherever `state_dir`
  points.

None of that is in holdfast's gift to fix. What it can do, and now does, is
**say so before it touches anything**: at startup it establishes what storage
every path it will act on sits on, tells you, and starts or refuses the whole run
in one decision.

## What gets checked

Every one of these is a **checked path**, and each gets its own classification
record at startup:

- every configured library root;
- the state directory (`state_dir`), or - where it does not exist yet - the
  storage it would be created on;
- every distinct mounted filesystem the startup walk finds beneath a configured
  library root, at any depth, whether or not a source lies on it.

A path is classified `local` only on a **positive** identification against the
set below. Anything else - a type this build does not recognise, a lookup that
fails, mount information that cannot be read, an overlay, an in-memory
filesystem, anything in user space (FUSE) - is `undetermined`, and
`undetermined` is **not local**. That is deliberate and it is the fail-safe
direction: a false warning costs you one line of configuration, and a false clear
costs you a film.

Three things that are **not** classifications, and are reported as themselves:
a configured library root that **does not exist** (reported as missing, never as
`undetermined` - there is no storage there to have a type, and nothing is wrong
with your filesystem); a path holdfast is **not permitted to inspect**; and a
path whose **resolved form cannot be established**, which holdfast cannot compare
against your `allow_non_local` entries and therefore will not start on. Each
refuses the run with its own cause and its own remedy, so you are never sent
looking for a storage problem that is not there.

## Filesystem types this build classifies local

A type is on this list only if, for every file on storage of that type, the
storage is attached to the host holdfast runs on and no other host can modify
that file through it.

```text
btrfs ext2 ext3 ext4 f2fs jfs xfs zfs
```

This build prints that same set at startup (`local_filesystem_types`), so you
never have to read source or rebuild to find out what a running binary
recognises. The two are held in agreement by the gate: a build whose set moves
with this file left unedited fails `make check`.

Not on the list, and deliberately: `overlay`, `tmpfs`, `ramfs` and any `fuse.*`
type. The name of such a filesystem does not by itself say what storage is
underneath it, and what is underneath may be a NAS.

## Opting in

When a checked path is not local, holdfast refuses to start and prints the exact
line that would permit it:

```yaml
allow_non_local:
  - /srv/media/tv
  - /var/lib/holdfast/state
```

It is **per path** and never a global switch: an entry covers the path it names
and no other, so opting the state directory in does not quietly opt a NAS mount
inside your library in too. Each entry must name a configured library root, the
state directory, or a path spelled **beneath a configured library root as you
configured it**; anything else is reported as malformed and refuses the run,
rather than being silently ignored. An entry that covers nothing - a typo beneath
a root, or a path that turned out to be local - is reported as unnecessary at
that same startup.

A covered path is not a fixed path. holdfast starts, and it says at startup that
the no-loss guarantee is **reduced** there. Nothing about the swap becomes safe
on a network filesystem; what changes is that you were told.

One refusal no declaration lifts: a path holdfast **cannot inspect** at all
(permission denied), and a configured library root it cannot **list**. An opt-in
permits a run on storage that is not local; it never permits a run on a path
holdfast cannot look at. A root whose contents are unknown to it would look
exactly like an empty library, so it refuses instead.

## What the startup check costs

The check is not free and it is not lazy, so here is the bill.

holdfast **traverses the directory tree beneath every configured library root**
at startup, **before the first encode** and before the job store is opened. It
visits directories, because that is the only way to meet a symbolic link or a
mount that sits several levels down; reading the mount table alone would miss
both.

A run that **is going to be refused pays that traversal in full first**. The
decision is taken once, over the whole set of checked paths together, so the walk
finishes before anything is decided. That is the price of naming every problem in
one refusal instead of making you fix one, restart, and find the next.

The cost **grows with the size of the directory tree** beneath your roots -
**every entry** in it is read - and not with the size or the content of the media
files: **no media file is opened** by this check, not one byte read, not one
probe run. A library of ten files and a library of ten thousand in the same tree
cost the same here. What makes this expensive is a deep or wide tree, not a big
one.

Two consequences worth knowing:

- On a cold cache over a slow network mount, startup can take a while. That is
  the traversal, not the encoder.
- The walk enters each region of storage **at most once**, so a bind mount that
  exposes a directory the walk already covered is reported and not descended
  again, and a bind loop or a symlink cycle terminates instead of running for
  ever. The same bound applies to the scan that follows: a source is enumerated
  only from a directory that this run's startup walk traversed successfully.
