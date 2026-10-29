# Internal APIs

Calls on each service's `INTERNAL_PORT`, never on the gateway: between Plat5's own services, and from the deployment's services to identity's event feeds and access evaluation. They take no token: internal ports are reachable only inside the deployment's network, never exposed outside it. Publishing one is giving away the deployment.

## Identity

### Admit

One call that turns a credential and the route's node chain into the member and permissions for this request. The gateway calls it on a cache miss ([`reference/gateway.md`](../gateway.md#cache)), at `{IDENTITY_INTERNAL_URL}/internal/admit`.

```
POST /internal/admit
{
  "credential": "e10s-k-2-…",          // or "idp": { "issuer", "subject", "name"? }
  "chain": [ { "kind": "organization", "ref": "gruow" }, { "kind": "project", "ref": "gatheroom" } ],
  "allow_deleted": false
}
→ 200
{
  "valid": true,
  "principal": "service_account",
  "key_id": "01jbz…",
  "nodes": [ { "kind": "organization", "id": "01jbw…" }, { "kind": "project", "id": "01jbx…" } ],
  "member_id": "01jby…",
  "user_id": null,
  "permissions": ["catalog:write", "project:read"]
}
```

1. **Principal.** Hash the key, or look up the IdP link. A new IdP subject creates the person. `name`, the first non-empty claim of `users.name.claims`, sets the person's name as `users.name.sync` says ([a person's name](../idp.md#a-persons-name)). A suspended person, a revoked key, a key identity can't read, or a service account whose node is deleted is `valid: false`. So is a service account on a user route.
2. **Nodes.** Resolve each `ref` (slug or id) under the previous node, by kind. A missing, purged or deleted node ends here. With `allow_deleted`, the last node may be deleted (not purged), and its own bindings still count. Nothing else ever counts a deleted node's bindings.
3. **Member.** The principal's active member in the chain's tree.
4. **Permissions.** The union of the roles of that member's bindings on any node in the chain, includes resolved ([`concepts/access.md`](../../concepts/access.md#resolution)).

An empty `chain` is a user route: `user_id` is the person's, and `nodes`, `member_id` and `permissions` are empty. On a tree route `user_id` is `null`.

Every field is in every answer, so the gateway never sees a missing one. With `valid: false`, `principal`, `key_id`, `member_id` and `user_id` are `null`, and `nodes` and `permissions` are empty. A missing node and a caller outside the tree both answer `member_id: null` with empty `nodes` and `permissions`. `key_id` is the key's id for a key, `null` for a JWT. `permissions` is sorted, with `*` as the role wrote it.

Identity resolves `chain` by kind name only and doesn't check it against its kinds: a gateway a revision ahead or behind gets `member_id: null`, not an error. A body identity can't read is **422**: neither or both of `credential` and `idp`, or an `idp` without `issuer` or `subject`.

#### What the gateway answers

| Admit result | Status |
|--------------|--------|
| `valid: false` | **401**, counted by the failed-auth limiter |
| `member_id: null` on a kind route | **403**, before the rate limit, counted by the failed-auth limiter |
| `permissions` share none with the route, and the route isn't `any_member` | **403**, after the rate limit |
| Otherwise | Forwarded |
| Transport error, non-2xx, missing fields | **503** |

### Plan counts

```
POST /internal/plan
{ "kinds": ["folder"] }
→ { "kinds": { "folder": { "nodes": 5, "sample": ["01jbx…"] } } }
```

The control plane calls it on a dry run and again on the apply, only when the plan removes a kind or changes a kind's parent ([`concepts/config.md`](../../concepts/config.md#blocked-changes)). A plan that does neither never calls identity. `nodes` counts the kind's live and deleted nodes, purged never. `sample` is up to five of the counted nodes' ids, live ones first, in id order. Every kind in the request has an entry, zeros included. All counts come from one snapshot. A body identity can't read (a kind name empty) is **422**.

### Tree events

Nodes and members, for a service that keeps tree data. When each event is written, and what a service does with it: [lifecycle](../../concepts/lifecycle.md#tree-events).

```
GET /internal/tree-events?starting_after=812&limit=50
→ 200
{
  "tree_events": [
    { "id": "813", "type": "node.deleted", "kind": "project", "node_id": "01jbx…", "root_id": "01jbw…", "occurred_at": "2026-10-27T09:14:03Z" },
    { "id": "814", "type": "member.removed", "member_id": "01jby…", "root_id": "01jbw…", "principal": "person", "occurred_at": "2026-10-27T09:15:40Z" }
  ],
  "has_more": false
}
```

| Field | |
|-------|--|
| `id` | A decimal string from the feed's sequence |
| `type` | `node.created`, `node.deleted`, `node.restored`, `node.purged`, `member.created` or `member.removed` |
| `kind`, `node_id` | Node events only: the node's kind and id |
| `member_id`, `principal` | Member events only: the member, and `person` or `service_account` |
| `root_id` | The root of the event's tree |
| `occurred_at` | The time of the transaction that wrote the event, RFC 3339 in UTC |

A field an event doesn't carry is absent, not `null`. Never a user id, principal id, name or role.

### User events

People, for a service that keeps user-route data. When each event is written, and what a service does with it: [lifecycle](../../concepts/lifecycle.md#user-events).

```
GET /internal/user-events?starting_after=56
→ 200
{
  "user_events": [
    { "id": "57", "type": "user.created", "user_id": "01jbz…", "occurred_at": "2026-10-27T09:12:51Z" },
    { "id": "58", "type": "user.deleted", "user_id": "01jbq…", "occurred_at": "2026-10-27T09:15:40Z" }
  ],
  "has_more": false
}
```

| Field | |
|-------|--|
| `id` | A decimal string from the feed's sequence, separate from tree events' |
| `type` | `user.created` or `user.deleted` |
| `user_id` | The person's user id |
| `occurred_at` | As for tree events |

These four fields and no others: no tree, no member, no name.

### Paging a feed

Both feeds page the same way, like [lists](../lists.md) but with sequence ids.

| Param | |
|-------|--|
| `limit` | Optional. Default **50**, max **100**; over the max is clamped. Below 1, or not an integer: **422** |
| `starting_after` | Optional. The last `id` read from the same feed; omitted, the feed starts from its first event. Anything but a positive decimal integer with no leading zero: **422**. An id past the end is an empty page |

Events come in `id` order, ascending, which is commit order. `has_more` is `true` when another page exists. The two feeds' ids are unrelated: pass each feed its own. Don't do arithmetic on an id. Postgres unavailable: **500** `INTERNAL_ERROR`.

### Access evaluation

Whether a member holds a permission on a node, for a service checking access outside a request, or about a member other than the caller ([access](../../concepts/access.md#outside-a-request)). The paths and shapes of the [AuthZEN Authorization API 1.0](https://openid.net/specs/authorization-api-1_0-final.html), on identity's `INTERNAL_PORT` only, at `/access/v1/…`. Never a gateway route.

```
POST /access/v1/evaluation
{
  "subject": { "type": "member", "id": "01jby…" },
  "resource": { "type": "project", "id": "01jbx…" },
  "action": { "name": "project:read" },
  "context": { … }
}
→ 200
{ "decision": true }
```

| Field | |
|-------|--|
| `subject` | `type` is `member`, `id` a member id. Never a user or principal id |
| `resource` | `type` is a kind name, `id` a node id |
| `action` | `name` is a permission, as roles grant it |
| `context` | Optional. Accepted and ignored |

`properties` on `subject`, `resource` or `action`, and any other field, are ignored.

`decision` is `true` only when every step holds. Admit's resolution decides each one ([admit](#admit)):

1. **Node.** The node exists, its kind is `resource.type`, and it and every node above it are live.
2. **Member.** The member is active in the node's tree, and admit would accept its principal: a person who isn't suspended, or a service account whose node is live.
3. **Permission.** The union of the roles of the member's bindings on the node and its ancestors grants `action.name`: `includes` resolved, `*` granting every permission ([resolution](../../concepts/access.md#resolution)).

Anything else is `false`, never an error: a deleted, purged or unknown node, a `resource.type` that isn't the node's kind, a removed or unknown member, a member of another tree, or no binding that grants the permission. Identity keeps no cache for these answers: each one reads the bindings as they are, and roles as identity has them ([config](../../concepts/config.md#how-a-change-reaches-readers)).

#### Batch

```
POST /access/v1/evaluations
{
  "subject": { "type": "member", "id": "01jby…" },
  "action": { "name": "project:read" },
  "options": { "evaluations_semantic": "execute_all" },
  "evaluations": [
    { "resource": { "type": "project", "id": "01jbx…" } },
    { "resource": { "type": "project", "id": "01jbz…" }, "action": { "name": "project:write" } }
  ]
}
→ 200
{ "evaluations": [ { "decision": true }, { "decision": false } ] }
```

- **Defaults.** The top-level `subject`, `action`, `resource` and `context` are defaults. A key in an item replaces the default whole. Every item must end up with a `subject`, an `action` and a `resource`.
- **Order.** `evaluations` in the answer are in request order, one per item answered.
- **No items.** Without `evaluations`, or with an empty list, the call is one evaluation of the top-level keys, and the answer is `{ "decision": … }`.
- **Size.** At most 100 items.
- **Semantics.** `options.evaluations_semantic`; other options are ignored.

| `evaluations_semantic` | Answers |
|------------------------|---------|
| `execute_all` (the default) | Every item |
| `deny_on_first_deny` | Items up to and including the first `false` |
| `permit_on_first_permit` | Items up to and including the first `true` |

Each item is decided as a single evaluation would decide it. A call resolves each distinct node's chain once.

#### Errors

| Result | Response |
|--------|----------|
| Answered | **200**, whatever the decisions |
| A body identity can't read: not a JSON object; a `subject`, `resource` or `action` missing, after defaults; a `subject.type` other than `member`; an id that isn't a lowercase ULID; an empty `resource.type` or `action.name`; a field of the wrong type; an unknown `evaluations_semantic`; more than 100 items | **400** `INVALID_REQUEST`, in the [error envelope](../errors.md). `message` says which field, with its path (`evaluations[2].subject.id`) |
| Postgres unavailable | **500** `INTERNAL_ERROR`. No decision: the caller acts as if it were `false`, and retries later |

AuthZEN gives an error status and a message string, with no shape for the body; the message is the envelope's `message`. An `X-Request-ID` on the request comes back on the response, as AuthZEN asks.

## Control plane feeds

On `INTERNAL_PORT`. Each is one read, at one head, in one Postgres snapshot.

| Path | Reader | `200` body |
|------|--------|------------|
| `GET /internal/gateway` | gateway | `{ "revision", "kinds", "services", "rate_limits", "cors", "users" }`. **404** before the first apply. `users` gives the gateway `name.claims`, the JWT claims it reads for the `name` it sends to admit |
| `GET /internal/identity` | identity | `{ "revision", "deletion_grace", "reserved_slugs", "users", "kinds", "roles", "creator_role" }`. **404** before the first apply. `users` gives identity `name.sync` |

The gateway's `services` are expanded: each service's `url`, `timeouts` and `max_body_bytes` with `defaults` filled in, and its `routes` as the gateway matches them, one per method:

```json
{ "path": "/orgs/{organization}/projects/{project}/plans/{plan_id}/versions", "method": "POST",
  "scope": "project", "upstream": "/projects/{project.id}/plans/{plan_id}/versions",
  "required_permissions": ["catalog:write"], "rate_limit": ["default"], "audit": true, "allow_deleted": false }
```

Identity's and audit's resources are expanded the same way. `rate_limit` is resolved (`[]` for none), `audit` is the method default filled in (`false` in `public`, which is never audited), and `required_permissions` is a list or `"any_member"` (`null` outside a kind scope). Roles are served as written, with `includes`; identity resolves them. Other keys are the config's own ([config](../../concepts/config.md#the-config)), with defaults filled in: `deletion_grace` `30d`, `reserved_slugs` `[]`, `users` `{ "name": { "claims": ["name", "preferred_username"], "sync": "first_sign_in" } }` (each field filled in on its own), and `cors` `{ "allowed_origins": [], "allowed_headers": [] }` when the config has none.

Without `after`, the answer is immediate. With `?after={revision}`, the control plane waits until the head is past `after`, for up to 25 seconds. Nothing newer in that time is **204**, and the reader asks again. A cursor ahead of the head (the store was reset) is answered at once.

The gateway reports where it stands on each poll:

```
GET /internal/gateway?after=42&replica=gw-1&loaded=42&refused=43&reason=…
```

`replica` is the replica's id (`OTEL_SERVICE_INSTANCE_ID`, else the hostname), `loaded` the revision it serves (`0` before its first), and `refused` and `reason`, sent while the newest revision it was served is one it refused, why. The control plane keeps the latest report per replica for [`GET /status`](control-plane.md#reader-status).

Feed reads stay off traces and request metrics, like health checks. A 5xx is still logged. Each control-plane process polls the head in Postgres every second, so an apply through any replica reaches readers of every replica within about a second.

How readers boot and keep state: [config](../../concepts/config.md#how-a-change-reaches-readers).

## Audit

### Intent

```
PUT /internal/events/{request_id}
{
  "occurred_at": "…", "nodes": [ … ],
  "actor": { "member_id": "…", "user_id": null, "principal": "…", "auth_type": "…", "key_id": "…" },
  "service": "…", "method": "…", "route": "…", "params": { … },
  "ip": "…", "user_agent": "…"
}
```

| Result | Response |
|--------|----------|
| Written | **201** |
| An event for this `request_id` exists | **200**, unchanged |
| A field it can't store: a missing `request_id`, `occurred_at` or `service`, or a value of the wrong type | **422** |
| Postgres unavailable | **503**. The gateway retries |

Audit stores what it's sent and doesn't repeat the gateway's or identity's rules. Unknown fields are ignored, so a newer gateway can send more.

### Outcome

```
PATCH /internal/events/{request_id}
{ "outcome": "responded", "status": 200, "details": { … }, "created_id": "…", "named_node_ids": ["…"], "named_member_id": "…" }
```

`named_node_ids` and `named_member_id` only for a user-route event, from the service's [named nodes](../../concepts/audit.md#named-nodes), and only with `responded`. Sent for a tree route's event, they're ignored.

| Result | Response |
|--------|----------|
| Applied, or the event is already final | **204** |
| No event for this `request_id` | **404**. The gateway stops retrying |
| A field it can't store, or `outcome: pending` | **422** |
| Postgres unavailable | **503**. The gateway retries |

### Rejected

The gateway's own refusals after admit (a permission **403**, a **400** on a param, a **500**) are one write, in the background: an intent that also carries `"outcome": "rejected"` and its `status`, written final. It answers as an intent does.
