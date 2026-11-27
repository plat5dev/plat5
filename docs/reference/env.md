# Environment

Every Plat5 service's environment variables: wiring, secrets, trust anchors, module switches and boot settings. Desired state that changes while running lives in the control plane ([config](../concepts/config.md#what-it-holds)). Telemetry env vars follow standard OpenTelemetry conventions ([`reference/telemetry.md`](telemetry.md)).

Internal ports (`INTERNAL_PORT`) take no token. Keep them inside the deployment's network, never exposed outside it.

## Gateway

| Variable | |
|----------|--|
| `PORT` / `INTERNAL_PORT` | Public proxy port (default `5001`) and internal `/health/*` + `/metrics` port (default `8000`) |
| `CONTROL_PLANE_URL` | Required. Control plane's internal base URL, e.g. `http://control-plane:5003` |
| `IDENTITY_INTERNAL_URL` | Required. Identity's internal base URL, e.g. `http://identity:3001` |
| `AUTH_ISSUER`, `AUTH_JWKS_URI` | Required. [`reference/idp.md`](idp.md) |
| `AUTH_ALLOWED_AUDIENCES` | Required. Comma-separated; `*` accepts any audience |
| `AUTH_SUBJECT_CLAIM` | Dotted claim path for the person's stable IdP subject. Default `sub` |
| `AUTH_NAME_CLAIMS` | Comma-separated dotted claim paths for seeding a person's display name on first sign-in. Default `name,preferred_username` |
| `AUTH_SCOPE_CLAIM` | Optional dotted claim path for JWT permission scopes (space-delimited string or JSON string array). When set and present in a token, effective permissions on kind routes are intersected with the claim |
| `ADMIT_CACHE_TTL` | Duration. Default `5m` ([cache](gateway.md#cache)) |
| `ADMIT_NEGATIVE_CACHE_TTL` | Duration. Default `10s` |
| `RATE_LIMIT_ENABLED` | Default `true`. Set `false` to run without Valkey or rate limiting ([rate limits](gateway.md#rate-limits)) |
| `VALKEY_URL` | Required unless `RATE_LIMIT_ENABLED=false`. Supports `redis://` and TLS `rediss://` |
| `VALKEY_KEY_PREFIX` | Prefix for Valkey rate-limit keys. Default `plat5:rl:` (non-empty visible ASCII, no whitespace) |
| `RATE_LIMIT_AUTH_FAILURE_REQUESTS`, `RATE_LIMIT_AUTH_FAILURE_WINDOW` | Failed-auth IP limiter (active when `RATE_LIMIT_ENABLED=true`). Default `60` and `60s`; `0` requests = off |
| `AUDIT_MODE` | `sync` (default), `async`, or `off`. In `sync`, audited writes wait on an intent write before forwarding (**503** on failure). In `async`, writes forward immediately and queue one completed event in the background. `off` disables audit writes and ignores `AUDIT_URL` ([audit](../concepts/audit.md#on-and-off)) |
| `AUDIT_URL` | Audit's internal base URL, e.g. `http://audit:3003`. Required unless `AUDIT_MODE=off` |
| `TRUSTED_PROXIES` | Comma-separated IPs/CIDRs trusted for `X-Forwarded-For` and `X-Forwarded-Proto` ([client IP](gateway.md#client-ip)) |
| `CORS_ALLOWED_ORIGINS` | Optional comma-separated allowed browser origins (`http://host[:port]` or `https://host[:port]`, lowercase, no path, up to 100), or `*` alone. Unset or empty allows no cross-origin requests ([CORS](gateway.md#cors)) |
| `CORS_ALLOWED_HEADERS` | Optional comma-separated lowercase request header names (up to 32) added to the gateway's base preflight allowlist |
| `UPSTREAM_SIGNING_KEY` | Optional 32-byte Ed25519 seed encoded as base64url without padding. When set, every upstream request carries `X-Plat5-Signature` ([signing](headers.md#upstream-request-signing)); empty-but-set or invalid refuses boot |
| `UPSTREAM_SIGNING_KEY_ID` | Optional `kid` (`1–64` of `[A-Za-z0-9._-]`) for `X-Plat5-Signature`. Defaults to the first 16 hex chars of `SHA-256(public_key)`. Refuses boot if set without `UPSTREAM_SIGNING_KEY` |

The replica id reported to the control plane is `service.instance.id` from `OTEL_RESOURCE_ATTRIBUTES`, falling back to `HOSTNAME`.

## Control plane

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Feeds, health, metrics |
| `DATABASE_URL` | required | Postgres; schema `control_plane` |
| `AUTH_CONFIG_FILE` | unset | Path to a read-only YAML/JSON multi-issuer OIDC JWT auth config (`schema/control_plane_auth.json`). Recommended for production; at least one of `AUTH_CONFIG_FILE` or `ADMIN_TOKEN` is required |
| `ADMIN_TOKEN` | unset | Static bearer token granting `apply` access on the Admin API (required only when `AUTH_CONFIG_FILE` is unset; omit in hardened production) |
| `IDENTITY_INTERNAL_URL` | required | Identity's internal base URL, for plan counts |

Ready is **503** unless Postgres answers and every issuer configured in `AUTH_CONFIG_FILE` has loaded its JWKS at least once.

## Identity

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `3000` | Service API |
| `INTERNAL_PORT` | `3001` | `/health/*`, `/metrics`, admit, admit invalidations, plan counts, lifecycle log, access evaluation and resource search |
| `DATABASE_URL` | required | Postgres; schema `identity` |
| `CONTROL_PLANE_URL` | required | Identity doesn't listen until it has its first config |
| `SECRET_BRAND` | `plat5` | Prefix on minted API keys (`{brand}-k-2-…`) and invite tokens (`{brand}-i-1-…`); empty-but-set refuses boot |

Ready fails closed (**503**) when Postgres is unreachable or no config is loaded.

## Audit

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `3002` | Read API |
| `INTERNAL_PORT` | `3003` | `/health/*`, `/metrics`, internal event writes |
| `DATABASE_URL` | required | Postgres; schema `audit` |
