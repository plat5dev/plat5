# Gateway contract

The Plat5 gateway (`services/gateway/`): route match, authentication, admission against a node, rate limits, audit, the label check, and forwarding with node ids and the caller's member id.

Model: [`model.md`](model.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity: [`identity.md`](identity.md). Audit: [`audit.md`](audit.md).

## Responsibilities

| Layer | Responsibility |
|-------|---------------|
| **Gateway** | Routing, authentication (JWT, API key), resolving the path's slugs to nodes, admission, rate limits, audit intent and outcome, the label check, subject fill, CORS, security headers, trace propagation |
| **Edge / load balancer** | TLS termination (e.g. Cloudflare tunnels) |
| **Identity** | Admit: the member and labels for a credential on a node chain |
| **Services** | Business logic, data access, and state rules over the node ids they're given |

Services behind the gateway **must not** validate JWTs or parse `Authorization`, and never decide access by the caller. The gateway decided.

## Pipeline

Every route, in order:

1. **Match.** Unmatched → **404**. Take the chain params from the path, root first (`{organization}`, `{project}`).
2. **Authenticate and admit.** Verify the JWT, or check the key's shape; then admit, from cache when possible ([below](#admit)). `valid: false` → **401**. Node not found, or the caller has no member in the tree → **403** "permission denied, or it may not exist", counted against the failed-auth IP limiter.
3. **Rate limit.** Each policy on the route counts in the bucket of the node at its `count_at` ([`routes.md`](routes.md#rate_limit)). Every policy must pass → **429**.
4. **Audit intent**, on an audited route with audit on, with the node chain → **503** if it can't be written ([`audit.md`](audit.md#request-order)).
5. **Labels.** The caller's labels share none with the route's (and the route isn't `[]`) → **403** "permission denied, or it may not exist", with `required_labels` in `details`. `*` matches any label.
6. **Fill and forward.** Substitute `{subject.<kind>_id}`, `{subject.member_id}` and `{path.*}` in `upstream` (a bad path param → **400**, a bad subject id → **500**), set the subject headers, forward. Slugs never reach a service.
7. **Outcome**, in the background, then respond.

Both **403**s have the same body, `details.required_labels` included, so neither says whether the node exists. A node you can't use and a node that doesn't exist answer the same, as Google Cloud's "Permission denied on resource (or it may not exist)". Step 2's **403** runs before the rate limit so a caller outside a tree can't spend that tree's budget; step 5's runs after, so a member probing labels is bounded by its own tree's limit.

On a `public` route, step 2 is skipped and there is no subject. On a sign-in route (`scope: directory`), the chain is empty and the subject is the person's directory member.

## Credentials

`Authorization: Bearer` is the IdP JWT only. Plat5 API keys are on `X-API-Key`. If `X-API-Key` is present, it is the credential; don't fall through to the other header.

Any credential works on any route scope: a person's JWT or personal key reaches every tree they're in, and admit picks the member for the tree in the path. A service account key works only in its own tree, and never on sign-in routes.

**JWT.** Verified against JWKS ([`idp-contract.md`](idp-contract.md)). The gateway reads the issuer, the subject claim (`AUTH_SUBJECT_CLAIM`) and the name claim (the sign-in kind's `name_claim`), and sends them to admit as `idp`.

**`X-API-Key`.** The gateway checks the shape first: `{brand}-k-2-{secret}`, with a brand of `[a-z][a-z0-9]*` up to 32 characters and a 43-character base64url secret. Any other shape is **401** without an identity call, and counts against the failed-auth limiter. The gateway doesn't know the brand.

## Admit

```
POST {ADMIT_URL}
{ "credential" | "idp", "scope", "chain": [{ "kind", "ref" }], "resolve_deleted" }
→ { "valid", "found", "principal", "nodes": [{ "kind", "id" }], "member_id", "labels", "good_until" }
```

On identity's `INTERNAL_PORT`, required to boot. Optional `INTERNAL_AUTH_TOKEN` is sent as `X-Plat5-Internal-Token`. Contract: [`identity.md`](identity.md#admit).

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

What the TTL bounds: a removed binding, a role change, a deleted node, a suspended person or a revoked key reaches the edge within the TTL. An expiring binding is exact, through `good_until`. Raw keys and JWTs are hashed before use as cache keys. `APIKEY_CACHE_TTL_SECS` covers every admit; don't rename it.

## Headers to services

The same subject the path rewrite uses, so a service can read it either way.

| Header | Value |
|--------|-------|
| `X-Plat5-<Kind>-Id` (e.g. `X-Plat5-Organization-Id`, `X-Plat5-Project-Id`) | One per node in the chain |
| `X-Plat5-Member-Id` | The caller's member id in this tree; on a sign-in route, their directory member |
| `X-Plat5-Principal` | `person` or `service_account` |
| `X-Request-ID` | Correlation id, gateway-generated; also on the response |
| `traceparent` | W3C trace context |

`<Kind>` is the kind name in title case, with `_` becoming `-`: `project` → `Project`, `cost_center` → `Cost-Center`.

Never sent: a person's principal id, the caller's labels or role.

**Recommended use:** store the member id as an owner or author when a feature needs one; never decide access by the caller ([`model.md`](model.md#where-a-field-goes)).

### Stripped

| Header | Direction | Why |
|--------|-----------|-----|
| Every `X-Plat5-*` | Request, before the subject headers are set | A client can't name a node or a member |
| `Authorization`, `X-API-Key` | Request | Consumed by the gateway; never leaked to services |
| `X-Plat5-Audit-*` | Response | What the service tells audit ([`audit.md`](audit.md#what-a-service-adds)). Stripped on every route, audit on or off |

Clients still send credential headers **to the gateway**, and CORS may allow browsers to send them.

## Rules for services behind the gateway

The headers and rewritten path are authentic only if the service is on a private network that nothing but its front doors can reach.

1. **Trust the path and headers the front door wrote.** Read node ids from the path or `X-Plat5-<Kind>-Id`, the caller from `X-Plat5-Member-Id`.
2. **Document which headers you require.** A service that records authors says it needs `X-Plat5-Member-Id`, and answers **400** without it. Any front door that calls it supplies one.
3. **Never decide access by the caller.** Scope every lookup by the node id; enforce state rules. The front door already decided who may call.
4. **Propagate `traceparent`** on downstream calls.
5. **Log with `request_id`** from `X-Request-ID`. Don't set `X-Request-ID` on responses.
6. **Say what changed** with `X-Plat5-Audit-Details` on audited routes, and `X-Plat5-Audit-Created` on a create. No secrets ([`audit.md`](audit.md#what-a-service-adds)).

### Platform integrity

1. **Do not parse `Authorization`** or read `X-API-Key`. You won't receive them.
2. **Do not validate JWTs.** The gateway validates via JWKS.
3. **Do not implement CORS.** The gateway handles preflight and headers.
4. **Do not generate `X-Request-ID`.** Propagate it. Missing is a gateway bug.

## Other front doors

Services do no auth, so anything on the private network that speaks a service's API can call it: an operator plane, an internal job, a custom gateway. Each is its own product with its own docs and rules. Base Plat5 defines no operator headers, roles or catalog rules.

What a service can rely on is its own API: paths with node ids, and the headers it documents as required. A front door that calls a service needing `X-Plat5-Member-Id` supplies one, or the service refuses the request as malformed.

## Control plane

Routes, kinds and CORS origins come from the control plane: `CONTROL_PLANE_URL` (e.g. `http://control-plane:5003`), required to boot. The gateway reads `GET {CONTROL_PLANE_URL}/internal/gateway` with the same `INTERNAL_AUTH_TOKEN`.

- **Boot.** Waits for its first snapshot, retrying while the control plane is down, and doesn't listen until it has one.
- **Running.** Long-polls with `?after={revision}` and swaps routes, kinds and CORS whole on each change.
- **Control plane down.** Keeps serving the last snapshot.
- **A snapshot it can't validate.** Keeps the current one and logs an error.

Contract: [`control-plane.md`](control-plane.md#internal-feeds).

## Rate limits

Counters live in **Valkey**; replicas share one budget. `VALKEY_URL` is required to boot (`/health/ready` is 503 until Valkey answers PING). A command or connect that doesn't finish within 500ms is a Valkey error: limited requests get **503**. Schema and buckets: [`routes.md`](routes.md#rate_limit).

| | |
|--|--|
| Fallback | `RATE_LIMIT_REQUESTS` (default 60; `0` = unlimited), `RATE_LIMIT_WINDOW_SECONDS` (default 60), counted at the route's own node |
| Exceed | **429** `RATE_LIMITED`, `details.retry_after_seconds`, `Retry-After` |
| Admitted headers | On limited routes (2xx and 429): `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, for the tightest policy |
| Failed-auth IP | `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). Unadmitted 401s, step 2's 403s, and unmatched 404s. **429** sets `Retry-After`; no `X-RateLimit-*` |

## Audit

On an audited route other than `public`, the gateway writes an intent before forward and an outcome after. Intent not written → **503**, and the service isn't called. `AUDIT_URL` is required to boot unless `AUDIT_ENABLED=false`. Contract: [`audit.md`](audit.md).

## Direct-exposed services

Some services are exposed outside the gateway on purpose (e.g. an IdP at `auth.example.com` while the gateway serves `api.example.com`). They handle their own CORS, don't rely on subject headers, and are exempt from "Do not implement CORS". TLS still typically terminates at the edge.

## Client IP

What an audit event records as `ip`, and what `public` routes and the failed-auth limiter key on.

The TCP peer, unless the peer is in `TRUSTED_PROXIES` (comma-separated addresses and CIDRs). Then `X-Forwarded-For` is walked from the right, past trusted proxies, and the first address that isn't one is the client. Nothing left of an untrusted hop is read. An entry that isn't an address stops the walk at the last trusted proxy. No other header names the client.

`TRUSTED_PROXIES` empty (the default): `X-Forwarded-For` is ignored. Behind a tunnel or load balancer, list it, or every request has the edge's IP.

## TLS termination

TLS terminates at the edge or load balancer, not the gateway. Services behind the gateway receive plaintext HTTP and must not terminate TLS.

## CORS

The gateway answers every `OPTIONS` request as a preflight: `204 No Content`, `Access-Control-Allow-*` and `Access-Control-Max-Age: 86400`. It adds `Access-Control-Allow-*` on responses. Services behind the gateway have no CORS config.

The allowed origins are the control plane's `cors` document ([`control-plane.md`](control-plane.md#cors)). No document, or an empty list, answers `Access-Control-Allow-Origin: *`. Otherwise an `Origin` in the list is echoed with `Vary: Origin`, and any other origin gets no `Access-Control-Allow-Origin`.

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

## Environment

| Variable | |
|----------|--|
| `CONTROL_PLANE_URL` | Required |
| `ADMIT_URL` | Required. e.g. `http://identity:3001/internal/admit` |
| `INTERNAL_AUTH_TOKEN` | Sent to identity, audit and the control plane |
| `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`, `AUTH_SUBJECT_CLAIM` | [`idp-contract.md`](idp-contract.md) |
| `APIKEY_CACHE_TTL_SECS` | Default 300 |
| `VALKEY_URL` | Required |
| `RATE_LIMIT_*` | [Rate limits](#rate-limits) |
| `AUDIT_ENABLED`, `AUDIT_URL` | [`audit.md`](audit.md) |
| `TRUSTED_PROXIES` | [Client IP](#client-ip) |
| `UPSTREAM_CONNECT_TIMEOUT_MS`, `UPSTREAM_READ_TIMEOUT_MS` | Defaults for services without `timeouts` |

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

All use the Plat5 envelope ([`api-errors.md`](api-errors.md)). A client disconnect writes no body.
