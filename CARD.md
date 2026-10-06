# holdfast card

A data-safe, config-as-code media transcoder in Go: it re-encodes bloated video smaller and swaps
a source only after the replacement passed every gate. Public repository, AGPL.

## Offers

- The image `ghcr.io/nschatz/holdfast:<version>` (multi-arch, distroless), and the example
  deployment `docker-compose.yml` pinned by digest; `docs/docker.md` is the deployment guide.
- The CLI (`cmd/holdfast`): `serve`, `run`, `worker`, `analyze [--health]`, `plan`, `validate`,
  `export`, `resolve`, `restore`, `requeue`, `version` (`docs/layout.md`).
- The HTTP API: job ledger, queue, history, SSE, health sweep, nodes, scans, pause, exclusions
  (`docs/api-reference.md`); its generated schema at `GET /api/schema`.
- Prometheus metrics at `/metrics`, and shoutrrr notifications (`internal/metrics`, `internal/notify`).
- Intake from Sonarr and Radarr webhooks and `POST /api/scan`; post-swap rescans of Radarr,
  Sonarr and Plex, and a Plex play hold (`docs/post-swap-hook.md`).
- Remote worker nodes over a lease protocol (`docs/design/nodes.md`).
- A redacted hardware report and client report a GPU or media host can run
  (`scripts/hw-report.sh`, `scripts/client-report.sh`; `docs/hardware-reports.md`).

## Hand it work

`~/.local/bin/goals task add holdfast "<title>" --project <id> --body-file <spec>`

A spec for this repo must name:

- **Why**: the operator need, one or two lines.
- **Behaviour**: what changes, as config keys, CLI flags or API fields with their defaults.
- **Safety**: how the swap invariant and fail-safe skips hold; which gate covers it.
- **Proof**: the fixture or test that reds on the regression it guards.
- **Docs**: which `docs/` file and design anchor change.

```
# <title>
Why: <need>
Behaviour: <keys/flags/fields and defaults>
Safety: <how no source is mutated before every gate passes>
Proof: <test that reds without the change>
Docs: <docs/... and docs/design/...#anchor>
```

## Interfaces

- Config: YAML, then `HOLDFAST_*` env (koanf, `internal/config`); `config.example.yaml` is the
  reference, `holdfast validate` checks one. Credentials by `file:` or `cmd:` reference only (`docs/secrets.md`).
- HTTP API and job-row fields: `docs/api-reference.md`, `docs/api-schema.json` (the baseline a
  PR's API diff is checked against).
- Webhook shapes it accepts: Sonarr and Radarr Download (`docs/design/media-clients.md#webhook-intake`).
- Metric names: `internal/metrics`, each documented (checked by `internal/docscheck`).
- Tool pins (Go, ffmpeg, dovi_tool, hdr10plus_tool, Node, pnpm): the `Makefile`,
  `scripts/install-*.sh` and `web/`, held by `scripts/check-pins.sh`.
- Release tags `v*` and the compose pin: `docs/release.md`.

## Not here

- The live deployment, its compose file, mounts, GPU passthrough and routes: `homelab`.
- The dev container and its GPU guard: `claude-containers`. The task harness: `goals`.
- Python libraries (CAD, rendering, electronics and the like): `shopkit`.
