# Plat5 v2 cutover

> Transitional. Delete once v2 ships.

## Scope

Plat5 v2 replaces the fixed realms (directory, organization) with a tree of kinds declared in config, and replaces memberships with members (one per principal per tree) holding bindings on any node of their tree. The contracts are the rest of `docs/`; the reasons are in [`decisions/README.md`](decisions/README.md). This page is only what's transitional: what changes from v1, what gets removed, and the build order. Delete it once v2 ships.

**Hard cutover.** v2 is a new major version: new identity tables, new route scopes, new wire names. Nothing reads v1 data or accepts v1 shapes. Data moves by hand into the new tables. Kyvera stays on v1 prod until then.

| Area | Stays | Changes |
| --- | --- | --- |
| Services | Gateway, identity, audit, control plane | Each changes as below; no new service. The operator plane is a separate product with its own v2 work |
| Gateway | Admission order, CORS, client IP, request IDs, audit intent and outcome, upstream proxying | Route scopes, subject fields and headers, slug resolution, label check against a node, multiple rate-limit policies |
| Identity | Persons, IdP links, service accounts, API keys, invites as join tokens | Organizations become nodes; memberships become members with bindings; per-org sessions go; validate becomes admit |
| Control plane | Documents, revisions, long-poll feeds | A `kinds` document; roles gain `bindable_at` and `max_duration`; every apply returns a plan |
| Audit | Intent and outcome, service-added details | Events carry the node and its ancestors |

Out of scope: per-resource permissions below a node (the policy hook stays deferred), deny rules, a request-and-approve flow for support access, and limits stored as data per org.

## What changes from v1

v2 restates Plat5's principles as eight, replacing v1's twelve. The table after them shows where each v1 principle went.

### v2 principles

Now in [`concepts/model.md`](concepts/model.md#principles).

### Where v1's principles went

| v1 principle (`model.md`) | In v2 |
| --- | --- |
| 1. A scope names who owns the resource, never who is calling | Principles 4 and 5 |
| 2. Services see owners, never actors | **Dropped.** Services see the caller's member id (5) |
| 3. Attribution belongs to audit; no creator concept | **Dropped.** A service may record the member id as an author |
| 4. Two front doors, one service contract | **Generalized** to any front door (6) |
| 5. The credential names the realm; the client doesn't | **Replaced** by 4 |
| 6. Plat5 decides only over facts it stores | Principle 2 |
| 7. An optional policy engine decides access to individual resources | **Changed.** Projects are Plat5's nodes (2); the engine is only for permissions below a node |
| 8. Services enforce only what holds whoever asks | Principles 3 and 6 |
| 9. A credential is never narrower than its membership | A fact, not a principle: keys carry no labels; a narrower machine is a service account with fewer bindings |
| 10. Everyone with access is on the roster | **Changed.** Guests; everyone with access appears on the org's access list |
| 11. The deployment's data, not Plat5's names | Principle 7 |
| 12. An event goes in the log of the realm that owns it | A fact: audit logs are per root |

### Rules by area

| Area | v1 | v2 |
| --- | --- | --- |
| Realms and scopes | Directory and organizations; scopes `public`, `user`, `organization`, `member` | Kinds from config; `scope: <kind>` or `scope: public` on each route |
| Subject | One subject per scope; never `user_id` with `member_id` | Every ancestor's `{kind}_id` and the caller's `member_id` for that tree, in the path or as headers; never a person id |
| Caller | Never sent; services see owners only | `X-Plat5-Member-Id` and `X-Plat5-Principal` on every request Plat5's gateway admits |
| User ids | `user_id` on `user` scope | The directory member id on `scope: directory` routes: one per person, never sent to other trees |
| Denied or missing | 401 for the wrong realm; 403 for a missing label | 403 "permission denied, or it may not exist" for both, as Google Cloud does |
| Unlabeled routes | Admit any admitted principal; identity's reads are unlabeled | Every route states its labels; `required_labels: []` means any member of the tree |
| Roles | One role per member; roles on or off for the deployment | One role per binding, many bindings per member, access is their union; roles always on |
| Removing a role | Members holding it get no labels | Same: bindings keep the name and grant nothing until the role returns |
| Role defaults | `creator_role`, `default_role`, `service_account_default_role` | `creator_role`, `default_role`; roles also gain `bindable_at`, `max_duration` |
| Suspension | Person and member | Person only; a member is removed instead |
| Sessions | Minted per org for member credentials | Removed; the IdP token works everywhere |
| Org identifiers | Id and name; the slug was dropped because "a slug granted nothing and routed nothing" | Id, slug and label on every node. v1's reason no longer holds: slugs route |
| Deleting an org | Hard delete, cascading | Soft for `deletion_grace` (default 30 days), restorable, then purged; never reused |
| Lifecycle events | Births and ends of orgs and members | `<kind>.created/.deleted/.restored/.purged`, `member.created/.removed` |
| Rate limits | Subject fixed by scope; one bucket per org on org routes | A policy counts at a kind; several policies per route |
| Listing what you can reach | A `member` route where the service asks the engine | Plat5 filters node lists itself |
| Credential check | `validate` returns realm and labels | `admit` returns nodes, member and labels for this request |
| Last owner | A write may not remove an org's last `creator_role` holder; account deletion blocked while last owner | Dropped. An org left without an admin is recovered through the operator plane |
| Unchanged | — | Admission order, `starting_after` pagination, invites as join tokens, no grant cap, labels checked only at the gateway, audit without retention |

### v1 rules carried over

- **Removed is final.** Re-adding someone to an org makes a new member id. The old member stays, and the member lookup returns its name and `status: removed`, so services can show a former member.

Not carried: the last-owner and account-deletion guards (trust the config; the operator plane recovers an org with no admin); "no grant cap", which is the absence of a rule; the roles list, which is an API in the node API section; `service_account_default_role`, so creating a service account requires `role`.

## What v1 code and docs get removed

No compatibility layer: these go in the same release that adds their replacements.

| Where | Removed | Replaced by |
| --- | --- | --- |
| Gateway config | `public` / `user` / `organization` / `member` blocks on a service | A `scope` on each route |
| Gateway admission | `Admission::{User, Organization, Member}`, the wrong-realm 401 | The admit result |
| Gateway subject | The fixed `user_id`, `organization_id`, `member_id` fields | `{kind}_id` per node and `member_id`, in the path or as `X-Plat5-*` headers |
| Identity tables | `organizations`, `memberships`, `credentials` (sessions), `organization_invites`, `identities` | `nodes`, `node_ancestors`, `members`, `bindings`, `api_keys`, `invites`, `principals` |
| Identity routes | `/user/memberships`, `/user/organizations`, `/user/organizations/{id}/session`, `/org/members`, `/org/service-accounts`, `/org/invites` and their operator twins | Routes generated per kind, and `/me/…` |
| Identity internals | `POST /internal/credentials/validate`; version 1 key formats (`-mk-1-`, `-ms-1-`); the session format `{brand}-s-2-` | `POST /internal/admit`; `{brand}-k-2-` keys only |
| Roles | The `ROLES_ENABLED=false` mode; the plain `role: [labels]` form; `service_account_default_role` | Roles always on; `{ labels, bindable_at, max_duration }` |
| Env | `CREDENTIAL_VALIDATE_URL`, `AUTH_USER_ID_CLAIM`, `ROLES_ENABLED`, `SEED_ROUTES_DIR`, `SEED_ROLES_FILE` | `ADMIT_URL`, `AUTH_SUBJECT_CLAIM`, `SEED_DIR`; the control plane gains `IDENTITY_INTERNAL_URL` |
| Catalogs | `services/identity/routes.yml`, `services/audit/routes.yml` | Generated from the kinds into the deployment's `routes/identity.yml` and `routes/audit.yml` (`plat5 routes generate`) |
| Control plane | `GET /internal/roles`; `PUT /services/{name}`, `DELETE /services/{name}`, `PUT /roles`, `PUT /cors`, per-service and per-document revisions and restore; upsert of services; the two hand-aligned copies of `route_config.rs` | `GET /internal/identity`; one `POST /apply` of the whole config with dry run and preconditions; one revision per apply; `schema/v2/` |
| Lifecycle | `organization.created`, `organization.deleted`, `member.created`, `member.removed` | `<kind>.created/.deleted/.restored/.purged`, `member.created/.removed` |
| Operator console | Fixed organization and user pages | Out of this spec: the operator plane's own v2 work, in its repo |
| Docs | v1 docs, `identity-boundary.md` | Done on this branch: `docs/` is the v2 contract. "Realm" and "user" as wire names go; "member" now means one principal in one tree |
| Compose seeds | `compose/roles.yml`, mounted as `SEED_ROLES_FILE` | `compose/seed/plat5.yml` and its routes files, mounted as `SEED_DIR` |
| Code-level READMEs | `services/*/README.md`, `compose/README.md` as written | Updated in the step that changes that service |
| `TODO.md` | The policy-engine hook as the way to project access | Kept deferred, for permissions below a node only |

Outside this repo: the `template-*` service and console templates in plat5dev use v1 scopes and need the same change. The e10s repo follows with its own v2 work.

## Build order

One Claude Code session per step, each starting from the docs named in the step. `docs/` is law on this branch. Steps 1 and 2 can run in parallel; everything after needs both.

1. **Control plane: config and plan** ([config](concepts/config.md); [kinds](reference/config/kinds.md), [roles](reference/config/roles.md), [routes](reference/config/routes.md), [cors](reference/config/cors.md); [admin API](reference/api/control-plane.md), [CLI](reference/cli.md)). `plat5.yml` and routes files with `version: 2`, the JSON Schema in `schema/v2/`, whole-config apply with dry run, `expect` and `confirm`, revisions, `POST /catalog`. The `kinds` document, roles in the new form, route `scope`, the seven validation rules, with identity's counts stubbed. The CLI's `validate`, `plan` (diff and `--detailed-exitcode`), `apply`, `routes generate` and `config pull` are the same step in the CLI repo. Then generate the four config reference pages from the schema, so they can't drift. Done when a dry run classifies a set of fixture configs correctly, and a stale `expect` or a changed count is refused.
2. **Identity: nodes** ([model](concepts/model.md), [lifecycle](concepts/lifecycle.md), [identity schema](internals/identity-schema.md), [identity API](reference/api/identity.md)). The new schema, nodes with the closure table, slugs and tombstones, soft delete with `purge_after` stored on each deleted node (from `deletion_grace` at delete time, so a changed grace applies to new deletes only), restore, the purge sweeper (after `purge_after`), lifecycle events. Done when the invariants have tests.
3. **Identity: principals, members and bindings** ([access](concepts/access.md), [lifecycle](concepts/lifecycle.md#members), [identity API](reference/api/identity.md)). People and IdP links, the directory member, service accounts born at a node, API keys, bindings with expiry and its sweeper, invites with grants, guest invites below a root, `/me`.
4. **Identity: admit** ([access](concepts/access.md#roles-and-bindings), [internal APIs](reference/api/internal.md#admit)). `POST /internal/admit` with the five-step resolution, plus the counts the control plane's plan needs. Done when admit has table tests for every row of the status table.
5. **Identity: generated routes** ([edge catalog](reference/api/edge-catalog.md), [lists](concepts/access.md#lists)). Node, binding, service account and invite routes generated per kind from `kinds`, with the generated labels.
6. **Gateway** ([access](concepts/access.md#how-a-request-is-decided), [gateway](reference/gateway.md), [headers](reference/headers.md), [routes](reference/config/routes.md)). Route scopes, the chain from the path, admit with its cache and `good_until`, the two 403s, subject fill and `X-Plat5-*` headers, several rate-limit policies per route.
7. **Audit** ([audit](concepts/audit.md), [audit API](reference/api/audit.md), [audit storage](internals/audit-storage.md)). The node chain on events, logs per root, reads by node.
8. **Removals.** Sweep the removals table in code, compose and the CLI. Rework `plat5 init` (a `plat5.yml` and `routes/` instead of v1 route files and roles file) and `plat5 start` (apply `plat5.yml` through plan and apply; dev seed through the generated routes), which step 1 left on v1. v1's local project file is also `plat5.yml` (`project_id`, `modules`, `upstreams`, …); the rework decides where those settings go so the name means the config only.
9. **End to end.** `plat5 init` and `plat5 start` bring up a v2 local stack with `organization` and `project` kinds, and e2e tests for the flows e10s needs: create an org and project, add a service account at the project, check through the gateway, delete and restore.

The plat5dev `template-*` repos follow once the gateway step lands. The operator plane has its own v2 spec and sessions in its repo.
