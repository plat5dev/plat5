# Environment

Every service's environment variables. Config that changes while the deployment runs is in the control plane instead ([config](../concepts/config.md#what-it-holds)).

## Gateway

| Variable | |
|----------|--|
| `CONTROL_PLANE_URL` | Required |
| `ADMIT_URL` | Required. e.g. `http://identity:3001/internal/admit` |
| `INTERNAL_AUTH_TOKEN` | Sent to identity, audit and the control plane |
| `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`, `AUTH_SUBJECT_CLAIM` | [`reference/idp.md`](idp.md) |
| `APIKEY_CACHE_TTL_SECS` | Default 300 |
| `VALKEY_URL` | Required |
| `RATE_LIMIT_*` | [Rate limits](gateway.md#rate-limits) |
| `AUDIT_ENABLED`, `AUDIT_URL` | [`concepts/audit.md`](../concepts/audit.md) |
| `TRUSTED_PROXIES` | [Client IP](gateway.md#client-ip) |
| `UPSTREAM_CONNECT_TIMEOUT_MS`, `UPSTREAM_READ_TIMEOUT_MS` | Defaults for services without `timeouts` |

## Control plane

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Feeds, health, metrics |
| `DATABASE_URL` | required | Postgres; schema `control_plane` |
| `ADMIN_TOKEN` | required | Bearer token |
| `INTERNAL_AUTH_TOKEN` | empty | Required on `/internal/*` when set; sent to identity |
| `IDENTITY_INTERNAL_URL` | required | Identity's internal base URL, for plan counts, e.g. `http://identity:3001` |
| `SEED_DIR` | empty | A directory holding a `plat5.yml` and the routes files it lists, applied on first boot, when the head is `0`. Errors name the file. Never read again |

Ready is **503** unless Postgres answers.

## Identity

| | |
|--|--|
| Directory | `services/identity/` |
| `service.name` / `service.namespace` | `identity` / `identity` |
| Public port | `3000` (the service API) |
| Internal port | `3001` (`/health/*`, `/metrics`, admit, plan counts, lifecycle events) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`identity`** |
| `APIKEY_BRAND` | Default `plat5` ([credentials](api/identity.md#credentials)) |
| `CONTROL_PLANE_URL` | Required. Identity doesn't listen until it has `kinds` and `roles` ([`reference/api/internal.md`](api/internal.md#control-plane-feeds)) |

Ready fails closed (**503**) when Postgres is unreachable or the documents aren't loaded.

## Audit

| Variable | |
|----------|--|
| `DATABASE_URL` | Plat5 Postgres, schema `audit` |
| `INTERNAL_AUTH_TOKEN` | Required on `/internal/*` when set |

On the gateway, `AUDIT_ENABLED` (default `true`) and `AUDIT_URL` (required unless audit is off) turn audit on and point at it ([audit](../concepts/audit.md#the-model)).
