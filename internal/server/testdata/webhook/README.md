# Webhook intake fixtures

Synthetic payloads for `internal/server/webhook_test.go`. Every title, path, identifier and
release name here is made up; none comes from a real library or a real service.

Each file is written from the payload class the arr builds for that event, with the property
names the arr's serialiser produces (camelCase, null properties omitted), read 2026-10-02 from
the source at the release tags below, under `src/NzbDrone.Core/Notifications/Webhook/`.

| file | upstream class and builder |
|---|---|
| `sonarr-download-episodefile.json` | Sonarr `v4.0.20.3014`, `WebhookImportPayload.cs`, `WebhookBase.BuildOnDownloadPayload` |
| `sonarr-download-upgrade.json` | the same class with `isUpgrade` true and `deletedFiles` |
| `sonarr-download-importcomplete.json` | Sonarr `WebhookImportCompletePayload.cs`, `WebhookBase.BuildOnImportCompletePayload` |
| `sonarr-rename.json` | Sonarr `WebhookRenamePayload.cs`, `WebhookRenamedEpisodeFile.cs` |
| `sonarr-test.json` | Sonarr `WebhookBase.BuildTestPayload` (its values are the ones the source hard-codes) |
| `sonarr-grab.json` | Sonarr `WebhookGrabPayload.cs` |
| `radarr-download-moviefile.json` | Radarr `v6.4.4.10685`, `WebhookImportPayload.cs`, `WebhookBase.BuildOnDownloadPayload` |
| `radarr-download-upgrade.json` | the same class with `isUpgrade` true and `deletedFiles` |
| `radarr-rename.json` | Radarr `WebhookRenamePayload.cs`, `WebhookRenamedMovieFile.cs` |
| `radarr-test.json` | Radarr `WebhookBase.BuildTestPayload` (its values are the ones the source hard-codes) |

They were written by hand from those classes and were not captured from a running Sonarr or
Radarr: no goal of this program contacts a live service.
