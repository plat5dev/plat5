# Config

The deployment's desired state: kinds, roles, routes, and CORS origins, as one config. The operator applies the whole config at once, Terraform style: a dry run shows the plan, and the apply carries preconditions that make a stale plan fail. The control plane validates the config as a whole, records it as a revision in Postgres, and serves it to the gateway and identity. It is Plat5's only supported write path for any of these.

## What it holds

The control plane holds the operator's typed, deployment-wide state that changes while the deployment runs. Everything that belongs to a tree (nodes, members, bindings, keys) stays in identity.

| Document | Read by | Contract |
|----------|---------|----------|
| `kinds` | gateway, identity | [reference](../reference/config/kinds.md) |
| `roles` | identity | [`concepts/access.md`](access.md) |
| `services` (routes, named rate limits, upstream timeouts) | gateway, identity | [`reference/config/routes.md`](../reference/config/routes.md) |
| `cors` | gateway | [reference](../reference/config/cors.md) |

Each document has a published schema ([below](#schema)), and the [cross-document rules](#validation-at-apply) check them together. There is no free-form key.

These stay out of it, as process env:

| Kept out | Why |
|----------|-----|
| Wiring: URLs, ports | Needed to reach the control plane in the first place |
| Secrets: `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN` | Not desired state |
| Trust anchors: `AUTH_ISSUER`, `AUTH_JWKS_URI`, audiences, `AUTH_SUBJECT_CLAIM` | Changing the issuer lets you sign in as anyone; `ADMIN_TOKEN` must not be able to |
| `AUDIT_ENABLED` | Turning audit on or off changes what the stored log means. A boot switch |
| `APIKEY_BRAND`, `APIKEY_CACHE_TTL_SECS`, gateway rate-limit fallback | Set at boot; changing them while running buys nothing |
| Per-tree data | Identity |

## The config

A directory the team keeps in a repo. Every file starts with `version: 2` and holds one or more documents; the CLI reads them all into one config.

```
plat5/
  kinds.yml        version: 2, kinds: …
  roles.yml        version: 2, roles: …
  cors.yml         version: 2, cors: …
  catalog.yml      version: 2, services: { identity: …, audit: … }   # generated, then edited
  e10s.yml         version: 2, services: { e10s: … }
```

**The config is the whole desired state.** Each apply replaces everything: a service missing from the config is removed, and the plan shows it. There is no per-document or per-service write. Two files defining the same service, or the same document twice, is **422**.

`version` is the config format. A future format is a migration the CLI and control plane run, not a hard cutover; a file with a `version` the control plane doesn't know is **422**.

The documents' fields: [kinds](../reference/config/kinds.md), [roles](../reference/config/roles.md), [routes](../reference/config/routes.md), [cors](../reference/config/cors.md). The admin API: [`reference/api/control-plane.md`](../reference/api/control-plane.md). The CLI: [`reference/cli.md`](../reference/cli.md).

## Validation at apply

The control plane refuses a config that breaks any of these; the error names the file and field. They check `kinds`, `roles` and `services` together, which is why the config is applied as a whole.

1. Exactly one kind has `sign_in`; it has no parent and nothing names it as parent.
2. Every `parent` exists; no cycles; kind names are lowercase identifiers and not `public`.
3. Every `bindable_at` kind exists and isn't the sign-in kind.
4. `default_role` is bindable at every root kind other than the sign-in kind; `creator_role` at every kind that can be created. Neither has a `max_duration`.
5. Every route has a `scope`; its path holds exactly the kind params for its chain; its `upstream` uses only subject fields its scope provides.
6. Every route scoped to a kind other than the sign-in kind states `required_labels` for every method. `[]` is allowed and means any member of the tree. On `public` and sign-in routes it's omitted or `[]`.
7. Every `rate_limit` name exists on the service, and its `count_at` is the route's kind or an ancestor.

Shape errors (a malformed duration, a reserved slug that isn't a slug, an unknown key) are reported by the [schema](#schema) before these run. Route shape and ownership rules: [`reference/config/routes.md`](../reference/config/routes.md#validation).

## Schema

One JSON Schema per document, and one for the whole config, in the repo at `schema/v2/` and served at `GET /schema`. It is the single definition of each document's shape:

- The CLI validates locally with it (`plat5 validate`), so editors and CI catch shape errors without a deployment.
- The control plane validates every apply with it before the cross-document rules.
- The gateway and identity check each feed snapshot against it, and keep the current one if it fails.

The schema covers shape only. The seven rules above, and route ownership, are code in the control plane.

## Plan and apply

Two calls on one endpoint. A dry run computes the plan and writes nothing; an apply recomputes the same plan and writes it only if its preconditions hold. The server keeps no plans.

```
POST /apply?dry_run=true
{ "version": 2, "kinds": {…}, "roles": {…}, "cors": {…}, "services": {…} }
→ 200 the plan

POST /apply
{ "version": 2, "kinds": {…}, "roles": {…}, "cors": {…}, "services": {…},
  "expect":  { "revision": 41 },
  "confirm": { "folder": { "nodes": 14 } } }
→ 200 { "revision": 42 }
```

This is Kubernetes' `dryRun=All` plus a `resourceVersion` precondition, with Terraform's rule that a stale plan is refused.

### The plan

```json
{
  "revision": 41,
  "summary": { "add": 2, "change": 1, "remove": 1 },
  "changes": [
    { "class": "safe",        "op": "add",    "what": "kind",    "name": "environment" },
    { "class": "safe",        "op": "change", "what": "service", "name": "e10s", "routes": { "add": 1, "change": 0, "remove": 0 } },
    { "class": "blocked",     "op": "change", "what": "kind",    "name": "project", "reason": "parent_narrowed",
      "in_the_way": { "nodes": 3, "sample": ["n_…"] } },
    { "class": "destructive", "op": "remove", "what": "kind",    "name": "folder",
      "effects": { "nodes": 14, "bindings": 22, "service_accounts": 3, "keys": 9 } }
  ],
  "confirm": { "folder": { "nodes": 14 } }
}
```

- `revision` is the config revision the plan was made against. The apply sends it back as `expect.revision`.
- `confirm` is what the apply must send to allow the destructive changes: each removed kind with the number of nodes it will delete. `{}` when nothing is destructive.
- Any `blocked` change stops the whole apply. The counts come from identity ([`reference/api/internal.md`](../reference/api/internal.md#plan-counts)).

### Preconditions

| Apply sends | Refused when | Response |
|-------------|--------------|----------|
| `expect.revision`, always | The config revision isn't that one: someone applied since the dry run | **409** `PLAN_STALE` |
| `confirm`, when the plan is destructive | A destructive kind is missing from `confirm`, or its node count now differs | **409** `PLAN_STALE` |
| — | The recomputed plan has a `blocked` change | **422** |

`details` names which precondition failed and the current value. Nothing is written; dry-run again and confirm again. The first apply of a deployment sends `expect.revision: 0`.

Confirming counts rather than a data revision means ordinary traffic (new projects, new bindings) doesn't make a plan stale. Only the thing you agreed to destroy changing does.

### Classification

| Change | Class |
|--------|-------|
| Kind added at the bottom; parents widened; `label` or `segment` changed; reserved slug added | Safe |
| Role added, changed or removed; a kind added to or dropped from `bindable_at` | Safe. Bindings keep the role's name; a role that's gone, or not bindable at that kind, grants nothing until it's back, as with a deleted Google Cloud custom role |
| Service or route added, changed or removed | Safe for Plat5. A removed service's routes stop matching; its data is the service's |
| CORS changed | Safe |
| Parents narrowed with nodes under the removed parent | Blocked |
| Kind removed | Destructive: its nodes and descendants are soft-deleted. Routes scoped to it, or with its param in their chain, must go in the same apply (rule 5), and show as route removals |
| Kind renamed | Invalid: it reads as a remove plus an add |

Removing a kind is the only destructive change. What happens to its nodes, and how applying it again restores them: [lifecycle](lifecycle.md#removing-a-kind). Tightened rules (a shorter `max_duration`, a new reserved slug) apply to new values only, as Kubernetes validation ratcheting does.

Adding a kind in the middle (a `folder` between org and project) is two safe applies: add `folder` under `organization`, then widen `project`'s parent to `[organization, folder]`.

## Revisions

Each apply writes one revision: the whole config, in one Postgres transaction, with the plan's summary. Revisions are kept. There's no restore endpoint: rolling back is applying an older revision's config ([CLI](../reference/cli.md#cli)).

The current revision is the **head**. Writes are serialized on it, so head order is commit order, and readers long-poll it.

## How a change reaches readers

The gateway and identity long-poll the control plane's feeds ([`reference/api/internal.md`](../reference/api/internal.md#control-plane-feeds)), so an apply reaches every replica within about a second.

| | Gateway | Identity |
|--|--|--|
| Boot | Waits for the first snapshot; retries while the control plane is down. Does not listen until it has one | Waits for the first snapshot; retries. Does not listen until it has one, then creates the sign-in node if it doesn't exist |
| Running | Long-polls; swaps routes, kinds and CORS whole on each change | Long-polls; swaps kinds, roles and services whole on each change |
| Control plane down | Keeps serving the last snapshot | Keeps the last snapshot |
| A snapshot that fails the schema | Keeps the current one, logs, and moves its cursor past it | Same |

A reader never treats "nothing loaded" as an empty state: no routes would answer **404** for everything, and no roles would grant nothing to everyone.
