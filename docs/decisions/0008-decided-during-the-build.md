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
| ~~The generated invite list routes have `audit: true`~~ **Superseded in step 3** by invite tokens shown once, on create: lists carry `token_prefix`, so listing is a plain read | They returned live tokens: a read worth recording |
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

## Step 3: identity principals, members and bindings

| Decision | Why |
|----------|-----|
| Step 3 serves identity's service API for everything it builds; step 5 checks that every generated upstream is served and its labels work | Step 2 served the node routes it built. The catalog itself was generated in step 1 |
| Invite tokens are hashed at rest and shown once, on create, like keys. Lists show `token_prefix`; the generated invite lists aren't audited | A token that can be read back is a stored secret. Stripe and GitHub show secrets once |
| Response shapes for bindings, access entries, members, service accounts, keys and invites are in the identity API; `/members/{id}/nodes` items add `root_slug` and `root_label` | A console showing "Gruow / gatheroom" needs the org's name without a second call |
| The access list has one entry per binding that counts at the node, keyed by binding id, with where it sits. On a root it includes guests' bindings below, marked `guest: true` | A member can hold several bindings that count, each with its own role and expiry. GitHub lists outside collaborators on the org |
| Filtered lists read the labels of routes in the service named `identity`; identity's routes file must define that service | Identity must know which routes are its own; the generated catalog already names it `identity` |
| A deleted node is listed under `?state=deleted` only when every node above it is live | Admit counts a deleted node's own bindings only as the last of a live chain; such a node can't be restored before its ancestor anyway |
| The creator gets `creator_role` unless an inherited binding with no `expires_at` already holds it | An expiring inherited grant would take the creator's access to their new node with it |
| `X-Plat5-Member-Id` naming no active member of the node's tree is **400**; on `POST /roots/{kind}` the header is optional and, when sent, must be an active directory member | The service refuses a malformed request. A front door may create a root with no member behind it, as it may any node |
| Adding someone who has never signed in creates the person and their directory member together | Every person has one directory member; there's no state without one |
| Deleting a service account deletes its principal and keys and removes its member, as at purge. Removing its member through the members route is **422** | One way for a service account to end. Its member is removed only with it |
| Service account create takes `expires_at`, required when the role has a `max_duration` | Otherwise a role with `max_duration` could never be given to a new service account; member add has the same field |
| Invites store `role_duration`, and each grant its `duration`, in the config's duration form. The role is fixed at create, `default_role` then if omitted. Below a root, `role` is required | One duration form across Plat5. `default_role` is null below a root |
| A node's delete revokes the invites on it and below; restore leaves them revoked. At redeem, grants on nodes no longer live are skipped | As with keys, a token that leaked near a deletion stays dead |
| Redeem by someone already bound on the invite's node is a use and changes nothing, grants included. Redeem answers `{ member_id, root_id, node_id }` | "No change" means none. The console needs the member and where to go next |
| The binding sweeper runs with the purge sweeper, every minute, and also marks invites past `expires_at` `expired` | One background loop; no setting until someone needs one |
| Bindings, service accounts and invites on a deleted node have no rule in the service API | Same as `PATCH` in step 2: the gateway already refuses deleted nodes, and other front doors are trusted |
| `PATCH` of a binding with `expires_at: null` clears it; the result must still meet the role's `max_duration`. Key lists include revoked keys. `/members/{id}/nodes` without `?kind` returns every kind. Member routes on a node that isn't a root, or on the sign-in node, are **404**; `/members/{id}` routes on a member that isn't a directory member are **404** | Plain defaults, consistent with the rest of the API |
| Removing a member already removed is **204** and changes nothing. Binding lists leave out bindings past `expires_at`; the access list also leaves out bindings whose role grants nothing where they sit. Grants name live nodes below the root, each once | Repeat deletes succeed elsewhere in the API. A list shows what's in force, not what the sweeper hasn't reached yet |
| A member stores its principal kind (`members.principal`) | The member shape names `person` or `service_account`, and a removed service account or deleted account has no principal row left |
| Writes that touch members take the tree lock, like structural writes | One active member per principal per tree, and "removed when left with no bindings", can't race |

## Step 4: identity admit

| Decision | Why |
|----------|-----|
| Every admit answer has every field. With `valid: false` they're empty or null; with `found: false`, `nodes` is empty too. On a sign-in route `nodes` is empty | The gateway reads a missing field as identity failing (**503**). A partial chain tells the gateway nothing it acts on |
| Admit resolves `chain` by kind name and never checks it against identity's kinds. Only a body it can't read is **422**: neither or both credentials, an `idp` missing `issuer` or `subject`, `scope` empty or `public`, a chain on a sign-in route, or a chain that doesn't end at `scope` | The gateway and identity read the config on their own schedules. A request during that second must fail like any missing node (`found: false`, **403**), not as an outage |
| `labels` is sorted, with `*` as the role wrote it | One request gives one answer, which keeps tests and logs stable. The gateway matches `*` itself |
| An empty name claim is treated as missing: the name stays | An IdP that sends `""` hasn't told us a name. Blanking someone's name on every request would be worse |
| Plan counts are what would be lost: deleted nodes count, expired bindings and revoked keys don't, purged nodes never do. `sample` is up to five ids in id order. Every requested kind has an entry. The counts come from one read-only, repeatable-read snapshot | A deleted node can still be restored, so removing its kind loses it. An expired binding or a revoked key never works again. The control plane refuses an answer missing a requested kind. One snapshot keeps the numbers consistent with each other |
| Admit lives in `members/` and plan counts in `nodes/`, each with a `MountInternal` beside lifecycle's | Admit reads people, keys, members and bindings; the counts are about kind removal. Same pattern as the lifecycle feed |

## Step 5: identity, generated routes

| Decision | Why |
|----------|-----|
| The check is a test over the control plane's own output: `fixtures/catalog/` holds kinds with a two-parent kind, and the identity feed for that config with its generated catalog. A control-plane test fails when the feed is stale (`UPDATE_FIXTURES=1` rewrites it); identity's tests read that file | The generator is Rust and identity is Go. One generated file both sides test against can't drift silently, as the docs' field tables can't drift from the schema |
| Identity's test sends each request the way the gateway will: match the route, admit with the path's chain, check labels, fill `upstream`, forward with `X-Plat5-Member-Id`. Every route method in the catalog must be served with a 2xx, or the test names it | "Served" means reachable through the catalog with the ids the gateway fills, not just mounted. A route the generator adds and identity doesn't serve fails by name |
| Each label-gated route is checked with a role holding only its label, bound where [generated labels](../reference/api/edge-catalog.md#generated-labels) says it's checked, and refused without it | Proves the generated names, the route scopes and the ancestor rule line up, for every chain |
| The compose seed's `routes/identity.yml` and `routes/audit.yml` must equal what the generator writes for the seed's kinds | They say they're generated; a test keeps that true |
| No gaps: every generated upstream is served, with the documented status, and the filtered lists read the generated read and delete labels | Recorded so step 6 starts from a checked catalog |

## Step 6: gateway

| Decision | Why |
|----------|-----|
| The admit cache key includes `resolve_deleted` | Otherwise a `found: false` cached by a read of a deleted node answers the restore route too, and the restore is **403** for up to the TTL |
| The gateway writes v2 audit intents and outcomes now; audit accepts them in step 7. Until then the e2e runs with audit off | The v1 intent's fields (`organization_id`, `user_id`) are gone with v1 admission; keeping them would be a shim |
| An event on a sign-in route has `root_id: null`; the person's log is keyed by `actor.member_id` | Admit returns no nodes on a sign-in route, so the gateway never knows the sign-in node's id, and the person's directory member already names the log |
| Every rate-limit policy on a route is counted. "Tightest" is the policy with the fewest requests remaining; a **429** carries the longest `Retry-After` among the policies exceeded | Each bucket sees every admitted request, so one policy's budget doesn't depend on another's. The longest wait is the one after which every policy passes again |
| A snapshot the gateway can't build (schema, a scope or `count_at` that isn't a kind, chain params that aren't the scope's chain) is refused whole | v1 skipped the bad route and served the rest. A partial route table is a state nobody applied |
| The gateway checks each snapshot against `schema/v2/`, compiled in as the control plane does, and keeps no copy of the route config code | The removals table drops the hand-aligned `route_config.rs` copies; the schema is the one definition of shape |
| The compose e2e lives in `e2e/` (its own Go module, opt-in), serves its own JWKS on host `:5401` and mints tokens; compose's `APIKEY_CACHE_TTL_SECS` is overridable and the e2e sets it low | No IdP needed to run it. With a 300s cache, "refused after delete" would wait on the documented TTL |
