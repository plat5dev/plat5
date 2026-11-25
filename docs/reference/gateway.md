# Gateway

The Plat5 gateway (`services/gateway/`). How it decides a request: [access](../concepts/access.md#how-a-request-is-decided). What services receive: [`reference/headers.md`](headers.md). Environment: [`reference/env.md`](env.md#gateway).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT, API key), resolving path slugs to node ids, admission, permission checks, rate limits, upstream fill, audit intent and outcome, route checks, CORS, security headers, body limits, trace propagation |
| **Edge / load balancer** | TLS termination |
| **Identity** | Admit: the member and permissions for a credential on a node chain |
| **Services** | Business logic, data access, and state rules over the node ids they receive |

## Credentials

Every credential is `Authorization: Bearer`. Three dot-separated segments is a JWT, verified locally against JWKS; a JWT that fails is **401** without an identity call. Anything else is sent to admit as a key, as presented: the gateway doesn't know the key format.

## Admit

```
POST {IDENTITY_INTERNAL_URL}/internal/admit
{ "credential" | "idp" | "anonymous": true, "chain": [{ "kind", "ref" }], "allow_deleted", "allow_public" }
→ { "valid", "principal", "key_id", "allowed_cidrs", "org_allowed_cidrs", "nodes": [{ "kind", "id" }],
    "member_id", "user_id", "permissions", "rate_limit_tier", "expires_at", "invalidation_seq", "config_revision" }
```

On identity's `INTERNAL_PORT`. An empty chain is a user route. Contract: [`reference/api/internal.md`](api/internal.md#admit).

### Cache

In-process per replica, backed by identity's invalidation feed (`GET {IDENTITY_INTERNAL_URL}/internal/admit-invalidations`, [`reference/api/internal.md`](api/internal.md#admit-invalidations)).

| | |
|--|--|
| Key | The credential (a key as presented, anonymous flag, or a verified JWT's issuer, subject, forwarded scopes when `AUTH_SCOPE_CLAIM` is set, and forwarded policy claims when `AUTH_POLICY_CLAIMS` is set), `allow_deleted`, `allow_public`, and the chain refs as written in the path |
| Invalidation | Targeted epoch checks (`key_id`, `user_id`, `member_id`, `root_id`, global reset, and identity's `config_revision`) driven by identity's invalidation feed; entries stamped before the admit call so in-flight revocations are never pinned. A lagging identity replica returning an older `config_revision` is served without caching |
| TTL | `ADMIT_CACHE_TTL` (default `5m`), capped at `expires_at - now` when admit returns a key or binding expiry — fallback upper bound if the invalidation feed is disconnected |
| Negative entries | For invalid credentials and `member_id: null`, for `ADMIT_NEGATIVE_CACHE_TTL` (default `10s`). Never for identity errors |
| Singleflight | Concurrent misses for one key share a single identity call |
| JWT verification | Each request; the token's `exp` is checked every time |

## Control plane

Routes, kinds, rate limits, defaults and CORS come from the control plane (`CONTROL_PLANE_URL`, required to boot). The gateway reads `GET {CONTROL_PLANE_URL}/internal/gateway`.

- **Boot.** Waits for its first snapshot, retrying while the control plane is down, and doesn't listen until it has one.
- **Running.** Long-polls with `?after={revision}` and swaps them whole on each change.
- **Control plane down.** Keeps serving the last snapshot.
- **Invalid snapshot.** Refused whole: keeps the current snapshot, logs an error, counts it, and reports the revision and reason on its next poll.
- **Reporting.** Every long poll carries the replica id and loaded revision ([feeds](api/internal.md#control-plane-feeds)).

## Rate limits

Controlled by `RATE_LIMIT_ENABLED` (default `true`). When enabled, counters live in **Valkey** so replicas share one budget: `VALKEY_URL` is required to boot (URL syntax is validated at boot). Rate limiting always **fails open** when Valkey is unreachable or times out (> 500ms): requests are allowed without `X-RateLimit-*` headers, the failure is logged on state transitions and counted (`rate_limit_fail_open_total`), and a background reconnect loop restores enforcement automatically once Valkey recovers. Valkey availability does not block boot or fail `/health/ready`.

When `RATE_LIMIT_ENABLED=false`, `VALKEY_URL` is ignored, Valkey need not run, and configured rate limits (including the failed-auth IP limiter) are skipped. The gateway logs the mode at boot.

Policies and buckets: [rate limits](config/rate-limits.md).

| | |
|--|--|
| Check | Every policy on the route in one round trip, counted only if all pass |
| Exceed | **429** `RATE_LIMITED`, `details.retry_after_seconds`, `Retry-After` |
| Admitted headers | On limited routes (2xx and 429) when Valkey answered: `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, for the tightest policy (omitted when failing open) |
| Keys | Under `VALKEY_KEY_PREFIX` (default `plat5:rl:`) |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW` (default `60` / `60s`; `0` requests = off). What it counts: [access](../concepts/access.md#how-a-request-is-decided). **429** sets `Retry-After`; no `X-RateLimit-*` |

## Audit

Controlled by `AUDIT_ENABLED` (default `true`) and `AUDIT_MODE` (`sync` or `async`, default `sync`). In `sync` mode, on an audited route other than `public`, the gateway writes an intent after the permission check and before forward, and an outcome after; an unwritten intent returns **503** and the service isn't called. In `async` mode, audited requests forward immediately and queue one completed event in the background after the response. In both modes, gateway refusals after admit are one background event. `AUDIT_URL` is required to boot unless `AUDIT_ENABLED=false`. Contract: [`concepts/audit.md`](../concepts/audit.md).

## Client IP

What an audit event records as `ip`, and what `public` routes and the failed-auth limiter key on.

The TCP peer, unless the peer is in `TRUSTED_PROXIES` (comma-separated addresses and CIDRs). Then `X-Forwarded-For` is walked from the right, past trusted proxies, and the first address that isn't one is the client. Nothing left of an untrusted hop is read. An entry that isn't an address stops the walk at the last trusted proxy.

`TRUSTED_PROXIES` empty (the default): `X-Forwarded-For` is ignored.

## Paths

A param value of `.` or `..`, or one holding `/`, `?` or `#` once decoded, is **400**: it would change the path the upstream sees. The query string is forwarded as sent.

## Request bodies

A body larger than the service's `max_body_bytes` (or `defaults.max_body_bytes`, default 10 MiB) is **413** `PAYLOAD_TOO_LARGE`, checked on `Content-Length` and while streaming.

## Route checks

Whether the caller may call each of a batch of routes, for a UI that hides what the caller can't use ([access](../concepts/access.md#what-a-ui-shows)). Answered directly by the gateway at `POST /access/routes`; no service route may register `/access/routes`.

```
POST /access/routes
Authorization: Bearer …
{
  "checks": [
    { "method": "GET", "path": "/orgs/acme/projects/web/bindings" },
    { "method": "DELETE", "path": "/orgs/acme" }
  ]
}
→ 200
{ "checks": [ { "allowed": true }, { "allowed": false } ] }
```

| Field | Rule |
|-------|------|
| `checks` | 1 to 100. Answered in request order, one each |
| `method` | `GET`, `POST`, `PUT`, `PATCH`, `DELETE` or `HEAD`, uppercase |
| `path` | A public path: starts with `/`, visible ASCII, no `?` or `#`, at most 2048 characters |

Each `allowed` runs the gateway's own route match, admit (using the admit cache) and permission check:

| The route | `allowed` |
|-----------|-----------|
| None matches the path and method | `false` |
| `public` | `true` |
| `user` | `true` for a person, `false` for a service account |
| A kind | `true` when admit accepts the caller on the chain (a deleted node only on an `allow_deleted` route; public principals when `allow_public: true`), the root's `allowed_cidrs` permits the client IP, and built-in permissions plus any attached outbound `authorizer` (batched per authorizer) allow the route |

- **Credential.** Missing, malformed, or locally refused JWT → **401**. Any admit answering `valid: false` makes the whole call **401** (counted once by the failed-auth limiter when enabled). A batch that needs no admit (all `public` or unmatched) never calls identity.
- **Rate limit.** When rate limiting is enabled and `defaults.rate_limit` is set, counted once per call against `defaults.rate_limit` per credential (a key's hash, or a JWT's issuer and subject), with `X-RateLimit-*` on 200 and 429.
- **Not audited**, and nothing is forwarded. A new person's first check that hits admit creates them, like any first request.
- **Validation.** Bad JSON, `checks` outside 1–100, or bad `method`/`path` → **422** `VALIDATION_ERROR`. Body over 256 KiB → **413** `PAYLOAD_TOO_LARGE`. Other methods on `/access/routes` → **404**.

## TLS termination

TLS terminates at the edge or load balancer, not the gateway. Services behind the gateway receive plaintext HTTP (or HTTPS when their `url` is `https://…`).

## Upstream security

- **Request signing.** Set `UPSTREAM_SIGNING_KEY` (and optionally `UPSTREAM_SIGNING_KEY_ID`) to attach an Ed25519 `X-Plat5-Signature` header to every forwarded request ([`reference/headers.md`](headers.md#upstream-request-signing)), so services outside the private network can verify requests came from the gateway.
- **Private-IP blocking.** Set `UPSTREAM_BLOCK_PRIVATE_IPS=true` to refuse upstream connections (and outbound AuthZEN PDP calls) whose DNS-resolved address is loopback, private (RFC1918 / IPv6 ULA), link-local (`169.254.0.0/16` / `fe80::/10`), CGNAT (`100.64.0.0/10`), unspecified, or an IPv4-mapped IPv6 address pointing to any of those. Blocked upstream requests return **502** `SERVICE_UNAVAILABLE`; blocked authorizer calls return **503** `SERVICE_UNAVAILABLE`.

## Outbound authorizers

Routes in a kind scope may attach (or inherit from their kind) an external OpenID AuthZEN 1.0 PDP ([`reference/config/authorizers.md`](config/authorizers.md)) in `replace`, `and`, or `or` mode, with optional record-level `resource: { type, id }` templates. Decisions fail closed (**503** on timeout/error), respect credential scope narrowing, and batch per authorizer during `POST /access/routes`. Per-authorizer `cache_ttl` and `negative_cache_ttl` control response caching (`0s` by default for immediate consistency).

## CORS

The gateway answers every `OPTIONS` request as a preflight: `204 No Content`, `Access-Control-Allow-*` and `Access-Control-Max-Age: 86400`. It adds `Access-Control-Allow-Origin` and `Access-Control-Expose-Headers` on responses.

| Header | Value |
|--------|-------|
| `Access-Control-Allow-Origin` | With `allowed_origins: ["*"]`, `*`. Otherwise an `Origin` in the list, echoed, with `Vary: Origin`; otherwise omitted |
| `Access-Control-Allow-Methods` | `GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS` |
| `Access-Control-Allow-Headers` | `Authorization`, `Content-Type`, `X-Request-ID`, `traceparent`, `tracestate`, plus `cors.allowed_headers` |
| `Access-Control-Expose-Headers` | `X-Request-ID`, `Retry-After`, `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` |

## Security headers

On public proxy responses (errors included, not `/health` or `/metrics`), each added only when the upstream response hasn't set it:

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Referrer-Policy: strict-origin-when-cross-origin`

## JWKS

`AUTH_JWKS_URI` is required to boot. An empty cache retries every 2s until loaded; a loaded cache refreshes every 15 minutes. `AUTH_ALLOWED_AUDIENCES` is required: a token's `aud` must hold one of them, or `*` accepts any audience ([`reference/idp.md`](idp.md)).

## Boot and ready

The gateway listens only once it has a control-plane snapshot. `/health/ready` is **200** when JWKS is loaded, otherwise **503**. The JSON body still reports `checks.valkey_ready` for visibility when `RATE_LIMIT_ENABLED=true`, but a Valkey outage does not fail readiness (rate limiting fails open). The control plane and audit aren't part of ready: while the control plane is down, the gateway serves its last snapshot; while audit is down, audited routes return **503** in `sync` mode (or queue in `async` mode) and unaudited routes keep working.

## Errors

| Case | Code |
|------|------|
| Bad or missing credential, key IP allowlist refusal, or denied anonymous `allow_public` request | `UNAUTHORIZED` (401) |
| Node not found, not in the tree, org security policy/IP refusal, or permission/authorizer denied | `FORBIDDEN` (403) |
| A service account or narrowed personal key on a user route | `FORBIDDEN` (403) |
| Route not registered | `NOT_FOUND` (404) |
| Request body too large | `PAYLOAD_TOO_LARGE` (413) |
| Bad `/access/routes` body | `VALIDATION_ERROR` (422) |
| Rate limit (route policy or failed-auth IP) | `RATE_LIMITED` (429) |
| JWKS, identity, outbound authorizer, or sync audit intent down | `SERVICE_UNAVAILABLE` (503) |
| Upstream unreachable, timed out, or failing mid-proxy | **502** `SERVICE_UNAVAILABLE` |
| Path param `.` / `..` or containing `/` / `?` / `#` | `INVALID_REQUEST` (400) |

All use the Plat5 error envelope ([`reference/errors.md`](errors.md)).
