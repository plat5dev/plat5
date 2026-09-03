# Plat5 compose

Self-contained Plat5 runtime: gateway, route-registry, identity, postgres, etcd, redis. Own Docker network.

## Quick start

Consumer apps: [plat5dev/cli](https://github.com/plat5dev/cli) (`plat5 init` / `plat5 start`).

This tree:

```bash
cd compose
docker compose up --build
```

| URL | Service |
|-----|---------|
| `http://localhost:5001` | Gateway |
| `http://localhost:5002` | Route registry admin API |

Admin token default: `dev-admin-token` (`ADMIN_TOKEN`).
Internal control-plane token default: `dev-internal-token` (`INTERNAL_AUTH_TOKEN`) — gateway ↔ identity validate / resolve.

## Apply routes

```bash
curl -sS -X POST http://localhost:5002/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @../services/identity/routes.yml
```

Dev compose seeds identity routes only when that service has no history yet (does not overwrite). Prod does not seed — apply `services/identity/routes.yml` (or a subset) yourself.

## JWT / IdP

Set env (or defaults) for any IdP reachable from the gateway container:

| Variable | Local default | Notes |
|----------|---------------|--------|
| `AUTH_ISSUER` | `http://localhost:5000` | Token `iss` as clients see it |
| `AUTH_JWKS_URI` | `http://host.docker.internal:5000/.well-known/jwks.json` | Fetch JWKS via host port |
| `AUTH_USER_ID_CLAIM` | `properties.user_id` | Use `sub` for many OIDC providers |
| `AUTH_ALLOWED_AUDIENCES` | `plat5` | Plat5 API audience; match your IdP client/`aud` |

Gateway uses `host.docker.internal` so a host-published IdP does not need a shared Docker network. API keys are an alternative to JWT; the IdP is still required.

`APIKEY_BRAND` (default `plat5`) is the same value on gateway and identity. Wire prefixes are `{brand}-sk-1-` / `{brand}-mk-1-`. See [`../docs/identity.md`](../docs/identity.md).

Gateway rate limits (Redis; replicas share one budget): `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS` (default 60/60; `0` requests = unlimited fallback). Named policies on the service; `shared: true` is opt-in cross-service. Subject follows route scope (`public`→ip, `user`→user, `organization`→org). Failed-auth IP limiter: `RATE_LIMIT_AUTH_FAILURE_*` (default 60/60). `REDIS_URL` is required. See [`../docs/routes.md`](../docs/routes.md) and [`../docs/gateway-contract.md`](../docs/gateway-contract.md).

See [`../docs/idp-contract.md`](../docs/idp-contract.md).

## Telemetry

Optional `OTEL_EXPORTER_OTLP_ENDPOINT` (empty = no OTLP). When set, traces and metrics both default to OTLP; `/metrics` scrape stays on. CLI: `otel.endpoint` in `plat5.yml`. See [`../docs/telemetry.md`](../docs/telemetry.md).

## Prod

Image mode (default) — pull `ghcr.io/plat5dev/*:${PLAT5_VERSION}`:

```bash
cp .env.template .env   # set secrets + PLAT5_VERSION=v0.2.1
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Local source build instead of pull:

```bash
docker compose -f docker-compose.prod.yml -f docker-compose.prod.build.yml --env-file .env up --build -d
```

Required: `POSTGRES_PASSWORD`, `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN`, `AUTH_ISSUER`, `AUTH_JWKS_URI`.

`POSTGRES_PASSWORD` is interpolated into `DATABASE_URL` — use a URL-safe value (hex). Route registry admin port is **not** published in prod by default.

Operator walkthrough (Auth, TLS, attach your API): [`../docs/self-hosting.md`](../docs/self-hosting.md).
