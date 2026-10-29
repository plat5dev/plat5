# Config

The deployment's desired state: settings, kinds, roles, rate limits, CORS and services, as one config. The operator applies the whole config at once: a dry run shows the plan, and the apply carries a precondition that makes a stale plan fail. The control plane validates the config as a whole, records it as a revision in Postgres, and serves it to the gateway and identity. It is Plat5's only supported write path for any of these.

## What it holds

The control plane holds the operator's typed, deployment-wide state that changes while the deployment runs. Everything that belongs to a tree (nodes, members, bindings, keys) stays in identity.

| Document | Read by | Contract |
|----------|---------|----------|
| Settings: `deletion_grace`, `reserved_slugs`, `users`, `route_checks`, `defaults` | identity, gateway | [settings](../reference/config/settings.md) |
| `kinds` | gateway, identity | [kinds](../reference/config/kinds.md) |
| `roles`, `creator_role` | identity | [roles](../reference/config/roles.md) |
| `rate_limits` | gateway | [rate limits](../reference/config/rate-limits.md) |
| `cors` | gateway | [cors](../reference/config/cors.md) |
| `services` | gateway | [services](../reference/config/services.md); identity's and audit's [resources](../reference/config/resources.md) |

Each document has a published schema ([below](#schema)), and the [rules at apply](#validation-at-apply) check them together. There is no free-form key.

These stay out of it, as process env:

| Kept out | Why |
|----------|-----|
| Wiring: URLs, ports, and `AUDIT_ENABLED`, whether the deployment runs an audit service | Needed to reach the control plane, or to boot, in the first place |
| Secrets: `ADMIN_TOKEN` | Not desired state |
| Trust anchors: `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`, `AUTH_SUBJECT_CLAIM` | Changing the issuer lets you sign in as anyone; `ADMIN_TOKEN` must not be able to |
| `APIKEY_BRAND`, `ADMIT_CACHE_TTL`, `ADMIT_NEGATIVE_CACHE_TTL` | Set at boot; changing them while running buys nothing |
| Per-tree data | Identity |

## The config

Two kinds of file, split by owner, kept in a repo:

- **`plat5.yml`** is the deployment's shape: settings, kinds, roles, rate limits and CORS. It's small, changes rarely, and belongs to whoever runs Plat5. It also lists the service files.
- **One service file per service**: its URL, timeouts and routes. It belongs to that service's team. Identity's and audit's files hold only overrides of their [resources](../reference/config/resources.md).

```yaml
# plat5.yml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
kinds: { … }
roles: { … }
creator_role: admin
rate_limits: { … }
cors:
  allowed_origins: [https://app.example.com]
services:
  - services/identity.yml
  - services/audit.yml
  - services/e10s.yml
```

```yaml
# services/e10s.yml: one service, keyed by its name
e10s:
  url: http://e10s:3000
  scopes:
    project:
      upstream_prefix: /projects/{project.id}
      routes: [ … ]
```

The CLI reads `plat5.yml`, follows its `services` list, and sends one flat object: every top-level key of `plat5.yml`, with `services` replaced by a map of service name to service. That object is what the control plane validates and stores; the files are only how a team keeps it. A service file holding anything but exactly one service, or two files naming the same service, is **422** from the CLI.

**The config is the whole desired state.** Each apply replaces everything: a service missing from `services` is removed, and the plan shows it. There is no per-document or per-service write.

The config has no format key: its absence means the first format. A second format adds a `version` key, with a migration the CLI and control plane run. Any key the schema doesn't know is **422**.

The admin API: [`reference/api/control-plane.md`](../reference/api/control-plane.md). The CLI: [`reference/cli.md`](../reference/cli.md).

## Validation at apply

The control plane is the one validator: it checks the schema, then these rules, on every apply. The gateway and identity trust what it serves and don't check it again. Each error names the field path, such as `services.e10s.scopes.project.routes[2].path` or `kinds.project.parent`; the CLI adds the file, since `services.<name>` is that service's file and everything else is `plat5.yml`.

1. Every `parent` is a kind, and there are no cycles. Kind names are lowercase identifiers and not `public`, `member` or `user`.
2. Every `bindable_at` kind exists. Every role in `includes` exists, and includes don't cycle. `creator_role` is a role bindable at every kind.
3. Every permission a role grants is required by some route, identity's and audit's included, or is a kind's `list_permission`. `*` is always allowed.
4. Every route's scope is a kind, `user` or `public`, and its path, methods, permissions and upstream follow the [route rules](../reference/config/services.md#validation).
5. No two routes, in one service or across services, have the same method and shape ([route matching](../reference/config/services.md#matching)). No route matches `route_checks.path`, where the gateway answers [route checks](../reference/gateway.md#route-checks), in any method.
6. `rate_limits` has a `default` policy without `count_at`. Every `rate_limit` names a policy in `rate_limits`, and a policy's `count_at` is the route's kind or an ancestor.
7. `identity` and `audit` name only their own resources and verbs, and kinds that exist ([resources](../reference/config/resources.md)).

A kind with nodes can't be removed or change parent: those are [blocked](#blocked-changes) at plan time. Shape errors (a malformed duration, a reserved slug that isn't a slug, an unknown key) are reported by the [schema](#schema) before these run.

## Schema

One JSON Schema per document, and one for the whole config, in the repo at `schema/` and served at `GET /schema`. It is the single definition of each document's shape:

- The CLI validates locally with it (`plat5 validate`), so editors and CI catch shape errors without a deployment.
- The control plane validates every apply with it before the rules above.

The gateway and identity don't check snapshots against it: they parse what they need and build from it ([readers](#how-a-change-reaches-readers)).

## Plan and apply

Two calls on one endpoint. A dry run computes the plan and writes nothing; an apply recomputes the same plan and writes it only if its precondition holds. The server keeps no plans.

```
POST /apply?dry_run=true
{ "deletion_grace": "30d", "kinds": {…}, "roles": {…}, "creator_role": "admin", …, "services": {…} }
→ 200 the plan

POST /apply
{ "deletion_grace": "30d", "kinds": {…}, "roles": {…}, "creator_role": "admin", …, "services": {…},
  "expect": { "revision": 41 } }
→ 200 { "revision": 42, "summary": {…} }
```

The body is the flat config [above](#the-config). A dry run ignores `expect`.

A dry run writes nothing. The apply recomputes the plan and is refused if anyone applied since the dry run ([preconditions](#preconditions)), so a stale plan never lands.

### The plan

```json
{
  "revision": 41,
  "summary": { "add": 1, "change": 3, "remove": 0, "blocked": 1 },
  "changes": [
    { "op": "add", "what": "kind", "name": "environment",
      "after": { "parent": "project", "path": "/envs/{environment}", "list_permission": "environment:read" } },
    { "op": "remove", "what": "kind", "name": "folder",
      "before": { "parent": "organization", "path": "/folders/{folder}", "list_permission": "folder:read" },
      "blocked": { "reason": "kind_not_empty", "in_the_way": { "nodes": 3, "sample": ["01jbx…"] } } },
    { "op": "change", "what": "role", "name": "admin",
      "before": { "bindable_at": ["organization", "project", "folder"] },
      "after": { "bindable_at": ["organization", "project", "environment"] } },
    { "op": "change", "what": "role", "name": "developer",
      "before": { "permissions": ["project:read"] }, "after": { "permissions": ["project:read", "project:write"] } },
    { "op": "change", "what": "service", "name": "identity",
      "routes": [
        { "op": "add", "method": "GET", "path": "/orgs/{organization}/projects/{project}/envs",
          "after": { "scope": "project", "upstream": "/nodes/{project.id}/children/environment", "required_permissions": "any_member" } }
      ] }
  ]
}
```

Changes are ordered kinds, settings, roles, rate limits, CORS, services, then by name.

- `revision` is the config revision the plan was made against, `0` before the first apply. The apply sends it back as `expect.revision`.
- `what` is `kind`, `role`, `rate_limit`, `service`, `cors`, or `setting` (named `deletion_grace`, `reserved_slugs`, `users`, `route_checks`, `defaults` or `creator_role`). `op` is `add`, `change` or `remove`.
- `before` and `after` show what changed, field by field: only the fields that differ, `before` absent on an add, `after` absent on a remove.
- A service line lists its `routes`, expanded as the gateway will match them (full public path, method, scope, upstream, permissions, rate limits, audit), each with its own `op`, `before` and `after`. A service's own fields (`url`, `timeouts`, `max_body_bytes`) show in its `before` and `after`. Identity's and audit's routes change with the kinds, so adding a kind shows their new routes too.
- `summary` counts lines. A `blocked` line counts only in `blocked`.
- Any `blocked` line stops the whole apply.

### Blocked changes

| Change | Blocked while |
|--------|---------------|
| A kind removed | It has nodes |
| A kind's parent changed | It has nodes |

A kind's nodes are every node of it that isn't purged, deleted ones included, so a deleted node stays restorable for its whole grace and its kind waits for the purge. The line lists how many and up to five of their ids. Delete them first through the node routes, each restorable on its own.

The counts come from identity ([`reference/api/internal.md`](../reference/api/internal.md#plan-counts)), asked only when the plan removes a kind or changes a parent. A plan that does neither needs no identity, so a fresh stack's first apply works before identity is up.

Adding a kind in the middle of a populated tree needs a move API, which is deferred ([`TODO.md`](../../TODO.md)).

Everything else is safe:

| Change | Effect |
|--------|--------|
| Kind added | Its nodes can be created at once; identity's and audit's resources give it routes |
| `deletion_grace` changed | Applies to new deletes only: identity stores `purge_after` on each node when it's deleted ([lifecycle](lifecycle.md#deleting-restoring-and-purging)) |
| Role added, changed or removed; a kind added to or dropped from `bindable_at` | Bindings keep the role's name. A role that's gone, or not bindable at that kind, grants nothing, and grants again if a role with that name comes back. So don't reuse a removed role's name for something else |
| Service or route added, changed or removed | A removed service's routes stop matching; its data is the service's |
| CORS, rate limits, `users`, `defaults` or a reserved slug changed | Take effect on the readers' next poll |

A kind renamed reads as a remove plus an add. Tightened rules (a new reserved slug) apply to new values only: a node whose slug became reserved keeps it.

### Preconditions

| Apply sends | Refused when | Response |
|-------------|--------------|----------|
| `expect.revision`, always | The config revision isn't that one: someone applied since the dry run | **409** `PLAN_STALE` |
| — | The recomputed plan has a `blocked` line | **422** |

`details` names the precondition and the current value. Nothing is written; dry-run again. The first apply of a deployment sends `expect.revision: 0`.

## Revisions

Each apply writes one revision: the whole config as sent, in one Postgres transaction, with the plan's summary. An apply whose plan has no changes writes nothing and answers the current revision. Revisions are kept. There's no restore endpoint: rolling back is applying an older revision's config ([CLI](../reference/cli.md)).

The current revision is the **head**. Writes are serialized on it, so head order is commit order, and readers long-poll it.

A fresh deployment has no config: identity and the gateway wait for the first `plat5 apply`, which goes through the same plan as every later change.

## How a change reaches readers

The gateway and identity long-poll the control plane's feeds ([`reference/api/internal.md`](../reference/api/internal.md#control-plane-feeds)), so an apply reaches every replica within about a second. The control plane expands every service's routes, identity's and audit's resources included, before serving them: the gateway parses the feed and builds its route table, nothing more.

| | Gateway | Identity |
|--|--|--|
| Boot | Waits for the first snapshot; retries while the control plane is down. Does not listen until it has one | Same |
| Running | Long-polls; swaps routes, kinds, rate limits and CORS whole on each change | Long-polls; swaps settings, kinds and roles whole on each change |
| Control plane down | Keeps serving the last snapshot | Keeps the last snapshot |
| A snapshot it can't parse or build | Keeps the current one, logs, counts it, and reports the refusal and its reason on its next poll | Keeps the current one, logs, and moves its cursor past it |

Each gateway replica reports the revision it loaded, or the one it refused and why, on every long poll, so the control plane knows where each replica stands and `plat5 apply` can say "gateway: 3/3 replicas on revision 42" ([`reference/api/control-plane.md`](../reference/api/control-plane.md#status)).

A reader never treats "nothing loaded" as an empty state: no routes would answer **404** for everything, and no roles would grant nothing to everyone.

Upgrade the gateway with or before the control plane: a newer control plane can serve a snapshot an older gateway can't build, and that gateway keeps its last snapshot until it's upgraded.
