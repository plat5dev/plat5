# Audit

Who did what. Each root owns a log: every org has one, and the sign-in node's log holds each person's own. The gateway records each audited request into the log of the root whose tree it touched, with the chain of nodes it addressed. The **audit** service stores it and serves it back by node.

Gateway: [`gateway-contract.md`](gateway-contract.md). Route field: [`routes.md`](routes.md#audit). Lists: [`lists.md`](lists.md).

## The model

An audit event is one request, not one change. It says who called which route, on which nodes and ids, from where, and how the request ended. A service may add what changed ([details](#details)). Nothing else.

| Piece | Says | Owner |
|-------|------|-------|
| Actor | The member whose credential was admitted, and which credential | Gateway (admission) |
| Nodes | The chain the route addressed, root first | Gateway (admission) |
| Action | Service, method, route template, path params | Gateway (route match) |
| Origin | IP, user agent | Gateway |
| Outcome | How the request ended | Gateway |
| Details | What changed | The service, on `X-Plat5-Audit-Details` |
| Created | The id a create produced | The service, on `X-Plat5-Audit-Created` |
| Store and read API | | `audit` service |

Audit is on or off for the whole deployment. There is no mix.

| | Audit on (default) | Audit off (`AUDIT_ENABLED=false`) |
|---|---|---|
| Gateway `AUDIT_URL` | Required to boot | Ignored |
| `audit` service | Runs | Not run |
| Audited request | Intent written before forward. **503** if it can't be | Forwarded like any other |
| Route `audit` fields | Applied | Accepted at apply, ignored |
| `X-Plat5-Audit-*` | Read, stored, stripped | Stripped |

The gateway logs the mode at boot. Turning audit on doesn't backfill.

Audit is for reporting and compliance. A product feature never reads it: audit can be off, it's behind a label, and it may get retention. A feature that needs to know who did something records the member id itself ([`model.md`](model.md#where-a-field-goes)).

## Which log

| Scope | Event | Log |
|-------|-------|-----|
| `public` | Never | — |
| A kind | When audited | The root of the chain: the org |
| The sign-in kind | When audited | The person's own, keyed by their directory member. Also the tree the service names, on a success |

**The person's log** holds what they did to their own account: personal keys, account deletion, invite redeems. Only the person and other front doors read it.

**A sign-in route that changes a tree** (creating an org, redeeming an invite) is in that tree's log too. The tree isn't taken from the path: the service names the root and the member on `X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` ([below](#root-and-member)). The service is trusted; the caller is not.

Method decides the default: `POST`, `PUT`, `PATCH`, `DELETE` audited; `GET`, `HEAD` not; `OPTIONS` never. The route `audit` field overrides it.

An event exists once the gateway knows the actor and the node, and the request got past the rate limit:

| Gateway result | Event |
|----------------|-------|
| **401** | None. There's no trusted actor |
| **403** before the rate limit (node not found, not in the tree) | None. There's no tree to log into |
| **429** | None. Nothing ran |
| **503**, intent not written | None, or `rejected` if the intent landed unseen ([delivery](#delivery)) |
| **403** label check | `rejected` |
| **400** / **500** upstream substitution | `rejected` |
| Forwarded | `responded` or `no_response` |

## Request order

1. Match → **404**
2. Authenticate and admit → **401** / **403**
3. Rate limit → **429**
4. If audited and audit is on: write the intent → **503** if it can't be written
5. Labels → **403**
6. Substitute `upstream` → **400** / **500**
7. Forward
8. Write the outcome, in the background
9. Strip `X-Plat5-Audit-*`, respond

The rate limit runs before the label check, so a label-denied request counts against the route's limit. That bounds how many events a member without the labels can write.

## Delivery

Two writes per audited request, both keyed by `request_id`.

**Intent.** Before forward, and the client waits on it. It carries every event field except outcome, status, and details. Each attempt times out at 500ms; up to 3 attempts within 1s. If none succeeds, the gateway answers **503** and doesn't call the service. It then sends a `rejected` / `503` outcome in the background, because the intent may have landed without the gateway seeing it.

**Outcome.** After the upstream answers, or fails. The client doesn't wait on it. It runs in the gateway's logging phase, so an upstream failure or a client disconnect still produces one. The gateway queues it in process and retries with backoff. The queue is bounded; an outcome that overflows it is dropped, logged and counted.

Both writes are idempotent. An outcome is applied once, `pending` → final.

The guarantee: an audited request doesn't reach its service unless its intent is recorded. If the gateway dies or drops the outcome between forward and outcome, the event stays `pending`. Pending is not success, and nothing turns it into one.

## Event

```json
{
  "id": "01JA2Z6Q3Y8D5V2K9N4R7T1W0X",
  "occurred_at": "2026-10-08T18:30:00.123Z",
  "request_id": "7d6f1c2e-4b0a-4e8e-9a51-3f2b8c1d9e04",
  "root_id": "n_01J9ZX4K7M2P5Q8R1S3T6V9W0Y",
  "nodes": [ { "kind": "organization", "id": "n_01J9ZX4K…" }, { "kind": "project", "id": "n_01J9ZX5A…" } ],
  "actor": { "member_id": "m_01J9ZX5B…", "principal": "service_account", "auth_type": "apikey", "key_prefix": "e10s-k-2-x9Qa" },
  "service": "e10s",
  "method": "POST",
  "route": "/orgs/{organization}/projects/{project}/plans/{key}/versions",
  "params": { "key": "pro" },
  "ip": "203.0.113.7",
  "user_agent": "e10s-cli/1.4",
  "outcome": "responded",
  "status": 201,
  "details": { "version": 7 },
  "created_id": "ver_01JA2Z6R…"
}
```

| Field | |
|-------|--|
| `id` | ULID, assigned by audit when the intent is written |
| `occurred_at` | When the gateway received the request. UTC, milliseconds |
| `request_id` | `X-Request-ID`. Joins to every service log line |
| `root_id` | The root whose log this is in. On a sign-in route, the sign-in node |
| `nodes` | The route's chain, root first, as ids. `[]` on a sign-in route |
| `named_root_id`, `named_member_id` | On a sign-in route, the tree and member the service named. `null` otherwise |
| `actor.member_id` | The caller's member in this tree; on a sign-in route, their directory member |
| `actor.principal` | `person` or `service_account` |
| `actor.auth_type` | `jwt` or `apikey` |
| `actor.key_prefix` | The key's display prefix. `null` for a JWT |
| `service`, `method` | From the route |
| `route` | The matched path template, after `route_prefix`. Chain params stay as `{organization}`; their values are in `nodes`, never as slugs |
| `params` | Non-chain path params by name, as matched |
| `ip` | Client IP ([`gateway-contract.md`](gateway-contract.md#client-ip)) |
| `user_agent` | Truncated to 512 characters. `null` when absent |
| `outcome` | `pending`, `rejected`, `responded`, `no_response` |
| `status` | The status the gateway answered. `null` while `pending`, or when the client was gone |
| `details` | The service's object, or `null` |
| `created_id` | From `X-Plat5-Audit-Created`, or `null` |

Not in an event: bodies, the query string, headers other than `User-Agent`, the actor's role or labels, slugs, or a principal id.

## What a service adds

Headers on the upstream response. The gateway reads them only on audited routes, and strips them always, on every route.

| Header | Says | Read on |
|--------|------|---------|
| `X-Plat5-Audit-Details` | What changed | Any audited route |
| `X-Plat5-Audit-Created` | The id a create produced | Any audited route |
| `X-Plat5-Audit-Root`, `X-Plat5-Audit-Member` | The root of the tree a sign-in route changed, and the member in it | Audited sign-in routes, 2xx responses, both or neither |

None of them can create an event, or change its outcome, status, or who acted.

### Details

| | |
|--|--|
| Format | One JSON object. Visible ASCII only: escape the rest as `\u`. At most 4096 bytes |
| Invalid or over the cap | The event is recorded with `details: null`; the gateway logs a warning |
| Meaning | The service's. The gateway stores it as sent and never reads inside it |

What goes in: what changed, with old and new values where the service has them (`{ "role": { "from": "developer", "to": "admin" } }`). A create names what it created. What stays out: secrets, and anything the caller couldn't read from that route. Each service documents its own shapes; identity's are in [`identity.md`](identity.md#audit-details).

### Created

One id: visible ASCII, one path segment, at most 128 bytes. Invalid → ignored, with a warning.

### Root and member

`X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` are ids, with the same rules as `Created`. On a 2xx to an audited sign-in route, they become `named_root_id` and `named_member_id`, and the event is in that root's log too: a read of the root returns it, with the named member as the actor there. Anywhere else, or one without the other, they're ignored, with a warning.

## Reading

```
GET /nodes/{node_id}/audit-events
GET /members/{member_id}/audit-events
```

On audit's public port. The first returns events on the node and everything below it: any event whose `nodes` contains it, or whose `named_root_id` is it. The second is a person's own log, for a directory member.

The generated catalog publishes them per kind: `GET /orgs/{organization}/audit-events` with `organization:audit:read`, `GET /orgs/{organization}/projects/{project}/audit-events` with `project:audit:read`, and `GET /me/audit-events` on the sign-in kind ([`identity.md`](identity.md#generated-routes)). Reading a project's history takes a role at the project, or at the org.

| Query | |
|-------|--|
| `limit`, `starting_after` | As [`lists.md`](lists.md) |
| `actor_member_id` | The acting member |
| `service`, `method`, `outcome`, `request_id`, `created_id` | Exact match |
| `route` | Exact template |
| `params[<name>]` | A non-chain param's value. Several names AND |
| `occurred_after`, `occurred_before` | RFC 3339. After is inclusive, before is exclusive |

Filters AND. An unknown query param or a malformed value is **422**.

```json
{ "audit_events": [ ... ], "has_more": false }
```

Order is `id` **descending**: newest first. This is the one Plat5 list that runs newest first.

Audit doesn't know which nodes exist. A node with no events is an empty list, not **404**.

## Internal API

On audit's `INTERNAL_PORT`. Optional `INTERNAL_AUTH_TOKEN` → `X-Plat5-Internal-Token`.

### Intent

```
PUT /internal/events/{request_id}
{
  "occurred_at": "…", "root_id": "…", "nodes": [ … ],
  "actor": { "member_id": "…", "principal": "…", "auth_type": "…", "key_prefix": "…" },
  "service": "…", "method": "…", "route": "…", "params": { … },
  "ip": "…", "user_agent": "…"
}
```

| Result | Response |
|--------|----------|
| Written | **201** |
| An event for this `request_id` exists | **200**, unchanged |
| Malformed, or `occurred_at` more than 24h from audit's clock | **422** |
| Postgres unavailable | **503**. The gateway retries |

Unknown fields are ignored, so a newer gateway can send more.

### Outcome

```
PATCH /internal/events/{request_id}
{ "outcome": "responded", "status": 200, "details": { … }, "created_id": "…", "root_id": "…", "member_id": "…" }
```

`root_id` and `member_id` only for a sign-in-route event, from the service's pair.

| Result | Response |
|--------|----------|
| Applied, or the event is already final | **204** |
| No event for this `request_id` | **404**. The gateway stops retrying |
| Malformed, or `outcome: pending` | **422** |
| Postgres unavailable | **503**. The gateway retries |

## Storage

`audit_events` in schema `audit` on the Plat5 Postgres. Partitioned by month on `occurred_at`. Unique on `request_id`; a GIN index on the node ids for reads by node. Partitions are kept from last month to two months ahead, at boot and hourly.

Append-only, except the one `pending` → final transition. A trigger enforces it. No API updates or deletes an event. Events are kept: there's no retention yet, and purging a node doesn't delete its events.

## Runtime

| | |
|--|--|
| Directory | `services/audit/` |
| `service.name` / `service.namespace` | `audit` / `audit` |
| Public port | `3002` |
| Internal port | `3003` (`/health/*`, `/metrics`, `/internal/events`) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`audit`** |

## Not here

- Bodies, query strings, labels, roles, slugs or principal ids in an event
- Events from `public` routes, or a log chosen by a path param
- 401s, 429s, or not-found 403s in the log
- The gateway reading inside details, or a service header creating or re-outcoming an event, or changing who acted
- A product feature that reads audit
- Changing an event after its outcome, or deleting events through an API
- Audit for some trees or services and not others
- Retention and purge (deferred, [`../TODO.md`](../TODO.md))
- Other front doors' events. Each keeps its own log
