# Internal APIs

Calls between Plat5's own services, on each service's `INTERNAL_PORT`, never on the gateway. When `INTERNAL_AUTH_TOKEN` is set, every call carries it as `X-Plat5-Internal-Token` (constant-time compare). Unset means network trust only, for dev.

## Identity

### Admit

One call that turns a credential and the route's node chain into the member and labels for this request. The gateway calls it on a cache miss ([`reference/gateway.md`](../gateway.md#cache)).

```
POST /internal/admit
{
  "credential": "e10s-k-2-…",          // or "idp": { "issuer", "subject", "name"? }
  "scope": "project",
  "chain": [ { "kind": "organization", "ref": "gruow" }, { "kind": "project", "ref": "gatheroom" } ],
  "resolve_deleted": false
}
→ 200
{
  "valid": true,
  "found": true,
  "principal": "person",
  "nodes": [ { "kind": "organization", "id": "n_…" }, { "kind": "project", "id": "n_…" } ],
  "member_id": "m_…",
  "labels": ["project:read", "catalog:write", "subscribers:write"],
  "good_until": "2026-11-01T00:00:00Z"
}
```

1. **Principal.** Hash the key, or look up the IdP link. A new IdP subject creates the person and their directory member; `name` (from the `name_claim`) sets or updates the person's name. A suspended person, a revoked key, or a service account whose born node is deleted is `valid: false`.
2. **Nodes.** Resolve each `ref` (slug or id) under the previous node, by kind. A missing, purged or deleted node ends here with `found: false`. With `resolve_deleted`, the last node may be deleted (not purged), and its own bindings still count. Nothing else ever counts a deleted node's bindings.
3. **Member.** The principal's active member in the chain's tree. None means `member_id: null` and `labels: []`.
4. **Labels.** The union of the roles of that member's live bindings on any node in the chain ([`concepts/access.md`](../../concepts/access.md#resolution)).
5. **Good until.** The earliest `expires_at` among the contributing bindings, or `null`.

On a sign-in-kind route, `chain` is empty, the member is the person's directory member, and `labels` is `[]`. A service account has none, so its keys are `valid: false` there.

Every field is in every answer, so the gateway never sees a missing one. With `valid: false`, `found` is `false`, `principal`, `member_id` and `good_until` are `null`, and `nodes` and `labels` are empty. With `found: false`, `nodes` is empty too: a partial chain says nothing the gateway uses. On a sign-in route `nodes` is empty. `labels` is sorted, with `*` as the role wrote it.

Identity resolves `chain` by kind name only and doesn't check it against its kinds: a gateway a revision ahead or behind gets `found: false`, not an error. A body identity can't read is **422**: neither or both of `credential` and `idp`, an `idp` without `issuer` or `subject`, `scope` empty or `public`, a chain on a sign-in route, or a chain that is empty or doesn't end at `scope` on any other.

| Result | Gateway |
|--------|---------|
| `valid: false` | **401** |
| `found: false`, or no route label in `labels` (and the route isn't `[]`), or `member_id: null` | **403** "permission denied, or it may not exist" |
| Otherwise | Forwarded with the subject |
| Transport error, non-2xx, missing fields | **503** |

### Plan counts

```
POST /internal/plan
{ "kinds_removed": ["folder"], "parents_narrowed": [{ "kind": "project", "removed": ["folder"] }] }
→ { "kinds_removed": { "folder": { "nodes": 14, "bindings": 22, "service_accounts": 3, "keys": 9 } }, "parents_narrowed": { "project": { "nodes": 3, "sample": ["n_…"] } } }
```

The control plane calls it on a dry run, and again on the apply to check the confirmed counts, only when the plan removes a kind or narrows a parent. A plan that does neither never calls identity ([`concepts/config.md`](../../concepts/config.md#preconditions)). `nodes` counts every node the removal would delete, live and deleted: the kind's nodes and all their descendants, of any kind (removing `folder` counts the projects under folders too). `bindings`, `service_accounts` and `keys` are those on, born at, or of service accounts born at any of them. A narrowed parent counts the kind's nodes, live and deleted, under a parent kind the config drops, except those a kind removal in the same plan deletes: removing `folder` and dropping it from `project`'s parents is one apply.

The counts are what would be lost. A deleted node counts because it can still be restored; a binding past `expires_at` and a revoked key don't, because they never work again. Purged nodes never count. `sample` is up to five node ids, in id order. Every kind in the request has an entry, zeros included. All counts come from one snapshot. A body identity can't read (a kind name empty) is **422**.

### Lifecycle events

The feed, its event types and how to read it: [lifecycle](../../concepts/lifecycle.md#lifecycle-events).

## Control plane feeds

On `INTERNAL_PORT`. Each is one read, at one head, in one Postgres snapshot.

| Path | Reader | `200` body |
|------|--------|------------|
| `GET /internal/gateway` | gateway | `{ "revision", "kinds", "services", "cors" }`. **404** before the first apply |
| `GET /internal/identity` | identity | `{ "revision", "deletion_grace", "reserved_slugs", "kinds", "roles", "default_role", "creator_role", "services" }`. **404** before the first apply |

`services` is in the stored form: full paths, list-form `methods` (`route_prefix` and nested `methods` maps already expanded). Keys are the config's own ([config](../../concepts/config.md#the-config)), with defaults filled in: `deletion_grace` `30d`, `reserved_slugs` `[]`, and `cors` `{ "allowed_origins": [] }` when the config has none. Identity reads `services` to find the labels of its own read routes ([`reference/lists.md`](../lists.md#filtered-lists)).

Without `after`, the answer is immediate. With `?after={revision}`, the control plane waits until the head is past `after`, for up to 25 seconds. Nothing newer in that time is **204**, and the reader asks again. A cursor ahead of the head (the store was reset) is answered at once.

Feed reads stay off traces and request metrics, like health checks. A 5xx is still logged. Each control-plane process polls the head in Postgres every second, so an apply through any replica reaches readers of every replica within about a second.

How readers boot and keep state: [config](../../concepts/config.md#how-a-change-reaches-readers).

## Audit

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

`root_id` is `nodes[0].id` on a kind route. It's `null`, and `nodes` is `[]`, exactly on a sign-in route; `actor.member_id` is then the person's directory member, and the principal a `person`. `key_prefix` is set for `apikey` and `null` for `jwt`, and only a `person` has a `jwt`. Anything else is malformed.

Unknown fields are ignored, so a newer gateway can send more.

### Outcome

```
PATCH /internal/events/{request_id}
{ "outcome": "responded", "status": 200, "details": { … }, "created_id": "…", "root_id": "…", "member_id": "…" }
```

`root_id` and `member_id` only for a sign-in-route event, from the service's pair: both or neither, and only with `responded`. Sent for a kind route's event, they're ignored; its log is its chain's root.

| Result | Response |
|--------|----------|
| Applied, or the event is already final | **204** |
| No event for this `request_id` | **404**. The gateway stops retrying |
| Malformed, or `outcome: pending` | **422** |
| Postgres unavailable | **503**. The gateway retries |
