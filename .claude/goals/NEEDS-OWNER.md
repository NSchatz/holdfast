# NEEDS-OWNER - holdfast program (2026-09)

Steps only the owner can take: a run on real GPU hardware, a live check against the owner's Plex,
Sonarr or Radarr, merging the homelab PR, committing a hardware report. Nothing else goes here
(brief §0.6). Goals search this list before adding, and never wait on it.

Order: safety first, then what unblocks the most.

| # | Added by | What to do (exact command) | Tool | Expected result | What changes with the answer | State |
|---|---|---|---|---|---|---|
| 1 | goal 2 (S0176, homelab half) | Review and merge NSchatz/homelab#208 (the runbook's `HF` one-shot `docker run` shorthand gains `-e TZ=` set to the compose file's committed default). In a homelab clone: `git fetch origin && git switch holdfast-g2/s0176-runbook-tz && make ci`; if green, `gh pr merge 208 -R NSchatz/homelab --merge`. Then on the host, while the service runs: `$HF restore --config /config/config.yaml` (the runbook's step 6 listing) and `docker logs --since 5m holdfast` | a homelab clone with Docker (for `make ci`), `gh`; the host shell | `make ci` green (it could not run where the PR was prepared: every validator is a container and there was no Docker daemon); after the merge, the restore lines' `time=` offsets equal the service's for the same minutes (S0176 AC-1, the operator's criterion) | nothing in holdfast; if the offsets differ, the homelab `.env` overrides `TZ` and the runbook value must be changed to match it | OPEN |

States: `OPEN`, `DONE (<commit or PR>)`, `DROPPED (why)`. Only the owner, or a session they tell
to in its own chat, marks a row `DONE`.
