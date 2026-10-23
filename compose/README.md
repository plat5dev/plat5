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
| `http://localhost:5402` | Control plane admin API (the config: kinds, roles, routes, CORS origins) |

Admin token default: `dev-admin-token` (`ADMIN_TOKEN`).
Internal token default: `dev-internal-token` (`INTERNAL_AUTH_TOKEN`) — gateway → identity admit, audit, and the control plane; identity → the control plane.

## Change the config

The control plane applies [`seed/`](seed/) on first boot (see Config below). After that, change it with the CLI, which shows the plan first:

```bash
plat5 config pull my-config && cd my-config   # what's applied, as files
plat5 apply                                    # plat5.yml and the routes files it lists
```

Audit: the gateway records audited requests on kind and sign-in routes through the `audit` service (`AUDIT_URL`). It is on by default. `AUDIT_ENABLED=false` turns it off for the deployment; then audit need not run. See [`../docs/concepts/audit.md`](../docs/concepts/audit.md).

## JWT / IdP

Set env (or defaults) for any IdP reachable from the gateway container:

| Variable | Local default | Notes |
|----------|---------------|--------|
| `AUTH_ISSUER` | `http://localhost:5401` | Token `iss` as clients see it |
| `AUTH_JWKS_URI` | `http://host.docker.internal:5000/.well-known/jwks.json` | Fetch JWKS via host port |
| `AUTH_SUBJECT_CLAIM` | `properties.user_id` | Use `sub` for many OIDC providers |
| `AUTH_ALLOWED_AUDIENCES` | `plat5` | Plat5 API audience; match your IdP client/`aud` |

Gateway uses `host.docker.internal` so a host-published IdP does not need a shared Docker network. API keys are an alternative to JWT; the IdP is still required.

Config: kinds, roles, routes and CORS origins live in the control plane, applied as one config ([`../docs/concepts/config.md`](../docs/concepts/config.md)). On first boot it applies a seed (`SEED_DIR`): dev compose mounts [`seed/`](seed/) (`plat5.yml` and the generated identity and audit routes), prod compose mounts [`seed-prod/`](seed-prod/) (kinds and roles, no services). After that, editing the seed does nothing: change the config with `plat5 plan` and `plat5 apply`. CORS answers `*` until `plat5.yml` lists origins.

`APIKEY_BRAND` (default `plat5`) is identity's alone: keys are `{brand}-k-2-…`. The gateway checks a key's shape and sends it, or a JWT's issuer and subject, to identity's admit (`ADMIT_URL`) with the route's node chain; admit answers the member and labels for that request. Answers are cached for `APIKEY_CACHE_TTL_SECS` (default 300). See [`../docs/reference/gateway.md`](../docs/reference/gateway.md#admit).

Gateway rate limits (Valkey; replicas share one budget): `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS` (default 60/60; `0` requests = unlimited fallback), counted at the route's own node. Named policies on the service count at their `count_at` kind; a route can carry several. Failed-auth IP limiter: `RATE_LIMIT_AUTH_FAILURE_*` (default 60/60). `VALKEY_URL` is required. See [`../docs/reference/config/routes.md`](../docs/reference/config/routes.md#rate_limit) and [`../docs/reference/gateway.md`](../docs/reference/gateway.md).

See [`../docs/reference/idp.md`](../docs/reference/idp.md).

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

Required: `POSTGRES_PASSWORD`, `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN`, `AUTH_ISSUER`, `AUTH_JWKS_URI`.

`POSTGRES_PASSWORD` is interpolated into `DATABASE_URL` — use a URL-safe value (hex). The control plane's admin port is **not** published in prod by default.

Operator walkthrough (Auth, TLS, attach your API): [`../docs/guides/deploy.md`](../docs/guides/deploy.md).
