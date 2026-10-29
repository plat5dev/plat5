# Gateway

The Plat5 gateway (`services/gateway/`). How it decides a request: [access](../concepts/access.md#how-a-request-is-decided). What services receive: [`reference/headers.md`](headers.md). Environment: [`reference/env.md`](env.md#gateway).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT, API key), resolving the path's slugs to nodes, admission, rate limits, the permission check, upstream fill, audit intent and outcome, route checks, CORS, security headers, body limits, trace propagation |
| **Edge / load balancer** | TLS termination |
| **Identity** | Admit: the member and permissions for a credential on a node chain |
| **Services** | Business logic, data access, and state rules over the node ids they're given |

Services behind the gateway **must not** validate JWTs or parse `Authorization`, and never decide access by the caller. The gateway decided.

## Credentials

Every credential is `Authorization: Bearer`. Three dot-separated segments is a JWT, verified locally against JWKS; a JWT that fails is **401** without an identity call. Anything else is sent to admit as a key, as presented: the gateway doesn't know the key format.

## Admit

```
POST {IDENTITY_INTERNAL_URL}/internal/admit
{ "credential" | "idp", "chain": [{ "kind", "ref" }], "allow_deleted" }
→ { "valid", "principal", "key_id", "nodes": [{ "kind", "id" }], "member_id", "user_id", "permissions" }
```

On identity's `INTERNAL_PORT`. An empty chain is a user route. Contract: [`reference/api/internal.md`](api/internal.md#admit).

What the gateway answers for each result: [admit](api/internal.md#what-the-gateway-answers).

### Cache

In-process per replica.

| | |
|--|--|
| Key | The credential (a key's hash, or a verified JWT's issuer and subject), `allow_deleted`, and the chain refs as written in the path |
| TTL | `ADMIT_CACHE_TTL` (default `5m`) |
| Negative entries | For invalid credentials and `member_id: null`, for `ADMIT_NEGATIVE_CACHE_TTL` (default `10s`). Never for identity errors |
| Singleflight | Concurrent misses for one key share a single identity call |
| JWT verification | Each request; the token's `exp` is checked every time |

A refreshed JWT has the same issuer and subject, so it's no miss. What the TTL bounds: [access](../concepts/access.md#how-fast-changes-reach-the-edge).

## Control plane

Routes, kinds, rate limits, CORS, the name claims and the route checks path come from the control plane: `CONTROL_PLANE_URL` (e.g. `http://control-plane:5003`), required to boot. The gateway reads `GET {CONTROL_PLANE_URL}/internal/gateway`.

- **Boot.** Waits for its first snapshot, retrying while the control plane is down, and doesn't listen until it has one.
- **Running.** Long-polls with `?after={revision}` and swaps them whole on each change.
- **Control plane down.** Keeps serving the last snapshot.
- **A snapshot it can't build.** The control plane validated it; the gateway only parses it and builds its route table. A snapshot it can't parse or build is refused whole: it keeps the current one, logs an error, counts it, and reports the revision and the reason on its next poll.
- **Reporting.** Every long poll carries the replica's id and the revision it loaded ([feeds](api/internal.md#control-plane-feeds)).

| Metric | Type | Labels |
|--------|------|--------|
| `gateway_config_revision` | Gauge | The revision this replica serves |
| `gateway_config_refused_total` | Counter | `revision`: each snapshot it refused |

## Rate limits

Counters live in **Valkey**; replicas share one budget. `VALKEY_URL` is required to boot (`/health/ready` is 503 until Valkey answers PING). A command or connect that doesn't finish within 500ms is a Valkey error: limited requests get **503**. Policies and buckets: [rate limits](config/rate-limits.md).

| | |
|--|--|
| Check | Every policy on the route in one round trip, counted only if all pass |
| Exceed | **429** `RATE_LIMITED`, `details.retry_after_seconds`, `Retry-After` |
| Admitted headers | On limited routes (2xx and 429): `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, for the tightest policy |
| Keys | Under a fixed `plat5:rl:` prefix |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60; `0` = off). Counts only what identity refused: `valid: false` and `member_id: null`. A JWT refused locally and an unmatched route don't count. **429** sets `Retry-After`; no `X-RateLimit-*` |

## Audit

On an audited route other than `public`, the gateway writes an intent after the permission check and before forward, and an outcome after. Intent not written → **503**, and the service isn't called. Its own refusals after admit are one background event. `AUDIT_URL` is required to boot unless `AUDIT_ENABLED=false`. Contract: [`concepts/audit.md`](../concepts/audit.md).

## Client IP

What an audit event records as `ip`, and what `public` routes and the failed-auth limiter key on.

The TCP peer, unless the peer is in `TRUSTED_PROXIES` (comma-separated addresses and CIDRs). Then `X-Forwarded-For` is walked from the right, past trusted proxies, and the first address that isn't one is the client. Nothing left of an untrusted hop is read. An entry that isn't an address stops the walk at the last trusted proxy. No other header names the client.

`TRUSTED_PROXIES` empty (the default): `X-Forwarded-For` is ignored. Behind a tunnel or load balancer, list it, or every request has the edge's IP.

## Paths

A param value of `.` or `..`, or one holding `/`, `?` or `#` once decoded, is **400**: it would change the path the upstream sees. The query string is forwarded as sent.

## Request bodies

A body larger than the service's `max_body_bytes` (or `defaults.max_body_bytes`, [settings](config/settings.md)) is **413** `PAYLOAD_TOO_LARGE`, checked on `Content-Length` and while streaming.

## Route checks

Whether the caller may call each of a batch of routes, for a UI that hides what the caller can't use ([access](../concepts/access.md#what-a-ui-shows)). The gateway answers it itself; it isn't a service route. It's served at `route_checks.path` in `plat5.yml`, `/access/routes` by default ([settings](config/settings.md)), and no route may match that path.

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
| `method` | `GET`, `POST`, `PUT`, `PATCH`, `DELETE` or `HEAD`: the verbs a route takes, uppercase |
| `path` | A public path, as a request would send it: starts with `/`, visible ASCII, no `?` or `#` (no query string or fragment), at most 2048 characters |

Other fields are ignored. Each `allowed` is what the gateway would decide for this caller, this method and this path, up to the permission check ([how a request is decided](../concepts/access.md#how-a-request-is-decided)):

| The route | `allowed` |
|-----------|-----------|
| None matches the path and method | `false` |
| `public` | `true` |
| `user` | `true` for a person, `false` for a service account |
| A kind | `true` when admit finds the caller's member on the chain (a deleted node only on an `allow_deleted` route) and the caller's permissions there meet `required_permissions`, or the route is `any_member` |

A node that doesn't exist and a node the caller can't reach are both `false`, as both are the same **403** on the request. Rate limits, body limits, the param check of the fill and the upstream aren't evaluated, so `true` doesn't promise the request succeeds. Each admit comes from the [cache](#cache), as a request's would.

- **Credential.** Any credential a route takes. Missing, malformed, or a JWT refused locally: **401**. When admit refuses the credential itself on a check (a revoked key, a suspended person, a service account whose node is deleted), the whole call is **401**, counted once by the failed-auth limiter. A key refused on a `user` check is that check's `false` instead: identity answers a service account's key there the same way. A `false` is never counted by the failed-auth limiter.
- **Rate limit.** One count of the `default` policy per call, per credential (a key, or a JWT's issuer and subject), before any check. `X-RateLimit-*` on the **200** and the **429**, as on a route.
- **Not audited**, and nothing is forwarded.
- **CORS** as on any response; its preflight is any `OPTIONS`.
- **Other methods** on the path match no route: **404**.

| Case | Code |
|------|------|
| A body that isn't JSON, `checks` missing or outside 1 to 100, a check that isn't an object, a bad `method` or `path` | `VALIDATION_ERROR` (422). `details.fields` names each, as `checks[2].path` |
| A body over 256 KiB | `PAYLOAD_TOO_LARGE` (413) |

The other errors are the [gateway's](#errors).

## TLS termination

TLS terminates at the edge or load balancer, not the gateway. Services behind the gateway receive plaintext HTTP and must not terminate TLS.

## CORS

The gateway answers every `OPTIONS` request as a preflight: `204 No Content`, `Access-Control-Allow-*` and `Access-Control-Max-Age: 86400`. It adds `Access-Control-Allow-Origin` and `Access-Control-Expose-Headers` on responses. Services behind the gateway have no CORS config.

| Header | Value |
|--------|-------|
| `Access-Control-Allow-Origin` | With `allowed_origins: ["*"]`, `*`. Otherwise an `Origin` in the list, echoed, with `Vary: Origin`; any other origin, or an empty list, or no `cors` block, gets none, so no cross-origin request is allowed |
| `Access-Control-Allow-Methods` | `GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS` |
| `Access-Control-Allow-Headers` | `Authorization`, `Content-Type`, `X-Request-ID`, `traceparent`, `tracestate`, plus `cors.allowed_headers` |
| `Access-Control-Expose-Headers` | `X-Request-ID`, `Retry-After`, `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` |

The `cors` document: [cors](config/cors.md).

## Security headers

On public proxy responses, errors included (not internal `/health` / `/metrics`), each only when the upstream response hasn't set it:

| Header | Value |
|--------|--------|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |

## JWKS

`AUTH_JWKS_URI` is required to boot. An empty cache retries every 2s until loaded; a loaded cache refreshes every 15 minutes. `AUTH_ALLOWED_AUDIENCES` is required: a token's `aud` must hold one of them, or the list is `*`, which accepts any audience ([`reference/idp.md`](idp.md)).

## Boot and ready

The gateway listens only once it has a control-plane snapshot. `/health/ready` is **200** when JWKS is loaded **and** Valkey answers PING within 500ms, otherwise **503**. The control plane isn't part of ready: while it's down, the gateway serves its last snapshot. Audit isn't part of ready: while it's down, audited routes are **503** and the rest keep working.

## Errors

| Case | Code |
|------|------|
| Bad or missing credential | `UNAUTHORIZED` (401) |
| Node not found, not in the tree, or no route permission held | `FORBIDDEN` (403), "permission denied, or it may not exist" |
| Route not registered | `NOT_FOUND` (404) |
| Request body too large | `PAYLOAD_TOO_LARGE` (413) |
| A [route checks](#route-checks) body that doesn't follow its rules | `VALIDATION_ERROR` (422) |
| Rate limit (admitted route or failed-auth IP) | `RATE_LIMITED` (429) |
| JWKS, Valkey or identity down | `SERVICE_UNAVAILABLE` (503) |
| Audit intent not written | `SERVICE_UNAVAILABLE` (503) |
| Upstream unreachable, timed out, or failing mid-proxy | **502** with the `SERVICE_UNAVAILABLE` code |
| A path param of `.` or `..`, or holding `/`, `?` or `#` | `INVALID_REQUEST` (400) |

All use the Plat5 envelope ([`reference/errors.md`](errors.md)). A client disconnect writes no body.
