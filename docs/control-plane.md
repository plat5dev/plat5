# Control plane

The deployment's desired state: kinds, roles, routes, and CORS origins, as one config. The operator applies the whole config at once, Terraform style: a dry run shows the plan, and the apply carries preconditions that make a stale plan fail. The control plane validates the config as a whole, records it as a revision in Postgres, and serves it to the gateway and identity. It is Plat5's only supported write path for any of these.

## What it holds

The control plane holds the operator's typed, deployment-wide state that changes while the deployment runs. Everything that belongs to a tree (nodes, members, bindings, keys) stays in identity.

| Document | Read by | Contract |
|----------|---------|----------|
| `kinds` | gateway, identity | [below](#kinds) |
| `roles` | identity | [`roles.md`](roles.md) |
| `services` (routes, named rate limits, upstream timeouts) | gateway, identity | [`routes.md`](routes.md) |
| `cors` | gateway | [below](#cors) |

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

## Kinds

The tree's levels. Every node in identity is one of these kinds.

```yaml
version: 2
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

The control plane refuses a config that breaks any of these; the error names the file and field. They check `kinds`, `roles` and `services` together, which is why the config is applied as a whole.

1. Exactly one kind has `sign_in`; it has no parent and nothing names it as parent.
2. Every `parent` exists; no cycles; kind names are lowercase identifiers and not `public`.
3. Every `bindable_at` kind exists and isn't the sign-in kind.
4. `default_role` is bindable at every root kind other than the sign-in kind; `creator_role` at every kind that can be created. Neither has a `max_duration`.
5. Every route has a `scope`; its path holds exactly the kind params for its chain; its `upstream` uses only subject fields its scope provides.
6. Every route scoped to a kind other than the sign-in kind states `required_labels` for every method. `[]` is allowed and means any member of the tree. On `public` and sign-in routes it's omitted or `[]`.
7. Every `rate_limit` name exists on the service, and its `count_at` is the route's kind or an ancestor.

Shape errors (a malformed duration, a reserved slug that isn't a slug, an unknown key) are reported by the [schema](#schema) before these run. Route shape and ownership rules: [`routes.md`](routes.md#validation).

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
- Any `blocked` change stops the whole apply. The counts come from identity ([`identity.md`](identity.md#plan-counts)).

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

Removing a kind is the only destructive change. Identity soft-deletes its nodes when the new config reaches it, through the normal node lifecycle, so the same events are emitted. Applying the kind again within the grace period restores the nodes that removal deleted (not ones deleted separately before it), with `<kind>.restored` events; after it, the kind comes back empty. Tightened rules (a shorter `max_duration`, a new reserved slug) apply to new values only, as Kubernetes validation ratcheting does.

Adding a kind in the middle (a `folder` between org and project) is two safe applies: add `folder` under `organization`, then widen `project`'s parent to `[organization, folder]`.

## CLI

| Command | Does |
|---------|------|
| `plat5 validate [dir]` | Schema checks, offline |
| `plat5 plan [dir]` | Dry run; prints the diff |
| `plat5 plan [dir] -out plan.json` | Also saves the plan: the config, `expect` and `confirm` together |
| `plat5 apply [dir]` | Dry run, show the diff, ask to confirm, apply |
| `plat5 apply plan.json` | Apply a saved plan as it is. No prompt: saving it was the review |
| `plat5 apply [dir] --auto-approve` | Apply without the prompt. Refused when the plan is destructive, unless `--confirm folder=14` |
| `plat5 routes generate [dir]` | Write `catalog.yml` from the local `kinds.yml` ([generated catalog](#generated-catalog)) |
| `plat5 config pull [--revision n] [dir]` | Write an applied revision out as a config directory |

The diff is Terraform's: `+` add, `~` change, `-` remove, each destructive line marked, then `Plan: 2 to add, 1 to change, 1 to remove.` Blocked changes print with what's in the way.

`plat5 plan --detailed-exitcode` exits `0` with no changes, `2` with changes, `1` on an error. A scheduled CI job running it against the repo's config shows drift: a deployment that no longer matches the repo.

**Rolling back** is applying an older config: `plat5 config pull --revision 38`, then `plat5 apply`. It goes through the same plan, so a rollback that removes a kind is destructive like any other.

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5402` |
| Internal (feeds, health, metrics) | container `:5003` |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes require `Authorization: Bearer <ADMIN_TOKEN>`. Local default: `dev-admin-token`. Required and non-empty in all environments.

`/internal/*` on `INTERNAL_PORT` requires `X-Plat5-Internal-Token` when `INTERNAL_AUTH_TOKEN` is set, the same shared token the gateway sends identity and audit.

`ADMIN_TOKEN` can route traffic, change what every role grants, and remove a kind with its nodes. Treat it as the deployment's root credential. The revisions below are its history.

## API

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/apply?dry_run=true` | The plan. Writes nothing |
| `POST` | `/apply` | Apply, with `expect` and `confirm`. JSON or YAML |
| `GET` | `/config` | The current config and its `revision` |
| `GET` | `/revisions` | History, newest first: `revision`, `created_at`, `request_id`, and the plan's `summary` |
| `GET` | `/revisions/{rev}` | The config at that revision |
| `POST` | `/catalog` | `{ version, kinds }` → the generated identity and audit routes for those kinds, as `services`. Writes nothing |
| `GET` | `/schema` | The JSON Schema for the config and each document |

### Generated catalog

Identity's edge routes, and the audit service's read routes, are generated from `kinds` ([`identity.md`](identity.md#generated-routes)). `POST /catalog` takes the kinds in the body, so the catalog for a new kind can be generated before that kind is applied, and both go in one apply. `plat5 routes generate` writes it to `catalog.yml`. The team edits it like any routes file (renames labels, drops routes) and keeps it in the config. Omitting a route hides it from the gateway; the identity process still serves that path on the network.

### CORS


```yaml
version: 2
cors:
  allowed_origins: ["https://app.example.com", "http://localhost:5173"]
```

| Field | Rule |
|-------|------|
| `allowed_origins` | Up to 100. Each is `https://host` or `http://host`, with an optional `:port`. Lowercase, no path, no trailing `/`, no wildcard, no repeats: the gateway compares it byte for byte with the browser's `Origin` |

An empty list, or no `cors` in the config, answers CORS with `*`. Otherwise the gateway echoes an origin in the list and sends no `Access-Control-Allow-Origin` for any other. Request-time behavior: [`gateway-contract.md`](gateway-contract.md#cors).

### Responses

Validation, auth and empty-body failures use the Plat5 envelope ([`api-errors.md`](api-errors.md)) and write nothing.

| Case | Status |
|------|--------|
| Dry run | **200**, the plan |
| Applied | **200** `{ "revision": 42, "summary": {…} }`. Readers see it on their next poll |
| Schema or cross-document rule broken, unknown `version`, blocked change | **422** `VALIDATION_ERROR`, naming the file and field |
| A method + path shape owned by two services | **409** `ROUTE_CONFLICT` ([`routes.md`](routes.md#route-ownership)) |
| A precondition failed | **409** `PLAN_STALE` |
| `expect` missing | **422** |
| Identity unreachable while computing the plan | **503**: the plan needs its counts |
| Postgres failure | **503**. Nothing written |

## Revisions

Each apply writes one revision: the whole config, in one Postgres transaction, with the plan's summary. Revisions are kept. There's no restore endpoint: rolling back is applying an older revision's config ([CLI](#cli)).

The current revision is the **head**. Writes are serialized on it, so head order is commit order, and readers long-poll it.

## Internal feeds

On `INTERNAL_PORT`. Each is one read, at one head, in one Postgres snapshot.

| Path | Reader | `200` body |
|------|--------|------------|
| `GET /internal/gateway` | gateway | `{ "revision", "kinds", "services", "cors" }` |
| `GET /internal/identity` | identity | `{ "revision", "kinds", "roles", "services" }`. **404** before the first apply |

`services` is in the stored form: full paths, list-form `methods` (`route_prefix` and nested `methods` maps already expanded). `cors` is the empty list when the config has none. Identity reads `services` to find the labels of its own read routes ([`identity.md`](identity.md#filtered-lists)).

Without `after`, the answer is immediate. With `?after={revision}`, the control plane waits until the head is past `after`, for up to 25 seconds. Nothing newer in that time is **204**, and the reader asks again. A cursor ahead of the head (the store was reset) is answered at once.

Feed reads stay off traces and request metrics, like health checks. A 5xx is still logged. Each control-plane process polls the head in Postgres every second, so an apply through any replica reaches readers of every replica within about a second.

### Readers

| | Gateway | Identity |
|--|--|--|
| Boot | Waits for the first snapshot; retries while the control plane is down. Does not listen until it has one | Waits for the first snapshot; retries. Does not listen until it has one, then creates the sign-in node if it doesn't exist |
| Running | Long-polls; swaps routes, kinds and CORS whole on each change | Long-polls; swaps kinds, roles and services whole on each change |
| Control plane down | Keeps serving the last snapshot | Keeps the last snapshot |
| A snapshot that fails the schema | Keeps the current one, logs, and moves its cursor past it | Same |

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
| `SEED_DIR` | empty | A config directory applied on first boot, when the head is `0`. Never read again |

Ready is **503** unless Postgres answers.

## Seeds

On first boot there's nothing to destroy, so the seed applies without `confirm`. Dev compose mounts `compose/seed/` (kinds, roles, and the generated catalog) and sets `SEED_DIR`. Prod compose mounts a folder with `kinds.yml` and `roles.yml` only, so identity can start; apply the rest yourself. After the first apply the folder is never read again.

## Related

- [`routes.md`](routes.md): route format and scopes
- [`roles.md`](roles.md): roles and bindings
- [`identity.md`](identity.md): nodes, members, the generated catalog
- [`gateway-contract.md`](gateway-contract.md): request-time behavior
