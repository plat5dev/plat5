# Self-hosting

Run the Plat5 runtime on a server using published images and Docker Compose. (For local laptop development, use the [`plat5` CLI](https://github.com/plat5dev/cli).)

## Architecture

```
Internet
  https://api.example.com  → TLS edge (Caddy, nginx, Cloudflare Tunnel, …) → gateway :5001

Docker network (plat5_plat5):
  postgres, control-plane, gateway, identity
  + audit (optional, on by default)
  + valkey (optional, on by default for rate limits)

Your services:
  Plain HTTP servers on the plat5_plat5 network (e.g. http://api:3000)

IdP:
  Any OAuth 2.0 issuer of signed JWT access tokens (Auth0, Okta, Keycloak, Plat5 Auth, …)
```

TLS terminates at your edge/load balancer, never in the gateway. Internal ports (`INTERNAL_PORT`) take no token and must stay inside the private network.

## 1. Configure and start Plat5

```bash
git clone https://github.com/plat5dev/plat5.git
cd plat5/compose
cp .env.template .env
# Fill required values + pin PLAT5_VERSION to a released tag
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Required in `.env`:
- `PLAT5_VERSION` — released `v*` tag (do not use `latest`)
- `POSTGRES_PASSWORD` — URL-safe (hex recommended; raw base64 `+` / `/` breaks `DATABASE_URL`)
- `AUDIT_OWNER_PASSWORD`, `AUDIT_WRITER_PASSWORD` — passwords for the separate `audit_owner` (`audit migrate` / `audit prune`) and non-deleting `audit_writer` (`audit serve`) Postgres roles ([audit storage](../internals/audit-storage.md#roles))
- `ADMIN_TOKEN` — bearer token for the control plane Admin API
- `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES` — your IdP ([`reference/idp.md`](../reference/idp.md))

Optional tuning:
- `AUTH_SUBJECT_CLAIM` — defaults to `sub` (set `properties.user_id` if using [Plat5 Auth](https://github.com/plat5dev/auth))
- `AUTH_NAME_CLAIMS` — defaults to `name,preferred_username`
- `AUTH_SCOPE_CLAIM` — optional JWT claim path for narrowing token permissions
- `SECRET_BRAND` — prefix for API keys and invite tokens (default `plat5`)
- `TRUSTED_PROXIES` — CIDR/IP list of your edge proxy so `X-Forwarded-For` and `X-Forwarded-Proto` are honored
- `CORS_ALLOWED_ORIGINS` / `CORS_ALLOWED_HEADERS` — browser origins (or `*`) and extra request headers allowed by the gateway ([CORS](../reference/gateway.md#cors))
- `UPSTREAM_SIGNING_KEY` / `UPSTREAM_SIGNING_KEY_ID` — optional Ed25519 signing of forwarded upstream requests ([signing](../reference/headers.md#upstream-request-signing))
- `OTEL_EXPORTER_OTLP_ENDPOINT` — optional OTLP collector ([`reference/telemetry.md`](../reference/telemetry.md))

If your deployment forwards to untrusted external URLs and requires SSRF / private-IP egress protection, enforce it at the network layer (VPC security groups, Kubernetes `NetworkPolicy`, or an egress proxy) rather than in the gateway ([upstream security](../reference/gateway.md#upstream-security)).

### Transport encryption and supply-chain verification

For hardened or compliance-scoped deployments (see the [SOC 2, ISO 27001 & GDPR playbook](../playbooks/compliance-soc2-gdpr.md)):

- **Postgres TLS:** Append `?sslmode=require` (or `verify-full` with your CA mounted into the container's system trust store) to `DATABASE_URL` on `control-plane`, `identity`, and `audit`.
- **Valkey TLS:** Use a `rediss://` URL for `VALKEY_URL` on `gateway` (verified against the container's system CA bundle via `rustls`).
- **Upstream & internal traffic:** Point service `url` entries at `https://…` (or run inside a private VPC/mesh) and set `UPSTREAM_SIGNING_KEY` so services can cryptographically verify `X-Plat5-Signature` ([signing](../reference/headers.md#upstream-request-signing)).
- **Image verification:** Published release images on `ghcr.io/plat5dev/*` are multi-arch, run as non-root, include BuildKit SBOM and SLSA provenance attestations, and are signed with keyless Cosign (Sigstore):

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/plat5dev/plat5/\.github/workflows/release\.yml@refs/tags/v' \
  ghcr.io/plat5dev/gateway:${PLAT5_VERSION}

docker buildx imagetools inspect --format '{{json .SBOM}}' ghcr.io/plat5dev/gateway:${PLAT5_VERSION}
```

### Optional modules

Both **audit** and **rate limiting** default to on (`AUDIT_URL` and `VALKEY_URL` are required unless turned off):

- **Audit mode (`AUDIT_MODE`):** defaults to `sync` (fail-closed intent before forward). Set `AUDIT_MODE=async` for lower write latency via background queuing, or `AUDIT_MODE=off` to disable audit writes (then don't run the `audit` container and omit `audit` from `services`).
- **Without rate limiting:** set `RATE_LIMIT_ENABLED=false` on the gateway and don't run `valkey`. Rate-limit policies in your config are still accepted at apply and ignored at runtime. When enabled, rate limiting fails open if Valkey is unreachable and resumes automatically when Valkey recovers.
- **With an external AuthZEN PDP:** put the PDP on `plat5_plat5` (or point top-level `authorizer.url` at an HTTPS endpoint) and set any `token_env` environment variable on both `gateway` and `identity` ([authorizer](../reference/config/authorizer.md)).

Full env reference: [`reference/env.md`](../reference/env.md).

## 2. Apply your config

A fresh deployment has no config: `identity` and `gateway` wait for the first apply to the control plane (`POST /apply`, [`reference/api/control-plane.md`](../reference/api/control-plane.md)).

The control plane's port `:5002` is not published by default — reach it from inside `plat5_plat5` or over an SSH tunnel. Never expose `ADMIN_TOKEN` to the public internet.

With the [`plat5` CLI](https://github.com/plat5dev/cli):

```bash
plat5 plan
plat5 apply
```

Or call `POST /apply?dry_run=true` and `POST /apply` directly with JSON or YAML ([`concepts/config.md`](../concepts/config.md)).

- **Rollback:** fetch an older revision (`GET /revisions/{rev}` or `plat5 config pull --revision <n>`) and apply it through the same plan.
- **Drift checks in CI:** run `plat5 plan --detailed-exitcode` against your repo's config (exit `2` means the deployment differs).
- **Adding/removing kinds:** adding a kind takes effect immediately; removing a kind or changing its parent is blocked while any non-purged nodes of that kind exist.

## 3. Attach your services

1. Put your service container on the `plat5_plat5` external network (so the gateway can reach its `url`, e.g. `http://api:3000`).
2. Follow the rules in [write a service](write-a-service.md).
3. Add the service under `services` in your config and apply.

To allow browser clients, set `CORS_ALLOWED_ORIGINS` (and optionally `CORS_ALLOWED_HEADERS`) on the gateway container ([CORS](../reference/gateway.md#cors)).

## Health & verification

- `GET /health/ready` on each service's `INTERNAL_PORT` ([`reference/health.md`](../reference/health.md)).
- Before the first apply, `gateway` and `identity` wait for the initial config snapshot before listening on their public ports (`/health/ready` returns **503**). After apply, an unauthenticated request to a protected route returns **401** `UNAUTHORIZED`.
- `JWKS` must be reachable from the gateway container over HTTPS/HTTP.

## Footguns

- **Pick `AUTH_SUBJECT_CLAIM` before the first sign-in.** Changing it later signs existing people in as new users.
- **`docker compose down -v`** wipes Postgres — identity data, audit logs, and applied config revisions.

## Pins

Images: `ghcr.io/plat5dev/{gateway,identity,control-plane,audit}:$PLAT5_VERSION`.

1. Upgrade `gateway` with or before `control-plane` (an older gateway refuses a snapshot format it doesn't understand and stays on its last snapshot until upgraded).
2. Upgrade `control-plane`, `identity`, and `audit`. Stored feed bodies stay unchanged until your next apply.
3. Run a plan on your unchanged config to review any expansion differences, then apply if needed.
