# Internal APIs

Calls on each service's `INTERNAL_PORT`, never on the gateway: between Plat5's own services, and from the deployment's services to identity's lifecycle log and access evaluation. They take no token: internal ports are reachable only inside the deployment's network, never exposed outside it. Publishing one is giving away the deployment.

## Identity

### Admit

One call that turns a credential and the route's node chain into the member and permissions for this request. The gateway calls it on a cache miss ([`reference/gateway.md`](../gateway.md#cache)), at `{IDENTITY_INTERNAL_URL}/internal/admit`.

```
POST /internal/admit
{
  "credential": "e10s-k-2-…",          // or "idp": { "issuer", "subject", "name"?, "scopes"?, "claims"? }, or "anonymous": true
  "chain": [ { "kind": "organization", "ref": "gruow" }, { "kind": "project", "ref": "gatheroom" } ],
  "allow_deleted": false,
  "allow_public": false
}
→ 200
{
  "valid": true,
  "principal": "service_account",
  "key_id": "01jbz…",
  "allowed_cidrs": ["203.0.113.0/24"],
  "org_allowed_cidrs": ["198.51.100.0/24"],
  "nodes": [ { "kind": "organization", "id": "01jbw…" }, { "kind": "project", "id": "01jbx…" } ],
  "member_id": "01jby…",
  "user_id": null,
  "permissions": ["catalog:write", "project:read"],
  "rate_limit_tier": "enterprise",
  "expires_at": "2026-12-01T00:00:00Z",
  "invalidation_seq": 42,
  "config_revision": 7
}
```

1. **Principal.** Hash the key, look up the IdP link, or accept an `anonymous: true` admit (requires `allow_public: true` and a non-empty `chain`). A new IdP subject creates the person. `name`, the first non-empty claim of `AUTH_NAME_CLAIMS`, sets the person's name if it is currently empty ([a person's name](../idp.md#a-persons-name)). `valid` means only that the credential is good: a known, unrevoked, unexpired key, an IdP subject of an active principal, or a valid anonymous public request. A suspended person, an unknown, revoked or expired key, a key identity can't read, or a service account whose node is deleted is `valid: false`, on any chain.
2. **Nodes.** Resolve each `ref` (slug or id) under the previous node, by kind. A missing, purged or deleted node ends here. With `allow_deleted`, the last node may be deleted (not purged), and its own bindings still count. Nothing else ever counts a deleted node's bindings.
3. **Root security policy & member.** Load the resolved root's security policy (`org_allowed_cidrs`, `allow_personal_keys`, `required_idp_claims`). For a personal key when `allow_personal_keys` is `false`, or a JWT whose forwarded `idp.claims` fail `required_idp_claims`, admit refuses access in this tree (`member_id: null`, empty `nodes` and `permissions`). Otherwise find the principal's active, unsuspended member in the chain's tree (or if `allowed_roots` on a key excludes the root, `member_id: null`). A suspended member of the tree is refused and never falls back to public bindings.
4. **Permissions.** The union of the roles (predefined and custom) of that member's unexpired direct bindings and group bindings on any node in the chain, includes resolved ([`concepts/access.md`](../../concepts/access.md#resolution)), plus matching `all_users` / `all_authenticated_users` bindings when `allow_public: true` (and the caller is not a suspended member of the tree), intersected with the key's `scopes` (when non-empty) or the JWT's `scopes` (when sent). `rate_limit_tier` is the resolved root's tier (`null` when unset or on a user route). `expires_at` is the earliest RFC 3339 UTC expiration among the key and the counting bindings (`null` when none expire), used by the gateway to cap its admit cache TTL.

An empty `chain` is a user route: `nodes`, `member_id` and `permissions` are empty, and `rate_limit_tier` is `null`. For a person with an unrestricted credential, `user_id` is the person's. A valid service account, or a personal key narrowed by `scopes` or `allowed_roots`, answers `valid: true` with `user_id: null`, and the gateway refuses it with **403** on a user route. On a tree route `user_id` is `null`.

Every field is in every answer, so the gateway never sees a missing one. With `valid: false`, `principal`, `key_id`, `member_id` and `user_id` are `null`, and `allowed_cidrs`, `nodes` and `permissions` are empty. A missing node and a caller outside the tree both answer `member_id: null` with empty `nodes` and `permissions`. `key_id` is the key's id for a key, `null` for a JWT. `allowed_cidrs` is the key's IP allowlist (`[]` for a JWT or an unrestricted key). `permissions` is sorted, with `*` as the role wrote it. `invalidation_seq` is the commit-ordered head of identity's invalidation log as of this read, and `config_revision` is the control-plane revision identity used to resolve permissions.

Identity resolves `chain` by kind name only and doesn't check it against its kinds: a gateway a revision ahead or behind gets `member_id: null`, not an error. A body identity can't read is **422**: neither or both of `credential` and `idp`, or an `idp` without `issuer` or `subject`.

#### What the gateway answers

| Admit result | Status |
|--------------|--------|
| `valid: false`, or `allowed_cidrs` non-empty and doesn't contain the [client IP](../gateway.md#client-ip), or an anonymous `allow_public` request denied at any check | **401** |
| `principal: "service_account"` on a user route | **403** |
| `org_allowed_cidrs` non-empty and doesn't contain the [client IP](../gateway.md#client-ip) (authenticated request) | **403** |
| `member_id: null` (and no public access on an `allow_public` route) on a kind route | **403** |
| `permissions` share none with the route, and the route isn't `any_member` | **403** |
| Otherwise | Forwarded |
| Transport error, non-2xx, missing fields | **503** |

The order of these steps, and which answers the failed-auth IP limiter counts: [access](../../concepts/access.md#how-a-request-is-decided).

### Admit invalidations

Long-poll feed of commit-ordered access invalidations for gateway admit caches (`GET {IDENTITY_INTERNAL_URL}/internal/admit-invalidations`). Never exposes a `principal_id`; every target is an id the gateway already receives from admit.

```
GET /internal/admit-invalidations?after=41&config_revision=7
→ 200
{
  "head": 43,
  "config_revision": 7,
  "reset": false,
  "events": [
    { "id": 42, "scope": "key", "target": "01jbz…" },
    { "id": 43, "scope": "member", "target": "01jby…" }
  ]
}
```

| Field | |
|-------|--|
| `head` | Newest invalidation sequence id in identity (`0` when empty) |
| `config_revision` | Control-plane revision this identity replica currently serves |
| `reset` | `true` when `after` was pruned or ahead of `head` (DB reset); gateway flushes cached entries and advances its cursor to `head` |
| `events` | Invalidation entries past `after`, ascending by `id`, up to `limit` (default 500, max 1000). Each carries `scope` (`key`, `user`, `member`, `root`) and `target` (`key_id`, `user_id`, `member_id`, `root_id`) |

- Without `after`: answers immediately with `head`, `config_revision`, `reset: false`, and empty `events` (initial sync).
- With `after` (and optional `config_revision`): waits up to 25 seconds until either an invalidation past `after` commits or identity's `config_revision` differs from the caller's. Nothing in that time is **204**.
- Reads stay off traces and request metrics, like health checks. Postgres unavailable: **503**.

### Plan counts

```
POST /internal/plan
{ "kinds": ["folder"] }
→ { "kinds": { "folder": { "nodes": 5, "sample": ["01jbx…"] } } }
```

The control plane calls it on a dry run and again on the apply, only when the plan removes a kind or changes a kind's parent ([`concepts/config.md`](../../concepts/config.md#blocked-changes)). A plan that does neither never calls identity. `nodes` counts the kind's live and deleted nodes, purged never. `sample` is up to five of the counted nodes' ids, live ones first, in id order. Every kind in the request has an entry, zeros included. All counts come from one snapshot. A body identity can't read (a kind name empty) is **422**.

### Lifecycle events

Identity's one log of what's created and what ends. When each event is written, what the log guarantees, and what a service does with it: [lifecycle](../../concepts/lifecycle.md#lifecycle-events).

```
GET /internal/events?after=812&limit=50&types=node.purged,member.removed,user.deleted
→ 200
{
  "events": [
    { "id": "813", "type": "node.purged", "kind": "project", "node_id": "01jbx…", "root_id": "01jbw…", "occurred_at": "2026-10-27T09:14:03Z" },
    { "id": "815", "type": "member.removed", "member_id": "01jby…", "root_id": "01jbw…", "principal": "person", "occurred_at": "2026-10-27T09:15:40Z" },
    { "id": "816", "type": "user.deleted", "user_id": "01jbz…", "occurred_at": "2026-10-27T09:15:40Z" }
  ],
  "has_more": false
}
```

| Field | |
|-------|--|
| `id` | A decimal string from the log's one sequence |
| `type` | `node.created`, `node.deleted`, `node.restored`, `node.purged`, `member.created`, `member.removed`, `user.created` or `user.deleted` |
| `kind`, `node_id` | Node events only: the node's kind and id |
| `member_id`, `principal` | Member events only: the member, and `person` or `service_account` |
| `root_id` | Node and member events: the root of the event's tree |
| `user_id` | User events only: the person's user id |
| `occurred_at` | The time of the transaction that wrote the event, RFC 3339 in UTC |

A field an event doesn't carry is absent, not `null`. A node or member event never carries a user id, and a user event carries no tree. No event carries a principal id, a name or a role.

| Param | |
|-------|--|
| `after` | Optional. The last `id` read; omitted, the log starts from its first event. Anything but a positive decimal integer with no leading zero: **422** |
| `limit` | Optional. Default **50**, max **100**; over the max is clamped. Below 1, or not an integer: **422** |
| `types` | Optional. A comma-separated list of types; only those are answered, and only those end a wait. Omitted, every type. A type not in the list above, or an empty item: **422** |

A **422** is `VALIDATION_ERROR` in the [error envelope](../errors.md), naming the param. A **409** `CURSOR_AHEAD` has `null` details.

`events` are in `id` order, ascending, which is commit order. Ids increase but can skip numbers: don't do arithmetic on an id. `has_more` is `true` when more events matching `types` follow the last one in `events`; ask again at once with its `id` as `after`.

With or without `after`, a read with nothing to answer waits until an event past `after` (or any event, without one) that matches `types` exists, for up to 25 seconds. Nothing in that time is **204**, and the reader asks again with the same `after`. So a **200** always has at least one event, and a reader needs no pacing of its own: after a 200 or a 204 it asks again at once. An event reaches waiting readers on every identity replica within about a second.

An `after` past the newest event is **409** `CURSOR_AHEAD`, at once: identity's log was reset (its database restored or replaced), so the events the reader saved a cursor past no longer exist. The reader starts over from the log's first event ([lifecycle](../../concepts/lifecycle.md#following-the-log)).

Reads stay off traces and request metrics, like health checks. A 5xx is still logged. Postgres unavailable: **503** `SERVICE_UNAVAILABLE`.

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
3. **Permission.** The union of the roles of the member's direct bindings and group bindings on the node and its ancestors grants `action.name`: `includes` resolved, `*` granting every permission ([resolution](../../concepts/access.md#resolution)).

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

#### Resource search

<a id="resource-search"></a>Which live nodes of a kind a member holds a permission on, within a scope node or across their whole tree. Built on the AuthZEN Resource Search shape, on identity's `INTERNAL_PORT` only, at `POST /access/v1/search/resource`. Never a gateway route.

```
POST /access/v1/search/resource
{
  "subject":  { "type": "member", "id": "01jby…" },
  "action":   { "name": "project:read" },
  "resource": { "type": "project" },
  "scope":    { "node_id": "01jbw…" },
  "page":     { "limit": 100, "token": "01jbx…" }
}
→ 200
{ "all": true, "results": [] }
// or
{
  "all": false,
  "results": [
    { "type": "project", "id": "01jbx…" },
    { "type": "project", "id": "01jcz…" }
  ],
  "page": { "next_token": "01jcz…" }
}
```

| Field | |
|-------|--|
| `subject` | Required. `type` is `member`, `id` a member id |
| `action` | Required. `name` is a permission, as roles grant it |
| `resource` | Required. `type` is a kind name (`id` and `properties` accepted and ignored) |
| `scope` | Optional. `{ node_id }`: a live node in the member's tree to search at or below. Omitted: the member's root |
| `page` | Optional. `{ limit?, token? }`: `limit` 1 to 1000 (default 100); `token` is a lowercase ULID cursor (`id > token`) |
| `context` | Optional. Accepted and ignored |

- **Short-circuit (`all: true`).** When the member is admissible, `scope` (or their root) is live with live ancestors, `resource.type` is a kind at or below `scope`'s kind, and the member's direct or group bindings on `scope` or its ancestors already grant `action.name`, identity answers `{ "all": true, "results": [] }` without enumerating descendants: the caller holds `action.name` on every live node of `resource.type` under `scope`.
- **Explicit results (`all: false`).** Otherwise, `results` lists each live node of `resource.type` at or below `scope` (whose ancestors are all live) on which the member holds `action.name` through direct or group bindings, ordered by `id` ascending. When more results follow, `page.next_token` is the last id returned.
- **No matches.** An unknown, removed, or inadmissible member, a missing/deleted/purged scope, a scope in another tree, or an unknown/unreachable kind answers `{ "all": false, "results": [] }`, never an error. Identity keeps no cache for these answers.

#### Errors

| Result | Response |
|--------|----------|
| Answered | **200**, whatever the decisions |
| A body identity can't read: not sent as `application/json`, or not a JSON object; a `subject`, `resource` or `action` missing, after defaults; a `subject.type` other than `member`; an id that isn't a lowercase ULID; an empty `resource.type` or `action.name`; a field of the wrong type; an unknown `evaluations_semantic`; more than 100 items; or invalid `scope.node_id`, `page.limit` (outside 1–1000) or `page.token` | **400** `INVALID_REQUEST`, in the [error envelope](../errors.md). `message` says which field, with its path (`evaluations[2].subject.id`) |
| Postgres unavailable | **503** `SERVICE_UNAVAILABLE`. No decision: the caller acts as if it were `false`, and retries later |

AuthZEN gives an error status and a message string, with no shape for the body; the message is the envelope's `message`. An `X-Request-ID` on the request comes back on the response, as AuthZEN asks.

## Control plane feeds

On `INTERNAL_PORT`. Each is one read, at one head, in one Postgres snapshot. Each answers the body stored with that revision at apply, as it was expanded then ([revisions](../../concepts/config.md#revisions)); a feed read expands nothing.

| Path | Reader | `200` body |
|------|--------|------------|
| `GET /internal/gateway` | gateway | `{ "revision", "kinds", "services", "rate_limits", "defaults", "cors" }`. **404** before the first apply. `defaults` is `{ "rate_limit" }` (empty array when `defaults.rate_limit` is unset), used to rate-limit `POST /access/routes` |
| `GET /internal/identity` | identity | `{ "revision", "deletion_grace", "reserved_slugs", "kinds", "roles", "creator_role" }`. **404** before the first apply |

The gateway's `services` are expanded: each service's `url`, and its own `timeouts` and `max_body_bytes` with the config's `defaults` filled in, and its `routes` as the gateway matches them, one per method:

```json
{ "path": "/orgs/{organization}/projects/{project}/plans/{plan_id}/versions", "method": "POST",
  "scope": "project", "upstream": "/projects/{project.id}/plans/{plan_id}/versions",
  "required_permissions": ["catalog:write"], "rate_limit": ["standard"], "audit": true, "allow_deleted": false }
```

`rate_limit` is resolved (`defaults.rate_limit` for a route that names none, or `[]` when `defaults.rate_limit` is unset or the route says `false`), `audit` is the method default filled in (`false` in `public`, which is never audited), and `required_permissions` is a list or `"any_member"` (`null` outside a kind scope). Roles are served as written, with `includes`; identity resolves them. Kinds are served with `list_permission` filled in. Other keys have defaults filled in: `deletion_grace` `30d`, `reserved_slugs` `[]`, `rate_limits` `{}`, and `cors` `{ "allowed_origins": [], "allowed_headers": [] }` when omitted.

Without `after`, the answer is immediate. With `?after={revision}`, the control plane waits until the head is past `after`, for up to 25 seconds. Nothing newer in that time is **204**, and the reader asks again. A cursor ahead of the head (the store was reset) is answered at once.

The gateway reports where it stands on each poll:

```
GET /internal/gateway?after=42&replica=gw-1&loaded=42&refused=43&reason=…
```

`replica` is the replica's id (`service.instance.id` from `OTEL_RESOURCE_ATTRIBUTES`, else the hostname), `loaded` the revision it serves (`0` before its first), and `refused` and `reason`, sent while the newest revision it was served is one it refused, why. The control plane keeps the latest report per replica for [`GET /status`](control-plane.md#reader-status).

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
{ "occurred_at": "…", "outcome": "responded", "status": 200, "details": { … }, "created_id": "…", "named_node_ids": ["…"], "named_member_id": "…" }
```

`occurred_at` is optional; when sent (the gateway echoes the intent's timestamp), audit uses it to prune to the single monthly partition instead of scanning every partition. `named_node_ids` and `named_member_id` only for a user-route event, from the service's [named nodes](../../concepts/audit.md#named-nodes), and only with `responded`. Sent for a tree route's event, they're ignored.

| Result | Response |
|--------|----------|
| Applied, or the event is already final | **204** |
| No event for this `request_id` | **404**. The gateway stops retrying |
| A field it can't store, or `outcome: pending` | **422** |
| Postgres unavailable | **503**. The gateway retries |

### Completed and rejected (single write)

A gateway refusal after admit (a permission **403**, a **400** on a param, a **500**) — and any forwarded audited request when `AUDIT_MODE=async` — is sent as one background `PUT /internal/events/{request_id}` carrying the intent fields plus the final outcome fields (`outcome`, `status`, and optionally `details`, `created_id`, `named_node_ids`, `named_member_id`). Audit inserts the row already final and adds the corresponding `audit_log_keys` (`created_id`, and `named_node_ids` on user routes) in one transaction. It answers as an intent does.
