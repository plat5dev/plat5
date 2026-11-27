# Config

The deployment's desired state: settings, kinds, built-in RBAC (`roles` + `creator_role`) or an external `authorizer`, rate limits, and services, as one config. Each apply submits the whole config to the control plane: a dry run shows the plan, and the apply carries a precondition that makes a stale plan fail. The control plane validates the config as a whole, records it as a revision in Postgres with the bodies it serves, and serves them to the gateway and identity. It is Plat5's only supported write path for any of these.

The runtime reads no config files from disk — it accepts JSON or YAML over the control plane's [admin API](../reference/api/control-plane.md). Teams managing config in Git typically use the [`plat5` CLI](https://github.com/plat5dev/cli) (`plat5.yml` plus one file per service); an API client or control UI can call `POST /apply` directly.

## What it holds

The control plane holds the operator's typed, deployment-wide state that changes while the deployment runs. Everything that belongs to a tree (nodes, members, bindings, custom roles, security policies, keys) stays in identity.

| Document | Read by | Contract |
|----------|---------|----------|
| Settings: `deletion_grace`, `reserved_slugs`, `policy_claims`, `defaults` | identity, gateway | [settings](../reference/config/settings.md) |
| `kinds` | gateway, identity | [kinds](../reference/config/kinds.md) |
| `roles`, `creator_role`, `custom_roles`, `permissions` | identity | [roles](../reference/config/roles.md) |
| `authorizer` | gateway, identity | [authorizer](../reference/config/authorizer.md) |
| `rate_limits` | gateway | [rate limits](../reference/config/rate-limits.md) |
| `services` | gateway | [services](../reference/config/services.md); identity's and audit's [routes](../reference/config/generated-routes.md) |

Each document has a published schema ([below](#schema)), and the [rules at apply](#validation-at-apply) check them together. There is no free-form key. `kinds` is required, and the config must specify either `roles` + `creator_role` (built-in RBAC) or `authorizer` (external AuthZEN PDP) — mutually exclusive. Everything else is optional.

These stay out of it, as process env:

| Kept out | Why |
|----------|-----|
| Wiring and module switches: URLs, ports, `AUDIT_MODE` / `AUDIT_URL`, `RATE_LIMIT_ENABLED` / `VALKEY_URL` | Needed to boot and connect infrastructure, and kept out of the config so `ADMIN_TOKEN` can't turn audit or rate limiting off ([audit](audit.md#on-and-off), [gateway](../reference/gateway.md#rate-limits)) |
| Secrets: `ADMIN_TOKEN`, `UPSTREAM_SIGNING_KEY`, authorizer `token_env` values | Not desired state; never stored in config revisions |
| IdP trust anchors and claim mappings: `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`, `AUTH_SUBJECT_CLAIM`, `AUTH_NAME_CLAIMS`, `AUTH_SCOPE_CLAIM` | Changing the issuer lets you sign in as anyone; `ADMIN_TOKEN` must not be able to |
| `CORS_ALLOWED_ORIGINS`, `CORS_ALLOWED_HEADERS` | Environment-specific browser origins (`localhost` in dev vs production domain); instance facts stay in gateway env |
| `SECRET_BRAND`, `ADMIT_CACHE_TTL`, `ADMIT_NEGATIVE_CACHE_TTL` | Set at boot; changing them while running buys nothing (and changing `SECRET_BRAND` invalidates minted keys) |
| Per-tree data | Identity |

## The config shape

The control plane validates and stores one flat object (`schema/config.json`): top-level settings (`deletion_grace`, `reserved_slugs`, `policy_claims`, `defaults`), `kinds`, either `roles` + `creator_role` (+ optional `custom_roles`, `permissions`) or `authorizer`, optional `rate_limits`, and `services` as a map of service name to service.

```yaml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
kinds: { … }
roles: { … }       # or top-level authorizer: { url: … } (mutually exclusive)
creator_role: admin
rate_limits: { … }
services:
  identity: { … }
  audit: { … }
  api:
    url: http://api:3000
    scopes:
      project:
        upstream_prefix: /projects/{project.id}
        routes: [ … ]
```

**The config is the whole desired state.** Each apply replaces everything: a service missing from `services` is removed, and the plan shows it. There is no per-document or per-service write, because rules tie kinds, roles and routes together.

The config is parsed strictly everywhere: any unknown key is **422** at apply, in a plan, and on readback. A stored source the running version can't read fails loudly rather than silently dropping fields.

Identity's and audit's public routes are ordinary entries under `services` — the control plane adds no hidden routes to them ([reference routes](../reference/config/generated-routes.md)).

## Validation at apply

The control plane is the one validator: it checks the schema, then these rules, on every apply. The gateway and identity trust what it serves and don't check it again. Each error names the field path, such as `services.api.scopes.project.routes[2].path` or `kinds.project.parent`.

1. Every `parent` is a kind, and there are no cycles. Kind names are lowercase identifiers and not `public`, `member` or `user`. `host` is only on root kinds and must be unique across roots.
2. Either `roles` + `creator_role` (built-in RBAC) or `authorizer` (external AuthZEN PDP) is declared, never both (`custom_roles` and `permissions` require built-in RBAC mode). In built-in RBAC mode, every `bindable_at` kind (on roles and `permissions` metadata) exists, every role in `includes` exists without cycles, and `creator_role` is bindable at every kind.
3. Every route's scope is a kind, `user` or `public`, and its path, methods, `permissions`, `resource`, and upstream follow the [route rules](../reference/config/services.md#validation).
4. Route `resource: { type, id }` templates require top-level `authorizer` (and `permissions != "any_member"`), and every `{placeholder}` in `resource.id` must exist in the route's scope/path.
5. No two routes, in one service or across services, have the same method and shape ([route matching](../reference/config/services.md#matching)). No route matches `/access/routes`, where the gateway answers [route checks](../reference/gateway.md#route-checks), in any method.
6. Every policy `defaults.rate_limit` names is in `rate_limits` and has no `count_at`. Every route's `rate_limit` names a policy in `rate_limits`, and a policy's `count_at` is the route's kind or an ancestor.

A role may grant any permission, whether or not a route requires it (a service can check permissions via identity's evaluation API), and a route may require one no role grants (accessible only to `"*"` roles).

A kind with nodes can't be removed or change parent: those are [blocked](#blocked-changes) at plan time. Shape errors (a malformed duration, a reserved slug that isn't a slug, an unknown key) are reported by the [schema](#schema) before these run.

## Schema

One JSON Schema per document, and `schema/config.json` for the whole config, in `schema/` and served at `GET /schema` (no token required). It is the single definition of each document's shape, used by the control plane on every apply and by clients for offline validation.

## Plan and apply

Two calls on one endpoint (`POST /apply`). A dry run computes the plan and writes nothing; an apply recomputes the same plan and writes it only if its precondition holds. The server keeps no stored plans.

```
POST /apply?dry_run=true
{ "kinds": {…}, "roles": {…}, "creator_role": "admin", …, "services": {…} }
→ 200 the plan

POST /apply
{ "kinds": {…}, "roles": {…}, "creator_role": "admin", …, "services": {…},
  "expect": { "revision": 41 } }
→ 200 { "revision": 42, "summary": {…} }
```

A dry run ignores `expect`. The apply recomputes the plan and fails with **409** `PLAN_STALE` if anyone applied since the dry run, so a stale plan never lands.

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
          "after": { "scope": "project", "upstream": "/nodes/{project.id}/children/environment", "permissions": "any_member", "rate_limit": ["standard"] } }
      ] }
  ]
}
```

Changes are ordered kinds, settings, roles, authorizer, rate limits, services, then by name.

- `revision` is the config revision the plan was made against, `0` before the first apply. The apply sends it back as `expect.revision`.
- `what` is `kind`, `setting` (`deletion_grace`, `reserved_slugs`, `policy_claims`, `defaults`, `creator_role`, `custom_roles`, or `permissions`), `role`, `authorizer`, `rate_limit`, or `service`. `op` is `add`, `change` or `remove`.
- `before` and `after` show what changed, field by field: `before` comes from what readers are served now (the head's stored bodies); `after` is the new config expanded by the running control plane. After an upgrade, a plan of unchanged config shows exactly what the new version would serve differently ([upgrading](#upgrading)).
- A service line lists its `routes`, expanded as the gateway will match them, each with its own `op`, `before` and `after`.
- `summary` counts lines. A `blocked` line counts only in `blocked`, and any `blocked` line stops the whole apply.

### Blocked changes

| Change | Blocked while |
|--------|---------------|
| A kind removed | It has nodes |
| A kind's parent changed | It has nodes |

A kind's nodes are every node of it that isn't purged, deleted ones included, so a deleted node stays restorable for its whole grace and its kind waits for the purge. The line lists how many and up to five of their ids. Delete them first through the node routes, each restorable on its own.

The counts come from identity ([`reference/api/internal.md`](../reference/api/internal.md#plan-counts)), asked only when the plan removes a kind or changes a parent. A plan that does neither never calls identity, so a fresh stack's first apply works before identity is up.

Everything else is safe:

| Change | Effect |
|--------|--------|
| Kind added | Its nodes can be created at once, through the routes the config gives it |
| `deletion_grace` changed | Applies to new deletes only: identity stores `purge_after` on each node when it's deleted ([lifecycle](lifecycle.md#deleting-restoring-and-purging)) |
| Role added, changed or removed; a kind added to or dropped from `bindable_at` | Bindings keep the role's name. A role that's gone, or not bindable at that kind, grants nothing, and grants again if a role with that name comes back. Don't reuse a removed role's name for something else |
| Service or route added, changed or removed | A removed service's routes stop matching; its data stays in the service |
| `defaults` or a reserved slug changed | Take effect on the readers' next poll |

A kind renamed reads as a remove plus an add. Tightened rules (a new reserved slug) apply to new values only: a node whose slug became reserved keeps it.

### Preconditions

| Apply sends | Refused when | Response |
|-------------|--------------|----------|
| `expect.revision`, always | The config revision isn't that one: someone applied since the dry run | **409** `PLAN_STALE` |
| — | The recomputed plan has a `blocked` line | **422** |

`details` names the precondition and the current value. Nothing is written; dry-run again. The first apply of a deployment sends `expect.revision: 0`.

## Revisions

Each apply writes one revision, in one Postgres transaction, with the plan's summary. A revision stores:

- **the source**: the whole config as sent, for readback and rollback;
- **the served bodies**: the gateway's feed and identity's feed, expanded, with defaults filled in.

A revision means one thing on every reader: the feeds serve its stored bodies and never expand them again, so a control-plane upgrade never silently alters what gateways serve for an existing revision without a plan. An apply whose plan has no changes writes nothing and answers the current revision. Revisions are kept. There is no restore endpoint: rolling back is fetching an older revision (`GET /revisions/{rev}`) and applying it through the same plan.

The current revision is the **head**. Writes are serialized on it, so head order is commit order, and readers long-poll it.

A fresh deployment has no config: identity and the gateway wait for the first apply, which goes through the same plan as every later change.

## How a change reaches readers

The gateway and identity long-poll the control plane's feeds ([`reference/api/internal.md`](../reference/api/internal.md#control-plane-feeds)), so an apply reaches every replica within about a second. Each feed serves the head's stored body: the control plane expanded every service's routes at apply, and the gateway parses the feed and builds its route table, nothing more.

| | Gateway | Identity |
|--|--|--|
| Boot | Waits for the first snapshot; retries while the control plane is down. Does not listen until it has one | Same |
| Running | Long-polls; swaps routes, kinds, and rate limits whole on each change | Long-polls; swaps settings, kinds and roles whole on each change |
| Control plane down | Keeps serving the last snapshot | Keeps serving the last snapshot |
| A snapshot it can't parse or build | Keeps the current one, logs, counts it, and reports the refusal and its reason on its next poll | Keeps the current one, logs, and moves its cursor past it |

Each gateway replica reports the revision it loaded, or the one it refused and why, on every long poll, so the control plane knows where each replica stands ([`reference/api/control-plane.md`](../reference/api/control-plane.md#status)).

A reader never treats "nothing loaded" as an empty state: no routes would answer **404** for everything, and no roles would grant nothing to everyone.

## Upgrading

Upgrade the gateway with or before the control plane, then plan and apply. A new control plane serves the stored bodies as they are, so nothing changes for readers until then. A plan on unchanged config shows what the new version expands differently; an empty plan writes nothing. A body a newer control plane writes can be one an older gateway can't build: that gateway refuses it, keeps its last snapshot, and reports why, until it's upgraded. Steps: [deploy](../guides/deploy.md#pins).
