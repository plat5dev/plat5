# Audit API

The event and the read API. Concept: [audit](../../concepts/audit.md).

## Event

```json
{
  "id": "01ja2z6q3y8d5v2k9n4r7t1w0x",
  "occurred_at": "2026-10-08T18:30:00.123Z",
  "request_id": "7d6f1c2e-4b0a-4e8e-9a51-3f2b8c1d9e04",
  "root_id": "01j9zx4k7m2p5q8r1s3t6v9w0y",
  "nodes": [ { "kind": "organization", "id": "01j9zx4k…" }, { "kind": "project", "id": "01j9zx5a…" } ],
  "actor": { "member_id": "01j9zx5b…", "user_id": null, "principal": "service_account", "auth_type": "apikey", "key_id": "01j9zx5c…" },
  "service": "e10s",
  "method": "POST",
  "route": "/orgs/{organization}/projects/{project}/plans/{plan_id}/versions",
  "params": { "plan_id": "pro" },
  "ip": "203.0.113.7",
  "user_agent": "e10s-cli/1.4",
  "outcome": "responded",
  "status": 201,
  "details": { "version": 7 },
  "created_id": "ver_01ja2z6r…",
  "named_nodes": [],
  "named_member_id": null
}
```

| Field | |
|-------|--|
| `id` | Lowercase ULID, assigned by audit when the intent is written |
| `occurred_at` | When the gateway received the request. UTC, milliseconds |
| `request_id` | `X-Request-ID`. Joins to every service log line |
| `root_id` | The chain's root: `nodes[0].id`. `null` on a user route |
| `nodes` | The route's chain, root first, as ids. `[]` on a user route |
| `named_nodes`, `named_member_id` | On a user route, the chain and member the service named. `[]` and `null` otherwise |
| `actor.member_id` | The caller's member in this tree. `null` on a user route, except in a named node's read ([reading](#reading)) |
| `actor.user_id` | The caller's user id on a user route, in the person's own log only. `null` otherwise |
| `actor.principal` | `person` or `service_account` |
| `actor.auth_type` | `jwt` or `apikey` |
| `actor.key_id` | The key's id. `null` for a JWT |
| `service`, `method` | From the route |
| `route` | The matched public path template. Kind params stay as `{organization}`; their values are in `nodes`, never as slugs |
| `params` | The route's own path params by name, as matched |
| `ip` | Client IP ([`reference/gateway.md`](../gateway.md#client-ip)) |
| `user_agent` | Truncated to 512 characters. `null` when absent |
| `outcome` | `pending`, `rejected`, `responded`, `no_response` |
| `status` | The status the gateway answered. `null` while `pending`, or when the client was gone |
| `details` | The service's object, or `null` |
| `created_id` | From `X-Plat5-Audit-Created`, or `null` |

Not in an event: bodies, the query string, headers other than `User-Agent`, the actor's role or permissions, slugs, or a principal id.

## Reading

```
GET /nodes/{node_id}/audit-events
GET /users/{user_id}/audit-events
```

On audit's public port. The first is a node's log: every event whose `nodes` or `named_nodes` contain it, or whose `created_id` is it, so the node and everything below it. In it, a named event's `actor.member_id` is `named_member_id`, its `actor.user_id` is `null`, and `actor_member_id` filters on the named member: a user id never appears outside the person's own log. The second is a person's own log: the user-route events whose actor is that user.

Audit's resources publish them: `GET …/audit-events` at every kind with `audit:read`, and `GET /user/audit-events` for the caller's own ([resources](../config/resources.md#audits-defaults)). Reading a project's history takes `audit:read` at the project, or at the org.

| Query | |
|-------|--|
| `limit`, `starting_after` | As [`reference/lists.md`](../lists.md) |
| `actor_member_id` | The acting member |
| `service`, `method`, `outcome`, `request_id`, `created_id` | Exact match |
| `status` | The response status, exact |
| `route` | Exact template |
| `params[<name>]` | A route param's value. Several names AND |
| `occurred_after`, `occurred_before` | RFC 3339. After is inclusive, before is exclusive |

Filters AND. An unknown query param or a malformed value is **422**.

```json
{ "audit_events": [ ... ], "has_more": false }
```

Order is `id` **descending**: newest first. This is the one Plat5 list that runs newest first.

Audit doesn't know which nodes exist. A node with no events is an empty list, not **404**.
