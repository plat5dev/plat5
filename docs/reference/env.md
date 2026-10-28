# Environment

Every service's environment variables: wiring, secrets, trust anchors and boot settings. Config that changes while the deployment runs is in the control plane instead ([config](../concepts/config.md#what-it-holds)).

Internal ports (`INTERNAL_PORT`) take no token. Keep them inside the deployment's network, never exposed outside it.

## Gateway

| Variable | |
|----------|--|
| `CONTROL_PLANE_URL` | Required. The control plane's internal base URL, e.g. `http://control-plane:5003` |
| `IDENTITY_INTERNAL_URL` | Required. Identity's internal base URL, e.g. `http://identity:3001`; admit is at `/internal/admit` |
| `AUTH_ISSUER`, `AUTH_JWKS_URI` | Required. [`reference/idp.md`](idp.md) |
| `AUTH_ALLOWED_AUDIENCES` | Required. Comma-separated; `*` accepts any audience |
| `AUTH_SUBJECT_CLAIM` | Default `sub` |
| `ADMIT_CACHE_TTL` | A duration. Default `5m` ([cache](gateway.md#cache)) |
| `ADMIT_NEGATIVE_CACHE_TTL` | A duration. Default `10s` |
| `VALKEY_URL` | Required |
| `RATE_LIMIT_AUTH_FAILURE_REQUESTS`, `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` | The failed-auth IP limiter. Default 60/60; `0` = off ([rate limits](gateway.md#rate-limits)) |
| `AUDIT_ENABLED` | Wiring: whether the deployment runs an audit service. Default `true` ([`concepts/audit.md`](../concepts/audit.md)) |
| `AUDIT_URL` | Audit's internal base URL, e.g. `http://audit:3003`. Required unless audit is off |
| `TRUSTED_PROXIES` | [Client IP](gateway.md#client-ip) |
| `OTEL_SERVICE_INSTANCE_ID` | The replica id it reports to the control plane; default the hostname ([telemetry](telemetry.md)) |

## Control plane

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Feeds, health, metrics |
| `DATABASE_URL` | required | Postgres; schema `control_plane` |
| `ADMIN_TOKEN` | required | Bearer token |
| `IDENTITY_INTERNAL_URL` | required | Identity's internal base URL, for plan counts, e.g. `http://identity:3001` |

Ready is **503** unless Postgres answers. A fresh control plane has no config until the first `plat5 apply`.

## Identity

| | |
|--|--|
| Directory | `services/identity/` |
| `service.name` / `service.namespace` | `identity` / `identity` |
| Public port | `3000` (the service API) |
| Internal port | `3001` (`/health/*`, `/metrics`, admit, plan counts, lifecycle events, access evaluation) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`identity`** |
| `APIKEY_BRAND` | Default `plat5` ([credentials](api/identity.md#credentials)) |
| `CONTROL_PLANE_URL` | Required. Identity doesn't listen until it has its first config ([`reference/api/internal.md`](api/internal.md#control-plane-feeds)) |

Ready fails closed (**503**) when Postgres is unreachable or no config is loaded.

## Audit

| Variable | |
|----------|--|
| `DATABASE_URL` | Plat5 Postgres, schema `audit` |
