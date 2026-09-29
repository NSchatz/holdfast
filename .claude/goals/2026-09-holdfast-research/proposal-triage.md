# P1 - triage of the active specs and PR #94 (T30, T33)

## What this decides

For each of the 20 approved specs in the umbrella repo's `pipeline/active/` (S0151 and
S0162-S0180) and for holdfast PR #94 (S0159), this proposal says **keep** (and which of goals
2-14 carries it, by topic), **merged** (into an area an owner decision already settled), or
**drop**, with the reason and file:line evidence from `main`. The owner decides at Checkpoint T
(T30, T33, T48). Nothing here is built before that approval, and PR #94 stays unmerged until
then. Result: 20 keep, 1 merged (S0179 into goal 10), 0 drop.

## How it was triaged

Each spec's objective, scope, acceptance criteria and spec-gate verdict were read in the
read-only umbrella clone (read 2026-09-29). Every gap a spec names was then checked against
holdfast `main` (read 2026-09-29; `30d245f`, whose code is identical to `3c229da` because the
two newer commits touch only `.claude/`). A row is **drop** only if the gap is already closed in
code, a T row contradicts it, or it is obsolete. None qualified: every gap is still present,
and no T row contradicts a spec. Routing follows the goal sections of the brief (§6-§18), the
decisions T14, T28, T31 and T33, the inferred row I12, and review finding L3 (read 2026-09-29).
The one-line gists in `review-holdfast.md` section 6 were the starting point (read 2026-09-29).
Private data some specs quote (library paths, a time zone, host hardware, counts from the
owner's install) is summarised generically here.

## Rows

| Item | Gist | Verdict | Carried by | Reason |
|---|---|---|---|---|
| S0151 | `dependency-update-bot`: a Renovate config that opens (never merges) a PR when any pin goes stale, plus a hand-run coverage check that proves every pin is detected | keep | goal 2 (carried specs: pins) | Still needed: no update-bot config exists (`.github/` holds only `workflows/`). It fits no feature goal. Goal 2 already moves the runtime base pin, so this lands after that move. Installing the hosted app is the owner's act (spec AC-10), and it is neither one of the four NEEDS-OWNER kinds of brief §0.6 nor something a goal may do (§0.7 lets goals create nothing on GitHub beyond branches, PRs and minor tags), so the owner decides it at Checkpoint T: install the app, or approve a built-in alternative that needs no install (for example Dependabot for the actions, images and Go modules, with `pin-health.yml` keeping the ffmpeg pin). Goal 2 commits the configuration either way. The coverage check stays out of `check:`, so the gate stays network-free. Goals 8 and 13 add pin classes the spec's seven do not list (the metadata tool pins, the Node stage image, pnpm). The finale lists those as a follow-up instead of widening goals 8 and 13. |
| S0162 | `vmaf-log-off-tmpfs`: the per-frame VMAF log is written to the process temp dir (a tmpfs in the shipped compose file) and read whole into memory | keep | goal 4 (fidelity gate) | Still open: `internal/vmaf/vmaf.go:345` creates the log with `os.CreateTemp("", ...)`, `:371` reads it whole and `:376` unmarshals it. `docker-compose.yml:90-94` still says `/tmp` holds the VMAF log. Goal 4 owns the gate path. The fix must keep the pooled scores bit-identical (spec AC-3, AC-4), so it moves no floor and T10's default holds. It is separate from the encode memory watchdog on `main` (S0157), which bounds the encode, not the log. |
| S0163 | `workers-scale-cpu-quota`: `workers: auto` sized from the cgroup CPU quota, and several jobs in flight made safe on one drive | keep | goal 2 (carried specs: local worker pool) | Partly done: S0158 added the per-filesystem in-flight hold for the beside-the-source check (`internal/engine/source_room.go:96-125`), and S0161's quota reader exists (`internal/cpuquota/cpuquota.go`). Still open: `internal/config` has no `workers: auto` and no `cores_per_worker`. The scratch pre-check compares one statfs with one source size and counts nothing in flight (`internal/engine/scratch.go:151-164`). The working path is built from the stem and the output extension, and whatever sits at the first free candidate is removed (`internal/engine/swap.go:65-67`, `internal/engine/engine.go:3172-3187`), so `ep.mkv` and `ep.mp4` can collide. The retention-area prune race is still there (`internal/engine/undo.go:161-164`, `:205`). This is the LOCAL pool, not worker nodes (review L3). It should land before goal 11 multiplies the jobs in flight. AC-10 to AC-12 narrow to the scratch check, because the beside-the-source half already exists. |
| S0164 | `savings-per-hour-queue`: a `queue_order` value that ranks candidates by estimated bytes saved per hour of encode and verify | keep | goal 9 (queue priority) | T31 names S0164, and goal 9 builds this order. Not in code: `internal/config/queueorder.go:22-32` holds only the five existing values. The spec's membership rules become goal 9's acceptance for the order: every order offers the same set, each file once; a candidate whose key cannot be read goes last but is still offered; no per-file estimated saving is published (`README.md:225`). Goal 9 must settle one naming difference and record its choice: the spec says `savings_per_hour`, the brief says `savings_rate`. |
| S0165 | `per-rule-encoder-selection`: a resolution rule may name its own `encoder`; the VMAF floors stay per root | keep | goal 5 (hardware selection) | Not in code: `internal/config/rules.go:66` says `encoder` does not vary by band, and `docs/profiles.md:300` says a rule may not move the encoder. Goal 5's `encoder: auto` and per-library `hw_fallback` (T11, T12) decide which encoder a job gets, so a rule's encoder should go through the same availability probe and fallback. The floors stay on the root (spec AC-6, AC-7), which matches T10's default that the existing gates apply unchanged to hardware output. The spec's codec check across `max_height` bands (AC-16) must agree with goal 6's codec-family skip rules. |
| S0166 | `restart-survey-overcount`: the startup ledger survey counts rows on height-banded roots as taken under a moved configuration | keep | goal 2 (carried specs) | Still open: `DecisionInputsPerPath` (`internal/engine/inputs.go:190-214`) keeps the root's own profile for a root whose rules need a source height (lines 202-204). Every banded row is therefore resolved against the wrong band and announced as about to be re-decided. Reporting only (the survey re-opens nothing), and no feature goal owns the survey. |
| S0167 | `scan-records-source-facts`: skipped rows record the source codec and dimensions the probe already read | keep | goal 14 (history view) | Still open: `because` (`internal/engine/engine.go:2991-3000`) builds a skip outcome with dimensions but no `SourceCodec`, and the codec is written only on the dry-run row (`internal/engine/engine.go:1911`). The visible result is `/api/history` (spec AC-8), which goal 14's history view reads, so it joins the S0169-S0172 API-facts group. No new probe and no migration. |
| S0168 | `prune-excluded-directories`: the startup walk skips directories an `exclude_paths` pattern reaches | keep | goal 2 (carried specs) | Still open: the statement check at `internal/startup/filterdocs.go:65` still requires the docs to say that an excluded directory is still listed. The spec's example uses one of the owner's library paths; this row keeps the rule and leaves out the path. It lands before S0180 (the same walk; S0180's AC-6 names the pruned case) and before goal 9's health sweep walks the library. |
| S0169 | `summary-per-root-totals`: `/api/summary` gains per-root candidate bytes, projected savings, held bytes and free space, plus the documented top-level held figure | keep | goal 14 (summary and savings view) | Still open: the held figure is carried only by the SSE snapshot (`internal/server/hub.go:399`); `handleSummary` (`internal/server/server.go:243-265`) does not serve it, although `docs/api-reference.md:13` said it did. Goal 1's T34 pass (PR #98) corrected that line to match the code, so S0169 restores the field and the documentation line together. Goal 14 names S0169-S0172, and its summary view needs these figures. The schema change is additions only. |
| S0170 | `history-filter-paging`: `/api/history` gets a terminal-status filter and an opaque cursor | keep | goal 14 (history view) | Still open: the handler reads only `limit` (`internal/server/server.go:327`). Goal 14's history view "with filters and paging" needs exactly this contract: a status filter, a cursor bound to that filter, a 400 error body, and a schema diff with additions only. |
| S0171 | `queue-row-decision-facts`: an in-flight queue row shows the current attempt's source size, codec, dimensions and root | keep | goal 14 (queue view) | Still open: the claim sets these columns to NULL (`internal/store/sqlite.go:250`, `:383-387`), and nothing writes them before the terminal write, so an encoding row shows nulls. The queue view needs the running job's size and root. No new field and no migration. |
| S0172 | `would-transcode-state-label`: on a live engine, dry-run rows are counted as `pending` in the summary map, the SSE snapshot and the queue-depth gauge | keep | goal 14 (summary view) | Still open: the queue-depth gauge emits every stored status as stored (`internal/metrics/metrics.go:330-338`), and the summary copies the store's map (`internal/server/server.go:243-265`). No `dry_run` input reaches either. The change is read-side only; rows keep their stored status. It must land no later than the summary view, or the UI shows a dry-run bucket on a live engine. |
| S0173 | `run-mode-progress`: the oneshot `run` prints position, speed and ETA for each encode about every 30 s | keep | goal 2 (carried specs) | Still open: only `serve` attaches an Observer (`cmd/holdfast/main.go:1050-1066`). T53 makes the UI and API the daily surface, which makes this less important but does not remove the need: `run` binds no listener, so the UI cannot show a oneshot run, and `run` is the proving pass S0174 serves. It moves `run` encodes onto the progress-pipe path, so it lands before goal 3 writes its golden argv. |
| S0174 | `limit-encodes-and-queue-order`: `run --limit-encodes N` counts only encodes; `run --queue-order` overrides the order for one run | keep | goal 9 (queue priority) | Still open: `cmd/holdfast/run_bounded.go:57` defines only `--limit`. The override accepts whatever `queue_order` accepts, so it lands with goal 9's new value. It is a per-run CLI override of the whole order, not a per-item bump from the UI or API (T31's not-chosen option, §0.8). A `--limit-encodes` run is a bounded run, so it inherits PR #94's owner-checked sweep, which lands first in goal 2. |
| S0175 | `server-warning-scope`: limit the read-API exposure notices to the modes that can actually know them | keep | goal 13 (web UI serving) | Still open: both notices come from `Config.Notices()` (`internal/config/config.go:1628-1636`), and `validate` prints them (`cmd/holdfast/main.go:179`) while `run` and `serve` log them (`cmd/holdfast/main.go:950`), whatever the mode. Goal 13 changes what `/` serves (the embedded UI beside the source offer), and one of these notices states what `/` serves without a credential, so the scoping and that wording land together. |
| S0176 | `log-timezone-offset`: every log time field carries a numeric UTC offset, never `Z` | keep | goal 2 (carried specs) | Still open: the logger is a plain `slog.NewTextHandler` with no time formatting (`internal/logging/logging.go:23`), so a UTC process prints `Z`. Only the holdfast half is in scope. The runbook half (setting `TZ` on the documented `docker run` lines) belongs to the homelab repo: either a PR-never-merge there under T32, or the owner's own change. |
| S0177 | `working-file-extensions`: the in-progress encode and the retained original get non-video final extensions, and both name generations stay recognised | keep | goal 2 (carried specs) | Still open: `tempPath` (`internal/engine/swap.go:65-67`) ends the working file in the output video extension, and the retained original keeps its source extension (marker at `internal/engine/undo.go:50`), so a media server that scans by extension can list either one. It goes in goal 2 for two reasons. It changes the encode's output path and muxer selection (spec AC-4), which goal 3's golden argv must capture. And goal 10's post-swap Plex refresh makes Plex scan the folder while a retained original is there. It must be rebased against PR #94 (both touch the stale-temp sweep). |
| S0178 | `swap-mtime-choice`: flip the `preserve_mtime` default to `false` (a swap publishes a fresh mtime) and state the effective choice at `validate` and at startup | keep | goal 10 (media-server clients) | Still open: `PreserveMtimeEnabled` returns true when the key is unset (`internal/config/config.go:771`), and `config.example.yaml:562` says it defaults to on. It reverses an earlier umbrella ruling (default on) at the owner's own later request (an approved spec); no T row decides it. It shares goal 10's topic: how a media server and tools that detect changes learn that a file changed after a swap. Flag for Checkpoint T: an install that never set the key will change behaviour on upgrade. I5's rule that an existing config yields the same decisions covers new transformations, not this flip, so the owner should confirm it. |
| S0179 | `post-swap-rescan-hook`: optional Radarr/Sonarr rescan and Plex partial scan after a swap | merged | goal 10 (media-server clients) | T14 says the native clients cover S0179, and T28 decides the same rescans (goal 10 items 2-3), so the decided area includes it. No client exists in code (no media-client package under `internal/`). Goal 10 takes over the spec's safety criteria. The hook is off by default. A failed request never touches the job, the swap, the undo window or the exit code. There is one attempt per target, run off the workers, with a bounded queue and a bounded drain. The owner of a file is found by directory through a path map. No resolved credential appears in any log line. The deployment note covers the arr custom-format re-download risk. Goal 10 then adds what T28 adds: the Plex playing hold and the webhook intake. Jellyfin and Emby stay out (T28, §0.8). |
| S0180 | `census-scope-parity`: `plan --json` publishes, per root, the filters in force and the library size before and after them | keep | goal 2 (carried specs) | Still open: `plan` has no per-root scope section (`excluded_by_path_filter` appears nowhere in `cmd/holdfast`), and `analyze` counts filtered files as sources. Reporting only. It lands after S0168 in the same goal. The homelab census side stays a homelab item; the owner library paths the spec quotes are not repeated here. |
| PR #94 (S0159) | `bounded-run-temp-sweep`: owner records under `<state_dir>/temp-owners/` with a lock held while the temp exists; a bounded run removes only temps whose owner is provably dead | keep | goal 2 (cherry-pick per I12) | Open and not a draft. The build, mutation, package and GitGuardian checks all passed; 10 files, +1673/-40. Its base is `3c229da`, which is still `main`'s code (since then `main` has changed only by `.claude` commits). Carried per I12: cherry-picked onto `holdfast-g2/s0159-temp-sweep`, gated, and merged as a new PR; #94 is closed with a link, and its branch is left and listed as a follow-up. It goes first among goal 2's rows, because S0177 and S0174 build on its sweep. |

## Per goal

- **Goal 2** (carried specs and the debian13 base): PR #94 (S0159), S0151, S0163, S0166,
  S0168, S0173, S0176, S0177, S0180. Suggested order: PR #94, S0177, S0163, S0173 (all before
  goal 3's golden argv), then S0168, S0180, S0166, S0176, and S0151 after the base-image move.
- **Goal 3** (encode plan): none.
- **Goal 4** (fidelity gate and per-encoder quality): S0162.
- **Goal 5** (hardware runtime, argv and detection): S0165.
- **Goal 6** (hardware decode, new encoders, reports): none.
- **Goal 7** (audio and subtitles): none.
- **Goal 8** (dynamic HDR and crop): none.
- **Goal 9** (queue priority and health sweep): S0164, S0174.
- **Goal 10** (media-server clients): S0178, S0179 (merged).
- **Goal 11** (worker nodes: protocol, shared mount): none (S0163 lands first, in goal 2).
- **Goal 12** (worker nodes: streaming, TLS): none.
- **Goal 13** (web UI: toolchain, gate, embed): S0175.
- **Goal 14** (web UI: views and controls): S0167, S0169, S0170, S0171, S0172.

Total: 21 rows (9 + 1 + 1 + 2 + 2 + 1 + 5).

## Options

- **(a) The recommended routing (the table above).** Each spec goes to the goal whose topic it
  belongs to, and goal 2 takes the rest. Cost: goal 2 carries nine PR-sized items besides the
  base-image move, which pushes against I13's one-day goal size. The reporting rows routed to
  goal 14 (S0167, S0169-S0172) leave their defects on `main` for most of the program.
- **(b) Fold everything into goal 2.** Cost: about 20 PRs in one goal, far past I13. Specs whose
  design depends on a decided area would be built before that area exists and then reworked:
  S0164 before T31's priority design, S0165 before `encoder: auto`, S0179 before the T28
  clients. The "triage rows" slot in goals 4-14 goes unused. Benefit: every defect is fixed at
  the earliest point, and there is one place to sequence them.
- **(c) Drop every spec no decision names.** Keep only S0164 (T31), S0179 (T14, merged) and
  PR #94 (T33), and drop the other 17. Cost: defects with evidence on `main` stay:
  - the whole-file VMAF log in RAM-backed temp space (S0162);
  - playable working and retained files that media servers can see (S0177);
  - working-path collisions and uncounted scratch space once `workers` exceeds 1 (S0163);
  - false survey counts (S0166);
  - `Z` clocks beside local-time logs (S0176).

  Owner-requested, approved work (S0174, S0178) would be discarded with no reason from the
  owner, and T30 asked for a decision per spec, not a blanket one. Benefit: the smallest
  program.

## Recommendation

Adopt option (a): 20 rows keep and 1 merges (S0179 into goal 10's clients). Nothing is dropped,
because every spec's gap is still on `main` and no T row contradicts one. PR #94 is
cherry-picked first in goal 2 (I12). At Checkpoint T, five items need the owner's attention:

- S0178 flips a shipped default, so installs that never set `preserve_mtime` change behaviour.
- S0164 and the brief name the new order value differently (`savings_per_hour` against
  `savings_rate`).
- S0151 needs the owner to install the hosted app, which no goal may do and which is not a
  NEEDS-OWNER kind: install it, or approve a built-in alternative that needs no install.
- S0176's runbook half belongs to the homelab repo.
- Goal 2 is large: nine items besides the base-image move. The owner may move S0180 and S0173,
  which add the least, to a later goal in the approval.

## Sources

- The program brief `.claude/goals/2026-09-holdfast.md`: §0.7, §0.8, §1 (T1-T55, I1-I20), §2,
  §3.2, §5 item 6, §6-§18, and §23 finding L3. Read 2026-09-29.
- `.claude/goals/2026-09-holdfast-research/review-holdfast.md` section 6 (open work, one gist
  per spec). Read 2026-09-29.
- Umbrella repo `super` at `4104b5d9` (read-only clone, push URL disabled): for S0151, S0159 and
  S0162-S0180, `pipeline/active/<id>/spec.md`, `gate-state.json` and `verdict-spec-1.md`, plus
  S0176's `children/holdfast.md`. Read 2026-09-29.
- Umbrella `documentation/operator-decision-holdfast-backlog-review.md` and
  `documentation/conductor-ruling-holdfast-backlog-coherence.md` (ruling 7, the earlier
  `preserve_mtime` default). Read 2026-09-29.
- Umbrella `documentation/known-defects.md` (no entry bears on the active holdfast specs). Read
  2026-09-29.
- holdfast `main` at `30d245f` (code identical to `3c229da`): every file:line cited in the
  rows. Read 2026-09-29.
- holdfast PR #94: `gh pr view 94` (state, checks, base and head) and `gh pr diff 94
  --name-only`, read only. Read 2026-09-29.
