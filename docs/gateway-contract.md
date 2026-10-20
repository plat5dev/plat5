# Gateway Contract

Auth delegation, subject fill, and trace propagation from the Plat5 gateway (`services/gateway/`).

Boundary: [`identity-boundary.md`](identity-boundary.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity backends: [`identity.md`](identity.md). Audit: [`audit.md`](audit.md).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT, API key, session), scope projection, route `required_labels`, rate limits, audit intent and outcome, subject fill into `upstream`, CORS, security headers, trace propagation |
| **Edge / Load Balancer** | TLS termination (e.g. Cloudflare Zero Trust tunnels) |
| **Policy engine** (optional) | Access to individual resources. The gateway asks it about one resource; a service asks it which resources its subject can reach ([`model.md`](model.md#what-i-can-reach)) |
| **Downstream services** | Business logic, data access, and rules that hold whoever asks: state rules, and scoping every lookup by the subject |

Services behind the gateway **must not** re-validate JWTs or parse `Authorization`. The gateway handles authentication entirely.

## Subject

The route scope names the subject. `upstream` fills it. The client path does not. A route that needs the subject sets `upstream`. Omitted means proxy `path` unchanged: the upstream has no subject id.

| Scope | Credential | Subject fields |
|-------|------------|----------------|
| `public` | none | none |
| `user` | a person's credential: IdP JWT or **user** API key | `user_id` |
| `organization` | a member's credential: **member** API key or **session** | `organization_id` |
| `member` | same credential as `organization` | `organization_id`, `member_id` |

### Stripped before upstream (all scopes)

The gateway removes these request headers before the upstream call.

| Header | Why |
|--------|-----|
| `Authorization` | Consumed for JWT authn; must not leak bearer tokens to apps |
| `X-API-Key` | Consumed for API-key authn; must not leak raw keys to apps |
| `X-Plat5-Audit-Details`, `X-Plat5-Audit-Created`, `X-Plat5-Audit-Organization`, `X-Plat5-Audit-Member` | A service sets them on responses. A client must not hand one to a service that echoes headers |

Clients still send credential headers **to the gateway**. Services behind the gateway will not receive `Authorization` or `X-API-Key`. CORS may still allow browsers to send them.

### Stripped from responses (all scopes)

| Header | Why |
|--------|-----|
| `X-Plat5-Audit-Details`, `X-Plat5-Audit-Created`, `X-Plat5-Audit-Organization`, `X-Plat5-Audit-Member` | What the service tells audit ([`audit.md`](audit.md#what-a-service-adds)). Stripped on every route, audit on or off |

### Always (all scopes)

| Header | Description |
|--------|-------------|
| `X-Request-ID` | Correlation ID (gateway-generated; also on response) |
| `traceparent` | W3C trace context (OTel propagation) |

The scope names who owns what the route addresses: the person (`user`), the org (`organization`), or the member's own membership (`member`). It never names who called, and it is not a second proof. A user JWT and a user API key are the same proof. `organization` and `member` share one credential. That credential always carries `member_id` and `organization_id`. The scope drops fields before `upstream` substitution. Spans may still record the dropped ids. The app contract does not.

### Caller labels

Services are not told the caller's role or labels. The gateway decides which routes a caller may reach (`required_labels`, below), and nothing downstream repeats that check. Access to individual resources is the optional policy engine's: the gateway asks it about one resource, and a service asks it which resources its subject can reach ([`model.md`](model.md#what-i-can-reach)). A service enforces only what holds whoever asks.

## Route Configuration

Routes come from the control plane ([`control-plane.md`](control-plane.md)). Scopes are `public` / `user` / `organization` / `member` blocks. Full schema and publish path: [`routes.md`](routes.md).

```yaml
services:
  my-service:
    url: http://my-service:3000
    public:
      routes:
        - path: /public/health
          methods: [GET]
    organization:
      routes:
        - path: /org/widgets
          upstream: /organizations/{subject.organization_id}/widgets
          methods: [GET, POST]
          required_labels: [widgets:read]
        - path: /org/features
          upstream: /organizations/{subject.organization_id}/features
          methods:
            GET:
              required_labels: [org:read]
            POST:
              required_labels: [org:write]
```

`methods` may be a list (route-level `required_labels` / `rate_limit` apply to every verb) or a nested map (per-verb). Nested maps are expanded at apply into one stored row per verb; the gateway always sees `methods` as a string array. Full schema: [`routes.md`](routes.md).

Services publish via the **control plane** admin API (`POST /apply`). The gateway loads a snapshot at startup and long-polls `CONTROL_PLANE_URL` for changes ([`control-plane.md`](control-plane.md#internal-feeds)). Route existence is decoupled from service health — a downed service returns 502 `SERVICE_UNAVAILABLE`, not 404.

### Route `required_labels`

After admission and the rate limit: if the route has `required_labels`, a member credential's `labels` must hold `*` or share a label with the route, or **403** `FORBIDDEN`. One shared label is enough.

A credential carries its principal's labels. For a member key or member session, validate's list is the member's role labels ([`roles.md`](roles.md)). `labels` is always a list. `["*"]` (a `["*"]` role, or roles off) matches any route label. A route cannot require `*` itself. A valid validate result without a list counts as `[]`. `labels: []` cannot satisfy any `required_labels` and gets **403** there; unlabeled routes still admit it. User credentials (JWT and user API key) have no role and skip, so labels constrain `organization` and `member` routes only.

The **403** `details` are `{ "permission": "required_labels", "resource": "route", "resource_id": "<route path>", "required_labels": [...] }`. `required_labels` is the route's list, so a client can tell which labels would have admitted it. The body never carries the caller's labels. The gateway logs them, with the route's list and who called, on a `required_labels denied` line ([`telemetry.md`](telemetry.md#gateway-label-denied-line)).

### Rate limits

Counters live in **Valkey**. Replicas share one budget. `VALKEY_URL` is required to boot (`/health/ready` 503 until Valkey answers PING). A command or connect that does not finish within 500ms is a Valkey error: limited requests get **503** `SERVICE_UNAVAILABLE`. The gateway opens a new connection when Valkey answers again. Schema and buckets: [`routes.md`](routes.md).

| | |
|--|--|
| Fallback | `RATE_LIMIT_REQUESTS` (default 60; `0` = unlimited), `RATE_LIMIT_WINDOW_SECONDS` (default 60) |
| Per-route | omitted inherits fallback; `{requests, window_seconds}` = this route+method only; policy **name** = service `rate_limits` entry; `false` opts out |
| Named | `rate_limits` on the service. Name without `shared` → `{service}:{name}:{subject}`. `shared: true` → `{name}:{subject}` (opt-in cross-service; both services declare the same entry) |
| Who | All admitted routes (JWT, API key, and member session). Runs before `required_labels`, so a **403** counts |
| Subject | Derived from route scope: `public`→ip, `user`→`user_id`, `organization`→`organization_id`, `member`→`member_id`. Not configurable. Every `organization` route shares one bucket per org, so a runaway service account can throttle the people in its org: give its routes a named policy, or its work a `member` route. |
| Exceed | **429** `RATE_LIMITED`, type `api_error`, message `Too many requests. Try again in a moment.`, `details.retry_after_seconds`, `Retry-After` |
| Admitted headers | On limited admitted routes (2xx and 429): `X-RateLimit-Limit` (policy `requests`), `X-RateLimit-Remaining` (after this request; `0` on 429), `X-RateLimit-Reset` (unix epoch seconds when the window ends). Omitted on unlimited routes (`false` or fallback `0`). |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). Unadmitted 401s and unmatched 404s. Not per-route. **429** still sets `Retry-After`; no `X-RateLimit-*` (do not advertise this budget). |

## Rules for Services Behind the Gateway

Direct-exposed services are exempt (see below).

The rewritten path is authentic only if the upstream is on a private network the gateway can reach.

1. **Trust the path the gateway wrote** — Do not validate tokens. Read `{subject.*}` from `upstream`.
   - `user`: `user_id`
   - `organization`: `organization_id`
   - `member`: `organization_id` and `member_id`
2. **Propagate `traceparent`** on downstream calls
3. **Log with `request_id`** from `X-Request-ID`
4. **Do not set `X-Request-ID` on responses** — gateway owns it
5. **To say what changed, set `X-Plat5-Audit-Details`** on the response of an audited route, and `X-Plat5-Audit-Created` on a create. No secrets. The gateway strips them ([`audit.md`](audit.md#what-a-service-adds))

### Platform integrity (hard limits)

These protect the shared auth model for the entire platform.

1. **Do not parse `Authorization`** — gateway consumes it and strips it before upstream; you will not receive it
2. **Do not validate JWTs** — gateway validates via JWKS
3. **Do not read `X-API-Key`** — gateway consumes and strips it before upstream
4. **Do not implement CORS** — gateway handles preflight and headers
5. **Do not generate `X-Request-ID`** — gateway generates it; propagate if present. Missing → gateway bug

## Credentials

`Authorization` is the IdP JWT only. Plat5 credentials (API keys and sessions) stay on `X-API-Key`. If `X-API-Key` is present, it is the credential. Do not fall through to the other header.

Every credential belongs to a membership in a realm: the directory (a person's own) or an organization (a member's). The scope decides which realm a route accepts. Wrong realm for the scope is **401**. There is nothing to compare, so this is not **404**.

| Scope | Accepts | Rejects |
|-------|---------|---------|
| `user` | `Authorization: Bearer` JWT, or an `X-API-Key` whose validate realm is `directory` (a user key) | a member key or session |
| `organization`, `member` | An `X-API-Key` whose validate realm is `organization` (a member key or session) | a user JWT or user key |

**JWT.** The gateway verifies it against JWKS ([`idp-contract.md`](idp-contract.md)), reads the issuer and the subject claim, and resolves them with validate `{ "idp": { "issuer", "subject" } }` to the person's `user_id`. A new subject creates the person.

**`X-API-Key`.** The gateway checks the shape first: `{brand}-{k|s}-{version}-{secret}`, with a brand of `[a-z][a-z0-9]*` up to 32 characters and a 43-character base64url secret, or a version 1 form. A value of any other shape is **401** without an identity call, and counts against the failed-auth limiter. The gateway does not know the brand. Validate decides the rest ([`identity.md`](identity.md#credential-validate)).

One organization result (`member_id`, `organization_id`, and the role's `labels`) feeds both `organization` and `member`. The scope picks the fields `upstream` may fill.

1. Bad, missing, or wrong credential → **401** `UNAUTHORIZED`, whose message names what the route takes ([`error-copy.md`](error-copy.md#gateway-401))
2. Validate unavailable → **503** `SERVICE_UNAVAILABLE`
3. Admitted → per-route rate limit → **429** `RATE_LIMITED`
4. Then, on an audited route with audit on, the audit intent → **503** `SERVICE_UNAVAILABLE` if it cannot be written ([`audit.md`](audit.md#request-order))
5. Then `required_labels` (member credentials; `*` matches any label; users skip) → **403** `FORBIDDEN` on miss
6. Then substitute `{subject.*}` and `{path.*}` in `upstream` (bad path param → **400**, bad subject id → **500**)

An active service account can call `organization` and `member` routes. It uses a member key. It does not mint a session.

Identity's own HTTP paths name the subject. See [`identity.md`](identity.md) and [`routes.md`](routes.md).

## Internal identity API

Gateway calls identity over HTTP (not published on the edge route map):

| Call | Env | Typical URL |
|------|-----|-------------|
| Credential validate | `CREDENTIAL_VALIDATE_URL` | `http://identity:3001/internal/credentials/validate` |

It lives on identity’s **`INTERNAL_PORT`** and is required to boot. Optional shared `INTERNAL_AUTH_TOKEN` is sent as `X-Plat5-Internal-Token`. Contract: [`identity.md`](identity.md#credential-validate).

## Control plane

Routes and CORS origins come from the control plane: `CONTROL_PLANE_URL` (e.g. `http://control-plane:5003`), required to boot. The gateway reads `GET {CONTROL_PLANE_URL}/internal/gateway`, sending the same `INTERNAL_AUTH_TOKEN`.

- **Boot.** The gateway waits for its first snapshot, retrying while the control plane is down, and does not listen until it has one.
- **Running.** It long-polls with `?after={revision}` and swaps routes and CORS whole on each change.
- **Control plane down.** It keeps serving the last snapshot.
- **A snapshot it cannot validate.** It keeps the current one and logs an error.

Contract: [`control-plane.md`](control-plane.md#internal-feeds).

## Audit

On an audited `user`, `organization`, or `member` route, the gateway writes an intent to the audit service before forward and an outcome after. Intent not written → **503**, and the service is not called. Audit is on or off for the deployment: `AUDIT_URL` is required to boot unless `AUDIT_ENABLED=false`. Same `INTERNAL_AUTH_TOKEN`. Contract: [`audit.md`](audit.md).

## Public Routes

`public` scope has no subject. Do not attempt auth checks.

## Direct-Exposed Services

Some services are intentionally exposed outside the gateway (e.g. an **IdP / issuer** at `auth.company.com` while the gateway serves `api.company.com`):

- Handle their own CORS (browser OAuth flows require it)
- Do not rely on a gateway subject in the path
- Exempt from “Do not implement CORS”

TLS still typically terminates at the edge.

## Client IP

What an audit event records as `ip`, and what `public` routes and the failed-auth limiter key on.

The TCP peer, unless the peer is in `TRUSTED_PROXIES` (comma-separated addresses and CIDRs). Then `X-Forwarded-For` is walked from the right, past trusted proxies, and the first address that is not one is the client. A client can write anything at the left of the header, so nothing left of an untrusted hop is read. An entry that is not an address stops the walk at the last trusted proxy. No other header names the client.

`TRUSTED_PROXIES` empty (the default): `X-Forwarded-For` is ignored. Behind a tunnel or load balancer, list it, or every request has the edge's IP.

## TLS Termination

TLS terminates at the **edge / load balancer**, not the gateway process. Edge decrypts and forwards plaintext HTTP to the gateway (e.g. `localhost:5001`). Certificate management stays at the edge provider. Services behind the gateway receive plaintext HTTP and must not terminate TLS.

## CORS

Gateway answers every `OPTIONS` request as a preflight: `204 No Content`, no body, `Access-Control-Allow-*` and `Access-Control-Max-Age: 86400`; the connection stays reusable. It adds `Access-Control-Allow-*` on responses. Services behind the gateway have no CORS config. Direct-exposed services handle CORS as needed.

The allowed origins are the control plane's `cors` document ([`control-plane.md`](control-plane.md#cors)), and change without a restart. No document, or an empty list, answers `Access-Control-Allow-Origin: *`. Otherwise an `Origin` in the list is echoed with `Vary: Origin`, and any other origin gets no `Access-Control-Allow-Origin`.

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
| IdP subject | Issuer + subject → `user_id` | Suspended person | `APIKEY_CACHE_TTL_SECS` (default 300) |
| Credential | Valid credential → the validate result (`user_id`, or `member_id` + `organization_id` + `labels`) | Invalid credential | same |

`APIKEY_CACHE_TTL_SECS` covers every credential and subject resolve. Do not rename it.

Do not cache identity **503** / transport failures. Concurrent misses for the same cache key share **one** identity call (singleflight). Raw API keys and JWTs are hashed before use as cache keys.

Revoke, suspend (of a member or a person), remove, and role changes are visible at the edge when the TTL expires.

## Boot / ready

The gateway listens only once it has a control-plane snapshot. `/health/ready` is **200** when JWKS is loaded **and** Valkey answers PING within 500ms. Otherwise **503**. The control plane is not part of ready: while it is down, the gateway serves its last snapshot. A snapshot with no routes is still ready — unmatched paths are **404**. Audit is not part of ready: while it is down, audited routes are **503** and the rest keep working.

## Errors

| Case | Code |
|------|------|
| Auth failure (bad/missing credential) | `UNAUTHORIZED` (401) — gateway only |
| Member's role labels miss route `required_labels` | `FORBIDDEN` (403) |
| Route not registered | `NOT_FOUND` (404) |
| Request body too large | `PAYLOAD_TOO_LARGE` (413) |
| Rate limit (admitted route or failed-auth IP) | `RATE_LIMITED` (429); `Retry-After`; admitted limited routes also `X-RateLimit-*` |
| Auth infra down (JWKS, Valkey, credential validate) | `SERVICE_UNAVAILABLE` (503) |
| Audit intent not written (audited route) | `SERVICE_UNAVAILABLE` (503) |
| Upstream unreachable, timed out, or failing mid-proxy | **502** with the `SERVICE_UNAVAILABLE` code |
| Path param is not one segment | `INVALID_REQUEST` (400) |
| Subject id is not one segment | `INTERNAL_ERROR` (500) |
| Gateway internal failure mid-proxy | `INTERNAL_ERROR` (500) |

All of the above use the Plat5 JSON envelope (`api-errors.md`), including failures handled in `fail_to_proxy`. Client disconnect (no response needed) does not write a body.
