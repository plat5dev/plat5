# Gateway Contract

Auth delegation, identity headers, and trace propagation from the Plat5 gateway (`services/gateway/`).

Boundary: [`identity-boundary.md`](identity-boundary.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity backends: [`identity.md`](identity.md).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT/API key), member admission on `organization` scope, API-key `required_scopes`, rate limits, identity header injection, CORS, trace propagation |
| **Edge / Load Balancer** | TLS termination (e.g. Cloudflare Zero Trust tunnels) |
| **Downstream services** | Business logic, data access, resource authorization |

Services behind the gateway **must not** re-validate JWTs or parse `Authorization`. The gateway handles authentication entirely.

## Injected Headers

The gateway strips client-supplied identity headers, then injects only what the **route scope** allows.

| Scope | Credential | Identity headers injected |
|-------|------------|---------------------------|
| `public` | none | none |
| `user` | user JWT or **user** API key | `X-User-Id` only |
| `organization` | user JWT, **user** API key + member resolve, or **member** API key | **`X-Organization-Id`**, **`X-Member-Id`** only — **not** `X-User-Id` |

### Stripped before upstream (all scopes)

After authentication (or immediately on `public`), the gateway **removes** client credential headers so upstreams never see them:

| Header | Why |
|--------|-----|
| `Authorization` | Consumed for JWT authn; must not leak bearer tokens to apps |
| `X-API-Key` | Consumed for API-key authn; must not leak raw keys to apps |

Clients still send these headers **to the gateway**. Services behind the gateway will not receive them. CORS may still allow browsers to send them.

### Always (all scopes)

| Header | Description |
|--------|-------------|
| `X-Request-ID` | Correlation ID (gateway-generated; also on response) |
| `traceparent` | W3C trace context (OTel propagation) |

Org-scope identity is `X-Organization-Id` + `X-Member-Id` only. Member **role** is identity-service domain data — not a gateway header and not a lookup for org-scope apps.

## Route Configuration

Routes live in etcd under `edge/gateway/routes/`. Scopes are `public` / `user` / `organization` blocks — not a flat `auth:` flag. Full schema and publish path: [`routes.md`](routes.md).

```yaml
services:
  my-service:
    url: my-service:3000
    public:
      routes:
        - path: /public/health
          methods: [GET]
    user:
      routes:
        - path: /api/widgets
          methods: [GET, POST]
          required_scopes: [widgets:read]
        - path: /api/features
          methods:
            GET:
              required_scopes: [org:read]
            POST:
              required_scopes: [org:write]
```

`methods` may be a list (route-level `required_scopes` / `rate_limit` apply to every verb) or a nested map (per-verb). Nested maps are expanded at apply into one etcd row per verb; the gateway always sees `methods` as a string array. Full schema: [`routes.md`](routes.md).

Services publish via the **route-registry** admin API (`POST /v1/apply`). Gateway loads at startup and watches etcd. Route existence is decoupled from service health — a downed service returns 503, not 404.

### API key `required_scopes`

After match + admission: if the route has `required_scopes` **and** the API key has a non-null scopes list, the lists must have a nonempty intersection or **403** `FORBIDDEN` (existing envelope). JWTs and unrestricted keys (`scopes: null`) skip. This is a credential constraint, not resource ACL / FGA. There is no `allowed_services`.

### Rate limits

In-process per gateway instance. No Redis.

| | |
|--|--|
| Fallback | `RATE_LIMIT_REQUESTS` (default 60; `0` = unlimited), `RATE_LIMIT_WINDOW_SECONDS` (default 60) |
| Per-route | omitted inherits fallback (never silent unlimited); `{requests, window_seconds}` overrides; `false` opts out |
| Who | All admitted routes (JWT and API key) |
| Subject | Derived from route scope: `public`→ip, `user`→user, `organization`→org (org id, including SA/member keys) |
| Exceed | **429** `RATE_LIMITED`, type `api_error`, message `Too many requests. Try again in a moment.`, `details.retry_after_seconds`, `Retry-After` |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). Unadmitted 401s and unmatched 404s. Not per-route. |

## Rules for Services Behind the Gateway

Direct-exposed services are exempt (see below).

1. **Trust identity headers for your scope** — Do not validate tokens.
   - `user`: trust `X-User-Id`
   - `organization`: trust `X-Organization-Id` and `X-Member-Id` only
2. **Missing expected headers → platform bug** — Return `INTERNAL_ERROR` (500), not `UNAUTHORIZED`
3. **Propagate `traceparent`** on downstream calls
4. **Log with `request_id`** from `X-Request-ID`
5. **Do not set `X-Request-ID` on responses** — gateway owns it
6. **Do not trust client-supplied identity headers** — gateway strips spoofed values before inject

### Platform integrity (hard limits)

These protect the shared auth model for the entire platform.

1. **Do not parse `Authorization`** — gateway consumes it and strips it before upstream; you will not receive it
2. **Do not validate JWTs** — gateway validates via JWKS
3. **Do not read `X-API-Key`** — gateway consumes and strips it before upstream
4. **Do not implement CORS** — gateway handles preflight and headers
5. **Do not generate `X-Request-ID`** — gateway generates it; propagate if present. Missing → gateway bug

## Organization-scoped routes

Admission error policy (canonical): [`identity-boundary.md`](identity-boundary.md).

### User credential (JWT or user API key)

1. Bad/missing credential → **401** `UNAUTHORIZED`
2. Member resolve unavailable → **503** `SERVICE_UNAVAILABLE`
3. No member or status ≠ `active` → **404** `NOT_FOUND`
4. Active hit → inject only `X-Organization-Id` + `X-Member-Id`
5. Then `required_scopes` (restricted keys only) → **403** `FORBIDDEN` on miss
6. Then per-route rate limit → **429** `RATE_LIMITED`

### Member API key (`{brand}-mk-1-…`)

1. Bad/missing / invalid key → **401** `UNAUTHORIZED`
2. Member-key validate unavailable → **503** `SERVICE_UNAVAILABLE`
3. `organization_id` ≠ path org → **404** `NOT_FOUND` (existence policy)
4. Active member key for path org → inject only `X-Organization-Id` + `X-Member-Id`
5. Then `required_scopes` / rate limit as above

Gateway chooses validate URL by **wire prefix** before calling identity. Prefixes come from `APIKEY_BRAND` (same env as identity; unset → `plat5`): `{brand}-sk-1-` / `{brand}-mk-1-`. Contract: [`identity.md`](identity.md).

| Prefix | Endpoint env | Scope |
|--------|--------------|--------|
| `{brand}-sk-1-` | `USER_APIKEY_VALIDATE_URL` | user (+ org via member resolve) |
| `{brand}-mk-1-` | `MEMBER_APIKEY_VALIDATE_URL` | organization only |

Member keys are **not** valid on `user` scope routes → **401** `UNAUTHORIZED`. User keys are not sent to the member-key validate URL.

Missing org headers on an org-scoped service request → **500** `INTERNAL_ERROR`.

The **identity** service stays on **`user` scope** and enforces membership in-process. See [`identity.md`](identity.md).

## Internal identity control plane

Gateway calls identity backends over HTTP (not published on the edge route map):

| Call | Env | Typical URL |
|------|-----|-------------|
| User API key validate | `USER_APIKEY_VALIDATE_URL` | `http://identity:3001/internal/user-keys/validate` |
| Member API key validate | `MEMBER_APIKEY_VALIDATE_URL` | `http://identity:3001/internal/member-keys/validate` |
| Member resolve | `MEMBER_RESOLVE_URL` | `http://identity:3001/internal/members/resolve` |

Both live on identity’s **`INTERNAL_PORT`**. Optional shared `INTERNAL_AUTH_TOKEN` is sent as `X-Plat5-Internal-Token`. Contract: [`identity.md`](identity.md).

## Public Routes

`public` scope receives no identity headers. Do not expect `X-User-Id` or attempt auth checks.

## Direct-Exposed Services

Some services are intentionally exposed outside the gateway (e.g. an **IdP / issuer** at `auth.company.com` while the gateway serves `api.company.com`):

- Handle their own CORS (browser OAuth flows require it)
- Do not rely on gateway identity headers
- Exempt from “Do not implement CORS”

TLS still typically terminates at the edge.

## TLS Termination

TLS terminates at the **edge / load balancer**, not the gateway process. Edge decrypts and forwards plaintext HTTP to the gateway (e.g. `localhost:5001`). Certificate management stays at the edge provider. Services behind the gateway receive plaintext HTTP and must not terminate TLS.

## CORS

Gateway handles `OPTIONS` preflight and adds `Access-Control-Allow-*` on responses. Services behind the gateway have no CORS config. Direct-exposed services handle CORS as needed.

## Errors

| Case | Code |
|------|------|
| Auth failure (bad/missing credential) | `UNAUTHORIZED` (401) — gateway only |
| Restricted API key missing route `required_scopes` | `FORBIDDEN` (403) |
| Route not registered | `NOT_FOUND` (404) |
| Request body too large | `PAYLOAD_TOO_LARGE` (413) |
| Rate limit (admitted route or failed-auth IP) | `RATE_LIMITED` (429); `Retry-After` |
| Upstream or auth infra down (JWKS, key validate, member resolve) | `SERVICE_UNAVAILABLE` (503); proxy upstream failure may surface as **502** with the same `SERVICE_UNAVAILABLE` code |
| Gateway internal failure mid-proxy | `INTERNAL_ERROR` (500) |
| Missing expected identity headers in a service | `INTERNAL_ERROR` (500) |

All of the above use the Plat5 JSON envelope (`api-errors.md`), including failures handled in `fail_to_proxy`. Client disconnect (no response needed) does not write a body.
