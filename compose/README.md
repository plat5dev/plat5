# Plat5 compose

Self-contained Plat5 runtime: gateway, control-plane, identity, audit, postgres, valkey. Own Docker network.

## Quick start

Consumer apps: [plat5dev/cli](https://github.com/plat5dev/cli) (`plat5 init` / `plat5 start`).

This tree:

```bash
cd compose
docker compose up --build
```

| URL | Service |
|-----|---------|
| `http://localhost:5400` | Gateway |
| `http://localhost:5402` | Control plane admin API (the config: settings, kinds, roles, rate limits, CORS, services) |

Admin token default: `dev-admin-token` (`ADMIN_TOKEN`).

Internal ports take no token: compose keeps them on the `plat5` network and publishes none of them ([`../docs/reference/env.md`](../docs/reference/env.md)).

## Apply a config

A fresh stack has no config. Identity and the gateway wait for the first apply, and neither listens until then. Apply with the CLI, which shows the plan first:

```bash
plat5 apply                                    # plat5.yml and the service files it lists
plat5 config pull my-config                    # later: what's applied, as files
```

Every change after that goes the same way, and identity and the gateway pick it up without a restart. See [`../docs/concepts/config.md`](../docs/concepts/config.md#plan-and-apply). The e2e applies its own config ([`../e2e/README.md`](../e2e/README.md)).

Audit: the gateway records audited requests through the `audit` service (`AUDIT_URL`). It is on by default. `AUDIT_ENABLED=false` turns it off for the deployment; then audit need not run, and `services/audit.yml` stays out of the config. See [`../docs/concepts/audit.md`](../docs/concepts/audit.md).

## JWT / IdP

Set env (or defaults) for any IdP reachable from the gateway container:

| Variable | Local default | Notes |
|----------|---------------|--------|
| `AUTH_ISSUER` | `http://localhost:5401` | Token `iss` as clients see it |
| `AUTH_JWKS_URI` | `http://host.docker.internal:5401/.well-known/jwks.json` | Fetch JWKS via host port |
| `AUTH_SUBJECT_CLAIM` | `properties.user_id` | Plat5 Auth's claim. The gateway's own default is `sub`, which suits most OIDC providers |
| `AUTH_ALLOWED_AUDIENCES` | `plat5` | Required. Match your IdP client/`aud`; `*` accepts any |

Gateway uses `host.docker.internal` so a host-published IdP does not need a shared Docker network. API keys are an alternative to JWT, also sent as `Authorization: Bearer`; the IdP is still required. See [`../docs/reference/idp.md`](../docs/reference/idp.md).

`APIKEY_BRAND` (default `plat5`) is identity's alone: keys are `{brand}-k-2-…`. The gateway sends a key, or a JWT's issuer and subject, to identity's admit (`IDENTITY_INTERNAL_URL`) with the route's node chain; admit answers the member and permissions for that request. Answers are cached for `ADMIT_CACHE_TTL` (default `5m`), refusals for `ADMIT_NEGATIVE_CACHE_TTL` (default `10s`). See [`../docs/reference/gateway.md`](../docs/reference/gateway.md#admit).

Rate limits are policies in `plat5.yml` ([`../docs/reference/config/rate-limits.md`](../docs/reference/config/rate-limits.md)), counted in Valkey, so replicas share one budget. `VALKEY_URL` is required. The failed-auth IP limiter is env: `RATE_LIMIT_AUTH_FAILURE_*` (default 60/60).

## Telemetry

Optional `OTEL_EXPORTER_OTLP_ENDPOINT` (empty = no OTLP). When set, traces and metrics both default to OTLP; `/metrics` scrape stays on. CLI: `otel.endpoint` in `plat5.dev.yml`. See [`../docs/reference/telemetry.md`](../docs/reference/telemetry.md).

## Prod

Image mode (default) — pull published images:

```bash
cp .env.template .env   # set secrets
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Local source build instead of pull:

```bash
docker compose -f docker-compose.prod.yml -f docker-compose.prod.build.yml --env-file .env up --build -d
```

Required: `POSTGRES_PASSWORD`, `ADMIN_TOKEN`, `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`. Then apply your config with `plat5 apply`.

`POSTGRES_PASSWORD` is interpolated into `DATABASE_URL` — use a URL-safe value (hex). The control plane's admin port is **not** published in prod by default.

Operator walkthrough (Auth, TLS, attach your API): [`../docs/guides/deploy.md`](../docs/guides/deploy.md).
