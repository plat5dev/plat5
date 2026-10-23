# Gateway

The Plat5 gateway (`services/gateway/`). How it decides a request: [access](../concepts/access.md#how-a-request-is-decided). What services receive: [`reference/headers.md`](headers.md). Environment: [`reference/env.md`](env.md#gateway).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT, API key), resolving the path's slugs to nodes, admission, rate limits, audit intent and outcome, the label check, subject fill, CORS, security headers, trace propagation |
| **Edge / load balancer** | TLS termination (e.g. Cloudflare tunnels) |
| **Identity** | Admit: the member and labels for a credential on a node chain |
| **Services** | Business logic, data access, and state rules over the node ids they're given |

Services behind the gateway **must not** validate JWTs or parse `Authorization`, and never decide access by the caller. The gateway decided.

## Admit

```
POST {ADMIT_URL}
{ "credential" | "idp", "scope", "chain": [{ "kind", "ref" }], "resolve_deleted" }
→ { "valid", "found", "principal", "nodes": [{ "kind", "id" }], "member_id", "labels", "good_until" }
```

On identity's `INTERNAL_PORT`, required to boot. Optional `INTERNAL_AUTH_TOKEN` is sent as `X-Plat5-Internal-Token`. Contract: [`reference/api/internal.md`](api/internal.md#admit).

| Admit result | Status |
|--------------|--------|
| `valid: false` | **401** |
| `found: false`, or `member_id: null` | **403**, before the rate limit |
| `labels` share none with the route, and the route isn't `[]` | **403**, after the rate limit |
| Otherwise | Forwarded |
| Transport error, non-2xx, missing fields | **503** |


### Cache

In-process per replica.

| | |
|--|--|
| Key | Hash of the credential, the route's scope, and the chain refs as written in the path |
| TTL | The lesser of `APIKEY_CACHE_TTL_SECS` (default 300) and the result's `good_until` |
| Negative entries | For invalid credentials and not-found chains. Never for identity errors |
| Singleflight | Concurrent misses for one key share a single identity call |
| JWT claims | Cached until the token's `exp` |


What the TTL bounds: [access](../concepts/access.md#how-fast-changes-reach-the-edge).

## Control plane

Routes, kinds and CORS origins come from the control plane: `CONTROL_PLANE_URL` (e.g. `http://control-plane:5003`), required to boot. The gateway reads `GET {CONTROL_PLANE_URL}/internal/gateway` with the same `INTERNAL_AUTH_TOKEN`.

- **Boot.** Waits for its first snapshot, retrying while the control plane is down, and doesn't listen until it has one.
- **Running.** Long-polls with `?after={revision}` and swaps routes, kinds and CORS whole on each change.
- **Control plane down.** Keeps serving the last snapshot.
- **A snapshot it can't validate.** Keeps the current one and logs an error.

Contract: [`reference/api/internal.md`](api/internal.md#control-plane-feeds).

## Rate limits

Counters live in **Valkey**; replicas share one budget. `VALKEY_URL` is required to boot (`/health/ready` is 503 until Valkey answers PING). A command or connect that doesn't finish within 500ms is a Valkey error: limited requests get **503**. Schema and buckets: [`reference/config/routes.md`](config/routes.md#rate_limit).

| | |
|--|--|
| Fallback | `RATE_LIMIT_REQUESTS` (default 60; `0` = unlimited), `RATE_LIMIT_WINDOW_SECONDS` (default 60), counted at the route's own node |
| Exceed | **429** `RATE_LIMITED`, `details.retry_after_seconds`, `Retry-After` |
| Admitted headers | On limited routes (2xx and 429): `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, for the tightest policy |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). Unadmitted 401s, step 2's 403s, and unmatched 404s. **429** sets `Retry-After`; no `X-RateLimit-*` |

## Audit

On an audited route other than `public`, the gateway writes an intent before forward and an outcome after. Intent not written → **503**, and the service isn't called. `AUDIT_URL` is required to boot unless `AUDIT_ENABLED=false`. Contract: [`concepts/audit.md`](../concepts/audit.md).

## Client IP

What an audit event records as `ip`, and what `public` routes and the failed-auth limiter key on.

The TCP peer, unless the peer is in `TRUSTED_PROXIES` (comma-separated addresses and CIDRs). Then `X-Forwarded-For` is walked from the right, past trusted proxies, and the first address that isn't one is the client. Nothing left of an untrusted hop is read. An entry that isn't an address stops the walk at the last trusted proxy. No other header names the client.

`TRUSTED_PROXIES` empty (the default): `X-Forwarded-For` is ignored. Behind a tunnel or load balancer, list it, or every request has the edge's IP.

## TLS termination

TLS terminates at the edge or load balancer, not the gateway. Services behind the gateway receive plaintext HTTP and must not terminate TLS.

## CORS

The gateway answers every `OPTIONS` request as a preflight: `204 No Content`, `Access-Control-Allow-*` and `Access-Control-Max-Age: 86400`. It adds `Access-Control-Allow-*` on responses. Services behind the gateway have no CORS config.

The allowed origins are the control plane's `cors` document ([cors](config/cors.md)). No document, or an empty list, answers `Access-Control-Allow-Origin: *`. Otherwise an `Origin` in the list is echoed with `Vary: Origin`, and any other origin gets no `Access-Control-Allow-Origin`.

## Security headers

On public proxy responses, errors included (not internal `/health` / `/metrics`):

| Header | Value |
|--------|--------|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |

## JWKS

`AUTH_JWKS_URI` is required to boot. An empty cache retries every 2s until loaded; a loaded cache refreshes every 15 minutes.

## Boot and ready

The gateway listens only once it has a control-plane snapshot. `/health/ready` is **200** when JWKS is loaded **and** Valkey answers PING within 500ms, otherwise **503**. The control plane isn't part of ready: while it's down, the gateway serves its last snapshot. Audit isn't part of ready: while it's down, audited routes are **503** and the rest keep working.

## Errors

| Case | Code |
|------|------|
| Bad or missing credential | `UNAUTHORIZED` (401) |
| Node not found, not in the tree, or no route label held | `FORBIDDEN` (403), "permission denied, or it may not exist" |
| Route not registered | `NOT_FOUND` (404) |
| Request body too large | `PAYLOAD_TOO_LARGE` (413) |
| Rate limit (admitted route or failed-auth IP) | `RATE_LIMITED` (429) |
| JWKS, Valkey or identity down | `SERVICE_UNAVAILABLE` (503) |
| Audit intent not written | `SERVICE_UNAVAILABLE` (503) |
| Upstream unreachable, timed out, or failing mid-proxy | **502** with the `SERVICE_UNAVAILABLE` code |
| Path param not one segment | `INVALID_REQUEST` (400) |
| Subject id not one segment | `INTERNAL_ERROR` (500) |

All use the Plat5 envelope ([`reference/errors.md`](errors.md)). A client disconnect writes no body.
