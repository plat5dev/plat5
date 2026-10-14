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
| `http://localhost:5001` | Gateway |
| `http://localhost:5002` | Control plane admin API (routes, roles, CORS origins) |

Admin token default: `dev-admin-token` (`ADMIN_TOKEN`).
Internal token default: `dev-internal-token` (`INTERNAL_AUTH_TOKEN`) — gateway → identity validate, audit, and the control plane; identity → the control plane.

## Apply routes

```bash
curl -sS -X POST http://localhost:5002/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @../services/identity/routes.yml
```

Dev compose seeds identity and audit routes only when that service has no history yet (does not overwrite). Prod does not seed — apply `services/identity/routes.yml` (or a subset) and `services/audit/routes.yml` yourself.

Audit: the gateway records audited `organization` and `member` requests through the `audit` service (`AUDIT_URL`). It is on by default. `AUDIT_ENABLED=false` turns it off for the deployment; then audit need not run and its catalog is not applied. See [`../docs/audit.md`](../docs/audit.md).

## JWT / IdP

Set env (or defaults) for any IdP reachable from the gateway container:

| Variable | Local default | Notes |
|----------|---------------|--------|
| `AUTH_ISSUER` | `http://localhost:5000` | Token `iss` as clients see it |
| `AUTH_JWKS_URI` | `http://host.docker.internal:5000/.well-known/jwks.json` | Fetch JWKS via host port |
| `AUTH_USER_ID_CLAIM` | `properties.user_id` | Use `sub` for many OIDC providers |
| `AUTH_ALLOWED_AUDIENCES` | `plat5` | Plat5 API audience; match your IdP client/`aud` |

Gateway uses `host.docker.internal` so a host-published IdP does not need a shared Docker network. API keys are an alternative to JWT; the IdP is still required.

Roles: identity reads the roles document from the control plane and picks up changes without a restart. Dev and prod compose both seed [`roles.yml`](roles.yml) as the first document (`SEED_ROLES_FILE`); after that, change roles with `PUT /roles` on the admin API, since editing the file does nothing. Set `ROLES_ENABLED=false` to turn roles off: every member holds every label, and authorization is left to something else. See [`../docs/roles.md`](../docs/roles.md).

CORS: the gateway answers with `*` until you set origins with `PUT /cors` ([`../docs/control-plane.md`](../docs/control-plane.md#cors)).

`APIKEY_BRAND` (default `plat5`) is identity's alone: keys are `{brand}-k-2-…`, sessions `{brand}-s-2-…`. The gateway checks a key's shape and sends it to identity's one validate (`CREDENTIAL_VALIDATE_URL`), which says whose it is: a user key passes on `user` routes, a member key or session on `organization` and `member` routes. See [`../docs/identity.md`](../docs/identity.md#credential-format).

Gateway rate limits (Valkey; replicas share one budget): `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS` (default 60/60; `0` requests = unlimited fallback). Named policies on the service; `shared: true` is opt-in cross-service. Subject follows route scope (`public`→ip, `user`→user, `organization`→org, `member`→member). Failed-auth IP limiter: `RATE_LIMIT_AUTH_FAILURE_*` (default 60/60). `VALKEY_URL` is required. See [`../docs/routes.md`](../docs/routes.md) and [`../docs/gateway-contract.md`](../docs/gateway-contract.md).

See [`../docs/idp-contract.md`](../docs/idp-contract.md).

## Telemetry

Optional `OTEL_EXPORTER_OTLP_ENDPOINT` (empty = no OTLP). When set, traces and metrics both default to OTLP; `/metrics` scrape stays on. CLI: `otel.endpoint` in `plat5.yml`. See [`../docs/telemetry.md`](../docs/telemetry.md).

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

Operator walkthrough (Auth, TLS, attach your API): [`../docs/self-hosting.md`](../docs/self-hosting.md).
