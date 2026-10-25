# Self-hosting

Operator guide: run Plat5 (and optionally [Plat5 Auth](https://github.com/plat5dev/auth)) on one machine with published images + Compose.

This is **not** the CLI. `plat5 start` is laptop-only. On a server you pull GHCR images, fill `.env`, and apply your config yourself.

Local contracts stay in this repo (see the [docs map](../README.md)). Auth env/OIDC live in the Auth repo. This page is the cross-product operator path those READMEs already link.

## What you run

```
Internet
  https://api.example.com   → TLS edge → gateway :5001
  https://auth.example.com  → TLS edge → issuer  :5000   (or any OIDC IdP)

Docker (two stacks, two networks):
  plat5   postgres, valkey, gateway, identity, audit, control-plane
  auth    postgres, issuer     (skip if you already have an IdP)

Your app:
  API process on the plat5 compose network (gateway `url` = `http://api:3000`)
  SPA anywhere (Pages, nginx, …) — talks to public api + auth URLs
```

| Piece | Image pin | Compose |
|-------|-----------|---------|
| Runtime (gateway, identity, audit, control-plane) | `PLAT5_VERSION` | [`compose/docker-compose.prod.yml`](../../compose/docker-compose.prod.yml) |
| Auth (optional IdP) | `AUTH_VERSION` (independent) | [auth `compose/docker-compose.prod.yml`](https://github.com/plat5dev/auth/blob/master/compose/docker-compose.prod.yml) |
| Your API | your image | your compose; join plat5’s network |
| TLS | — | Cloudflare tunnel, Caddy, … — **not** in the gateway |

Do **not** join the Auth Docker network from Plat5. The gateway fetches JWKS over the **public issuer URL** (same `iss` clients see).

## 1. Auth (or BYO IdP)

Plat5 requires `AUTH_ISSUER` + `AUTH_JWKS_URI` to become ready. API keys are an alternative credential.

### Plat5 Auth

```bash
git clone https://github.com/plat5dev/auth.git
cd auth/compose
cp .env.template .env   # set secrets + AUTH_VERSION
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Required: `POSTGRES_PASSWORD`, `SMTP_HOST`, `SMTP_USER`, `SMTP_PASS`. Without all three SMTP vars the issuer still starts, but sending a login code fails (in every environment). Never set `AUTH_DEV_MODE` here: it logs login codes and enables `POST /dev/token`. Mail is **not** bundled — Resend/SES, or a host MTA.

If the issuer reaches SMTP on the Docker host, Auth's prod compose already maps `host.docker.internal` on the issuer (`extra_hosts: host.docker.internal:host-gateway`):

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
| `AUTH_SUBJECT_CLAIM` on Plat5 | `properties.user_id` for this issuer |

Env reference: [auth `docs/env.md`](https://github.com/plat5dev/auth/blob/master/docs/env.md).

### Other IdP

Point Plat5 at it. Typical `AUTH_SUBJECT_CLAIM` is `sub`. See [`reference/idp.md`](../reference/idp.md).

## 2. Plat5 runtime

```bash
git clone https://github.com/plat5dev/plat5.git
cd plat5/compose
cp .env.template .env
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Required: `POSTGRES_PASSWORD`, `ADMIN_TOKEN`, `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`.

The services' internal ports take no token: compose keeps them on the `plat5` network and publishes none of them. Never expose one outside the deployment.

Pin `PLAT5_VERSION` to a released tag (see `.env.template`).

```
AUTH_ISSUER=https://auth.example.com
AUTH_JWKS_URI=https://auth.example.com/.well-known/jwks.json
AUTH_ALLOWED_AUDIENCES=<same as AUTH_ALLOWED_CLIENTS>   # or * to accept any
AUTH_SUBJECT_CLAIM=properties.user_id   # Plat5 Auth; the default, sub, suits most OIDC IdPs
# APIKEY_BRAND=plat5                    # optional; identity only; keys {brand}-k-2-…
```

The tree's kinds, the roles and the routes live in the control plane, in `plat5.yml`, which you keep in a repo with the service files it lists ([`concepts/config.md`](../concepts/config.md#the-config)). A fresh stack has no config: identity and the gateway wait until you apply one with `plat5 apply`, which shows the plan first. Later changes go the same way, and identity and the gateway pick them up without a restart. Start from `plat5 init`'s files ([CLI](../reference/cli.md#the-local-stack)). Formats and rules: [`reference/config/kinds.md`](../reference/config/kinds.md), [`concepts/access.md`](../concepts/access.md).

Audit is on by default: the gateway records each audited request in its tree's audit log, through the `audit` service. While audit is down, audited requests are **503**. `AUDIT_ENABLED=false` on the gateway says the deployment runs no audit service; then stop the `audit` service and leave `services/audit.yml` out of `plat5.yml`. Model and guarantees: [`concepts/audit.md`](../concepts/audit.md).

`POSTGRES_PASSWORD` is interpolated into `DATABASE_URL`. Use a **URL-safe** value (hex). `+` / `/` from raw base64 break the URL (`invalid port`).

Compose network name is **`plat5_plat5`** (`name: plat5` + network `plat5`). Attach apps to that name.

The control plane's **:5002 is not published**. Apply from a container on `plat5_plat5`, or an SSH tunnel. Do not put the admin API on the public internet: `ADMIN_TOKEN` can route traffic and change what every role grants.

CORS allows no cross-origin requests until the config lists origins. Add them to `plat5.yml` and apply:

```yaml
cors:
  allowed_origins: ["https://app.example.com"]
```

Roles change the same way: edit `roles` in `plat5.yml`, then `plat5 apply`. Every apply is a revision; rolling back is applying an older one ([`reference/cli.md`](../reference/cli.md#cli)).

JWKS must be a URL the **gateway container** can fetch. Public HTTPS is the usual case — no `host.docker.internal` on the gateway.

## 3. TLS

Gateway and issuer speak HTTP. Terminate TLS at the edge:

- Cloudflare tunnel: `auth.example.com` → `http://127.0.0.1:5000`, `api.example.com` → `http://127.0.0.1:5001`
- Or Caddy / nginx on the host

Do not put SMTP submission on a tunnel.

`AUTH_ISSUER` / `PUBLIC_ISSUER_URL` / SPA `VITE_AUTH_ISSUER` must be the **same** public string. `http://localhost:5401` ≠ `https://auth.example.com`.

## 4. Attach your API

1. Run the process on **`plat5_plat5`** (external network). Hostname = the host in its service file's `url` (e.g. `url: http://api:3000`).
2. Trust the path and `X-Plat5-*` headers the gateway wrote. Do not validate JWTs. [`reference/gateway.md`](../reference/gateway.md).
3. Write a service file for it, list it in `plat5.yml`'s `services` beside `services/identity.yml` and `services/audit.yml`, and apply:

```yaml
# services/api.yml, listed in plat5.yml's services; set url to what the gateway can reach
api:
  url: http://api:3000
  scopes:
    project:
      upstream_prefix: /projects/{project.id}
      routes:
        - path: /widgets
          methods:
            GET:  { required_permissions: [project:read] }
            POST: { required_permissions: [widgets:write] }
```

```bash
plat5 routes list        # every route the gateway will serve, identity's and audit's included
plat5 apply              # plat5.yml and every service file it lists; shows the plan, asks to confirm
```

Identity's and audit's routes come from their resources, for every kind, with defaults you override in their service files ([resources](../reference/config/resources.md)). Removing one hides it from the gateway; the identity process still serves it on the network. Grant your routes' permissions in `roles`, or only `*` holders can call them.

Your writes are in their tree's audit log by default. Set `audit: false` on a write nobody needs there, and `audit: true` on a read worth recording ([`reference/config/services.md`](../reference/config/services.md#audit)).

An apply is the whole config: a service missing from `services` is removed, and the plan shows it. Format: [`reference/config/services.md`](../reference/config/services.md), [`concepts/config.md`](../concepts/config.md).

Local CLI `upstreams:` (bare port → `host.docker.internal`) is for laptop Docker. On a shared compose network, put `url:` in the file.

## 5. SPA

Build-time env (Vite example):

```
VITE_GATEWAY_URL=https://api.example.com
VITE_AUTH_ISSUER=https://auth.example.com
VITE_AUTH_CLIENT_ID=<AUTH_ALLOWED_CLIENTS>
VITE_AUTH_REDIRECT_URI=https://app.example.com/callback
```

The SPA does not need the brand. A credential's kind comes back in the API response that issued it.

SPA fallback so `/callback` serves `index.html` (Pages `_redirects`: `/* /index.html 200`).

Match Auth allowlists to that origin + redirect, and add the origin to `cors.allowed_origins` in `plat5.yml`.

Static hosting (Cloudflare Pages, object storage, nginx) is fine. The console is not part of the Plat5 runtime.

## Health

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://auth.example.com/.well-known/jwks.json
# 200

curl -sS https://api.example.com/user
# 401 UNAUTHORIZED: route exists, no credential
```

Empty route map → 404. After apply, missing JWT → 401.

The gateway starts serving once it has loaded routes from the control plane. Gateway ready requires JWKS loaded and Valkey reachable (`/health/ready` on internal `:8000`). While Valkey is unreachable, limited requests are 503; they resume when it answers again. Valkey restart resets open rate-limit windows.

## Footguns

- Choose `AUTH_SUBJECT_CLAIM` before the first sign-in. Identity links each person by issuer and that claim's value, so changing the claim later signs everyone in as a new person. With Plat5 Auth use `properties.user_id` (the template default); use `sub` for generic OIDC IdPs.
- `plat5 start` and its `plat5.dev.yml` don't operate this stack. `plat5.yml` does, through `plat5 apply`.
- `docker compose down -v` on this stack wipes identity (nodes, people, keys), audit, and the applied config. Plat5 Auth runs as its own stack; its users go only with its own volumes.
- Wiping Postgres loses the applied config (kinds, roles, routes, CORS) along with identity and audit. Re-apply the config from your repo.
- Audience and SPA client id are one string if you customize them. The brand is identity's alone.

## Pins

Images: `ghcr.io/plat5dev/{gateway,identity,control-plane,audit}:$PLAT5_VERSION` and `ghcr.io/plat5dev/auth:$AUTH_VERSION`. Tags are independent. Use released `v*` tags, not `latest`.

Upgrade the gateway with or before the control plane. The gateway [refuses a whole snapshot](../reference/gateway.md#control-plane) it can't build, so a control plane that writes something an older gateway doesn't know leaves that gateway on its last snapshot until it's upgraded.
