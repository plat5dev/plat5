# Control plane admin API

Concept: [config](../../concepts/config.md). Environment: [`reference/env.md`](../env.md#control-plane).

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5402` |
| Internal (feeds, health, metrics) | container `:5003` |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes accept `Authorization: Bearer <token>`, except `GET /schema` and `GET /auth/info`, which are public.

Configure at least one authentication mechanism at boot (both may coexist for break-glass, with a startup warning):

- **OIDC JWTs (`AUTH_CONFIG_FILE`, recommended for production):** A mounted read-only YAML/JSON file validated against `schema/control_plane_auth.json` (`plat5 validate --auth-config <file>`). Declares one or more trusted OIDC issuers (e.g., GitHub Actions OIDC for keyless CI/CD + Okta/Entra/Google for human operators), discovers `jwks_uri` via OIDC Discovery (`{issuer}/.well-known/openid-configuration`, with optional explicit `jwks_uri`), enforces RFC 8725 asymmetric JWT verification (`RS*`, `PS*`, `ES256`, `ES384`, `EdDSA`; explicit `audiences`, never `*`; `exp` & `nbf`; optional `max_token_lifetime`), and evaluates claim rules (full-string anchored `*` globs, bare `*` forbidden) to grant **`read`** or **`apply`** access:
  - **`read`**: `GET /config`, `GET /revisions`, `GET /revisions/{rev}`, `GET /status`, `GET /auth/whoami`, and `POST /apply?dry_run=true`.
  - **`apply`**: all `read` routes plus mutating `POST /apply`.
- **Static bearer token (`ADMIN_TOKEN`, default for local dev):** Compared in constant time; grants `apply` access. Local default: `dev-admin-token`. Omit `ADMIN_TOKEN` in hardened production deployments that use `AUTH_CONFIG_FILE`.

`/internal/*` on `INTERNAL_PORT` takes no token: internal ports are reachable only inside the deployment's network, never exposed outside it ([internal APIs](internal.md)).

## API

| Method | Path | Access | Notes |
|--------|------|--------|-------|
| `POST` | `/apply?dry_run=true` | `read` | The plan. Writes nothing |
| `POST` | `/apply` | `apply` | Apply, with `expect`. JSON or YAML. Optional `X-Plat5-Change-Reason` (≤ 512 bytes) and `X-Plat5-Change-Ref` (≤ 256 bytes) headers |
| `GET` | `/config` | `read` | The head revision's source, `actor`, optional `metadata`, and `revision`. **404** before the first apply |
| `GET` | `/revisions` | `read` | History, newest first: `revision`, `created_at`, `request_id`, `summary`, verified `actor`, and optional `metadata` |
| `GET` | `/revisions/{rev}` | `read` | That revision's source, verified `actor`, and optional `metadata` ([revisions](../../concepts/config.md#revisions)) |
| `GET` | `/status` | `read` | <a id="status"></a>Where each reader replica stands ([below](#reader-status)) |
| `GET` | `/auth/whoami` | `read` | Verified caller `actor` (`type`, `issuer`, `issuer_name`, `subject`) and effective `access` (`read` or `apply`) |
| `GET` | `/auth/info` | public | Non-secret auth discovery (`methods` and configured issuers' public `cli` OAuth metadata for `plat5 login`; never exposes rules) |
| `GET` | `/schema` | public | The JSON Schema for the config, `control_plane_auth.json`, and each document |

## Responses

Validation, auth and empty-body failures use the Plat5 envelope ([`reference/errors.md`](../errors.md)) and write nothing.

| Case | Status |
|------|--------|
| Dry run | **200**, the plan, with `services` ([below](#dry-run)) |
| Applied | **200** `{ "revision": 42, "summary": {…} }`. Readers see it on their next poll |
| Missing/invalid token, unknown issuer/kid, expired token, or exceeded `max_token_lifetime` | **401** `UNAUTHORIZED` |
| Valid JWT with unmatched claims or `read`-only access calling mutating `POST /apply` | **403** `FORBIDDEN` (`details.required_access`) |
| Malformed body or invalid `X-Plat5-Change-Reason` / `X-Plat5-Change-Ref` header | **400** `INVALID_REQUEST` |
| Schema or rule broken, unknown key, two routes of one shape, blocked change | **422** `VALIDATION_ERROR`, naming each field path, such as `services.api.scopes.project.routes[2].path` |
| The precondition failed | **409** `PLAN_STALE` (`{ precondition, expected, actual }`) |
| `expect` missing | **422** |
| Identity unreachable while computing a plan that removes a kind or changes a parent, or OIDC issuer JWKS unavailable | **503** `SERVICE_UNAVAILABLE` |
| Postgres failure | **503**. Nothing written |

## Dry run

A dry run answers the plan ([config](../../concepts/config.md#the-plan)) plus `services`: every service in the planned config, expanded by the running control plane exactly as the gateway feed would serve them after the apply ([feeds](internal.md#control-plane-feeds)), each with its `url`, `timeouts`, `max_body_bytes` and `routes`. It's the config as it would be after the apply, not the change. `plat5 routes list` prints its routes.

```json
{ "revision": 41, "summary": { … }, "changes": [ … ],
  "services": { "e10s": { "url": "http://e10s:8080", "timeouts": { … }, "max_body_bytes": 1048576, "routes": [ … ] } } }
```

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
