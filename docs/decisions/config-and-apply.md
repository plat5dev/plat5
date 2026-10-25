# Config and apply

Why the deployment's config is one document applied whole, through a plan, and why the local stack applies it the same way. The rules: [`concepts/config.md`](../concepts/config.md), [`reference/api/control-plane.md`](../reference/api/control-plane.md), [`reference/cli.md`](../reference/cli.md).

Config is data that can delete data: removing a kind removes its nodes. Plat5 allows that with safeguards rather than forbidding it. It takes Kubernetes' rule that a kind can be deleted with its objects ([custom resource definitions](https://v1-36.docs.kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/_print/)) and runs it through Google Cloud's rule that a deleted project is restorable for 30 days ([delete and restore](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)).

## The config

| Decision | Why |
|----------|-----|
| `plat5.yml` holds the deployment's shape (settings, kinds, roles, defaults, CORS) and lists one routes file per service; every route stays explicit, generated ones included | Split by owner: whoever runs Plat5, and each service's team. One entry point, and the CLI never guesses at a layout |
| The config is one flat object: `plat5.yml`'s keys, with `services` as a map of name to service. The feeds use the same keys; identity's includes the settings. `cors` is `{ allowed_origins: [] }` when unset | The documents are checked together anyway; one shape from the file to the feed leaves nothing to translate |
| The config has no format key. A second format adds `version`, with a migration | An absent key means the first format; a key with one possible value says nothing |
| One published JSON Schema, `schema/`, compiled into the control plane and the gateway, copied into the CLI | A CRD's schema is the one definition of its shape. Hand-aligned copies of validation drift |
| Durations are `^[1-9][0-9]*(s\|m\|h\|d)$` | One number, one unit: unambiguous and easy to show in a diff. Seconds for completeness, as Kubernetes and Google Cloud accept them |
| **422** for route-level `required_labels`, `rate_limit` or `audit` beside map-form `methods`; `resolve_deleted` on a public or sign-in route; a `count_at` on a policy a public route uses | Each is a config that can't mean anything. Refusing nonsense at apply is what validation is for |
| `route_prefix` on the service; sign-in routes rate-limit per directory member | Less repetition in a routes file; the sign-in node is one node for everyone, so it can't be the bucket |
| The control plane's errors name a field path (`services.e10s.routes[2].scope`); the CLI adds the file. `SEED_DIR` names the file itself | The control plane receives one body and can't know files. Kubernetes errors work the same way: the server names the field, `kubectl` names the file |
| The control plane holds the deployment's typed desired state, and nothing else: no wiring, secrets, trust anchors or switches that change what stored data means | It must never become an untyped settings bag |

## Plan and apply

| Decision | Why |
|----------|-----|
| No stored plans. A dry run returns the plan; the apply recomputes it and carries two preconditions: the config revision the dry run saw, and the node count of each kind it destroys | Kubernetes' server-side dry run ([dryRun](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)) and `resourceVersion` precondition ([concurrency control](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)), with Terraform's rule that a stale saved plan is refused ([saved plans](https://developer.hashicorp.com/terraform/cli/commands/plan#out-filename)) |
| Counts, not a data revision | A counter bumped by every node or binding write would make a destructive plan stale forever on a busy deployment. Confirming "14 nodes" only goes stale when the thing being destroyed changes |
| The whole config, every apply | The cross-document rules tie kinds, roles and routes together, so a partial apply would be checked against everything else anyway. Terraform's model: what's not in the config is removed, and the plan says so. A service team can't push its routes alone; Kubernetes' ApplySets are the pattern if separate owners are ever needed |
| Changes are safe, blocked or destructive. Removing a kind is the only destructive change; narrowing a parent with nodes under it is blocked | A destructive change applies only once planned and confirmed. Google Cloud deletes only an empty folder ([folders.delete](https://docs.cloud.google.com/resource-manager/reference/rest/v3/folders/delete)) |
| Making a root a child or a child a root, or moving `sign_in`, is **422**. Renaming a kind reads as a remove plus an add | A CRD's scope can't change after creation either |
| Changing `name_claim` or `deletion_grace`, and removing a reserved slug, are safe | A changed grace applies to new deletes only ([lifecycle](lifecycle.md#deleting-restoring-and-purging)) |
| Tightened rules (a shorter `max_duration`, a new reserved slug) apply to new values only | Kubernetes validation ratcheting ([KEP-4008](https://www.kubernetes.dev/resources/keps/4008/)) |
| Adding a kind in the middle is two safe applies: add it under the parent, then widen the child's parents | Each step is safe on its own, so neither needs a confirmation |
| The plan names `kind`, `role`, `service`, `cors` and `setting`. The summary is `{ add, change, remove, blocked }`: kinds, roles, settings and CORS count once per line; a service counts its routes, plus one change for its own fields; blocked lines count only in `blocked` | A blocked line isn't a change that will happen. Routes are what a reader thinks of as a service's changes |
| The control plane asks identity for counts only when a plan removes a kind or narrows a parent, and answers **503** only then | Asking on every plan would deadlock first boot: identity waits for a config, and the first apply would wait for identity |
| A kind removal's node count includes every descendant of any kind. A narrowing count leaves out nodes a removal in the same plan deletes | Otherwise removing a middle kind is always blocked. The count you confirm is everything that goes |
| Plan counts are what would be lost: deleted nodes count, expired bindings and revoked keys don't, purged nodes never do. `sample` is up to five ids in id order. Every requested kind has an entry. The counts come from one read-only, repeatable-read snapshot | A deleted node can still be restored, so removing its kind loses it. One snapshot keeps the numbers consistent with each other |
| A dry run ignores `expect` and `confirm`. Extra keys in `confirm` are **409** `PLAN_STALE`. `services` may be empty | A confirm for something no longer destroyed means the plan moved. A production seed holds kinds and roles only |

## Revisions

| Decision | Why |
|----------|-----|
| One revision per apply; an apply with nothing to change writes none | Terraform's "No changes" |
| Revisions store the config as sent, before `route_prefix` and nested `methods` are expanded. `config pull` writes `plat5.yml` and `routes/<service>.yml` into a directory with no `.yml` files | Pull gives back what was written, like Kubernetes' last-applied annotation |
| Every feed and `GET /config` answer **404** before the first apply | Nothing loaded never reads as empty |
| Rolling back is applying an older revision's config through the same plan | A rollback that removes a kind is as destructive as any other |

## Readers

| Decision | Why |
|----------|-----|
| The gateway and identity long-poll the control plane and keep their last config while it's down | The control plane isn't on the request path |
| The gateway checks each snapshot against the schema, compiled in, and refuses a snapshot it can't build (a scope or `count_at` that isn't a kind, chain params that aren't the scope's chain) whole | A partial route table is a state nobody applied |
| Identity doesn't check snapshots against the schema; it refuses only one it can't work with | The control plane validated the config at apply. Identity needs only what it reads to be well formed |
| Upgrade the gateway with or before the control plane | The gateway refuses a snapshot it can't build, and keeps the last one until it's upgraded |
| Identity checks every config it reads against its nodes ([lifecycle](lifecycle.md#removing-a-kind)) | The Kubernetes controller pattern: compare desired with actual each time, so a revision identity missed still takes effect |

## The CLI

| Decision | Why |
|----------|-----|
| The CLI owns the saved plan and the diff, with `--detailed-exitcode` for drift checks in CI | Terraform's workflow ([detailed-exitcode](https://developer.hashicorp.com/terraform/cli/commands/plan#detailed-exitcode)) |
| The saved plan file is exactly the apply body: the config, `expect` and `confirm` | Nothing to translate; the file is the request |
| The CLI embeds a copy of the schema for offline `validate` | Editors and CI catch shape errors without a deployment |
| The CLI finds the control plane through its flags and env vars, defaulting to the local stack | Nothing to learn for local work |

## The local stack

| Decision | Why |
|----------|-----|
| `plat5 start`'s project file is `plat5.dev.yml`; `plat5.yml` means the deployment's config only | One name, one meaning. A deployment never reads the dev file, as a compose file beside a Kubernetes manifest isn't the manifest |
| `plat5.dev.yml` holds nothing that's in `plat5.yml`: no routes, roles or upstreams. A service's `url` is the gateway's address; `start` joins the app's compose services the urls name to the stack's network, and with the app run on the host maps those names to it | `start` and `plat5 apply` send the same config, so a local apply never flips urls back |
| `start` applies `plat5.yml` through the same plan as `plat5 apply`. A plan that destroys nothing applies without a prompt; a destructive or blocked one is printed and refused, pointing at `plat5 apply` | Dangerous changes are planned and confirmed (model principle 8). `start` isn't interactive, so it never confirms on your behalf |
| `start` brings the stack up, applies, then waits for health; the local stack has no seed | Identity and the gateway are ready only once they have a config. Applying through the plan keeps one path for every change |
| The dev seed goes through the generated routes: a dev token carrying a `name` claim, a node labelled `Dev` of each root kind other than the sign-in kind, and one personal key | A personal key reaches every tree its person can. A person's name comes only from the IdP. One node per root kind avoids picking one |
| `start` has no operator module until the operator plane's own work lands | It would run against routes identity doesn't serve |
| `init` writes `--template none` only until the starter repos ship `plat5.yml` and `routes/`; its files are plat5's compose seed | A starter written for other routes would be a broken project. The seed is already checked against the generator |
| The local project's `cli:` floor is `0.8.0`, the first release that reads `plat5.dev.yml` | An older CLI says to upgrade instead of failing on the file |

## One definition, copies checked

| Decision | Why |
|----------|-----|
| The four config reference pages' field tables are generated from the schema between markers. A test fails when they're stale; `UPDATE_DOCS=1` rewrites them. Prose stays hand-written | Kubernetes' API reference works this way; generating whole pages would push prose into the schema |
| `fixtures/catalog/` holds kinds with a two-parent kind and the identity feed for that config with its generated catalog. A control-plane test fails when the feed is stale (`UPDATE_FIXTURES=1` rewrites it); identity's and audit's tests read it | The generator is Rust and identity is Go. One generated file both sides test against can't drift silently |
| Identity's catalog test sends each request as the gateway will: match the route, admit with the path's chain, check labels, fill `upstream`, forward with `X-Plat5-Member-Id`. Each label-gated route is checked with a role holding only its label, and refused without it | Proves the generated names, the route scopes and the ancestor rule line up, for every chain |
| The compose seed's `routes/identity.yml` and `routes/audit.yml` must equal what the generator writes for the seed's kinds | They say they're generated; a test keeps that true |
| The gateway's route and pipeline tests are driven by the same catalog fixture | Every catalog route's scope, chain and filled upstream is checked where it's enforced |
| The CLI's schema copy and `init` files are checked against a `plat5` checkout beside the CLI, and skip without one | The CLI repo builds alone; the check runs wherever both are checked out, as in CI |
| The compose e2e lives in `e2e/` (its own Go module, opt-in), serves its own JWKS on host `:5401` and mints tokens; compose's `APIKEY_CACHE_TTL_SECS` is overridable and the e2e sets it low | No IdP needed to run it. With a 300s cache, "refused after delete" would wait on the TTL |
