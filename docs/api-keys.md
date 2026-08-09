# API keys service

Plat5 **api-keys** service: user API key lifecycle and gateway validation.

Boundary: [`identity-boundary.md`](identity-boundary.md). Errors: [`api-errors.md`](api-errors.md).

## Scope and headers

| | |
|--|--|
| Gateway scope (public CRUD) | **`user` only** |
| Expect | `X-User-Id` |
| Missing `X-User-Id` | **500** `INTERNAL_ERROR` (gateway bug) |

Opaque `user_id` strings only — no IdP FK, no org coupling.

## Public API

Prefix: `/api/keys` (published on gateway `user` scope).

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/api/keys` | Create; plaintext key returned **once** (`plat5-sk-1-…`) |
| `GET` | `/api/keys` | List (no hashes); `limit` / `offset` / `has_more` |
| `DELETE` | `/api/keys/{id}` | Soft-revoke; idempotent |

Stored as SHA-256 hex. List may include revoked keys (`revoked_at` set).

## Internal validate

Not published on the gateway (`routes.yml` omits it). Served only on **`INTERNAL_PORT`**.

```
POST /internal/keys/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # required when token is set

{ "key": "plat5-sk-1-…" }
```

| Result | Response |
|--------|----------|
| Valid active key | **200** `{ "valid": true, "user_id": "…" }` |
| Missing / revoked / unknown | **200** `{ "valid": false }` |

Gateway maps: `valid: false` → client **401**; transport / non-2xx / `valid: true` without `user_id` → **503**.

### Trust model

| Layer | Behavior |
|-------|----------|
| Path | `/internal/keys/validate` only (not under `/api/…`) |
| Bind | `INTERNAL_PORT` (default `3001`); not host-published in compose |
| Token | Optional `INTERNAL_AUTH_TOKEN`. When set, require header `X-Plat5-Internal-Token` (constant-time compare). When unset, any caller on the private network may validate (dev). |
| Gateway | `APIKEY_VALIDATE_URL` + same `INTERNAL_AUTH_TOKEN` |

Compose default (dev): `dev-internal-token`. Prod: set a strong shared secret on gateway + api-keys.

Gateway may cache successful validates (`APIKEY_CACHE_TTL_SECS`, default 300s). Revoke is not edge-instant until TTL expires.

## Runtime

| | |
|--|--|
| Directory | `api-keys` |
| `service.name` | `api-keys` |
| `service.namespace` | `identity` |
| Public port | `3000` (CRUD only) |
| Internal port | `3001` (`/health/*`, `/metrics`, validate) |
| Database | Plat5 Postgres via `DATABASE_URL` |
| Schema | **`api_keys`** (service-owned) |

Ready probe fails closed (**503**) when Postgres is unreachable.
