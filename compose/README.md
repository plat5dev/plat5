# Plat5 compose

Self-contained Plat5 runtime: gateway, route-registry, api-keys, organizations, postgres, etcd. Own Docker network.

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
Internal control-plane token default: `dev-internal-token` (`INTERNAL_AUTH_TOKEN`) — gateway ↔ api-keys validate / organizations resolve.

## Apply routes

```bash
curl -sS -X POST http://localhost:5002/v1/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @../services/api-keys/routes.yml
```

Identity routes (`api-keys`, `organizations`) are seeded by route-registry on startup.

## JWT / IdP

Set env (or defaults) for any IdP reachable from the gateway container:

| Variable | Local default | Notes |
|----------|---------------|--------|
| `AUTH_ISSUER` | `http://localhost:5000` | Token `iss` as clients see it |
| `AUTH_JWKS_URI` | `http://host.docker.internal:5000/.well-known/jwks.json` | Fetch JWKS via host port |
| `AUTH_USER_ID_CLAIM` | `properties.user_id` | Use `sub` for many OIDC providers |
| `AUTH_ALLOWED_AUDIENCES` | `plat5` | Plat5 API audience; match your IdP client/`aud` |

Gateway uses `host.docker.internal` so a host-published IdP does not need a shared Docker network. API keys work with no IdP.

See [`../docs/idp-contract.md`](../docs/idp-contract.md).

## Telemetry

Optional `OTEL_EXPORTER_OTLP_ENDPOINT` (empty = no OTLP). When set, traces and metrics both default to OTLP; `/metrics` scrape stays on. CLI: `otel.endpoint` in `plat5.yml`. See [`../docs/telemetry.md`](../docs/telemetry.md).

## Prod

Image mode (default) — pull `ghcr.io/plat5dev/*:${PLAT5_VERSION}`:

```bash
cp .env.template .env   # set secrets + PLAT5_VERSION=v0.1.2
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Local source build instead of pull:

```bash
docker compose -f docker-compose.prod.yml -f docker-compose.prod.build.yml --env-file .env up --build -d
```

Required: `POSTGRES_PASSWORD`, `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN`, `AUTH_ISSUER`, `AUTH_JWKS_URI`.

Identity seed routes live under `seed/` (image-mode friendly; keep in sync with service `routes.yml`).

Route registry admin port is **not** published in prod by default.
