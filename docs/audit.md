# Audit

Who did what in an org. The gateway records each audited request on an `organization` or `member` route into that org's log. The **audit** service stores it and serves it back to the org.

Boundary: [`identity-boundary.md`](identity-boundary.md). Gateway: [`gateway-contract.md`](gateway-contract.md). Route field: [`routes.md`](routes.md). Lists: [`lists.md`](lists.md).

## The model

An audit event is one request, not one change. It says who called which route, on which ids, from where, and how the request ended. A service may add what changed ([details](#details)). Nothing else.

| Piece | Says | Owner |
|-------|------|-------|
| Actor | The member whose credential was admitted, and which credential | Gateway (admission) |
| Action | Service, method, route template, path params | Gateway (route match) |
| Origin | IP, user agent | Gateway |
| Outcome | How the request ended | Gateway |
| Details | What changed | The service, on `X-Plat5-Audit-Details` |
| Store and read API | | `audit` service |

The gateway is the only process that knows the actor and the route together. Services are not told the actor, and audit does not change that. Details flow from the service to the gateway, never the other way.

Audit is on or off for the whole deployment. There is no mix.

| | Audit on (default) | Audit off (`AUDIT_ENABLED=false`) |
|---|---|---|
| Gateway `AUDIT_URL` | Required to boot | Ignored |
| `audit` service | Runs | Not run |
| Audited request | Intent written before forward. **503** if it cannot be | Forwarded like any other |
| Route `audit` fields | Applied | Accepted at apply, ignored |
| `X-Plat5-Audit-Details` | Read, stored, stripped | Stripped |
| `GET /org/audit-events` | Apply the audit catalog | Do not apply it |

The gateway logs the mode at boot. Turning audit on does not backfill. An org's log starts when audit is first on.

## Which requests

Scope decides where an event goes. It is not configurable.

| Scope | Event | Org log | Actor |
|-------|-------|---------|-------|
| `public` | Never | — | — |
| `user` | Never | — | — |
| `organization` | When audited | `subject.organization_id` | The credential's `member_id` |
| `member` | When audited | `subject.organization_id` | The credential's `member_id` (also the subject) |

Service accounts are members. Their keys are member keys. Nothing special.

A `user` route has no verified org. An `organization_id` in a user path is a param, not a subject, and routing an event by it would let any caller write into any org's log. So creating an org, redeeming an invite, and minting a session are in no org log. Everything done with the minted session is.

Method decides the default:

| Method | Default |
|--------|---------|
| `POST`, `PUT`, `PATCH`, `DELETE` | Audited |
| `GET`, `HEAD` | Not audited |
| `OPTIONS` | Never. The gateway answers it as a preflight |

The route `audit` field overrides the default ([`routes.md`](routes.md#audit)):

```yaml
- path: /org/invites
  methods:
    GET:
      audit: true      # returns live invite tokens
    POST:              # omitted: audited (a write)
- path: /org/events
  methods:
    POST:
      audit: false     # high-volume ingest
```

An event exists once the gateway knows the actor, and the request got past the rate limit:

| Gateway result | Event |
|----------------|-------|
| **401** | None. There is no trusted actor. Recording it would let anyone write into an org's log |
| **429** | None. Nothing ran. Recording it would turn the limiter into a write amplifier |
| **503**, intent not written | None, or `rejected` if the intent landed unseen ([delivery](#delivery)) |
| **403** `required_labels` | `rejected` |
| **400** / **500** upstream substitution | `rejected` |
| Forwarded | `responded` or `no_response` |

## Request order

Every route, audited or not:

1. Match → **404**
2. Authenticate → **401**
3. Rate limit → **429**
4. If audited and audit is on: write the intent → **503** if it cannot be written
5. `required_labels` → **403**
6. Substitute `upstream` → **400** / **500**
7. Forward
8. Write the outcome, in the background
9. Strip `X-Plat5-Audit-Details`, respond

The rate limit runs before `required_labels`, so a label-denied request counts against the route's limit. That bounds how many events a member without the labels can write. A route with `rate_limit: false` has no bound.

## Delivery

Two writes per audited request, both keyed by `request_id`.

**Intent.** Before forward, and the client waits on it. It carries every event field except outcome, status, and details. Each attempt times out at 500ms; up to 3 attempts within 1s. If none succeeds, the gateway answers **503** `SERVICE_UNAVAILABLE` and does not call the service. It then sends a `rejected` / `503` outcome in the background, because the intent may have landed without the gateway seeing it.

**Outcome.** After the upstream answers, or fails. The client does not wait on it. It carries outcome, status, and details. It runs in the gateway's logging phase, so an upstream failure or a client disconnect still produces one. The gateway queues it in process and retries with backoff. The queue is bounded. An outcome that overflows it is dropped, logged, and counted.

Both writes are idempotent. A retried intent does not make a second event. An outcome is applied once, `pending` → final. A later outcome for the same request changes nothing.

The guarantee: an audited request does not reach its service unless its intent is recorded. If the gateway dies, or drops the outcome, between forward and outcome, the event stays `pending`. Pending is not success, and nothing turns it into one.

The cost: one internal round trip on audited requests, before forward.

Audit is not part of the gateway's `/health/ready`. While audit is down, audited requests get **503** and every other route keeps working.

## Event

```json
{
  "id": "01JA2Z6Q3Y8D5V2K9N4R7T1W0X",
  "occurred_at": "2026-10-08T18:30:00.123Z",
  "request_id": "7d6f1c2e-4b0a-4e8e-9a51-3f2b8c1d9e04",
  "organization_id": "01J9ZX4K7M2P5Q8R1S3T6V9W0Y",
  "actor": {
    "member_id": "01J9ZX5B8C1D4E7F0G3H6J9K2M",
    "auth_type": "member_session",
    "key_prefix": "plat5-ms-1-x9Qa"
  },
  "service": "identity",
  "method": "PATCH",
  "route": "/org/members/{member_id}",
  "params": { "member_id": "01J9ZX6N2P5Q8R1S4T7V0W3X6Y" },
  "ip": "203.0.113.7",
  "user_agent": "Mozilla/5.0 …",
  "outcome": "responded",
  "status": 200,
  "details": { "role": { "from": "developer", "to": "admin" } }
}
```

| Field | |
|-------|--|
| `id` | ULID, assigned by audit when the intent is written |
| `occurred_at` | When the gateway received the request. UTC, milliseconds |
| `request_id` | `X-Request-ID`. Gateway-generated, unique per request. Joins to every service log line |
| `organization_id` | The route subject's org. Whose log this is |
| `actor.member_id` | The admitted credential's member |
| `actor.auth_type` | `member_apikey` or `member_session` |
| `actor.key_prefix` | The credential's display prefix. Same value as `key_prefix` on identity's key list, or a session's `token_prefix` ([`telemetry.md`](telemetry.md#gateway-request-line)) |
| `service` | The service name in `routes.yml` |
| `method` | HTTP method |
| `route` | The matched path template, after `route_prefix`. Not the raw path, not `upstream` |
| `params` | Path params by name, as matched. Each value is one segment. `{}` when the route has none |
| `ip` | Client IP as the gateway derives it |
| `user_agent` | `User-Agent`, truncated to 512 characters. `null` when absent |
| `outcome` | See below |
| `status` | The HTTP status the gateway answered with. `null` while `pending`, or when the client was gone before an answer |
| `details` | The service's object, or `null` |

| `outcome` | Means | The change |
|-----------|-------|------------|
| `pending` | Intent recorded, no outcome | Unknown |
| `rejected` | The gateway answered without calling the service | Did not happen |
| `responded` | The service answered. `status` is its status | Per `status` |
| `no_response` | The gateway called the service and got no answer (**502**) | Unknown |

Not in an event: request or response bodies, the query string, headers other than `User-Agent`, the actor's role or labels, a `user_id`.

## Details

`X-Plat5-Audit-Details` on the upstream response.

| | |
|--|--|
| Set by | The service, on its response |
| Format | One JSON object. Visible ASCII only: escape the rest as `\u`. At most 4096 bytes |
| Read | Only on audited routes, only from the upstream response |
| Invalid or over the cap | The event is recorded with `details: null`. The gateway logs a warning |
| Stripped | Always, on every route, audit on or off. From the response before the client, and from the request before upstream |
| Meaning | The service's. The gateway stores it as sent and never reads inside it |
| Cannot | Create an event, or change its outcome, status, actor, or org |

What goes in: what changed, with old and new values where the service has them. The old value is usually already in hand: a write reads and locks the row it changes.

```json
{ "role": { "from": "developer", "to": "admin" } }
```

A create names what it created, since the new id is not in `params`:

```json
{ "member_id": "01J9ZX7Q3R6S9T2V5W8X1Y4Z7A", "role": "developer" }
```

What stays out: secrets (never a key, token, or invite token), and anything the caller could not read from that route.

Each service documents its own shapes. Plat5 does not define a schema for them. Identity's: [`identity.md`](identity.md#audit-details).

## Reading

```
GET /organizations/{organization_id}/audit-events
```

On audit's public port. The audit catalog ([`services/audit/routes.yml`](../services/audit/routes.yml)) publishes it as `GET /org/audit-events`, `organization` scope, `required_labels: [org:audit:read]`. Like identity's catalog, the operator applies it.

| Query | |
|-------|--|
| `limit`, `starting_after` | As [`lists.md`](lists.md) |
| `actor_member_id` | The acting member |
| `service`, `method`, `outcome`, `request_id` | Exact match |
| `route` | Exact template, e.g. `/org/members/{member_id}` |
| `params[<name>]` | A path param's value, e.g. `params[member_id]=01J…`. One per name; several names AND |
| `occurred_after`, `occurred_before` | RFC 3339. After is inclusive, before is exclusive |

Filters AND. An unknown query param or a malformed value → **422** `VALIDATION_ERROR`.

```json
{
  "audit_events": [ ... ],
  "has_more": false
}
```

Order is `id` **descending**: newest first. `starting_after` is the last `id` of the previous page, and the walk continues to older events. This is the one Plat5 list that runs newest first.

Audit does not know which orgs exist. An org with no events gets an empty list, not **404**. Through the gateway the org always exists: the caller was admitted to it.

Reading is a `GET`, so it is not audited unless the catalog says `audit: true`.

## Internal API

On audit's `INTERNAL_PORT`. Not on the gateway. Optional `INTERNAL_AUTH_TOKEN` → `X-Plat5-Internal-Token`, as on identity ([`identity.md`](identity.md#internal-apis)).

### Intent

```
PUT /internal/events/{request_id}
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{
  "occurred_at": "…", "organization_id": "…",
  "actor": { "member_id": "…", "auth_type": "…", "key_prefix": "…" },
  "service": "…", "method": "…", "route": "…", "params": { … },
  "ip": "…", "user_agent": "…"
}
```

| Result | Response |
|--------|----------|
| Written | **201** |
| An event for this `request_id` exists | **200**. Left unchanged |
| Malformed, or `occurred_at` more than 24h from audit's clock | **422** |
| Postgres unavailable | **503**. The gateway retries |

Create if absent. A retry sends the same body. Unknown fields are ignored, so a newer gateway can send more.

### Outcome

```
PATCH /internal/events/{request_id}

{ "outcome": "responded", "status": 200, "details": { … } }
```

| Result | Response |
|--------|----------|
| Applied, or the event is already final | **204**. A final event is left unchanged |
| No event for this `request_id` | **404**. The gateway stops retrying |
| Malformed, or `outcome: pending` | **422** |
| Postgres unavailable | **503**. The gateway retries |

`status` may be `null` only when the client was gone before an answer, so never with `responded`. `details` may be set only with `responded`.

### Gateway env

| Variable | |
|----------|--|
| `AUDIT_ENABLED` | Default `true`. `false` turns audit off for the deployment |
| `AUDIT_URL` | Audit's internal base URL, e.g. `http://audit:3003`. Required unless audit is off |
| `INTERNAL_AUTH_TOKEN` | Same token as identity |

## Storage

`audit_events` in schema `audit` on the Plat5 Postgres. Partitioned by month on `occurred_at`. Unique on `request_id`. The audit service keeps partitions from last month to two months ahead, at boot and hourly.

Append-only, except the one `pending` → final transition. A trigger enforces it: an update may only move a pending event to its outcome, and a delete is refused. No API updates or deletes an event. Events are kept: there is no retention yet, and deleting an org does not delete its events.

## Runtime

| | |
|--|--|
| Directory | `services/audit/` |
| `service.name` | `audit` |
| `service.namespace` | `audit` |
| Public port | `3002` (`GET /organizations/{organization_id}/audit-events`) |
| Internal port | `3003` (`/health/*`, `/metrics`, `/internal/events`) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`audit`** |

Ready probe fails closed (**503** `unhealthy`) when Postgres is unreachable.

## Not here

- Request or response bodies, query strings, labels, role, or `user_id` in an event
- Events from `public` or `user` routes, or an org log chosen by a path param
- 401s or 429s in the log
- The gateway reading inside details, or details creating, moving, or re-outcoming an event
- An actor header, or any other way a service learns who called
- Changing an event after its outcome, or deleting events through an API
- Turning `pending` into anything else after the fact
- Audit for some orgs or services and not others
- Audit in Valkey or etcd
- Retention and purge (deferred, [`../TODO.md`](../TODO.md))
- `user`-route events through a service-asserted org (deferred, [`../TODO.md`](../TODO.md))
