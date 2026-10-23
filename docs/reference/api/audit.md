# Audit API

The event and the read API. Concept: [audit](../../concepts/audit.md).

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
| `root_id` | The root whose log this is in. `null` on a sign-in route: the person's log is keyed by `actor.member_id` |
| `nodes` | The route's chain, root first, as ids. `[]` on a sign-in route |
| `named_root_id`, `named_member_id` | On a sign-in route, the tree and member the service named. `null` otherwise |
| `actor.member_id` | The caller's member in this tree; on a sign-in route, their directory member, except in the named root's read ([reading](#reading)) |
| `actor.principal` | `person` or `service_account` |
| `actor.auth_type` | `jwt` or `apikey` |
| `actor.key_prefix` | The key's display prefix. `null` for a JWT |
| `service`, `method` | From the route |
| `route` | The matched path template, after `route_prefix`. Chain params stay as `{organization}`; their values are in `nodes`, never as slugs |
| `params` | Non-chain path params by name, as matched |
| `ip` | Client IP ([`reference/gateway.md`](../gateway.md#client-ip)) |
| `user_agent` | Truncated to 512 characters. `null` when absent |
| `outcome` | `pending`, `rejected`, `responded`, `no_response` |
| `status` | The status the gateway answered. `null` while `pending`, or when the client was gone |
| `details` | The service's object, or `null` |
| `created_id` | From `X-Plat5-Audit-Created`, or `null` |

Not in an event: bodies, the query string, headers other than `User-Agent`, the actor's role or labels, slugs, or a principal id.

## Reading

```
GET /nodes/{node_id}/audit-events
GET /members/{member_id}/audit-events
```

On audit's public port. The first returns events on the node and everything below it: any event whose `nodes` contains it, or whose `named_root_id` is it. In that root's read, a named event's `actor.member_id` is `named_member_id`, and `actor_member_id` filters on it: a directory member never appears outside the person's own log. The second is a person's own log: the sign-in-route events whose `actor.member_id` is that directory member.

The generated catalog publishes them per kind: `GET /orgs/{organization}/audit-events` with `organization:audit:read`, `GET /orgs/{organization}/projects/{project}/audit-events` with `project:audit:read`, and `GET /me/audit-events` on the sign-in kind ([`reference/api/edge-catalog.md`](edge-catalog.md#generated-routes)). Reading a project's history takes a role at the project, or at the org.

| Query | |
|-------|--|
| `limit`, `starting_after` | As [`reference/lists.md`](../lists.md) |
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
