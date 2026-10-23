# 0008: Decided during the build

- **Status:** Accepted. Open: each build step adds its rows
- **Date:** Oct 2026

Each build session turns up questions the contracts don't answer. Every answer goes into `docs/` first; this record keeps the reason. One table per step, one row per decision. The contracts hold the rules; follow the links there.

## Step 1: control plane, config and plan

| Decision | Why |
|----------|-----|
| The config is one flat object: `plat5.yml`'s keys, with `services` as a map of name to service. The feeds use the same keys; identity's includes the settings. `cors` is `{ allowed_origins: [] }` when unset | The documents are checked together anyway; one shape from the file to the feed leaves nothing to translate |
| The control plane's errors name a field path (`services.e10s.routes[2].scope`); the CLI adds the file. `SEED_DIR` names the file itself | The control plane receives one body and can't know files. Kubernetes errors work the same way: the server names the field, `kubectl` names the file |
| The control plane asks identity for counts only when a plan removes a kind or narrows a parent, and answers **503** only then | Asking on every plan would deadlock first boot: identity waits for a config, and the seed apply would wait for identity |
| A kind removal's node count includes every descendant of any kind. A narrowing count leaves out nodes a removal in the same plan deletes | Otherwise removing a middle kind like `folder` is always blocked. The count you confirm is everything that goes |
| The summary is `{ add, change, remove, blocked }`. Kinds, roles, settings and CORS count once per line; a service counts its routes, plus one change for its own fields; blocked lines count only in `blocked` | A blocked line isn't a change that will happen. Routes are what a reader thinks of as a service's changes |
| The plan names `kind`, `role`, `service`, `cors` and `setting`. Changing `name_claim`, changing `deletion_grace` and removing a reserved slug are safe. Making a root a child or a child a root, or moving `sign_in`, is **422** | A CRD's scope can't change after creation either. `deletion_grace` is safe because of the next row |
| A deleted node's grace is fixed when it's deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only | Shortening it must not purge what people were promised could be restored. Tightened rules apply to new values only |
| Durations are `^[1-9][0-9]*(s\|m\|h\|d)$` | One number, one unit: unambiguous and easy to show in a diff. Seconds for completeness, as Kubernetes and Google Cloud accept them |
| Kind names can't be `public` or `member` | `public` is a scope; `member` would clash with `member_id`, `X-Plat5-Member-Id` and the `{member}` param |
| A kind's `segment` has the slug shape `^[a-z][a-z0-9-]{0,62}$`. The default is the kind name with `_` turned into `-`, plus `s` | Edge paths look consistent, written or derived. Headers already map `_` to `-` |
| `label` and `name_claim` are at most 128 characters | Same limit as other names |
| `me` is dropped from the example `reserved_slugs` | Slugs are 3 to 63 characters, so `me` can never be one |
| Every feed and `GET /config` answer **404** before the first apply | Nothing loaded never reads as empty |
| An apply with nothing to change writes no revision | Terraform's "No changes" |
| Revisions store the config as sent, before `route_prefix` and nested `methods` are expanded. `config pull` writes `plat5.yml` and `routes/<service>.yml` into a directory with no `.yml` files | Pull gives back what was written, like Kubernetes' last-applied annotation |
| The saved plan file is exactly the apply body: the config, `expect` and `confirm` | Nothing to translate; the file is the request |
| A dry run ignores `expect` and `confirm`. Extra keys in `confirm` are **409** `PLAN_STALE`. `services` may be empty | A confirm for something no longer destroyed means the plan moved. The prod seed has kinds and roles only |
| **422** for route-level `required_labels`, `rate_limit` or `audit` beside map-form `methods`; `resolve_deleted` on a public or sign-in route; a `count_at` on a policy a public route uses | Each is a config that can't mean anything. Refusing nonsense at apply is what validation is for |
| The four config reference pages' field tables are generated from `schema/v2/` between markers. A test fails when they're stale; `UPDATE_DOCS=1` rewrites them. Prose stays hand-written | Kubernetes' API reference works this way; generating whole pages would push prose into the schema |
| The CLI embeds a copy of `schema/v2/` for offline `validate` | Editors and CI catch shape errors without a deployment |
| The generated catalog's URLs are `http://identity:3000` and `http://audit:3002` | The compose defaults; the files are edited anyway |
| The generated invite list routes have `audit: true` | They return live tokens: a read worth recording |
| The CLI finds the control plane through its existing flags and env vars, defaulting to the local stack | Nothing new to learn |
| `plat5 start` and `init` stay on v1 until steps 8 and 9. The v1 project file and the v2 config are both named `plat5.yml`; step 8 settles that | They still work against the pinned v1 images |

## Step 2: identity nodes

| Decision | Why |
|----------|-----|
| Create is built in the store in step 2. `POST …/children/{kind}`, `POST /roots/{kind}` and the filtered children list are served in step 3 | Each needs members and bindings. Serving them earlier would break the contract: a node nobody below the org can reach |
| A kind removal is recorded in `kind_removals` (kind, `deletion_id`). Identity checks every config it reads against its nodes: a missing kind's live nodes are soft-deleted under one id; a returning kind restores them within grace; the row is dropped on restore or once that removal's nodes are purged | The Kubernetes controller pattern: compare desired with actual each time, so a revision identity missed still takes effect. Dropping at purge means a kind applied again after the grace starts empty |
| The v1 identity packages are deleted, not kept unmounted. Kept: key hashing and format, the error envelope, pagination, ids, audit helpers, db, metrics, middleware, telemetry | With the schema gone they were dead code passing only against fakes. Git history keeps them |
| One fresh `001_init.sql`; boot refuses a database that records v1 migrations | Hard cutover: data moves into a fresh database by hand |
| The sign-in node's label is its kind's `label`, or the kind name | The seed's directory kind has no label |
| A generated slug comes from the label: lowercase, other characters to `-`, trimmed, filled or prefixed to fit the shape, `-2`, `-3` on collision | Readable, and always valid |
| Deleting an already deleted or purged node succeeds and changes nothing; it keeps its `deletion_id` and `purge_after`. The sign-in node can't be deleted (**422**) | Kubernetes accepts a delete of something already being deleted. A second delete mustn't extend the grace |
| Restoring a live node succeeds and changes nothing. Restoring under a deleted parent is **409**; parents are never restored for it. Restoring a node whose kind isn't in the config is **409**: apply the kind again | Google Cloud makes you undelete the parent first |
| Nothing is created under a deleted or purged parent (**409**). `PATCH` on a deleted node has no rule | The gateway already refuses deleted nodes; other front doors are trusted |
| A service account born at a purged node goes with it: principal and keys deleted, member removed with `member.removed` | Google Cloud deletes a project's service accounts with the project |
| Purge takes the deleted nodes below it, even if their own `purge_after` is later | A child under a purged parent can never be restored, so keeping it loses nothing but storage |
| Delete answers **204**, restore **200** with the node, and releasing the slug of a node that isn't purged is **409** | Plain, and consistent with the rest of the API |
| A `deletion_id` is a bare ULID. The purge sweeper runs every minute and whenever a new config arrives, with no setting | Internal; no setting until someone needs one |
