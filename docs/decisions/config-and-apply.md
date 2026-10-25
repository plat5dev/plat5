# Config and apply

Why the deployment's config is one document applied whole, through a plan, validated once, and why the local stack applies it the same way. The rules: [`concepts/config.md`](../concepts/config.md), [`reference/config/services.md`](../reference/config/services.md), [`reference/api/control-plane.md`](../reference/api/control-plane.md), [`reference/cli.md`](../reference/cli.md).

## The config

| Decision | Why |
|----------|-----|
| `plat5.yml` holds the deployment's shape (settings, kinds, roles, rate limits, CORS) and lists one file per service | Split by owner: whoever runs Plat5, and each service's team. One entry point, and the CLI never guesses at a layout |
| A service file's one top-level key is the service's name; under it sit the service's own settings and its routes, grouped by scope, each scope with an optional `upstream_prefix` | A service's upstream shape is usually one prefix per scope. Writing it once leaves each route its relative path, methods and permissions |
| Routes in a kind scope are relative to the node, and the public path is the kinds' pieces joined root first | The public URL shape is set once in `plat5.yml`. Widening or renaming a kind's path doesn't touch any service file |
| Placeholders name what they hold: `{<kind>.id}`, `{member.id}`, `{user.id}`, and the route's own params bare (`{plan_id}`) | A dot always means a resolved value and params can't be kind names, so nothing is ambiguous. Only `.id` exists, so slugs never reach a service |
| Route-level `required_permissions`, `rate_limit` and `audit` are defaults each verb can override; a list of methods is a map with no overrides | OpenAPI's path-level settings work the same way. A rule refusing the mix would only make the same thing harder to write |
| `required_permissions: any_member` admits any member; `[]` is **422** | An empty list reads as "nothing required" to one reader and "nothing allowed" to another. Saying it is explicit config |
| `OPTIONS` isn't a route method | The gateway answers every `OPTIONS` as a preflight, so a route for it could never match |
| A route under a child kind's collection is **422**; the collection itself is allowed | Otherwise a customer's project named `archived` is shadowed by a literal `/projects/archived`, unreachable by slug, with no error anywhere |
| Two routes with the same method and shape are **422**, naming both, in one service or across services. A literal beats a param when matching | One of the two would be silently dead. Refusing at apply, with both field paths, says which |
| Deployment defaults (upstream timeouts, `max_body_bytes`) sit in `defaults:` in `plat5.yml`; a service's own fields override them. Boot-only wiring, secrets and trust anchors stay env | Config decides what a deployment could decide, and a change is an apply, not a restart |
| The config is one flat object: `plat5.yml`'s keys, with `services` as a map of name to service | The documents are checked together anyway; one shape from the file to the control plane leaves nothing to translate |
| The config has no format key. A second format adds `version`, with a migration | An absent key means the first format; a key with one possible value says nothing |
| One published JSON Schema, `schema/`, compiled into the control plane and copied into the CLI. `GET /schema` needs no token | A CRD's schema is the one definition of its shape. A schema isn't a secret, and editors fetch it |
| Durations are `^[1-9][0-9]*(s\|m\|h\|d)$`, for timeouts too | One number, one unit: unambiguous and easy to show in a diff |
| The control plane's errors name a field path (`services.e10s.scopes.project.routes[2].path`); the CLI adds the file | The control plane receives one body and can't know files. Kubernetes errors work the same way: the server names the field, `kubectl` names the file |
| The control plane holds the deployment's typed desired state, and nothing else: no wiring, secrets or trust anchors | It must never become an untyped settings bag |
| `AUDIT_ENABLED` stays env | It's wiring: whether the deployment runs an audit service. Events already written mean the same afterwards |

## Roles in the config

| Decision | Why |
|----------|-----|
| A role can name other roles in `includes`, transitively, and grants their permissions plus its own. Only permissions are included. A cycle or a missing role is **422** | Kubernetes aggregates ClusterRoles the same way ([aggregated ClusterRoles](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#aggregated-clusterroles)). `bindable_at` is where a role makes sense, which an including role decides for itself |
| Identity resolves includes when it reads the config; the feed carries roles as written | One place resolves them, and the plan shows the roles as the team wrote them |
| A role permission no route requires is **422**. Identity's and audit's resources count, defaults included, and so does a kind's `list_permission`. A route permission no role grants is legal | The first is a typo or a removed route, and it would grant nothing forever. The second is a route meant for `*` holders only |
| `role` is required on member add and on invites; there is no default role | Default-deny below the root is the role you give new members. A default that only fills an omitted field hides that choice |
| One example role set, kept in the CLI's templates and shown in the docs. Its top role is `admin` with `*`, and no other role holds `bindings:write` | Whoever holds `bindings:write` can grant any role, so a second role holding it would be `admin` under another name |

## Validating once

| Decision | Why |
|----------|-----|
| The control plane is the one validator: the schema, the rules tying documents together, and route shapes, on every apply | It's the only writer of the config, as the Kubernetes API server validates objects once and its controllers trust them. Two copies of the route rules drift apart |
| The gateway parses the feed and builds its route table, nothing more. A snapshot it can't parse or build is refused whole and reported | A partial route table is a state nobody applied |
| A contract test, not shared code: every fixture config the control plane accepts is fed through the gateway's builder in its tests | "Apply accepted it" provably means "the gateway can build it", without shared code between two services released apart |
| Upgrade the gateway with or before the control plane | A newer control plane can accept what an older gateway can't build; the older one keeps its last snapshot |

## Identity's and audit's routes

| Decision | Why |
|----------|-----|
| Identity's and audit's routes come from resources with documented defaults, overridden per deployment, for every kind or one; nothing is generated | Generated files get edited, and the next generation overwrites the edits. Defaults plus overrides keep them and need nothing per kind |
| The control plane expands them, with every other service's routes, before serving the gateway; identity's feed carries no routes | One expansion, in the one validator. Identity learns list visibility from `list_permission`, not from routes |
| `plat5 routes list` prints every expanded route, and the plan shows route changes expanded | With defaults, the expanded routes are what a reader needs to see |

## Plan and apply

| Decision | Why |
|----------|-----|
| No stored plans. A dry run returns the plan; the apply recomputes it and carries one precondition, the config revision the dry run saw | Kubernetes' server-side dry run ([dryRun](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)) and `resourceVersion` precondition ([concurrency control](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)), with Terraform's rule that a stale saved plan is refused ([saved plans](https://developer.hashicorp.com/terraform/cli/commands/plan#out-filename)) |
| The whole config, every apply | The rules tie kinds, roles and routes together, so a partial apply would be checked against everything else anyway. Terraform's model: what's not in the config is removed, and the plan says so |
| Only an empty kind can be removed: a kind with live nodes is blocked, listing what's in the way. A kind's parent changes only while it has no nodes | Google Cloud deletes only an empty folder ([folders.delete](https://docs.cloud.google.com/resource-manager/reference/rest/v3/folders/delete)). Nodes are deleted first, each restorable on its own, so no config change destroys data and no plan needs a confirmation |
| Every plan line shows what changed, field by field, and service lines show their expanded routes. The summary counts lines | A line that names a change without showing it can't be reviewed. Since the plan shows everything, `plat5 apply plan.json` skips the prompt |
| A renamed kind reads as a remove plus an add | A kind's name is in every path param, placeholder, header and event |
| A tightened rule (a new reserved slug) applies to new values only | Kubernetes validation ratcheting ([KEP-4008](https://www.kubernetes.dev/resources/keps/4008/)) |
| The control plane asks identity for counts only when a plan removes a kind or changes a parent, and answers **503** only then | Asking on every plan would deadlock a fresh stack: identity waits for a config, and the first apply would wait for identity |
| The control plane has no seed. A fresh stack waits for its first `plat5 apply` | One path for every change, the first one included. A seed folder would re-implement the CLI's merge |

## Revisions

| Decision | Why |
|----------|-----|
| One revision per apply; an apply with nothing to change writes none | Terraform's "No changes" |
| Revisions store the config as sent. `config pull` writes `plat5.yml` and `services/<service>.yml` into a directory with no `.yml` files | Pull gives back what was written, like Kubernetes' last-applied annotation |
| Every feed and `GET /config` answer **404** before the first apply | Nothing loaded never reads as empty |
| Rolling back is applying an older revision's config through the same plan | One path for every change |

## Readers

| Decision | Why |
|----------|-----|
| The gateway and identity long-poll the control plane and keep their last config while it's down | The control plane isn't on the request path |
| Each gateway replica reports, on every long poll, the revision it loaded and any it refused with the reason. It also exports a gauge of its loaded revision and a counter of refused snapshots | Otherwise a refused snapshot shows only as a log line. This way the control plane knows where each replica stands, as `kubectl rollout status` does for a Deployment |
| `plat5 apply` waits briefly for the replicas and reports them, or the refusal and why | The person who applied learns at once whether the change is live |
| Internal ports take no token; the deployment's network is the boundary | A shared token between Plat5's own services guards nothing the network doesn't, and is one more secret to rotate |

## The CLI

| Decision | Why |
|----------|-----|
| The CLI owns the saved plan and the diff, with `--detailed-exitcode` for drift checks in CI | Terraform's workflow ([detailed-exitcode](https://developer.hashicorp.com/terraform/cli/commands/plan#detailed-exitcode)) |
| The saved plan file is exactly the apply body: the config and `expect` | Nothing to translate; the file is the request |
| The CLI embeds a copy of the schema for offline `validate` and `routes list` | Editors and CI catch shape errors without a deployment |
| The CLI finds the control plane through its flags and env vars, defaulting to the local stack | Nothing to learn for local work |

## The local stack

| Decision | Why |
|----------|-----|
| `plat5 start`'s project file is `plat5.dev.yml`; `plat5.yml` means the deployment's config only | One name, one meaning. A deployment never reads the dev file, as a compose file beside a Kubernetes manifest isn't the manifest |
| `plat5.dev.yml` holds nothing that's in `plat5.yml`. A service's `url` is the gateway's address; `start` joins the app's compose services the urls name to the stack's network, and with the app run on the host maps those names to it | `start` and `plat5 apply` send the same config, so a local apply never flips urls back |
| `start` brings the stack up, applies `plat5.yml` through the same plan as `plat5 apply`, then waits for health. A blocked plan is printed and refused | Identity and the gateway are ready only once they have a config. One path for every change |
| The dev seed goes through identity's routes: a dev token carrying a `name` claim, a node named `Dev` of each root kind, and one personal key | A personal key reaches every tree its person can. A person's name comes only from the IdP. One node per root kind avoids picking one |
| The Plat5 repo keeps no starting flow or example service of its own: the README points to the CLI and its templates | One starting flow, kept where it runs |

## One definition, copies checked

| Decision | Why |
|----------|-----|
| The config reference pages' field tables are generated from the schema between markers. A test fails when they're stale; `UPDATE_DOCS=1` rewrites them. Prose stays hand-written | Kubernetes' API reference works this way; generating whole pages would push prose into the schema |
| `fixtures/catalog/` holds a config with one parent per kind and the expanded routes and identity feed the control plane serves for it. A control-plane test fails when they're stale (`UPDATE_FIXTURES=1` rewrites them); identity's, audit's and the gateway's tests read them | The control plane is Rust and identity and audit are Go. One expanded file every side tests against can't drift silently |
| The CLI's schema copy and `init` files are checked against a `plat5` checkout beside the CLI, and skip without one | The CLI repo builds alone; the check runs wherever both are checked out, as in CI |
