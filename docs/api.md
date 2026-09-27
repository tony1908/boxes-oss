# Activity, usage, and pricing API

`GET /api/v1/sandboxes/{id}/activity` returns newest first. `category` is optional, `before` is the opaque `nextCursor` from the preceding page, and `limit` defaults to 50 with a maximum of 100.

```json
{
  "items": [
    {
      "id": "act_…",
      "time": "2026-09-25T12:00:00Z",
      "actorType": "user",
      "actorLabel": "owner@example.com",
      "category": "lifecycle",
      "action": "started/ready",
      "detail": {"node": "nd_…"},
      "outcome": "ok"
    }
  ],
  "nextCursor": ""
}
```

`durationMs` and `outcome` are omitted when they do not apply. `nextCursor` is an empty string on the last page. Activity detail stores short command and prompt previews plus non-sensitive metadata; it omits environment values, tokens, credentials, file contents, and full prompts. Rows older than 90 days are pruned daily. Activity rows remain available after the sandbox is deleted.

`GET /api/v1/sandboxes/{id}/usage` returns `runningSeconds`, `runningSecondsThisMonth`, `perDay` (30 UTC dates), `commandsCount`, `agentRunsCount`, `desktopSessionsCount`, `snapshotsCount`, and `since`. It includes `snapshotsBytes` when a snapshot exists, using the most recent restic snapshot size. When an hourly price is configured it also returns `estimatedCost`:

```json
{
  "runningSeconds": 1800,
  "runningSecondsThisMonth": 1800,
  "perDay": [{"date": "2026-09-25", "runningSeconds": 1800}],
  "commandsCount": 2,
  "agentRunsCount": 1,
  "desktopSessionsCount": 1,
  "snapshotsCount": 3,
  "snapshotsBytes": 123456,
  "since": null,
  "estimatedCost": {"currency": "USD", "amount": 0.25, "hourlyRate": 0.5}
}
```

Owners can read or update pricing at `GET` and `PUT /api/v1/settings/pricing`:

```json
{"currency":"USD","hourlyBySize":{"xsmall":0.1,"small":0.2,"default":0.5,"large":1,"xlarge":2}}
```

An empty `hourlyBySize` disables cost estimates.
