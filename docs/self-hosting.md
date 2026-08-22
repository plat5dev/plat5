# Self-hosting

Operator guide: run Plat5 (and optionally [Plat5 Auth](https://github.com/plat5dev/auth)) on one machine with published images + Compose.

This is **not** the CLI. `plat5 start` is laptop-only. On a server you pull GHCR images, fill `.env`, and apply routes yourself.

Local contracts stay in this repo (`idp-contract.md`, `routes.md`, `route-registry.md`, `gateway-contract.md`). Auth env/OIDC live in the Auth repo. This page is the cross-product operator path those READMEs already link.

## What you run

```
Internet
  https://api.example.com   → TLS edge → gateway :5001
  https://auth.example.com  → TLS edge → issuer  :5000   (or any OIDC IdP)

Docker (two stacks, two networks):
  plat5   postgres, etcd, gateway, identity, route-registry
  auth    postgres, issuer     (skip if you already have an IdP)

Your app:
  API process on the plat5 compose network (gateway `url` = `api:3000`)
  SPA anywhere (Pages, nginx, …) — talks to public api + auth URLs
```

| Piece | Image pin | Compose |
|-------|-----------|---------|
| Runtime (gateway, identity, route-registry) | `PLAT5_VERSION` | [`compose/docker-compose.prod.yml`](../compose/docker-compose.prod.yml) |
| Auth (optional IdP) | `AUTH_VERSION` (independent) | [auth `compose/docker-compose.prod.yml`](https://github.com/plat5dev/auth/blob/master/compose/docker-compose.prod.yml) |
| Your API | your image | your compose; join plat5’s network |
| TLS | — | Cloudflare tunnel, Caddy, … — **not** in the gateway |

Do **not** join the Auth Docker network from Plat5. The gateway fetches JWKS over the **public issuer URL** (same `iss` clients see).

## 1. Auth (or BYO IdP)

Plat5 requires `AUTH_ISSUER` + `AUTH_JWKS_URI` to become ready. API keys are an alternative credential, not an IdP-free mode.

### Plat5 Auth

```bash
git clone https://github.com/plat5dev/auth.git
cd auth/compose
cp .env.template .env   # set secrets + AUTH_VERSION
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Required: `POSTGRES_PASSWORD`, `SMTP_HOST`, `SMTP_USER`, `SMTP_PASS`. Prod (`DEPLOYMENT_ENV=prod`) fails closed without all three SMTP vars. Mail is **not** bundled — Resend/SES, or a host MTA.

If the issuer reaches SMTP on the Docker host:

```yaml
# extra_hosts on the issuer service
extra_hosts:
  - "host.docker.internal:host-gateway"
```

```
SMTP_HOST=host.docker.internal
SMTP_PORT=587
SMTP_TLS_INSECURE=true   # snakeoil / private CA
```

Issuer **always** sends `SMTP_USER` / `SMTP_PASS` (nodemailer). Use submission **:587 + SASL**, not an open :25 relay.

Also set:

| Variable | Role |
|----------|------|
| `PUBLIC_ISSUER_URL` | Token `iss` / links (e.g. `https://auth.example.com`) |
| `AUTH_ALLOWED_CLIENTS` | OAuth `client_id` — must match gateway `AUTH_ALLOWED_AUDIENCES` and the SPA client id |
| `AUTH_ALLOWED_REDIRECT_URIS` | Exact callback URLs (`https://app.example.com/callback`) |
| `AUTH_ALLOWED_ORIGINS` | Browser CORS for the SPA origin |
| `AUTH_DISPLAY_NAME` | Login title + verification email copy |
| `AUTH_USER_ID_CLAIM` on Plat5 | `properties.user_id` for this issuer |

Env reference: [auth `docs/env.md`](https://github.com/plat5dev/auth/blob/master/docs/env.md).

### Other IdP

Point Plat5 at it. Typical `AUTH_USER_ID_CLAIM` is `sub`. See [`idp-contract.md`](idp-contract.md).

## 2. Plat5 runtime

```bash
git clone https://github.com/plat5dev/plat5.git
cd plat5/compose
cp .env.template .env
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Required: `POSTGRES_PASSWORD`, `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN`, `AUTH_ISSUER`, `AUTH_JWKS_URI`.

Pin `PLAT5_VERSION` to a released tag (see `.env.template`).

```
AUTH_ISSUER=https://auth.example.com
AUTH_JWKS_URI=https://auth.example.com/.well-known/jwks.json
AUTH_ALLOWED_AUDIENCES=<same as AUTH_ALLOWED_CLIENTS>
AUTH_USER_ID_CLAIM=properties.user_id   # Plat5 Auth; use sub for many OIDC IdPs
# APIKEY_BRAND=plat5                    # optional; keys {brand}-sk-1- / {brand}-mk-1-
```

`POSTGRES_PASSWORD` is interpolated into `DATABASE_URL`. Use a **URL-safe** value (hex). `+` / `/` from raw base64 break the URL (`invalid port`).

Compose network name is **`plat5_plat5`** (`name: plat5` + network `plat5`). Attach apps to that name.

Route-registry **:5002 is not published**. Apply from a container on `plat5_plat5`, or an SSH tunnel. Do not put the admin API on the public internet.

Gateway `ALLOWED_ORIGINS` is unset in prod compose → CORS `*`. Set it on the gateway service when you want an allowlist.

JWKS must be a URL the **gateway container** can fetch. Public HTTPS is the usual case — no `host.docker.internal` on the gateway.

## 3. TLS

Gateway and issuer speak HTTP. Terminate TLS at the edge:

- Cloudflare tunnel: `auth.example.com` → `http://127.0.0.1:5000`, `api.example.com` → `http://127.0.0.1:5001`
- Or Caddy / nginx on the host

Do not put SMTP submission on a tunnel.

`AUTH_ISSUER` / `PUBLIC_ISSUER_URL` / SPA `VITE_AUTH_ISSUER` must be the **same** public string. `http://localhost:5000` ≠ `https://auth.example.com`.

## 4. Attach your API

1. Run the process on **`plat5_plat5`** (external network). Hostname = the service name in `routes.yml` (e.g. `api` → `url: api:3000`).
2. Trust gateway headers. Do not validate JWTs. [`gateway-contract.md`](gateway-contract.md).
3. Apply identity catalog + your routes (prod does **not** seed):

```yaml
# your-api/routes.yml — set url to what the gateway can reach
services:
  api:
    url: api:3000
    organization:
      organization_param: organization_id
      route_prefix: /api/organizations/{organization_id}
      routes:
        - path: /widgets
          methods: [GET, POST]
```

```bash
# from a throwaway client on plat5_plat5
curl -sS -X POST http://route-registry:5002/v1/apply \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary @services/identity/routes.yml

curl -sS -X POST http://route-registry:5002/v1/apply \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

Identity public paths are operator-owned. Catalog: [`services/identity/routes.yml`](../services/identity/routes.yml). Omitting a path hides it from the gateway; the identity process still runs.

Apply is **upsert**. Format: [`routes.md`](routes.md), [`route-registry.md`](route-registry.md).

Local CLI `upstreams:` (bare port → `host.docker.internal`) is for laptop Docker. On a shared compose network, put `url:` in the file.

## 5. SPA

Build-time env (Vite example):

```
VITE_GATEWAY_URL=https://api.example.com
VITE_AUTH_ISSUER=https://auth.example.com
VITE_AUTH_CLIENT_ID=<AUTH_ALLOWED_CLIENTS>
VITE_AUTH_REDIRECT_URI=https://app.example.com/callback
VITE_APIKEY_BRAND=<APIKEY_BRAND>
```

SPA fallback so `/callback` serves `index.html` (Pages `_redirects`: `/* /index.html 200`).

Match Auth allowlists to that origin + redirect. Empty gateway `ALLOWED_ORIGINS` is `*`.

Static hosting (Cloudflare Pages, object storage, nginx) is fine. The console is not part of the Plat5 runtime.

## Health

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://auth.example.com/.well-known/jwks.json
# 200

curl -sS https://api.example.com/api/organizations
# 401 UNAUTHORIZED — route exists, no token
```

Empty route map → 404. After apply, missing JWT → 401.

Gateway ready requires JWKS loaded (`/health/ready` on internal `:8000`).

## Footguns

- Template `AUTH_USER_ID_CLAIM=sub` is wrong for Plat5 Auth (`properties.user_id`).
- `plat5.yml` / `plat5 start` do not operate this stack.
- `docker compose down -v` wipes identity orgs/keys and Auth users.
- Recreating etcd without Postgres is ok (projection rebuilds). Wiping Postgres loses desired routes — re-apply.
- Brand / audience / SPA client id are one string if you customize them.

## Pins

Images: `ghcr.io/plat5dev/{gateway,identity,route-registry}:$PLAT5_VERSION` and `ghcr.io/plat5dev/auth:$AUTH_VERSION`. Tags are independent. Use released `v*` tags, not `latest`.
