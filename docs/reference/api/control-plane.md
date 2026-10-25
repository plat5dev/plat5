# Control plane admin API

Concept: [config](../../concepts/config.md). Environment: [`reference/env.md`](../env.md#control-plane).

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5402` |
| Internal (feeds, health, metrics) | container `:5003` |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes require `Authorization: Bearer <ADMIN_TOKEN>`, compared in constant time, except `GET /schema`, which is public. Local default: `dev-admin-token`. Required and non-empty in all environments.

`/internal/*` on `INTERNAL_PORT` takes no token: internal ports are reachable only inside the deployment's network, never exposed outside it ([internal APIs](internal.md)).

`ADMIN_TOKEN` can route traffic, change what every role grants, and remove a kind. Treat it as the deployment's root credential. The revisions below are its history.

## API

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/apply?dry_run=true` | The plan. Writes nothing |
| `POST` | `/apply` | Apply, with `expect`. JSON or YAML |
| `GET` | `/config` | The current config, as applied, and its `revision`. **404** before the first apply |
| `GET` | `/revisions` | History, newest first: `revision`, `created_at`, `request_id`, and the plan's `summary` |
| `GET` | `/revisions/{rev}` | The config at that revision |
| `GET` | `/status` | <a id="status"></a>Where each reader replica stands ([below](#reader-status)) |
| `GET` | `/schema` | The JSON Schema for the config and each document. No token |

## Responses

Validation, auth and empty-body failures use the Plat5 envelope ([`reference/errors.md`](../errors.md)) and write nothing.

| Case | Status |
|------|--------|
| Dry run | **200**, the plan |
| Applied | **200** `{ "revision": 42, "summary": {…} }`. Readers see it on their next poll |
| Schema or rule broken, unknown key, two routes of one shape, blocked change | **422** `VALIDATION_ERROR`, naming each field path, such as `services.e10s.scopes.project.routes[2].path`. The CLI adds the file |
| The precondition failed | **409** `PLAN_STALE` |
| `expect` missing | **422** |
| Identity unreachable while computing a plan that removes a kind or changes a parent | **503**: that plan needs its counts |
| Postgres failure | **503**. Nothing written |

## Reader status

```
GET /status
→ 200
{
  "revision": 42,
  "gateway": [
    { "replica": "gw-1", "loaded": 42, "refused": null, "seen_at": "…" },
    { "replica": "gw-2", "loaded": 41, "refused": { "revision": 42, "reason": "route /orgs/{organization}/files: …" }, "seen_at": "…" }
  ]
}
```

`revision` is the head. Each gateway replica that long-polled in the last 60 seconds is listed, by its replica id ([`reference/api/internal.md`](internal.md#control-plane-feeds)), with the revision it has loaded and the last one it refused, if any, with the reason. Sorted by replica id. A replica that stops polling drops off the list after 60 seconds.
