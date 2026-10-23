# Control plane

The deployment's desired state: kinds, roles, routes, and CORS origins. The operator writes it through an admin HTTP API. Every write is planned first; the control plane validates the documents together, records them with revisions in Postgres, and serves the current state to the gateway and identity. It is Plat5's only supported write path for any of these.

## What it holds

The control plane holds the operator's typed, deployment-wide state that changes while the deployment runs. Everything that belongs to a tree (nodes, members, bindings, keys) stays in identity.

| Document | Read by | Contract |
|----------|---------|----------|
| `kinds` | gateway, identity | [below](#kinds) |
| `roles` | identity | [`roles.md`](roles.md) |
| Services (routes, named rate limits, upstream timeouts) | gateway, identity | [`routes.md`](routes.md) |
| `cors` | gateway | [below](#cors) |

Each document has its own schema and its own validation, and the [cross-document rules](#validation-at-apply) check them together. There is no free-form key.

These stay out of it, as process env:

| Kept out | Why |
|----------|-----|
| Wiring: URLs, ports | Needed to reach the control plane in the first place |
| Secrets: `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN` | Not desired state |
| Trust anchors: `AUTH_ISSUER`, `AUTH_JWKS_URI`, audiences, `AUTH_SUBJECT_CLAIM` | Changing the issuer lets you sign in as anyone; `ADMIN_TOKEN` must not be able to |
| `AUDIT_ENABLED` | Turning audit on or off changes what the stored log means. A boot switch |
| `APIKEY_BRAND`, `APIKEY_CACHE_TTL_SECS`, gateway rate-limit fallback | Set at boot; changing them while running buys nothing |
| Per-tree data | Identity |

## Pipeline

```
kinds.yml  ┐
roles.yml  ├→ PUT /{document} or POST /apply → plan → confirm → Postgres schema `control_plane` (revision, head)
routes.yml │                                                     ├→ GET /internal/gateway   ← gateway (long-poll)
cors       ┘                                                     └→ GET /internal/identity  ← identity (long-poll)
```

Postgres is the source of truth, and the only store. Every write bumps one revision for the whole control plane (the **head**) in the same transaction as the change. Writes are serialized on it, so head order is commit order. Readers long-poll the head.

## Kinds

The tree's levels. Every node in identity is one of these kinds.

```yaml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin, me]
kinds:
  directory:
    sign_in: idp
    name_claim: name
  organization:
    label: Organization
    segment: orgs
  project:
    label: Project
    parent: organization
```

| Field | Rule |
|-------|------|
| kind name | Lowercase identifier, `[a-z][a-z0-9_]*`, max 32. Not `public`. Names the kind in route scopes, path params, subject fields (`{subject.project_id}`), headers (`X-Plat5-Project-Id`), events (`project.deleted`) and audit. Fixed once nodes exist: a rename reads as a remove plus an add |
| `parent` | The kind, or list of kinds, a node sits under. Omitted for a root |
| `label` | What consoles show. Changes any time |
| `sign_in` | `idp`. Marks the one kind people sign in to: a single-node root (the directory), created at boot. Exactly one kind has it |
| `name_claim` | On the sign-in kind only. The IdP claim holding a person's name. Default `name` |
| `segment` | The path segment for generated routes. Default: the kind name plus `s` |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for every kind. Default `30d` |

Slug shape, uniqueness and reuse are fixed rules, not config ([`identity.md`](identity.md#slugs)).

## Validation at apply

The control plane refuses a document set that breaks any of these; the error names the field. They check `kinds`, `roles` and services together, so changing kinds and routes is one plan.

1. Exactly one kind has `sign_in`; it has no parent and nothing names it as parent.
2. Every `parent` exists; no cycles; kind names are lowercase identifiers and not `public`.
3. Every `bindable_at` kind exists and isn't the sign-in kind.
4. `default_role` is bindable at every root kind other than the sign-in kind; `creator_role` at every kind that can be created.
5. Every route has a `scope`; its path holds exactly the kind params for its chain; its `upstream` uses only subject fields its scope provides.
6. Every route scoped to a kind other than the sign-in kind states `required_labels` for every method. `[]` is allowed and means any member of the tree. On `public` and sign-in routes it's omitted or `[]`.
7. Every `rate_limit` name exists on the service, and its `count_at` is the route's kind or an ancestor.

Shape errors (a malformed duration, a reserved slug that isn't a slug) are reported by schema validation before these run. Route shape rules: [`routes.md`](routes.md#validation).

## The plan

Every write runs as a plan first. Without a `plan_id`, the control plane returns the plan and changes nothing. Sent again with that `plan_id`, and a `confirm` naming every kind the plan marks destructive, it applies, unless the plan has gone stale.

```
PUT /kinds           { "document": {...} }                                    → the plan, nothing applied
PUT /kinds           { "document": {...}, "plan_id": "pl_…", "confirm": ["folder"] }
PUT /roles           { "document": {...}, "plan_id": "pl_…" }
PUT /services/{name} { "document": {...}, "plan_id": "pl_…" }
POST /apply          { "kinds": {...}, "roles": {...}, "services": {...}, "plan_id": "pl_…", "confirm": [...] }
```

`plat5 apply` shows the plan and asks for confirmation; `plat5 apply --plan` only prints it, for CI.

```json
{
  "plan_id": "pl_…",
  "revision_from": 41,
  "identity_revision": 9813,
  "changes": [
    { "class": "safe",        "what": "kind.added",   "kind": "environment" },
    { "class": "blocked",     "what": "kind.parent_narrowed", "kind": "project", "in_the_way": { "nodes": 3, "sample": ["…"] } },
    { "class": "destructive", "what": "kind.removed", "kind": "folder",
      "effects": { "nodes": 14, "bindings": 22, "service_accounts": 3, "keys": 9, "routes": 4 } }
  ],
  "applies": false
}
```

Any `blocked` change stops the whole apply. The counts come from identity's internal plan call ([`identity.md`](identity.md#plan-counts)).

**Stale plans are refused, as Terraform refuses a stale saved plan.** A plan records the config revision it was made against (`revision_from`) and identity's data revision (`identity_revision`), a counter bumped by any change to nodes, members or bindings. Apply returns **409** `PLAN_STALE` and changes nothing when the config revision has moved, or when the plan has a destructive change and the identity revision has moved. Re-plan and confirm again.

### Classification

| Change | Class |
|--------|-------|
| Kind added at the bottom; parents widened; `label` or `segment` changed; reserved slug added | Safe |
| Role added, changed or removed; a kind added to or dropped from `bindable_at` | Safe. Bindings keep the role's name; a role that's gone, or not bindable at that kind, grants nothing until it's back, as with a deleted Google Cloud custom role |
| Route added, changed or removed | Safe for Plat5 |
| Parents narrowed with nodes under the removed parent | Blocked |
| Kind removed | Destructive: its nodes and descendants are soft-deleted |
| Kind renamed | Invalid: it reads as a remove plus an add |

Removing a kind is the only destructive change. It soft-deletes through the normal node lifecycle, so it emits the same events and is restorable within the grace period by re-applying the kind. Tightened rules (a shorter `max_duration`, a new reserved slug) apply to new values only, as Kubernetes validation ratcheting does.

Adding a kind in the middle (a `folder` between org and project) is two safe applies: add `folder` under `organization`, then widen `project`'s parent to `[organization, folder]`.

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5402` |
| Internal (feeds, health, metrics) | container `:5003` |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes require:

```
Authorization: Bearer <ADMIN_TOKEN>
```

Local default: `dev-admin-token` (`ADMIN_TOKEN` env). Required and non-empty in all environments.

`/internal/*` on `INTERNAL_PORT` requires `X-Plat5-Internal-Token` when `INTERNAL_AUTH_TOKEN` is set, the same shared token the gateway sends identity and audit.

`ADMIN_TOKEN` can route traffic, change what every role grants, and remove a kind with its nodes. Treat it as the deployment's root credential. The revisions below are its history.

## API

### Services

| Method | Path | Body | Notes |
|--------|------|------|--------|
| `POST` | `/apply` | `{ kinds?, roles?, cors?, services?, plan_id?, confirm? }`, JSON or YAML | Plans or applies every document in the body together, one PG transaction |
| `GET` | `/services` | — | Current (non-deleted) services |
| `GET` | `/services/{name}` | — | Current config plus `name` / `revision` |
| `PUT` | `/services/{name}` | `{ document, plan_id? }` | Plan, or new revision |
| `DELETE` | `/services/{name}` | `{ plan_id? }` | Plan, or tombstone revision |
| `GET` | `/services/{name}/revisions` | — | History (includes delete revisions) |
| `GET` | `/services/{name}/revisions/{rev}` | — | One revision (`config` is `null` if delete) |
| `POST` | `/services/{name}/revisions/{rev}/restore` | `{ plan_id? }` | Plan, or new revision copying that config |

Services in `/apply` are an **upsert**: services not in the body are left alone.

Named `rate_limits` live on each service. Apply and `PUT /services/{name}` validate `shared: true` policy names against **all** current services, not only the payload. Schema: [`routes.md`](routes.md).

### Generated catalog

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/kinds/catalog` | The identity routes generated from the current `kinds` document, as a `routes.yml` ([`identity.md`](identity.md#generated-routes)). Not applied |

`plat5 routes generate` writes it to a file. The team edits it like any routes file (renames labels, drops routes) and applies it. Omitting a route hides it from the gateway; the identity process still serves that path on the network.

### Documents

`kinds`, `roles` and `cors` share one shape of API. `{document}` is one of those names.

| Method | Path | Body | Notes |
|--------|------|------|--------|
| `GET` | `/{document}` | — | Current document plus `revision`. **404** if never written |
| `PUT` | `/{document}` | `{ document, plan_id?, confirm? }`, JSON or YAML | Plan, or new revision |
| `GET` | `/{document}/revisions` | — | History, newest first |
| `GET` | `/{document}/revisions/{rev}` | — | One revision |
| `POST` | `/{document}/revisions/{rev}/restore` | `{ plan_id?, confirm? }` | Plan, or new revision copying that document, validated as if written now |

A document has no delete. `PUT` replaces the whole document. A body that fails validation is **422** `VALIDATION_ERROR` and writes nothing; an unknown key is **422**.

Revision responses:

```json
{ "document": "roles", "revision": 2, "config": { … }, "request_id": "…", "created_at": "…" }
```

#### CORS

```json
{ "allowed_origins": ["https://app.example.com", "http://localhost:5173"] }
```

| Field | Rule |
|-------|------|
| `allowed_origins` | Up to 100. Each is `https://host` or `http://host`, with an optional `:port`. Lowercase, no path, no trailing `/`, no wildcard, no repeats: the gateway compares it byte for byte with the browser's `Origin` |

An empty list, or no `cors` document, answers CORS with `*`. Otherwise the gateway echoes an origin in the list and sends no `Access-Control-Allow-Origin` for any other. Request-time behavior: [`gateway-contract.md`](gateway-contract.md#cors).

### Response shape

A plan is **200** with the plan body and `"applies": false`. An apply is **200**:

```json
{
  "plan_id": "pl_…",
  "applies": true,
  "revision": 42,
  "results": [ { "document": "kinds", "revision": 3 }, { "service": "e10s", "status": "upserted", "revision": 7 } ]
}
```

Validation, auth and empty-body failures use the Plat5 envelope ([`api-errors.md`](api-errors.md)) and write nothing. A method + path shape already owned by a **different** service rejects the whole apply with **409** `ROUTE_CONFLICT` ([`routes.md`](routes.md)). A stale plan is **409** `PLAN_STALE`. A destructive change without its kind in `confirm` is **422** naming the kinds. Identity unreachable while planning is **503**: the plan needs its counts. A Postgres failure rolls the whole batch back (**503**). `200` means readers will see it on their next poll.

## Revisions

Each write (apply, put, delete, restore) appends a revision. Restore appends a new revision that copies an old config, planned and validated like any write: an old revision that now breaks a rule is **422**, one whose routes another service owns now is **409** `ROUTE_CONFLICT`, and one that removes a kind is destructive.

Delete stores `config: null`. History remains. Restore of a delete revision is **422**.

A service's or a document's `revision` counts its own writes. The head counts every write, and is what readers poll.

## Internal feeds

On `INTERNAL_PORT`. Each is one read, at one head, in one Postgres snapshot.

| Path | Reader | `200` body |
|------|--------|------------|
| `GET /internal/gateway` | gateway | `{ "revision", "kinds", "services": { name → ServiceConfig }, "cors": { "allowed_origins": [] } }` |
| `GET /internal/identity` | identity | `{ "revision", "kinds", "roles", "services" }`. **404** while there is no `kinds` or `roles` document |

`revision` is the head. `services` is in the stored form: full paths, list-form `methods` (`route_prefix` and nested `methods` maps already expanded). `cors` is the empty list when there is no document. Identity reads `services` to find the labels of its own list routes ([`identity.md`](identity.md#filtered-lists)).

Without `after`, the answer is immediate. With `?after={revision}`, the control plane waits until the head is past `after`, for up to 25 seconds. Nothing newer in that time is **204**, and the reader asks again. A cursor ahead of the head (the store was reset) is answered at once.

Feed reads stay off traces and request metrics, like health checks. A 5xx is still logged.

Any write wakes every reader. Each control-plane process polls the head in Postgres every second, so a write through any replica reaches readers of every replica within about a second.

### Readers

| | Gateway | Identity |
|--|--|--|
| Boot | Waits for the first snapshot; retries while the control plane is down. Does not listen until it has one | Waits for `kinds` and `roles`; retries while the control plane is down or has none. Does not listen until it has them, then creates the sign-in node if it doesn't exist |
| Running | Long-polls; swaps routes, kinds and CORS whole on each change | Long-polls; swaps kinds, roles and services whole on each change |
| Control plane down | Keeps serving the last snapshot | Keeps the last documents |
| A snapshot it cannot load | Keeps the current one, logs, and moves its cursor past it | Same |

A reader never treats "nothing loaded" as an empty state: no routes would answer **404** for everything, and no roles would grant nothing to everyone.

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Feeds, health, metrics |
| `DATABASE_URL` | required | Postgres; schema `control_plane` |
| `ADMIN_TOKEN` | required | Bearer token |
| `INTERNAL_AUTH_TOKEN` | empty | Required on `/internal/*` when set; sent to identity |
| `IDENTITY_INTERNAL_URL` | required | Identity's internal base URL, for plan counts, e.g. `http://identity:3001` |
| `SEED_DIR` | empty | On first boot, apply `kinds.yml`, `roles.yml` and every routes file here when there has never been a write. Never read again |

Ready is **503** unless Postgres answers.

## Seeds

Dev compose mounts `compose/seed/` (kinds, roles, and the generated identity and audit catalogs) and sets `SEED_DIR`. Prod compose mounts a folder with `kinds.yml` and `roles.yml` only, so identity can start; apply routes yourself. The seed apply is planned and applied in one step; it's never destructive, since there is nothing to remove. Once anything has been written, the folder is never read again: change the deployment with `plat5 apply`.

## Validation code

Route types and validation live in each of gateway and control-plane (`src/route_config.rs`). Keep them aligned deliberately; the stored JSON and [`routes.md`](routes.md) are the contract. Expand (`route_prefix` and nested `methods` maps) happens at write time only.

Kinds and roles validation live in control-plane (`src/documents.rs`) and identity. Identity checks again on receipt, so a document set it cannot use never replaces the one it has.

## Related

- [`routes.md`](routes.md): route format and scopes
- [`roles.md`](roles.md): roles and bindings
- [`identity.md`](identity.md): nodes, members, the generated catalog
- [`gateway-contract.md`](gateway-contract.md): request-time behavior
