# Audit

Who did what. Each root owns a log: every org has one. Each person also has their own, keyed by their directory member (`actor.member_id`), not by a node. The gateway records each audited request into the log of the root whose tree it touched, with the chain of nodes it addressed. The **audit** service stores it and serves it back by node.

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

Audit is for reporting and compliance. A product feature never reads it: audit can be off, it's behind a label, and it may get retention. A feature that needs to know who did something records the member id itself ([`concepts/model.md`](model.md#where-a-field-goes)).

## Which log

| Scope | Event | Log |
|-------|-------|-----|
| `public` | Never | — |
| A kind | When audited | The root of the chain: the org |
| The sign-in kind | When audited | The person's own, keyed by their directory member (`actor.member_id`; the event has no `root_id`). Also the tree the service names, on a success |

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

Audit runs inside the gateway's pipeline ([access](access.md#how-a-request-is-decided)): the intent is written after the rate limit and before the label check, and the outcome after the service answers. The rate limit running before the label check bounds how many events a member without the labels can write.

## Delivery

Two writes per audited request, both keyed by `request_id`.

**Intent.** Before forward, and the client waits on it. It carries every event field except outcome, status, and details. Each attempt times out at 500ms; up to 3 attempts within 1s. If none succeeds, the gateway answers **503** and doesn't call the service. It then sends a `rejected` / `503` outcome in the background, because the intent may have landed without the gateway seeing it.

**Outcome.** After the upstream answers, or fails. The client doesn't wait on it. It runs in the gateway's logging phase, so an upstream failure or a client disconnect still produces one. The gateway queues it in process and retries with backoff. The queue is bounded; an outcome that overflows it is dropped, logged and counted.

Both writes are idempotent. An outcome is applied once, `pending` → final.

The guarantee: an audited request doesn't reach its service unless its intent is recorded. If the gateway dies or drops the outcome between forward and outcome, the event stays `pending`. Pending is not success, and nothing turns it into one.

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

What goes in: what changed, with old and new values where the service has them (`{ "role": { "from": "developer", "to": "admin" } }`). A create names what it created. What stays out: secrets, and anything the caller couldn't read from that route. Each service documents its own shapes; identity's are in [`reference/api/identity.md`](../reference/api/identity.md#audit-details).

### Created

One id: visible ASCII, one path segment, at most 128 bytes. Invalid → ignored, with a warning.

### Root and member

`X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` are ids, with the same rules as `Created`. On a 2xx to an audited sign-in route, they become `named_root_id` and `named_member_id`, and the event is in that root's log too: a read of the root returns it, with the named member as the actor there. Anywhere else, or one without the other, they're ignored, with a warning.

The event's fields and the read API: [`reference/api/audit.md`](../reference/api/audit.md). Storage: [`internals/audit-storage.md`](../internals/audit-storage.md).

## Not here

- Bodies, query strings, labels, roles, slugs or principal ids in an event
- Events from `public` routes, or a log chosen by a path param
- 401s, 429s, or not-found 403s in the log
- The gateway reading inside details, or a service header creating or re-outcoming an event, or changing who acted
- A product feature that reads audit
- Changing an event after its outcome, or deleting events through an API
- Audit for some trees or services and not others
- Retention and purge (deferred, [`TODO.md`](../../TODO.md))
- Other front doors' events. Each keeps its own log
