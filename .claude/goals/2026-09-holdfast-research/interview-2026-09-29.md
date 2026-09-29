# Interview log, holdfast program (2026-09-29)

Verbatim except identity scrubbed (brief I1): the owner's name is replaced by "the owner" or `[owner]`.

## Round 1 (2026-09-29)
Q1 Direction: Public v1.0 release / Running on my library / Tdarr feature parity / Research and propose -> **Tdarr feature parity**
Q2 SDD workflow relation: Program replaces it / Program feeds it / Both side by side -> **Program replaces it**
Q3 Autonomy: One checkpoint after g1 / Review after every goal / Checkpoints at engine changes -> **One checkpoint after g1**
Q4 Size: As big as needed / About 4 / About 8 -> **As big as needed**

## Round 2 (2026-09-29)
Q1 Accelerators (multi): NVIDIA / Intel iGPU / AMD / None -> **NVIDIA GPU (NVENC), Intel iGPU (QSV/VAAPI), AMD GPU (AMF/VAAPI)**
Q2 Other programs: Independent run beside / Shared Needs the owner list / Run after them -> **Independent, run beside**
Q3 Contract: Inherit unchanged / with changes / walk through -> **Inherit unchanged**
Q4 Old roadmap: Mark superseded, keep S-names in history / Program edits umbrella / Keep S-numbers -> **Mark superseded, keep S-names in history**

## Round 3 (2026-09-29)
Q1 HW testing: NVIDIA via --gpu, Intel/AMD the owner's / No live GPU in goals / All three reachable -> **No live GPU in goals**
Q2 HW gates: Same gates no exceptions (Recommended) / Per-encoder VMAF targets / Research and propose -> **Research and propose**
Q3 HW fallback: Fall back to software / Skip the file / Configurable per library -> **Configurable per library**

## Round 4 (2026-09-29)
Q1 Encode (multi): QSV/VAAPI/AMF in the image / Hardware decode (-hwaccel) / Auto HW detect + fallback / H.264 / more output codecs -> **all four**
Q2 Streams (multi): Audio transcode/downmix / Subtitle extraction / Crop / black-bar detection / Dolby Vision / HDR10+ transcode -> **all four**
Q3 Platform (multi): Web UI / Distributed worker nodes / Plugin / flow system / Native Plex/Jellyfin/*arr clients -> **Native Plex/Jellyfin/*arr clients, Distributed worker nodes, Web UI**
Q4 Ops (multi): Priority / savings-rate queue / Library health-check mode / Tdarr migration importer / None -> **Priority / savings-rate queue, Library health-check mode**

## Round 5 (2026-09-29)
Q1 Node swap: Server gates and swaps (Recommended) / Workers gate, server swaps / Research and propose -> **Server gates and swaps (Recommended)**
Q2 Node media: Shared mount / Server streams files / Both, configurable -> **Both, configurable**
Q3 Web UI: Read-mostly no build step / Full SPA / Research and propose -> **Full SPA**
Q4 Audio (multi): Re-encode lossless/bulky / Add stereo downmix / Keep original alongside / Normalise loudness -> **all four**

## Round 6 (2026-09-29)
Q1 Audio conflict: Replace by default, keep opt-in / Keep by default, replace opt-in / Per-codec rule -> **Replace by default, keep opt-in**
Q2 SPA stack: Svelte + Vite, pnpm, embedded / React + Vite / Research and propose -> **Svelte + Vite, pnpm, embedded**
Q3 SPA home: web/ + Playwright / web/ unit tests only / Separate repo -> **web/ in this repo, unit tests only**
Q4 Auth: Existing tokens + node token (Recommended) / Add user login / Research and propose -> **Existing tokens + node token (Recommended)**

## Round 7 (2026-09-29)
Q1 Subtitles: Text subs to sidecars, keep in file / Extract and strip / Extract + OCR image subs -> **Text subs to sidecars, keep in file**
Q2 DV/HDR10+: HDR10+ and DV P8 via libx265 / Also convert P7 to P8.1 / Research and propose -> **Also convert P7 to P8.1**
Q3 Crop: Opt-in, consensus-sampled (Recommended) / On by default / Research and propose -> **Opt-in, consensus-sampled (Recommended)**
Q4 Codecs (multi): H.264 (libx264 + HW h264_*) / AV1 HW (qsv/vaapi/amf) / VP9 / VideoToolbox -> **H.264 (libx264 + HW h264_*), AV1 HW (qsv/vaapi/amf)**

## Round 8 (2026-09-29)
Q1 Clients (multi): Plex rescan+watched / Jellyfin rescan+watched / Sonarr/Radarr rescan+import hooks / Tautulli stays as is -> **Plex: rescan + 'being watched', Sonarr/Radarr: rescan + import hooks**
Q2 Health: Scheduled sweep report only / Sweep + quarantine / Sweep + repair -> **Scheduled sweep, report only**
Q3 Active specs: Fold all / Fold p2 drop p3 / Goal 1 triages, proposes / Ignore -> **Goal 1 triages, proposes**
Q4 Priority (multi): Savings-per-hour / Per-path/rule priority / Bump from UI/API -> **Per-path/rule priority, Savings-per-hour ordering**

## Round 9 (2026-09-29)
Q1 Other repos: super+homelab read-only / super read, homelab PR-never-merge / Both PR-never-merge / Never touch -> **super read, homelab PR-never-merge**
Q2 PR #94: Goal 1 merges it first / I merge it before goal 1 / Part of the triage -> **Part of the triage**
Q3 Cleanup (multi): Docs/code drift / Dead code and branches / Dash rule enforcement / Mutation domain widening -> **Docs/code drift, Dead code and branches**
Q4 Deletions: Yes those 6 branches + 1 tag / Only the 2 merged / No, Needs the owner -> **Yes, those 6 branches + 1 tag**

## Round 10 (2026-09-29)
Q1 Merge gate: Local gate + green CI (Recommended) / Local gate only / Wait for CI on engine changes -> **Local gate + green CI (Recommended)**
Q2 Releases: Never; Needs the owner / Pre-releases only / Yes, minor releases -> **Yes, minor releases**
Q3 Attribution: CLAUDE.md wins: no trailer / Harness trailer allowed -> **CLAUDE.md wins: no trailer**
Q4 Gate budget: <=20 min / <=35 min / No budget, just report -> **No budget, just report**

## Round 11 (2026-09-29)
Q1 Machine: 2 agents, GOMAXPROCS/-p 2, heavy lock / 3 agents / 1 agent -> **2 agents, GOMAXPROCS/-p 2, heavy lock**
Q2 Private (multi): LAN addresses and hostnames / Library contents / Account IDs / tokens / Name/email beyond authorship -> **all four**
Q3 Needs the owner: NEEDS-[owner].md in repo; days / GitHub issues; days / NEEDS-[owner].md; same day -> **NEEDS-[owner].md in repo; days**
Q4 Results: Script writes a report, you commit / You paste into a session / Research and propose -> **Script writes a report, you commit**
Note (planner, after the round): the option text named HOLDFAST_GATE_JOBS, but HOLDFAST_* is holdfast's koanf config prefix (internal/config/config.go:36), so the committed variable is GOFLAGS=-p=2 plus GOMAXPROCS=2 instead. Inferred; shown at Checkpoint T.

## Round 12 (2026-09-29)
Q1 Order: Foundations first (Recommended) / Value first / Risk first -> **Foundations first (Recommended)**
Q2 AMD/AMF: AMD via VAAPI (Mesa) in image / Research and propose / Separate AMF image variant -> **Research and propose**
Q3 Node proto: HTTP+JSON on existing server / gRPC / Research and propose -> **Research and propose**
Q4 Success (multi): Released version with all areas / Updated comparison.md / Real-hardware reports / Invariant untouched -> **Released version with all areas, Real-hardware reports**

## Round 13 (2026-09-29)
Q1 Proposals: All in goal 1 (Recommended) / Just-in-time + finale list / Add a second checkpoint -> **All in goal 1 (Recommended)**
Q2 Live svc: Never live; Needs the owner / Local containers OK -> **Never live; Needs the owner**
Q3 Docs: docs/design/*.md with anchors / ADRs in docs/adr/ / Only in brief/ledgers -> **docs/design/*.md with anchors**
Q4 CLAUDE.md: Keep under ~150 lines / No limit / Keep under 200 lines -> **Keep under 200 lines**

## Round 14 (2026-09-29)
Q1 Slow gate: Keep it; batch PRs (Recommended) / Local fast subset + CI full / Full local only on engine PRs -> **Keep it; batch PRs (Recommended)**
Q2 Interfaces (multi): The web UI / CLI + YAML in git / JSON API / Prometheus / Claude Code skill -> **The web UI, JSON API / Prometheus**
Q3 Sources: Primary sources first / Anything, cited -> **Primary sources first**
Q4 Anything else: Nothing else / Yes, see my notes -> **Nothing else**
Interview closed after 14 rounds, 55 questions.
