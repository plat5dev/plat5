# Gateway Contract

Auth delegation, subject fill, and trace propagation from the Plat5 gateway (`services/gateway/`).

Boundary: [`identity-boundary.md`](identity-boundary.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity backends: [`identity.md`](identity.md).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT, API key, member session), scope projection, API-key `required_scopes`, rate limits, subject fill into `upstream`, CORS, security headers, trace propagation |
| **Edge / Load Balancer** | TLS termination (e.g. Cloudflare Zero Trust tunnels) |
| **Downstream services** | Business logic, data access, resource authorization |

Services behind the gateway **must not** re-validate JWTs or parse `Authorization`. The gateway handles authentication entirely.

## Subject

The route scope names the subject. `upstream` fills it. The client path does not. A route that needs the subject sets `upstream`. Omitted means proxy `path` unchanged: the upstream has no subject id.

| Scope | Credential | Subject fields |
|-------|------------|----------------|
| `public` | none | none |
| `user` | user JWT or **user** API key | `user_id` |
| `organization` | **member** API key or **member** session | `organization_id` |
| `member` | same credential as `organization` | `organization_id`, `member_id` |

### Stripped before upstream (all scopes)

The gateway removes these request headers before the upstream call. It does not set them.

| Header | Why |
|--------|-----|
| `Authorization` | Consumed for JWT authn; must not leak bearer tokens to apps |
| `X-API-Key` | Consumed for API-key authn; must not leak raw keys to apps |
| `X-User-Id`, `X-Organization-Id`, `X-Member-Id` | Not a subject channel. A client must not supply one |

Clients still send credential headers **to the gateway**. Services behind the gateway will not receive them. CORS may still allow browsers to send them.

### Always (all scopes)

| Header | Description |
|--------|-------------|
| `X-Request-ID` | Correlation ID (gateway-generated; also on response) |
| `traceparent` | W3C trace context (OTel propagation) |

The scope chooses what the route is allowed to see. It is not a second proof. A user JWT and a user API key are the same proof. `organization` and `member` share one credential. That credential always carries `member_id` and `organization_id`. The scope drops fields before `upstream` substitution. Spans may still record the dropped ids. The app contract does not.

## Route Configuration

Routes live in etcd under `edge/gateway/routes/`. Scopes are `public` / `user` / `organization` / `member` blocks. Full schema and publish path: [`routes.md`](routes.md).

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

Services publish via the **route-registry** admin API (`POST /apply`). Gateway loads at startup and watches etcd. Route existence is decoupled from service health — a downed service returns 503, not 404.

### API key `required_scopes`

After match + admission: if the route has `required_scopes` **and** the API key has a non-null scopes list, the lists must have a nonempty intersection or **403** `FORBIDDEN`. JWTs, unrestricted keys, and member sessions (`scopes: null`) skip. A key with `scopes: []` is restricted (empty list) — it cannot satisfy any `required_scopes` and gets **403** there; unlabeled routes still admit it.

### Rate limits

Counters live in **Valkey**. Replicas share one budget. `VALKEY_URL` is required to boot (`/health/ready` 503 until Valkey answers PING). A command or connect that does not finish within 500ms is a Valkey error: limited requests get **503** `SERVICE_UNAVAILABLE`. The gateway opens a new connection when Valkey answers again. Schema and buckets: [`routes.md`](routes.md).

| | |
|--|--|
| Fallback | `RATE_LIMIT_REQUESTS` (default 60; `0` = unlimited), `RATE_LIMIT_WINDOW_SECONDS` (default 60) |
| Per-route | omitted inherits fallback; `{requests, window_seconds}` = this route+method only; policy **name** = service `rate_limits` entry; `false` opts out |
| Named | `rate_limits` on the service. Name without `shared` → `{service}:{name}:{subject}`. `shared: true` → `{name}:{subject}` (opt-in cross-service; both services declare the same entry) |
| Who | All admitted routes (JWT, API key, and member session) |
| Subject | Derived from route scope: `public`→ip, `user`→`user_id`, `organization`→`organization_id`, `member`→`member_id`. Not configurable. |
| Exceed | **429** `RATE_LIMITED`, type `api_error`, message `Too many requests. Try again in a moment.`, `details.retry_after_seconds`, `Retry-After` |
| Admitted headers | On limited admitted routes (2xx and 429): `X-RateLimit-Limit` (policy `requests`), `X-RateLimit-Remaining` (after this request; `0` on 429), `X-RateLimit-Reset` (unix epoch seconds when the window ends). Omitted on unlimited routes (`false` or fallback `0`). |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). Unadmitted 401s and unmatched 404s. Not per-route. **429** still sets `Retry-After`; no `X-RateLimit-*` (do not advertise this budget). |

## Rules for Services Behind the Gateway

Direct-exposed services are exempt (see below).

The rewritten path is authentic only if the upstream is on a private network the gateway can reach.

1. **Trust the path the gateway wrote** — Do not validate tokens. Read `{subject.*}` from `upstream`. Do not read a subject header.
   - `user`: `user_id`
   - `organization`: `organization_id`
   - `member`: `organization_id` and `member_id`
2. **Propagate `traceparent`** on downstream calls
3. **Log with `request_id`** from `X-Request-ID`
4. **Do not set `X-Request-ID` on responses** — gateway owns it
5. **Do not read `X-User-Id`, `X-Organization-Id`, or `X-Member-Id`** — the gateway removes them and does not set them

### Platform integrity (hard limits)

These protect the shared auth model for the entire platform.

1. **Do not parse `Authorization`** — gateway consumes it and strips it before upstream; you will not receive it
2. **Do not validate JWTs** — gateway validates via JWKS
3. **Do not read `X-API-Key`** — gateway consumes and strips it before upstream
4. **Do not implement CORS** — gateway handles preflight and headers
5. **Do not generate `X-Request-ID`** — gateway generates it; propagate if present. Missing → gateway bug

## Credentials

`Authorization` is the IdP JWT only. Plat5 secrets stay on `X-API-Key`. If `X-API-Key` is present, it is the credential. Do not fall through to the other header. Do not try the other validator.

Wrong credential for the scope is **401**. There is nothing to compare, so this is not **404**.

| Scope | Accepts | Rejects |
|-------|---------|---------|
| `user` | `Authorization: Bearer` JWT, or `X-API-Key` `{brand}-sk-1-` | member key, session |
| `organization`, `member` | `X-API-Key` `{brand}-mk-1-` or `{brand}-ms-1-` | user JWT, user API key |

Prefix dispatch happens before the identity call. One member-credential result (`member_id`, `organization_id`, `scopes`) feeds both `organization` and `member`. The scope picks the fields `upstream` may fill.

1. Bad, missing, or wrong credential → **401** `UNAUTHORIZED`
2. Validate unavailable → **503** `SERVICE_UNAVAILABLE`
3. Admitted → substitute `{subject.*}` in `upstream`
4. Then `required_scopes` (restricted keys only; session `scopes: null` skips) → **403** `FORBIDDEN` on miss
5. Then per-route rate limit → **429** `RATE_LIMITED`

Gateway chooses validate URL by **wire prefix** before calling identity. Prefixes come from `APIKEY_BRAND` (same env as identity; unset → `plat5`). Contract: [`identity.md`](identity.md).

| Prefix | Endpoint env | Scopes |
|--------|--------------|--------|
| `{brand}-sk-1-` | `USER_APIKEY_VALIDATE_URL` | `user` |
| `{brand}-mk-1-` | `MEMBER_APIKEY_VALIDATE_URL` | `organization`, `member` |
| `{brand}-ms-1-` | `MEMBER_SESSION_VALIDATE_URL` | `organization`, `member` |

An active service account can call `organization` and `member` routes. It uses a member key. It does not mint a session.

Identity's own HTTP paths name the subject. The catalog publishes only routes whose subject the template can fill. See [`identity.md`](identity.md) and [`routes.md`](routes.md).

## Internal identity control plane

Gateway calls identity backends over HTTP (not published on the edge route map):

| Call | Env | Typical URL |
|------|-----|-------------|
| User API key validate | `USER_APIKEY_VALIDATE_URL` | `http://identity:3001/internal/user-keys/validate` |
| Member API key validate | `MEMBER_APIKEY_VALIDATE_URL` | `http://identity:3001/internal/member-keys/validate` |
| Member session validate | `MEMBER_SESSION_VALIDATE_URL` | `http://identity:3001/internal/member-sessions/validate` |

All three live on identity’s **`INTERNAL_PORT`**. All three URLs are required to boot. Optional shared `INTERNAL_AUTH_TOKEN` is sent as `X-Plat5-Internal-Token`. Contract: [`identity.md`](identity.md).

## Public Routes

`public` scope has no subject. Do not attempt auth checks.

## Direct-Exposed Services

Some services are intentionally exposed outside the gateway (e.g. an **IdP / issuer** at `auth.company.com` while the gateway serves `api.company.com`):

- Handle their own CORS (browser OAuth flows require it)
- Do not rely on a gateway subject in the path
- Exempt from “Do not implement CORS”

TLS still typically terminates at the edge.

## TLS Termination

TLS terminates at the **edge / load balancer**, not the gateway process. Edge decrypts and forwards plaintext HTTP to the gateway (e.g. `localhost:5001`). Certificate management stays at the edge provider. Services behind the gateway receive plaintext HTTP and must not terminate TLS.

## CORS

Gateway handles `OPTIONS` preflight and adds `Access-Control-Allow-*` on responses. Services behind the gateway have no CORS config. Direct-exposed services handle CORS as needed.

## Security headers

On public proxy responses (including errors; not internal `/health` / `/metrics`):

| Header | Value |
|--------|--------|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |

## JWKS

`AUTH_JWKS_URI` is required to boot. Empty cache retries every 2s until loaded; loaded cache refreshes every 15 minutes.

## Admission cache

In-process per replica.

| Cache | Positive | Negative | TTL |
|-------|----------|----------|-----|
| JWT claims | Validated token (TTL from `exp`) | — | token `exp` |
| User API key | Valid key → `user_id` + `scopes` | Invalid key | `APIKEY_CACHE_TTL_SECS` (default 300) |
| Member API key | Valid key → `member_id` + `organization_id` + `scopes` | Invalid key | same |
| Member session | Valid token → `member_id` + `organization_id` + `scopes: null` | Invalid token | same |

`APIKEY_CACHE_TTL_SECS` covers user keys, member keys, and sessions. Do not rename it for sessions.

Do not cache identity **503** / transport failures. Concurrent misses for the same cache key share **one** identity call (singleflight). Raw API keys and JWTs are hashed before use as cache keys.

Revoke, suspend, and remove are visible at the edge when the TTL expires.

## Boot / ready

`/health/ready` is **200** when JWKS is loaded **and** Valkey answers PING within 500ms. Otherwise **503**. etcd empty (no routes) is still ready — unmatched paths are **404**.

## Errors

| Case | Code |
|------|------|
| Auth failure (bad/missing credential) | `UNAUTHORIZED` (401) — gateway only |
| Restricted API key missing route `required_scopes` | `FORBIDDEN` (403) |
| Route not registered | `NOT_FOUND` (404) |
| Request body too large | `PAYLOAD_TOO_LARGE` (413) |
| Rate limit (admitted route or failed-auth IP) | `RATE_LIMITED` (429); `Retry-After`; admitted limited routes also `X-RateLimit-*` |
| Upstream or auth infra down (JWKS, Valkey, key or session validate) | `SERVICE_UNAVAILABLE` (503); proxy upstream failure may surface as **502** with the same `SERVICE_UNAVAILABLE` code |
| Path param is not one segment | `INVALID_REQUEST` (400) |
| Subject id is not one segment | `INTERNAL_ERROR` (500) |
| Gateway internal failure mid-proxy | `INTERNAL_ERROR` (500) |

All of the above use the Plat5 JSON envelope (`api-errors.md`), including failures handled in `fail_to_proxy`. Client disconnect (no response needed) does not write a body.
